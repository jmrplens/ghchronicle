package collect

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

func TestForks(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/forks", "forks.json")
	points, err := Forks{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/repos/octocat/hello-world/forks")
	if len(calls) != 1 || calls[0].Query["sort"] != "oldest" {
		t.Errorf("calls = %d, query %v", len(calls), calls[0].Query)
	}
	if len(points) != 2 {
		t.Fatalf("got %d forks, want 2", len(points))
	}
	alice := find(t, points, "gh_fork", map[string]string{"by": "alice"})
	if want := time.Date(2025, 2, 10, 10, 0, 0, 0, time.UTC); !alice.Time.Equal(want) {
		t.Errorf("fork stamped %s, want created_at %s", alice.Time, want)
	}
	// Pushed ten days after forking: a real derivative.
	if alice.Fields["advanced"] != true || fieldInt(t, alice, "stars") != 2 || fieldInt(t, alice, "forks") != 1 {
		t.Errorf("alice's fork = %v", alice.Fields)
	}
	// Pushed thirty seconds after forking: the fork itself, a bookmark.
	bob := find(t, points, "gh_fork", map[string]string{"by": "bob"})
	if bob.Fields["advanced"] != false {
		t.Errorf("bob's fork = %v", bob.Fields)
	}
	if fieldInt(t, bob, "days_since_push") != 38 {
		t.Errorf("days_since_push = %v", bob.Fields["days_since_push"])
	}
}

func TestPlanning(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotVars map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		gotVars = vars
		if !strings.Contains(query, "milestones(") || !strings.Contains(query, "labels(") {
			t.Errorf("query does not ask for labels and milestones:\n%s", query)
		}
		f.write(w, "graphql_planning.json")
	})
	points, err := Planning{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if gotVars["owner"] != "octocat" || gotVars["name"] != "hello-world" {
		t.Errorf("variables = %v", gotVars)
	}
	day := startOfDay(testNow)

	labels := only(t, points, "gh_label")
	if len(labels) != 1 {
		t.Fatalf("got %d labels, want 1: an unused label is noise", len(labels))
	}
	if labels[0].Tags["label"] != "bug" || fieldInt(t, labels[0], "used") != 7 || !labels[0].Time.Equal(day) {
		t.Errorf("label = %v %v at %s", labels[0].Tags, labels[0].Fields, labels[0].Time)
	}

	open := find(t, points, "gh_milestone", map[string]string{"milestone": "v2.0"})
	if open.Tags["state"] != "OPEN" || !open.Time.Equal(day) {
		t.Errorf("open milestone = %v at %s", open.Tags, open.Time)
	}
	due := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if fieldInt(t, open, "days_to_due") != int64(due.Sub(testNow).Hours()/24) || open.Fields["progress"] != 37.5 {
		t.Errorf("open milestone fields = %v", open.Fields)
	}
	if hasField(open, "seconds_to_close") {
		t.Error("an open milestone has no close time")
	}
	closed := find(t, points, "gh_milestone", map[string]string{"milestone": "v1.2"})
	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	if fieldInt(t, closed, "seconds_to_close") != int64(closedAt.Sub(created).Seconds()) {
		t.Errorf("seconds_to_close = %v", closed.Fields["seconds_to_close"])
	}
}

func TestPlanningGraphQLErrorIsSilent(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"gone"}]}`))
	})
	points, err := Planning{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("err=%v points=%d", err, len(points))
	}
}

// outboundFixture answers the four GraphQL shapes Outbound asks for, all of
// them from `viewer` or from search, one query each, and records the search
// queries and the starred variables it was sent.
func outboundFixture(t *testing.T, searches *[]string, starred *[]map[string]any) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			if starred != nil {
				*starred = append(*starred, vars)
			}
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			if searches != nil {
				q, _ := vars["query"].(string)
				*searches = append(*searches, q)
			}
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "repositoryDiscussionComments"):
			// gh_discussion_comment is written by two collectors from two
			// queries. The other one is checked the same way in
			// repoactivity_test: a selection added to one and not the other
			// gives the measurement two shapes, and half its rows would
			// answer "was this thread ever answered" with a zero value.
			for _, field := range []string{
				"answerChosenBy", "answer {", "closed", "stateReason", "isAnswerable",
			} {
				if !strings.Contains(query, field) {
					t.Errorf("query does not ask for %s:\n%s", field, query)
				}
			}
			f.write(w, "viewer_discussion_comments.json")
		default:
			f.write(w, "viewer_issue_comments.json")
		}
	})
	return f
}

func TestOutbound(t *testing.T) {
	t.Parallel()
	var searches []string
	var starred []map[string]any
	f := outboundFixture(t, &searches, &starred)

	points, err := Outbound{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_star_given", "gh_external_contribution",
		"gh_discussion_comment", "gh_issue_comment")

	checkOutboundComments(t, points)
	checkStarsGiven(t, points, starred)
	checkSearchesAreScoped(t, searches)
	checkExternalContributions(t, points)
	// The rows are the ones the REST paths wrote for the same three stars and
	// two items, recorded before the move to GraphQL.
	checkGolden(t, "outbound", points, "gh_star_given", "gh_external_contribution")
}

// checkOutboundComments reads the comments: whose repository they were left
// in is the fact nothing else here sees, so it is a tag.
func checkOutboundComments(t *testing.T, points []sink.Point) {
	t.Helper()
	// A comment in someone else's repository is the fact nothing else here
	// sees, so which side of that line it falls on is a tag.
	answer := find(t, points, "gh_discussion_comment", map[string]string{"repo": "fosrl/pangolin"})
	if answer.Tags["own"] != "false" || answer.Tags["is_answer"] != "true" || fieldInt(t, answer, "answers") != 1 {
		t.Errorf("accepted answer elsewhere = %v %v", answer.Tags, answer.Fields)
	}
	if want := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC); !answer.Time.Equal(want) {
		t.Errorf("comment stamped %s, want createdAt %s", answer.Time, want)
	}
	mine := find(t, points, "gh_discussion_comment", map[string]string{"repo": "octocat/hello-world"})
	if mine.Tags["own"] != "true" || fieldInt(t, mine, "answers") != 0 {
		t.Errorf("own repository comment = %v %v", mine.Tags, mine.Fields)
	}
	checkDiscussionContext(t, points)
	comment := find(t, points, "gh_issue_comment", map[string]string{"repo": "torvalds/linux"})
	if comment.Tags["own"] != "false" || comment.Tags["number"] != "42" {
		t.Errorf("issue comment = %v", comment.Tags)
	}
}

// checkDiscussionContext reads what the thread says about itself, which is the
// only thing that separates a question nobody answered from one somebody else
// answered: both comments carry is_answer=false.
func checkDiscussionContext(t *testing.T, points []sink.Point) {
	t.Helper()
	checkOwnAcceptedAnswer(t, points)
	checkAnsweredBySomebodyElse(t, points)
	checkUnansweredIdea(t, points)
}

// checkOwnAcceptedAnswer reads the thread this account answered.
func checkOwnAcceptedAnswer(t *testing.T, points []sink.Point) {
	t.Helper()
	won := find(t, points, "gh_discussion_comment", map[string]string{"repo": "fosrl/pangolin"})
	if won.Fields["discussion_answered"] != true || won.Fields["answered_by"] != "octocat" {
		t.Errorf("the account's own accepted answer = %v", won.Fields)
	}
	// Answered by this account, marked by the person who asked. The two are
	// different fields because they are different people.
	if won.Fields["answer_chosen_by"] != "nozamdavid" || won.Fields["category"] != "Q&A" {
		t.Errorf("answer chosen by = %v", won.Fields)
	}
	if won.Fields["discussion_closed"] != false || hasField(won, "state_reason") ||
		hasField(won, "seconds_to_close") {
		t.Errorf("an open thread has no close = %v", won.Fields)
	}
	if fieldInt(t, won, "seconds_to_answer") != 4*3600 {
		t.Errorf("seconds_to_answer = %v", won.Fields["seconds_to_answer"])
	}
	if won.Fields["discussion_answerable"] != true {
		t.Errorf("a Q&A thread is answerable: %v", won.Fields)
	}
}

// checkAnsweredBySomebodyElse reads the thread where the account's comment
// lost to somebody else's answer.
func checkAnsweredBySomebodyElse(t *testing.T, points []sink.Point) {
	t.Helper()
	lost := find(t, points, "gh_discussion_comment", map[string]string{"repo": "ThrowTheSwitch/Ceedling"})
	if lost.Tags["is_answer"] != "false" || fieldInt(t, lost, "answers") != 0 {
		t.Errorf("a comment that did not win is still not the answer: %v %v", lost.Tags, lost.Fields)
	}
	if lost.Fields["discussion_answered"] != true || lost.Fields["answered_by"] != "mvandervoord" {
		t.Errorf("the thread was answered by somebody else: %v", lost.Fields)
	}
	if lost.Fields["discussion_closed"] != true || lost.Fields["state_reason"] != "RESOLVED" {
		t.Errorf("closed thread = %v", lost.Fields)
	}
	if fieldInt(t, lost, "seconds_to_close") != 3*86400 {
		t.Errorf("seconds_to_close = %v", lost.Fields["seconds_to_close"])
	}
	if lost.Fields["discussion_answerable"] != true {
		t.Errorf("a Q&A thread is answerable: %v", lost.Fields)
	}
}

// checkUnansweredIdea reads the thread nobody answered, which used to be
// indistinguishable from the two above.
func checkUnansweredIdea(t *testing.T, points []sink.Point) {
	t.Helper()
	open := find(t, points, "gh_discussion_comment", map[string]string{"repo": "octocat/hello-world"})
	if open.Fields["discussion_answered"] != false || hasField(open, "answered_by") ||
		hasField(open, "seconds_to_answer") {
		t.Errorf("unanswered thread = %v", open.Fields)
	}
	if open.Fields["category"] != "Ideas" {
		t.Errorf("category = %v", open.Fields["category"])
	}
	// An idea cannot be answered, and GitHub says so by answering isAnswered
	// with null rather than false. Without discussion_answerable this row
	// would read exactly like the Q&A question nobody has answered yet.
	if open.Fields["discussion_answerable"] != false {
		t.Errorf("an idea is not answerable: %v", open.Fields)
	}
}

// checkStarsGiven reads the stars this account handed out: one query of a
// hundred, newest first, which is the order the REST listing served them in.
func checkStarsGiven(t *testing.T, points []sink.Point, starred []map[string]any) {
	t.Helper()
	if len(starred) != 1 || starred[0]["first"] != float64(100) || starred[0]["after"] != nil {
		t.Errorf("starred queries = %v, want one page of a hundred from the start", starred)
	}
	stars := only(t, points, "gh_star_given")
	if len(stars) != 3 {
		t.Fatalf("got %d stars given", len(stars))
	}
	// GitHub detects no language in a repository that is only prose, and
	// answers null: measured on 2026-09-10, 9 of the 93 repositories this
	// account has starred. Written raw that is an empty tag value, which
	// InfluxDB drops, leaving those rows in a series with no language tag.
	prose := find(t, points, "gh_star_given", map[string]string{"repo": "sindresorhus/awesome"})
	if prose.Tags["language"] != noneTag {
		t.Errorf("a repository with no language must still carry the tag, got %q", prose.Tags["language"])
	}
	goStar := find(t, points, "gh_star_given", map[string]string{"repo": "golang/go"})
	if goStar.Tags["language"] != "Go" || goStar.Tags["user"] != "octocat" || fieldInt(t, goStar, "repo_stars") != 125000 {
		t.Errorf("star given = %v %v", goStar.Tags, goStar.Fields)
	}
	if want := time.Date(2026, 8, 12, 20, 0, 0, 0, time.UTC); !goStar.Time.Equal(want) {
		t.Errorf("star stamped %s, want starred_at %s", goStar.Time, want)
	}
}

// checkSearchesAreScoped reads the five searches: each one scoped away from
// the account's own repositories, and each one a query of its own, so one
// that is refused costs only its own rows.
func checkSearchesAreScoped(t *testing.T, searches []string) {
	t.Helper()
	if len(searches) != 5 {
		t.Fatalf("made %d searches, want 5", len(searches))
	}
	for _, q := range searches {
		if !strings.Contains(q, "author:octocat") || !strings.Contains(q, "-user:octocat") {
			t.Errorf("search query %q is not scoped to the account's outside work", q)
		}
	}
	// The state a search is for is a qualifier of the query, which is the
	// only way search will separate merged from closed.
	if !slices.Contains(searches, "is:pr is:merged author:octocat -user:octocat") ||
		!slices.Contains(searches, "is:pr is:closed is:unmerged author:octocat -user:octocat") {
		t.Errorf("searches = %q", searches)
	}
}

// checkExternalContributions reads the work done in other people's
// repositories, dated when it closed or at the start of today while it is open.
func checkExternalContributions(t *testing.T, points []sink.Point) {
	t.Helper()
	contribs := only(t, points, "gh_external_contribution")
	if len(contribs) != 10 {
		t.Errorf("got %d contributions, want 2 items from each of 5 searches", len(contribs))
	}
	merged := find(t, points, "gh_external_contribution", map[string]string{"kind": "pull_request", "state": "merged", "number": "118"})
	if merged.Tags["repo"] != "someone/else" || merged.Tags["user"] != "octocat" {
		t.Errorf("merged tags = %v", merged.Tags)
	}
	if fieldInt(t, merged, "merged") != 1 || fieldInt(t, merged, "seconds_to_merge") != 4*86400 || merged.Fields["title"] != "Handle 422 from the events feed" {
		t.Errorf("merged fields = %v", merged.Fields)
	}
	if want := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC); !merged.Time.Equal(want) {
		t.Errorf("closed item stamped %s, want closed_at %s", merged.Time, want)
	}
	open := find(t, points, "gh_external_contribution", map[string]string{"kind": "issue", "state": "open", "number": "9"})
	if !open.Time.Equal(startOfDay(testNow)) || open.Tags["repo"] != "another/project" {
		t.Errorf("open item = %s %v", open.Time, open.Tags)
	}
	if hasField(open, "merged") {
		t.Error("an issue is never merged")
	}
}

func TestOutboundSearchForbiddenIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			_, _ = w.Write([]byte(`{"data":{"search":null},"errors":[{"type":"FORBIDDEN","message":"search is not available"}]}`))
		case strings.Contains(query, "repositoryDiscussionComments"):
			f.write(w, "viewer_discussion_comments.json")
		default:
			f.write(w, "viewer_issue_comments.json")
		}
	})
	points, err := Outbound{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a refused search must not fail the stars: %v", err)
	}
	if len(only(t, points, "gh_star_given")) != 3 || len(byMeasurement(points)["gh_external_contribution"]) != 0 {
		t.Errorf("got %v", measurements(points))
	}
}

// A backfill follows the starred cursor as far as the walk allows; a sweep
// stops at its page cap. Either way the last page is the one that says there
// is no next.
func TestOutboundStarredFollowsTheCursor(t *testing.T) {
	t.Parallel()
	var starred []map[string]any
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			starred = append(starred, vars)
			if vars["after"] == nil {
				// The first page says there is another.
				b := strings.Replace(string(fixture(t, "graphql_starred.json")), `"hasNextPage": false`, `"hasNextPage": true`, 1)
				_, _ = w.Write([]byte(b))
				return
			}
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "repositoryDiscussionComments"):
			f.write(w, "viewer_discussion_comments.json")
		default:
			f.write(w, "viewer_issue_comments.json")
		}
	})
	points, err := Outbound{Login: "octocat", Walk: Unbounded}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(starred) != 2 || starred[1]["after"] != "Y3Vyc29yOjM=" {
		t.Errorf("starred queries = %v, want the second to carry the first page's cursor", starred)
	}
	if got := len(only(t, points, "gh_star_given")); got != 6 {
		t.Errorf("got %d stars from two pages of three", got)
	}
}

func TestPlanningRateLimitIsReported(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	})
	_, err := Planning{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMITED") {
		t.Fatalf("err = %v, want the rate limit reported", err)
	}
}

// A backfill bounded by backfill.since stops following the starred cursor
// once a page has gone past the bound, as every other newest-first walk does.
func TestOutboundStarredStopsAtTheBackfillBound(t *testing.T) {
	t.Parallel()
	var starred []map[string]any
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			starred = append(starred, vars)
			// Every page says there is another; the bound is what stops it.
			b := strings.Replace(string(fixture(t, "graphql_starred.json")), `"hasNextPage": false`, `"hasNextPage": true`, 1)
			_, _ = w.Write([]byte(b))
		case strings.Contains(query, "search(type: ISSUE"):
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "repositoryDiscussionComments"):
			f.write(w, "viewer_discussion_comments.json")
		default:
			f.write(w, "viewer_issue_comments.json")
		}
	})
	// The fixture's oldest star was given on 2026-08-12; a bound after it
	// ends the walk at the first page.
	since := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	points, err := Outbound{Login: "octocat", Walk: Walk{Pages: -1, Since: since}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(starred) != 1 {
		t.Errorf("%d starred queries, want the walk to stop at the page that passed the bound", len(starred))
	}
	if got := len(only(t, points, "gh_star_given")); got != 3 {
		t.Errorf("got %d stars, want the 3 of the page read", got)
	}
}

// TestOutboundCommentsAreReadFromTheNewestEnd pins the direction of the two
// viewer comment walks. Both connections list oldest first and the
// discussion one takes no orderBy (measured on 2026-09-12: first: 3 of the
// 706 issue comments answered the three of 2020), so a sweep that reads one
// page from the front never sees a comment left this morning. The sweep asks
// for the last hundred; a backfill walks back with before from the page's
// startCursor, and stops once the oldest comment of a page is past Since.
func TestOutboundCommentsAreReadFromTheNewestEnd(t *testing.T) {
	t.Parallel()
	var discussion, issue []map[string]any
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "repositoryDiscussionComments"):
			discussion = append(discussion, vars)
			if vars["before"] == nil {
				// The newest page says there is an older one.
				b := strings.Replace(string(fixture(t, "viewer_discussion_comments.json")), `"hasPreviousPage": false`, `"hasPreviousPage": true`, 1)
				_, _ = w.Write([]byte(b))
				return
			}
			f.write(w, "viewer_discussion_comments.json")
		default:
			issue = append(issue, vars)
			f.write(w, "viewer_issue_comments.json")
		}
	})

	// A sweep: one page, the newest.
	if _, err := (Outbound{Login: "octocat"}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	for name, seen := range map[string][]map[string]any{"discussion": discussion, "issue": issue} {
		if len(seen) != 1 || seen[0]["last"] != float64(100) || seen[0]["first"] != nil || seen[0]["before"] != nil {
			t.Errorf("a sweep's %s comments query = %v, want last: 100 and no cursor", name, seen)
		}
	}

	// A backfill: back from the newest page, one cursor at a time, and the
	// page is oldest first so its first node is what Since is held against.
	discussion, issue = nil, nil
	if _, err := (Outbound{Login: "octocat", Walk: Unbounded}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(discussion) != 2 || discussion[1]["before"] != "Y3Vyc29yOjE=" {
		t.Errorf("backfill discussion queries = %v, want the second to carry the first page's startCursor", discussion)
	}
	discussion = nil
	since := Walk{Pages: -1, Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := (Outbound{Login: "octocat", Walk: since}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(discussion) != 1 {
		t.Errorf("a backfill since September asked %d discussion pages, want to stop at the page whose oldest comment is 2024", len(discussion))
	}
}
