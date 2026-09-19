package teardown

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/internal/config"
)

// influxServer answers the catalog query and records what was dropped.
type influxServer struct {
	tables  []string
	dropped []string
	status  int
}

func (s *influxServer) start(t *testing.T) *config.InfluxSink {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, `{"error":"no"}`)
			return
		}
		if r.Method == http.MethodDelete {
			s.dropped = append(s.dropped, r.URL.Query().Get("table"))
			return
		}
		rows := make([]map[string]string, 0, len(s.tables))
		for _, name := range s.tables {
			rows = append(rows, map[string]string{"table_name": name})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return &config.InfluxSink{URL: srv.URL, Token: "t", Bucket: "github"}
}

// TestItClaimsOnlyItsOwnTables. The catalog of a shared database holds other
// people's tables, and an uninstall that took the lot would be a different and
// much worse tool.
func TestItClaimsOnlyItsOwnTables(t *testing.T) {
	t.Parallel()
	s := &influxServer{tables: []string{
		"gh_repo", "gh_star", "payments", "gh_old_measurement_nobody_writes", "telegraf_cpu",
	}}
	store := &influx{sink: s.start(t)}
	held, err := store.Holds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gh_old_measurement_nobody_writes", "gh_repo", "gh_star"}
	if strings.Join(held, ",") != strings.Join(want, ",") {
		t.Errorf("holds = %v, want only the gh_ ones: %v", held, want)
	}
}

// TestAskingTheStoreFindsWhatACompiledListWouldMiss: the measurement above that
// nobody writes any more is exactly the reason this asks rather than carrying
// the names this version happens to emit.
func TestAskingTheStoreFindsWhatACompiledListWouldMiss(t *testing.T) {
	t.Parallel()
	s := &influxServer{tables: []string{"gh_retired_in_v1"}}
	store := &influx{sink: s.start(t)}
	held, err := store.Holds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0] != "gh_retired_in_v1" {
		t.Errorf("holds = %v, want the measurement no current collector writes", held)
	}
}

// TestDropRefusesWhatIsNotOurs, whatever it is handed. The caller builds the
// list from Holds, so this is the guard for the day something else does.
func TestDropRefusesWhatIsNotOurs(t *testing.T) {
	t.Parallel()
	s := &influxServer{}
	store := &influx{sink: s.start(t)}
	if err := store.Drop(t.Context(), "payments"); err == nil {
		t.Error("it dropped a table that is not one of ours")
	}
	if len(s.dropped) != 0 {
		t.Errorf("it called the store anyway: %v", s.dropped)
	}
	if err := store.Drop(t.Context(), "gh_repo"); err != nil {
		t.Errorf("it refused one of ours: %v", err)
	}
}

// TestATableAlreadyGoneIsTheOutcomeAskedFor, not a failure: InfluxDB answers a
// second delete with a conflict, and the thing wanted is for it to be gone.
func TestATableAlreadyGoneIsTheOutcomeAskedFor(t *testing.T) {
	t.Parallel()
	s := &influxServer{status: http.StatusConflict}
	store := &influx{sink: s.start(t)}
	if err := store.Drop(t.Context(), "gh_repo"); err != nil {
		t.Errorf("a table already deleted was reported as a failure: %v", err)
	}
}

// TestAStoreThatWillNotAnswerIsReported rather than read as an empty store,
// which would say there is nothing to remove.
func TestAStoreThatWillNotAnswerIsReported(t *testing.T) {
	t.Parallel()
	s := &influxServer{status: http.StatusInternalServerError}
	store := &influx{sink: s.start(t)}
	if _, err := store.Holds(t.Context()); err == nil {
		t.Error("a store that failed was read as holding nothing")
	}
}

// TestTheSinksThatCannotBeEmptiedSayWhy. Silence would read as nothing to
// remove, which is the opposite of the truth for two of them.
func TestTheSinksThatCannotBeEmptiedSayWhy(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sinks: config.Sinks{
		Graphite:   &config.GraphiteSink{Addr: "graphite:2003"},
		Prometheus: &config.PrometheusSink{Listen: ":9090"},
		SQL:        &config.SQLSink{Dialect: "postgres", Path: "/tmp/points.sql"},
	}}
	stores, cannot := For(cfg)
	if len(stores) != 1 || stores[0].Name() != "sql" {
		t.Errorf("stores = %v, want the sql file and nothing else", stores)
	}
	said := map[string]string{}
	for _, u := range cannot {
		said[u.Sink] = u.Reason
	}
	for sink, phrase := range map[string]string{
		"graphite":   "no way to delete",
		"prometheus": "scraped rather than written to",
		"sql":        "never connects",
	} {
		if !strings.Contains(said[sink], phrase) {
			t.Errorf("%s said %q, want it to carry %q", sink, said[sink], phrase)
		}
	}
}

// TestTheSQLSinkOffersItsFileAndItsRotations, since that is the whole of what
// it wrote.
func TestTheSQLSinkOffersItsFileAndItsRotations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "points.sql")
	for _, name := range []string{path, path + ".1", path + ".2"} {
		if err := os.WriteFile(name, []byte("insert"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.sql"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &sqlFile{sink: &config.SQLSink{Path: path}}
	held, err := store.Holds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 3 {
		t.Errorf("holds = %v, want the file and its two rotations and nothing else", held)
	}
	for _, item := range held {
		if err = store.Drop(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "unrelated.sql")); err != nil {
		t.Error("it removed a file it did not write")
	}
}
