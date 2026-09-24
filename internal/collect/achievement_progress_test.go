package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// progressFixture serves the recorded page, the recorded counts and, for
// every page of the co-author walk, the recorded page of 2026-09-12 08:48Z:
// ten merged pull requests, three of them with a co-authored commit.
func progressFixture(t *testing.T) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return recordedAchievements(t) })
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		switch {
		case strings.Contains(query, "query achievementCounts("):
			f.write(w, "graphql_achievement_counts.json")
		case strings.Contains(query, "query coauthoredPulls("):
			f.write(w, "graphql_coauthored_pulls.json")
		default:
			t.Errorf("unexpected GraphQL query: %s", query)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	return f
}

// TestAchievementProgressFromTheCounts pins the row per tiered badge against
// the fixtures recorded on 2026-09-12: 1,847 merged pull requests, 6
// accepted answers, 113 stars on the most starred repository, and the one
// page of the walk with three co-authored pull requests. The first three
// agree with the page; the fourth cannot, since one page of the walk is 3
// where the page shows silver, and that is the disagreement the row
// carries, with no threshold and no percentage, rather than a bar drawn
// from a rule the page contradicts.
func TestAchievementProgressFromTheCounts(t *testing.T) {
	t.Parallel()
	f := progressFixture(t)
	var warned []string
	a := Achievements{Login: "jmrplens", WebBase: f.srv.URL, Warn: func(msg string, _ ...any) { warned = append(warned, msg) }}
	points, err := a.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_achievement", "gh_achievement_progress")
	progress := only(t, points, "gh_achievement_progress")
	if len(progress) != 4 {
		t.Fatalf("got %d progress rows, want one per tiered badge", len(progress))
	}
	for slug, want := range map[string]progressRow{
		"pull-shark":          {1847, 4, 0, 4, 1, 100},
		"galaxy-brain":        {6, 1, 8, 1, 1, 75},
		"starstruck":          {113, 1, 128, 1, 1, 88.3},
		"pair-extraordinaire": {3, 1, 0, 3, 0, 0},
	} {
		p := find(t, points, "gh_achievement_progress", map[string]string{"user": "jmrplens", "achievement": slug})
		checkProgressRow(t, p, slug, want, f.srv.URL)
	}
	// The badges carry their image too, for the shelf.
	shelf := find(t, points, "gh_achievement", map[string]string{"achievement": "pull-shark"})
	if shelf.Fields["image"] != "https://github.githubassets.com/assets/pull-shark-gold-90985540b385.png" {
		t.Errorf("badge image = %v", shelf.Fields["image"])
	}

	// The disagreement is one line, naming the badge and both tiers.
	got := Disagreements(points)
	if len(got) != 1 || got[0] != "pair-extraordinaire: count 3 implies tier 1, page shows 3" {
		t.Errorf("disagreements = %q", got)
	}
	if len(warned) != 0 {
		t.Errorf("warned %q on a walk that was whole", warned)
	}

	// The walk asked for public repositories only, over the life of the
	// account, a hundred to the page: the fixture answers one page, so one
	// query, beside the one for the counts.
	calls := f.calls("/graphql")
	if len(calls) != 2 {
		t.Fatalf("%d GraphQL queries, want the counts and one page of the walk", len(calls))
	}
	vars := graphQLVars(t, calls[1].Body)
	if vars["query"] != "is:pr is:merged is:public author:jmrplens merged:2017-05-25..2026-09-08" || vars["first"] != float64(100) {
		t.Errorf("walk variables = %v", vars)
	}
	if _, cursor := vars["after"]; cursor {
		t.Errorf("the first page carried a cursor: %v", vars)
	}
}

// progressRow is what one row of gh_achievement_progress should read.
type progressRow struct {
	count, tier, next, pageTier, agrees int64
	percent                             float64
}

// checkProgressRow holds one row to the numbers, the url of the badge's
// page, the badge image and the dating. A row that disagrees with the page
// carries neither a threshold nor a percentage: the bar is what the rule
// says, and the page just said the rule is wrong.
func checkProgressRow(t *testing.T, p sink.Point, slug string, want progressRow, base string) {
	t.Helper()
	got := progressRow{
		fieldInt(t, p, "count"), fieldInt(t, p, "tier_number"), 0,
		fieldInt(t, p, "page_tier"), fieldInt(t, p, "agrees"), 0,
	}
	if got.agrees == 0 {
		if hasField(p, "percent") || hasField(p, "next_threshold") {
			t.Errorf("%s disagrees with the page and still carries a bar: %v", slug, p.Fields)
		}
	} else {
		got.next = fieldInt(t, p, "next_threshold")
		pct, isFloat := p.Fields["percent"].(float64)
		if !isFloat {
			t.Errorf("%s: percent is %T, want a float", slug, p.Fields["percent"])
		}
		got.percent = pct
	}
	if got != want {
		t.Errorf("%s = %+v, want %+v", slug, got, want)
	}
	if p.Fields["url"] != base+"/jmrplens?achievement="+slug+"&tab=achievements" {
		t.Errorf("%s: url = %v", slug, p.Fields["url"])
	}
	image, _ := p.Fields["image"].(string)
	if !strings.HasPrefix(image, "https://github.githubassets.com/assets/"+slug+"-") {
		t.Errorf("%s: image = %q, want the badge image the page shows", slug, image)
	}
	if !p.Time.Equal(startOfDay(testNow)) {
		t.Errorf("%s stamped %s, want the start of the day", slug, p.Time)
	}
}

// graphQLVars reads the variables out of a recorded GraphQL request.
func graphQLVars(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	return env.Variables
}

// TestTierOf pins the reading of a count against the thresholds: below the
// first is no badge with the first as the target, a count on a threshold is
// that tier, and the top has no next.
func TestTierOf(t *testing.T) {
	t.Parallel()
	thresholds := [4]int{2, 16, 128, 1024}
	for _, tc := range []struct{ count, tier, next int }{
		{0, 0, 2},
		{1, 0, 2},
		{2, 1, 16},
		{15, 1, 16},
		{16, 2, 128},
		{127, 2, 128},
		{128, 3, 1024},
		{1023, 3, 1024},
		{1024, 4, 0},
		{5000, 4, 0},
	} {
		tier, next := tierOf(tc.count, thresholds)
		if tier != tc.tier || next != tc.next {
			t.Errorf("tierOf(%d) = %d, %d, want %d, %d", tc.count, tier, next, tc.tier, tc.next)
		}
	}
	for _, tc := range []struct {
		count, next int
		want        float64
	}{
		{0, 2, 0}, {1, 2, 50}, {6, 8, 75}, {113, 128, 88.3}, {29, 48, 60.4}, {1847, 0, 100},
	} {
		if got := percentOf(tc.count, tc.next); got != tc.want {
			t.Errorf("percentOf(%d, %d) = %v, want %v", tc.count, tc.next, got, tc.want)
		}
	}
}

// walkPage is one answer of the fake to a page of the walk.
type walkPage struct {
	issueCount int
	coauthored int
	plain      int
	next       string
}

func (p walkPage) body() string {
	var nodes []string
	for range p.coauthored {
		nodes = append(nodes, `{"mergeCommit":{"message":"Pair (#1)\n\nCo-authored-by: Someone <s@example.com>"},"commits":{"totalCount":1,"nodes":[{"commit":{"message":"work"}}]}}`)
	}
	for range p.plain {
		nodes = append(nodes, `{"mergeCommit":{"message":"Solo (#2)"},"commits":{"totalCount":1,"nodes":[{"commit":{"message":"work"}}]}}`)
	}
	return fmt.Sprintf(`{"data":{"search":{"issueCount":%d,"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}}}`,
		p.issueCount, p.next != "", p.next, strings.Join(nodes, ","))
}

var mergedRange = regexp.MustCompile(`merged:(\S+)`)

// walkServer answers the walk by the merged: range and cursor of each query,
// from a table, and records the ranges asked in order.
func walkServer(t *testing.T, pages map[string]walkPage) (*fixtureServer, func() []string) {
	t.Helper()
	f := newFixtureServer(t)
	var mu sync.Mutex
	var asked []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		q, _ := vars["query"].(string)
		key := mergedRange.FindStringSubmatch(q)[1]
		if after, _ := vars["after"].(string); after != "" {
			key += "@" + after
		}
		key = fmt.Sprintf("%s#%v", key, vars["first"])
		mu.Lock()
		asked = append(asked, key)
		mu.Unlock()
		page, ok := pages[key]
		if !ok {
			t.Errorf("unexpected walk query %s", key)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(page.body()))
	})
	return f, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

// TestCoauthoredWalkSplitsARangeOverTheSearchCap pins how the walk gets past
// the thousand results one search will page: a range whose count is over the
// cap is split in two by date, each half asked on its own, and a half still
// over the cap is split again. The first page of a range that has to be
// split is the price of learning so, one point.
func TestCoauthoredWalkSplitsARangeOverTheSearchCap(t *testing.T) {
	t.Parallel()
	f, asked := walkServer(t, map[string]walkPage{
		// 2020-01-01 to 2020-01-10 is ten days: split at the fifth.
		"2020-01-01..2020-01-10#100":    {issueCount: 1500, coauthored: 50, plain: 50, next: "x"},
		"2020-01-01..2020-01-05#100":    {issueCount: 1200, coauthored: 50, plain: 50, next: "x"},
		"2020-01-01..2020-01-03#100":    {issueCount: 200, coauthored: 2, plain: 98, next: "c1"},
		"2020-01-01..2020-01-03@c1#100": {issueCount: 200, coauthored: 3, plain: 97},
		"2020-01-04..2020-01-05#100":    {issueCount: 1000, coauthored: 1, plain: 99},
		"2020-01-06..2020-01-10#100":    {issueCount: 300, coauthored: 4, plain: 96},
	})
	w := coauthoredWalk{login: "o"}
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, from, from.AddDate(0, 0, 9)); err != nil {
		t.Fatal(err)
	}
	if w.pulls != 10 {
		t.Errorf("counted %d co-authored pull requests, want the 10 across the leaves and none from the pages that were split", w.pulls)
	}
	want := []string{
		"2020-01-01..2020-01-10#100",
		"2020-01-01..2020-01-05#100",
		"2020-01-01..2020-01-03#100", "2020-01-01..2020-01-03@c1#100",
		"2020-01-04..2020-01-05#100",
		"2020-01-06..2020-01-10#100",
	}
	if got := asked(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("asked %q\nwant  %q", got, want)
	}
	if w.queries != len(want) || w.capped {
		t.Errorf("queries = %d, capped = %t", w.queries, w.capped)
	}
}

// A single day over the cap cannot be split: the walk takes what the search
// will give and says the count is a floor.
func TestCoauthoredWalkStopsAtTheCapOnOneDay(t *testing.T) {
	t.Parallel()
	pages := map[string]walkPage{}
	cursor := ""
	for i := range 10 {
		next := fmt.Sprintf("c%d", i+1)
		key := "2020-01-01..2020-01-01#100"
		if cursor != "" {
			key = "2020-01-01..2020-01-01@" + cursor + "#100"
		}
		pages[key] = walkPage{issueCount: 1300, coauthored: 1, plain: 99, next: next}
		cursor = next
	}
	f, asked := walkServer(t, pages)
	w := coauthoredWalk{login: "o"}
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, day, day); err != nil {
		t.Fatal(err)
	}
	if n := len(asked()); n != 10 {
		t.Errorf("asked %d pages of a day over the cap, want the ten the search allows and not the eleventh it refuses", n)
	}
	if w.pulls != 10 || !w.capped {
		t.Errorf("pulls = %d, capped = %t, want 10 and a floor", w.pulls, w.capped)
	}
}

// TestCoauthoredWalkHalvesAPageTheGatewayRefuses pins the answer to the
// gateway's HTML 502: the same cursor, half the page, down to ten.
func TestCoauthoredWalkHalvesAPageTheGatewayRefuses(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var mu sync.Mutex
	var firsts []float64
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		first, _ := vars["first"].(float64)
		mu.Lock()
		firsts = append(firsts, first)
		mu.Unlock()
		if first > 25 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
			return
		}
		_, _ = w.Write([]byte(walkPage{issueCount: 3, coauthored: 2, plain: 1}.body()))
	})
	w := coauthoredWalk{login: "o"}
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, day, day.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(firsts) != "[100 50 25]" || w.pulls != 2 {
		t.Errorf("page sizes asked = %v, pulls = %d", firsts, w.pulls)
	}
}

// A pull request with more commits than one page carries, and no trailer
// in the ones read nor in its merge commit, is counted as not co-authored
// and reported as a floor: the trailer may sit past the hundredth commit.
func TestCoauthoredWalkReportsATruncatedPullRequest(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"search":{"issueCount":2,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
			`{"mergeCommit":{"message":"Big (#3)"},"commits":{"totalCount":140,"nodes":[{"commit":{"message":"work"}}]}},` +
			`{"mergeCommit":null,"commits":{"totalCount":140,"nodes":[{"commit":{"message":"work\n\nco-authored-by: A <a@example.com>"}}]}}` +
			`]}}}`))
	})
	w := coauthoredWalk{login: "o"}
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, day, day); err != nil {
		t.Fatal(err)
	}
	if w.pulls != 1 || w.truncated != 1 {
		t.Errorf("pulls = %d, truncated = %d, want 1 and 1: the trailer is read in any case, and only the unread one is a floor", w.pulls, w.truncated)
	}
}

// TestAchievementsWriteTheBadgesWhenTheCountsFail pins what a refused count
// costs: the progress rows of the day, said through Warn, and never the
// badges the page already gave.
func TestAchievementsWriteTheBadgesWhenTheCountsFail(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]http.HandlerFunc{
		"the counts refused": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		},
		"the counts without the user": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"pulls":{"issueCount":1},"answers":{"discussionCount":0},"user":null}}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			achievementsPage(f, "jmrplens", func() string { return recordedAchievements(t) })
			f.graphQL(func(w http.ResponseWriter, r *http.Request, _ string, _ map[string]any) { answer(w, r) })
			var warned []string
			a := Achievements{Login: "jmrplens", WebBase: f.srv.URL, Warn: func(msg string, _ ...any) { warned = append(warned, msg) }}
			points, err := a.Collect(ctx(t), f.Client, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := byMeasurement(points); len(got["gh_achievement"]) != 8 || len(got["gh_achievement_progress"]) != 0 {
				t.Errorf("got %d badges and %d progress rows, want 8 and none", len(got["gh_achievement"]), len(got["gh_achievement_progress"]))
			}
			if len(warned) != 1 || !strings.Contains(warned[0], "no progress rows today") {
				t.Errorf("warned %q, want the missing rows said once", warned)
			}
		})
	}

	// And a walk that fails after the counts came back is the same day
	// without progress rows.
	t.Run("the walk refused", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		achievementsPage(f, "jmrplens", func() string { return recordedAchievements(t) })
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
			if strings.Contains(query, "query achievementCounts(") {
				f.write(w, "graphql_achievement_counts.json")
				return
			}
			_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		})
		var warned []string
		a := Achievements{Login: "jmrplens", WebBase: f.srv.URL, Warn: func(msg string, _ ...any) { warned = append(warned, msg) }}
		points, err := a.Collect(ctx(t), f.Client, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := byMeasurement(points); len(got["gh_achievement"]) != 8 || len(got["gh_achievement_progress"]) != 0 {
			t.Errorf("got %d badges and %d progress rows, want 8 and none", len(got["gh_achievement"]), len(got["gh_achievement_progress"]))
		}
		if len(warned) != 1 || !strings.Contains(warned[0], "co-authored pull requests unavailable") {
			t.Errorf("warned %q", warned)
		}
	})
}

// A badge the page does not show still gets its row: how far the account is
// from a badge it has not earned is the same question, with the page's tier
// at zero. The recorded page is rewritten without its Galaxy Brain card.
func TestAchievementProgressForABadgeNotOnThePage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	page := recordedAchievements(t)
	cards := achievementCard.FindAllStringIndex(page, -1)
	var without string
	for _, span := range cards {
		if strings.Contains(page[span[0]:span[1]], `data-achievement-slug="galaxy-brain"`) {
			without = page[:span[0]] + page[span[1]:]
		}
	}
	if without == "" {
		t.Fatal("the recorded page has no Galaxy Brain card to remove")
	}
	achievementsPage(f, "jmrplens", func() string { return without })
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, "query achievementCounts(") {
			f.write(w, "graphql_achievement_counts.json")
			return
		}
		f.write(w, "graphql_coauthored_pulls.json")
	})
	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(only(t, points, "gh_achievement")); n != 7 {
		t.Errorf("%d badges, want the 7 left on the page", n)
	}
	p := find(t, points, "gh_achievement_progress", map[string]string{"achievement": "galaxy-brain"})
	if fieldInt(t, p, "count") != 6 || fieldInt(t, p, "tier_number") != 1 || fieldInt(t, p, "page_tier") != 0 || fieldInt(t, p, "agrees") != 0 {
		t.Errorf("galaxy-brain off the page = %v", p.Fields)
	}
	if hasField(p, "percent") || hasField(p, "next_threshold") {
		t.Errorf("a count the page has not caught up with is a disagreement, and a disagreement draws no bar: %v", p.Fields)
	}
	if p.Fields["name"] != "Galaxy Brain" || hasField(p, "image") {
		t.Errorf("a badge off the page has its name from the table and no image: %v", p.Fields)
	}
}
