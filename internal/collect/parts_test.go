package collect

import (
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// timeOutWhole answers with GitHub's timeout every query that asks for more
// than one part of q, and hands the rest to the handler the fixture server
// had: a query that pages nothing and has grown past the gateway's ten
// seconds. It counts the queries it answered.
func timeOutWhole(f *fixtureServer, q partedQuery) *partsAsked {
	inner := f.graphql
	asked := &partsAsked{}
	f.graphQL(func(w http.ResponseWriter, r *http.Request, query string, vars map[string]any) {
		parts := 0
		for _, p := range q.parts {
			if strings.Contains(query, p.selection) {
				parts++
			}
		}
		asked.mu.Lock()
		if parts > 0 {
			asked.sizes = append(asked.sizes, parts)
		}
		asked.mu.Unlock()
		if parts > 1 {
			gatewayGaveUp(w)
			return
		}
		inner(w, r, query, vars)
	})
	return asked
}

// partsAsked is how many parts each query timeOutWhole saw asked for.
type partsAsked struct {
	mu    sync.Mutex
	sizes []int
}

// wantEveryPartAlone fails unless every query was asked whole first and then
// every part on its own, once for each time the collector asked it.
func (a *partsAsked) wantEveryPartAlone(t *testing.T, parts int) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	whole, alone := 0, 0
	for _, n := range a.sizes {
		switch n {
		case parts:
			whole++
		case 1:
			alone++
		}
	}
	if whole == 0 || len(a.sizes) == 0 || a.sizes[0] != parts || alone != whole*parts {
		t.Errorf("parts asked per query %v, want all %d at once and then each alone", a.sizes, parts)
	}
}

// measurementCounts is how many points of each measurement points holds.
func measurementCounts(points []sink.Point) map[string]int {
	out := map[string]int{}
	for _, p := range points {
		out[p.Measurement]++
	}
	return out
}

// partedAccount runs the account family, with every query of more than one
// part timed out when timeout is set.
func partedAccount(t *testing.T, timeout bool) ([]sink.Point, *partsAsked) {
	t.Helper()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		f.write(w, "graphql_account.json")
	})
	f.file("/users/octocat", "user_profile.json")
	f.handle("/user/packages", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	var asked *partsAsked
	if timeout {
		asked = timeOutWhole(f, accountQuery)
	}
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("timed out %v: %v", timeout, err)
	}
	return points, asked
}

// partedHistory runs the history family the same way.
func partedHistory(t *testing.T, timeout bool) ([]sink.Point, *partsAsked) {
	t.Helper()
	f := newFixtureServer(t)
	var calls []map[string]any
	historyServer(t, f, &calls)
	var asked *partsAsked
	if timeout {
		asked = timeOutWhole(f, historyQuery)
	}
	points, err := History{Login: "octocat", From: 2025}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("timed out %v: %v", timeout, err)
	}
	return points, asked
}

// partedAchievements runs the achievements family the same way.
func partedAchievements(t *testing.T, timeout bool) ([]sink.Point, *partsAsked) {
	t.Helper()
	f := progressFixture(t)
	var asked *partsAsked
	if timeout {
		asked = timeOutWhole(f, achievementCountsQuery)
	}
	a := Achievements{Login: "jmrplens", WebBase: f.srv.URL, Warn: func(string, ...any) {}}
	points, err := a.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("timed out %v: %v", timeout, err)
	}
	return points, asked
}

// TestAPartedQueryIsAskedInPartsWhenGitHubGivesUp: each query that pages
// nothing and is made of parts asks every part on its own when GitHub gave up
// on the whole, and writes what the whole would have.
func TestAPartedQueryIsAskedInPartsWhenGitHubGivesUp(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		run   func(*testing.T, bool) ([]sink.Point, *partsAsked)
		parts int
	}{
		"the account":             {partedAccount, len(accountQuery.parts)},
		"a year of contributions": {partedHistory, len(historyQuery.parts)},
		"the achievement counts":  {partedAchievements, len(achievementCountsQuery.parts)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			whole, _ := tc.run(t, false)
			inParts, asked := tc.run(t, true)
			asked.wantEveryPartAlone(t, tc.parts)
			if want, got := measurementCounts(whole), measurementCounts(inParts); !maps.Equal(want, got) {
				t.Errorf("points by measurement in parts %v, whole %v", got, want)
			}
		})
	}
}
