package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// TestFamiliesGoesWithBackfillAndNamesFamilies: -families narrows a
// backfill and nothing else, and a name that is not a family is a command
// line that does not parse, exit 2, rather than a walk of nothing that ends
// complete.
func TestFamiliesGoesWithBackfillAndNamesFamilies(t *testing.T) {
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"-families", "discussions"}, "-families goes with -backfill"},
		{[]string{"-once", "-families", "discussions"}, "-families goes with -backfill"},
		{[]string{"-backfill", "-families", "discussions,dicussions"}, `"dicussions" is not a family`},
		{[]string{"-backfill", "-families", " , "}, `"" is not a family`},
		{[]string{"-backfill", "-families", "account-group"}, "-groups lists them"},
	} {
		got := runCommand(t, tc.args...)
		if got.status != 2 || !strings.Contains(got.stderr, tc.says) {
			t.Errorf("%v = %d, %q, want 2 and %q", tc.args, got.status, got.stderr, tc.says)
		}
	}
	o, err := parseFlags([]string{"ghchronicle", "-backfill", "-families", "outbound, discussions,outbound"}, &strings.Builder{})
	if err != nil || !slices.Equal(o.only, []string{"discussions", "outbound"}) {
		t.Errorf("-families outbound, discussions,outbound = %v, %v", o.only, err)
	}
}

// TestABackfillOfSomeFamiliesWalksThoseAlone: -backfill -families walks the
// families named into every store, and none other; a family the
// configuration switches off is refused before anything is asked, since the
// walk would read none of it and end complete.
func TestABackfillOfSomeFamiliesWalksThoseAlone(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "every:\n  families:\n    history: 0\nsinks:\n  file:\n    path: "+
		filepath.Join(dir, "points.lp")+"\n")

	got := runCommand(t, "-config", cfg, "-backfill", "-families", "discussions,outbound", "-backfill-since", "30d")
	if got.status != notExited {
		t.Fatalf("-backfill -families = %d:\n%s", got.status, got.stderr)
	}
	var families []string
	for line := range strings.SplitSeq(got.stderr, "\n") {
		if !strings.Contains(line, "msg=written") {
			continue
		}
		_, rest, _ := strings.Cut(line, " family=")
		family, _, _ := strings.Cut(rest, " ")
		if !slices.Contains(families, family) {
			families = append(families, family)
		}
	}
	slices.Sort(families)
	if !slices.Equal(families, []string{"collector", "discussions", "outbound"}) {
		t.Errorf("the backfill wrote %v, want discussions, outbound and the collector's own rows:\n%s", families, got.stderr)
	}
	if !strings.Contains(got.stderr, `msg="backfill of some families only" families=discussions,outbound`) ||
		!strings.Contains(got.stderr, `msg="backfill complete, its checkpoint is removed"`) {
		t.Errorf("the log does not say what was walked and that it ended:\n%s", got.stderr)
	}

	asked := len(gh.Requests())
	got = runCommand(t, "-config", cfg, "-backfill", "-families", "history")
	if got.status != 1 || !strings.Contains(got.stderr, "this configuration switches history off") {
		t.Errorf("a family switched off = %d:\n%s", got.status, got.stderr)
	}
	if n := len(gh.Requests()) - asked; n != 0 || strings.Contains(got.stderr, "migration") {
		t.Errorf("a family switched off was refused after %d requests to GitHub and a migration check:\n%s",
			n, got.stderr)
	}
}

// TestBackfillStatusSaysHowToResumeANarrowedWalk: a checkpoint of some
// families is resumed by the same families and the same bound, which the
// resume line names, since a resume under anything else is refused as
// another walk.
func TestBackfillStatusSaysHowToResumeANarrowedWalk(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	path := writeConfig(t, dir, gh.URL(), "sinks:\n  file:\n    path: "+filepath.Join(dir, "points.lp")+"\n")
	cfg, err := config.LoadWith(path, config.Relax{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 18, 2, 40, 42, 0, time.UTC)
	narrowed := run.ScopeOf(cfg, "90d").Narrowed([]string{"discussions", "outbound"}, nil)
	backfill, err := run.OpenProgress(cfg.BackfillProgressFile(), "test-build", narrowed, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = backfill.FinishFamily("outbound", 0, 3, at); err != nil {
		t.Fatal(err)
	}

	got := runCommand(t, "-config", path, "-backfill-status")
	if got.status != notExited {
		t.Fatalf("-backfill-status = %d:\n%s", got.status, got.stderr)
	}
	for _, want := range []string{
		"backfill in progress\n",
		"  families     1 of 2 complete\n",
		"  resume       ghchronicle -config " + path + " -backfill -backfill-since 90d -families discussions,outbound\n",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the status does not say %q:\n%s", want, got.stdout)
		}
	}
}
