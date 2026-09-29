package collect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// gatewayGaveUp answers a query the way GitHub answers one it could not finish
// in its ten seconds: an HTML 502, which the fixture's client reads as that
// timeout whatever the time (see newFixtureServer).
func gatewayGaveUp(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
}

// isTimeout reports whether err is GitHub's GraphQL timeout.
func isTimeout(err error) bool {
	_, tooLarge := errors.AsType[*ghapi.TooLargeError](err)
	return tooLarge
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

// TestCommitsHandUpATimeoutTheyCannotHalvePast: a page the gateway gives up on
// at every size is halved while it is larger than ten, as the pull request
// walk's is, so fifty goes to 25, 12 and 6, and then it is a failure, handed up with the commits the walk read before it, which
// the runner writes, reports and does not record as walked.
func TestCommitsHandUpATimeoutTheyCannotHalvePast(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	pages := &commitPagesAsked{}
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		pages.note(vars)
		if vars["after"] == nil {
			f.write(w, "graphql_commits_page1.json")
			return
		}
		gatewayGaveUp(w)
	})
	points, err := Commits{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if !isTimeout(err) {
		t.Fatalf("err = %v, want the gateway's timeout handed up", err)
	}
	cursor := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 1"
	want := "50 <nil>, 50 " + cursor + ", 25 " + cursor + ", 12 " + cursor + ", 6 " + cursor
	if pages.String() != want {
		t.Errorf("pages asked = %s, want %s", pages, want)
	}
	if got := len(only(t, points, "gh_commit")); got != 2 {
		t.Errorf("got %d commits, want the first page's 2 kept beside the failure", got)
	}
}

// reply is how the fake answers one query of a walk: a fixture, its
// connection at next rewritten to say another page follows when next is set,
// or the gateway's timeout when fixture is empty. stray is a query the walk
// should not have asked at all, since it comes after the timeout.
type reply struct {
	fixture string
	next    []string
	stray   bool
}

// write sends the reply.
func (rp reply) write(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	if rp.fixture == "" {
		gatewayGaveUp(w)
		return
	}
	var page map[string]any
	mustUnmarshal(t, fixture(t, rp.fixture), &page)
	if len(rp.next) > 0 {
		conn := page
		for _, key := range rp.next {
			conn = conn[key].(map[string]any)
		}
		conn["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "page-2"}
	}
	_, _ = w.Write(mustMarshal(t, page))
}

// firstPageOnly answers the first page of a walk from a fixture that says
// another follows, and gives up on the page after it, the one that carries
// the cursor.
func firstPageOnly(cursor, fixture string, next ...string) func(string, map[string]any) reply {
	return func(_ string, vars map[string]any) reply {
		if vars[cursor] != nil {
			return reply{}
		}
		return reply{fixture: fixture, next: next}
	}
}

// starsThenSearchGivesUp is the one production met, on 2026-09-28: the stars
// answer, and the merged search gives up on its first page, after which
// nothing else is asked.
func starsThenSearchGivesUp(query string, _ map[string]any) reply {
	switch {
	case strings.Contains(query, "starredRepositories(first:"):
		return reply{fixture: "graphql_starred.json"}
	case strings.Contains(query, "search(type: ISSUE"):
		return reply{}
	}
	return reply{stray: true}
}

// commentsGiveUp answers everything the account walk asks before the issue
// comments, and gives up on those.
func commentsGiveUp(query string, _ map[string]any) reply {
	switch {
	case strings.Contains(query, "starredRepositories(first:"):
		return reply{fixture: "graphql_starred.json"}
	case strings.Contains(query, "search(type: ISSUE"):
		return reply{fixture: "graphql_search_issues.json"}
	case strings.Contains(query, "repositoryDiscussionComments"):
		return reply{fixture: "viewer_discussion_comments.json"}
	}
	return reply{}
}

// timeoutWalk is one walk the gateway gives up on part way, and the
// measurement the pages it read before the timeout write, empty for a single
// query that reads nothing before it.
type timeoutWalk struct {
	name    string
	answer  func(query string, vars map[string]any) reply
	collect func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error)
	kept    string
}

// timeoutWalks is every walk and single query that has no smaller page to ask.
func timeoutWalks() []timeoutWalk {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	outbound := func(walk Walk) func(context.Context, *ghapi.Client) ([]sink.Point, error) {
		return func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
			return Outbound{Login: "octocat", Walk: walk}.Collect(ctx, c, testNow)
		}
	}
	onlyStars := func(query string, vars map[string]any) reply {
		if !strings.Contains(query, "starredRepositories(first:") {
			return reply{stray: true}
		}
		return firstPageOnly("after", "graphql_starred.json", "data", "viewer", "starredRepositories")(query, vars)
	}
	return []timeoutWalk{
		{
			name:   "discussions",
			answer: firstPageOnly("after", "graphql_discussions.json", "data", "repository", "discussions"),
			collect: func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
				return Discussions{Walk: Unbounded}.Collect(ctx, c, testRepo, testNow)
			},
			kept: "gh_discussion",
		},
		{
			name:   "issue events of a sweep",
			answer: firstPageOnly("issueAfter", "graphql_issue_timeline.json", "data", "repository", "issues"),
			collect: func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
				return IssueEvents{Since: since}.Collect(ctx, c, testRepo, testNow)
			},
			kept: "gh_issue_event",
		},
		{
			name:   "issue events of a backfill",
			answer: firstPageOnly("issueAfter", "graphql_issue_timeline.json", "data", "repository", "issues"),
			collect: func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
				return IssueEvents{Walk: Unbounded}.Collect(ctx, c, testRepo, testNow)
			},
			kept: "gh_issue_event",
		},
		{
			name:   "labels and milestones",
			answer: func(string, map[string]any) reply { return reply{} },
			collect: func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
				return Planning{}.Collect(ctx, c, testRepo, testNow)
			},
		},
		{name: "stars given", answer: onlyStars, collect: outbound(Unbounded), kept: "gh_star_given"},
		{name: "work in other people's repositories", answer: starsThenSearchGivesUp, collect: outbound(Walk{}), kept: "gh_star_given"},
		{name: "comments left anywhere", answer: commentsGiveUp, collect: outbound(Walk{}), kept: "gh_discussion_comment"},
	}
}

// TestAWalkWithNoSmallerPageHandsUpTheTimeout is every other walk the gateway
// can give up on part way. Only one of them has met the timeout in
// production, once, on a page that answered every other time well inside the
// ten seconds, so none halves: each hands the timeout up with the rows of the
// pages it read, which the runner writes and reports, where each of them used
// to end there as though its data had run out and report success. A single
// query that is not a walk is the same failure with nothing read.
func TestAWalkWithNoSmallerPageHandsUpTheTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range timeoutWalks() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			// The per-item lists a backfill of issue events reads.
			timelineFixtures(t, f)
			f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
				rp := tc.answer(query, vars)
				if rp.stray {
					t.Errorf("asked on after the timeout: %s", query)
				}
				rp.write(t, w)
			})
			points, err := tc.collect(ctx(t), f.Client)
			if !isTimeout(err) {
				t.Fatalf("err = %v, want the gateway's timeout handed up", err)
			}
			checkPoints(t, points)
			if tc.kept == "" && len(points) != 0 {
				t.Errorf("a query that failed wrote %d points", len(points))
			}
			if tc.kept != "" && len(byMeasurement(points)[tc.kept]) == 0 {
				t.Errorf("the rows of the pages read before the timeout were dropped: %v", measurements(points))
			}
		})
	}
}

// TestBranchesHandUpARepositoryTheGatewayGivesUpOnAlone: a batch the gateway
// gave up on is halved, as aliasBatch halves one, and the repositories that
// answer in the smaller batches keep their rows. One that the gateway still
// gives up on alone has no smaller query left, and is a failure, where it used
// to be dropped as though it had no branches.
func TestBranchesHandUpARepositoryTheGatewayGivesUpOnAlone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, `name: "slow"`) {
			gatewayGaveUp(w)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"r0":{"defaultBranchRef":{"name":"main"},"refs":{"totalCount":1,"nodes":[` +
			`{"name":"main","target":{"oid":"abc123","committedDate":"2026-09-01T00:00:00Z"}}]}}}}`))
	})
	repos := []Repo{
		{Owner: "octocat", Name: "a", FullName: "octocat/a"},
		{Owner: "octocat", Name: "slow", FullName: "octocat/slow"},
	}
	points, err := Branches{Repos: repos, Batch: 2}.Collect(ctx(t), f.Client, testNow)
	if !isTimeout(err) {
		t.Fatalf("err = %v, want the gateway's timeout on the repository it gave up on alone", err)
	}
	checkPoints(t, points)
	find(t, points, "gh_branch", map[string]string{"repo": "a", "branch": "main"})
	if n := len(f.calls("/graphql")); n != 3 {
		t.Errorf("%d queries, want the batch of two and then each repository alone", n)
	}
}
