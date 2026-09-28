package teardown

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// clearedAt is the instant the tests clear at.
var clearedAt = time.Date(2026, 9, 29, 10, 10, 10, 0, time.UTC)

// requests is every request a fake store received, method, path and body.
type requests struct {
	mu   sync.Mutex
	seen []string
}

func (q *requests) note(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	line := strings.TrimSpace(r.Method + " " + r.URL.RequestURI() + " " + string(body))
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seen = append(q.seen, line)
	return string(body)
}

// changes is every request that was not a read.
func (q *requests) changes() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for _, s := range q.seen {
		if !strings.HasPrefix(s, "GET ") {
			out = append(out, s)
		}
	}
	return out
}

// influx3Tables is an InfluxDB 3 whose catalog lists tables, and which does
// to a deleted table what 3.11.2 does: renames it <name>-<instant> and keeps
// listing it.
func influx3Tables(t *testing.T, q *requests, tables ...string) *config.InfluxSink {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.note(r)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/ping":
			_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/configure/table":
			name := r.URL.Query().Get("table")
			i := slices.Index(tables, name)
			if i < 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			tables[i] = name + "-20260929T101012"
		case r.URL.Path == "/api/v3/query_sql":
			rows := []map[string]string{}
			for _, name := range tables {
				rows = append(rows, map[string]string{"table_name": name})
			}
			_ = json.NewEncoder(w).Encode(rows)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return &config.InfluxSink{URL: srv.URL, Token: "t", Bucket: "github"}
}

// TestInfluxDB3SetsATableAsideByDeletingIt: the delete names the one table
// and the database, and the name the server gave the copy is read back as
// the one table of that measurement that was not there before, not an older
// copy of an earlier migration.
func TestInfluxDB3SetsATableAsideByDeletingIt(t *testing.T) {
	t.Parallel()
	q := &requests{}
	store := &influx{sink: influx3Tables(t, q, "gh_discussion_comment", "gh_discussion_comment-20260901T000000",
		"gh_discussion_comment_x", "payments")}
	aside, held, err := store.Clear(t.Context(), "gh_discussion_comment", clearedAt)
	if err != nil || !held {
		t.Fatalf("Clear = %+v, %v, %v", aside, held, err)
	}
	want := Aside{
		Name: "gh_discussion_comment-20260929T101012", Measurement: "gh_discussion_comment",
		At: time.Date(2026, 9, 29, 10, 10, 12, 0, time.UTC), ByServer: true,
	}
	if aside != want {
		t.Errorf("aside = %+v, want %+v", aside, want)
	}
	if got := q.changes(); !slices.Equal(got, []string{"DELETE /api/v3/configure/table?db=github&table=gh_discussion_comment"}) {
		t.Errorf("it changed %v, want the one table deleted", got)
	}
}

// TestNothingIsClearedThatIsNotThere, and nothing is sent that would change
// a store for a name no collector writes: a comma, a star or a space would be
// read by a store as more than the one name.
func TestNothingIsClearedThatIsNotThere(t *testing.T) {
	t.Parallel()
	q := &requests{}
	store := &influx{sink: influx3Tables(t, q, "gh_repo")}
	if _, held, err := store.Clear(t.Context(), "gh_discussion_comment", clearedAt); err != nil || held {
		t.Errorf("a table that is not there: held %v, %v", held, err)
	}
	for _, name := range []string{"payments", "gh_*", "gh_a,gh_b", "gh_repo-20260101T000000", "GH_REPO", "gh_a b"} {
		if _, _, err := store.Clear(t.Context(), name, clearedAt); err == nil {
			t.Errorf("it cleared %q", name)
		}
	}
	if got := q.changes(); len(got) != 0 {
		t.Errorf("it changed %v", got)
	}
}

// TestInfluxDB2DeletesTheOneMeasurement: 2.x has no rename, so the rows are
// deleted by predicate over every instant it can hold, in the sink's org and
// bucket. Measured on 2.7.12, this took gh_discussion_comment's rows and left
// gh_discussion_comment_x, the other measurements and the other bucket.
func TestInfluxDB2DeletesTheOneMeasurement(t *testing.T) {
	t.Parallel()
	q := &requests{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.note(r)
		switch r.URL.Path {
		case "/ping":
			w.Header().Set("X-Influxdb-Version", "v2.7.12")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/delete":
			if r.Header.Get("Authorization") != "Token tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	store := &influx{sink: &config.InfluxSink{URL: srv.URL, Token: "tok", Org: "o", Bucket: "github"}}
	aside, held, err := store.Clear(t.Context(), "gh_discussion_comment", clearedAt)
	if err != nil || !held || aside.Name != "" {
		t.Fatalf("Clear = %+v, %v, %v, want the rows deleted and nothing kept", aside, held, err)
	}
	want := `POST /api/v2/delete?bucket=github&org=o {"start":"1677-09-21T00:12:43.145224194Z",` +
		`"stop":"2262-04-11T23:47:16.854775806Z","predicate":"_measurement=\"gh_discussion_comment\""}`
	if got := q.changes(); !slices.Equal(got, []string{want}) {
		t.Errorf("it sent %v, want %s", got, want)
	}
}

// elasticIndices is an Elasticsearch holding indices with a number of
// documents each, which clones, blocks and deletes the way 9.5.3 does.
type elasticIndices struct {
	mu      sync.Mutex
	docs    map[string]int
	blocked map[string]bool
	// short makes a clone hold one document fewer than its source.
	short bool
}

func (e *elasticIndices) serve(t *testing.T, q *requests) *config.ElasticsearchSink {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := q.note(r)
		e.mu.Lock()
		defer e.mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		index := parts[0]
		n, found := e.docs[index]
		switch {
		case index == "_cat":
			var rows []map[string]string
			for name := range e.docs {
				rows = append(rows, map[string]string{"index": name})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case !found:
			w.WriteHeader(http.StatusNotFound)
		case len(parts) == 1 && r.Method == http.MethodDelete:
			delete(e.docs, index)
		case parts[1] == "_count":
			_ = json.NewEncoder(w).Encode(map[string]int{"count": n})
		case parts[1] == "_block":
			e.blocked[index] = true
		case parts[1] == "_refresh":
		case parts[1] == "_settings" && strings.Contains(body, `"write":false`):
			delete(e.blocked, index)
		case parts[1] == "_clone" && e.blocked[index]:
			e.docs[parts[2]], e.blocked[parts[2]] = n, true
			if e.short {
				e.docs[parts[2]]--
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return &config.ElasticsearchSink{URL: srv.URL, Prefix: "ghchronicle"}
}

// TestElasticsearchClonesTheIndexAsideAndDeletesIt, in that order: no more
// writes, a clone holding every document, and only then the delete, so the
// sink's next write creates the index afresh.
func TestElasticsearchClonesTheIndexAsideAndDeletesIt(t *testing.T) {
	t.Parallel()
	q := &requests{}
	es := &elasticIndices{docs: map[string]int{
		"ghchronicle-gh_discussion_comment": 3, "ghchronicle-gh_discussion_comment_x": 1, "other-gh_discussion_comment": 1,
	}, blocked: map[string]bool{}}
	store := &elastic{sink: es.serve(t, q)}
	aside, held, err := store.Clear(t.Context(), "gh_discussion_comment", clearedAt)
	if err != nil || !held {
		t.Fatalf("Clear = %+v, %v, %v", aside, held, err)
	}
	if aside.Name != "ghchronicle-gh_discussion_comment-20260929t101010" || aside.ByServer || !aside.At.Equal(clearedAt) {
		t.Errorf("aside = %+v", aside)
	}
	want := []string{
		"PUT /ghchronicle-gh_discussion_comment/_block/write",
		"POST /ghchronicle-gh_discussion_comment/_refresh",
		"POST /ghchronicle-gh_discussion_comment/_clone/ghchronicle-gh_discussion_comment-20260929t101010",
		"DELETE /ghchronicle-gh_discussion_comment",
	}
	if got := q.changes(); !slices.Equal(got, want) {
		t.Errorf("it sent\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if es.docs["ghchronicle-gh_discussion_comment-20260929t101010"] != 3 || es.docs["ghchronicle-gh_discussion_comment_x"] != 1 ||
		es.docs["other-gh_discussion_comment"] != 1 || len(es.docs) != 3 {
		t.Errorf("indices after = %v", es.docs)
	}
	asides, err := store.Asides(t.Context(), []string{"gh_discussion_comment"})
	if err != nil || len(asides) != 1 || asides[0].Name != aside.Name || !asides[0].At.Equal(clearedAt) {
		t.Errorf("Asides = %+v, %v", asides, err)
	}
	if err = store.Purge(t.Context(), aside.Name); err != nil || len(es.docs) != 2 {
		t.Errorf("Purge = %v, indices %v", err, es.docs)
	}
}

// TestAnIndexThatCouldNotBeClonedTakesWritesAgain: a clone short of a
// document is not a copy to delete the index behind, so the index is kept,
// its block taken off for the sink's writes, and the partial clone removed.
func TestAnIndexThatCouldNotBeClonedTakesWritesAgain(t *testing.T) {
	t.Parallel()
	q := &requests{}
	es := &elasticIndices{docs: map[string]int{"ghchronicle-gh_discussion_comment": 3}, blocked: map[string]bool{}, short: true}
	store := &elastic{sink: es.serve(t, q)}
	if _, _, err := store.Clear(t.Context(), "gh_discussion_comment", clearedAt); err == nil {
		t.Fatal("a clone short of a document was taken as the copy")
	}
	if es.docs["ghchronicle-gh_discussion_comment"] != 3 || len(es.docs) != 1 || es.blocked["ghchronicle-gh_discussion_comment"] {
		t.Errorf("after a failed clone: indices %v, blocked %v", es.docs, es.blocked)
	}
}

// TestPurgeTakesOnlyACopy: whatever it is handed, a name that is not the
// sink's prefix, a measurement and an instant is refused before the store is
// called.
func TestPurgeTakesOnlyACopy(t *testing.T) {
	t.Parallel()
	q := &requests{}
	es := &elasticIndices{docs: map[string]int{}, blocked: map[string]bool{}}
	store := &elastic{sink: es.serve(t, q)}
	for _, name := range []string{
		"ghchronicle-gh_discussion_comment", "other-gh_x-20260929t101010", "ghchronicle-*",
		"ghchronicle-gh_x-20260929t101010,ghchronicle-gh_repo", "ghchronicle-payments-20260929t101010",
	} {
		if err := store.Purge(t.Context(), name); err == nil {
			t.Errorf("it purged %q", name)
		}
	}
	pg := &postgres{sink: &config.PostgresSink{DSN: "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"}}
	for _, name := range []string{"gh_discussion_comment", "payments-20260929T101010", "gh_x-2026"} {
		if err := pg.Purge(t.Context(), name); err == nil || !strings.Contains(err.Error(), "not a copy") {
			t.Errorf("postgres purge of %q: %v", name, err)
		}
	}
	if got := q.changes(); len(got) != 0 {
		t.Errorf("it changed %v", got)
	}
}

// TestACopyIsReadBackFromItsName, the one record a store keeps of when it was
// made, and a name that only starts like one is not one.
func TestACopyIsReadBackFromItsName(t *testing.T) {
	t.Parallel()
	ms := []string{"gh_discussion_comment", "gh_repo"}
	for name, want := range map[string]bool{
		"gh_discussion_comment-20260929T101010":   true,
		"gh_discussion_comment-20260929t101010":   true,
		"gh_discussion_comment_x-20260929T101010": false,
		"gh_discussion_comment-20260929T1010":     false,
		"gh_discussion_comment-20260929T101010x":  false,
		"gh_discussion_comment":                   false,
		"gh_star-20260929T101010":                 false,
	} {
		a, ok := asideOf(name, ms)
		if ok != want {
			t.Errorf("%s: %v, want %v", name, ok, want)
		}
		if ok && (!a.At.Equal(clearedAt) || a.Measurement != "gh_discussion_comment") {
			t.Errorf("%s read as %+v", name, a)
		}
	}
}
