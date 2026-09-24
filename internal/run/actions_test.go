package run

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// runListPages is what each run list request asked for, and jobListings how
// many times a run's jobs were listed, in one slice of the fake's log.
func runListPages(t *testing.T, requests []fakegh.Request) (perPage []string, jobListings int) {
	t.Helper()
	for _, req := range requests {
		switch {
		case strings.HasSuffix(req.Path, "/actions/runs"):
			q, err := url.ParseQuery(req.Query)
			if err != nil {
				t.Fatal(err)
			}
			perPage = append(perPage, q.Get("per_page"))
		case strings.Contains(req.Path, "/actions/runs/") && strings.HasSuffix(req.Path, "/jobs"):
			jobListings++
		}
	}
	return perPage, jobListings
}

// TestASweepDoesNotListTheJobsItAlreadyWrote runs actions three times in one
// process: the first sweep asks for pages of a hundred and lists the jobs of
// every completed run; the next ordinary sweep asks for pages of thirty and
// lists no jobs, because the runs are the same ones; a backfill lists them
// all again, whatever the process remembers.
func TestASweepDoesNotListTheJobsItAlreadyWrote(t *testing.T) {
	t.Parallel()
	r, fake, log := fakeRunner(t)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	pages, listed := runListPages(t, fake.Requests())
	if len(pages) == 0 || listed == 0 {
		t.Fatalf("the first sweep made %d run list requests and %d job listings", len(pages), listed)
	}
	for _, p := range pages {
		if p != "100" {
			t.Errorf("the first sweep asked for per_page=%s, want a hundred", p)
		}
	}

	// Only actions is due again: everything else was marked a moment ago.
	before := len(fake.Requests())
	every, _ := r.Cfg.Interval("actions")
	r.State.LastRun["actions"] = time.Now().Add(-2 * every)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v\n%s", err, log)
	}
	pages, listed = runListPages(t, fake.Requests()[before:])
	if len(pages) == 0 {
		t.Fatal("the second sweep did not list the runs at all")
	}
	for _, p := range pages {
		if p != "30" {
			t.Errorf("an ordinary sweep asked for per_page=%s, want thirty", p)
		}
	}
	if listed != 0 {
		t.Errorf("the second sweep listed jobs %d times for runs whose jobs it already wrote", listed)
	}

	// A backfill is its own process and primes every family; here only the
	// memory of the two sweeps above matters, so actions is made due again
	// under the backfill's collectors instead.
	before = len(fake.Requests())
	r.State.LastRun["actions"] = time.Now().Add(-2 * every)
	r.Backfill, r.BackfillSince = true, time.Now()
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("backfill Once: %v\n%s", err, log)
	}
	pages, listed = runListPages(t, fake.Requests()[before:])
	for _, p := range pages {
		if p != "100" {
			t.Errorf("a backfill asked for per_page=%s, want a hundred", p)
		}
	}
	if listed == 0 {
		t.Error("a backfill skipped the jobs an earlier sweep wrote; it must expand every run")
	}
}

// TestTheFirstSweepRemembersEveryRepositorysRuns: actions is built once per
// repository, and on the first sweep the family is not yet marked for any
// of them. What the first repository's collector remembered must still be
// there when the second's is built, or the next sweep lists the jobs of
// every repository but the last again.
func TestTheFirstSweepRemembersEveryRepositorysRuns(t *testing.T) {
	t.Parallel()
	r, _, _ := fakeRunner(t)
	now := time.Now()
	first := r.actions(now)
	if first.PerPage != 100 || first.Walk.Pages != 10 {
		t.Errorf("the first sweep asks for %d pages of %d, want 10 of 100", first.Walk.Pages, first.PerPage)
	}
	seen := collect.RunKey{ID: 1, Attempt: 1}
	first.Expanded[seen] = struct{}{}
	second := r.actions(now)
	if _, ok := second.Expanded[seen]; !ok {
		t.Error("the second repository of the first sweep forgot what the first one expanded")
	}

	// An ordinary sweep: pages of thirty, and as many of them as reach the
	// two hundred runs two pages of a hundred did.
	every, _ := r.Cfg.Interval("actions")
	r.State.LastRun["actions"] = now.Add(-every)
	ordinary := r.actions(now)
	if ordinary.PerPage != 30 || ordinary.Walk.Pages*ordinary.PerPage < 200 {
		t.Errorf("an ordinary sweep asks for %d pages of %d, want pages of 30 reaching 200 runs",
			ordinary.Walk.Pages, ordinary.PerPage)
	}
	if _, ok := ordinary.Expanded[seen]; !ok {
		t.Error("an ordinary sweep forgot what the first sweep expanded")
	}
}

// TestTheActionsWindowIsTwiceTheCadenceAndNeverUnderTwoHours: a quick
// cadence still reads two hours, so the exporter does not go empty between
// builds; a slow one reads two of its own cadences, so a late sweep still
// overlaps; and the first sweep of a fresh install reads a month.
func TestTheActionsWindowIsTwiceTheCadenceAndNeverUnderTwoHours(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	if got := r.actions(now); !got.Since.Equal(now.AddDate(0, 0, -30)) {
		t.Errorf("the first sweep reads from %s, want a month back", got.Since)
	}
	r.State.Mark("actions", now.Add(-15*time.Minute))
	if every, _ := r.Cfg.Interval("actions"); every != 15*time.Minute {
		t.Fatalf("actions cadence = %s, want the fifteen minute default", every)
	}
	if got := r.actions(now); !got.Since.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("a fifteen minute cadence reads from %s, want two hours back", got.Since)
	}
	r.Cfg.Every = config.Every{Families: map[string]string{"actions": "3h"}}
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := r.actions(now); !got.Since.Equal(now.Add(-6 * time.Hour)) {
		t.Errorf("a three hour cadence reads from %s, want two cadences back", got.Since)
	}
}
