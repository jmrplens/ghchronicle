package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
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
	if len(points) != 3 {
		t.Fatalf("got %d forks, want 3", len(points))
	}
	alice := find(t, points, "gh_fork", map[string]string{"by": "alice"})
	if want := time.Date(2025, 2, 10, 10, 0, 0, 0, time.UTC); !alice.Time.Equal(want) {
		t.Errorf("fork stamped %s, want created_at %s", alice.Time, want)
	}
	// Pushed ten days after forking: a real derivative. The ten days are
	// counted from the fork's own date, not from the sweep's clock, so the
	// row says the same thing however long after it is read.
	if alice.Fields["advanced"] != true || fieldInt(t, alice, "stars") != 2 || fieldInt(t, alice, "forks") != 1 {
		t.Errorf("alice's fork = %v", alice.Fields)
	}
	if fieldInt(t, alice, "seconds_to_push") != 10*86400 {
		t.Errorf("seconds_to_push = %v, want the ten days between the fork and its last push", alice.Fields["seconds_to_push"])
	}
	// Pushed thirty seconds after forking: the fork itself, a bookmark.
	bob := find(t, points, "gh_fork", map[string]string{"by": "bob"})
	if bob.Fields["advanced"] != false {
		t.Errorf("bob's fork = %v", bob.Fields)
	}
	if fieldInt(t, bob, "seconds_to_push") != 30 {
		t.Errorf("seconds_to_push = %v, want the thirty seconds the fork itself took", bob.Fields["seconds_to_push"])
	}
	// A fork nobody has pushed to inherits the parent's own last push, which
	// is usually older than the fork: measured live on 2026-09-17, 19 of the
	// 28 forks of jmrplens/TFG-TFM_EPS and 27 of 34 of jmrplens/phonometry.
	// So a negative gap is the common case, not an edge, and what it says is
	// that this fork has never been pushed to at all.
	carol := find(t, points, "gh_fork", map[string]string{"by": "carol"})
	if carol.Fields["advanced"] != false || fieldInt(t, carol, "seconds_to_push") != -2*86400 {
		t.Errorf("carol's fork = %v, want the two days GitHub reports its push before the fork", carol.Fields)
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
			// The fixture answers every field whether or not it was asked
			// for, so what the query selects is checked here or nowhere.
			wantSelected(t, query, "additions", "deletions", "changedFiles",
				"stargazerCount", "forkCount", "isPrivate", "url primaryLanguage { name }")
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "repositoryDiscussionComments"):
			// gh_discussion_comment is written by two collectors from two
			// queries. The other one is checked the same way in
			// repoactivity_test: a selection added to one and not the other
			// gives the measurement two shapes, and half its rows would
			// answer "was this thread ever answered" with a zero value.
			// isPrivate is the exception: the other one has it from the
			// listing that found the repository, and asks for nothing.
			wantSelected(t, query, "answerChosenBy", "answer {", "closed", "stateReason",
				"isAnswerable", "repository { nameWithOwner isPrivate }")
			if strings.Contains(query, "onlyAnswers: true") {
				f.write(w, "viewer_discussion_answers.json")
				return
			}
			f.write(w, "viewer_discussion_comments.json")
		default:
			wantSelected(t, query, "repository { nameWithOwner isPrivate }")
			f.write(w, "viewer_issue_comments.json")
		}
	})
	return f
}

// wantSelected fails for every selection the query does not make.
func wantSelected(t *testing.T, query string, selections ...string) {
	t.Helper()
	for _, sel := range selections {
		if !strings.Contains(query, sel) {
			t.Errorf("query does not ask for %s:\n%s", sel, query)
		}
	}
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
		"gh_upstream_repo", "gh_discussion_comment", "gh_issue_comment")

	checkOutboundComments(t, points)
	checkStarsGiven(t, points, starred)
	checkSearchesAreScoped(t, searches)
	checkExternalContributions(t, points)
	checkUpstreamRepositories(t, points)
	// The rows are the ones the REST paths wrote for the same three stars and
	// two items, recorded before the move to GraphQL, with the size and the
	// visibility the REST search never read added to the contributions.
	checkGolden(t, "outbound", points, "gh_star_given", "gh_external_contribution")
}

// checkOutboundComments reads the comments: whose repository they were left
// in is the fact nothing else here sees, so it is a tag.
func checkOutboundComments(t *testing.T, points []sink.Point) {
	t.Helper()
	// A comment in someone else's repository is the fact nothing else here
	// sees, so which side of that line it falls on is a tag.
	answer := find(t, points, "gh_discussion_comment", map[string]string{"full_name": "fosrl/pangolin"})
	if answer.Tags["own"] != "false" || answer.Tags["is_answer"] != "true" || fieldInt(t, answer, "answers") != 1 {
		t.Errorf("accepted answer elsewhere = %v %v", answer.Tags, answer.Fields)
	}
	if want := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC); !answer.Time.Equal(want) {
		t.Errorf("comment stamped %s, want createdAt %s", answer.Time, want)
	}
	mine := find(t, points, "gh_discussion_comment", map[string]string{"full_name": "octocat/hello-world"})
	if mine.Tags["own"] != "true" || fieldInt(t, mine, "answers") != 0 {
		t.Errorf("own repository comment = %v %v", mine.Tags, mine.Fields)
	}
	checkDiscussionContext(t, points)
	comment := find(t, points, "gh_issue_comment", map[string]string{"full_name": "torvalds/linux"})
	if comment.Tags["own"] != "false" || comment.Tags["number"] != "42" {
		t.Errorf("issue comment = %v", comment.Tags)
	}
	checkCommentsSayWhetherPrivate(t, points)
}

// checkCommentsSayWhetherPrivate reads the repository's visibility off every
// comment row. `own` does not answer it: the account's own repositories are
// private and public alike, and an organisation's private repository is not
// the account's own at all.
func checkCommentsSayWhetherPrivate(t *testing.T, points []sink.Point) {
	t.Helper()
	for _, m := range []string{"gh_issue_comment", "gh_discussion_comment"} {
		for _, p := range only(t, points, m) {
			want := p.Tags["full_name"] == "octocat/hello-world"
			if got, ok := p.Fields["private"].(bool); !ok || got != want {
				t.Errorf("%s in %s: private = %v, want %v", m, p.Tags["full_name"], p.Fields["private"], want)
			}
		}
	}
	// The accepted answer only the second walk reads carries it as well.
	late := find(t, points, "gh_discussion_comment", map[string]string{"comment": "7654321"})
	if late.Fields["private"] != false {
		t.Errorf("an answer from the answers walk = %v", late.Fields)
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
	won := find(t, points, "gh_discussion_comment", map[string]string{"full_name": "fosrl/pangolin"})
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
	lost := find(t, points, "gh_discussion_comment", map[string]string{"full_name": "ThrowTheSwitch/Ceedling"})
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
	open := find(t, points, "gh_discussion_comment", map[string]string{"full_name": "octocat/hello-world"})
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
	prose := find(t, points, "gh_star_given", map[string]string{"full_name": "sindresorhus/awesome"})
	if prose.Tags["language"] != noneTag {
		t.Errorf("a repository with no language must still carry the tag, got %q", prose.Tags["language"])
	}
	goStar := find(t, points, "gh_star_given", map[string]string{"full_name": "golang/go"})
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
	// only way search will separate merged from closed. The order is named
	// in every one: a closed state by when it last moved, so what closed
	// since the last sweep is on its first page however old it is, and an
	// open one by when it was opened, which is the order a walk to the end
	// cannot lose an item to.
	for _, want := range []string{
		"is:pr is:merged author:octocat -user:octocat sort:updated-desc",
		"is:pr is:open author:octocat -user:octocat sort:created-desc",
		"is:pr is:closed is:unmerged author:octocat -user:octocat sort:updated-desc",
		"is:issue is:open author:octocat -user:octocat sort:created-desc",
		"is:issue is:closed author:octocat -user:octocat sort:updated-desc",
	} {
		if !slices.Contains(searches, want) {
			t.Errorf("no search %q among %q", want, searches)
		}
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
	if merged.Tags["full_name"] != "someone/else" || merged.Tags["user"] != "octocat" {
		t.Errorf("merged tags = %v", merged.Tags)
	}
	if fieldInt(t, merged, "merged") != 1 || fieldInt(t, merged, "seconds_to_merge") != 4*86400 || merged.Fields["title"] != "Handle 422 from the events feed" {
		t.Errorf("merged fields = %v", merged.Fields)
	}
	if want := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC); !merged.Time.Equal(want) {
		t.Errorf("closed item stamped %s, want closed_at %s", merged.Time, want)
	}
	open := find(t, points, "gh_external_contribution", map[string]string{"kind": "issue", "state": "open", "number": "9"})
	if !open.Time.Equal(startOfDay(testNow)) || open.Tags["full_name"] != "another/project" {
		t.Errorf("open item = %s %v", open.Time, open.Tags)
	}
	if hasField(open, "merged") {
		t.Error("an issue is never merged")
	}
	checkContributionSizeAndVisibility(t, merged, open)
}

// checkContributionSizeAndVisibility reads the two facts about an item that do
// not move once it is closed: how large the change was, and whether the
// repository it went to is private, which is what lets a consumer leave
// private work out of anything it shows to others.
func checkContributionSizeAndVisibility(t *testing.T, merged, open sink.Point) {
	t.Helper()
	if fieldInt(t, merged, "additions") != 167 || fieldInt(t, merged, "deletions") != 12 ||
		fieldInt(t, merged, "changed_files") != 6 {
		t.Errorf("a pull request carries its size: %v", merged.Fields)
	}
	if merged.Fields["private"] != false || open.Fields["private"] != true {
		t.Errorf("private = %v and %v, want false on someone/else and true on another/project",
			merged.Fields["private"], open.Fields["private"])
	}
	for _, name := range []string{"additions", "deletions", "changed_files"} {
		if hasField(open, name) {
			t.Errorf("an issue has no diff, and carries %s: %v", name, open.Fields)
		}
	}
	// The star count moves every day, and this row is dated when the item
	// closed: on it, every move would rewrite a partition of the past.
	for _, p := range []sink.Point{merged, open} {
		for _, name := range []string{"stars", "repo_stars", "forks", "language"} {
			if hasField(p, name) {
				t.Errorf("gh_external_contribution carries %s, a current state of the repository: %v", name, p.Fields)
			}
		}
	}
}

// checkUpstreamRepositories reads the repositories the searches reached, one
// row each however many items and searches named it, stamped at the sweep
// because every number on it is the repository as it stands now.
func checkUpstreamRepositories(t *testing.T, points []sink.Point) {
	t.Helper()
	upstream := only(t, points, "gh_upstream_repo")
	if len(upstream) != 2 {
		t.Fatalf("got %d upstream repositories from 10 items in 5 searches, want the 2 distinct ones", len(upstream))
	}
	for _, p := range upstream {
		if !p.Time.Equal(testNow) {
			t.Errorf("%s stamped %s, want the sweep %s", p.Tags["full_name"], p.Time, testNow)
		}
		if len(p.Tags) != 3 {
			t.Errorf("%s tags = %v, want the repository's three and nothing else", p.Tags["full_name"], p.Tags)
		}
	}
	upstreamGo := find(t, points, "gh_upstream_repo", map[string]string{
		"full_name": "someone/else", "owner": "someone", "repo": "else",
	})
	if fieldInt(t, upstreamGo, "stars") != 5152 || fieldInt(t, upstreamGo, "forks") != 555 ||
		upstreamGo.Fields["private"] != false || upstreamGo.Fields["language"] != "Go" ||
		upstreamGo.Fields["url"] != "https://github.com/someone/else" {
		t.Errorf("someone/else = %v", upstreamGo.Fields)
	}
	// GitHub answers primaryLanguage null for a repository it detects no
	// language in, and a field says nothing rather than an empty string.
	private := find(t, points, "gh_upstream_repo", map[string]string{"full_name": "another/project"})
	if private.Fields["private"] != true || fieldInt(t, private, "stars") != 21 || hasField(private, "language") {
		t.Errorf("another/project = %v", private.Fields)
	}
}

// A search that fails after others answered still leaves the repositories
// those reached: their contributions are handed up with the error, and the
// row that says what each of those repositories is goes with them.
func TestOutboundKeepsTheUpstreamRepositoriesOfAFailedSweep(t *testing.T) {
	t.Parallel()
	searched := 0
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			searched++
			if searched > 1 {
				_, _ = w.Write([]byte(accountInternalError))
				return
			}
			f.write(w, "graphql_search_issues.json")
		default:
			t.Errorf("the comments were read after a search failed: %s", query)
		}
	})
	points, err := Outbound{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Fatal("a failed search was not reported")
	}
	if got := len(only(t, points, "gh_external_contribution")); got != 2 {
		t.Errorf("got %d contributions, want the first search's 2", got)
	}
	upstream := only(t, points, "gh_upstream_repo")
	if len(upstream) != 2 {
		t.Errorf("got %d upstream repositories, want the 2 the first search reached", len(upstream))
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
// The accepted answers are the same connection filtered, and are read from
// the same end.
func TestOutboundCommentsAreReadFromTheNewestEnd(t *testing.T) {
	t.Parallel()
	var discussion, issue, answers []map[string]any
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "onlyAnswers: true"):
			answers = append(answers, vars)
			f.write(w, "viewer_discussion_answers.json")
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
	for name, seen := range map[string][]map[string]any{"discussion": discussion, "issue": issue, "answer": answers} {
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

// answersFixture serves the newest page of comments as one with older pages
// behind it, which a sweep does not read, and the accepted answers through
// answer, recording the text of both queries.
func answersFixture(t *testing.T, answer func(w http.ResponseWriter, vars map[string]any)) (f *fixtureServer, newest, answers *string) {
	t.Helper()
	newest, answers = new(string), new(string)
	f = newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "starredRepositories(first:"):
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "search(type: ISSUE"):
			f.write(w, "graphql_search_issues.json")
		case strings.Contains(query, "onlyAnswers: true"):
			*answers = query
			answer(w, vars)
		case strings.Contains(query, "repositoryDiscussionComments"):
			*newest = query
			b := strings.Replace(string(fixture(t, "viewer_discussion_comments.json")), `"hasPreviousPage": false`, `"hasPreviousPage": true`, 1)
			_, _ = w.Write([]byte(b))
		default:
			f.write(w, "viewer_issue_comments.json")
		}
	})
	return f, newest, answers
}

// The newest hundred is a window measured in comments, and an answer can be
// accepted after its comment has left it: measured on 2026-09-26, one was
// accepted fourteen days after it was written, and the account's newest
// hundred of 108 comments no longer held one of its 15 accepted answers. A
// sweep reads the accepted answers on their own, so an answer older than the
// newest page is written with is_answer=true, dated when it was written like
// every other comment; and one both walks see is written once, since a
// second copy in the same batch would be counted twice by the exporter and
// averaged twice into its upvotes.
func TestAnAnswerAcceptedPastTheNewestHundredIsRead(t *testing.T) {
	t.Parallel()
	f, newest, answers := answersFixture(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write(fixture(t, "viewer_discussion_answers.json"))
	})
	points, err := Outbound{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	late := find(t, points, "gh_discussion_comment", map[string]string{"comment": "7654321"})
	if late.Tags["is_answer"] != "true" || late.Tags["own"] != "false" || late.Tags["full_name"] != "someone/else" ||
		late.Tags["number"] != "57" || fieldInt(t, late, "answers") != 1 {
		t.Errorf("the answer accepted past the newest hundred = %v %v", late.Tags, late.Fields)
	}
	if want := time.Date(2023, 11, 20, 16, 30, 0, 0, time.UTC); !late.Time.Equal(want) {
		t.Errorf("stamped %s, want when the comment was written, %s", late.Time, want)
	}
	if late.Fields["url"] != "https://github.com/someone/else/discussions/57#discussioncomment-7654321" ||
		late.Fields["answered_by"] != "octocat" || late.Fields["discussion_answered"] != true {
		t.Errorf("the thread's context did not come with it: %v", late.Fields)
	}

	comments := only(t, points, "gh_discussion_comment")
	both := 0
	for _, p := range comments {
		if p.Tags["comment"] == "18283966" {
			both++
		}
	}
	if both != 1 || len(comments) != 4 {
		t.Errorf("the answer both walks see was written %d times among %d comments, want once among 4", both, len(comments))
	}

	// One selection for both, which is what makes the answer both walks see
	// the same row: a field one of them forgot would be a zero on half of it.
	_, newestNodes, _ := strings.Cut(*newest, "pageInfo")
	_, answerNodes, _ := strings.Cut(*answers, "pageInfo")
	if newestNodes == "" || newestNodes != answerNodes {
		t.Errorf("the two walks ask for different comments:\n%s\n%s", *newest, *answers)
	}
}

// The accepted answers are read back from the newest, to the end or to five
// pages on a sweep, and a backfill's date bound does not stop them: a sweep
// reads its pages whatever their dates, and a backfill reads no less.
func TestTheAcceptedAnswersAreReadPastTheirFirstPage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		walk  Walk
		more  func(before any) bool
		pages int
	}{
		{"a sweep, to the end", Walk{}, func(before any) bool { return before == nil }, 2},
		{"a backfill since last week, to the end", Walk{Pages: -1, Since: testNow.AddDate(0, 0, -7)}, func(before any) bool { return before == nil }, 2},
		{"a sweep, to its bound", Walk{}, func(any) bool { return true }, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked []map[string]any
			f, _, _ := answersFixture(t, func(w http.ResponseWriter, vars map[string]any) {
				asked = append(asked, vars)
				b := string(fixture(t, "viewer_discussion_answers.json"))
				if tc.more(vars["before"]) {
					b = strings.Replace(b, `"hasPreviousPage": false`, `"hasPreviousPage": true`, 1)
				}
				_, _ = w.Write([]byte(b))
			})
			if _, err := (Outbound{Login: "octocat", Walk: tc.walk}).Collect(ctx(t), f.Client, testNow); err != nil {
				t.Fatal(err)
			}
			if len(asked) != tc.pages {
				t.Fatalf("asked %d pages of accepted answers, want %d", len(asked), tc.pages)
			}
			if asked[0]["last"] != float64(100) || asked[0]["before"] != nil || asked[1]["before"] != "Y3Vyc29yOnYyOpHOAHTMsQ==" {
				t.Errorf("answer queries = %v, want the newest hundred and then back from its startCursor", asked)
			}
		})
	}
}

// A walk of the accepted answers that fails is the family's failure, and the
// newest hundred read before it are still rows the store does not have.
func TestAFailedAnswerWalkKeepsTheNewestComments(t *testing.T) {
	t.Parallel()
	f, _, _ := answersFixture(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte(accountInternalError))
	})
	points, err := Outbound{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err == nil || !strings.Contains(err.Error(), "INTERNAL") {
		t.Fatalf("err = %v, want the failed answer walk reported", err)
	}
	if got := len(byMeasurement(points)["gh_discussion_comment"]); got != 3 {
		t.Errorf("kept %d comments of the newest page, want its 3", got)
	}
}

// outboundStates is every outbound search by its qualifiers, with the state
// its rows are tagged with and whether it is read whole on every sweep.
var outboundStates = []struct {
	filter, kind, state string
	open                bool
}{
	{"is:pr is:merged", "pull_request", "merged", false},
	{"is:pr is:open", "pull_request", "open", true},
	{"is:pr is:closed is:unmerged", "pull_request", "closed", false},
	{"is:issue is:open", "issue", "open", true},
	{"is:issue is:closed", "issue", "closed", false},
}

// outboundPage is one page of an outbound search as GraphQL answers it.
func outboundPage(t *testing.T, count int, next bool, cursor string, nodes ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"data": map[string]any{"search": map[string]any{
		"issueCount": count,
		"pageInfo":   map[string]any{"hasNextPage": next, "endCursor": cursor},
		"nodes":      nodes,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// searchNode is one item of a search page, last updated at updated, and
// closed then unless the search is for an open state.
func searchNode(number int, updated string, open bool) map[string]any {
	n := map[string]any{
		"number": number, "title": fmt.Sprintf("Item %d", number),
		"url":       fmt.Sprintf("https://github.com/someone/else/pull/%d", number),
		"createdAt": "2026-01-05T10:00:00Z", "updatedAt": updated, "closedAt": nil,
		"comments":   map[string]any{"totalCount": 0},
		"repository": map[string]any{"nameWithOwner": "someone/else"},
	}
	if !open {
		n["closedAt"] = updated
	}
	return n
}

// pageOneCursor is the endCursor of the first page twoPageSearches serves,
// spelled the way GitHub spells it: an offset, "cursor:2".
const pageOneCursor = "Y3Vyc29yOjI="

// twoPageSearches is a fake whose every outbound search has two pages: items
// 1 and 2, the second last updated at pageOneEnds, then item 3 of June. It
// records the variables each search was asked with, by its qualifiers.
func twoPageSearches(t *testing.T, pageOneEnds string) (*fixtureServer, map[string][]map[string]any) {
	t.Helper()
	return twoPageSearchesClosed(t, pageOneEnds, pageOneEnds)
}

// twoPageSearchesClosed is twoPageSearches with the item page one ends on
// closed at closed rather than when it last moved: one closed long ago and
// commented on since sits as high in the list as one closed yesterday.
func twoPageSearchesClosed(t *testing.T, pageOneEnds, closed string) (*fixtureServer, map[string][]map[string]any) {
	t.Helper()
	asked := map[string][]map[string]any{}
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "search(type: ISSUE"):
			q, _ := vars["query"].(string)
			filter, _, _ := strings.Cut(q, " author:")
			asked[filter] = append(asked[filter], vars)
			open := strings.Contains(filter, "is:open")
			if vars["after"] == nil {
				last := searchNode(2, pageOneEnds, open)
				if !open {
					last["closedAt"] = closed
				}
				_, _ = w.Write(outboundPage(t, 3, true, pageOneCursor,
					searchNode(1, "2026-09-05T10:00:00Z", open), last))
				return
			}
			_, _ = w.Write(outboundPage(t, 3, false, "Y3Vyc29yOjM=", searchNode(3, "2026-06-01T10:00:00Z", open)))
		case strings.Contains(query, "starredRepositories(first:"):
			f.write(w, "graphql_starred.json")
		case strings.Contains(query, "repositoryDiscussionComments"):
			f.write(w, "viewer_discussion_comments.json")
		default:
			f.write(w, "viewer_issue_comments.json")
		}
	})
	return f, asked
}

// pagesAsked checks how many pages of each search were asked for, and that
// every page after the first carried the cursor the one before it ended on.
func pagesAsked(t *testing.T, asked map[string][]map[string]any, want func(open bool) int) {
	t.Helper()
	for _, s := range outboundStates {
		seen := asked[s.filter]
		if len(seen) != want(s.open) {
			t.Errorf("%q was asked for %d pages, want %d", s.filter, len(seen), want(s.open))
			continue
		}
		if seen[0]["after"] != nil {
			t.Errorf("%q started from %v, want the first page", s.filter, seen[0]["after"])
		}
		if len(seen) > 1 && seen[1]["after"] != pageOneCursor {
			t.Errorf("%q asked its second page after %v, want the first page's cursor %s", s.filter, seen[1]["after"], pageOneCursor)
		}
	}
}

// A search is walked by its cursor, which is what issue #76 was about: every
// search used to ask for one page of a hundred and stop, so an account past a
// hundred items of a state lost the rest, silently. A backfill with no bound
// reads every state to the end, and the rows of every page carry the state
// of the search they came from.
func TestOutboundSearchFollowsTheCursor(t *testing.T) {
	t.Parallel()
	f, asked := twoPageSearches(t, "2026-08-01T10:00:00Z")
	var warned []string
	o := Outbound{Login: "octocat", Walk: Unbounded, Warn: func(msg string, _ ...any) { warned = append(warned, msg) }}
	points, err := o.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	pagesAsked(t, asked, func(bool) int { return 2 })
	for _, s := range outboundStates {
		for _, number := range []string{"1", "2", "3"} {
			find(t, points, "gh_external_contribution", map[string]string{"kind": s.kind, "state": s.state, "number": number})
		}
	}
	if got := len(only(t, points, "gh_external_contribution")); got != 15 {
		t.Errorf("got %d contributions, want 3 items from each of 5 searches", got)
	}
	// Three counted and three read: a walk that reached its count says
	// nothing.
	if len(warned) != 0 {
		t.Errorf("a walk that read everything it counts warned %q", warned)
	}
}

// A sweep with no Moved bound reads the first page of a closed state, and
// every page of an open state, whose rows are rewritten each day for every
// item still open.
func TestASweepReadsOnePageOfAClosedStateAndEveryPageOfAnOpenOne(t *testing.T) {
	t.Parallel()
	f, asked := twoPageSearches(t, "2026-08-01T10:00:00Z")
	points, err := Outbound{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	pagesAsked(t, asked, func(open bool) int {
		if open {
			return 2
		}
		return 1
	})
	for _, s := range outboundStates {
		want := 2
		if s.open {
			want = 3
		}
		got := 0
		for _, p := range only(t, points, "gh_external_contribution") {
			if p.Tags["kind"] == s.kind && p.Tags["state"] == s.state {
				got++
			}
		}
		if got != want {
			t.Errorf("%q wrote %d rows on a sweep, want %d", s.filter, got, want)
		}
	}
}

// A backfill bounded by backfill.since stops a closed state at the first page
// whose last item moved before the bound: closing an item moves it, so
// nothing further down the list closed inside the bound. An open state is not
// bounded by a date at all, since its row is today's.
func TestABackfillStopsAClosedSearchAtThePagePastSince(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	// Page one ends on the first of August, before the bound.
	f, asked := twoPageSearches(t, "2026-08-01T10:00:00Z")
	if _, err := (Outbound{Login: "octocat", Walk: Walk{Pages: -1, Since: since}}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	pagesAsked(t, asked, func(open bool) int {
		if open {
			return 2
		}
		return 1
	})

	// Page one ends on the twentieth, inside it: the walk goes on.
	f, asked = twoPageSearches(t, "2026-08-20T10:00:00Z")
	if _, err := (Outbound{Login: "octocat", Walk: Walk{Pages: -1, Since: since}}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	pagesAsked(t, asked, func(bool) int { return 2 })

	// Page one ends on an item closed on the first of August, before the
	// bound, and commented on since, on the first of September. The list is
	// in the order items last moved, so what comes after it can have closed
	// inside the bound, and the walk goes on: stopped by when that item
	// closed, which #76 itself suggested, it would lose them.
	f, asked = twoPageSearchesClosed(t, "2026-09-01T10:00:00Z", "2026-08-01T10:00:00Z")
	if _, err := (Outbound{Login: "octocat", Walk: Walk{Pages: -1, Since: since}}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	pagesAsked(t, asked, func(bool) int { return 2 })
}

// A sweep's closed states read back to Moved, which the runner sets a cadence
// before the sweep before, and not to a page count. A hundred items that moved
// after one closed, a bot locking old threads, a relabel, a week the collector
// was down, carry it to the second page, and a sweep that read one page lost
// it for good. The bound is when an item last moved, as the backfill's is, so
// one closed before the bound and commented on after it does not stop the walk.
func TestASweepReadsAClosedStateBackToTheSweepBefore(t *testing.T) {
	t.Parallel()
	moved := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, ends, closed string
		closedPages        int
	}{
		{"page one ends after it", "2026-08-20T10:00:00Z", "2026-08-20T10:00:00Z", 2},
		{"page one ends before it", "2026-08-01T10:00:00Z", "2026-08-01T10:00:00Z", 1},
		{"page one ends on an item closed before it and moved after", "2026-09-01T10:00:00Z", "2026-08-01T10:00:00Z", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, asked := twoPageSearchesClosed(t, tc.ends, tc.closed)
			if _, err := (Outbound{Login: "octocat", Moved: moved}).Collect(ctx(t), f.Client, testNow); err != nil {
				t.Fatal(err)
			}
			pagesAsked(t, asked, func(open bool) int {
				if open {
					return 2
				}
				return tc.closedPages
			})
		})
	}
}

// GitHub serves a thousand results of any search and then says there is no
// next page, while the count still says how many there are: measured on
// 2026-09-26, 2,860 merged pull requests answered ten pages and stopped. A
// walk that runs out of pages before it runs out of count says so through
// Warn, since nothing fails and the rows past the cap are simply missing. A
// sweep that stops a closed state at its own page limit says nothing about
// it: there was a next page, and the sweep chose not to read it.
func TestAnOutboundSearchPastTheCapIsSaid(t *testing.T) {
	t.Parallel()
	type warning struct {
		msg  string
		args []any
	}
	for _, tc := range []struct {
		name string
		walk Walk
		next bool
		want []string
	}{
		{"out of pages on a backfill", Unbounded, false, []string{"merged", "open", "closed", "open", "closed"}},
		{"out of pages on a sweep", Walk{}, false, []string{"merged", "open", "closed", "open", "closed"}},
		// The open states still run out, being read to the end whatever
		// the walk; the closed ones stop at the sweep's one page.
		{"stopped at the sweep's page", Walk{}, true, []string{"open", "open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
				switch {
				case strings.Contains(query, "search(type: ISSUE"):
					q, _ := vars["query"].(string)
					open := strings.Contains(q, "is:open")
					// Only a closed state is told there is more, so what
					// stops it is the walk and not the answer.
					next := tc.next && !open
					_, _ = w.Write(outboundPage(t, 1500, next, pageOneCursor,
						searchNode(1, "2026-09-05T10:00:00Z", open), searchNode(2, "2026-09-01T10:00:00Z", open)))
				case strings.Contains(query, "starredRepositories(first:"):
					f.write(w, "graphql_starred.json")
				case strings.Contains(query, "repositoryDiscussionComments"):
					f.write(w, "viewer_discussion_comments.json")
				default:
					f.write(w, "viewer_issue_comments.json")
				}
			})
			var warned []warning
			o := Outbound{Login: "octocat", Walk: tc.walk, Warn: func(msg string, args ...any) {
				warned = append(warned, warning{msg, args})
			}}
			if _, err := o.Collect(ctx(t), f.Client, testNow); err != nil {
				t.Fatal(err)
			}
			var states []string
			for _, w := range warned {
				if !strings.Contains(w.msg, "a thousand at most") {
					t.Errorf("warned %q, want it to name GitHub's cap", w.msg)
				}
				if len(w.args) != 8 || w.args[0] != "kind" || w.args[2] != "state" ||
					w.args[4] != "count" || w.args[5] != 1500 || w.args[6] != "read" || w.args[7] != 2 {
					t.Errorf("warned with %v, want the kind, the state, the count and what was read", w.args)
					continue
				}
				states = append(states, w.args[3].(string))
			}
			if !slices.Equal(states, tc.want) {
				t.Errorf("warned for %q, want %q", states, tc.want)
			}
		})
	}
}

// A search that fails part way keeps the pages that answered: they are rows
// the store does not have, and the failure is still the family's.
func TestAnOutboundSearchThatFailsPartWayKeepsItsPages(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case strings.Contains(query, "search(type: ISSUE") && vars["after"] != nil:
			_, _ = w.Write([]byte(accountInternalError))
		case strings.Contains(query, "search(type: ISSUE"):
			_, _ = w.Write(outboundPage(t, 3, true, pageOneCursor,
				searchNode(1, "2026-09-05T10:00:00Z", false), searchNode(2, "2026-09-01T10:00:00Z", false)))
		default:
			f.write(w, "graphql_starred.json")
		}
	})
	points, err := Outbound{Login: "octocat", Walk: Unbounded}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Fatal("a search that failed on its second page was not reported")
	}
	merged := 0
	for _, p := range byMeasurement(points)["gh_external_contribution"] {
		if p.Tags["state"] == "merged" {
			merged++
		}
	}
	if merged != 2 {
		t.Errorf("kept %d rows of the merged search's first page, want both", merged)
	}
}
