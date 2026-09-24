package collect

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

func repoActivityRoutes(f *fixtureServer) {
	f.file("/repos/octocat/hello-world/stats/participation", "stats_participation.json")
	f.file("/repos/octocat/hello-world/stats/punch_card", "stats_punch_card.json")
	f.file("/repos/octocat/hello-world/actions/workflows", "workflows.json")
}

func TestRepoActivityWeeksAreAnchoredToSunday(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	repoActivityRoutes(f)

	tuesday := testNow                 // 2026-09-08
	friday := testNow.AddDate(0, 0, 3) // 2026-09-11, same week
	sunday := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)

	fromTuesday, err := RepoActivity{}.Collect(ctx(t), f.Client, testRepo, tuesday)
	if err != nil {
		t.Fatal(err)
	}
	fromFriday, err := RepoActivity{}.Collect(ctx(t), f.Client, testRepo, friday)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, fromTuesday)
	checkPoints(t, fromFriday)

	weeksTue := only(t, fromTuesday, "gh_commits_week")
	weeksFri := only(t, fromFriday, "gh_commits_week")
	if len(weeksTue) != 52 || len(weeksFri) != 52 {
		t.Fatalf("got %d and %d weekly rows, want 52", len(weeksTue), len(weeksFri))
	}
	// Two sweeps in the same week must land on the same rows, or every
	// re-read writes a second copy of the year.
	for i := range weeksTue {
		if !weeksTue[i].Time.Equal(weeksFri[i].Time) {
			t.Errorf("week %d: Tuesday sweep stamped %s, Friday sweep %s", i, weeksTue[i].Time, weeksFri[i].Time)
		}
		if weeksTue[i].Time.Weekday() != time.Sunday {
			t.Errorf("week %d stamped on a %s, want Sunday", i, weeksTue[i].Time.Weekday())
		}
	}
	last := weeksTue[51]
	if !last.Time.Equal(sunday) {
		t.Errorf("current week stamped %s, want the Sunday that starts it, %s", last.Time, sunday)
	}
	if first := weeksTue[0]; !first.Time.Equal(sunday.AddDate(0, 0, -51*7)) {
		t.Errorf("oldest week stamped %s", first.Time)
	}
	if fieldInt(t, last, "commits") != 2 || fieldInt(t, last, "owner_commits") != 2 {
		t.Errorf("current week fields = %v", last.Fields)
	}
	if fieldInt(t, weeksTue[2], "commits") != 4 || fieldInt(t, weeksTue[2], "owner_commits") != 3 {
		t.Errorf("third week fields = %v", weeksTue[2].Fields)
	}
}

func TestRepoActivityPunchCardAndWorkflows(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	repoActivityRoutes(f)
	points, err := RepoActivity{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	// Empty hours and the malformed row with weekday 7 are dropped.
	punch := only(t, points, "gh_commit_punchcard")
	if len(punch) != 5 {
		t.Errorf("got %d punch card cells, want 5 non-empty valid ones", len(punch))
	}
	mon := find(t, points, "gh_commit_punchcard", map[string]string{"weekday": "Mon", "hour": "09"})
	if fieldInt(t, mon, "commits") != 3 || !mon.Time.Equal(testNow) {
		t.Errorf("Mon 09 = %v at %s", mon.Fields, mon.Time)
	}
	for _, p := range punch {
		if len(p.Tags["hour"]) != 2 {
			t.Errorf("hour %q is not two digits, which breaks sorting", p.Tags["hour"])
		}
	}

	wf := only(t, points, "gh_workflow")
	if len(wf) != 2 {
		t.Fatalf("got %d workflows, want 2", len(wf))
	}
	ci := find(t, points, "gh_workflow", map[string]string{"workflow": "CI"})
	if ci.Fields["active"] != true || ci.Tags["path"] != ".github/workflows/ci.yml" || ci.Tags["state"] != "active" {
		t.Errorf("CI workflow = %v %v", ci.Tags, ci.Fields)
	}
	nightly := find(t, points, "gh_workflow", map[string]string{"workflow": "Nightly"})
	if nightly.Fields["active"] != false || nightly.Tags["state"] != "disabled_manually" {
		t.Errorf("Nightly workflow = %v %v", nightly.Tags, nightly.Fields)
	}
}

// TestRepoActivityWorkflowDates reads the two dates the listing has always
// carried and nothing ever emitted, so "did the definition change on the day
// the duration jumped" has an answer.
//
// The fixture keeps the local offsets GitHub actually sends for these two
// fields, which are not the Z the rest of the API uses.
func TestRepoActivityWorkflowDates(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/stats/participation", "stats_participation.json")
	f.file("/repos/octocat/hello-world/stats/punch_card", "stats_punch_card.json")
	f.file("/repos/octocat/hello-world/actions/workflows", "workflows_local_time.json")
	points, err := RepoActivity{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	ci := find(t, points, "gh_workflow", map[string]string{"workflow": "CI"})
	// 2025-12-20T22:02:51+01:00 is 2025-12-20T21:02:51Z, 261 whole days before
	// testNow; the last edit, 2026-01-29T20:21:56+01:00, is 221. The offset is
	// read rather than ignored: taking the wall clock for UTC would move both
	// by an hour, which is a day at the wrong boundary.
	if got := fieldInt(t, ci, "age_days"); got != 261 {
		t.Errorf("age_days = %d, want 261", got)
	}
	if got := fieldInt(t, ci, "days_since_change"); got != 221 {
		t.Errorf("days_since_change = %d, want 221", got)
	}
	// A workflow never edited since it was added reads the same on both.
	mirror := find(t, points, "gh_workflow", map[string]string{"workflow": "GitLab Mirror"})
	if fieldInt(t, mirror, "age_days") != fieldInt(t, mirror, "days_since_change") {
		t.Errorf("untouched workflow = %v", mirror.Fields)
	}
}

func TestRepoActivityNotReadyIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// The stats endpoints answer 202 with an empty body while GitHub computes.
	f.handle("/repos/octocat/hello-world/stats/participation", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	f.handle("/repos/octocat/hello-world/stats/punch_card", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	f.file("/repos/octocat/hello-world/actions/workflows", "workflows.json")
	points, err := RepoActivity{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("202 must be skipped: %v", err)
	}
	if got := measurements(points); len(got) != 1 || got[0] != "gh_workflow" {
		t.Errorf("got %v, want only gh_workflow", got)
	}
}

// TestRepoActivityAFailureKeepsTheReadsBeforeIt pins what a real failure on
// the second read hands back: the weeks already read, and no request after it.
func TestRepoActivityAFailureKeepsTheReadsBeforeIt(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	repoActivityRoutes(f)
	f.status("/repos/octocat/hello-world/stats/punch_card", 500, "boom")
	points, err := RepoActivity{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil {
		t.Fatal("a 500 on the punch card must be returned")
	}
	if got := measurements(points); len(got) != 1 || got[0] != "gh_commits_week" {
		t.Errorf("got %v, want only the weeks read before the failure", got)
	}
	if calls := f.calls("/repos/octocat/hello-world/actions/workflows"); len(calls) != 0 {
		t.Errorf("workflows read %d times after the failure", len(calls))
	}
}

func TestDiscussions(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotVars map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		gotVars = vars
		if !strings.Contains(query, "discussions(") {
			t.Errorf("query does not ask for discussions:\n%s", query)
		}
		f.write(w, "graphql_discussions.json")
	})
	points, err := Discussions{First: 25}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if gotVars["owner"] != "octocat" || gotVars["name"] != "hello-world" || gotVars["first"] != float64(25) {
		t.Errorf("variables = %v", gotVars)
	}
	discussions := only(t, points, "gh_discussion")
	if len(discussions) != 2 {
		t.Fatalf("got %d discussions, want 2", len(discussions))
	}
	answered := find(t, points, "gh_discussion", map[string]string{"category": "Q&A"})
	if answered.Tags["author"] != "erin" || answered.Fields["has_answer"] != true {
		t.Errorf("answered discussion = %v %v", answered.Tags, answered.Fields)
	}
	// The answer comes after the date the row carries, so as a tag it
	// opened a second series at the same instant the day it was chosen.
	if _, isTag := answered.Tags["answered"]; isTag {
		t.Error("answered must be a field, not a tag")
	}
	if hasField(answered, "answered") {
		t.Error("the demoted flag must not reuse the old tag's column name")
	}
	if fieldInt(t, answered, "seconds_to_answer") != int64(4.5*3600) || fieldInt(t, answered, "upvotes") != 3 {
		t.Errorf("answered fields = %v", answered.Fields)
	}
	if want := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC); !answered.Time.Equal(want) {
		t.Errorf("discussion stamped %s, want createdAt %s", answered.Time, want)
	}
	idea := find(t, points, "gh_discussion", map[string]string{"category": "Ideas"})
	if idea.Tags["author"] != "(ghost)" || idea.Fields["has_answer"] != false || hasField(idea, "seconds_to_answer") {
		t.Errorf("idea = %v %v", idea.Tags, idea.Fields)
	}
}

// TestDiscussionsHonourTheCommentAndReplyPages checks the two knobs that,
// with First, are the whole cost of the query: measured on 2026-09-11 the
// 50/20/20 shape costs eleven points on a repository with no discussions and
// the 10/10/10 one costs one. Both have to be variables, because a literal
// in the query text is a page size the runner cannot choose per sweep.
func TestDiscussionsHonourTheCommentAndReplyPages(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotVars map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		gotVars = vars
		for _, want := range []string{"comments(first: $comments)", "replies(first: $replies)"} {
			if !strings.Contains(query, want) {
				t.Errorf("the query does not page by %s:\n%s", want, query)
			}
		}
		f.write(w, "graphql_discussions.json")
	})
	if _, err := (Discussions{First: 10, Comments: 7, Replies: 3}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if gotVars["first"] != float64(10) || gotVars["comments"] != float64(7) || gotVars["replies"] != float64(3) {
		t.Errorf("variables = %v, want first 10, comments 7, replies 3", gotVars)
	}
	// The zero value keeps the shape the backfill and the probe rely on.
	if _, err := (Discussions{}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if gotVars["first"] != float64(50) || gotVars["comments"] != float64(20) || gotVars["replies"] != float64(20) {
		t.Errorf("default variables = %v, want 50, 20 and 20", gotVars)
	}
}

// TestDiscussionsCommentsCarryTheSameContextAsTheAccountWalk is the check that
// gh_discussion_comment has one shape.
//
// Two collectors write that measurement: this one, over the account's own
// repositories, and Outbound's walk over everything the account wrote
// anywhere. Extending one query and not the other would publish the thread's
// context on half the rows and zero-valued fields on the rest.
func TestDiscussionsCommentsCarryTheSameContextAsTheAccountWalk(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		for _, field := range []string{"answerChosenBy", "answer {", "closed", "stateReason", "isAnswerable"} {
			if !strings.Contains(query, field) {
				t.Errorf("query does not ask for %s:\n%s", field, query)
			}
		}
		f.write(w, "graphql_discussions_answered.json")
	})
	points, err := Discussions{Login: "octocat"}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	comments := only(t, points, "gh_discussion_comment")
	if len(comments) != 3 {
		t.Fatalf("got %d comments, want two comments and one reply", len(comments))
	}
	checkEveryCommentCarriesTheThread(t, comments)
	// The account's own comment lost the thread: it is not the answer, and
	// only discussion_answered tells that apart from nobody answering.
	mine := find(t, points, "gh_discussion_comment", map[string]string{"comment": "9724102"})
	if mine.Tags["is_answer"] != "false" || mine.Tags["author"] != "octocat" || mine.Tags["own"] != "true" {
		t.Errorf("own comment = %v", mine.Tags)
	}
	reply := find(t, points, "gh_discussion_comment", map[string]string{"comment": "9724110"})
	if reply.Tags["is_reply"] != "true" || fieldInt(t, reply, "reply_to") != 9724102 {
		t.Errorf("reply = %v %v", reply.Tags, reply.Fields)
	}

	// The thread's own row keeps what closing says, which answering does not.
	thread := only(t, points, "gh_discussion")[0]
	if thread.Fields["closed"] != true || thread.Fields["state_reason"] != "RESOLVED" {
		t.Errorf("discussion = %v", thread.Fields)
	}
	if fieldInt(t, thread, "seconds_to_close") != 3*86400 || fieldInt(t, thread, "seconds_to_answer") != 2*86400 {
		t.Errorf("discussion = %v", thread.Fields)
	}
}

// checkEveryCommentCarriesTheThread reads the thread's own state off every row
// of the answered fixture, the answer and the reply included.
func checkEveryCommentCarriesTheThread(t *testing.T, comments []sink.Point) {
	t.Helper()
	for _, c := range comments {
		if c.Fields["discussion_answered"] != true || c.Fields["answered_by"] != "mvandervoord" {
			t.Errorf("comment %s = %v", c.Tags["comment"], c.Fields)
		}
		if c.Fields["answer_chosen_by"] != "lapo4719" || c.Fields["category"] != "Q&A" {
			t.Errorf("comment %s = %v", c.Tags["comment"], c.Fields)
		}
		if c.Fields["discussion_answerable"] != true {
			t.Errorf("comment %s sits in a Q&A category: %v", c.Tags["comment"], c.Fields)
		}
		if c.Fields["discussion_closed"] != true || c.Fields["state_reason"] != "RESOLVED" {
			t.Errorf("comment %s = %v", c.Tags["comment"], c.Fields)
		}
		if fieldInt(t, c, "seconds_to_answer") != 2*86400 || fieldInt(t, c, "seconds_to_close") != 3*86400 {
			t.Errorf("comment %s = %v", c.Tags["comment"], c.Fields)
		}
	}
}

func TestDiscussionsSwitchedOffIsNotAnError(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":"FORBIDDEN","message":"Discussions are not enabled"}]}`))
	})
	points, err := Discussions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("err=%v points=%d, want nil and none", err, len(points))
	}
}

func TestDiscussionsWalkByCursorDuringABackfill(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		calls = append(calls, vars)
		var page map[string]any
		mustUnmarshal(t, fixture(t, "graphql_discussions.json"), &page)
		conn := page["data"].(map[string]any)["repository"].(map[string]any)["discussions"].(map[string]any)
		if vars["after"] == nil {
			conn["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "disc-cursor-1"}
		} else {
			conn["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": "disc-cursor-2"}
		}
		_, _ = w.Write(mustMarshal(t, page))
	})
	points, err := Discussions{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(calls) != 2 || calls[1]["after"] != "disc-cursor-1" {
		t.Errorf("calls = %v, want a second query carrying the cursor", calls)
	}
	if len(points) != 4 {
		t.Errorf("got %d discussions over two pages, want 4", len(points))
	}
}

func TestDiscussionsRateLimitIsReported(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	})
	_, err := Discussions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMITED") {
		t.Fatalf("err = %v, want the rate limit reported", err)
	}
}
