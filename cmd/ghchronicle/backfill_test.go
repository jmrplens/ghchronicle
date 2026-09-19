package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/run"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
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
