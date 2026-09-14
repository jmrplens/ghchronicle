package run

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
)

// pullsRunner is a runner with the default cadences and nothing collected
// yet, enough to ask it which pull request query it would send.
func pullsRunner(t *testing.T) *Runner {
	t.Helper()
	cfg := &config.Config{
		GitHub:       config.GitHub{Token: "token"},
		Targets:      config.Targets{User: "o"},
		AllowNoSinks: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &Runner{Cfg: cfg, API: ghapi.New("token", 0), State: LoadState(""), Log: slog.New(slog.DiscardHandler)}
}

// TestPullsPageFollowsTheTotals is the sizing: the page is what the
// repository holds, rounded up to the next size the gateway prices, never
// larger than fifty and fifty until the totals family has said anything.
func TestPullsPageFollowsTheTotals(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Now()
	repo := collect.Repo{Owner: "o", Name: "n", FullName: "o/n"}
	if got := r.pulls(repo, now); got.First != 50 || !got.Walk.Since.IsZero() || got.Walk.Pages != 0 {
		t.Errorf("before totals ran = %+v, want a whole page of fifty", got)
	}
	r.noteCounts(nil)
	r.counts["o/n"] = collect.ItemCounts{Pulls: 3, Issues: 0}
	if got := r.pulls(repo, now); got.First != 5 {
		t.Errorf("three pull requests = %+v, want a page of five", got)
	}
	r.counts["o/n"] = collect.ItemCounts{Pulls: 12, Issues: 44}
	if got := r.pulls(repo, now); got.First != 50 {
		t.Errorf("forty-four issues = %+v, want a page of fifty", got)
	}
	// A page that shrank cannot lose a row: whatever the total, the page
	// covers it.
	for total := range 60 {
		r.counts["o/n"] = collect.ItemCounts{Pulls: total}
		if got := r.pulls(repo, now); got.First < min(total, 50) {
			t.Errorf("%d pull requests = a page of %d", total, got.First)
		}
	}
}

// TestPullsSweepReadsWhatChangedAndAWholePageOnceADay is the shape decided
// on 2026-09-11: an ordinary sweep walks by updatedAt back to twice the
// cadence, ten at a time, and once a day the family reads a whole page with
// no bound, which is the only read that rewrites an open pull request
// nobody touched.
func TestPullsSweepReadsWhatChangedAndAWholePageOnceADay(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Now()
	repo := collect.Repo{Owner: "o", Name: "n", FullName: "o/n"}
	every, _ := r.Cfg.Interval("issues")

	// Never read a whole page: this sweep is the daily one.
	if !r.fullPassDue("issues", now) {
		t.Fatal("a family that never read a whole page is due one")
	}
	if got := r.pulls(repo, now); !got.Walk.Since.IsZero() || got.First != 50 {
		t.Errorf("the daily pass = %+v, want a whole page with no bound", got)
	}
	r.State.MarkFull("issues", now)

	// The sweep after it reads what changed, twice the cadence back.
	sweep := r.pulls(repo, now)
	if want := now.Add(-2 * every); !sweep.Walk.Since.Equal(want) {
		t.Errorf("Since = %s, want two cadences back, %s", sweep.Walk.Since, want)
	}
	if sweep.First != 10 || sweep.Walk.Pages >= 0 {
		t.Errorf("a sweep = %+v, want ten at a time and as many pages as the bound needs", sweep)
	}
	// A repository smaller than ten is asked for what it holds.
	r.noteCounts(nil)
	r.counts["o/n"] = collect.ItemCounts{Pulls: 4}
	if got := r.pulls(repo, now); got.First != 5 {
		t.Errorf("a sweep of a repository with four pull requests = %+v, want five", got)
	}

	// A sweep that comes late reaches back to the one before it, so nothing
	// that was touched while the service was down waits for the daily pass.
	r.State.Mark("issues", now.Add(-5*every))
	if got := r.pulls(repo, now); !got.Walk.Since.Equal(now.Add(-6 * every)) {
		t.Errorf("Since after a five-cadence gap = %s, want one cadence before the last run", got.Walk.Since)
	}

	// The next UTC day the whole page is due again.
	later := now.Add(24 * time.Hour)
	if got := r.pulls(repo, later); !got.Walk.Since.IsZero() {
		t.Errorf("a day later = %+v, want the whole page again", got)
	}

	// A backfill is neither: every page back to its own bound.
	r.Backfill, r.BackfillSince = true, now.AddDate(-1, 0, 0)
	if got := r.pulls(repo, now); got.First != 50 || got.Walk.Pages >= 0 || !got.Walk.Since.Equal(r.BackfillSince) {
		t.Errorf("a backfill = %+v", got)
	}
}

// TestTheDailyPullsPassIsMarkedWhenTheFamilyRan sweeps the fake once and
// checks two things the sweep leaves behind: the totals it read now size the
// next page, and the whole-page read is on record so the next sweep reads
// what changed instead.
func TestTheDailyPullsPassIsMarkedWhenTheFamilyRan(t *testing.T) {
	t.Parallel()
	r, _, log := fakeRunner(t)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	repo := collect.Repo{Owner: "octocat", Name: "hello-world", FullName: "octocat/hello-world"}
	// The fake's totals say 221 pull requests and 43 issues.
	if counts := r.counts[repo.FullName]; counts.Pulls != 221 || counts.Issues != 43 {
		t.Errorf("counts after the sweep = %+v, want what the totals family read", counts)
	}
	if _, marked := r.State.LastFull["issues"]; !marked {
		t.Fatal("the sweep read a whole page of pull requests and did not record it")
	}
	saved := LoadState(r.State.path)
	if _, kept := saved.LastFull["issues"]; !kept {
		t.Error("the whole-page read did not reach the state file")
	}
	if got := r.pulls(repo, time.Now()); got.Walk.Since.IsZero() || got.First != 10 {
		t.Errorf("the next sweep = %+v, want what changed since this one", got)
	}
}

// TestDiscussionsAreOnlyAskedWhereTheyAreOn is the skip that removes sixteen
// of eighteen discussions queries on the account this was measured against.
// The fake answers the query for any repository, so a point for a repository
// whose forum is off is a query that should not have been sent.
func TestDiscussionsAreOnlyAskedWhereTheyAreOn(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		got := &captured{name: "captured"}
		r, _, log := fakeRunner(t, got)
		r.Cfg.Every = everyOnly("discussions")
		if err := r.Cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		r.repos = []collect.Repo{{Owner: "octocat", Name: "hello-world", FullName: "octocat/hello-world", HasDiscussions: on}}
		r.reposAt = time.Now()
		if err := r.Once(t.Context()); err != nil {
			t.Fatalf("Once: %v\n%s", err, log)
		}
		if asked := got.measured("gh_discussion") > 0; asked != on {
			t.Errorf("has_discussions %v: discussions collected %v", on, asked)
		}
		if _, marked := r.State.LastRun["discussions"]; !marked {
			t.Errorf("has_discussions %v: a repository skipped is not a repository that failed, the family must be marked", on)
		}
	}
}

// TestDiscussionsAreSizedForTheSweep checks the page the runner hands the
// collector: ten threads on a sweep, which costs two points where the
// backfill's fifty costs eleven, and twenty comments and twenty replies on
// both, because those pages read oldest first and a shorter one would not
// record the eleventh comment of a thread at all.
func TestDiscussionsAreSizedForTheSweep(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	if got := r.discussions(); got.First != 10 || got.Comments != 20 || got.Replies != 20 || got.Walk.Pages != 0 {
		t.Errorf("a sweep = %+v, want ten threads with twenty comments and replies on one page", got)
	}
	r.Backfill, r.BackfillSince = true, time.Now()
	if got := r.discussions(); got.First != 50 || got.Comments != 20 || got.Replies != 20 || got.Walk.Pages >= 0 || !got.Walk.Since.Equal(r.BackfillSince) {
		t.Errorf("a backfill = %+v, want fifty, twenty, twenty and the walk", got)
	}
}

// TestTheDailyPullsPassIsOncePerUTCDay pins what "once a day" means. An open
// pull request nobody touches is stamped at the start of the UTC day and the
// daily pass is the only read that rewrites it, so every UTC day on which the
// family ran must hold one such pass. Twenty-four hours after the last one is
// not that: the hourly family slips a tick now and then, the pass drifts later
// with it, and the day it drifts across midnight is a day with no row for
// any untouched open pull request.
func TestTheDailyPullsPassIsOncePerUTCDay(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	lateEvening := time.Date(2026, 9, 11, 23, 50, 0, 0, time.UTC)
	r.State.MarkFull("issues", lateEvening)
	if !r.fullPassDue("issues", lateEvening.Add(20*time.Minute)) {
		t.Error("a new UTC day twenty minutes after the last pass is due one, whatever the clock says")
	}
	if r.fullPassDue("issues", lateEvening.Add(-23*time.Hour)) {
		t.Error("the same UTC day is not due a second pass")
	}
	// Midnight is decided in UTC, which is the day the open rows are stamped
	// at, not in whatever zone the host runs in.
	r.State.MarkFull("issues", lateEvening.In(time.FixedZone("east", 3*3600)))
	if !r.fullPassDue("issues", lateEvening.Add(20*time.Minute).In(time.FixedZone("west", -5*3600))) {
		t.Error("the day boundary moved with the host's zone")
	}
}

// TestTheDailyPullsPassIsNotOnRecordWhenARepositoryFailed is the other way
// an untouched open pull request could go a day without its row: the daily
// pass fails on one repository and is recorded as done for all of them, so
// nothing rewrites that repository's open rows until the next day. Left
// unrecorded, the next sweep reads the whole page again, which costs one more
// daily pass and loses nothing.
func TestTheDailyPullsPassIsNotOnRecordWhenARepositoryFailed(t *testing.T) {
	t.Parallel()
	broken := true
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if broken && body.Variables["name"] == "b" {
			_, _ = w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"gone"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"pageInfo":{},"nodes":[]},"issues":{"pageInfo":{},"nodes":[]}}}}`))
	})
	r.Cfg.Every = everyOnly("issues")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.repos = []collect.Repo{
		{Owner: "o", Name: "a", FullName: "o/a"},
		{Owner: "o", Name: "b", FullName: "o/b"},
	}
	now := time.Now()
	if err := r.repoFamilies(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if _, ran := r.State.LastRun["issues"]; !ran {
		t.Fatal("one repository failing is not the family failing: it ran")
	}
	if _, full := r.State.LastFull["issues"]; full {
		t.Error("the daily pass failed on a repository and was put on record as done")
	}
	broken = false
	if err := r.repoFamilies(t.Context(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, full := r.State.LastFull["issues"]; !full {
		t.Error("the daily pass reached every repository and was not put on record")
	}
}

// TestTheDailyPullsPassOutgrowsAStaleTotal is the guard on the sizing. The
// totals family runs twice a day, so a repository that crossed a page size
// since it last ran holds more than the page asked for; a page that came
// back full with more behind it is that case, and the daily pass reads one
// page more rather than trust the count. Fifty is where the sizing stops and
// where today's read stopped, so a page of fifty is never followed.
func TestTheDailyPullsPassOutgrowsAStaleTotal(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Now()
	repo := collect.Repo{Owner: "o", Name: "n", FullName: "o/n"}
	r.noteCounts(nil)
	for total, pages := range map[int]int{4: 2, 10: 2, 20: 2, 21: 0, 400: 0} {
		r.counts["o/n"] = collect.ItemCounts{Pulls: total}
		if got := r.pulls(repo, now); got.Walk.Pages != pages || !got.Walk.Since.IsZero() {
			t.Errorf("the daily pass of a repository with %d pull requests = %+v, want Pages %d and no bound", total, got, pages)
		}
	}
}
