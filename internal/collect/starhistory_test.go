package collect

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// historyPath is the one repository's daily star history.
const historyPath = "/repos/octocat/hello-world/stargazers/history"

// The two fixtures are pinned to testNow, a Tuesday. Page one is thirty
// weeks, newest first, from the week of 2026-09-06: that week has a star on
// its Sunday and two on its Tuesday and zeros for the days still to come, the
// week before is labeled at 07:00 UTC rather than at midnight and has one
// star on its Wednesday, 2026-06-20 has three, and every other week is empty.
// Page two is a short last page of three weeks, one of them empty.
const (
	historyPage1 = "stargazers_history_page1.json"
	historyPage2 = "stargazers_history_page2.json"
)

// historyRoutes serves the history page by page: page one from its fixture
// and page n+2 from later[n], an empty name or a page past the list being the
// empty array GitHub answers past the last page. It never sends a Link
// header, and neither does GitHub on a 304.
func historyRoutes(f *fixtureServer, later ...string) {
	f.handle(historyPath, func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		switch {
		case page == 1:
			f.write(w, historyPage1)
		case page-2 < len(later) && later[page-2] != "":
			f.write(w, later[page-2])
		default:
			_, _ = w.Write([]byte("[]"))
		}
	})
}

// dayOf is the row stamped at the start of date, and whether there is one.
func dayOf(points []sink.Point, date string) (sink.Point, bool) {
	for _, p := range points {
		if p.Time.Format(time.DateOnly) == date {
			return p, true
		}
	}
	return sink.Point{}, false
}

// TestStarHistoryWritesOneRowPerDay is the whole walk against the recording:
// every day of page one, the days of page two that have a star, each stamped
// at the start of its own date.
func TestStarHistoryWritesOneRowPerDay(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f, historyPage2)
	points, err := StarHistory{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	checkGolden(t, "star_history", points)
	for _, p := range points {
		if !p.Time.Equal(p.Time.UTC().Truncate(24 * time.Hour)) {
			t.Errorf("a day stamped at %s, want the start of its date in UTC", p.Time)
		}
	}
	for date, want := range map[string]int{
		"2026-09-06": 1, // the Sunday the current week is labeled with
		"2026-09-08": 2, // the Tuesday testNow falls on, index two of its week
		"2026-06-20": 3, // a Saturday, the last day of its week
	} {
		p, ok := dayOf(points, date)
		if !ok || p.Fields["stars"] != want {
			t.Errorf("%s = %v (found %t), want %d stars", date, p.Fields, ok, want)
		}
	}
}

// TestAWeekLabeledOffMidnightKeepsItsDates: the label is anchored where the
// API puts it, and a week label moved to 07:00 UTC, the Pacific midnight the
// days are counted by, must not move every day of that week onto an hour.
func TestAWeekLabeledOffMidnightKeepsItsDates(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f)
	points, err := StarHistory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wednesday, ok := dayOf(points, "2026-09-02")
	if !ok || wednesday.Fields["stars"] != 1 {
		t.Fatalf("the Wednesday of the week labeled 07:00 = %v (found %t), want one star", wednesday.Fields, ok)
	}
	if want := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC); !wednesday.Time.Equal(want) {
		t.Errorf("stamped %s, want %s", wednesday.Time, want)
	}
}

// TestAStarDayIsDatedByItsWeekNotByTheClock is the weekly-anchoring rule. A
// day's date comes from the week GitHub labeled it with, so read three weeks
// after the fixture's newest week, the same days land on the same dates, and
// nothing lands on the weeks the clock has reached and the page has not. A
// day counted from the clock instead would put this page's newest week in the
// clock's week, and in the hours after midnight UTC on a Sunday, while GitHub
// is still on Saturday in Los Angeles, it would move every row by a week.
func TestAStarDayIsDatedByItsWeekNotByTheClock(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f)
	now := testNow.AddDate(0, 0, 21)
	points, err := StarHistory{}.Collect(ctx(t), f.Client, testRepo, now)
	if err != nil {
		t.Fatal(err)
	}
	for date, want := range map[string]int{"2026-09-06": 1, "2026-09-08": 2, "2026-06-20": 3} {
		if p, ok := dayOf(points, date); !ok || p.Fields["stars"] != want {
			t.Errorf("read on %s, %s = %v (found %t), want %d stars", now.Format(time.DateOnly), date, p.Fields, ok, want)
		}
	}
	// The Sunday after the fixture's newest week: the page holds nothing from
	// there on, whatever the clock says.
	end := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	for _, p := range points {
		if !p.Time.Before(end) {
			t.Errorf("a row on %s, past the newest week the page holds", p.Time.Format(time.DateOnly))
		}
	}
	// Every day of the thirty weeks, the ones still to come at testNow
	// included, since none of them is ahead of this clock.
	if len(points) != 30*7 {
		t.Errorf("%d rows, want the page's %d days", len(points), 30*7)
	}
}

// TestAStarDayIsAboutARepositoryAndNothingElse: the endpoint names no
// stargazer, so the row carries the three repository tags and a count, and no
// url: the stargazer list is a page only the owner can open, and no table
// lists this row.
func TestAStarDayIsAboutARepositoryAndNothingElse(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f)
	points, err := StarHistory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("no rows, so there is nothing to check")
	}
	want := repoTags(testRepo.Owner, testRepo.Name)
	for _, p := range points {
		if p.Measurement != "gh_star_day" {
			t.Errorf("measurement %q", p.Measurement)
		}
		if !reflect.DeepEqual(p.Tags, want) {
			t.Errorf("tags %v, want %v", p.Tags, want)
		}
		if _, isInt := p.Fields["stars"].(int); !isInt || len(p.Fields) != 1 {
			t.Errorf("fields %v, want one integer, stars", p.Fields)
		}
	}
}

// TestZerosAreWrittenOnlyOnThePageASweepReadsAgain. A day that someone
// unstarred goes back to zero on page one, and only a zero row can rewrite it.
// Older pages are read once, and their zeros would be a row per day back to
// the repository's creation for nothing.
func TestZerosAreWrittenOnlyOnThePageASweepReadsAgain(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f, historyPage2)
	points, err := StarHistory{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Page one: the Monday between two starred days, and a whole empty week.
	for _, date := range []string{"2026-09-07", "2026-08-23", "2026-08-29", "2026-02-15"} {
		if p, ok := dayOf(points, date); !ok || p.Fields["stars"] != 0 {
			t.Errorf("page one's %s = %v (found %t), want a zero row", date, p.Fields, ok)
		}
	}
	// Page two: its starred days, and none of its empty ones.
	for date, want := range map[string]int{"2026-02-13": 2, "2026-01-25": 1, "2026-01-31": 1} {
		if p, ok := dayOf(points, date); !ok || p.Fields["stars"] != want {
			t.Errorf("page two's %s = %v (found %t), want %d", date, p.Fields, ok, want)
		}
	}
	for _, date := range []string{"2026-02-08", "2026-02-01", "2026-02-07", "2026-01-26"} {
		if p, ok := dayOf(points, date); ok {
			t.Errorf("page two wrote the empty day %s: %v", date, p.Fields)
		}
	}
}

// TestNoStarDayIsWrittenAheadOfTheSweep: GitHub fills the current week's
// days still to come with zeros, and a row dated in the future would stand in
// every dashboard until the day arrived.
func TestNoStarDayIsWrittenAheadOfTheSweep(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f)
	points, err := StarHistory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		if p.Time.After(testNow) {
			t.Errorf("a day after the sweep was written: %s", p.Time)
		}
	}
	if _, ok := dayOf(points, testNow.Format(time.DateOnly)); !ok {
		t.Error("the sweep's own day, begun in UTC, was not written")
	}
	// Twenty-nine whole weeks and the three days of this one so far.
	if len(points) != 29*7+3 {
		t.Errorf("%d rows from page one, want 206", len(points))
	}
}

// TestAStarHistorySweepReadsTheNewestPageOnly: an ordinary sweep takes
// Walk{}, which is page one, even when page one is full and more lies
// behind it. It asks with the default media type, the one the ETag it gets
// back belongs to.
func TestAStarHistorySweepReadsTheNewestPageOnly(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	historyRoutes(f, historyPage2)
	if _, err := (StarHistory{}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(historyPath)
	if len(calls) != 1 {
		t.Fatalf("a sweep made %d requests, want page one alone", len(calls))
	}
	if page := calls[0].Query["page"]; page != "" {
		t.Errorf("page one was asked for as page=%s; the bare path is the one that is cached", page)
	}
	if got := calls[0].Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want the default: the ETag varies with it", got)
	}
}

// TestAWholeStarHistoryStopsWhereTheDataDoes: an unbounded walk ends at a
// page shorter than thirty weeks, at an empty page, at the 422 GitHub answers
// past its paging ceiling, and at the cap, whichever comes first, and none of
// them is an error. The Link header is never asked: the fake serves none.
func TestAWholeStarHistoryStopsWhereTheDataDoes(t *testing.T) {
	t.Parallel()
	limited := func(f *fixtureServer) {
		f.handle(historyPath, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"In order to keep the API fast for everyone, pagination is limited for this resource."}`))
				return
			}
			f.write(w, historyPage1)
		})
	}
	for _, tc := range []struct {
		name  string
		walk  Walk
		serve func(f *fixtureServer)
		calls int
		rows  int
	}{
		{"a short page", Unbounded, func(f *fixtureServer) { historyRoutes(f, historyPage2) }, 2, 209},
		// Page one again as page two is a full page, so only the empty
		// page three can end the walk. Read as a later page, it adds its
		// four starred days and none of its zeros.
		{"an empty page", Unbounded, func(f *fixtureServer) { historyRoutes(f, historyPage1, "") }, 3, 206 + 4},
		{"a 422", Unbounded, limited, 2, 206},
		{"the cap", Walk{Pages: 2}, func(f *fixtureServer) { historyRoutes(f, historyPage1, historyPage1) }, 2, 206 + 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			tc.serve(f)
			points, complete, err := StarHistory{Walk: tc.walk}.Read(ctx(t), f.Client, testRepo, testNow)
			if err != nil {
				t.Fatalf("the walk ended in an error: %v", err)
			}
			if !complete {
				t.Error("a walk that ended where the data does was reported cut short")
			}
			if n := len(f.calls(historyPath)); n != tc.calls {
				t.Errorf("%d requests, want %d", n, tc.calls)
			}
			if len(points) != tc.rows {
				t.Errorf("%d rows, want %d", len(points), tc.rows)
			}
		})
	}
}

// TestAStarHistoryBackfillStopsAtItsDate: a backfill bounded by date reads
// until a page reaches back past it, and keeps the whole of that last page,
// the weeks before the bound included.
func TestAStarHistoryBackfillStopsAtItsDate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		since time.Time
		calls int
	}{
		// Page one reaches back to 2026-02-15, so a bound after it is met.
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 1},
		// One before it is not, and page two is read.
		{time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), 2},
	} {
		f := newFixtureServer(t)
		historyRoutes(f, historyPage2)
		points, err := StarHistory{Walk: Walk{Pages: -1, Since: tc.since}}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls(historyPath)); n != tc.calls {
			t.Errorf("since %s: %d requests, want %d", tc.since.Format(time.DateOnly), n, tc.calls)
		}
		_, before := dayOf(points, "2026-01-25")
		if tc.calls == 2 && !before {
			t.Errorf("since %s: the last page's week before the bound was dropped", tc.since.Format(time.DateOnly))
		}
	}
}

// TestAStarHistoryAnsweredFromTheCacheWalksAsFarAsBefore is the case the
// walk is built around. GitHub's 304 carries no Link header, and a walk that
// read its pages off one would stop at page one every time the cache answered
// it. Walked twice through one client, the second time entirely on 304s, it
// reads the same pages and writes the same rows.
func TestAStarHistoryAnsweredFromTheCacheWalksAsFarAsBefore(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle(historyPath, func(w http.ResponseWriter, r *http.Request) {
		name := historyPage1
		if r.URL.Query().Get("page") == "2" {
			name = historyPage2
		}
		sum := sha256.Sum256(fixture(t, name))
		tag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.write(w, name)
	})
	first, err := StarHistory{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StarHistory{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls(historyPath)
	if len(calls) != 4 {
		t.Fatalf("%d requests over two walks, want two pages each", len(calls))
	}
	for _, c := range calls[2:] {
		if c.Header.Get("If-None-Match") == "" {
			t.Errorf("the second walk asked %v without the validator, so it was never a 304", c.Query)
		}
	}
	if !slices.Equal(lines(first), lines(second)) {
		t.Errorf("the walk answered from the cache wrote %d rows, the first %d", len(second), len(first))
	}
}

// TestAStarHistoryThatIsNotThereIsNothingToCollect: a repository the token
// cannot read answers 404, and so does every repository on a GitHub
// Enterprise Server, which does not serve the history. That is nothing here
// rather than a failure, and not a history read either.
func TestAStarHistoryThatIsNotThereIsNothingToCollect(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, complete, err := StarHistory{Walk: Unbounded}.Read(ctx(t), f.Client, testRepo, testNow)
	if err != nil || points != nil {
		t.Errorf("404: err=%v points=%d, want nil and none", err, len(points))
	}
	if complete {
		t.Error("a history that answered 404 was reported read to its end")
	}
}

// TestAStarHistoryThatSaysNothingIsHerePartWayIsCutShort: past page one a 403
// with no rate-limit headers, a secondary limit GitHub did not name, or a 404,
// a repository gone mid-walk, keeps the pages read and is no failure, but it
// is not the end of the history, which is a short page, an empty one or a
// 422, and the walk says so.
func TestAStarHistoryThatSaysNothingIsHerePartWayIsCutShort(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		f := newFixtureServer(t)
		f.handle(historyPath, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"` + http.StatusText(status) + `"}`))
				return
			}
			f.write(w, historyPage1)
		})
		points, complete, err := StarHistory{Walk: Unbounded}.Read(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Errorf("%d on page two: %v, want nothing here", status, err)
		}
		if len(points) != 206 {
			t.Errorf("%d on page two: %d rows kept, want page one's 206", status, len(points))
		}
		if complete {
			t.Errorf("%d on page two: the walk was reported read to its end", status)
		}
	}
}

// TestAStarHistoryThatFailsPartWayKeepsWhatItRead: a 502 on page two is a
// failure the runner has to hear about, and page one's rows are still true.
func TestAStarHistoryThatFailsPartWayKeepsWhatItRead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle(historyPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		f.write(w, historyPage1)
	})
	points, err := StarHistory{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil {
		t.Fatal("a 502 on page two was reported as the end of the history")
	}
	if len(points) != 206 {
		t.Errorf("%d rows kept, want page one's 206", len(points))
	}
}

// TestAStarHistoryOutOfBudgetIsAFailure: GitHub answers a spent budget with
// the 403 a feature that is off answers with, and reading it as nothing here
// would publish a history that quietly stopped.
func TestAStarHistoryOutOfBudgetIsAFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle(historyPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(testNow.Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	if _, err := (StarHistory{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
		t.Fatal("a spent budget read as a repository with no history")
	}
}
