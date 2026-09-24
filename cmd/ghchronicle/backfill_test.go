package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// TestAfterAPassTheBackfillDecidesByWhatItRecorded covers every way a pass can
// end. The point of the table is the one row that is easy to get wrong: a pass
// that recorded nothing stops even though there is work left, because the
// obstacle is not one that waiting clears.
func TestAfterAPassTheBackfillDecidesByWhatItRecorded(t *testing.T) {
	t.Parallel()
	left := []string{"actions"}
	for name, tc := range map[string]struct {
		stopped bool
		left    []string
		retry   time.Duration
		gained  bool
		passes  int
		want    verdict
	}{
		"nothing left":             {left: nil, retry: time.Hour, gained: true, passes: 1, want: stop},
		"going back was not asked": {left: left, retry: 0, gained: true, passes: 1, want: stop},
		"the run was stopped":      {stopped: true, left: left, retry: time.Hour, gained: true, passes: 1, want: stop},
		"it recorded nothing":      {left: left, retry: time.Hour, gained: false, passes: 1, want: stuck},
		"passes ran out":           {left: left, retry: time.Hour, gained: true, passes: retryLimit, want: atLimit},
		"work left and moving":     {left: left, retry: time.Hour, gained: true, passes: 1, want: again},
		// Being stopped outranks everything: a walk asked to end does not sit
		// through an hour's wait to try again.
		"stopped with work left": {stopped: true, left: left, retry: time.Hour, gained: false, passes: 1, want: stop},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := afterPass(tc.stopped, tc.left, tc.retry, tc.gained, tc.passes); got != tc.want {
				t.Errorf("afterPass = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBackfillStatusSaysThereIsNoneWhenThereIsNone. A status command that
// exits non-zero for an ordinary state is one nobody can put in a script, and
// "there is no backfill in progress" is an ordinary state.
func TestBackfillStatusSaysThereIsNoneWhenThereIsNone(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "sinks:\n  file:\n    path: "+filepath.Join(dir, "points.lp")+"\n")

	got := runCommand(t, "-config", cfg, "-backfill-status")
	if got.status != notExited {
		t.Fatalf("-backfill-status = %d, want a clean return:\n%s", got.status, got.stderr)
	}
	if !strings.Contains(got.stdout, "no backfill in progress") {
		t.Errorf("it does not say there is none:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, filepath.Join(dir, "state-progress.json")) {
		t.Errorf("it does not name the file it looked for:\n%s", got.stdout)
	}
}

// TestBackfillStatusReadsTheCheckpointAWalkLeft. The file was always meant to
// be read, and until this existed reading it meant knowing the path and
// parsing JSON by hand.
func TestBackfillStatusReadsTheCheckpointAWalkLeft(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "sinks:\n  file:\n    path: "+filepath.Join(dir, "points.lp")+"\n")

	// Written by the same code a walk writes it with, rather than by hand: a
	// status that reads a shape nothing produces proves nothing.
	at := time.Date(2026, 9, 18, 2, 40, 42, 0, time.UTC)
	progress, err := run.OpenProgress(filepath.Join(dir, "state-progress.json"), "test-build",
		run.Scope{Families: []string{"repo", "actions"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = progress.FinishFamily("repo", 3, 90, at); err != nil {
		t.Fatal(err)
	}
	if err = progress.WroteRepo("actions", "octocat/hello-world", 7, at); err != nil {
		t.Fatal(err)
	}

	got := runCommand(t, "-config", cfg, "-backfill-status")
	if got.status != notExited {
		t.Fatalf("-backfill-status = %d, want a clean return:\n%s", got.status, got.stderr)
	}
	for _, want := range []string{
		"backfill in progress",
		"1 of 2 complete",
		"actions, 1 repositories written",
		"left         actions",
		"-backfill",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the report lacks %q:\n%s", want, got.stdout)
		}
	}
}

// TestBackfillStatusNeedsNoToken. The credential is there because a sweep asks
// GitHub; this run asks nothing. Requiring it would put the status of a walk
// behind the secret the walk needs rather than the one its reader has, which
// on a server is a systemd EnvironmentFile nobody has sourced.
func TestBackfillStatusNeedsNoToken(t *testing.T) {
	dir := t.TempDir()
	// A config naming no token at all, which is what a reader meets when the
	// real one comes from the environment of the service.
	body := "github:\n  base_url: http://127.0.0.1:1\ntargets:\n  user: " + fakegh.Login +
		"\nstate_file: " + filepath.Join(dir, "state.json") +
		"\nsinks:\n  file:\n    path: " + filepath.Join(dir, "points.lp") + "\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")

	if got := runCommand(t, "-config", path, "-backfill-status"); got.status != notExited ||
		!strings.Contains(got.stdout, "no backfill in progress") {
		t.Errorf("-backfill-status with no token = status %d, stdout %q, stderr %q",
			got.status, got.stdout, got.stderr)
	}
	// And the same configuration is still refused for a run that does ask
	// GitHub, which is the rule this waives and does not remove.
	if got := runCommand(t, "-config", path, "-once"); got.status == notExited {
		t.Error("a sweep ran with no token, so waiving it for the status waived it for everything")
	}
}

// TestSinceAndShownPathSayTheAwkwardCases. Both are one line of the report,
// and both have a case that is easy to leave printing something nobody can
// read: a clock that moved backwards, and a configuration that keeps no state
// file and therefore no checkpoint.
func TestSinceAndShownPathSayTheAwkwardCases(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	if got := since(now.Add(-90*time.Second), now); got != 90*time.Second {
		t.Errorf("since = %v, want 1m30s", got)
	}
	// A clock that moved backwards between the write and the read. Reporting
	// the future reads as a bug in the walk rather than in the clock.
	if got := since(now.Add(time.Hour), now); got != 0 {
		t.Errorf("an instant in the future is %v ago, want none", got)
	}
	if got := shownPath("/var/lib/ghchronicle/state-progress.json"); got != "/var/lib/ghchronicle/state-progress.json" {
		t.Errorf("shownPath rewrote a path it should have printed: %q", got)
	}
	if got := shownPath(""); !strings.Contains(got, "state_file") {
		t.Errorf("with no state file it says %q, which does not tell the reader why there is no checkpoint", got)
	}
}

// TestTheBackfillGoesBackAndThenStopsWhenAPassRecordsNothing drives the loop
// itself, which afterPass only decides for.
//
// The checkpoint's scope names a family the configuration does not enable, so
// there is always one left however many passes run. The first pass records the
// families that do run, which is a gain and sends it back; the second records
// nothing new, which is the obstacle that waiting does not clear, and it stops
// and says so. That is every branch of the loop, the wait and the reprime
// included, in a second.
func TestTheBackfillGoesBackAndThenStopsWhenAPassRecordsNothing(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg, err := config.LoadWith(writeConfig(t, dir, gh.URL(),
		"groups: [account]\nsinks:\n  file:\n    path: "+filepath.Join(dir, "points.lp")+"\n"),
		config.Relax{})
	if err != nil {
		t.Fatal(err)
	}
	// A reserve of one, for the reason the end-to-end suite gives where it
	// does the same: an unbounded walk against the fake spends down to the
	// reserve and then parks for an hour waiting for a window that never turns
	// over. The runner reads it from the configuration, not from the client.
	cfg.GitHub.ReserveRate = 1
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(gh.URL())
	api.SetReserve(1, true)

	// A family nothing enables, so the walk can never finish covering it.
	scope := run.ScopeOf(cfg, "")
	scope.Families = append(scope.Families, "never-enabled")
	progress, err := run.OpenProgress(cfg.BackfillProgressFile(), "test-build", scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	runner := &run.Runner{
		Cfg: cfg, API: api, Sinks: nil, State: run.LoadState(cfg.StateFile),
		Log:      slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Prime:    true,
		Backfill: true,
		Progress: progress,
		// Bounded, and not for speed. An unbounded backfill walks billing back
		// month by month until three answer empty, and the fake answers every
		// month there is, so the walk never reaches an end to stop at.
		BackfillSince: time.Now().AddDate(0, 0, -30),
	}

	if err = walkUntilDoneOrStuck(t.Context(), runner, time.Millisecond, runner.Log); err != nil {
		t.Fatalf("the walk: %v\n%s", err, log.String())
	}
	said := log.String()
	for _, want := range []string{
		"waiting before going back for the families this pass left",
		"this pass recorded nothing new",
		"never-enabled",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the log does not carry %q:\n%s", want, said)
		}
	}
}
