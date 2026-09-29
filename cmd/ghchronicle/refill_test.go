package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// TestBackfillStatusSaysWhereTheRefillIs: the refill of a migration keeps a
// checkpoint of its own, which the status prints after the backfill's, with
// what it writes where and the command that resumes it.
func TestBackfillStatusSaysWhereTheRefillIs(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	path := writeConfig(t, dir, gh.URL(), "sinks:\n  file:\n    path: "+filepath.Join(dir, "points.lp")+"\n")
	cfg, err := config.LoadWith(path, config.Relax{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 18, 2, 40, 42, 0, time.UTC)
	refill := run.ScopeOf(cfg, "2023-11-14").Narrowed([]string{"discussions", "outbound"},
		map[string][]string{"file": {"gh_discussion_comment"}})
	progress, err := run.OpenProgress(cfg.RefillProgressFile(), "test-build", refill, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = progress.FinishFamily("outbound", 0, 3, at); err != nil {
		t.Fatal(err)
	}

	got := runCommand(t, "-config", path, "-backfill-status")
	if got.status != notExited {
		t.Fatalf("-backfill-status = %d:\n%s", got.status, got.stderr)
	}
	for _, want := range []string{
		"no backfill in progress\n",
		"refill in progress, reading back what a migration cleared\n",
		"  writing      gh_discussion_comment to file\n",
		"  left         discussions\n",
		"  checkpoint   " + filepath.Join(dir, "state-refill.json") + "\n",
		"  resume       ghchronicle -config " + path + " -migrate -yes, or the next start under migrate: auto\n",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the status does not say %q:\n%s", want, got.stdout)
		}
	}
}

// TestARefillOwedIsPaidUnderAutoAndSaidUnderWarn: a store an earlier run
// cleared and did not finish reading back is read before the first sweep
// under migrate: auto, and only said, with the command that reads it, under
// migrate: warn.
func TestARefillOwedIsPaidUnderAutoAndSaidUnderWarn(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	for _, tc := range []struct {
		name, body string
		paid       bool
	}{
		{"auto", "", true},
		{"warn", "migrate: warn\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ways := &recordingWays{}
			ways.install(t)
			dir := t.TempDir()
			// A store in this release's shape already: the old rows are
			// gone, and only the record says their history is owed.
			store := &oldComments{users: []string{fakegh.Login}}
			path := upgradedConfig(t, dir, gh.URL(), store.start(t), tc.body)
			cfg, err := config.LoadWith(path, config.Relax{})
			if err != nil {
				t.Fatal(err)
			}
			state := run.LoadState(cfg.StateFile)
			migrate.Stamp(state, cfg, version)
			state.Stores["influxdb"].MarkApplied(commentsID, time.Now())
			state.Stores["influxdb"].OweRefill(commentsID, "gh_discussion_comment", []string{"discussions", "outbound"},
				time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC))
			if err = state.Save(); err != nil {
				t.Fatal(err)
			}

			got := runCommand(t, "-config", path, "-once")
			if got.status != notExited {
				t.Fatalf("-once = %d:\n%s", got.status, got.stderr)
			}
			paid := slices.Equal(ways.refilled, []string{"influxdb:gh_discussion_comment"})
			owedAfter := run.LoadState(cfg.StateFile).Stores["influxdb"].Refill != nil
			if paid != tc.paid || owedAfter == tc.paid {
				t.Errorf("refilled %v, still owed %v:\n%s", ways.refilled, owedAfter, got.stderr)
			}
			said := `msg="refill owed: a store a migration cleared does not hold that history yet" sink=influxdb ` +
				`measurements=gh_discussion_comment families=discussions,outbound since=2023-11-14`
			if strings.Contains(got.stderr, said) == tc.paid {
				t.Errorf("under %s the refill owed is said %v:\n%s", tc.name, !tc.paid, got.stderr)
			}
			if !tc.paid && !strings.Contains(got.stderr, `apply="ghchronicle -config `+path+` -migrate -yes"`) {
				t.Errorf("the warning does not name the command that reads it:\n%s", got.stderr)
			}
		})
	}
}

// TestAServiceStoppedDuringTheRefillEndsThere: a stop while the service reads
// back what a migration cleared ends the run cleanly, with the refill still
// owed for the next start, and starts no sweep the stop has already cut.
func TestAServiceStoppedDuringTheRefillEndsThere(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	stop, cancel := context.WithCancel(t.Context())
	defer cancel()
	signalsFrom(stop, t)
	ways := &recordingWays{during: cancel}
	ways.install(t)
	dir := t.TempDir()
	store := &oldComments{users: []string{fakegh.Login}}
	path := upgradedConfig(t, dir, gh.URL(), store.start(t), "")

	got := runCommand(t, "-config", path)
	if got.status != notExited {
		t.Fatalf("the service stopped during the refill = %d:\n%s", got.status, got.stderr)
	}
	for _, never := range []string{`msg="ghchronicle running"`, `msg="sweep failed"`} {
		if strings.Contains(got.stderr, never) {
			t.Errorf("the service went on to sweep after the stop: %s\n%s", never, got.stderr)
		}
	}
	cfg, err := config.LoadWith(path, config.Relax{})
	if err != nil {
		t.Fatal(err)
	}
	if run.LoadState(cfg.StateFile).Stores["influxdb"].Refill == nil {
		t.Error("the refill a stop cut short is not owed any more")
	}
}
