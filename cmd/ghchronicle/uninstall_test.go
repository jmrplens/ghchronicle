package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// teardownStub is a Grafana and an InfluxDB in one server, which is all the
// uninstall talks to, and it remembers every deletion it was asked for.
type teardownStub struct {
	tables []string
	// refuseDelete makes every removal fail, which is how the run’s behavior
	// when one does is read.
	refuseDelete bool
	deleted      []string
	// release is the InfluxDB release /ping names, the way 3.0.0 to 3.4.0
	// name it; empty answers as Grafana does.
	release string
}

func (s *teardownStub) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if s.refuseDelete {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"message":"it is in use"}`)
				return
			}
			target := r.URL.Path
			if table := r.URL.Query().Get("table"); table != "" {
				target = "table:" + table
				// What InfluxDB 3.11.2 answers a delete of a table it has
				// already deleted, which it lists under this name until it
				// purges it.
				if strings.Contains(table, "-2026") {
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, "attempted to modify resource that was already deleted")
					return
				}
			}
			s.deleted = append(s.deleted, target)
			return
		}
		switch {
		case r.URL.Path == "/ping" && s.release != "":
			w.Header().Set("X-Influxdb-Build", "Core")
			w.Header().Set("X-Influxdb-Version", s.release)
			_, _ = io.WriteString(w, `{"version":"`+s.release+`"}`)
		case strings.Contains(r.URL.Path, "/api/v3/query_sql"):
			rows := make([]map[string]string, 0, len(s.tables))
			for _, name := range s.tables {
				rows = append(rows, map[string]string{"table_name": name})
			}
			_ = json.NewEncoder(w).Encode(rows)
		default:
			// Everything the uninstall asks Grafana about is there.
			_, _ = io.WriteString(w, `{"uid":"x"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// teardownConfig points both halves at the one stub.
func teardownConfig(t *testing.T, url string) *config.Config {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(state, ".json")+"-written.bin",
		[]byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Sinks:     config.Sinks{Influx: &config.InfluxSink{URL: url, Token: "t", Bucket: "github"}},
		Grafana:   &config.Grafana{URL: url, Token: "grafana-token"},
		StateFile: state,
	}
}

// TestAnUninstallRemovesNothingUntilItIsToldYes. The list is the default
// because the alternative is a typed command that empties a store.
func TestAnUninstallRemovesNothingUntilItIsToldYes(t *testing.T) {
	t.Parallel()
	s := &teardownStub{tables: []string{"gh_repo", "gh_star", "payments"}}
	cfg := teardownConfig(t, s.serve(t))
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "all", false, &said); err != nil {
		t.Fatal(err)
	}
	if len(s.deleted) != 0 {
		t.Errorf("it removed %v without being told yes", s.deleted)
	}
	if !strings.Contains(said.String(), "Nothing was") {
		t.Errorf("output = %q, want it to say nothing was removed", said.String())
	}
	for _, want := range []string{
		"gh_repo in influxdb", "the dashboard ghchronicle-influxdb",
		"the state file",
	} {
		if !strings.Contains(said.String(), want) {
			t.Errorf("output = %q, want it to list %q", said.String(), want)
		}
	}
	if strings.Contains(said.String(), "payments") {
		t.Error("it listed a table that is not ours")
	}
	if _, err := os.Stat(cfg.StateFile); err != nil {
		t.Error("the state file went without a yes")
	}
}

// TestWithYesItActuallyRemoves, and the state files go with it.
func TestWithYesItActuallyRemoves(t *testing.T) {
	t.Parallel()
	s := &teardownStub{tables: []string{"gh_repo", "gh_star"}}
	cfg := teardownConfig(t, s.serve(t))
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "all", true, &said); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(s.deleted, " ")
	for _, want := range []string{
		"table:gh_repo", "table:gh_star",
		"/api/dashboards/uid/ghchronicle-influxdb", "/api/datasources/uid/ghchronicle-influxdb",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("deleted = %v, want it to include %q", s.deleted, want)
		}
	}
	if _, err := os.Stat(cfg.StateFile); !os.IsNotExist(err) {
		t.Errorf("the state file is still there: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(cfg.StateFile, ".json") + "-written.bin"); !os.IsNotExist(err) {
		t.Error("the dedupe ledger beside the state file was left behind")
	}
}

// TestAnUninstallRefusesWhileAnotherProcessHoldsTheStateFile: removing the
// tables or the state files under a running service removes what it goes on
// writing, and the lock file is among the state files; unlinked while the
// service held it, the next -migrate -yes took a lock of its own and ran
// beside the service. With the lock held, nothing is removed and the holder
// is named; once it is let go, everything goes, the lock file and the
// refill checkpoint with the rest. The lock file goes last, after the
// uninstall has let go of its own hold: Windows removes no file a process
// holds open, and that was the one state file an uninstall there left.
func TestAnUninstallRefusesWhileAnotherProcessHoldsTheStateFile(t *testing.T) {
	t.Parallel()
	s := &teardownStub{tables: []string{"gh_repo"}}
	cfg := teardownConfig(t, s.serve(t))
	if err := os.WriteFile(cfg.RefillProgressFile(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := run.TakeLock(cfg.LockFile(), run.HeldByService, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	err = uninstall(t.Context(), cfg, "data,state", true, &said)
	if err == nil || !strings.Contains(err.Error(), "ghchronicle test, service") || !strings.Contains(err.Error(), "nothing was removed") {
		t.Errorf("an uninstall beside the service = %v", err)
	}
	if _, statErr := os.Stat(cfg.LockFile()); statErr != nil || len(s.deleted) != 0 {
		t.Errorf("beside the service it removed %v, and the lock file: %v", s.deleted, statErr)
	}
	if err = service.Release(); err != nil {
		t.Fatal(err)
	}
	said.Reset()
	if err = uninstall(t.Context(), cfg, "data,state", true, &said); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{cfg.StateFile, cfg.LockFile(), cfg.RefillProgressFile()} {
		if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
			t.Errorf("%s is still there after the uninstall:\n%s", gone, said.String())
		}
	}
	lines := strings.Split(strings.TrimRight(said.String(), "\n"), "\n")
	if last := lines[len(lines)-1]; last != "  removed the state file "+cfg.LockFile() {
		t.Errorf("the last thing removed is %q, not the lock file:\n%s", last, said.String())
	}
}

// TestATableInfluxDBAlreadyDeletedIsNotReportedRemoved. InfluxDB 3 keeps a
// table it deleted, renamed <name>-<instant>, for 72 hours, lists it with the
// others and answers a delete of it with a 409, which Drop takes as the table
// being gone. Listed, every uninstall said "removed" of a table that was still
// there afterwards; a migration sets a table aside exactly that way. It is
// said once as the server's to purge, and not offered.
func TestATableInfluxDBAlreadyDeletedIsNotReportedRemoved(t *testing.T) {
	t.Parallel()
	s := &teardownStub{tables: []string{"gh_discussion_comment", "gh_discussion_comment-20260928T222330", "gh_repo"}}
	cfg := teardownConfig(t, s.serve(t))
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, targetData, true, &said); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.deleted, " "); got != "table:gh_discussion_comment table:gh_repo" {
		t.Errorf("deleted %q, want the two live tables alone", got)
	}
	if strings.Contains(said.String(), "removed gh_discussion_comment-20260928T222330") {
		t.Errorf("it reported removed a table the server keeps:\n%s", said.String())
	}
	if !strings.Contains(said.String(), "purged by the server itself") ||
		!strings.Contains(said.String(), "gh_discussion_comment-20260928T222330") {
		t.Errorf("it does not say whose the deleted table is to purge:\n%s", said.String())
	}
}

// TestAnUninstallSaysAnInfluxDBBefore32KeepsWhatItDeletes. 3.0 and 3.1 have
// no hard deletion: every table the uninstall deletes stays, renamed, beside
// the ones deleted already, which is worth knowing before yes, and so is the
// request that removes one on a later release.
func TestAnUninstallSaysAnInfluxDBBefore32KeepsWhatItDeletes(t *testing.T) {
	t.Parallel()
	s := &teardownStub{
		tables:  []string{"gh_discussion_comment", "gh_discussion_comment-20260928T222330", "gh_repo"},
		release: "3.1.0",
	}
	url := s.serve(t)
	var said strings.Builder
	if err := uninstall(t.Context(), teardownConfig(t, url), targetData, false, &said); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"note: influxdb: each table deleted here is renamed <table>-<instant> and kept for good, and the tables " +
			"deleted already are kept for good: gh_discussion_comment-20260928T222330. InfluxDB 3 Core 3.1.0 has no " +
			"hard deletion",
		"curl -X DELETE '" + url + "/api/v3/configure/table?db=github&table=<table>-<instant>&hard_delete_at=now'",
	} {
		if !strings.Contains(said.String(), want) {
			t.Errorf("the uninstall does not say %q:\n%s", want, said.String())
		}
	}
	if strings.Contains(said.String(), "purged by the server itself") {
		t.Errorf("it says a server with no hard deletion purges what it deleted:\n%s", said.String())
	}
}

// TestAnAdoptedDatasourceSurvivesAnUninstall. It was somebody else's before
// this ran and it stays theirs: this published a dashboard against it, it did
// not make it.
func TestAnAdoptedDatasourceSurvivesAnUninstall(t *testing.T) {
	t.Parallel()
	s := &teardownStub{}
	cfg := teardownConfig(t, s.serve(t))
	cfg.Grafana.Datasource.UID = "theirs"
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "dashboard", true, &said); err != nil {
		t.Fatal(err)
	}
	for _, path := range s.deleted {
		if strings.Contains(path, "/api/datasources/") {
			t.Errorf("it deleted a datasource it had only adopted: %v", s.deleted)
		}
	}
	if len(s.deleted) != 1 {
		t.Errorf("deleted = %v, want the dashboard alone", s.deleted)
	}
}

// TestTheLokiDatasourceItMadeGoesToo. It is made under a uid of its own rather
// than a store's, which is how every uninstall up to 2.5.0 left it behind while
// the table on the dashboards page said a datasource it created goes.
func TestTheLokiDatasourceItMadeGoesToo(t *testing.T) {
	t.Parallel()
	s := &teardownStub{}
	cfg := teardownConfig(t, s.serve(t))
	cfg.Sinks.Loki = &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"}
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "dashboard", true, &said); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.deleted, "/api/datasources/uid/ghchronicle-loki") {
		t.Errorf("deleted = %v, want the Loki datasource among them", s.deleted)
	}
}

// TestALokiDatasourceItWasGivenStays, even when somebody gave theirs the name
// this would have made: named in loki_uid, it was only ever read.
func TestALokiDatasourceItWasGivenStays(t *testing.T) {
	t.Parallel()
	s := &teardownStub{}
	cfg := teardownConfig(t, s.serve(t))
	cfg.Sinks.Loki = &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"}
	cfg.Grafana.Datasource.LokiUID = "ghchronicle-loki"
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "dashboard", true, &said); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.deleted, "/api/datasources/uid/ghchronicle-loki") {
		t.Errorf("deleted = %v, and the Loki datasource was one it had only read", s.deleted)
	}
}

// TestEachTargetTakesOnlyItsOwn, so asking for one thing never takes another.
func TestEachTargetTakesOnlyItsOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		target    string
		wantsAPI  bool
		wantsFile bool
	}{
		{"dashboard", true, false},
		{"data", true, false},
		{"state", false, true},
	} {
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			s := &teardownStub{tables: []string{"gh_repo"}}
			cfg := teardownConfig(t, s.serve(t))
			var said strings.Builder
			if err := uninstall(t.Context(), cfg, tc.target, true, &said); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(cfg.StateFile)
			if gone := os.IsNotExist(err); gone != tc.wantsFile {
				t.Errorf("state file gone = %v, want %v", gone, tc.wantsFile)
			}
			if touched := len(s.deleted) > 0; touched != tc.wantsAPI {
				t.Errorf("touched the server = %v, want %v (%v)", touched, tc.wantsAPI, s.deleted)
			}
		})
	}
}

// TestAnUnknownTargetIsRefusedWholesale rather than doing the part it
// understood, which is how a typo becomes a surprise.
func TestAnUnknownTargetIsRefusedWholesale(t *testing.T) {
	t.Parallel()
	s := &teardownStub{tables: []string{"gh_repo"}}
	cfg := teardownConfig(t, s.serve(t))
	var said strings.Builder
	err := uninstall(t.Context(), cfg, "state,dashboards", true, &said)
	if err == nil || !strings.Contains(err.Error(), "dashboards") {
		t.Fatalf("err = %v, want it to name what it did not understand", err)
	}
	if _, statErr := os.Stat(cfg.StateFile); statErr != nil {
		t.Error("it removed the target it did understand before refusing")
	}
	if len(s.deleted) != 0 {
		t.Errorf("it removed %v before refusing", s.deleted)
	}
}

// TestAnEmptyTargetListIsRefused, since -uninstall with nothing after it reads
// like a request to remove everything and is not one.
func TestAnEmptyTargetListIsRefused(t *testing.T) {
	t.Parallel()
	s := &teardownStub{}
	cfg := teardownConfig(t, s.serve(t))
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "  ,  ", true, &said); err == nil {
		t.Error("an empty list was taken as a request to remove something")
	}
}

// TestOneRemovalThatFailsDoesNotStopTheRest, and the run reports it. Stopping
// at the first would leave the job half done with no list of what is left,
// which is the worst of both outcomes.
func TestOneRemovalThatFailsDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	s := &teardownStub{tables: []string{"gh_repo"}, refuseDelete: true}
	cfg := teardownConfig(t, s.serve(t))
	var said strings.Builder
	err := uninstall(t.Context(), cfg, "dashboard,data", true, &said)
	if err == nil {
		t.Fatal("every removal failed and the run reported success")
	}
	if !strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("err = %v, want it to count what did not go", err)
	}
	if !strings.Contains(said.String(), "could not remove") {
		t.Errorf("output = %q, want a line naming each one", said.String())
	}
	// It kept going rather than stopping at the first.
	if got := strings.Count(said.String(), "could not remove"); got < 2 {
		t.Errorf("%d failures reported, want it to have tried them all", got)
	}
}

// TestNothingToRemoveSaysSo rather than printing an empty list and a count of
// zero, which reads like a run that did not look.
func TestNothingToRemoveSaysSo(t *testing.T) {
	t.Parallel()
	s := &teardownStub{}
	cfg := &config.Config{
		Sinks:   config.Sinks{Stdout: true},
		Grafana: &config.Grafana{URL: s.serve(t), Token: "grafana-token"},
	}
	// No store to publish for, so no dashboard of ours, and no state file.
	var said strings.Builder
	if err := uninstall(t.Context(), cfg, "state", true, &said); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said.String(), "nothing of this is here to remove") {
		t.Errorf("output = %q, want it to say there was nothing", said.String())
	}
}
