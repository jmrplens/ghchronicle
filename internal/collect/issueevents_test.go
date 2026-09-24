package collect

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// timelineFixtures answers the sweep's timeline query and the backfill's
// per-issue lists for the two items of issue_events.json, so the same seven
// events can be read by either road. The per-issue rows are the list's rows
// without the embedded issue, which is what that endpoint answers.
func timelineFixtures(t *testing.T, f *fixtureServer) {
	t.Helper()
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		f.write(w, "graphql_issue_timeline.json")
	})
	var rows []map[string]any
	if err := json.Unmarshal(fixture(t, "issue_events.json"), &rows); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{12, 41} {
		var own []map[string]any
		for _, row := range rows {
			if int(row["issue"].(map[string]any)["number"].(float64)) != number {
				continue
			}
			bare := map[string]any{}
			for k, v := range row {
				if k != "issue" {
					bare[k] = v
				}
			}
			own = append(own, bare)
		}
		// Oldest first, as the endpoint lists them.
		for i, j := 0, len(own)-1; i < j; i, j = i+1, j-1 {
			own[i], own[j] = own[j], own[i]
		}
		body, err := json.Marshal(own)
		if err != nil {
			t.Fatal(err)
		}
		f.handle("/repos/octocat/hello-world/issues/"+strconv.Itoa(number)+"/events", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		})
	}
}

// timelineVars is what the last timeline query asked for.
func timelineVars(t *testing.T, f *fixtureServer) map[string]any {
	t.Helper()
	calls := f.calls("/graphql")
	if len(calls) == 0 {
		t.Fatal("no timeline query was made")
	}
	var env struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(calls[len(calls)-1].Body, &env); err != nil {
		t.Fatal(err)
	}
	return env.Variables
}

func TestIssueEventsRecordTheTransitions(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	timelineFixtures(t, f)

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points, err := IssueEvents{Since: since}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_issue_event")
	if len(points) != 7 {
		t.Fatalf("got %d events, want one per timeline item", len(points))
	}
	// One query, ten items a page with their timelines, from the window's
	// start; and no per-issue list, because neither item is in a stack or
	// over a hundred events.
	vars := timelineVars(t, f)
	if vars["first"] != float64(timelineWithEvents) || vars["withTimeline"] != true || vars["since"] != "2026-09-01T00:00:00Z" {
		t.Errorf("query variables = %v", vars)
	}
	if n := len(f.calls("/repos/octocat/hello-world/issues/41/events")) + len(f.calls("/repos/octocat/hello-world/issues/12/events")); n != 0 {
		t.Errorf("the sweep read %d per-issue lists, want none", n)
	}

	// Which connection the item came from says whether the event was on a
	// pull request, so it takes no second call to tell them apart.
	labeled := find(t, points, "gh_issue_event", map[string]string{"event": "labeled"})
	if labeled.Tags["kind"] != "pull_request" || labeled.Tags["bot"] != "true" || labeled.Tags["label"] != "docker" {
		t.Errorf("labeled = %v", labeled.Tags)
	}
	// GraphQL names an app without the [bot] suffix the list writes, and
	// the tag is the list's.
	if labeled.Tags["actor"] != "dependabot[bot]" {
		t.Errorf("actor = %q, want the login as /issues/events writes it", labeled.Tags["actor"])
	}
	if fieldInt(t, labeled, "number") != 41 {
		t.Errorf("number = %v, and it is a field: an issue number is not a series", labeled.Fields["number"])
	}
	if labeled.Fields["url"] != "https://github.com/octocat/hello-world/pull/41" {
		t.Errorf("url = %v", labeled.Fields["url"])
	}
	if want := time.Date(2026, 9, 7, 6, 45, 42, 0, time.UTC); !labeled.Time.Equal(want) {
		t.Errorf("event stamped %s, want createdAt %s", labeled.Time, want)
	}

	// A reopening exists in no other measurement at all.
	reopened := find(t, points, "gh_issue_event", map[string]string{"event": "reopened"})
	if reopened.Tags["kind"] != "issue" || reopened.Tags["bot"] != "false" {
		t.Errorf("reopened = %v", reopened.Tags)
	}
	// Every point of the measurement carries the tag, so the measurement is
	// one series per event rather than one per tag set that happened to fit.
	if reopened.Tags["label"] != noneTag || reopened.Tags["milestone"] != noneTag {
		t.Errorf("an event with no label and no milestone = %v", reopened.Tags)
	}
	milestoned := find(t, points, "gh_issue_event", map[string]string{"event": "milestoned"})
	if milestoned.Tags["milestone"] != "v2.8.0" {
		t.Errorf("milestoned = %v", milestoned.Tags)
	}

	checkReviewRequestTags(t, points)
	checkRenameAndCommitFields(t, points)
}

// checkReviewRequestTags reads who was asked for a review and by whom, which
// gh_issue_event recorded as an anonymous "a review was requested" until now.
func checkReviewRequestTags(t *testing.T, points []sink.Point) {
	t.Helper()
	req := find(t, points, "gh_issue_event", map[string]string{"event": "review_requested"})
	if req.Tags["requested_reviewer"] != "octocat" || req.Tags["review_requester"] != "dependabot[bot]" {
		t.Errorf("review_requested = %v", req.Tags)
	}
	// Both are tags, so like label and milestone they are written on every
	// point of the measurement. A tag on some points and not on others gives
	// gh_issue_event two Graphite path depths, and that bug has been fixed
	// here once already.
	for _, p := range points {
		if p.Tags["requested_reviewer"] == "" || p.Tags["review_requester"] == "" {
			t.Errorf("%s event has no reviewer fallback: %v", p.Tags["event"], p.Tags)
		}
	}
	other := find(t, points, "gh_issue_event", map[string]string{"event": "reopened"})
	if other.Tags["requested_reviewer"] != noneTag || other.Tags["review_requester"] != noneTag {
		t.Errorf("an event that is not a review request = %v", other.Tags)
	}
}

// checkRenameAndCommitFields reads the two facts the event body carried and
// the collector dropped: what a title was changed from and to, and the commit
// an event points at.
func checkRenameAndCommitFields(t *testing.T, points []sink.Point) {
	t.Helper()
	renamed := find(t, points, "gh_issue_event", map[string]string{"event": "renamed"})
	if renamed.Fields["rename_from"] != "sent tail fields" {
		t.Errorf("rename_from = %v", renamed.Fields["rename_from"])
	}
	if renamed.Fields["rename_to"] != "feat(tools): publish the tail of the fields GitLab sends and we dropped" {
		t.Errorf("rename_to = %v", renamed.Fields["rename_to"])
	}
	referenced := find(t, points, "gh_issue_event", map[string]string{"event": "referenced"})
	if referenced.Fields["commit_id"] != "4d73bc0213b3daea8477a1bf43fc7bfa0d755338" {
		t.Errorf("commit_id = %v", referenced.Fields["commit_id"])
	}
	// 80 of 100 events measured send an explicit null commit_id, so the
	// field is written only where there is one rather than on every point.
	closed := find(t, points, "gh_issue_event", map[string]string{"event": "closed"})
	if hasField(closed, "commit_id") || hasField(closed, "rename_from") {
		t.Errorf("an event with neither = %v", closed.Fields)
	}
	// state_reason is null in all 51 closed events measured, and intent in
	// all 175 that carry it. Neither is collected.
	if hasField(closed, "state_reason") || hasField(closed, "intent") {
		t.Errorf("a key that is always null must not become a field: %v", closed.Fields)
	}
}

// TestIssueEventsBothRoadsAgree is the measurement of 2026-09-11 as a test:
// the seven events read through the timeline and the same seven read through
// the per-issue list render to the same seven points, line for line, which
// is what lets a sweep and a backfill write the same rows.
func TestIssueEventsBothRoadsAgree(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	timelineFixtures(t, f)
	sweep, err := IssueEvents{Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	backfill, err := IssueEvents{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(sweep) != 7 || len(backfill) != 7 {
		t.Fatalf("sweep %d points, backfill %d, want 7 each", len(sweep), len(backfill))
	}
	if !samePoints(sweep, backfill) {
		t.Errorf("the two roads disagree:\n%s\n%s", strings.Join(lines(sweep), "\n"), strings.Join(lines(backfill), "\n"))
	}
	// And the backfill went the way it should: a listing without timelines,
	// fifty items a page, then one per-issue list per item.
	vars := timelineVars(t, f)
	if vars["first"] != float64(timelineListing) || vars["withTimeline"] != false {
		t.Errorf("backfill listing variables = %v", vars)
	}
	for _, number := range []string{"12", "41"} {
		if n := len(f.calls("/repos/octocat/hello-world/issues/" + number + "/events")); n != 1 {
			t.Errorf("the backfill read item %s's list %d times, want once", number, n)
		}
	}
}

// TestIssueEventsFallBackWhenTheActorIsGone pins the last tag of the
// measurement that could still go out empty. GitHub declares the actor
// nullable, the collector has always guarded against it, and the guard used to
// leave `actor=""`, which InfluxDB drops: those events would have formed a
// second series of gh_issue_event with no actor tag at all.
func TestIssueEventsFallBackWhenTheActorIsGone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"repository":{` +
			`"issues":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":12,"title":"Cannot bind a second port",` +
			`"url":"https://github.com/octocat/hello-world/issues/12","updatedAt":"2026-09-07T06:45:42Z",` +
			`"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"__typename":"ClosedEvent","createdAt":"2026-09-07T06:45:42Z","actor":null,"closer":null}]}}]},` +
			`"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`))
	})
	points, err := IssueEvents{Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// checkPoints refuses an empty tag value, so this would fail there too;
	// the explicit read says which tag and what it must say.
	checkPoints(t, points)
	if len(points) != 1 {
		t.Fatalf("got %d points", len(points))
	}
	if points[0].Tags["actor"] != noneTag || points[0].Tags["bot"] != "false" {
		t.Errorf("an event with no actor = %v", points[0].Tags)
	}
}

// TestIssueEventsStopAtTheWalkHorizon: a backfill bounded at the 7th lists
// the items updated since then, which is the pull request alone, walks its
// list and keeps that day's three; the issue, last updated on the 6th, is
// not asked for at all.
func TestIssueEventsStopAtTheWalkHorizon(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	timelineFixtures(t, f)

	since := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	points, err := IssueEvents{Walk: Walk{Pages: -1, Since: since}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("got %d events, want only those inside the horizon", len(points))
	}
	if n := len(f.calls("/repos/octocat/hello-world/issues/12/events")); n != 0 {
		t.Errorf("an item updated before the bound was asked for its list %d times", n)
	}
}

// TestIssueEventsReadTheListForAStackedPullRequest: added_to_stack has no
// type in the timeline, so a pull request in a stack is read through its
// own list instead, and its timeline items are not written twice.
func TestIssueEventsReadTheListForAStackedPullRequest(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"repository":{` +
			`"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]},` +
			`"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":706,"title":"Stacked",` +
			`"url":"https://github.com/octocat/hello-world/pull/706","updatedAt":"2026-09-08T09:00:00Z","stackEntry":{"position":2},` +
			`"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"__typename":"LabeledEvent","createdAt":"2026-09-08T08:00:00Z","actor":{"login":"octocat","__typename":"User"},"label":{"name":"tooling"}}]}}]}}}}`))
	})
	f.handle("/repos/octocat/hello-world/issues/706/events", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"event":"added_to_stack","created_at":"2026-09-08T07:59:00Z","actor":{"login":"octocat","type":"User"},"commit_id":null},` +
			`{"id":2,"event":"labeled","created_at":"2026-09-08T08:00:00Z","actor":{"login":"octocat","type":"User"},"label":{"name":"tooling"},"commit_id":null}]`))
	})
	points, err := IssueEvents{Since: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 2 {
		t.Fatalf("got %d events, want the list's two and the timeline's item not repeated:\n%s", len(points), strings.Join(lines(points), "\n"))
	}
	stacked := find(t, points, "gh_issue_event", map[string]string{"event": "added_to_stack"})
	if stacked.Tags["kind"] != "pull_request" || stacked.Fields["url"] != "https://github.com/octocat/hello-world/pull/706" {
		t.Errorf("added_to_stack = %v %v", stacked.Tags, stacked.Fields)
	}
}

// The name the list gives an event is the timeline type's, without Event, in
// snake case: pinned on the four shapes that could go wrong, and on the one
// exception.
func TestTimelineTypesAreNamedAsTheListNamesThem(t *testing.T) {
	t.Parallel()
	for typename, want := range map[string]string{
		"LabeledEvent":              "labeled",
		"RenamedTitleEvent":         "renamed",
		"HeadRefForcePushedEvent":   "head_ref_force_pushed",
		"ReviewRequestRemovedEvent": "review_request_removed",
		"AddedToProjectV2Event":     "added_to_project_v2",
		"AutoMergeEnabledEvent":     "auto_merge_enabled",
	} {
		if got := restEventName(typename); got != want {
			t.Errorf("restEventName(%s) = %q, want %q", typename, got, want)
		}
	}
	for typename, want := range map[string]string{
		"HeadRefForcePushedEvent": "HEAD_REF_FORCE_PUSHED_EVENT",
		"RenamedTitleEvent":       "RENAMED_TITLE_EVENT",
		"AddedToProjectV2Event":   "ADDED_TO_PROJECT_V2_EVENT",
	} {
		if got := timelineItemType(typename); got != want {
			t.Errorf("timelineItemType(%s) = %q, want %q", typename, got, want)
		}
	}
	// Every type the table names has its fragment in the query, on the
	// timeline it belongs to, and the seven non-events are asked of neither.
	for typename, ev := range timelineEvents {
		fragment := "... on " + typename + " {"
		if strings.Count(issueTimelineQuery, fragment) != map[bool]int{true: 1, false: 2}[ev.pullOnly] {
			t.Errorf("%s appears %d times in the query", typename, strings.Count(issueTimelineQuery, fragment))
		}
	}
	for _, typename := range []string{"IssueComment", "PullRequestCommit", "PullRequestReview", "CrossReferencedEvent"} {
		if strings.Contains(issueTimelineQuery, "... on "+typename+" {") {
			t.Errorf("%s is asked for, and the list never emits it", typename)
		}
	}
}

func TestAccountKeys(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/user/keys", "user_keys.json")
	f.file("/user/gpg_keys", "user_gpg_keys.json")

	points, err := Keys{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_key")

	used := find(t, points, "gh_key", map[string]string{"key": "nginx"})
	if used.Tags["kind"] != "ssh" || fieldInt(t, used, "days_since_use") != 7 {
		t.Errorf("used key = %v %v", used.Tags, used.Fields)
	}
	// Never used is a different fact from used long ago, and it is the one
	// that says a key can be removed.
	never := find(t, points, "gh_key", map[string]string{"key": "matrix"})
	if hasField(never, "days_since_use") || fieldInt(t, never, "never_used") != 1 {
		t.Errorf("unused key = %v", never.Fields)
	}
	// A key is a standing fact, so it is stamped at the start of the day like
	// a deploy key: dating it at creation would hide every key older than the
	// dashboard range, which is most of them.
	if want := testNow.UTC().Truncate(24 * time.Hour); !used.Time.Equal(want) {
		t.Errorf("key stamped %s, want the start of the day %s", used.Time, want)
	}
	if fieldInt(t, used, "age_days") < 100 {
		t.Errorf("age_days = %v, and it is where the creation date went", used.Fields["age_days"])
	}
	gpg := find(t, points, "gh_key", map[string]string{"kind": "gpg"})
	if gpg.Tags["key"] != "FCF653391E2C91FC" || fieldInt(t, gpg, "emails") != 2 {
		t.Errorf("gpg key = %v %v", gpg.Tags, gpg.Fields)
	}
	// The expiry of the key that signs everything, in days, because that is
	// what an alert threshold is written against.
	if days := fieldInt(t, gpg, "days_to_expiry"); days < 500 || days > 700 {
		t.Errorf("days_to_expiry = %d, want about 573", days)
	}
}

func TestCachePrefixDropsTheContentHash(t *testing.T) {
	t.Parallel()
	// The whole key is a series per build. The prefix is what a reader means
	// by "the pnpm cache", and the whole key stays as a field.
	for key, want := range map[string]string{
		"node-cache-Linux-x64-pnpm-95070cf0a7bb1e24893222dad3a0f17146df6b709443a20a85e19daccfb434c0": "node-cache-Linux-x64-pnpm",
		"cache-lychee-source-826f92781d79adf96fb819ce652d809241ddaa44":                               "cache-lychee-source",
		"no-hash-here": "no-hash-here",
		"":             "",
	} {
		if got := cachePrefix(key); got != want {
			t.Errorf("cachePrefix(%q) = %q, want %q", key, got, want)
		}
	}
}

// headRoute answers the head of the default branch the way GitHub does when
// asked for the bare SHA: forty characters of text, no JSON, and only under
// that media type. A request for the JSON form is the five kilobyte listing
// this used to download, and it is refused here so the test notices.
func headRoute(f *fixtureServer) {
	f.handle("/repos/octocat/hello-world/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = io.WriteString(w, `{"message":"the test wants the bare sha"}`)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.sha")
		_, _ = io.WriteString(w, "head1234\n")
	})
}

func TestDependenciesAggregateRatherThanListing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/dependency-graph/sbom", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sbom": map[string]any{"packages": []any{
			map[string]any{
				"name": "left-pad", "licenseConcluded": "MIT",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:npm/left-pad@1.3.0"}},
			},
			map[string]any{
				"name": "qs", "licenseConcluded": "BSD-3-Clause",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:npm/qs@6.15.3"}},
			},
			map[string]any{
				"name": "actions/checkout", "licenseConcluded": "NOASSERTION",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:githubactions/actions/checkout@v7"}},
			},
		}}})
	})
	headRoute(f)
	f.handle("/repos/octocat/hello-world/dependency-graph/compare/base9876...head1234", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"change_type": "added", "ecosystem": "npm", "name": "a", "license": "MIT"},
			{"change_type": "added", "ecosystem": "npm", "name": "b", "license": "MIT"},
			{
				"change_type": "removed", "ecosystem": "npm", "name": "qs", "license": "BSD-3-Clause",
				"vulnerabilities": []any{map[string]any{"severity": "moderate"}},
			},
		})
	})

	d := &Dependencies{Base: "base9876", SBOM: true}
	points, err := d.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_dependency", "gh_dependency_license", "gh_dependency_change")

	if d.ResolvedHead != "head1234" {
		t.Errorf("ResolvedHead = %q, and it is what the next range starts from", d.ResolvedHead)
	}
	npm := find(t, points, "gh_dependency", map[string]string{"ecosystem": "npm"})
	if fieldInt(t, npm, "packages") != 2 {
		t.Errorf("npm packages = %v", npm.Fields)
	}
	find(t, points, "gh_dependency", map[string]string{"ecosystem": "githubactions"})
	// NOASSERTION is GitHub saying it could not tell, not a license.
	undetermined := find(t, points, "gh_dependency_license", map[string]string{"license": "undetermined"})
	if fieldInt(t, undetermined, "packages") != 1 {
		t.Errorf("undetermined = %v", undetermined.Fields)
	}
	added := find(t, points, "gh_dependency_change", map[string]string{"change": "added"})
	if fieldInt(t, added, "packages") != 2 || fieldInt(t, added, "vulnerable") != 0 {
		t.Errorf("added = %v", added.Fields)
	}
	removed := find(t, points, "gh_dependency_change", map[string]string{"change": "removed"})
	if fieldInt(t, removed, "packages") != 1 || fieldInt(t, removed, "vulnerable") != 1 {
		t.Errorf("removed = %v", removed.Fields)
	}
}

// TestDependenciesSkipTheSBOMWhenTheHeadDidNotMove pins that a repository
// without a commit since the last sweep costs one free 304 for its head and
// nothing from the SBOM bucket, and that a repository whose head moved, or a
// first sweep, still takes the photograph.
func TestDependenciesSkipTheSBOMWhenTheHeadDidNotMove(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/dependency-graph/sbom", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sbom": map[string]any{"packages": []any{
			map[string]any{
				"name": "left-pad", "licenseConcluded": "MIT",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:npm/left-pad@1.3.0"}},
			},
		}}})
	})
	headRoute(f)

	// The base is the head the route answers: nothing moved, and the one
	// row says so with both ends of the range that did not open.
	still := &Dependencies{Base: "head1234", SBOM: true}
	points, err := still.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantNoDependencyChange(t, points, "head1234", "head1234")
	if n := len(f.calls("/repos/octocat/hello-world/dependency-graph/sbom")); n != 0 {
		t.Errorf("%d SBOM reads for a head that did not move, want none", n)
	}
	if n := len(f.calls("/repos/octocat/hello-world/dependency-graph/compare/head1234...head1234")); n != 0 {
		t.Errorf("%d compares for a head that did not move, want none: the zero row is free", n)
	}
	if still.ResolvedHead != "head1234" {
		t.Errorf("ResolvedHead = %q, the next sweep must still compare against the head", still.ResolvedHead)
	}

	// A first sweep has no base, and a moved head has a different one: both
	// take the photograph.
	for _, base := range []string{"", "base9876"} {
		f.handle("/repos/octocat/hello-world/dependency-graph/compare/base9876...head1234", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		})
		before := len(f.calls("/repos/octocat/hello-world/dependency-graph/sbom"))
		d := &Dependencies{Base: base, SBOM: true}
		if _, collectErr := d.Collect(ctx(t), f.Client, testRepo, testNow); collectErr != nil {
			t.Fatalf("base %q: %v", base, collectErr)
		}
		if n := len(f.calls("/repos/octocat/hello-world/dependency-graph/sbom")) - before; n != 1 {
			t.Errorf("base %q: %d SBOM reads, want the photograph", base, n)
		}
	}
}

func TestDependenciesWithoutABaseOnlyPhotograph(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	headRoute(f)

	// The first sweep has no range. It must still record where the next one
	// starts rather than failing or comparing against nothing, and it writes
	// the zero row with the head alone, so the table exists from this sweep.
	d := &Dependencies{}
	points, err := d.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantNoDependencyChange(t, points, "", "head1234")
	if d.ResolvedHead != "head1234" {
		t.Errorf("ResolvedHead = %q", d.ResolvedHead)
	}

	// No head at all, no row: there is nothing to say a range against.
	f.status("/repos/octocat/hello-world/commits/HEAD", http.StatusNotFound, "Not Found")
	empty := &Dependencies{}
	points, err = empty.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 || empty.ResolvedHead != "" {
		t.Errorf("without a head: %d points, ResolvedHead %q, err %v; want nothing", len(points), empty.ResolvedHead, err)
	}
}

// TestDependenciesWriteAZeroRowForARangeWithoutChanges is the third case of
// the same rule: a real range the compare answers with nothing, which is most
// commits, still writes the row. Without it the table waited for the first
// dependency bump, and measured on 2026-09-12 the panel over it was an error
// on every time range after a day of sweeps.
func TestDependenciesWriteAZeroRowForARangeWithoutChanges(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	headRoute(f)
	f.handle("/repos/octocat/hello-world/dependency-graph/compare/base9876...head1234", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	d := &Dependencies{Base: "base9876"}
	points, err := d.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantNoDependencyChange(t, points, "base9876", "head1234")
}

// wantNoDependencyChange checks that points is the one zero row of
// gh_dependency_change and nothing else: change and ecosystem at (none),
// both counters at zero, the range's ends as fields, and no base at all on a
// first sweep, because the sinks drop an empty string and the row must not
// carry one.
func wantNoDependencyChange(t *testing.T, points []sink.Point, from, to string) {
	t.Helper()
	checkPoints(t, points)
	rows := only(t, points, "gh_dependency_change")
	if len(rows) != 1 || len(points) != 1 {
		t.Fatalf("got %d points, %d of gh_dependency_change; want the one zero row", len(points), len(rows))
	}
	row := rows[0]
	if row.Tags["change"] != noneTag || row.Tags["ecosystem"] != noneTag {
		t.Errorf("zero row tagged %v, want change and ecosystem at %s", row.Tags, noneTag)
	}
	if fieldInt(t, row, "packages") != 0 || fieldInt(t, row, "vulnerable") != 0 {
		t.Errorf("zero row counts %v, want zero", row.Fields)
	}
	if row.Fields["head"] != to {
		t.Errorf("head = %v, want %q", row.Fields["head"], to)
	}
	if base, has := row.Fields["base"]; (from == "") == has || (has && base != from) {
		t.Errorf("base = %v (present %v), want %q", base, has, from)
	}
	if !row.Time.Equal(testNow) {
		t.Errorf("zero row stamped %s, want the sweep, as every row of this measurement is", row.Time)
	}
}

// TestIssueEventsFileTheMentionedUserApart pins where a mention goes.
//
// GitHub files `mentioned` and `subscribed` with the person it happened to as
// the actor. Kept there, the account named in "@coderabbitai" sat in the
// actor column beside the app that reviews, under a spelling of its own, and
// the `@id` and `@graph` of a JSON-LD block in a pull request body became
// actors called id and graph. The subject of the event goes in its own tag,
// like the label of a labeling, and the actor is left unnamed.
func TestIssueEventsFileTheMentionedUserApart(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"repository":{` +
			`"issues":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":12,"title":"Cannot bind a second port",` +
			`"url":"https://github.com/octocat/hello-world/issues/12","updatedAt":"2026-09-07T06:45:42Z",` +
			`"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[` +
			`{"__typename":"MentionedEvent","createdAt":"2026-09-07T06:45:42Z","actor":{"login":"id","__typename":"User"}},` +
			`{"__typename":"SubscribedEvent","createdAt":"2026-09-07T06:45:43Z","actor":{"login":"coderabbitai","__typename":"Bot"}},` +
			`{"__typename":"ClosedEvent","createdAt":"2026-09-07T06:45:44Z","actor":{"login":"dependabot","__typename":"Bot"},"closer":null}` +
			`]}}]},` +
			`"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`))
	})
	points, err := IssueEvents{Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 3 {
		t.Fatalf("got %d points, want 3", len(points))
	}
	mention := find(t, points, "gh_issue_event", map[string]string{"event": "mentioned"})
	if mention.Tags["mentioned"] != "id" || mention.Tags["actor"] != noneTag || mention.Tags["bot"] != "false" {
		t.Errorf("a mention names its subject in its own tag and no actor: %v", mention.Tags)
	}
	subscribed := find(t, points, "gh_issue_event", map[string]string{"event": "subscribed"})
	if subscribed.Tags["mentioned"] != "coderabbitai[bot]" || subscribed.Tags["actor"] != noneTag {
		t.Errorf("a subscription is filed the same way, with the app spelled as REST spells it: %v", subscribed.Tags)
	}
	// Every other event carries the tag too, empty, so the measurement keeps
	// one Graphite depth; and the app that acted is spelled the REST way.
	closed := find(t, points, "gh_issue_event", map[string]string{"event": "closed"})
	if closed.Tags["mentioned"] != noneTag || closed.Tags["actor"] != "dependabot[bot]" || closed.Tags["bot"] != "true" {
		t.Errorf("closed event tags = %v", closed.Tags)
	}
}
