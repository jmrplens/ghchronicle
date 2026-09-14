package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// families is every name the probe prints a line under, in its order.
var families = []string{
	"traffic", "repo", "stars", "account", "pulls", "actions", "artifacts", "activity",
	"discuss", "billing", "profile", "commits", "activity2", "analyses", "forks",
	"planning", "outbound", "history", "settings", "rulesets", "joblogs", "events", "notifs",
}

// probeFake runs the probe against the fake GitHub the end-to-end suites
// collect from, and returns its status and both streams.
func probeFake(t *testing.T, dump string, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	fake := fakegh.New(t, "../../test/e2e/testdata")
	c := ghapi.New("test-token", 10*time.Second)
	c.SetBaseURL(fake.URL())
	var out, errOut strings.Builder
	status = run(t.Context(), c, args, dump, &out, &errOut)
	return status, out.String(), errOut.String()
}

// TestProbePrintsOneLinePerFamily runs every collector against the fake and
// reports each one on a line of its own, in order, then the quota it spent.
func TestProbePrintsOneLinePerFamily(t *testing.T) {
	t.Parallel()
	status, stdout, stderr := probeFake(t, "", fakegh.Login+"/hello-world")
	if status != 0 || stderr != "" {
		t.Fatalf("probe = %d, %q, want a clean run", status, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != len(families)+2 {
		t.Fatalf("probe printed %d lines, want one per family and the quota:\n%s", len(lines), stdout)
	}
	collected := 0
	for i, family := range families {
		fields := strings.Fields(lines[i])
		if len(fields) < 2 || fields[0] != family {
			t.Errorf("line %d = %q, want the %s family", i, lines[i], family)
			continue
		}
		if fields[1] != "ERROR" {
			collected++
			if len(fields) < 3 || fields[2] != "points" {
				t.Errorf("line %d = %q, want a count of points", i, lines[i])
			}
		}
	}
	// The fake serves every family the collector sweeps, so most of them come
	// back with something; the probe is useless if they all fail alike.
	if collected < len(families)/2 {
		t.Errorf("only %d of %d families collected anything:\n%s", collected, len(families), stdout)
	}
	if !strings.HasPrefix(lines[len(lines)-1], "quota ") {
		t.Errorf("last line = %q, want the quota the probe spent", lines[len(lines)-1])
	}
}

// TestProbeDumpsTheFamilyAsked prints every point of the family GHC_DUMP names
// in full, before that family's summary, and nothing of any other.
func TestProbeDumpsTheFamilyAsked(t *testing.T) {
	t.Parallel()
	status, stdout, _ := probeFake(t, "repo", fakegh.Login+"/hello-world")
	if status != 0 {
		t.Fatalf("probe = %d, want a clean run", status)
	}
	lines := strings.Split(stdout, "\n")
	summary := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "repo ") {
			summary = i
			break
		}
	}
	if summary < 2 || !strings.HasPrefix(lines[0], "traffic ") {
		t.Fatalf("the repo family was not dumped between the traffic line and its own:\n%s", stdout)
	}
	for i, line := range lines {
		dumped := strings.HasPrefix(line, "gh_")
		if want := i > 0 && i < summary; dumped != want {
			t.Errorf("line %d = %q, want only the repo family's points dumped, right before its summary", i, line)
		}
	}
}

// TestProbeRefusesWhatIsNotARepository names the argument instead of stopping
// on an index out of range, which is what a name without an owner used to do.
func TestProbeRefusesWhatIsNotARepository(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"hello-world", "octocat/", "/hello-world"} {
		var out, errOut strings.Builder
		status := run(t.Context(), ghapi.New("", 0), []string{arg}, "", &out, &errOut)
		if status != 2 || out.String() != "" || !strings.Contains(errOut.String(), `"`+arg+`" is not a repository`) {
			t.Errorf("probe %s = %d, %q, %q, want 2 and the argument named", arg, status, out.String(), errOut.String())
		}
	}
}

// TestTruncateMarksWhatItCut leaves a short line alone and marks a long one.
func TestTruncateMarksWhatItCut(t *testing.T) {
	t.Parallel()
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q, want a short line unchanged", got)
	}
	if got := truncate("0123456789", 4); got != "0123…" {
		t.Errorf("truncate = %q, want the first four bytes and a mark", got)
	}
}

// TestProbeWithoutAnArgumentAsksForTheDefault probes defaultRepo when no
// repository is named.
func TestProbeWithoutAnArgumentAsksForTheDefault(t *testing.T) {
	t.Parallel()
	fake := fakegh.New(t, "../../test/e2e/testdata")
	c := ghapi.New("test-token", 10*time.Second)
	c.SetBaseURL(fake.URL())
	var out, errOut strings.Builder
	if status := run(t.Context(), c, nil, "", &out, &errOut); status != 0 {
		t.Fatalf("probe = %d, %q, want a run that reports its failures and goes on", status, errOut.String())
	}
	for _, r := range fake.Requests() {
		if r.Path == "/repos/"+defaultRepo {
			return
		}
	}
	t.Errorf("the fake was never asked for /repos/%s", defaultRepo)
}

// TestProbeReportsAFailingCollectorAndGoesOn prints a collector's error on its
// line and moves to the next, so one broken family cannot hide the others.
// A canceled run is the one way to make all of them fail at once.
func TestProbeReportsAFailingCollectorAndGoesOn(t *testing.T) {
	t.Parallel()
	fake := fakegh.New(t, "../../test/e2e/testdata")
	c := ghapi.New("test-token", 10*time.Second)
	c.SetBaseURL(fake.URL())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, errOut strings.Builder
	if status := run(ctx, c, []string{fakegh.Login + "/hello-world"}, "", &out, &errOut); status != 0 {
		t.Fatalf("probe = %d, want failed collectors reported rather than fatal", status)
	}
	for _, family := range families {
		if !strings.Contains(out.String(), fmt.Sprintf("%-9s ERROR ", family)) {
			t.Errorf("no ERROR line for %s:\n%s", family, out.String())
		}
	}
}
