package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// families is every name the probe prints a line under, in its order.
var families = []string{
	"traffic", "repo", "stars", "starhistory", "account", "pulls", "actions", "artifacts", "activity",
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
		if dumped != (i > 0 && i < summary) {
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
		if !strings.Contains(out.String(), fmt.Sprintf("%-11s ERROR ", family)) {
			t.Errorf("no ERROR line for %s:\n%s", family, out.String())
		}
	}
}

// TestTruncateLeavesALineOfExactlyTheLimit keeps a line that fills the limit
// whole: nothing was cut, so nothing may be marked as cut.
func TestTruncateLeavesALineOfExactlyTheLimit(t *testing.T) {
	t.Parallel()
	if got := truncate("0123", 4); got != "0123" {
		t.Errorf("truncate = %q, want a line of exactly the limit unchanged", got)
	}
}

// ghRequest is one call the windowed stand-in was asked, body and all.
type ghRequest struct {
	method, path string
	query        url.Values
	body         []byte
}

// probeWindows runs the probe against a GitHub that answers only the run
// listing, with a full first page of runs every one of which finished at
// finished, and a Not Found for anything else. It returns every request with
// the moments just before and just after the run, which is as close as a test
// can pin the probe's own clock: the windows it looks back over are counted
// back from a time.Now the test cannot hand it.
func probeWindows(t *testing.T, finished func(start time.Time) time.Time) (asked []ghRequest, start, end time.Time) {
	t.Helper()
	var mu sync.Mutex
	start = time.Now()
	stamp := finished(start).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
		}
		mu.Lock()
		asked = append(asked, ghRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query(), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/repos/"+fakegh.Login+"/hello-world/actions/runs" || r.URL.Query().Get("status") != "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		runs := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			for i := range 100 {
				runs = append(runs, map[string]any{
					"id": 5000 + i, "run_attempt": 1, "status": "in_progress", "updated_at": stamp,
				})
			}
		}
		out, err := json.Marshal(map[string]any{"total_count": 100, "workflow_runs": runs})
		if err != nil {
			t.Errorf("encoding the runs: %v", err)
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	c := ghapi.New("test-token", 10*time.Second)
	c.SetBaseURL(srv.URL)
	var out, errOut strings.Builder
	if status := run(t.Context(), c, []string{fakegh.Login + "/hello-world"}, "", &out, &errOut); status != 0 {
		t.Fatalf("probe = %d, %q, want a run that reports its failures and goes on", status, errOut.String())
	}
	end = time.Now()
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(asked), start, end
}

// TestProbeWalksTheRunsOfTheLastWeek pages on past a full page of runs that
// finished yesterday, because the probe's actions window is the last seven
// days and a run from yesterday sits inside it. A window counted forward from
// now would call every run older than the bound and stop after one page.
func TestProbeWalksTheRunsOfTheLastWeek(t *testing.T) {
	t.Parallel()
	asked, _, _ := probeWindows(t, func(start time.Time) time.Time { return start.Add(-24 * time.Hour) })
	pages := map[string]bool{}
	for _, r := range asked {
		if strings.HasSuffix(r.path, "/actions/runs") && r.query.Get("status") == "" {
			pages[r.query.Get("page")] = true
		}
	}
	if !pages["1"] || !pages["2"] {
		t.Errorf("the run listing was asked for pages %v, want the second one too for runs inside the week", pages)
	}
}

// TestProbeAsksForTheCommitsOfTheLastThirtyDays sends the commit history the
// bound thirty days back from the run, not thirty days ahead of it, which
// would ask for a history that has not happened yet and get none.
func TestProbeAsksForTheCommitsOfTheLastThirtyDays(t *testing.T) {
	t.Parallel()
	asked, start, end := probeWindows(t, func(start time.Time) time.Time { return start })
	var since []string
	for _, r := range asked {
		var env struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.path != "/graphql" || json.Unmarshal(r.body, &env) != nil || !strings.Contains(env.Query, "history(") {
			continue
		}
		s, _ := env.Variables["since"].(string)
		since = append(since, s)
	}
	if len(since) == 0 {
		t.Fatal("the commit history was never asked for")
	}
	earliest := start.AddDate(0, 0, -30).Truncate(time.Second)
	latest := end.AddDate(0, 0, -30)
	for _, s := range since {
		got, err := time.Parse(time.RFC3339, s)
		if err != nil || got.Before(earliest) || got.After(latest) {
			t.Errorf("the commit history was asked since %q, want thirty days before the run, between %s and %s",
				s, earliest.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
		}
	}
}

// TestProbeAsksForTheFailuresOfTheLastThirtyDays filters the failed runs from
// a creation date counted back from thirty days ago, re-run reach included.
// Counted forward, the filter would start yesterday and every failure of the
// month before it would go unfetched.
func TestProbeAsksForTheFailuresOfTheLastThirtyDays(t *testing.T) {
	t.Parallel()
	asked, start, end := probeWindows(t, func(start time.Time) time.Time { return start })
	day := 24 * time.Hour
	// The thirty one days a failure may have been created before it
	// finished, which the collector reaches back on top of the window.
	reach := 31 * day
	want := map[string]bool{
		">=" + start.AddDate(0, 0, -30).Add(-reach).UTC().Truncate(day).Format(time.RFC3339): true,
		">=" + end.AddDate(0, 0, -30).Add(-reach).UTC().Truncate(day).Format(time.RFC3339):   true,
	}
	listed := 0
	for _, r := range asked {
		if !strings.HasSuffix(r.path, "/actions/runs") || r.query.Get("status") != "failure" {
			continue
		}
		listed++
		if created := r.query.Get("created"); !want[created] {
			t.Errorf("the failed runs were asked created %q, want one of %v", created, want)
		}
	}
	if listed == 0 {
		t.Fatal("the failed runs were never asked for")
	}
}
