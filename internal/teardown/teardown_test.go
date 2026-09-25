package teardown

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
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

// TestAStoreKeepsItsConnectionWhenTheSharedPoolIsEmptied: closing an
// httptest server empties http.DefaultTransport's idle pool, which every
// parallel test here does when it ends. A store drawing on that pool loses
// its connection to whichever test ended last, and a request that was using
// it fails for a reason that belongs to another test.
func TestAStoreKeepsItsConnectionWhenTheSharedPoolIsEmptied(t *testing.T) {
	t.Parallel()
	shared, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is a %T, so this test cannot empty it", http.DefaultTransport)
	}
	s := &influxServer{tables: []string{"gh_repo"}}
	store := &influx{sink: s.start(t)}
	var reused []bool
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = append(reused, info.Reused) },
	})
	for range 2 {
		if _, err := store.Holds(ctx); err != nil {
			t.Fatal(err)
		}
		shared.CloseIdleConnections()
	}
	if len(reused) != 2 || !reused[1] {
		t.Errorf("connections reused = %v, want the second call on the first call's "+
			"connection although the shared pool was emptied between them", reused)
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

// TestTheElasticsearchStoreClaimsItsPrefixAndNothingElse. An index outside the
// prefix the sink writes under was put there by something else, and the
// server's own listing is what says which are which.
func TestTheElasticsearchStoreClaimsItsPrefixAndNothingElse(t *testing.T) {
	t.Parallel()
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/"))
			return
		}
		// What _cat/indices answers for the prefix it was asked about.
		_, _ = io.WriteString(w, `[{"index":"ghc-gh_repo"},{"index":"ghc-gh_star"}]`)
	}))
	t.Cleanup(srv.Close)
	store := &elastic{sink: &config.ElasticsearchSink{
		URL: srv.URL, Prefix: "ghc-", APIKey: "key",
	}}
	held, err := store.Holds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(held, ",") != "ghc-gh_repo,ghc-gh_star" {
		t.Errorf("holds = %v, want the two the server listed", held)
	}
	if err = store.Drop(t.Context(), "somebody-elses-index"); err == nil {
		t.Error("it deleted an index outside the sink's prefix")
	}
	if err = store.Drop(t.Context(), "ghc-gh_repo"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "ghc-gh_repo" {
		t.Errorf("deleted = %v, want only the one under the prefix", deleted)
	}
}

// TestAnIndexAlreadyGoneIsNotAFailure, for the reason a dropped table is not:
// what was asked for is that it not be there.
func TestAnIndexAlreadyGoneIsNotAFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"index_not_found_exception"}`)
	}))
	t.Cleanup(srv.Close)
	store := &elastic{sink: &config.ElasticsearchSink{URL: srv.URL, Prefix: "ghc-"}}
	if err := store.Drop(t.Context(), "ghc-gh_repo"); err != nil {
		t.Errorf("an index already gone was reported as a failure: %v", err)
	}
	held, err := store.Holds(t.Context())
	if err != nil || len(held) != 0 {
		t.Errorf("holds = %v, %v; want nothing and no error from a store with no indices", held, err)
	}
}

// TestElasticsearchCarriesWhicheverCredentialItWasGiven, because the listing
// and the delete both need it and a store with authentication on answers
// neither without.
func TestElasticsearchCarriesWhicheverCredentialItWasGiven(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sink config.ElasticsearchSink
		want string
	}{
		{"an api key", config.ElasticsearchSink{Prefix: "ghc-", APIKey: "k"}, "ApiKey k"},
		{"a password", config.ElasticsearchSink{
			Prefix: "ghc-", Username: "u", Password: "p",
		}, "Basic dTpw"},
		{"neither", config.ElasticsearchSink{Prefix: "ghc-"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("Authorization")
				_, _ = io.WriteString(w, `[]`)
			}))
			t.Cleanup(srv.Close)
			sink := tc.sink
			sink.URL = srv.URL
			store := &elastic{sink: &sink}
			if _, err := store.Holds(t.Context()); err != nil {
				t.Fatal(err)
			}
			if seen != tc.want {
				t.Errorf("Authorization = %q, want %q", seen, tc.want)
			}
		})
	}
}

// TestEveryStoreSaysItsName, which is what the lines a person reads are keyed
// on: a store that answered to the wrong name would report its tables under
// another sink's heading.
func TestEveryStoreSaysItsName(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sinks: config.Sinks{
		Influx:        &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"},
		Elasticsearch: &config.ElasticsearchSink{URL: "http://es:9200", Prefix: "ghc-"},
		SQL:           &config.SQLSink{Path: "/tmp/points.sql"},
	}}
	stores, _ := For(cfg)
	var names []string
	for _, s := range stores {
		names = append(names, s.Name())
	}
	if strings.Join(names, ",") != "influxdb,elasticsearch,sql" {
		t.Errorf("names = %v, want one per configured sink in a fixed order", names)
	}
}

// TestAnUnsupportedSinkReadsAsOneLine, since that line is printed as it is.
func TestAnUnsupportedSinkReadsAsOneLine(t *testing.T) {
	t.Parallel()
	got := Unsupported{Sink: "graphite", Reason: "offers no delete"}.String()
	if got != "graphite: offers no delete" {
		t.Errorf("String() = %q", got)
	}
}

// TestThePostgresStoreRefusesWhatIsNotOursBeforeConnecting. The prefix check
// comes first on purpose: a name that is not this project's is refused whether
// or not there is a server to refuse it at, and a run with the database down
// still says the right thing about it.
func TestThePostgresStoreRefusesWhatIsNotOursBeforeConnecting(t *testing.T) {
	t.Parallel()
	// A dsn nothing is listening on. Reaching the connection would take the
	// timeout; refusing first takes none, which is also how this test says
	// which of the two happened.
	store := &postgres{sink: &config.PostgresSink{
		DSN: "postgres://u:p@127.0.0.1:1/d?sslmode=disable&connect_timeout=1",
	}}
	if err := store.Drop(t.Context(), "payments"); err == nil ||
		!strings.Contains(err.Error(), "not one of this project's tables") {
		t.Errorf("err = %v, want the refusal rather than a connection error", err)
	}
}

// TestThePostgresStoreReportsADatabaseItCannotReach rather than reading it as
// a database holding nothing, which would say there is nothing to remove.
func TestThePostgresStoreReportsADatabaseItCannotReach(t *testing.T) {
	t.Parallel()
	store := &postgres{sink: &config.PostgresSink{
		DSN: "postgres://u:p@127.0.0.1:1/d?sslmode=disable&connect_timeout=1",
	}}
	if _, err := store.Holds(t.Context()); err == nil {
		t.Error("a database that cannot be reached was read as holding nothing")
	}
	if err := store.Drop(t.Context(), "gh_repo"); err == nil {
		t.Error("a drop against a database that is not there reported success")
	}
	if store.Name() != "postgres" {
		t.Errorf("Name() = %q", store.Name())
	}
}

// TestIdentifiersAreQuotedTheWayTheSinkQuotesThem. A table this drops is one
// that sink wrote, and the two have to agree about what its name is.
func TestIdentifiersAreQuotedTheWayTheSinkQuotesThem(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"gh_repo":  `"gh_repo"`,
		`gh_"odd"`: `"gh_""odd"""`,
		"gh_UPPER": `"gh_UPPER"`,
	} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestBothWaysIntoPostgresAreOfferedSeparately: the connecting sink can be
// emptied, and the file sink's file can be removed, and they are two entries
// because they are two things.
func TestBothWaysIntoPostgresAreOfferedSeparately(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sinks: config.Sinks{
		Postgres: &config.PostgresSink{DSN: "postgres://u@h:5432/d"},
		SQL:      &config.SQLSink{Path: "/tmp/points.sql"},
	}}
	stores, cannot := For(cfg)
	var names []string
	for _, s := range stores {
		names = append(names, s.Name())
	}
	if strings.Join(names, ",") != "postgres,sql" {
		t.Errorf("stores = %v, want one for each way in", names)
	}
	// The file sink still says the thing only it has to say.
	var said string
	for _, u := range cannot {
		if u.Sink == "sql" {
			said = u.Reason
		}
	}
	if !strings.Contains(said, "never connects") {
		t.Errorf("the sql sink said %q, want it to say rows already loaded are not its to remove", said)
	}
}
