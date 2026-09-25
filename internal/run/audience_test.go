package run

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// audienceRequests counts, over one slice of the fake's log, the REST reads of
// the star and fork lists and the GraphQL batches that replace them.
func audienceRequests(requests []fakegh.Request) (stargazers, forks, batches int) {
	for _, req := range requests {
		switch {
		case strings.HasSuffix(req.Path, "/stargazers"):
			stargazers++
		case strings.HasSuffix(req.Path, "/forks"):
			forks++
		case strings.Contains(req.GraphQL, "fragment audience on Repository"):
			batches++
		}
	}
	return stargazers, forks, batches
}

// TestStarsAndForksAreBatchedOnceWalked runs the two families three times in
// one process: the first sweep walks both lists through REST, page by page,
// because the repository has never been seen and the install is fresh; the
// next ordinary sweep asks GraphQL for the newest hundred of each in one
// batch and reads no REST list at all; a backfill walks REST again.
func TestStarsAndForksAreBatchedOnceWalked(t *testing.T) {
	t.Parallel()
	r, fake, log := fakeRunner(t)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	stargazers, forks, batches := audienceRequests(fake.Requests())
	if stargazers == 0 || forks == 0 {
		t.Fatalf("the first sweep read the stargazers %d times and the forks %d, want both walked", stargazers, forks)
	}
	if batches != 0 {
		t.Errorf("the first sweep sent %d batches for lists it had not walked yet", batches)
	}

	// Only stars and forks are due again: everything else was marked a
	// moment ago.
	before := len(fake.Requests())
	for _, family := range []string{"stars", "forks"} {
		every, _ := r.Cfg.Interval(family)
		r.State.LastRun[family] = time.Now().Add(-2 * every)
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v\n%s", err, log)
	}
	stargazers, forks, batches = audienceRequests(fake.Requests()[before:])
	if stargazers != 0 || forks != 0 {
		t.Errorf("an ordinary sweep read the stargazers %d times and the forks %d through REST, want the batch", stargazers, forks)
	}
	if batches != 2 {
		t.Errorf("an ordinary sweep sent %d batches, want one per family", batches)
	}
	if strings.Contains(log.String(), "batched collector failed") {
		t.Errorf("the batch failed:\n%s", log)
	}

	before = len(fake.Requests())
	for _, family := range []string{"stars", "forks"} {
		every, _ := r.Cfg.Interval(family)
		r.State.LastRun[family] = time.Now().Add(-2 * every)
	}
	r.Backfill, r.BackfillSince = true, time.Now().AddDate(-1, 0, 0)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("backfill Once: %v\n%s", err, log)
	}
	stargazers, forks, batches = audienceRequests(fake.Requests()[before:])
	if stargazers == 0 || forks == 0 || batches != 0 {
		t.Errorf("a backfill read the stargazers %d times, the forks %d and sent %d batches, want REST only", stargazers, forks, batches)
	}
}

// audienceRunner is a runner over handler with only the forks family enabled,
// past its first sweep and with its one repository already seen, which is the
// state in which the batch serves the family.
func audienceRunner(t *testing.T, handler http.HandlerFunc) *Runner {
	t.Helper()
	r := sweepRunner(t, handler)
	r.Cfg.Every = everyOnly("forks")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.State.LastRun["forks"] = time.Now().Add(-time.Hour)
	r.State.FirstSaw["o/n"] = time.Now().Add(-time.Hour)
	return r
}

// TestAForksBatchThatFailsLeavesTheFamilyUnmarked: once a repository has been
// walked, the batch is the whole of the forks family for it, and a sweep
// whose batch failed collected nothing. Marking the family would hide that
// until its next cadence, half a day, the way a family that failed on every
// repository would be hidden.
func TestAForksBatchThatFailsLeavesTheFamilyUnmarked(t *testing.T) {
	t.Parallel()
	r := audienceRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	before := r.State.LastRun["forks"]
	if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if when := r.State.LastRun["forks"]; !when.Equal(before) {
		t.Errorf("a sweep whose batch failed marked forks as run at %s", when)
	}
}

// answerForksBatch answers the audience batch with one fork and the total
// the caller names, and every REST request with an empty page, counting the
// reads of the fork list.
func answerForksBatch(total int, walked *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/graphql" {
			_, _ = fmt.Fprintf(w, `{"data":{"r0":{"forks":{"totalCount":%d,"nodes":[{"nameWithOwner":"a/n",`+
				`"createdAt":"2025-02-10T10:00:00Z","pushedAt":"2025-02-20T10:00:00Z","stargazerCount":2,`+
				`"url":"https://github.com/a/n","owner":{"login":"a"}}]}}}}`, total)
			return
		}
		if strings.HasSuffix(req.URL.Path, "/forks") {
			walked.Add(1)
		}
		_, _ = w.Write([]byte("[]"))
	}
}

// TestARepositoryWithMoreForksThanTheBatchReadsIsStillWalked: the batch reads
// the newest hundred, and a fork row past them carries a star count and a
// days_since_push the REST walk used to refresh on up to five hundred forks a
// sweep. A repository the batch reports as holding more than its page is
// walked the old way in the same sweep; one that fits is not.
func TestARepositoryWithMoreForksThanTheBatchReadsIsStillWalked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		total  int
		walked int32
	}{{total: 150, walked: 1}, {total: 2, walked: 0}} {
		var walked atomic.Int32
		r := audienceRunner(t, answerForksBatch(tc.total, &walked))
		if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := walked.Load(); got != tc.walked {
			t.Errorf("with %d forks the REST list was read %d times, want %d", tc.total, got, tc.walked)
		}
		if _, marked := r.State.LastRun["forks"]; !marked {
			t.Errorf("with %d forks the family was not marked as run", tc.total)
		}
	}
}

// TestABatchThatFailsLeavesUnmarkedOnlyTheFamiliesItWas: a failed batch is
// the whole of branches, which collects nowhere else, and the whole of stars
// for a repository already walked, so both are left unmarked the way a family
// that failed on every repository is. It is none of repo, whose per-repository
// read asks the same thing again, and that family is still marked.
func TestABatchThatFailsLeavesUnmarkedOnlyTheFamiliesItWas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		family string
		marked bool
	}{{"stars", false}, {"branches", false}, {"repo", true}} {
		r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/graphql" {
				// An error GraphQL answers with a 200, which no batch skips:
				// a 5xx reads as a query too large, and some batches shrink
				// and carry on from that.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
				return
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		})
		r.Cfg.Every = everyOnly(tc.family)
		if err := r.Cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		log, buf := debugLog()
		r.Log = log
		r.State.LastRun[tc.family] = time.Now().Add(-time.Hour)
		r.State.FirstSaw["o/n"] = time.Now().Add(-time.Hour)
		before := r.State.LastRun[tc.family]
		if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), `msg="batched collector failed" family=`+tc.family) {
			t.Fatalf("%s: the batch did not fail, so this case proves nothing:\n%s", tc.family, buf)
		}
		if marked := !r.State.LastRun[tc.family].Equal(before); marked != tc.marked {
			t.Errorf("%s: a sweep whose batch failed was marked %v, want %v", tc.family, marked, tc.marked)
		}
	}
}

// historyAnchor is the Sunday the newest week of starGitHub's history is
// labeled with, fixed and in the past, so every day of it is one a sweep may
// write whenever the test runs.
var historyAnchor = time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)

// starGitHub answers the stars family for every repository of owner o: a
// daily star history of full pages thirty weeks each and then a short last
// page of three, one star on the Monday of every week; a stargazer list of
// one page holding listBody, empty when that is, or the status listStatus;
// and an audience batch whose connections are empty, or an error when
// batchFails. failPage is a page of the history that answers failStatus
// instead, 502 when that is zero, or a spent budget when spent is set; zero
// for none. Every REST request is counted by path.
type starGitHub struct {
	full       int
	failPage   int
	failStatus int
	spent      bool
	listStatus int
	listBody   string
	batchFails bool

	mu    sync.Mutex
	asked map[string]int
	pages []string
}

func (g *starGitHub) handler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	g.mu.Lock()
	if g.asked == nil {
		g.asked = map[string]int{}
	}
	g.asked[req.URL.Path]++
	if strings.HasSuffix(req.URL.Path, "/stargazers/history") {
		g.pages = append(g.pages, req.URL.Path+"?"+req.URL.RawQuery)
	}
	full, failPage, failStatus, spent := g.full, g.failPage, g.failStatus, g.spent
	listStatus, listBody, batchFails := g.listStatus, g.listBody, g.batchFails
	g.mu.Unlock()
	switch {
	case req.URL.Path == "/graphql":
		if batchFails {
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"r0":{"stargazers":{"edges":[]}},"r1":{"stargazers":{"edges":[]}}}}`))
	case strings.HasSuffix(req.URL.Path, "/stargazers"):
		if listStatus != 0 {
			http.Error(w, `{"message":"`+http.StatusText(listStatus)+`"}`, listStatus)
			return
		}
		if listBody == "" {
			listBody = "[]"
		}
		_, _ = w.Write([]byte(listBody))
	case strings.HasSuffix(req.URL.Path, "/stargazers/history"):
		page := 1
		if p := req.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		if page == failPage {
			failHistory(w, failStatus, spent)
			return
		}
		_, _ = w.Write(historyPage(page, full))
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// failHistory answers a page of the history that fails: with status, 502 when
// it is zero, and as a spent budget when spent is set, which GitHub tells
// from a feature switched off only by its headers.
func failHistory(w http.ResponseWriter, status int, spent bool) {
	if spent {
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	http.Error(w, `{"message":"`+http.StatusText(status)+`"}`, status)
}

// historyPage is page n of a history of full thirty-week pages and a short
// last one, newest first as GitHub serves it, and an empty array past it.
func historyPage(n, full int) []byte {
	weeks := 30
	switch {
	case n == full+1:
		weeks = 3
	case n > full+1:
		return []byte("[]")
	}
	rows := make([]string, 0, weeks)
	for i := range weeks {
		sunday := historyAnchor.AddDate(0, 0, -7*((n-1)*30+i))
		rows = append(rows, fmt.Sprintf(`{"week":%d,"total":1,"days":[0,1,0,0,0,0,0]}`, sunday.Unix()))
	}
	return []byte("[" + strings.Join(rows, ",") + "]")
}

// historyRequests is how many pages of the history each repository was asked
// for, and how many requests read a stargazer list.
func (g *starGitHub) historyRequests() (history map[string]int, lists int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	history = map[string]int{}
	for path, n := range g.asked {
		switch {
		case strings.HasSuffix(path, "/stargazers/history"):
			history[strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/stargazers/history")] += n
		case strings.HasSuffix(path, "/stargazers"):
			lists += n
		}
	}
	return history, lists
}

// forget clears the count, so the next pass is read on its own.
func (g *starGitHub) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked, g.pages = nil, nil
}

// starsRunner is a runner over g with only the stars family enabled, two
// repositories already discovered and nothing yet in its state, keeping every
// point it sends.
func starsRunner(t *testing.T, g *starGitHub) (*Runner, *kept) {
	t.Helper()
	r := sweepRunner(t, g.handler)
	r.Cfg.Every = everyOnly("stars")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.repos = []collect.Repo{
		{Owner: "o", Name: "a", FullName: "o/a"},
		{Owner: "o", Name: "b", FullName: "o/b"},
	}
	store := &kept{}
	r.Sinks = []sink.Sink{store}
	return r, store
}

// due makes the stars family due again, as the next sweep would find it.
func due(r *Runner) { r.State.LastRun["stars"] = time.Now().Add(-time.Hour) }

// TestTheStarHistoryIsReadWholeOnceAndThenByItsNewestPage: a repository seen
// for the first time has its list walked and its history read back to its
// first week, and is then remembered, so the next sweep asks for page one of
// its history alone and reads no list at all.
func TestTheStarHistoryIsReadWholeOnceAndThenByItsNewestPage(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 2}
	r, store := starsRunner(t, g)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	history, lists := g.historyRequests()
	for _, repo := range []string{"o/a", "o/b"} {
		if history[repo] != 3 {
			t.Errorf("the first sweep read %d pages of %s's history, want the two full ones and the short last one", history[repo], repo)
		}
		if r.State.HistoryDue(repo) {
			t.Errorf("a whole walk of %s's history was not recorded", repo)
		}
	}
	if lists != 2 {
		t.Errorf("the first sweep read %d stargazer lists, want both walked once", lists)
	}
	if n := store.measured("gh_star_day"); n == 0 {
		t.Fatal("no gh_star_day row reached the sink")
	}

	g.forget()
	due(r)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	history, lists = g.historyRequests()
	for _, repo := range []string{"o/a", "o/b"} {
		if history[repo] != 1 {
			t.Errorf("the next sweep read %d pages of %s's history, want page one alone", history[repo], repo)
		}
	}
	if lists != 0 {
		t.Errorf("the next sweep read %d stargazer lists, which the batch serves now", lists)
	}
	for _, page := range g.pages {
		if strings.Contains(page, "page=") {
			t.Errorf("a sweep asked for %s, beyond the page it re-reads", page)
		}
	}
}

// TestAnUpgradeReadsEveryHistoryWholeAndNoListAgain: a state file written
// before the history was read has every repository in first_saw and nothing
// in history_read. The first sweep after the upgrade reads each history back
// to its first week, once, and leaves the lists to the batch as before.
func TestAnUpgradeReadsEveryHistoryWholeAndNoListAgain(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 1}
	r, _ := starsRunner(t, g)
	for _, repo := range r.repos {
		r.State.FirstSaw[repo.FullName] = time.Now().AddDate(0, -3, 0)
	}
	due(r)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	history, lists := g.historyRequests()
	if history["o/a"] != 2 || history["o/b"] != 2 {
		t.Errorf("the first sweep after the upgrade read %v pages, want both histories whole", history)
	}
	if lists != 0 {
		t.Errorf("the upgrade walked %d stargazer lists again", lists)
	}

	g.forget()
	due(r)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if history, _ = g.historyRequests(); history["o/a"] != 1 || history["o/b"] != 1 {
		t.Errorf("the sweep after the upgrade read %v pages, want one each", history)
	}
}

// TestAStarHistoryCutShortIsReadWholeAgain: a 502 part way through the whole
// walk keeps the rows of the pages already read, counts the repository as
// failed, and leaves it unrecorded, so the next sweep walks it whole again
// rather than leaving the older weeks unread until somebody runs a backfill.
func TestAStarHistoryCutShortIsReadWholeAgain(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 2, failPage: 2}
	r, store := starsRunner(t, g)
	r.repos = r.repos[:1]
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Page one's thirty weeks, every day of them.
	if n := store.measured("gh_star_day"); n != 30*7 {
		t.Errorf("%d gh_star_day rows reached the sink, want page one's %d", n, 30*7)
	}
	if !r.State.HistoryDue("o/a") {
		t.Error("a history cut short was recorded as read whole")
	}
	if row := r.health.runs["stars"]; row == nil || row.Failed != 1 {
		t.Errorf("the family row = %+v, want the repository counted as failed", row)
	}
	if _, marked := r.State.LastRun["stars"]; marked {
		t.Error("a family whose one repository failed was marked as run")
	}

	g.mu.Lock()
	g.failPage = 0
	g.mu.Unlock()
	g.forget()
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if history, _ := g.historyRequests(); history["o/a"] != 3 {
		t.Errorf("the next sweep read %d pages, want the whole history again", history["o/a"])
	}
	if r.State.HistoryDue("o/a") {
		t.Error("the walk that reached the end was not recorded")
	}
}

// TestAStarHistoryThatSaysNothingIsHerePartWayIsReadWholeAgain: a 403 or a
// 404 past page one is not where a history ends, which is a short page, an
// empty one or a 422. It is a secondary limit GitHub sent without the headers
// that would name it, or a repository that went away mid-walk. The pages read
// are kept and nothing has failed, as with any answer that there is nothing
// here, but the walk is not recorded as whole, so the next sweep reads the
// history back to its first week again rather than leaving the older weeks to
// a backfill.
func TestAStarHistoryThatSaysNothingIsHerePartWayIsReadWholeAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		g := &starGitHub{full: 3, failPage: 2, failStatus: status}
		r, store := starsRunner(t, g)
		r.repos = r.repos[:1]
		if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if n := store.measured("gh_star_day"); n != 30*7 {
			t.Errorf("%d on page two: %d gh_star_day rows reached the sink, want page one's %d", status, n, 30*7)
		}
		if row := r.health.runs["stars"]; row == nil || row.Failed != 0 {
			t.Errorf("%d on page two: the family row = %+v, want nothing failed", status, row)
		}
		if !r.State.HistoryDue("o/a") {
			t.Errorf("%d on page two: a history read to page one of four was recorded as read whole", status)
		}

		g.mu.Lock()
		g.failPage = 0
		g.mu.Unlock()
		g.forget()
		due(r)
		if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if history, _ := g.historyRequests(); history["o/a"] != 4 {
			t.Errorf("%d on page two: the next sweep read %d pages, want the whole history again", status, history["o/a"])
		}
		if r.State.HistoryDue("o/a") {
			t.Errorf("%d on page two: the walk that reached the end was not recorded", status)
		}
	}
}

// TestAStarHistoryNotServedIsAskedWholeUntilItIs: GitHub Enterprise Server
// does not serve the history, and every repository answers page one with a
// 404. That is nothing here, not a failure, and not a history read either:
// recorded, a server upgraded to one that serves it would have only its
// newest thirty weeks read until somebody ran a backfill. Asking whole again
// costs the one request a sweep makes anyway, and the first sweep that finds
// the history reads it back to its first week.
func TestAStarHistoryNotServedIsAskedWholeUntilItIs(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 2, failPage: 1, failStatus: http.StatusNotFound}
	r, store := starsRunner(t, g)
	r.repos = r.repos[:1]
	for sweep := range 2 {
		g.forget()
		due(r)
		if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if history, _ := g.historyRequests(); history["o/a"] != 1 {
			t.Errorf("sweep %d asked %d pages of a history that is not served, want page one", sweep+1, history["o/a"])
		}
		if !r.State.HistoryDue("o/a") {
			t.Fatalf("sweep %d recorded a history it never read as read whole", sweep+1)
		}
		if row := r.health.runs["stars"]; row == nil || row.Failed != 0 {
			t.Errorf("sweep %d: the family row = %+v, want nothing failed", sweep+1, row)
		}
	}
	if n := store.measured("gh_star_day"); n != 0 {
		t.Errorf("%d gh_star_day rows from a history that is not served", n)
	}

	g.mu.Lock()
	g.failPage = 0
	g.mu.Unlock()
	g.forget()
	due(r)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if history, _ := g.historyRequests(); history["o/a"] != 3 {
		t.Errorf("the first sweep to find the history read %d pages, want all of it", history["o/a"])
	}
	if r.State.HistoryDue("o/a") {
		t.Error("the whole walk was not recorded")
	}
}

// TestAStarHistoryBackfillWalksBackToItsDate: a backfill of a history already
// read whole walks back to BackfillSince and no further; one never read whole
// is read whole, and recorded.
func TestAStarHistoryBackfillWalksBackToItsDate(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 3}
	r, _ := starsRunner(t, g)
	r.State.HistoryRead["o/a"] = time.Now().AddDate(0, -1, 0)
	r.Backfill = true
	// Page one reaches back twenty-nine weeks and page two fifty-nine, so a
	// bound forty-five weeks back is passed on page two.
	r.BackfillSince = historyAnchor.AddDate(0, 0, -7*45)
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	history, lists := g.historyRequests()
	if history["o/a"] != 2 {
		t.Errorf("the backfill read %d pages of a history already read, want the two that reach its date", history["o/a"])
	}
	if history["o/b"] != 4 {
		t.Errorf("the backfill read %d pages of a history never read whole, want all four", history["o/b"])
	}
	if r.State.HistoryDue("o/b") {
		t.Error("the whole walk of o/b was not recorded")
	}
	if lists != 2 {
		t.Errorf("the backfill read %d stargazer lists, want both", lists)
	}
}

// TestAHiddenStargazerListStillHasItsDailyStars is the case the history is
// read everywhere for: GitHub answers the list with a 404 to a token that is
// not an admin or a collaborator of the repository, and the sweep still has
// the repository's stars from its daily history.
func TestAHiddenStargazerListStillHasItsDailyStars(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, fake, log := fakeRunner(t, store)
	r.Cfg.Every = everyOnly("stars")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	fake.Fail("/repos/octocat/hello-world/stargazers", http.StatusNotFound)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if n := store.measured("gh_star"); n != 0 {
		t.Errorf("%d gh_star rows from a list GitHub hid", n)
	}
	stars := 0
	for _, p := range store.rows("gh_star_day") {
		n, _ := p.Fields["stars"].(int)
		stars += n
	}
	if stars != 4 {
		t.Errorf("the daily history counted %d stars, want the fixture's four", stars)
	}
	if _, marked := r.State.LastRun["stars"]; !marked {
		t.Errorf("a list that is hidden failed the family:\n%s", log)
	}
}

// TestAFirstSightKeepsWhicheverHalfAnswered: a repository seen for the first
// time has its stargazer list walked and its history read in the same sweep,
// and either can fail while the other answers. The rows of the half that
// answered still reach the sink, and the half that failed still counts the
// repository as failed and leaves the family unmarked. A list that failed
// uncounted is the worse loss of the two: FirstSight has recorded the
// repository before the walk, so no later sweep walks that list whole again,
// and without the count nothing would say it was cut short.
func TestAFirstSightKeepsWhicheverHalfAnswered(t *testing.T) {
	t.Parallel()
	oneStar := `[{"starred_at":"2026-08-01T10:00:00Z","user":{"login":"u"}}]`
	for _, tc := range []struct {
		name        string
		g           *starGitHub
		stars, days int
	}{
		// Page one's thirty weeks, every day of them, and then a 502.
		{"the history cut short", &starGitHub{full: 2, failPage: 2, listBody: oneStar}, 1, 30 * 7},
		// A history of one short page, read whole.
		{"the list refused", &starGitHub{full: 0, listStatus: http.StatusBadGateway}, 0, 3 * 7},
	} {
		r, store := starsRunner(t, tc.g)
		r.repos = r.repos[:1]
		if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if n := store.measured("gh_star"); n != tc.stars {
			t.Errorf("%s: %d gh_star rows reached the sink, want %d", tc.name, n, tc.stars)
		}
		if n := store.measured("gh_star_day"); n != tc.days {
			t.Errorf("%s: %d gh_star_day rows reached the sink, want %d", tc.name, n, tc.days)
		}
		if row := r.health.runs["stars"]; row == nil || row.Failed != 1 {
			t.Errorf("%s: the family row = %+v, want the repository counted as failed", tc.name, row)
		}
		if _, marked := r.State.LastRun["stars"]; marked {
			t.Errorf("%s: a family whose one repository failed was marked as run", tc.name)
		}
	}
}

// TestAFailedBatchAndAFailedHistoryCountARepositoryOnce: when the batch fails
// it has already counted every repository it served, and the history of one
// of them failing as well must not count that repository again. Counted
// twice, a family where everything failed reads as one where not everything
// did, and is marked as run with nothing collected.
//
// A repository seen for the first time in the same sweep is the other side of
// that line: the batch never served it, so its failed history is the only
// count it gets. Whether a repository was served is asked before FirstSight
// records it; asked after, the new one reads as served, goes uncounted, and a
// family whose batch and every history failed is marked as run.
func TestAFailedBatchAndAFailedHistoryCountARepositoryOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		repos, seen int
	}{{"one repository", 1, 1}, {"two repositories", 2, 2}, {"one seen and one new", 2, 1}} {
		g := &starGitHub{full: 0, failPage: 1, batchFails: true}
		r, _ := starsRunner(t, g)
		r.repos = r.repos[:tc.repos]
		log, buf := debugLog()
		r.Log = log
		for _, repo := range r.repos[:tc.seen] {
			r.State.FirstSaw[repo.FullName] = time.Now().Add(-time.Hour)
			r.State.HistoryRead[repo.FullName] = time.Now().Add(-time.Hour)
		}
		due(r)
		before := r.State.LastRun["stars"]
		if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		row := r.health.runs["stars"]
		if row == nil {
			t.Fatalf("%s: the family wrote no row about itself", tc.name)
		}
		if row.Failed > row.Repos {
			t.Errorf("%s: %d failures counted over %d repositories", tc.name, row.Failed, row.Repos)
		}
		if row.Failed != tc.repos {
			t.Errorf("%s: %d failures counted, want every repository once", tc.name, row.Failed)
		}
		if when := r.State.LastRun["stars"]; !when.Equal(before) {
			t.Errorf("%s: a family that failed everywhere was marked as run at %s", tc.name, when)
		}
		// Counted or not, still said: the log and the family's row both name
		// every repository whose history failed.
		for _, repo := range r.repos {
			if !strings.Contains(buf.String(), `msg="collector failed" family=stars repo=`+repo.FullName) {
				t.Errorf("%s: the failure of %s's history was not reported:\n%s", tc.name, repo.FullName, buf)
			}
		}
		if len(row.Failures) != tc.repos {
			t.Errorf("%s: the family row names %d failed repositories, want %d", tc.name, len(row.Failures), tc.repos)
		}
	}
}

// TestAFailedBatchStillDeliversTheDailyStars: the batch is the names of the
// newest stars and the history is the counts, and one failing takes nothing
// from the other. The family stays due, as it did before the history, so the
// names are asked for again on the next tick.
func TestAFailedBatchStillDeliversTheDailyStars(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 0, batchFails: true}
	r, store := starsRunner(t, g)
	for _, repo := range r.repos {
		r.State.FirstSaw[repo.FullName] = time.Now().Add(-time.Hour)
		r.State.HistoryRead[repo.FullName] = time.Now().Add(-time.Hour)
	}
	due(r)
	before := r.State.LastRun["stars"]
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := store.measured("gh_star_day"); n != 2*3*7 {
		t.Errorf("%d gh_star_day rows reached the sink, want both repositories' three weeks", n)
	}
	if when := r.State.LastRun["stars"]; !when.Equal(before) {
		t.Errorf("a family whose batch brought back nothing was marked as run at %s", when)
	}
}

// TestASpentBudgetAfterAFailedBatchStillStopsTheFamily: audienceWalk keeps a
// failed history from counting a repository the failed batch has counted
// already, and a spent budget is the one error it must not keep back.
// collectFamily stops the family on it, leaves it unmarked and counts every
// repository, and it can only do that if it sees the error: swallowed, the
// sweep would go on asking the next repository's history on a budget that is
// gone and report the first as one more repository that failed.
func TestASpentBudgetAfterAFailedBatchStillStopsTheFamily(t *testing.T) {
	t.Parallel()
	g := &starGitHub{full: 0, failPage: 1, spent: true, batchFails: true}
	r, _ := starsRunner(t, g)
	log, buf := debugLog()
	r.Log = log
	for _, repo := range r.repos {
		r.State.FirstSaw[repo.FullName] = time.Now().Add(-time.Hour)
		r.State.HistoryRead[repo.FullName] = time.Now().Add(-time.Hour)
	}
	due(r)
	before := r.State.LastRun["stars"]
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `msg="batched collector failed" family=stars`) {
		t.Fatalf("the batch did not fail, so this case proves nothing:\n%s", buf)
	}
	if !strings.Contains(buf.String(), `msg="budget spent mid-family" family=stars at=o/a`) {
		t.Errorf("the spent budget did not reach collectFamily:\n%s", buf)
	}
	if strings.Contains(buf.String(), `msg="collector failed" family=stars repo=o/a`) {
		t.Errorf("the spent budget was reported as the repository failing:\n%s", buf)
	}
	if history, _ := g.historyRequests(); history["o/b"] != 0 {
		t.Errorf("the next repository's history was asked %d times on a spent budget", history["o/b"])
	}
	if row := r.health.runs["stars"]; row == nil || row.Failed != row.Repos {
		t.Errorf("the family row = %+v, want every repository counted", row)
	}
	if when := r.State.LastRun["stars"]; !when.Equal(before) {
		t.Errorf("a family stopped by a spent budget was marked as run at %s", when)
	}
}
