package run

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
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
// repository holds, rounded up to the next size the gateway prices, and never
// more than ten on an hourly read or twenty-five on either of the day's reads,
// the one of every open item sized from the open counts; each is its cap until
// the totals family has said anything. A repository with no day's read on
// record asks for a first page of fifty, the newest fifty 2.6.4 wrote on a new
// state file.
func TestPullsPageFollowsTheTotals(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Now()
	repo := collect.Repo{Owner: "o", Name: "n", FullName: "o/n"}
	if got := r.pulls(repo, now); got.First != 50 {
		t.Errorf("the day's read on a new state file = %+v, want a first page of fifty", got)
	}
	if got := r.openPulls(repo); got.First != 25 || !got.Open {
		t.Errorf("every open item before totals ran = %+v, want twenty-five a page, open only", got)
	}
	r.State.MarkFull("issues", now.Add(-24*time.Hour))
	if got := r.pulls(repo, now); got.First != 25 {
		t.Errorf("the day's read after 2.6.4's record = %+v, want twenty-five", got)
	}
	r.State.MarkFull(dayReadKey("issues", repo), now)
	if got := r.pulls(repo, now); got.First != 10 {
		t.Errorf("an hourly read before totals ran = %+v, want ten", got)
	}
	r.noteCounts(nil)
	r.counts["o/n"] = collect.ItemCounts{Pulls: 3, OpenPulls: 2}
	if got := r.pulls(repo, now); got.First != 5 {
		t.Errorf("three pull requests = %+v, want a page of five", got)
	}
	if got := r.openPulls(repo); got.First != 5 {
		t.Errorf("two open pull requests = %+v, want a page of five", got)
	}
	r.counts["o/n"] = collect.ItemCounts{Pulls: 12, Issues: 244, OpenIssues: 26}
	if got := r.openPulls(repo); got.First != 25 {
		t.Errorf("twenty-six open issues = %+v, want pages of twenty-five", got)
	}
	// A count that went stale cannot lose a row: every read walks every page
	// back to its bound, whatever the count said.
	for total := range 60 {
		r.counts["o/n"] = collect.ItemCounts{Pulls: total, OpenPulls: total}
		if got := r.pulls(repo, now); got.First < min(total, 10) || got.Walk.Pages >= 0 {
			t.Errorf("%d pull requests = %+v", total, got)
		}
		if got := r.openPulls(repo); got.First < min(total, 25) || got.Walk.Pages >= 0 || !got.Walk.Since.IsZero() {
			t.Errorf("%d open pull requests = %+v", total, got)
		}
	}
}

// TestPullsReadWhatMovedAndReachBackADayOnceADay is the shape decided on
// 2026-09-30: every sweep walks by updatedAt back to twice the cadence, and
// the first sweep of each UTC day reaches back to a cadence before the
// repository's day's read before it, and a month when there was none, beside
// its read of every open item.
func TestPullsReadWhatMovedAndReachBackADayOnceADay(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Now()
	repo := collect.Repo{Owner: "o", Name: "n", FullName: "o/n"}
	every, _ := r.Cfg.Interval("issues")

	// Never read: this sweep is the day's first, on a state file with no
	// day's read on record.
	if !r.dayReadDue("issues", repo, now) {
		t.Fatal("a repository that never took the day's read is due one")
	}
	if got := r.pulls(repo, now); !got.Walk.Since.Equal(now.AddDate(0, -1, 0)) || got.Walk.Pages >= 0 {
		t.Errorf("the day's read on a new state file = %+v, want every page back a month", got)
	}
	// A state file 2.6.4 wrote holds the family's record: the day's read
	// reaches back to it, and no further than a month.
	r.State.MarkFull("issues", now.Add(-26*time.Hour))
	if got := r.pulls(repo, now); !got.Walk.Since.Equal(now.Add(-26*time.Hour - every)) {
		t.Errorf("the day's read after 2.6.4's record = %+v, want a cadence before it", got)
	}
	r.State.MarkFull("issues", now.AddDate(0, -3, 0))
	if got := r.pulls(repo, now); !got.Walk.Since.Equal(now.AddDate(0, -1, 0)) {
		t.Errorf("the day's read of a repository added since 2.6.4's old record = %+v, want a month", got)
	}
	r.State.MarkFull(dayReadKey("issues", repo), now)

	// The sweep after it reads what changed, twice the cadence back.
	sweep := r.pulls(repo, now)
	if want := now.Add(-2 * every); !sweep.Walk.Since.Equal(want) {
		t.Errorf("Since = %s, want two cadences back, %s", sweep.Walk.Since, want)
	}
	if sweep.First != 10 || sweep.Walk.Pages >= 0 {
		t.Errorf("a sweep = %+v, want ten at a time and as many pages as the bound needs", sweep)
	}

	// A sweep that comes late reaches back to the one before it, so nothing
	// that was touched while the service was down waits for the day's read.
	r.State.Mark("issues", now.Add(-5*every))
	if got := r.pulls(repo, now); !got.Walk.Since.Equal(now.Add(-6 * every)) {
		t.Errorf("Since after a five-cadence gap = %s, want one cadence before the last run", got.Walk.Since)
	}

	// The next UTC day's first sweep reaches back to a cadence before this
	// day's read, which covers any hourly read a repository failed since.
	r.State.Mark("issues", now)
	later := now.Add(24 * time.Hour)
	if got := r.pulls(repo, later); !got.Walk.Since.Equal(now.Add(-every)) || got.First != 25 {
		t.Errorf("a day later = %+v, want twenty-five a page back to %s", got, now.Add(-every))
	}

	// A backfill is neither: every page back to its own bound.
	r.Backfill, r.BackfillSince = true, now.AddDate(-1, 0, 0)
	if got := r.pulls(repo, now); got.First != 50 || got.Walk.Pages >= 0 || !got.Walk.Since.Equal(r.BackfillSince) {
		t.Errorf("a backfill = %+v", got)
	}
}

// TestTheDailyPullsPassIsMarkedWhenTheFamilyRan sweeps the fake once and
// checks two things the sweep leaves behind: the totals it read now size the
// next page, and the day's read is on record so the next sweep reads
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
	if _, marked := r.State.LastFull[dayReadKey("issues", repo)]; !marked {
		t.Fatal("the sweep took the day's read of pull requests and did not record it")
	}
	saved := LoadState(r.State.path)
	if _, kept := saved.LastFull[dayReadKey("issues", repo)]; !kept {
		t.Error("the day's read did not reach the state file")
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
	repo := collect.Repo{Owner: "o", Name: "n", FullName: "o/n"}
	key := dayReadKey("issues", repo)
	lateEvening := time.Date(2026, 9, 11, 23, 50, 0, 0, time.UTC)
	r.State.MarkFull(key, lateEvening)
	if !r.dayReadDue("issues", repo, lateEvening.Add(20*time.Minute)) {
		t.Error("a new UTC day twenty minutes after the last pass is due one, whatever the clock says")
	}
	if r.dayReadDue("issues", repo, lateEvening.Add(-23*time.Hour)) {
		t.Error("the same UTC day is not due a second pass")
	}
	// Midnight is decided in UTC, which is the day the open rows are stamped
	// at, not in whatever zone the host runs in.
	r.State.MarkFull(key, lateEvening.In(time.FixedZone("east", 3*3600)))
	if !r.dayReadDue("issues", repo, lateEvening.Add(20*time.Minute).In(time.FixedZone("west", -5*3600))) {
		t.Error("the day boundary moved with the host's zone")
	}
}

// TestTheDaysReadIsRecordedPerRepository is the other way an untouched open
// pull request could go a day without its row, and the cost of guarding it:
// the day's read fails on one repository, and that repository takes it again
// on the next sweep while the one it reached does not. 2.6.4 recorded the read
// for the family once every repository had answered, so one that kept failing
// made every sweep take the day's read of every repository again.
func TestTheDaysReadIsRecordedPerRepository(t *testing.T) {
	t.Parallel()
	broken := true
	var mu sync.Mutex
	openAsked := map[string]int{}
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		name, _ := body.Variables["name"].(string)
		if strings.Contains(body.Query, "states: OPEN") {
			mu.Lock()
			openAsked[name]++
			mu.Unlock()
		}
		if broken && name == "b" {
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"pageInfo":{},"nodes":[]},"issues":{"pageInfo":{},"nodes":[]}}}}`))
	})
	r.Cfg.Every = everyOnly("issues")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	a := collect.Repo{Owner: "o", Name: "a", FullName: "o/a"}
	b := collect.Repo{Owner: "o", Name: "b", FullName: "o/b"}
	r.repos = []collect.Repo{a, b}
	now := time.Now()
	if err := r.repoFamilies(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if _, ran := r.State.LastRun["issues"]; !ran {
		t.Fatal("one repository failing is not the family failing: it ran")
	}
	if _, done := r.State.LastFull[dayReadKey("issues", a)]; !done {
		t.Error("the day's read reached o/a and was not put on record for it")
	}
	if _, done := r.State.LastFull[dayReadKey("issues", b)]; done {
		t.Error("the day's read failed on o/b and was put on record for it")
	}
	broken = false
	clear(openAsked)
	r.State.Mark("issues", now.Add(-2*time.Hour))
	if err := r.repoFamilies(t.Context(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if openAsked["a"] != 0 || openAsked["b"] != 1 {
		t.Errorf("every open item asked again of %v, want o/b alone", openAsked)
	}
	if _, done := r.State.LastFull[dayReadKey("issues", b)]; !done {
		t.Error("the day's read reached o/b and was not put on record for it")
	}
}

// TestABackfillTakesNoDaysRead: a backfill reads no open item it has no
// reason to, and records nothing, so the sweeps later that day still take the
// day's read. 2.6.4 recorded it for the family after a backfill, and an open
// item last touched before the backfill's bound went without its row that day.
func TestABackfillTakesNoDaysRead(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"pageInfo":{},"nodes":[]},"issues":{"pageInfo":{},"nodes":[]}}}}`))
	})
	r.Cfg.Every = everyOnly("issues")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Backfill, r.BackfillSince = true, time.Now().AddDate(0, -3, 0)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(r.State.LastFull) != 0 {
		t.Errorf("a backfill recorded a day's read: %v", r.State.LastFull)
	}
	r.Backfill = false
	if !r.dayReadDue("issues", r.repos[0], time.Now()) {
		t.Error("the sweep after a backfill is not due the day's read")
	}
}

// issueItem is one issue as the fake behind issuesServer lists it.
type issueItem struct {
	number  int
	state   string
	updated time.Time
}

// issuesServer answers the pull request query with no pull requests and with
// items as the issues, newest first, paged by first and issueAfter the way
// GitHub pages them; a query filtered to the open items is answered with the
// open ones only. Anything else, the movement query among them, is answered
// with nothing, so nothing is skipped.
func issuesServer(t *testing.T, items []issueItem, now time.Time) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(body.Query, "reviewThreads(") {
			_, _ = w.Write([]byte(`{"errors":[{"type":"UNKNOWN","message":"not here"}]}`))
			return
		}
		list := items
		if strings.Contains(body.Query, "states: OPEN") {
			list = nil
			for _, it := range items {
				if it.state == "OPEN" {
					list = append(list, it)
				}
			}
		}
		first := int(body.Variables["first"].(float64))
		after := 0
		if cursor, ok := body.Variables["issueAfter"].(string); ok {
			after, _ = strconv.Atoi(cursor)
		}
		end := min(after+first, len(list))
		nodes := make([]map[string]any, 0, end-after)
		for _, it := range list[after:end] {
			node := map[string]any{
				"number": it.number, "state": it.state,
				"createdAt": now.AddDate(0, -3, 0), "updatedAt": it.updated,
			}
			if it.state == "CLOSED" {
				node["closedAt"] = it.updated
			}
			nodes = append(nodes, node)
		}
		page := map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequests": map[string]any{"pageInfo": map[string]any{}, "nodes": []any{}},
			"issues": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": end < len(list), "endCursor": strconv.Itoa(end)},
				"nodes":    nodes,
			},
		}}}
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}
}

// TestTheDaysReadWritesAnOpenItemFiftyOthersMovedPast is the loss measured on
// 2026-09-30: open issue 961 of jmrplens/gitlab-mcp-server, 59th by
// updatedAt, had no row for the 29th, because the day's read was the newest
// fifty of every state. Here the open issue last touched two months ago is
// 160th, behind fifty-eight closed in the last few minutes and a hundred
// closed forty days ago, and the day's read writes its row of the day. The open issue that moved a minute ago is in
// both of the day's reads and is written once.
func TestTheDaysReadWritesAnOpenItemFiftyOthersMovedPast(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	items := []issueItem{{100, "OPEN", now.Add(-time.Minute)}}
	for i := range 58 {
		items = append(items, issueItem{i + 2, "CLOSED", now.Add(-2*time.Minute - time.Duration(i)*time.Second)})
	}
	// Past the day's read of what moved, which stops at the first page
	// whose last item is older than its month.
	for i := range 100 {
		items = append(items, issueItem{i + 200, "CLOSED", now.AddDate(0, 0, -40).Add(-time.Duration(i) * time.Second)})
	}
	items = append(items, issueItem{1, "OPEN", now.AddDate(0, -2, 0)})

	r := sweepRunner(t, issuesServer(t, items, now))
	r.Cfg.Every = everyOnly("issues")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	points, err := r.sinceWindow(t.Context(), "issues", r.repos[0], now)
	if err != nil {
		t.Fatal(err)
	}
	today := now.Truncate(24 * time.Hour)
	rows := map[string]int{}
	for _, p := range points {
		if p.Measurement == "gh_issue" && p.Tags["state"] == "OPEN" && p.Time.Equal(today) {
			rows[p.Tags["number"]]++
		}
	}
	if rows["1"] != 1 {
		t.Errorf("the open issue 160 items back has %d rows for today, want 1", rows["1"])
	}
	if rows["100"] != 1 {
		t.Errorf("the open issue both reads found has %d rows for today, want 1", rows["100"])
	}
}
