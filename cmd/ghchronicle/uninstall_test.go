package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// teardownStub is a Grafana and an InfluxDB in one server, which is all the
// uninstall talks to, and it remembers every deletion it was asked for.
type teardownStub struct {
	tables []string
	// refuseDelete makes every removal fail, which is how the run’s behavior
	// when one does is read.
	refuseDelete bool
	deleted      []string
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
			}
			s.deleted = append(s.deleted, target)
			return
		}
		switch {
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
