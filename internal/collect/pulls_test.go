package collect

import (
	"maps"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

func TestPullsPerItemPoints(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotVars map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		gotVars = vars
		if !strings.Contains(query, "pullRequests(") || !strings.Contains(query, "issues(") {
			t.Errorf("query does not ask for pull requests and issues:\n%s", query)
		}
		f.write(w, "graphql_pulls.json")
	})

	points, err := Pulls{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_pull_request", "gh_pull_request_review", "gh_issue", "gh_review_thread")
	if gotVars["owner"] != "octocat" || gotVars["name"] != "hello-world" || gotVars["first"] != float64(50) {
		t.Errorf("variables sent = %v, want owner, name and the default first of 50", gotVars)
	}
	// Ten, not fifty: measured against the live API, fifty threads costs 28
	// points and 9.8 s against a gateway that gives up at ten seconds.
	if gotVars["threads"] != float64(10) {
		t.Errorf("threads = %v, want the default page of 10", gotVars["threads"])
	}

	checkMergedPullRequest(t, points)
	checkOpenPullRequest(t, points)
	checkReviewRows(t, points)
	checkIssueRows(t, points)
	checkMergedPullRequestExtras(t, points)
	checkOpenPullRequestExtras(t, points)
	checkReviewThreadRows(t, points)
	checkIssuePlanning(t, points)
}

// checkMergedPullRequestExtras reads the fields that only a closed pull
// request has: who merged it, the commit it produced and its place in a stack.
func checkMergedPullRequestExtras(t *testing.T, points []sink.Point) {
	t.Helper()
	merged := find(t, points, "gh_pull_request", map[string]string{"number": "42"})
	// The whole conversation, which is not the issue-style comment count.
	// Measured on the live API, PR 631 of jmrplens/gitlab-mcp-server:
	// comments 2, totalCommentsCount 10, reviewThreads 2.
	if fieldInt(t, merged, "total_comments") != 10 || fieldInt(t, merged, "comments") != 3 {
		t.Errorf("total_comments and comments must differ: %v", merged.Fields)
	}
	// The true total, not the two threads written below it, so a truncated
	// page is visible rather than silent.
	if fieldInt(t, merged, "review_threads") != 4 {
		t.Errorf("review_threads = %v, want the connection total of 4", merged.Fields["review_threads"])
	}
	if merged.Fields["base_ref"] != "main" || merged.Fields["head_ref"] != "feature/thing" {
		t.Errorf("branch names = %v", merged.Fields)
	}
	if _, isTag := merged.Tags["base_ref"]; isTag {
		t.Error("branch names are unbounded, so they must be fields")
	}
	if merged.Fields["merged_by"] != "alice" {
		t.Errorf("merged_by = %v", merged.Fields["merged_by"])
	}
	// The join to gh_commit.
	if merged.Fields["merge_commit"] != "7733804423c0229c27b667f862160c34688da811" {
		t.Errorf("merge_commit = %v", merged.Fields["merge_commit"])
	}
	// A stack of three is one delivery, not three.
	if fieldInt(t, merged, "stack") != 690 || fieldInt(t, merged, "stack_size") != 3 || fieldInt(t, merged, "stack_position") != 2 {
		t.Errorf("stack fields = %v", merged.Fields)
	}
	// Measured: a merged pull request still answers CONFLICTING and DIRTY,
	// so writing them into a row dated the day it merged would be a lie.
	if hasField(merged, "mergeable") || hasField(merged, "merge_state") {
		t.Errorf("a merged pull request must not carry current merge state: %v", merged.Fields)
	}
}

// checkOpenPullRequestExtras reads the mirror image: current merge state is
// written, the merge fields are not, and a pull request outside a stack
// carries no stack fields at all.
func checkOpenPullRequestExtras(t *testing.T, points []sink.Point) {
	t.Helper()
	open := find(t, points, "gh_pull_request", map[string]string{"number": "43"})
	if open.Fields["mergeable"] != "CONFLICTING" || open.Fields["merge_state"] != "DIRTY" {
		t.Errorf("an open pull request carries its current merge state: %v", open.Fields)
	}
	if hasField(open, "merged_by") || hasField(open, "merge_commit") {
		t.Errorf("nothing merged it, so neither field belongs: %v", open.Fields)
	}
	if hasField(open, "stack") || hasField(open, "stack_size") || hasField(open, "stack_position") {
		t.Errorf("this pull request is in no stack: %v", open.Fields)
	}
	if fieldInt(t, open, "review_requests") != 2 {
		t.Errorf("review_requests = %v, want 2 waiting reviewers", open.Fields["review_requests"])
	}
}

// checkReviewThreadRows reads the review threads, the measurement that did
// not exist before: the review debt, dated when it was raised.
func checkReviewThreadRows(t *testing.T, points []sink.Point) {
	t.Helper()
	threads := only(t, points, "gh_review_thread")
	// Five threads in the fixture, one of them with no comment and so no date
	// to carry.
	if len(threads) != 4 {
		t.Fatalf("got %d review threads, want 4 (the one with no comment is skipped)", len(threads))
	}
	// The collision measured on the live API: on the newest fifty pull
	// requests of jmrplens/gitlab-mcp-server, nine groups of threads agree on
	// every tag the audit named and on the second they were opened, the worst
	// of them five deep. The fixture carries such a pair, identical in every
	// tag and every field but the thread. Compared on the series key and the
	// timestamp, which is what InfluxDB keys a point by: comparing rendered
	// lines instead would pass on a difference in the fields, which is
	// exactly the difference that does not save the row.
	series := map[string]int{}
	for _, th := range threads {
		series[seriesKey(th)]++
	}
	if len(series) != len(threads) {
		t.Errorf("%d threads collapse into %d series: %v", len(threads), len(series), series)
	}
	if got := find(t, points, "gh_review_thread", map[string]string{"thread": "3979413673"}); got.Tags["number"] != "42" {
		t.Errorf("the second thread of the pair is missing: %v", got.Tags)
	}
	checkBotReviewThread(t, points)
	checkHumanReviewThread(t, points)

	// A deleted account leaves a thread with a null author.
	ghost := find(t, points, "gh_review_thread", map[string]string{"number": "43"})
	if ghost.Tags["author"] != "(ghost)" || ghost.Tags["bot"] != "false" {
		t.Errorf("a deleted thread author must become (ghost) and not a bot: %v", ghost.Tags)
	}
}

// checkBotReviewThread reads the bot thread, resolved and outdated. Every
// review thread on this account is opened by a review bot, which is why bot
// is a tag.
func checkBotReviewThread(t *testing.T, points []sink.Point) {
	t.Helper()
	bot := find(t, points, "gh_review_thread", map[string]string{"thread": "3979413666"})
	// GraphQL says `coderabbitai` with __typename Bot; the login is written
	// the way REST spells the same app, so one bot is one author everywhere.
	if bot.Tags["number"] != "42" || bot.Tags["bot"] != "true" || bot.Tags["author"] != "coderabbitai[bot]" {
		t.Errorf("bot thread tags = %v", bot.Tags)
	}
	// Resolution and outdatedness are current state on a row whose date never
	// moves, so they are fields. As tags, resolving a thread would open a
	// second series at the same timestamp and the unresolved row would sit
	// there for ever, which is the opposite of what dating the thread at its
	// first comment is for.
	for _, mutable := range []string{"resolved", "outdated"} {
		if _, isTag := bot.Tags[mutable]; isTag {
			t.Errorf("%q is mutable state and must be a field, or the row stops converging", mutable)
		}
	}
	if fieldInt(t, bot, "resolved") != 1 || fieldInt(t, bot, "outdated") != 1 {
		t.Errorf("bot thread fields = %v", bot.Fields)
	}
	// Dated at the first comment, not at the resolution: a later resolution
	// rewrites this row instead of adding one.
	if want := time.Date(2026, 8, 21, 7, 15, 0, 0, time.UTC); !bot.Time.Equal(want) {
		t.Errorf("thread stamped %s, want the first comment at %s", bot.Time, want)
	}
	// A tag here would be one series per file of every repository.
	if bot.Fields["path"] != "tools/build-all.sh" {
		t.Errorf("path = %v", bot.Fields["path"])
	}
	if _, isTag := bot.Tags["path"]; isTag {
		t.Error("path must be a field: as a tag it is one series per file of every repository")
	}
	if bot.Fields["subject_type"] != "LINE" || bot.Fields["resolved_by"] != "coderabbitai[bot]" {
		t.Errorf("bot thread fields = %v", bot.Fields)
	}
	if fieldInt(t, bot, "comments") != 3 {
		t.Errorf("comments = %v, want the whole thread", bot.Fields["comments"])
	}
}

// checkHumanReviewThread reads a thread a person opened, still open, so
// nobody resolved it.
func checkHumanReviewThread(t *testing.T, points []sink.Point) {
	t.Helper()
	human := find(t, points, "gh_review_thread", map[string]string{"author": "alice"})
	if human.Tags["bot"] != "false" {
		t.Errorf("human thread tags = %v", human.Tags)
	}
	if fieldInt(t, human, "resolved") != 0 || fieldInt(t, human, "outdated") != 0 {
		t.Errorf("human thread fields = %v", human.Fields)
	}
	if hasField(human, "resolved_by") {
		t.Errorf("an unresolved thread has nobody to name: %v", human.Fields)
	}
	if human.Fields["subject_type"] != "FILE" {
		t.Errorf("subject_type = %v", human.Fields["subject_type"])
	}
}

// seriesKey is what InfluxDB keys a point by: the measurement, the tag set
// and the timestamp. The fields are deliberately not in it, because two
// points that differ only in their fields are the same row, and the second
// one overwrites the first.
func seriesKey(p sink.Point) string {
	keys := make([]string, 0, len(p.Tags))
	for k := range p.Tags {
		keys = append(keys, k+"="+p.Tags[k])
	}
	sort.Strings(keys)
	return p.Measurement + "," + strings.Join(keys, ",") + " " + p.Time.String()
}

// checkIssuePlanning reads the four planning tags and the sub-issue fields.
func checkIssuePlanning(t *testing.T, points []sink.Point) {
	t.Helper()
	closed := find(t, points, "gh_issue", map[string]string{"number": "7"})
	if closed.Fields["resolution"] != "COMPLETED" {
		t.Errorf("resolution = %q: without it a closed issue and a dropped one are one row", closed.Fields["resolution"])
	}
	if fieldInt(t, closed, "parent_issue") != 365 || closed.Fields["assigned_to"] != "dave" || closed.Fields["milestone_title"] != "3.1.0" {
		t.Errorf("planning fields = %v", closed.Fields)
	}
	// Each of the four changes after the date the row carries, so as a tag
	// each opened a second series at the same instant the day it changed.
	for _, tag := range []string{"state_reason", "parent", "assignee", "milestone"} {
		if _, isTag := closed.Tags[tag]; isTag {
			t.Errorf("%s must be a field, not a tag", tag)
		}
		if hasField(closed, tag) {
			t.Errorf("the demoted %s must not reuse the old tag's column name", tag)
		}
	}
	// Asked of the issue and not of the pull request, because only the issue
	// sees a closure made by reference rather than by keyword.
	if fieldInt(t, closed, "pull_request") != 42 {
		t.Errorf("pull_request = %v, want the pull request that closed it", closed.Fields["pull_request"])
	}

	// The open one has none of the four, and every field is still written:
	// a column that exists only once some row wrote it cannot be named in a
	// query before then, and InfluxDB 3 fails the whole query when it is.
	openIssue := find(t, points, "gh_issue", map[string]string{"number": "8"})
	for _, field := range []string{"resolution", "assigned_to", "milestone_title"} {
		if openIssue.Fields[field] != "(none)" {
			t.Errorf("field %q = %q, want the fallback written on every point", field, openIssue.Fields[field])
		}
	}
	if fieldInt(t, openIssue, "parent_issue") != 0 {
		t.Errorf("parent_issue = %v, want 0 when the issue has no parent", openIssue.Fields["parent_issue"])
	}
	if fieldInt(t, openIssue, "sub_issues_total") != 100 || fieldInt(t, openIssue, "sub_issues_completed") != 94 {
		t.Errorf("sub-issue fields = %v", openIssue.Fields)
	}
	if fieldInt(t, openIssue, "pull_request") != 0 {
		t.Errorf("pull_request = %v, want 0 when no pull request closed it", openIssue.Fields["pull_request"])
	}
}

// checkMergedPullRequest reads the closed pull request, which is where every
// duration this measurement exists for is computable.
func checkMergedPullRequest(t *testing.T, points []sink.Point) {
	t.Helper()
	// The number is what makes this a per-item measurement, so it is a tag.
	merged := find(t, points, "gh_pull_request", map[string]string{"number": "42"})
	if merged.Tags["state"] != "MERGED" || merged.Tags["author"] != "alice" {
		t.Errorf("merged PR tags = %v", merged.Tags)
	}
	// Both move while the row's date stays, so as tags a draft marked ready
	// was two rows at the same start of day for ever.
	for _, tag := range []string{"draft", "review_decision"} {
		if _, isTag := merged.Tags[tag]; isTag {
			t.Errorf("%s must be a field, not a tag", tag)
		}
		if hasField(merged, tag) {
			t.Errorf("the demoted %s must not reuse the old tag's column name", tag)
		}
	}
	if merged.Fields["is_draft"] != false || merged.Fields["decision"] != "APPROVED" {
		t.Errorf("merged PR fields = %v", merged.Fields)
	}
	if _, isField := merged.Fields["number"]; isField {
		t.Error("number must be a tag, not a field")
	}
	// A closed pull request is dated when it closed.
	closedAt := time.Date(2026, 8, 22, 16, 0, 0, 0, time.UTC)
	if !merged.Time.Equal(closedAt) {
		t.Errorf("merged PR stamped %s, want closedAt %s", merged.Time, closedAt)
	}
	if fieldInt(t, merged, "seconds_to_merge") != 54*3600 {
		t.Errorf("seconds_to_merge = %v, want 54h", merged.Fields["seconds_to_merge"])
	}
	// sourcery-ai reviewed five seconds after the pull request opened, alice
	// answered it on her own pull request an hour later, bob twenty-three
	// hours later. The first field is the bot's five seconds, which is what
	// "time to first review" read on every pull request of this account;
	// the second is the wait for somebody else, and the author's own reply
	// is not that: on this account it was the earliest non-bot review on
	// every pull request that had one.
	if fieldInt(t, merged, "seconds_to_first_review") != 5 {
		t.Errorf("seconds_to_first_review = %v, want 5 s (the bot)", merged.Fields["seconds_to_first_review"])
	}
	if fieldInt(t, merged, "seconds_to_first_human_review") != 23*3600 {
		t.Errorf("seconds_to_first_human_review = %v, want 23h (bob, not alice's own reply at 1h)", merged.Fields["seconds_to_first_human_review"])
	}
	if fieldInt(t, merged, "churn") != 150 || fieldInt(t, merged, "reviews") != 4 || fieldInt(t, merged, "commits") != 5 {
		t.Errorf("merged PR fields = %v", merged.Fields)
	}
	checkPullRequestReaderFields(t, merged)
}

// checkPullRequestReaderFields reads what a reader knows the pull request by,
// as fields: a title is unbounded and nine labels on one pull request are one
// row.
func checkPullRequestReaderFields(t *testing.T, merged sink.Point) {
	t.Helper()
	if merged.Fields["title"] != "Add the widget" || merged.Fields["author_association"] != "OWNER" {
		t.Errorf("merged PR title/association = %v", merged.Fields)
	}
	if fieldInt(t, merged, "labels") != 2 || merged.Fields["label_names"] != "enhancement,breaking" {
		t.Errorf("merged PR labels = %v %v", merged.Fields["labels"], merged.Fields["label_names"])
	}
	for _, name := range []string{"title", "label_names", "author_association"} {
		if _, isTag := merged.Tags[name]; isTag {
			t.Errorf("%s must be a field, not a tag", name)
		}
	}
}

// TestFirstHumanReviewIsAbsentWhenOnlyBotsReviewed pins the honest answer for
// a pull request nobody has looked at yet: the bot's wait is recorded, the
// human one is not, rather than being zero or the bot's.
func TestFirstHumanReviewIsAbsentWhenOnlyBotsReviewed(t *testing.T) {
	t.Parallel()
	opened := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bot := opened.Add(5 * time.Second)
	pr := &pullNode{Number: 9, State: "OPEN", CreatedAt: opened, Author: &actor{Login: "carol", Typename: "User"}}
	pr.Reviews.TotalCount = 3
	pr.Reviews.Nodes = append(pr.Reviews.Nodes,
		struct {
			Author      *actor     `json:"author"`
			State       string     `json:"state"`
			SubmittedAt *time.Time `json:"submittedAt"`
			URL         string     `json:"url"`
		}{Author: &actor{Login: "carol", Typename: "User"}, State: "COMMENTED", SubmittedAt: &bot},
		struct {
			Author      *actor     `json:"author"`
			State       string     `json:"state"`
			SubmittedAt *time.Time `json:"submittedAt"`
			URL         string     `json:"url"`
		}{Author: &actor{Login: "sourcery-ai", Typename: "Bot"}, State: "COMMENTED", SubmittedAt: &bot},
		struct {
			Author      *actor     `json:"author"`
			State       string     `json:"state"`
			SubmittedAt *time.Time `json:"submittedAt"`
			URL         string     `json:"url"`
		}{Author: &actor{Login: "dependabot[bot]", Typename: "User"}, State: "COMMENTED", SubmittedAt: &bot},
	)
	if _, ok := firstHumanReview(pr); ok {
		t.Error("two bot reviews and the author's own reply must not produce a human wait")
	}
	points := Pulls{}.pullPoints([]pullNode{*pr}, map[string]string{"repo": "r"}, testNow)
	row := find(t, points, "gh_pull_request", map[string]string{"number": "9"})
	if hasField(row, "seconds_to_first_human_review") {
		t.Errorf("seconds_to_first_human_review must be absent, got %v", row.Fields["seconds_to_first_human_review"])
	}
	for _, rv := range only(t, points, "gh_pull_request_review") {
		self := rv.Tags["reviewer"] == "carol"
		if rv.Tags["bot"] != boolTag(!self) || rv.Tags["self"] != boolTag(self) {
			t.Errorf("reviewer %q must be tagged bot=%t self=%t, got %v", rv.Tags["reviewer"], !self, self, rv.Tags)
		}
	}
}

// checkOpenPullRequest reads the one still open. It has no close date, so it
// lands on the start of the UTC day and hourly sweeps rewrite one row instead
// of adding twenty-four.
func checkOpenPullRequest(t *testing.T, points []sink.Point) {
	t.Helper()
	open := find(t, points, "gh_pull_request", map[string]string{"number": "43"})
	if !open.Time.Equal(startOfDay(testNow)) {
		t.Errorf("open PR stamped %s, want start of day %s", open.Time, startOfDay(testNow))
	}
	if open.Tags["author"] != "(ghost)" {
		t.Errorf("a null author must become (ghost), got %q", open.Tags["author"])
	}
	if open.Fields["is_draft"] != true || open.Tags["state"] != "OPEN" {
		t.Errorf("open PR = %v %v", open.Tags, open.Fields)
	}
	// Null is what GitHub answers for a pull request nobody has reviewed and
	// none is required of: 555 of the 558 read on 2026-09-10. Written raw it
	// is no field at all, and a column no row has written cannot be named in
	// a query, so the fallback is written on every point.
	if open.Fields["decision"] != "(none)" {
		t.Errorf("a null review decision must become %q, got %q", "(none)", open.Fields["decision"])
	}
	if hasField(open, "seconds_to_merge") || !hasField(open, "seconds_open") {
		t.Errorf("open PR fields = %v", open.Fields)
	}
}

// checkReviewRows reads the reviews, which become facts of their own dated
// when they were submitted.
func checkReviewRows(t *testing.T, points []sink.Point) {
	t.Helper()
	// Reviews become their own facts, dated when submitted.
	reviews := only(t, points, "gh_pull_request_review")
	if len(reviews) != 4 {
		t.Fatalf("got %d review points, want 4", len(reviews))
	}
	bob := find(t, points, "gh_pull_request_review", map[string]string{"reviewer": "bob"})
	if bob.Tags["number"] != "42" || bob.Tags["author"] != "alice" || bob.Tags["bot"] != "false" || bob.Tags["self"] != "false" {
		t.Errorf("review tags = %v", bob.Tags)
	}
	// An approval can be dismissed afterwards and keeps its submittedAt, so
	// as a tag the state opened a second row at the review's own instant.
	if _, isTag := bob.Tags["state"]; isTag {
		t.Error("the review state must be a field, not a tag: a dismissal moves it after the fact")
	}
	if hasField(bob, "state") {
		t.Error("the demoted state must not reuse the old tag's column name")
	}
	if bob.Fields["review_state"] != "APPROVED" {
		t.Errorf("review_state = %v, want APPROVED", bob.Fields["review_state"])
	}
	// The author's reply in a thread arrives as a COMMENTED review under
	// her own name. It is a row, since it happened, and it is tagged so a
	// reviewers table can leave it out: nobody reviews their own work.
	alice := find(t, points, "gh_pull_request_review", map[string]string{"reviewer": "alice"})
	if alice.Tags["self"] != "true" || alice.Tags["bot"] != "false" || fieldInt(t, alice, "seconds_to_review") != 3600 {
		t.Errorf("self review = %v %v", alice.Tags, alice.Fields)
	}
	// The bot is a review row like any other, told apart by its tag so the
	// reviewers table can show it apart or leave it out.
	sourcery := find(t, points, "gh_pull_request_review", map[string]string{"reviewer": "sourcery-ai[bot]"})
	if sourcery.Tags["bot"] != "true" || fieldInt(t, sourcery, "seconds_to_review") != 5 {
		t.Errorf("bot review = %v %v", sourcery.Tags, sourcery.Fields)
	}
	if want := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC); !bob.Time.Equal(want) {
		t.Errorf("review stamped %s, want submittedAt %s", bob.Time, want)
	}
	if fieldInt(t, bob, "seconds_to_review") != 23*3600 {
		t.Errorf("seconds_to_review = %v", bob.Fields["seconds_to_review"])
	}
	// A deleted reviewer becomes (ghost), and a ghost was a person: GitHub
	// retires accounts, not apps, so it is not tagged as a bot.
	ghost := find(t, points, "gh_pull_request_review", map[string]string{"reviewer": "(ghost)"})
	if ghost.Fields["review_state"] != "COMMENTED" || ghost.Tags["bot"] != "false" || ghost.Tags["self"] != "false" {
		t.Errorf("ghost review tags = %v", ghost.Tags)
	}
}

// checkIssueRows reads the issues, which follow the same dating rule as the
// pull requests.
func checkIssueRows(t *testing.T, points []sink.Point) {
	t.Helper()
	// Issues follow the same dating rule.
	closed := find(t, points, "gh_issue", map[string]string{"number": "7"})
	if want := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC); !closed.Time.Equal(want) {
		t.Errorf("closed issue stamped %s, want %s", closed.Time, want)
	}
	if fieldInt(t, closed, "seconds_to_close") != 2*86400 || fieldInt(t, closed, "labels") != 2 {
		t.Errorf("closed issue fields = %v", closed.Fields)
	}
	openIssue := find(t, points, "gh_issue", map[string]string{"number": "8"})
	if !openIssue.Time.Equal(startOfDay(testNow)) || openIssue.Tags["author"] != "(ghost)" {
		t.Errorf("open issue = %s %v", openIssue.Time, openIssue.Tags)
	}
}

func TestPullsHonoursFirst(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotFirst any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		gotFirst = vars["first"]
		f.write(w, "graphql_pulls.json")
	})
	if _, err := (Pulls{First: 100}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if gotFirst != float64(100) {
		t.Errorf("first = %v, want 100", gotFirst)
	}
}

func TestPullsGraphQLErrorIsReturned(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`))
	})
	_, err := Pulls{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil || !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Fatalf("err = %v, want the GraphQL error", err)
	}
}

func TestPullsSendsBothConnectionsOnTheFirstPage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotVars map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		gotVars = vars
		f.write(w, "graphql_pulls.json")
	})
	if _, err := (Pulls{}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if gotVars["withPRs"] != true || gotVars["withIssues"] != true {
		t.Errorf("both connections must be included on the first page: %v", gotVars)
	}
	if _, set := gotVars["prAfter"]; set {
		t.Error("no cursor on the first page")
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("a sweep is one query, made %d", n)
	}
}

func TestPullsHalvesThePageWhenTheGatewayGivesUp(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var firsts []any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		firsts = append(firsts, vars["first"])
		if len(firsts) == 1 {
			// Measured: the gateway answers a query it cannot finish with an
			// HTML 502, whatever the point cost.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			return
		}
		f.write(w, "graphql_pulls.json")
	})
	points, err := Pulls{First: 100}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("a too-large query must be retried smaller, not failed: %v", err)
	}
	if len(firsts) != 2 || firsts[0] != float64(100) || firsts[1] != float64(50) {
		t.Errorf("page sizes asked for = %v, want 100 then 50", firsts)
	}
	checkPoints(t, points)
	if len(only(t, points, "gh_pull_request")) != 2 {
		t.Error("the retried page must be rendered")
	}
	// The retry must render the whole selection, review threads included:
	// they are what pushed the query into the gateway's ceiling, so they are
	// the part a half-hearted retry would drop.
	if len(only(t, points, "gh_review_thread")) != 4 {
		t.Errorf("got %d review threads after the retry, want 4", len(only(t, points, "gh_review_thread")))
	}
}

// TestPullsHonoursThreads checks the knob the review threads made necessary.
func TestPullsHonoursThreads(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var got any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		got = vars["threads"]
		if !strings.Contains(query, "reviewThreads(first: $threads)") {
			t.Errorf("the thread page must be a variable, not a literal:\n%s", query)
		}
		f.write(w, "graphql_pulls.json")
	})
	if _, err := (Pulls{Threads: 25}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if got != float64(25) {
		t.Errorf("threads = %v, want 25", got)
	}
}

// TestPullsHalvingDoesNotDoubleEmitAPage is the hazard the review threads
// made real. Measured on 2026-09-10, three attempts out of three: the
// backfill page size of 100 with the new selection is an HTML 502 after
// 10.8 s, so a backfill now halves on the first page of every busy
// repository. The retry reuses the same cursor, and a page counted twice
// would double every duration this collector exists to measure.
func TestPullsHalvingDoesNotDoubleEmitAPage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var firsts []any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		firsts = append(firsts, vars["first"])
		if len(firsts) == 1 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			return
		}
		var page map[string]any
		mustUnmarshal(t, fixture(t, "graphql_pulls.json"), &page)
		repo := page["data"].(map[string]any)["repository"].(map[string]any)
		prs := repo["pullRequests"].(map[string]any)
		if vars["prAfter"] == nil {
			prs["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "page-2"}
			repo["issues"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": ""}
		} else {
			// Distinct numbers, so a page written twice is visible as a
			// duplicate rather than hidden behind a repeated fixture.
			for i, n := range prs["nodes"].([]any) {
				n.(map[string]any)["number"] = 44 + i
			}
			prs["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": ""}
			delete(repo, "issues")
		}
		_, _ = w.Write(mustMarshal(t, page))
	})
	points, err := Pulls{First: 100, Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(firsts) != 3 || firsts[0] != float64(100) || firsts[1] != float64(50) || firsts[2] != float64(50) {
		t.Errorf("page sizes asked for = %v, want 100 then 50 twice", firsts)
	}
	seen := map[string]int{}
	for _, p := range only(t, points, "gh_pull_request") {
		seen[p.Tags["number"]]++
	}
	want := map[string]int{"42": 1, "43": 1, "44": 1, "45": 1}
	if !maps.Equal(seen, want) {
		t.Errorf("pull requests written = %v, want each of 42 to 45 exactly once", seen)
	}
	// Eight threads over two pages, two of them without a comment to date them.
	if n := len(only(t, points, "gh_review_thread")); n != 8 {
		t.Errorf("got %d review threads, want 8", n)
	}
	if n := len(only(t, points, "gh_issue")); n != 2 {
		t.Errorf("got %d issues, want the one page that had them", n)
	}
}

func TestPullsWalksBothCursorsDuringABackfill(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		calls = append(calls, vars)
		var page map[string]any
		mustUnmarshal(t, fixture(t, "graphql_pulls.json"), &page)
		repo := page["data"].(map[string]any)["repository"].(map[string]any)
		prs := repo["pullRequests"].(map[string]any)
		issues := repo["issues"].(map[string]any)
		if vars["prAfter"] == nil {
			// Pull requests have a second page; issues end here.
			prs["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "pr-cursor-1"}
			issues["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": "issue-cursor-1"}
		} else {
			prs["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": "pr-cursor-2"}
			// Excluded by the directive, so the server omits the field.
			delete(repo, "issues")
		}
		_, _ = w.Write(mustMarshal(t, page))
	})
	points, err := Pulls{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(calls) != 2 {
		t.Fatalf("made %d queries, want 2", len(calls))
	}
	if calls[1]["prAfter"] != "pr-cursor-1" || calls[1]["withPRs"] != true || calls[1]["withIssues"] != false {
		t.Errorf("second query = %v, want the pull request cursor and issues switched off", calls[1])
	}
	if len(only(t, points, "gh_pull_request")) != 4 || len(only(t, points, "gh_issue")) != 2 {
		t.Errorf("got %d pull requests and %d issues, want 4 and 2", len(only(t, points, "gh_pull_request")), len(only(t, points, "gh_issue")))
	}
}

func TestPullsBackfillStopsAtTheBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		var page map[string]any
		mustUnmarshal(t, fixture(t, "graphql_pulls.json"), &page)
		repo := page["data"].(map[string]any)["repository"].(map[string]any)
		for _, conn := range []string{"pullRequests", "issues"} {
			c := repo[conn].(map[string]any)
			c["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "more"}
			for _, n := range c["nodes"].([]any) {
				n.(map[string]any)["updatedAt"] = "2020-01-01T00:00:00Z"
			}
		}
		_, _ = w.Write(mustMarshal(t, page))
	})
	// Everything on the page is older than the bound, so there is no second
	// query even though the API offers one.
	_, err := Pulls{Walk: Walk{Pages: -1, Since: testNow.AddDate(-1, 0, 0)}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("made %d queries, the bound should have ended the walk after 1", n)
	}
}

// TestPageForCoversTheTotal is the sizing that turns a fixed eight points per
// repository into what the repository actually holds. The page must never be
// smaller than the total, or a repository with seven pull requests would
// stop rewriting two of them; and never larger than fifty, which is where
// the gateway starts giving up.
func TestPageForCoversTheTotal(t *testing.T) {
	t.Parallel()
	for total, want := range map[int]int{
		0: 5, 1: 5, 5: 5, 6: 10, 10: 10, 11: 20, 20: 20, 21: 50, 50: 50, 400: 50,
	} {
		if got := PageFor(total); got != want {
			t.Errorf("PageFor(%d) = %d, want %d", total, got, want)
		}
	}
}

// TestReadItemCountsFollowsTheTotalsWireFormat reads the counts the way the
// runner does, out of the points the totals family emits, so a renamed field
// there is caught here rather than by every page silently growing to fifty.
func TestReadItemCountsFollowsTheTotalsWireFormat(t *testing.T) {
	t.Parallel()
	points := []sink.Point{
		{
			Measurement: "gh_repo_total", Tags: map[string]string{"full_name": "octocat/hello-world"},
			Fields: map[string]any{
				"pulls_open": 2, "pulls_merged": 30, "pulls_closed": 1,
				"issues_open": 4, "issues_closed": 40, "issues": 44,
			},
		},
		{
			Measurement: "gh_repo_total", Tags: map[string]string{"full_name": "octocat/quiet"},
			Fields: map[string]any{"pulls_open": 0, "pulls_merged": 0, "pulls_closed": 0, "issues_open": 0, "issues_closed": 0},
		},
		// Another measurement carrying the same tag is not a count.
		{
			Measurement: "gh_repo_policy", Tags: map[string]string{"full_name": "octocat/other"},
			Fields: map[string]any{"pulls_open": 9},
		},
	}
	got := map[string]ItemCounts{"octocat/stale": {Pulls: 1}}
	ReadItemCounts(points, got)
	want := map[string]ItemCounts{
		"octocat/hello-world": {Pulls: 33, Issues: 44},
		"octocat/quiet":       {},
		"octocat/stale":       {Pulls: 1},
	}
	if !maps.Equal(got, want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
	if got["octocat/hello-world"].Most() != 44 || PageFor(got["octocat/quiet"].Most()) != 5 {
		t.Errorf("the page follows the larger of the two connections: %v", got)
	}
}

// TestPullsReadsExactlyThePagesTheWalkAllows is what the daily pass leans on:
// sized from a count up to twelve hours old, it is allowed one page more than
// the one it asked for, and no more, however many pages the API offers.
func TestPullsReadsExactlyThePagesTheWalkAllows(t *testing.T) {
	t.Parallel()
	for pages, want := range map[int]int{0: 1, 2: 2} {
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
			var page map[string]any
			mustUnmarshal(t, fixture(t, "graphql_pulls.json"), &page)
			repo := page["data"].(map[string]any)["repository"].(map[string]any)
			for _, conn := range []string{"pullRequests", "issues"} {
				repo[conn].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "more"}
			}
			_, _ = w.Write(mustMarshal(t, page))
		})
		if _, err := (Pulls{First: 5, Walk: Walk{Pages: pages}}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls("/graphql")); n != want {
			t.Errorf("Walk{Pages: %d} made %d queries, want %d", pages, n, want)
		}
	}
}
