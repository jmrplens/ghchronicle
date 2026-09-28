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

// asked is every request a fake store received: the method, the path and
// what it was asked, so a test can hold an inspection to reading alone.
type asked struct {
	mu   sync.Mutex
	seen []string
}

func (a *asked) note(r *http.Request, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, r.Method+" "+r.URL.Path+" "+r.URL.Query().Get("q")+body)
}

// onlyRead fails on anything a store could take as a change: a method other
// than GET, a POST anywhere but the query endpoint, or a statement that is
// not a SELECT.
func (a *asked) onlyRead(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.seen {
		method, rest, _ := strings.Cut(s, " ")
		path, q, _ := strings.Cut(rest, " ")
		switch {
		case method == http.MethodGet:
		case method == http.MethodPost && path == "/api/v2/query":
		default:
			t.Errorf("an inspection sent %s %s", method, path)
		}
		if q != "" && !strings.HasPrefix(q, "SELECT ") && !strings.HasPrefix(q, `{"dialect"`) {
			t.Errorf("an inspection asked %q", q)
		}
	}
}

// influx3Catalog is an InfluxDB 3 that holds gh_discussion_comment in the
// shape 2.6.0 wrote, answered the way 3.11.2 answers (measured).
func influx3Catalog(t *testing.T, a *asked) *config.InfluxSink {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.note(r, "")
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/ping":
			w.Header().Set("X-Influxdb-Version", "3.11.2")
			_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
		case strings.Contains(q, "information_schema.columns") && strings.Contains(q, "'gh_discussion_comment'"):
			_, _ = io.WriteString(w, `[{"column_name":"answers","data_type":"Int64"},`+
				`{"column_name":"comment","data_type":"Dictionary(Int32, Utf8)"},`+
				`{"column_name":"is_answer","data_type":"Dictionary(Int32, Utf8)"},`+
				`{"column_name":"reason","data_type":"Utf8"},`+
				`{"column_name":"user","data_type":"Dictionary(Int32, Utf8)"}]`)
		case strings.Contains(q, "information_schema.columns"):
			_, _ = io.WriteString(w, `[]`)
		case strings.HasPrefix(q, "SELECT count(*)"):
			_, _ = io.WriteString(w, `[{"n":3,"oldest":"2023-11-14T22:13:20"}]`)
		case strings.HasPrefix(q, `SELECT DISTINCT "user"`):
			_, _ = io.WriteString(w, `[{"v":"octocat"},{"v":"other"}]`)
		default:
			t.Errorf("an unexpected question: %s", q)
		}
	}))
	t.Cleanup(srv.Close)
	return &config.InfluxSink{URL: srv.URL, Token: "t", Bucket: "github"}
}

// TestInfluxDB3IsAskedItsCatalogForATagColumn: an old tag is a column of the
// live table typed as a tag, and a field of the same name is not one.
func TestInfluxDB3IsAskedItsCatalogForATagColumn(t *testing.T) {
	t.Parallel()
	a := &asked{}
	store := &influx{sink: influx3Catalog(t, a)}
	name, err := store.Describe(t.Context())
	if err != nil || name != "InfluxDB 3 Core 3.11.2" {
		t.Fatalf("Describe = %q, %v", name, err)
	}
	shape, err := store.Shape(t.Context(), "gh_discussion_comment", []string{"is_answer", "reason"}, []string{"user", "owner"})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	if !shape.Exists || !slices.Equal(shape.Old, []string{"is_answer"}) || shape.Rows != 3 || !shape.Oldest.Equal(want) {
		t.Errorf("shape = %+v, want the table, is_answer alone as the old tag, 3 rows from %s", shape, want)
	}
	if got := shape.Values["user"]; !slices.Equal(got, []string{"octocat", "other"}) {
		t.Errorf("users = %v", got)
	}
	if _, asked := shape.Values["owner"]; asked {
		t.Error("owner is not a column of the table, yet its values were asked for")
	}
	absent, err := store.Shape(t.Context(), "gh_dependabot_alert_item", []string{"state"}, nil)
	if err != nil || absent.Exists || len(absent.Old) > 0 {
		t.Errorf("a table that is not there reads as %+v, %v", absent, err)
	}
	a.onlyRead(t)
}

// TestInfluxDB2IsAskedForRowsNotKeys: 2.x keeps a tag key listed after the
// rows that had it are deleted, so the question is whether a row has it.
func TestInfluxDB2IsAskedForRowsNotKeys(t *testing.T) {
	t.Parallel()
	a := &asked{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.note(r, string(body))
		if r.URL.Path == "/ping" {
			w.Header().Set("X-Influxdb-Version", "v2.7.12")
			w.Header().Set("X-Influxdb-Build", "OSS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Token tok2" {
			t.Errorf("a Flux query carried %q", got)
		}
		var q struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &q)
		switch {
		case strings.Contains(q.Query, `min(column: "_time")`):
			_, _ = io.WriteString(w, ",result,table,_time\n,_result,0,2023-11-14T22:13:20Z\n\n")
		case strings.Contains(q.Query, `exists r["is_answer"]`):
			_, _ = io.WriteString(w, ",result,table,_time\n,_result,0,2023-11-14T22:13:20Z\n\n")
		case strings.Contains(q.Query, `distinct(column: "user")`):
			_, _ = io.WriteString(w, ",result,table,_value\n,_result,0,other\n,_result,0,octocat\n\n")
		default:
			_, _ = io.WriteString(w, "\n")
		}
	}))
	t.Cleanup(srv.Close)
	store := &influx{sink: &config.InfluxSink{URL: srv.URL, Token: "tok2", Org: "gh", Bucket: "github"}}
	name, err := store.Describe(t.Context())
	if err != nil || name != "InfluxDB 2 OSS 2.7.12" {
		t.Fatalf("Describe = %q, %v", name, err)
	}
	shape, err := store.Shape(t.Context(), "gh_discussion_comment", []string{"is_answer", "state"}, []string{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if !shape.Exists || !slices.Equal(shape.Old, []string{"is_answer"}) || shape.Rows != -1 ||
		!slices.Equal(shape.Values["user"], []string{"octocat", "other"}) {
		t.Errorf("shape = %+v", shape)
	}
	a.onlyRead(t)
}

// TestElasticsearchCountsDocumentsRatherThanReadingTheMapping: the mapping
// keeps a field after its documents go, so only a count says one is left.
func TestElasticsearchCountsDocumentsRatherThanReadingTheMapping(t *testing.T) {
	t.Parallel()
	a := &asked{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.note(r, "")
		switch {
		case r.URL.Path == "/":
			_, _ = io.WriteString(w, `{"version":{"number":"9.5.3"}}`)
		case !strings.HasPrefix(r.URL.Path, "/gh-gh_discussion_comment/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"type":"index_not_found_exception"},"status":404}`)
		case strings.HasSuffix(r.URL.Path, "/_count") && strings.Contains(string(body), `"is_answer"`):
			_, _ = io.WriteString(w, `{"count":2}`)
		case strings.HasSuffix(r.URL.Path, "/_count") && len(body) > 0:
			_, _ = io.WriteString(w, `{"count":0}`)
		case strings.HasSuffix(r.URL.Path, "/_count"):
			_, _ = io.WriteString(w, `{"count":3}`)
		case strings.HasSuffix(r.URL.Path, "/_search"):
			if !strings.Contains(string(body), `"user.keyword"`) {
				t.Errorf("the values were not asked of the keyword sub-field: %s", body)
			}
			_, _ = io.WriteString(w, `{"aggregations":{"oldest":{"value_as_string":"2023-11-14T22:13:20.000Z"},`+
				`"v0":{"buckets":[{"key":"other"},{"key":"octocat"}]}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	store := &elastic{sink: &config.ElasticsearchSink{URL: srv.URL, Prefix: "GH", APIKey: "k"}}
	if name, err := store.Describe(t.Context()); err != nil || name != "Elasticsearch 9.5.3" {
		t.Fatalf("Describe = %q, %v", name, err)
	}
	shape, err := store.Shape(t.Context(), "gh_discussion_comment", []string{"is_answer", "state"}, []string{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if !shape.Exists || !slices.Equal(shape.Old, []string{"is_answer"}) || shape.Rows != 3 ||
		shape.Oldest.IsZero() || !slices.Equal(shape.Values["user"], []string{"octocat", "other"}) {
		t.Errorf("shape = %+v", shape)
	}
	absent, err := store.Shape(t.Context(), "gh_dependabot_alert_item", []string{"state"}, nil)
	if err != nil || absent.Exists {
		t.Errorf("an index that is not there reads as %+v, %v", absent, err)
	}
	a.onlyRead(t)
}

// TestAStoreThatRefusesTheQuestionSaysWhy: a token that may write and not
// read is an answer nobody should mistake for an empty store.
func TestAStoreThatRefusesTheQuestionSaysWhy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, `{"version":"3.11.2"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"insufficient permissions"}`)
	}))
	t.Cleanup(srv.Close)
	store := &influx{sink: &config.InfluxSink{URL: srv.URL, Bucket: "github"}}
	if _, err := store.Shape(t.Context(), "gh_discussion_comment", []string{"is_answer"}, nil); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Errorf("a refusal reads as %v, want the 403", err)
	}
}

// TestInspectorsAreTheStoresThatCanBeAsked: the rest follow the record.
func TestInspectorsAreTheStoresThatCanBeAsked(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sinks: config.Sinks{
		Influx:        &config.InfluxSink{URL: "http://i", Bucket: "b"},
		Postgres:      &config.PostgresSink{DSN: "postgres://u@h/d"},
		Elasticsearch: &config.ElasticsearchSink{URL: "http://e"},
		SQL:           &config.SQLSink{Path: "/tmp/x.sql"},
		Graphite:      &config.GraphiteSink{Addr: "g:2003"},
		Telegraf:      &config.TelegrafSink{URL: "http://t"},
	}}
	var names []string
	for _, in := range Inspectors(cfg) {
		names = append(names, in.Name())
	}
	if !slices.Equal(names, []string{"influxdb", "postgres", "elasticsearch"}) {
		t.Errorf("Inspectors = %v", names)
	}
}
