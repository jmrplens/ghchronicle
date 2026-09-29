package collect

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// gatewayGaveUp answers a query the way GitHub answers one it could not finish
// in its ten seconds: an HTML 502, which the fixture's client reads as that
// timeout whatever the time (see newFixtureServer).
func gatewayGaveUp(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
}

// commitPagesAsked records the page size and cursor of every commit query, in
// order, as "first after".
type commitPagesAsked struct {
	mu    sync.Mutex
	asked []string
}

func (c *commitPagesAsked) note(vars map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, fmt.Sprint(vars["first"], " ", vars["after"]))
}

func (c *commitPagesAsked) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.asked, ", ")
}

// TestCommitsHalveAPageTheGatewayGaveUpOnAndGoOn is what twelve commit walks
// met in production between 2026-09-11 and 2026-09-18, all at a page of
// fifty: the gateway's 502 or 504 after ten seconds, which the walk took for
// the end of the history and reported as a success. The page is now asked
// again at half the size on the same cursor, and the walk goes on at that size
// to the end of the history, each commit written once.
func TestCommitsHalveAPageTheGatewayGaveUpOnAndGoOn(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	pages := &commitPagesAsked{}
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		pages.note(vars)
		switch {
		case vars["after"] == nil:
			f.write(w, "graphql_commits_page1.json")
		case vars["first"] == float64(50):
			gatewayGaveUp(w)
		default:
			f.write(w, "graphql_commits_page2.json")
		}
	})
	points, err := Commits{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("a page the gateway gave up on must be asked again smaller, not failed: %v", err)
	}
	checkPoints(t, points)
	cursor := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 1"
	if want := "50 <nil>, 50 " + cursor + ", 25 " + cursor; pages.String() != want {
		t.Errorf("pages asked = %s, want %s", pages, want)
	}
	if got := len(only(t, points, "gh_commit")); got != 3 {
		t.Errorf("got %d commits, want the 3 of both pages, each once", got)
	}
}

// TestASweepReadsTheSpanItWasGivenAtAHalvedPage: a sweep reads one page of
// fifty commits. Halved to twenty-five it reads two, which is the same span of
// the history in one more request, rather than half of what it was asked for.
func TestASweepReadsTheSpanItWasGivenAtAHalvedPage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	pages := &commitPagesAsked{}
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		pages.note(vars)
		switch {
		case vars["first"] == float64(50):
			gatewayGaveUp(w)
		case vars["after"] == nil:
			f.write(w, "graphql_commits_page1.json")
		default:
			f.write(w, "graphql_commits_page2.json")
		}
	})
	points, err := Commits{Since: testNow.AddDate(0, 0, -30)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	cursor := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 1"
	if want := "50 <nil>, 25 <nil>, 25 " + cursor; pages.String() != want {
		t.Errorf("pages asked = %s, want %s", pages, want)
	}
	if got := len(only(t, points, "gh_commit")); got != 3 {
		t.Errorf("got %d commits, want the 3 of both halved pages", got)
	}
}
