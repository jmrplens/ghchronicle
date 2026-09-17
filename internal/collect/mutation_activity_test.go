package collect

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// activityBase is the tag set every collector of this repository starts from.
var activityBase = map[string]string{"owner": "octocat", "repo": "hello-world", "full_name": "octocat/hello-world"}

// activityRunawayGraphQL answers with an error once a query has been asked
// more than limit times. A loop that never ends then fails the test with an
// error instead of spinning until the context gives up.
func activityRunawayGraphQL(w http.ResponseWriter, f *fixtureServer, limit int) bool {
	if len(f.calls("/graphql")) <= limit {
		return false
	}
	_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"runaway walk"}]}`))
	return true
}

// activityRunawayREST is the REST twin: past limit calls on a path it
// answers 500, which no collector skips.
func activityRunawayREST(w http.ResponseWriter, f *fixtureServer, path string, limit int) bool {
	if len(f.calls(path)) <= limit {
		return false
	}
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"message":"runaway walk"}`))
	return true
}

// activityGraphQLVars decodes the variables of every GraphQL request so far,
// in the order they were asked.
func activityGraphQLVars(t *testing.T, f *fixtureServer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range f.calls("/graphql") {
		var env struct {
			Variables map[string]any `json:"variables"`
		}
		mustUnmarshal(t, c.Body, &env)
		out = append(out, env.Variables)
	}
	return out
}

// activityGraphQLQueries is the query text of every GraphQL request so far.
func activityGraphQLQueries(t *testing.T, f *fixtureServer) []string {
	t.Helper()
	var out []string
	for _, c := range f.calls("/graphql") {
		var env struct {
			Query string `json:"query"`
		}
		mustUnmarshal(t, c.Body, &env)
		out = append(out, env.Query)
	}
	return out
}

// activityStamp formats a moment the way GitHub writes one.
func activityStamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// --- actions.go ---------------------------------------------------------

// TestTheActorTagFallsBackToTheRunActorAndThenToNone covers the run that has
// no triggering actor, which a response from an older API version is: the tag
// must still name who pushed rather than drop to the placeholder.
func TestTheActorTagFallsBackToTheRunActorAndThenToNone(t *testing.T) {
	t.Parallel()
	var r runRow
	if got := actorTag(&r); got != noneTag {
		t.Errorf("a run with no actor at all tagged %q, want %q", got, noneTag)
	}
	r.Actor.Login = "dependabot[bot]"
	if got := actorTag(&r); got != "dependabot[bot]" {
		t.Errorf("a run with only an actor tagged %q, want the actor", got)
	}
	r.TriggeringActor.Login = "octocat"
	if got := actorTag(&r); got != "octocat" {
		t.Errorf("a re-run tagged %q, want the person who asked for the attempt", got)
	}
}

// TestWorkflowRunFieldsLeaveOutWhatWouldSayNothing reads the two shapes of a
// run the fixtures never had: a title equal to the workflow name and no run
// number, and a run whose start or creation time is missing. A field written
// for those would be a copy of the name, a zero, or a queue of fifty years.
func TestWorkflowRunFieldsLeaveOutWhatWouldSayNothing(t *testing.T) {
	t.Parallel()
	plain := runRow{
		Name: "CI", DisplayTitle: "CI", RunAttempt: 1,
		CreatedAt: testNow.Add(-10 * time.Minute), UpdatedAt: testNow,
	}
	fields := workflowRunFields(&plain, activityBase)
	if _, ok := fields["title"]; ok {
		t.Errorf("a title equal to the workflow name was written: %v", fields["title"])
	}
	if _, ok := fields["run_number"]; ok {
		t.Errorf("a run without a number carries run_number %v", fields["run_number"])
	}
	// No run_started_at, so the duration is measured from creation.
	if got := fields["duration_seconds"]; got != 600 {
		t.Errorf("duration_seconds = %v, want 600 measured from created_at", got)
	}
	if _, ok := fields["queued_seconds"]; ok {
		t.Errorf("a run that never said when it started has queued_seconds %v", fields["queued_seconds"])
	}

	titled := runRow{
		Name: "CI", DisplayTitle: "Bump docker/build-push-action", RunNumber: 1, RunAttempt: 1,
		RunStartedAt: testNow.Add(-8 * time.Minute), UpdatedAt: testNow,
	}
	fields = workflowRunFields(&titled, activityBase)
	if fields["title"] != "Bump docker/build-push-action" || fields["run_number"] != 1 {
		t.Errorf("title/run_number = %v/%v", fields["title"], fields["run_number"])
	}
	if got := fields["duration_seconds"]; got != 480 {
		t.Errorf("duration_seconds = %v, want 480 measured from run_started_at", got)
	}
	if _, ok := fields["queued_seconds"]; ok {
		t.Errorf("a run with no creation time has queued_seconds %v", fields["queued_seconds"])
	}
}

// TestCachePointsWriteTheTotalAndAgeEveryEntry reads both cache listings and
// holds each entry to its age in whole days, the number that says which key
// GitHub evicts first.
func TestCachePointsWriteTheTotalAndAgeEveryEntry(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/cache/usage", "actions_cache.json")
	f.file("/repos/octocat/hello-world/actions/caches", "actions_caches.json")
	points, err := cachePoints(ctx(t), f.Client, testRepo, activityBase, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := len(byMeasurement(points)["gh_actions_cache"]); n != 1 {
		t.Errorf("got %d cache totals, want 1", n)
	}
	if n := len(byMeasurement(points)["gh_actions_cache_entry"]); n != 2 {
		t.Fatalf("got %d cache entries, want 2", n)
	}
	// Created 2026-09-05T12:13:43Z, read 2026-09-08T15:04:05Z: three days.
	big := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": "node-cache-Linux-x64-pnpm"})
	if got := fieldInt(t, big, "age_days"); got != 3 {
		t.Errorf("age_days = %d, want 3", got)
	}
}

// TestCachePointsSkipAMissingListingButReportABrokenOne keeps the two answers
// apart: a 404 is a repository without Actions, a 500 is something broken
// that a sweep must not report as an empty cache.
func TestCachePointsSkipAMissingListingButReportABrokenOne(t *testing.T) {
	t.Parallel()
	t.Run("usage missing, listing broken", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/repos/octocat/hello-world/actions/caches", http.StatusInternalServerError, "boom")
		points, err := cachePoints(ctx(t), f.Client, testRepo, activityBase, testNow)
		if err == nil {
			t.Fatalf("a broken cache listing was not reported, got %d points", len(points))
		}
	})
	t.Run("usage broken", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/repos/octocat/hello-world/actions/cache/usage", http.StatusInternalServerError, "boom")
		f.file("/repos/octocat/hello-world/actions/caches", "actions_caches.json")
		if _, err := cachePoints(ctx(t), f.Client, testRepo, activityBase, testNow); err == nil {
			t.Fatal("a broken cache usage was not reported")
		}
	})
	t.Run("both missing", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		points, err := cachePoints(ctx(t), f.Client, testRepo, activityBase, testNow)
		if err != nil || len(points) != 0 {
			t.Fatalf("a repository without caches gave %d points and %v", len(points), err)
		}
	})
}

// TestCachePrefixTakesOnlyALongHexSegmentForTheHash walks the edges of what
// counts as a content hash: exactly sixteen digits, uppercase digits, a long
// segment that is not hexadecimal, and a key that starts with its hash.
func TestCachePrefixTakesOnlyALongHexSegmentForTheHash(t *testing.T) {
	t.Parallel()
	cases := []struct{ key, want string }{
		{"node-cache-Linux-x64-pnpm-0123456789abcdef", "node-cache-Linux-x64-pnpm"},
		{"node-cache-0123456789abcde", "node-cache-0123456789abcde"},
		{"cache-lychee-0123456789ABCDEF", "cache-lychee"},
		{"setup-go-abcdefghijklmnopqrstuvwxyz", "setup-go-abcdefghijklmnopqrstuvwxyz"},
		{"tool-0.123456789abcdef", "tool-0.123456789abcdef"},
		{"0123456789abcdef-linux", "0123456789abcdef-linux"},
	}
	for _, tc := range cases {
		if got := cachePrefix(tc.key); got != tc.want {
			t.Errorf("cachePrefix(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// activityJobs is a job listing with the three shapes the fixtures lack: a
// job that names no commit of its own and was never queued, one that does
// both, and one still running.
const activityJobs = `{"total_count": 3, "jobs": [
  {"name": "inherits", "status": "completed", "conclusion": "success", "run_attempt": 1,
   "started_at": "2026-09-07T10:00:35Z", "completed_at": "2026-09-07T10:03:00Z",
   "head_sha": "", "head_branch": "", "labels": [],
   "steps": [
     {"name": "half", "number": 1, "conclusion": "success", "started_at": "2026-09-07T10:00:35Z", "completed_at": null},
     {"name": "whole", "number": 2, "conclusion": "success", "started_at": "2026-09-07T10:00:40Z", "completed_at": "2026-09-07T10:02:50Z"}
   ]},
  {"name": "own", "status": "completed", "conclusion": "success", "run_attempt": 1,
   "created_at": "2026-09-07T10:00:05Z", "started_at": "2026-09-07T10:00:35Z", "completed_at": "2026-09-07T10:03:00Z",
   "head_sha": "ffffffffffffffffffffffffffffffffffffffff", "head_branch": "other", "labels": ["ubuntu-latest"], "steps": []},
  {"name": "unfinished", "status": "in_progress", "conclusion": "", "completed_at": null, "steps": []}
]}`

// TestActionsJobsTakeWhatTheyLackFromTheirRun reads a job listing whose jobs
// differ in what they carry. A job without a commit belongs to its run's; one
// with its own keeps it; a job or step that has not finished is not a fact
// yet; and a job that never said when it was created has no queue time.
func TestActionsJobsTakeWhatTheyLackFromTheirRun(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.handle("/repos/octocat/hello-world/actions/runs/1000163135/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(activityJobs))
	})
	points, err := Actions{Jobs: true, MaxJobRuns: 1, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	jobs := only(t, points, "gh_workflow_job")
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want the two that finished", len(jobs))
	}
	inherits := find(t, points, "gh_workflow_job", map[string]string{"job_name": "inherits"})
	if inherits.Fields["head_sha"] != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" || inherits.Fields["head_branch"] != "main" {
		t.Errorf("a job without a commit got %v on %v, want the run's", inherits.Fields["head_sha"], inherits.Fields["head_branch"])
	}
	if hasField(inherits, "queued_seconds") {
		t.Errorf("a job with no created_at has queued_seconds %v", inherits.Fields["queued_seconds"])
	}
	if inherits.Tags["labels"] != noneTag {
		t.Errorf("a job with no labels tagged %q", inherits.Tags["labels"])
	}
	own := find(t, points, "gh_workflow_job", map[string]string{"job_name": "own"})
	if own.Fields["head_sha"] != "ffffffffffffffffffffffffffffffffffffffff" {
		t.Errorf("a job with its own commit got %v", own.Fields["head_sha"])
	}
	if got := fieldInt(t, own, "queued_seconds"); got != 30 {
		t.Errorf("queued_seconds = %d, want 30", got)
	}
	steps := only(t, points, "gh_workflow_step")
	if len(steps) != 1 || steps[0].Tags["step"] != "whole" {
		t.Errorf("got steps %v, want only the one that finished", steps)
	}
}

// TestActionsRunListThatFailsFailsTheCollection keeps a broken listing apart
// from an empty one: the first is an error with nothing written, the second
// still publishes the declared total.
func TestActionsRunListThatFailsFailsTheCollection(t *testing.T) {
	t.Parallel()
	t.Run("broken", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/repos/octocat/hello-world/actions/runs", http.StatusInternalServerError, "boom")
		points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err == nil || points != nil {
			t.Fatalf("a broken run list gave %d points and %v", len(points), err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"total_count": 7, "workflow_runs": []}`))
		})
		points, err := Actions{Walk: Walk{Pages: 3, Since: testNow.Add(-time.Hour)}}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls("/repos/octocat/hello-world/actions/runs")); n != 1 {
			t.Errorf("an empty page was followed by %d more requests", n-1)
		}
		if total := only(t, points, "gh_workflow_run_total")[0]; fieldInt(t, total, "runs") != 7 {
			t.Errorf("run total = %v, want the declared 7", total.Fields)
		}
	})
}

// TestArtifactsGiveARetentionOnlyWhenBothDatesAreKnown reads an artifact
// whose expiry GitHub left out. A retention computed from a zero date would
// be a negative number of days that a policy panel reads as a violation.
func TestArtifactsGiveARetentionOnlyWhenBothDatesAreKnown(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/actions/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count": 2, "artifacts": [
		  {"name": "undated", "size_in_bytes": 10, "expired": false, "created_at": "2026-09-07T10:03:00Z", "expires_at": null, "workflow_run": {"id": 5}},
		  {"name": "dated", "size_in_bytes": 10, "expired": false, "created_at": "2026-09-07T10:03:00Z", "expires_at": "2026-09-08T10:02:31Z", "workflow_run": {"id": 5}}
		]}`))
	})
	points, err := Artifacts{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if undated := find(t, points, "gh_artifact", map[string]string{"artifact": "undated"}); hasField(undated, "retention_days") {
		t.Errorf("an artifact without an expiry has retention_days %v", undated.Fields["retention_days"])
	}
	if dated := find(t, points, "gh_artifact", map[string]string{"artifact": "dated"}); fieldInt(t, dated, "retention_days") != 1 {
		t.Errorf("retention_days = %v, want 1", dated.Fields["retention_days"])
	}
}

// TestArtifactsReportABrokenListing is the other side of a disabled feature:
// a 500 is not "no artifacts".
func TestArtifactsReportABrokenListing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/actions/artifacts", http.StatusInternalServerError, "boom")
	if _, err := (Artifacts{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
		t.Fatal("a broken artifact listing was not reported")
	}
}

// TestArtifactsStopWalkingOnceAFullPageIsOlderThanTheBound serves full pages
// for ever: the walk must stop at the first one whose oldest artifact is past
// the bound instead of reading the whole history.
func TestArtifactsStopWalkingOnceAFullPageIsOlderThanTheBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/actions/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(artifactPage(t, 100, 1000, 1))
	})
	_, err := Artifacts{Walk: Walk{Pages: 5, Since: testNow}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/artifacts")); n != 1 {
		t.Errorf("made %d artifact requests, want 1: the first page is already past the bound", n)
	}
}

// --- deployments.go -----------------------------------------------------

// TestAliasRetryGivesUpOnlyWhenThereIsNothingSmallerToAsk pins what each
// failure of a batch leads to. A single repository the gateway gives up on
// cannot be halved, and treating it as recoverable would drop its error.
func TestAliasRetryGivesUpOnlyWhenThereIsNothingSmallerToAsk(t *testing.T) {
	t.Parallel()
	tooLarge := &ghapi.TooLargeError{Status: http.StatusBadGateway}
	gone := errors.New("graphql: NOT_FOUND: Could not resolve to a Repository")
	broken := errors.New("graphql: INTERNAL: something went wrong")
	cases := []struct {
		name      string
		err       error
		batch     int
		retryAt   int
		recovered bool
	}{
		{"too large, several", tooLarge, 4, 2, true},
		{"too large, one", tooLarge, 1, 0, false},
		{"gone, several", gone, 3, 1, true},
		{"gone, one", gone, 1, 0, true},
		{"broken", broken, 3, 0, false},
	}
	for _, tc := range cases {
		retryAt, recovered := aliasRetry(tc.err, tc.batch)
		if retryAt != tc.retryAt || recovered != tc.recovered {
			t.Errorf("%s: aliasRetry = (%d, %t), want (%d, %t)", tc.name, retryAt, recovered, tc.retryAt, tc.recovered)
		}
	}
}

// TestDecodeAliasesEmitsOnlyTheRepositoriesThatAnswered hands decodeAliases
// the three ways an alias can fail to be a repository: null, missing, and
// not the shape asked for. Every caller would turn an emitted zero value into
// a row of zeros.
func TestDecodeAliasesEmitsOnlyTheRepositoriesThatAnswered(t *testing.T) {
	t.Parallel()
	batch := []Repo{
		{FullName: "octocat/null"},
		{FullName: "octocat/garbled"},
		{FullName: "octocat/answered"},
		{FullName: "octocat/missing"},
	}
	res := map[string]json.RawMessage{
		"r0": json.RawMessage(`null`),
		"r1": json.RawMessage(`"not an object"`),
		"r2": json.RawMessage(`{"name": "answered"}`),
	}
	var emitted []string
	decodeAliases(batch, res, func(repo Repo, node struct {
		Name string `json:"name"`
	},
	) {
		emitted = append(emitted, repo.FullName+"="+node.Name)
	})
	if len(emitted) != 1 || emitted[0] != "octocat/answered=answered" {
		t.Errorf("emitted %v, want only the repository that answered", emitted)
	}
}

// activityDeploymentsAnswer is one alias of a deployments query.
func activityDeploymentsAnswer(next bool, nodes ...string) string {
	return fmt.Sprintf(`{"deployments": {"totalCount": %d, "pageInfo": {"hasNextPage": %t, "endCursor": "cursor"}, "nodes": [%s]}}`,
		len(nodes), next, strings.Join(nodes, ","))
}

const activityDeploymentNode = `{"databaseId": 11, "createdAt": "2026-09-07T10:00:00Z", "environment": "production",
  "state": "ACTIVE", "task": "deploy", "latestStatus": null, "creator": {"login": "octocat"},
  "commit": {"oid": "abc"}, "ref": {"name": "main"}}`

// TestDeploymentsAskTheirDefaultPageOrTheOneConfigured reads the page size off
// the query text, since that is all the gateway sees of it.
func TestDeploymentsAskTheirDefaultPageOrTheOneConfigured(t *testing.T) {
	t.Parallel()
	for first, want := range map[int]string{0: "first: 100,", -3: "first: 100,", 7: "first: 7,"} {
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
			_, _ = fmt.Fprintf(w, `{"data": {"r0": %s}}`, activityDeploymentsAnswer(false))
		})
		if _, err := (Deployments{Repos: deployRepos[:1], First: first}).Collect(ctx(t), f.Client, testNow); err != nil {
			t.Fatal(err)
		}
		if q := activityGraphQLQueries(t, f); len(q) != 1 || !strings.Contains(q[0], want) {
			t.Errorf("First %d asked %q, want it to contain %q", first, q, want)
		}
	}
}

// TestDeploymentsDoNotFollowANextPageThatHasNoDeployments guards the index
// into the last deployment of a page: a page that says there is more but
// holds nothing has no last deployment to date the walk by.
func TestDeploymentsDoNotFollowANextPageThatHasNoDeployments(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = fmt.Fprintf(w, `{"data": {"r0": %s}}`, activityDeploymentsAnswer(true))
	})
	points, err := Deployments{Repos: deployRepos[:1], Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Fatalf("got %d points and %v", len(points), err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("made %d queries, want 1", n)
	}
}

// activityOneRepoAtATime answers a deployments query per repository: gone
// names a repository that answers NOT_FOUND, broken one that answers an
// error nothing can skip, and a batch holding more than one repository is
// answered NOT_FOUND so it has to be split.
func activityOneRepoAtATime(t *testing.T, f *fixtureServer, gone, broken string) {
	t.Helper()
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if activityRunawayGraphQL(w, f, 5) {
			return
		}
		switch {
		case strings.Count(query, "repository(") > 1:
			_, _ = w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"one of them is gone"}]}`))
		case gone != "" && strings.Contains(query, fmt.Sprintf("name: %q", gone)):
			_, _ = w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"gone"}]}`))
		case broken != "" && strings.Contains(query, fmt.Sprintf("name: %q", broken)):
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
		default:
			_, _ = fmt.Fprintf(w, `{"data": {"r0": %s}}`, activityDeploymentsAnswer(false, activityDeploymentNode))
		}
	})
}

// TestDeploymentsSplitABatchThatHoldsAGoneRepository asks for two
// repositories, one renamed away. The batch fails as a whole, and only asking
// each on its own keeps the deployments of the one still there.
func TestDeploymentsSplitABatchThatHoldsAGoneRepository(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	activityOneRepoAtATime(t, f, "gitlab-mcp-server", "")
	points, err := Deployments{Repos: deployRepos, Batch: 2}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Tags["full_name"] != "jmrplens/phonometry" {
		t.Errorf("got %v, want the one deployment of the repository still there", points)
	}
	if n := len(f.calls("/graphql")); n != 3 {
		t.Errorf("made %d queries, want the batch and one per repository", n)
	}
}

// TestDeploymentsAskAGoneRepositoryOnlyOnce is the end of that split: a
// single repository that is gone cannot be split further, and asking it again
// would never stop.
func TestDeploymentsAskAGoneRepositoryOnlyOnce(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	activityOneRepoAtATime(t, f, "phonometry", "")
	points, err := Deployments{Repos: deployRepos[:1]}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Fatalf("a gone repository gave %d points and %v, want nothing and no error", len(points), err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("made %d queries, want 1", n)
	}
}

// TestDeploymentsKeepWhatAnsweredAndStillReportTheOneThatFailed keeps what a
// sweep did collect when one repository fails, and reports the failure
// whether or not the others answered.
func TestDeploymentsKeepWhatAnsweredAndStillReportTheOneThatFailed(t *testing.T) {
	t.Parallel()
	t.Run("one of two fails", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		activityOneRepoAtATime(t, f, "", "gitlab-mcp-server")
		points, err := Deployments{Repos: deployRepos, Batch: 1}.Collect(ctx(t), f.Client, testNow)
		if len(points) != 1 {
			t.Fatalf("got %d points, want the working repository's one kept", len(points))
		}
		if err == nil {
			t.Error("one repository failed and the collector reported success")
		}
	})
	t.Run("the only one fails", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		activityOneRepoAtATime(t, f, "", "phonometry")
		points, err := Deployments{Repos: deployRepos[:1], Batch: 1}.Collect(ctx(t), f.Client, testNow)
		if err == nil {
			t.Fatalf("a sweep that collected nothing reported success with %d points", len(points))
		}
	})
}

// activityDeploymentPoint decodes one deployment as the gateway sends it and
// renders its point.
func activityDeploymentPoint(t *testing.T, raw string) sink.Point {
	t.Helper()
	var n deploymentNode
	mustUnmarshal(t, []byte(raw), &n)
	return n.point(testRepo, activityBase)
}

// TestADeploymentWithoutAStatusOrADateHasNoDuration reads the deployments a
// status never reached, and one whose own creation date is missing: neither
// has two moments to subtract.
func TestADeploymentWithoutAStatusOrADateHasNoDuration(t *testing.T) {
	t.Parallel()
	bare := activityDeploymentPoint(t, `{"databaseId": 1, "createdAt": "2026-09-07T10:00:00Z", "state": ""}`)
	if hasField(bare, "seconds_to_status") || hasField(bare, "seconds_live") {
		t.Errorf("a deployment without a status has a duration: %v", bare.Fields)
	}
	if hasField(bare, "commit") || hasField(bare, "run_id") {
		t.Errorf("a deployment without a commit or a log has %v", bare.Fields)
	}
	if bare.Fields["outcome"] != noneTag {
		t.Errorf("outcome = %v, want %q for a deployment without any state", bare.Fields["outcome"], noneTag)
	}

	undated := activityDeploymentPoint(t, `{"databaseId": 2, "state": "",
	  "latestStatus": {"state": "FAILURE", "createdAt": "2026-09-07T10:00:33Z", "logUrl": "https://github.com/octocat/hello-world/deployments"}}`)
	if hasField(undated, "seconds_to_status") || hasField(undated, "seconds_live") {
		t.Errorf("a deployment with no creation date has a duration: %v", undated.Fields)
	}
	// The deployment's own state is empty, so the status names it.
	if undated.Fields["deployment_state"] != "failure" || undated.Fields["outcome"] != "failure" {
		t.Errorf("state/outcome = %v/%v, want the status's failure", undated.Fields["deployment_state"], undated.Fields["outcome"])
	}
	if hasField(undated, "run_id") {
		t.Errorf("a log URL that is not a run gave run_id %v", undated.Fields["run_id"])
	}
}

// TestDeploymentOutcomeNamesEveryStateGitHubHas walks the state enum, since a
// state that falls through to the default is published under its raw name.
func TestDeploymentOutcomeNamesEveryStateGitHubHas(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{
		"ACTIVE": "success", "INACTIVE": "success", "success": "success",
		"FAILURE": "failure", "error": "error",
		"PENDING": "pending", "queued": "pending", "IN_PROGRESS": "pending", "WAITING": "pending",
		"": noneTag, "DESTROYED": "destroyed",
	} {
		if got := deploymentOutcome(state); got != want {
			t.Errorf("deploymentOutcome(%q) = %q, want %q", state, got, want)
		}
	}
}

// TestRunIDReadsARelativeLogURLAndRejectsANonNumericOne covers the two edges
// of the parse: the marker at the very start, and a run segment that is not a
// number.
func TestRunIDReadsARelativeLogURLAndRejectsANonNumericOne(t *testing.T) {
	t.Parallel()
	if got := runID("/actions/runs/42"); got != 42 {
		t.Errorf("runID of a relative URL = %d, want 42", got)
	}
	if got := runID("https://github.com/octocat/hello-world/actions/runs/latest/job/1"); got != 0 {
		t.Errorf("runID of a non-numeric run = %d, want 0", got)
	}
}

// --- branches.go --------------------------------------------------------

// activityBranchInventory is one repository's answer to the branch query.
const activityBranchInventory = `{"defaultBranchRef": {"name": "main"}, "refs": {"totalCount": 2, "nodes": [
  {"name": "main", "target": {"oid": "abc", "committedDate": "2026-09-01T00:00:00Z"}},
  {"name": "", "target": {"oid": "def"}}
]}}`

// TestBranchesAskOneQueryForABatchThatFitsExactly asks for three repositories
// with a batch of three, and answers one of them null and one with something
// that is not an inventory. A batch that fits exactly must not be followed by
// an empty query, and neither odd alias may become a row.
func TestBranchesAskOneQueryForABatchThatFitsExactly(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = fmt.Fprintf(w, `{"data": {"r0": %s, "r1": null, "r2": "garbled"}}`, activityBranchInventory)
	})
	repos := []Repo{testRepo, {Owner: "octocat", Name: "gone", FullName: "octocat/gone"}, {Owner: "octocat", Name: "odd", FullName: "octocat/odd"}}
	points, err := Branches{Repos: repos, Batch: 3}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("made %d queries for one batch, want 1", n)
	}
	if len(points) != 1 || points[0].Tags["branch"] != "main" || points[0].Tags["is_default"] != "true" {
		t.Errorf("got %v, want the one named branch of the one repository that answered", points)
	}
}

// TestBranchesKeepWhatAnsweredAndStillReportTheOneThatFailed is the branch
// twin of the deployments rule: one broken repository does not cost the
// others, and it is still reported.
func TestBranchesKeepWhatAnsweredAndStillReportTheOneThatFailed(t *testing.T) {
	t.Parallel()
	serve := func(f *fixtureServer) {
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
			if strings.Contains(query, `name: "broken"`) {
				_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"data": {"r0": %s}}`, activityBranchInventory)
		})
	}
	broken := Repo{Owner: "octocat", Name: "broken", FullName: "octocat/broken"}
	t.Run("one of two fails", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		serve(f)
		points, err := Branches{Repos: []Repo{testRepo, broken}, Batch: 1}.Collect(ctx(t), f.Client, testNow)
		if len(points) != 1 {
			t.Fatalf("got %d points, want the working repository's branch kept", len(points))
		}
		if err == nil {
			t.Error("one repository failed and the collector reported success")
		}
	})
	t.Run("the only one fails", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		serve(f)
		points, err := Branches{Repos: []Repo{broken}, Batch: 1}.Collect(ctx(t), f.Client, testNow)
		if err == nil {
			t.Fatalf("a sweep that collected nothing reported success with %d points", len(points))
		}
	})
}

// --- commits.go ---------------------------------------------------------

// activityCommitsAnswer is one page of the default branch's history.
func activityCommitsAnswer(next bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"data": {"repository": {"defaultBranchRef": {"name": "main", "target": {"history": {
	  "totalCount": 9, "pageInfo": {"hasNextPage": %t, "endCursor": %q}, "nodes": [%s]}}}}}}`,
		next, cursor, strings.Join(nodes, ","))
}

// activityCommitNode is a commit by an author GitHub matched to no login.
const activityCommitNode = `{"oid": "0123456789abcdef0123", "url": "https://github.com/octocat/hello-world/commit/0123456789abcdef0123",
  "committedDate": "2026-09-01T00:00:00Z", "additions": 1, "deletions": 2, "changedFilesIfAvailable": 1,
  "messageHeadline": "fix", "author": {"name": "Octo Cat", "user": {"login": ""}}, "signature": null,
  "associatedPullRequests": {"nodes": []},
  "statusCheckRollup": {"state": "SUCCESS", "contexts": {"totalCount": 0, "nodes": []}}}`

// TestCommitsAskForAPageSmallerThanTheCap sends First thirty, which is inside
// both caps and must reach the gateway unchanged.
func TestCommitsAskForAPageSmallerThanTheCap(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(activityCommitsAnswer(false, "")))
	})
	if _, err := (Commits{First: 30}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if vars := activityGraphQLVars(t, f); len(vars) != 1 || vars[0]["first"] != float64(30) {
		t.Errorf("asked %v, want first 30", vars)
	}
}

// TestCommitsStopAtAPageAlreadyPastTheWalkBound serves a page of commits
// older than the bound that still says there is more: the walk ends there.
// It also reads the commit itself, whose author has a name but no login and
// whose checks all passed.
func TestCommitsStopAtAPageAlreadyPastTheWalkBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		if activityRunawayGraphQL(w, f, 3) {
			return
		}
		_, _ = w.Write([]byte(activityCommitsAnswer(true, "more", activityCommitNode)))
	})
	points, err := Commits{Walk: Walk{Pages: 3, Since: testNow.Add(-24 * time.Hour)}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("made %d queries, want 1: the first page is already older than the bound", n)
	}
	commit := only(t, points, "gh_commit")[0]
	if commit.Tags["author"] != "Octo Cat" {
		t.Errorf("author = %q, want the name when there is no login", commit.Tags["author"])
	}
	if fieldInt(t, commit, "checks_failed") != 0 {
		t.Errorf("a passing rollup has checks_failed %v", commit.Fields["checks_failed"])
	}
}

// TestCommitsFollowAnEmptyPageThatSaysThereIsMore serves an empty page with a
// cursor and then a page with a commit: the walk has no commit to date on the
// first page, so it cannot stop there and must not index into it.
func TestCommitsFollowAnEmptyPageThatSaysThereIsMore(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		if activityRunawayGraphQL(w, f, 3) {
			return
		}
		if vars["after"] == "c1" {
			_, _ = w.Write([]byte(activityCommitsAnswer(false, "", activityCommitNode)))
			return
		}
		_, _ = w.Write([]byte(activityCommitsAnswer(true, "c1")))
	})
	points, err := Commits{Walk: Walk{Pages: 2}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/graphql")); n != 2 {
		t.Errorf("made %d queries, want 2", n)
	}
	if n := len(byMeasurement(points)["gh_commit"]); n != 1 {
		t.Errorf("got %d commits, want the one on the second page", n)
	}
}

// TestCommitChecksCountErrorsAsFailuresAndNothingElse reads a rollup of every
// check shape: a passing and an erroring check run from other apps, a check
// run that names no app, and a status with no date. Only an error or a
// failure counts as failed, a check without an app is not attributable, and
// an undated check is stamped with its commit.
func TestCommitChecksCountErrorsAsFailuresAndNothingElse(t *testing.T) {
	t.Parallel()
	var rollup checkRollup
	mustUnmarshal(t, []byte(`{"state": "FAILURE", "contexts": {"totalCount": 4, "nodes": [
	  {"__typename": "CheckRun", "name": "circle", "conclusion": "SUCCESS", "completedAt": "2026-09-01T00:05:00Z", "detailsUrl": "d1", "checkSuite": {"app": {"slug": "circleci"}}},
	  {"__typename": "CheckRun", "name": "sonar", "conclusion": "ERROR", "completedAt": null, "detailsUrl": "d2", "checkSuite": {"app": {"slug": "sonarcloud"}}},
	  {"__typename": "CheckRun", "name": "orphan", "conclusion": "FAILURE", "checkSuite": {"app": null}},
	  {"__typename": "StatusContext", "context": "jenkins", "state": "PENDING", "createdAt": null, "targetUrl": "t"}
	]}}`), &rollup)
	committed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := commitCheckPoints(&rollup, activityBase, "0123456789ab", committed)
	checkPoints(t, points)
	if len(points) != 3 {
		t.Fatalf("got %d checks, want three: the one without an app is not attributable", len(points))
	}
	circle := find(t, points, "gh_commit_check", map[string]string{"check": "circle"})
	if fieldInt(t, circle, "failed") != 0 || !circle.Time.Equal(committed.Add(5*time.Minute)) {
		t.Errorf("circle failed=%v at %s", circle.Fields["failed"], circle.Time)
	}
	sonar := find(t, points, "gh_commit_check", map[string]string{"check": "sonar"})
	if fieldInt(t, sonar, "failed") != 1 || !sonar.Time.Equal(committed) {
		t.Errorf("sonar failed=%v at %s, want an error counted and the commit's date", sonar.Fields["failed"], sonar.Time)
	}
	jenkins := find(t, points, "gh_commit_check", map[string]string{"check": "jenkins", "app": "status"})
	if fieldInt(t, jenkins, "failed") != 0 || !jenkins.Time.Equal(committed) {
		t.Errorf("jenkins failed=%v at %s", jenkins.Fields["failed"], jenkins.Time)
	}
}

// --- events.go ----------------------------------------------------------

// TestEventPayloadsCountCommitsFromWhicheverFieldCarriesThem reads the payload
// shapes of the event feed: a push that declares its size, one that only
// lists its commits, an event with neither, and a payload that is not an
// object.
func TestEventPayloadsCountCommitsFromWhicheverFieldCarriesThem(t *testing.T) {
	t.Parallel()
	point := func(payload string) sink.Point {
		return eventPoint(&eventRow{Type: "PushEvent", CreatedAt: testNow, Payload: json.RawMessage(payload)})
	}
	if got := point(`{"size": 5, "commits": [{}]}`); fieldInt(t, got, "commits") != 5 {
		t.Errorf("a declared size gave commits %v, want 5", got.Fields["commits"])
	}
	if got := point(`{"size": 0, "commits": [{"message": "a"}, {"message": "b"}]}`); fieldInt(t, got, "commits") != 2 {
		t.Errorf("a commit list gave commits %v, want 2", got.Fields["commits"])
	}
	if got := point(`{"action": "opened"}`); hasField(got, "commits") || got.Tags["action"] != "opened" {
		t.Errorf("an event without commits gave %v %v", got.Tags, got.Fields)
	}
	if got := point(`"not an object"`); got.Tags["action"] != "" {
		t.Errorf("an unreadable payload still tagged action %q", got.Tags["action"])
	}
}

// TestEventsWithNothingToReadAreNotAnError covers an account whose feed is
// hidden and one whose feed is empty.
func TestEventsWithNothingToReadAreNotAnError(t *testing.T) {
	t.Parallel()
	hidden := newFixtureServer(t)
	e := &Events{Login: "octocat"}
	if points, err := e.Collect(ctx(t), hidden.Client, testNow); err != nil || len(points) != 0 {
		t.Errorf("a hidden feed gave %d points and %v", len(points), err)
	}
	empty := newFixtureServer(t)
	empty.handle("/users/octocat/events", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	if points, err := e.Collect(ctx(t), empty.Client, testNow); err != nil || len(points) != 0 {
		t.Errorf("an empty feed gave %d points and %v", len(points), err)
	}
	if n := len(empty.calls("/users/octocat/events")); n != 1 {
		t.Errorf("an empty page was followed by %d more requests", n-1)
	}
}

// activityNotificationPage is n notifications, newest first, a minute apart.
func activityNotificationPage(t *testing.T, n int, newest time.Time) []byte {
	t.Helper()
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{
			"reason": "mention", "unread": true,
			"updated_at": activityStamp(newest.Add(-time.Duration(i) * time.Minute)),
			"repository": map[string]any{"full_name": "octocat/hello-world"},
			"subject": map[string]any{
				"title": "Flaky test", "type": "Issue",
				"url": "https://api.github.com/repos/octocat/hello-world/issues/9",
			},
		}
	}
	return mustMarshal(t, rows)
}

// activityEndlessNotifications serves full pages of fifty for ever.
func activityEndlessNotifications(t *testing.T, f *fixtureServer) {
	t.Helper()
	f.handle("/notifications", func(w http.ResponseWriter, _ *http.Request) {
		if activityRunawayREST(w, f, "/notifications", 6) {
			return
		}
		_, _ = w.Write(activityNotificationPage(t, 50, testNow.Add(-time.Hour)))
	})
}

// TestNotificationsReadExactlyThePagesAsked serves full pages for ever and
// holds each configuration to its page count: Pages alone, and a Walk that
// overrides it.
func TestNotificationsReadExactlyThePagesAsked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		n         Notifications
		wantCalls int
	}{
		{"pages alone", Notifications{Pages: 1}, 1},
		{"walk overrides pages", Notifications{Pages: 5, Walk: Walk{Pages: 2}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			activityEndlessNotifications(t, f)
			n := tc.n
			points, err := n.Collect(ctx(t), f.Client, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(f.calls("/notifications")); got != tc.wantCalls {
				t.Errorf("made %d requests, want %d", got, tc.wantCalls)
			}
			if len(points) != 50*tc.wantCalls {
				t.Errorf("got %d notifications, want %d", len(points), 50*tc.wantCalls)
			}
		})
	}
}

// TestNotificationsStopAtAFullPagePastTheBound serves full pages that are all
// older than the walk's bound: one page is enough to know it.
func TestNotificationsStopAtAFullPagePastTheBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	activityEndlessNotifications(t, f)
	n := Notifications{Walk: Walk{Pages: 3, Since: testNow.Add(-70 * time.Minute)}}
	if _, err := n.Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if got := len(f.calls("/notifications")); got != 1 {
		t.Errorf("made %d requests, want 1", got)
	}
}

// TestNotificationsWithNothingToReadAreNotAnError covers a token without the
// notifications scope and an empty inbox, and a notification with no page to
// link to.
func TestNotificationsWithNothingToReadAreNotAnError(t *testing.T) {
	t.Parallel()
	denied := newFixtureServer(t)
	if points, err := (&Notifications{}).Collect(ctx(t), denied.Client, testNow); err != nil || len(points) != 0 {
		t.Errorf("a denied inbox gave %d points and %v", len(points), err)
	}
	empty := newFixtureServer(t)
	empty.handle("/notifications", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	if points, err := (&Notifications{Pages: 3}).Collect(ctx(t), empty.Client, testNow); err != nil || len(points) != 0 {
		t.Errorf("an empty inbox gave %d points and %v", len(points), err)
	}
	if n := len(empty.calls("/notifications")); n != 1 {
		t.Errorf("an empty page was followed by %d more requests", n-1)
	}
	if fields := notificationFields("title", ""); fields["url"] != nil {
		t.Errorf("a notification with no page carries url %v", fields["url"])
	}
}

// --- issueevents.go -----------------------------------------------------

// TestRestEventNameSplitsEveryInnerCapital reads two timeline type names: one
// capital at the start, which is not a word boundary, and an inner A, which
// is.
func TestRestEventNameSplitsEveryInnerCapital(t *testing.T) {
	t.Parallel()
	for typename, want := range map[string]string{
		"AssignedEvent":       "assigned",
		"IssueTypeAddedEvent": "issue_type_added",
		"ZeroEvent":           "zero",
	} {
		if got := restEventName(typename); got != want {
			t.Errorf("restEventName(%q) = %q, want %q", typename, got, want)
		}
	}
}

// TestSnakeCaseSplitsAtBothEndsOfTheCapitals reads the two capitals that
// bound the range, each inside a name. No timeline type GitHub has today holds
// an inner Z, so only this test says a type that does would not be glued
// into one word, both on the list and as an item type enum value.
func TestSnakeCaseSplitsAtBothEndsOfTheCapitals(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"MarkedAsDuplicate": "marked_as_duplicate",
		"MovedToZone":       "moved_to_zone",
		"SizeZ":             "size_z",
		"lowerStart":        "lower_start",
		"nothing@[]":        "nothing@[]",
	} {
		if got := snakeCase(name); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", name, got, want)
		}
	}
	if got := timelineItemType("MovedToZoneEvent"); got != "MOVED_TO_ZONE_EVENT" {
		t.Errorf("timelineItemType(MovedToZoneEvent) = %q, want MOVED_TO_ZONE_EVENT", got)
	}
}

// activityTimelineRow decodes one timeline item and turns it into the REST row.
func activityTimelineRow(t *testing.T, raw string) *issueEventRow {
	t.Helper()
	var it timelineItem
	mustUnmarshal(t, []byte(raw), &it)
	return it.row(&issueEventIssue{Number: 7})
}

// TestTimelineItemsTakeTheirCommitFromWhicheverFieldNamesOne covers the three
// fields a timeline item can name a commit in, and an item that names none.
func TestTimelineItemsTakeTheirCommitFromWhicheverFieldNamesOne(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		`{"__typename": "MergedEvent", "commit": {"oid": "c1"}}`:                  "c1",
		`{"__typename": "HeadRefForcePushedEvent", "afterCommit": {"oid": "c2"}}`: "c2",
		`{"__typename": "ClosedEvent", "closer": {"oid": "c3"}}`:                  "c3",
		`{"__typename": "ReopenedEvent"}`:                                         "",
	} {
		if r := activityTimelineRow(t, raw); r == nil || r.CommitID != want {
			t.Errorf("%s gave %+v, want commit %q", raw, r, want)
		}
	}
}

// TestTimelineItemsOfAnUnknownTypeOrWithoutPeopleStillRead covers a type the
// table does not know, which is dropped rather than guessed at, and a removed
// review request whose requester and reviewer are both gone.
func TestTimelineItemsOfAnUnknownTypeOrWithoutPeopleStillRead(t *testing.T) {
	t.Parallel()
	if r := activityTimelineRow(t, `{"__typename": "SomethingNewEvent"}`); r != nil {
		t.Errorf("an unknown type gave %+v, want nothing", r)
	}
	r := activityTimelineRow(t, `{"__typename": "ReviewRequestRemovedEvent", "createdAt": "2026-09-07T10:00:00Z", "actor": null, "requestedReviewer": null}`)
	if r == nil || r.Event != "review_request_removed" {
		t.Fatalf("got %+v", r)
	}
	if r.Actor != nil || r.RequestedReviewer != nil || r.ReviewRequester != nil {
		t.Errorf("a request with nobody left names %+v %+v %+v", r.Actor, r.RequestedReviewer, r.ReviewRequester)
	}
	var gone *timelineActor
	if got := gone.restLogin(); got != "" {
		t.Errorf("a missing actor logs in as %q", got)
	}
}

// TestIssueEventPointsWithoutAnIssueCarryNoTitleOrLink renders an event that
// names no issue, and one that names an issue with a title and a page.
func TestIssueEventPointsWithoutAnIssueCarryNoTitleOrLink(t *testing.T) {
	t.Parallel()
	bare := issueEventPoint(activityBase, &issueEventRow{Event: "labeled", CreatedAt: testNow})
	if hasField(bare, "title") || hasField(bare, "url") || fieldInt(t, bare, "number") != 0 || bare.Tags["kind"] != "issue" {
		t.Errorf("an event without an issue gave %v %v", bare.Tags, bare.Fields)
	}
	named := issueEventPoint(activityBase, &issueEventRow{
		Event: "labeled", CreatedAt: testNow,
		Issue: &issueEventIssue{Number: 3, Title: "Cannot bind", HTMLURL: "https://github.com/octocat/hello-world/issues/3"},
	})
	if named.Fields["title"] != "Cannot bind" || named.Fields["url"] != "https://github.com/octocat/hello-world/issues/3" {
		t.Errorf("an event on an issue gave %v", named.Fields)
	}
}

// TestTimelineConnectionHasMoreOnlyWithANodeInsideTheBound walks the three
// conditions of more: a next page, a node to date it by, and that node inside
// the bound.
func TestTimelineConnectionHasMoreOnlyWithANodeInsideTheBound(t *testing.T) {
	t.Parallel()
	bound := Walk{Since: testNow.Add(-24 * time.Hour)}
	node := func(at time.Time) timelineNode { return timelineNode{UpdatedAt: at} }
	cases := []struct {
		name string
		tc   timelineConnection
		want bool
	}{
		{"next and recent", timelineConnection{PageInfo: pageInfo{HasNextPage: true}, Nodes: []timelineNode{node(testNow.Add(-48 * time.Hour)), node(testNow)}}, true},
		{"next and old", timelineConnection{PageInfo: pageInfo{HasNextPage: true}, Nodes: []timelineNode{node(testNow), node(testNow.Add(-48 * time.Hour))}}, false},
		{"next and empty", timelineConnection{PageInfo: pageInfo{HasNextPage: true}}, false},
		{"no next", timelineConnection{Nodes: []timelineNode{node(testNow)}}, false},
	}
	for _, tc := range cases {
		if got := tc.tc.more(bound); got != tc.want {
			t.Errorf("%s: more = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// activityTimelineNode is one issue or pull request with its timeline.
func activityTimelineNode(number int, items ...string) string {
	return fmt.Sprintf(`{"number": %d, "title": "item %d", "url": "https://github.com/octocat/hello-world/issues/%d",
	  "updatedAt": %q, "timelineItems": {"pageInfo": {"hasNextPage": false}, "nodes": [%s]}}`,
		number, number, number, activityStamp(testNow.Add(-time.Hour)), strings.Join(items, ","))
}

// activityLabeled is a label added an hour and a half ago.
var activityLabeled = fmt.Sprintf(`{"__typename": "LabeledEvent", "createdAt": %q, "actor": {"login": "octocat", "__typename": "User"}, "label": {"name": "bug"}}`,
	activityStamp(testNow.Add(-90*time.Minute)))

// activityTimelineConnection is one page of issues or pull requests.
func activityTimelineConnection(next bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"pageInfo": {"hasNextPage": %t, "endCursor": %q}, "nodes": [%s]}`, next, cursor, strings.Join(nodes, ","))
}

// activityTimelineAnswer answers only the connections the query included.
func activityTimelineAnswer(w http.ResponseWriter, vars map[string]any, issues, pulls string) {
	parts := []string{}
	if vars["withIssues"] == true {
		parts = append(parts, `"issues": `+issues)
	}
	if vars["withPRs"] == true {
		parts = append(parts, `"pullRequests": `+pulls)
	}
	_, _ = fmt.Fprintf(w, `{"data": {"repository": {%s}}}`, strings.Join(parts, ", "))
}

// TestIssueEventsDefaultToTheLastThirtyDays reads the since a sweep with no
// window sends.
func TestIssueEventsDefaultToTheLastThirtyDays(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		activityTimelineAnswer(w, vars, activityTimelineConnection(false, ""), activityTimelineConnection(false, ""))
	})
	if _, err := (IssueEvents{}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	want := activityStamp(testNow.AddDate(0, 0, -30))
	if vars := activityGraphQLVars(t, f); len(vars) != 1 || vars[0]["since"] != want {
		t.Errorf("asked %v, want since %s", vars, want)
	}
}

// TestIssueEventsKeepWalkingPullRequestsAfterTheIssuesRunOut serves a first
// page where the issues end and the pull requests go on. The second query
// must ask for pull requests alone, from their cursor, and every item of
// both pages must be read, both issues of the first page included.
func TestIssueEventsKeepWalkingPullRequestsAfterTheIssuesRunOut(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		if activityRunawayGraphQL(w, f, 4) {
			return
		}
		if vars["prAfter"] == "p1" {
			activityTimelineAnswer(w, vars, activityTimelineConnection(false, ""),
				activityTimelineConnection(false, "p2", activityTimelineNode(3, activityLabeled)))
			return
		}
		activityTimelineAnswer(w, vars,
			activityTimelineConnection(false, "i1", activityTimelineNode(1, activityLabeled), activityTimelineNode(4, activityLabeled)),
			activityTimelineConnection(true, "p1", activityTimelineNode(2, activityLabeled)))
	})
	points, err := IssueEvents{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	vars := activityGraphQLVars(t, f)
	if len(vars) != 2 {
		t.Fatalf("made %d queries, want 2", len(vars))
	}
	if vars[1]["withIssues"] != false || vars[1]["withPRs"] != true || vars[1]["prAfter"] != "p1" {
		t.Errorf("second query asked %v, want pull requests alone from p1", vars[1])
	}
	seen := map[string]bool{}
	for _, p := range only(t, points, "gh_issue_event") {
		seen[fmt.Sprint(p.Fields["number"])] = true
	}
	for _, number := range []string{"1", "2", "3", "4"} {
		if !seen[number] {
			t.Errorf("item %s was not read; read %v", number, seen)
		}
	}
}

// TestIssueEventsHistoryReadsExactlyThePagesAsked walks a backfill of two
// pages over connections that never end.
func TestIssueEventsHistoryReadsExactlyThePagesAsked(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		if activityRunawayGraphQL(w, f, 4) {
			return
		}
		activityTimelineAnswer(w, vars,
			activityTimelineConnection(true, "i", activityTimelineNode(1)),
			activityTimelineConnection(true, "p", activityTimelineNode(2)))
	})
	for _, number := range []string{"1", "2"} {
		f.handle("/repos/octocat/hello-world/issues/"+number+"/events", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		})
	}
	if _, err := (IssueEvents{Walk: Walk{Pages: 2}}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/graphql")); n != 2 {
		t.Errorf("made %d queries, want the 2 the walk allows", n)
	}
}

// TestIssueEventsReadTheListForATruncatedTimeline sends an issue whose
// timeline has more than the query carried, so its events come from the
// REST list: the ones before the window are dropped, and a list that fails
// fails the collection.
func TestIssueEventsReadTheListForATruncatedTimeline(t *testing.T) {
	t.Parallel()
	truncated := fmt.Sprintf(`{"number": 5, "title": "busy", "url": "https://github.com/octocat/hello-world/issues/5",
	  "updatedAt": %q, "timelineItems": {"pageInfo": {"hasNextPage": true}, "nodes": []}}`, activityStamp(testNow.Add(-time.Hour)))
	serve := func(f *fixtureServer) {
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
			activityTimelineAnswer(w, vars, activityTimelineConnection(false, "", truncated), activityTimelineConnection(false, ""))
		})
	}
	since := testNow.Add(-24 * time.Hour)
	t.Run("listed", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		serve(f)
		f.handle("/repos/octocat/hello-world/issues/5/events", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `[{"event": "labeled", "created_at": %q}, {"event": "closed", "created_at": %q}]`,
				activityStamp(testNow.Add(-48*time.Hour)), activityStamp(testNow.Add(-time.Hour)))
		})
		points, err := IssueEvents{Since: since}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) != 1 || points[0].Tags["event"] != "closed" {
			t.Errorf("got %v, want only the event inside the window", points)
		}
	})
	t.Run("broken", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		serve(f)
		f.status("/repos/octocat/hello-world/issues/5/events", http.StatusInternalServerError, "boom")
		if _, err := (IssueEvents{Since: since}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
			t.Fatal("a broken event list was not reported")
		}
	})
	t.Run("broken on a pull request", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
			activityTimelineAnswer(w, vars, activityTimelineConnection(false, ""), activityTimelineConnection(false, "", truncated))
		})
		f.status("/repos/octocat/hello-world/issues/5/events", http.StatusInternalServerError, "boom")
		if _, err := (IssueEvents{Since: since}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
			t.Fatal("a broken event list of a pull request was not reported")
		}
	})
}

// --- joblogs.go ---------------------------------------------------------

// TestJobLogsStopAtTheCapInsideOneRun gives one run two failed jobs and a cap
// of one. The cap is checked per job, not only per run, so the second log is
// never fetched.
func TestJobLogsStopAtTheCapInsideOneRun(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"workflow_runs": [{"id": 7, "path": ".github/workflows/ci.yml", "status": "completed", "conclusion": "failure", "updated_at": %q}]}`,
			activityStamp(testNow.Add(-time.Hour)))
	})
	f.handle("/repos/octocat/hello-world/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jobs": [
		  {"id": 71, "name": "test", "conclusion": "failure", "completed_at": "2026-09-08T14:00:00Z"},
		  {"id": 72, "name": "lint", "conclusion": "failure", "completed_at": "2026-09-08T14:00:00Z"}
		]}`))
	})
	for _, id := range []string{"71", "72"} {
		f.handle("/repos/octocat/hello-world/actions/jobs/"+id+"/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("no timestamp on this line\n"))
		})
	}
	points, err := JobLogs{MaxJobs: 1}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/jobs/72/logs")); n != 0 {
		t.Errorf("fetched the second log %d times past a cap of one", n)
	}
	if len(points) != 1 {
		t.Fatalf("got %d lines, want 1", len(points))
	}
	// A line without its own timestamp is stamped when the job finished.
	if want := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC); !points[0].Time.Equal(want) {
		t.Errorf("stamped %s, want the job's completion %s", points[0].Time, want)
	}
}

// TestJobLogsReportEveryBrokenStep fails each of the three requests in turn:
// the run list, the job list and the log. Each is an error, not a quiet sweep.
func TestJobLogsReportEveryBrokenStep(t *testing.T) {
	t.Parallel()
	for _, broken := range []string{
		"/repos/octocat/hello-world/actions/runs",
		"/repos/octocat/hello-world/actions/runs/1000163134/jobs",
		"/repos/octocat/hello-world/actions/jobs/2000000011/logs",
	} {
		t.Run(broken, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			jobLogRoutes(t, f)
			f.status(broken, http.StatusInternalServerError, "boom")
			if _, err := (JobLogs{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
				t.Errorf("a failure of %s was not reported", broken)
			}
		})
	}
}

// TestJobLogsFailedRunsReadExactlyThePagesAsked serves full pages of a
// hundred failed runs for ever and then a short page, and holds the walk to
// its page count and to the short page that ends it.
func TestJobLogsFailedRunsReadExactlyThePagesAsked(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/actions/runs"
	t.Run("full pages", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
			if activityRunawayREST(w, f, path, 4) {
				return
			}
			_, _ = w.Write(runPage(t, 100, testNow.Add(-time.Hour), 5000))
		})
		runs, err := JobLogs{Walk: Walk{Pages: 2}}.failedRuns(ctx(t), f.Client, testRepo)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls(path)); n != 2 || len(runs) != 200 {
			t.Errorf("made %d requests for %d runs, want 2 for 200", n, len(runs))
		}
	})
	t.Run("short page", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
			if activityRunawayREST(w, f, path, 4) {
				return
			}
			_, _ = w.Write(runPage(t, 3, testNow.Add(-time.Hour), 5000))
		})
		runs, err := JobLogs{Walk: Walk{Pages: 2}}.failedRuns(ctx(t), f.Client, testRepo)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls(path)); n != 1 || len(runs) != 3 {
			t.Errorf("made %d requests for %d runs, want 1 for 3", n, len(runs))
		}
	})
}

// TestJobLogsLookBackAMonthBeforeTheWindow pins the created filter to the
// rerun reach: thirty one days before the window, at the start of that day.
func TestJobLogsLookBackAMonthBeforeTheWindow(t *testing.T) {
	t.Parallel()
	got := JobLogs{Since: testNow}.createdFilter()
	if want := "&created=%3E%3D2026-08-08T00%3A00%3A00Z"; got != want {
		t.Errorf("createdFilter = %q, want %q", got, want)
	}
}

// TestLastLinesKeepsALineLongerThanTheDefaultBuffer feeds a hundred kilobyte
// line, which a scanner at its default limit gives up on and drops every line
// after.
func TestLastLinesKeepsALineLongerThanTheDefaultBuffer(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 100*1024)
	got := lastLines("first\n"+long+"\nlast\n", 5)
	if len(got) != 3 || got[1] != long || got[2] != "last" {
		t.Errorf("got %d lines, want first, the long one and last", len(got))
	}
}

// TestStripANSIAtTheEndOfALine covers escape sequences cut short by the end of
// the line, and an escape that is not a color sequence at all.
func TestStripANSIAtTheEndOfALine(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"abc\x1b":      "abc",
		"x\x1b[31":     "x",
		"a\x1bXb":      "ab",
		"\x1b[1;31mok": "ok",
		"a\x1b[ b":     "ab",
	} {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitLogLineReadsATimestampOfFortyCharacters holds the width limit to
// its exact value: forty characters is still a timestamp, forty one is not.
func TestSplitLogLineReadsATimestampOfFortyCharacters(t *testing.T) {
	t.Parallel()
	forty := "2026-09-07T10:00:05.12345678901234+00:00"
	at, msg := splitLogLine(forty + " FAIL")
	if len(forty) != 40 || at.IsZero() || msg != "FAIL" {
		t.Errorf("a %d character timestamp gave %s %q", len(forty), at, msg)
	}
	fortyOne := "2026-09-07T10:00:05.123456789012345+00:00"
	at, msg = splitLogLine(fortyOne + " FAIL")
	if len(fortyOne) != 41 || !at.IsZero() || msg != fortyOne+" FAIL" {
		t.Errorf("a %d character token gave %s %q", len(fortyOne), at, msg)
	}
}

// --- pulls.go -----------------------------------------------------------

// activityPullsAnswer is one page of the pulls query, answering only the
// connections it asked for.
func activityPullsAnswer(w http.ResponseWriter, vars map[string]any, pulls, issues string) {
	parts := []string{}
	if vars["withPRs"] == true {
		parts = append(parts, `"pullRequests": `+pulls)
	}
	if vars["withIssues"] == true {
		parts = append(parts, `"issues": `+issues)
	}
	_, _ = fmt.Fprintf(w, `{"data": {"repository": {%s}}}`, strings.Join(parts, ", "))
}

// activityPullItem is one open pull request or issue.
func activityPullItem(number int) string {
	return fmt.Sprintf(`{"number": %d, "state": "OPEN", "url": "https://github.com/octocat/hello-world/pull/%d", "createdAt": %q, "updatedAt": %q}`,
		number, number, activityStamp(testNow.Add(-48*time.Hour)), activityStamp(testNow.Add(-time.Hour)))
}

// TestPullsWalkIssuesAloneOnceThePullRequestsRunOut serves pull requests that
// end on the first page and issues that go on. The first query carries no
// cursor at all, and the second asks for issues alone from theirs.
func TestPullsWalkIssuesAloneOnceThePullRequestsRunOut(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		if activityRunawayGraphQL(w, f, 4) {
			return
		}
		if vars["issueAfter"] == "i1" {
			activityPullsAnswer(w, vars, activityTimelineConnection(false, ""), activityTimelineConnection(false, "", activityPullItem(3)))
			return
		}
		activityPullsAnswer(w, vars, activityTimelineConnection(false, "", activityPullItem(1)), activityTimelineConnection(true, "i1", activityPullItem(2)))
	})
	points, err := Pulls{Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	vars := activityGraphQLVars(t, f)
	if len(vars) != 2 {
		t.Fatalf("made %d queries, want 2", len(vars))
	}
	for _, cursor := range []string{"prAfter", "issueAfter"} {
		if _, sent := vars[0][cursor]; sent {
			t.Errorf("the first query sent %s %v", cursor, vars[0][cursor])
		}
	}
	if _, sent := vars[1]["prAfter"]; sent || vars[1]["issueAfter"] != "i1" || vars[1]["withPRs"] != false {
		t.Errorf("the second query asked %v, want issues alone from i1", vars[1])
	}
	if n := len(byMeasurement(points)["gh_issue"]); n != 2 {
		t.Errorf("got %d issues, want both pages' 2", n)
	}
}

// TestPullsDoNotFollowANextPageThatHasNothingOnIt guards the index into the
// last item of a page that says there is more but holds nothing.
func TestPullsDoNotFollowANextPageThatHasNothingOnIt(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		if activityRunawayGraphQL(w, f, 4) {
			return
		}
		activityPullsAnswer(w, vars, activityTimelineConnection(true, "p"), activityTimelineConnection(true, "i"))
	})
	points, err := Pulls{Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Fatalf("got %d points and %v", len(points), err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("made %d queries, want 1", n)
	}
}

// TestPullsHalveOnlyAPageTheGatewayGaveUpOnAndOnlyDownToTen holds the retry
// to its two conditions: an ordinary error is returned at once whatever the
// page size, and a page of ten the gateway gave up on is not halved again.
func TestPullsHalveOnlyAPageTheGatewayGaveUpOnAndOnlyDownToTen(t *testing.T) {
	t.Parallel()
	t.Run("ordinary error", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
		})
		if _, err := (Pulls{First: 50}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
			t.Fatal("an ordinary error was not reported")
		}
		if n := len(f.calls("/graphql")); n != 1 {
			t.Errorf("an ordinary error was retried: %d queries", n)
		}
	})
	t.Run("gateway gives up on ten", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
		})
		_, err := Pulls{First: 10}.Collect(ctx(t), f.Client, testRepo, testNow)
		if _, tooLarge := errors.AsType[*ghapi.TooLargeError](err); !tooLarge {
			t.Fatalf("got %v, want the gateway's refusal", err)
		}
		if n := len(f.calls("/graphql")); n != 1 {
			t.Errorf("a page of ten was halved: %d queries", n)
		}
	})
}

// TestPullHelpersReadTheEdgesOfTheirInput covers the small readers the pull
// points are built from, on the inputs the fixtures never carry: a thread
// with no id, a pull request whose author is gone, an unnamed label, a total
// that is not an int, and a total with no repository.
func TestPullHelpersReadTheEdgesOfTheirInput(t *testing.T) {
	t.Parallel()
	if got := threadTag(0); got != noneTag {
		t.Errorf("threadTag(0) = %q", got)
	}
	if isSelf(&pullNode{}, &actor{Login: "octocat"}) {
		t.Error("a pull request by a deleted account was reviewed by itself")
	}
	if got := labelNames([]labelNode{{Name: "bug"}, {Name: ""}, {Name: "docs"}}); got != "bug,docs" {
		t.Errorf("labelNames = %q", got)
	}
	if got := fieldSum(map[string]any{"a": 2, "b": int64(3), "c": 4}, "a", "b", "c"); got != 6 {
		t.Errorf("fieldSum = %d, want only the ints", got)
	}
	into := map[string]ItemCounts{}
	ReadItemCounts([]sink.Point{{Measurement: "gh_repo_total", Fields: map[string]any{"pulls_open": 3}}}, into)
	if len(into) != 0 {
		t.Errorf("a total with no repository was read as %v", into)
	}
}

// TestFirstHumanReviewIsTheEarliestWhateverTheOrder lists reviews out of
// order, with one still pending: the earliest submitted one by someone else
// wins.
func TestFirstHumanReviewIsTheEarliestWhateverTheOrder(t *testing.T) {
	t.Parallel()
	var pr pullNode
	mustUnmarshal(t, []byte(`{"author": {"login": "octocat", "__typename": "User"}, "reviews": {"totalCount": 3, "nodes": [
	  {"author": {"login": "hubot", "__typename": "User"}, "submittedAt": "2026-09-07T12:00:00Z"},
	  {"author": {"login": "monalisa", "__typename": "User"}, "submittedAt": "2026-09-07T11:00:00Z"},
	  {"author": {"login": "pending", "__typename": "User"}, "submittedAt": null}
	]}}`), &pr)
	first, ok := firstHumanReview(&pr)
	if want := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC); !ok || !first.Equal(want) {
		t.Errorf("first human review = %s %t, want %s", first, ok, want)
	}
	if rows := reviewPoints(&pr, activityBase, nil); len(rows) != 2 {
		t.Errorf("got %d review rows, want the two submitted", len(rows))
	}
}

// --- repoactivity.go ----------------------------------------------------

// TestWeeklyCommitsTolerateAShorterOwnerSeries serves an owner series one
// week shorter than the total, which GitHub does for a repository younger
// than a year. The week without an owner figure has no field for it.
func TestWeeklyCommitsTolerateAShorterOwnerSeries(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/stats/participation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"all": [1, 2, 3], "owner": [0, 1]}`))
	})
	points, err := weeklyCommitPoints(ctx(t), f.Client, testRepo, activityBase, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("got %d weeks, want 3", len(points))
	}
	if fieldInt(t, points[1], "owner_commits") != 1 || hasField(points[2], "owner_commits") {
		t.Errorf("owner figures = %v and %v", points[1].Fields, points[2].Fields)
	}
}

// TestPunchCardKeepsSundayAndDropsADayThatDoesNotExist reads the first day of
// the week, which is day zero, and days outside the week.
func TestPunchCardKeepsSundayAndDropsADayThatDoesNotExist(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/stats/punch_card", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0, 9, 3], [-1, 9, 2], [7, 9, 1], [6, 23, 0], [6, 23, 4]]`))
	})
	points, err := punchCardPoints(ctx(t), f.Client, testRepo, activityBase, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d hours, want Sunday at nine and Saturday at eleven", len(points))
	}
	sunday := find(t, points, "gh_commit_punchcard", map[string]string{"weekday": "Sun", "hour": "09"})
	if fieldInt(t, sunday, "commits") != 3 {
		t.Errorf("Sunday commits = %v", sunday.Fields["commits"])
	}
}

// TestWorkflowsWithoutDatesHaveNoAgeAndABrokenListFails covers the workflow
// list's two unhappy shapes.
func TestWorkflowsWithoutDatesHaveNoAgeAndABrokenListFails(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"workflows": [{"name": "CI", "path": ".github/workflows/ci.yml", "state": "active", "html_url": "u"}]}`))
	})
	points, err := workflowPoints(ctx(t), f.Client, testRepo, activityBase, testNow)
	if err != nil || len(points) != 1 {
		t.Fatalf("got %d points and %v", len(points), err)
	}
	if hasField(points[0], "age_days") || hasField(points[0], "days_since_change") {
		t.Errorf("an undated workflow has ages: %v", points[0].Fields)
	}
	broken := newFixtureServer(t)
	broken.status("/repos/octocat/hello-world/actions/workflows", http.StatusInternalServerError, "boom")
	if _, brokenErr := workflowPoints(ctx(t), broken.Client, testRepo, activityBase, testNow); brokenErr == nil {
		t.Error("a broken workflow list was not reported")
	}
}

// TestDiscussionContextNamesOnlyThePeopleStillThere reads a discussion with no
// answer, one whose answer's author is gone, and one fully named.
func TestDiscussionContextNamesOnlyThePeopleStillThere(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw, answeredBy, chosenBy string
	}{
		{`{"answer": null, "answerChosenBy": null}`, "", ""},
		{`{"answer": {"author": null}, "answerChosenBy": {"login": "octocat"}}`, "", "octocat"},
		{`{"answer": {"author": {"login": "hubot"}}, "answerChosenBy": {"login": "octocat"}}`, "hubot", "octocat"},
	}
	for _, tc := range cases {
		var d discussionNode
		mustUnmarshal(t, []byte(tc.raw), &d)
		c := d.context()
		if c.AnsweredBy != tc.answeredBy || c.ChosenBy != tc.chosenBy {
			t.Errorf("%s: answered by %q chosen by %q", tc.raw, c.AnsweredBy, c.ChosenBy)
		}
	}
}

// activityDiscussionsAnswer is one page of discussions with one discussion
// updated at the given moment, or none.
func activityDiscussionsAnswer(next bool, updated *time.Time) string {
	nodes := ""
	if updated != nil {
		nodes = fmt.Sprintf(`{"number": 1, "title": "t", "url": "u", "createdAt": %q, "updatedAt": %q,
		  "category": {"name": "Q&A", "isAnswerable": true}, "reactions": {"totalCount": 0}, "comments": {"totalCount": 0, "nodes": []}}`,
			activityStamp(*updated), activityStamp(*updated))
	}
	return fmt.Sprintf(`{"data": {"repository": {"discussions": {"totalCount": 9, "pageInfo": {"hasNextPage": %t, "endCursor": "d"}, "nodes": [%s]}}}}`, next, nodes)
}

// TestDiscussionsWalkStopsWhereItShould serves discussions that never end and
// holds the walk to each of its stops: the page count, an empty page, and a
// page past the bound.
func TestDiscussionsWalkStopsWhereItShould(t *testing.T) {
	t.Parallel()
	recent := testNow.Add(-time.Hour)
	cases := []struct {
		name      string
		walk      Walk
		updated   *time.Time
		wantCalls int
	}{
		{"page count", Walk{Pages: 2}, &recent, 2},
		{"empty page", Walk{Pages: 3}, nil, 1},
		{"past the bound", Walk{Pages: 3, Since: testNow}, &recent, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
				if activityRunawayGraphQL(w, f, 4) {
					return
				}
				_, _ = w.Write([]byte(activityDiscussionsAnswer(true, tc.updated)))
			})
			if _, err := (Discussions{Walk: tc.walk}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
				t.Fatal(err)
			}
			if n := len(f.calls("/graphql")); n != tc.wantCalls {
				t.Errorf("made %d queries, want %d", n, tc.wantCalls)
			}
		})
	}
}

// --- repoevents.go ------------------------------------------------------

// TestRepoActivityLogStopsAtAFullPageWithoutANextLink serves a full page of a
// hundred with no Link header. Without a cursor there is no next page to ask
// for, however full this one is.
func TestRepoActivityLogStopsAtAFullPageWithoutANextLink(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/activity"
	f := newFixtureServer(t)
	f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		if activityRunawayREST(w, f, path, 4) {
			return
		}
		_, _ = w.Write(repeat(t, "repo_activity.json", "", 100, func(i int, row map[string]any) {
			row["timestamp"] = spacedOut(i)
		}))
	})
	points, err := RepoActivityLog{Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(path)); n != 1 {
		t.Errorf("made %d requests, want 1", n)
	}
	if len(points) != 100 {
		t.Errorf("got %d points, want the page's 100", len(points))
	}
}

// TestAfterCursorReadsTheEdgesOfALinkHeader covers a relative next link whose
// cursor is its first parameter, a next link with an empty cursor, and a
// header with a trailing separator.
func TestAfterCursorReadsTheEdgesOfALinkHeader(t *testing.T) {
	t.Parallel()
	for link, want := range map[string]string{
		`<after=abc>; rel="next"`: "abc",
		`<https://api.github.com/repos/o/n/activity?after=&per_page=100>; rel="next"`: "",
		`<https://api.github.com/repos/o/n/activity?after=abc>; rel="next";`:          "abc",
		`<https://api.github.com/repos/o/n/activity?after=abc>; rev=next`:             "",
		`<https://api.github.com/repos/o/n/activity?after=abc; rel="next"`:            "",
	} {
		if got := afterCursor(link); got != want {
			t.Errorf("afterCursor(%q) = %q, want %q", link, got, want)
		}
	}
}

// --- conditions left one-sided --------------------------------------------

// activityPaginationLimit answers the way the activity feeds do once past
// their ceiling.
func activityPaginationLimit(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write([]byte(`{"message":"In order to keep the API fast for everyone, pagination is limited for this resource."}`))
}

// TestListingsTreatThePaginationCeilingAsTheEndOfTheData asks every REST walk
// of these collectors for a first page GitHub refuses with its pagination
// ceiling. That is where the data ends, not a failure.
func TestListingsTreatThePaginationCeilingAsTheEndOfTheData(t *testing.T) {
	t.Parallel()
	collect := map[string]func(f *fixtureServer) ([]sink.Point, error){
		"/repos/octocat/hello-world/actions/runs": func(f *fixtureServer) ([]sink.Point, error) {
			return Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		},
		"/repos/octocat/hello-world/actions/artifacts": func(f *fixtureServer) ([]sink.Point, error) {
			return Artifacts{}.Collect(ctx(t), f.Client, testRepo, testNow)
		},
		"/repos/octocat/hello-world/activity": func(f *fixtureServer) ([]sink.Point, error) {
			return RepoActivityLog{}.Collect(ctx(t), f.Client, testRepo, testNow)
		},
		"/repos/octocat/hello-world/actions/runs?status=failure": func(f *fixtureServer) ([]sink.Point, error) {
			return JobLogs{}.Collect(ctx(t), f.Client, testRepo, testNow)
		},
	}
	for path, run := range collect {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.handle(strings.TrimSuffix(path, "?status=failure"), activityPaginationLimit)
			if _, err := run(f); err != nil {
				t.Errorf("the pagination ceiling was reported as %v", err)
			}
		})
	}
}

// TestFeedsReportABrokenPageRatherThanEndingQuietly is the other side of the
// ceiling: a 500 on the event feed, the inbox or the activity log is an
// error.
func TestFeedsReportABrokenPageRatherThanEndingQuietly(t *testing.T) {
	t.Parallel()
	t.Run("events", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/users/octocat/events", http.StatusInternalServerError, "boom")
		if _, err := (&Events{Login: "octocat"}).Collect(ctx(t), f.Client, testNow); err == nil {
			t.Error("a broken event feed was not reported")
		}
	})
	t.Run("notifications", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/notifications", http.StatusInternalServerError, "boom")
		if _, err := (&Notifications{}).Collect(ctx(t), f.Client, testNow); err == nil {
			t.Error("a broken inbox was not reported")
		}
	})
	t.Run("activity", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/repos/octocat/hello-world/activity", http.StatusInternalServerError, "boom")
		if _, err := (RepoActivityLog{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
			t.Error("a broken activity log was not reported")
		}
	})
}

// TestRepoActivityLogStopsAtEachEndOfTheWalk serves an activity log that
// always offers a next page, and ends it three ways: an empty page, a short
// page, and a full page older than the bound.
func TestRepoActivityLogStopsAtEachEndOfTheWalk(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/activity"
	cases := []struct {
		name  string
		rows  int
		since time.Time
	}{
		{"empty page", 0, time.Time{}},
		{"short page", 3, time.Time{}},
		{"past the bound", 100, testNow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
				if activityRunawayREST(w, f, path, 4) {
					return
				}
				w.Header().Set("Link", `<https://api.github.com/repos/octocat/hello-world/activity?per_page=100&after=more>; rel="next"`)
				if tc.rows == 0 {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				_, _ = w.Write(repeat(t, "repo_activity.json", "", tc.rows, func(i int, row map[string]any) {
					row["timestamp"] = spacedOut(i)
				}))
			})
			points, err := RepoActivityLog{Walk: Walk{Pages: 3, Since: tc.since}}.Collect(ctx(t), f.Client, testRepo, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(f.calls(path)); n != 1 {
				t.Errorf("made %d requests, want 1", n)
			}
			if len(points) != tc.rows {
				t.Errorf("got %d points, want %d", len(points), tc.rows)
			}
		})
	}
}

// TestActionsTimesThatAreMissingLeaveTheirDurationOut reads a job that was
// created but never started, a step with an end and no start, and an
// artifact with an expiry but no creation date. None has two moments to
// subtract. The run list is walked for its one page only, and the artifact
// walk ends at an empty page.
func TestActionsTimesThatAreMissingLeaveTheirDurationOut(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(runPage(t, 2, testNow.Add(-time.Hour), 7))
	})
	f.handle("/repos/octocat/hello-world/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jobs": [{"name": "unstarted", "conclusion": "failure",
		  "created_at": "2026-09-08T14:00:00Z", "started_at": null, "completed_at": "2026-09-08T14:01:00Z",
		  "steps": [{"name": "cut", "number": 1, "conclusion": "failure", "started_at": null, "completed_at": "2026-09-08T14:01:00Z"}]}]}`))
	})
	points, err := Actions{Jobs: true, MaxJobRuns: 1, PerPage: 2, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs")); n != 1 {
		t.Errorf("made %d run list requests past a walk of one page", n)
	}
	job := find(t, points, "gh_workflow_job", map[string]string{"job_name": "unstarted"})
	if hasField(job, "queued_seconds") {
		t.Errorf("a job that never started has queued_seconds %v", job.Fields["queued_seconds"])
	}
	if steps := byMeasurement(points)["gh_workflow_step"]; len(steps) != 0 {
		t.Errorf("a step with no start was written: %v", steps)
	}

	arts := newFixtureServer(t)
	arts.handle("/repos/octocat/hello-world/actions/artifacts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`{"total_count": 100, "artifacts": []}`))
			return
		}
		_, _ = w.Write(repeat(t, "artifacts.json", "artifacts", 100, func(i int, row map[string]any) {
			row["name"] = fmt.Sprintf("undated-%d", i)
			row["created_at"] = nil
		}))
	})
	points, err = Artifacts{Walk: Walk{Pages: 3}}.Collect(ctx(t), arts.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(arts.calls("/repos/octocat/hello-world/actions/artifacts")); n != 2 {
		t.Errorf("made %d artifact requests, want the full page and the empty one", n)
	}
	for _, p := range only(t, points, "gh_artifact") {
		if hasField(p, "retention_days") {
			t.Fatalf("an artifact with no creation date has retention_days %v", p.Fields["retention_days"])
		}
	}
}

// TestBranchesWithNothingToSayAreNotAnError answers a batch whose aliases are
// all missing: no branches, and nothing failed.
func TestBranchesWithNothingToSayAreNotAnError(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data": {"unrelated": null}}`))
	})
	points, err := Branches{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Fatalf("got %d points and %v, want neither", len(points), err)
	}
}

// TestIssueEventsTimelineEndsAndFailures covers the rest of the listing: a
// pull request walk that continues after the issues are done, a connection
// that says there is more but gives no cursor, a timeline item of a type the
// table does not know, and a query that fails, once for good and once
// because the repository is gone.
func TestIssueEventsTimelineEndsAndFailures(t *testing.T) {
	t.Parallel()
	unknown := `{"__typename": "SomethingNewEvent", "createdAt": "2026-09-08T14:00:00Z"}`
	t.Run("issues outlast pull requests", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
			if activityRunawayGraphQL(w, f, 4) {
				return
			}
			if vars["issueAfter"] == "i1" {
				activityTimelineAnswer(w, vars, activityTimelineConnection(true, "", activityTimelineNode(3, activityLabeled)), "")
				return
			}
			activityTimelineAnswer(w, vars,
				activityTimelineConnection(true, "i1", activityTimelineNode(1, activityLabeled, unknown)),
				activityTimelineConnection(false, "", activityTimelineNode(2, activityLabeled)))
		})
		points, err := IssueEvents{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
		}
		// The second page says there is more but gives no cursor, so it is
		// the last one asked.
		if n := len(f.calls("/graphql")); n != 2 {
			t.Errorf("made %d queries, want 2", n)
		}
		if len(points) != 3 {
			t.Errorf("got %d events, want the three known ones", len(points))
		}
	})
	for name, body := range map[string]string{
		"broken": `{"errors":[{"type":"INTERNAL","message":"boom"}]}`,
		"gone":   `{"errors":[{"type":"NOT_FOUND","message":"gone"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
				_, _ = w.Write([]byte(body))
			})
			_, err := IssueEvents{}.Collect(ctx(t), f.Client, testRepo, testNow)
			if (err != nil) != (name == "broken") {
				t.Errorf("%s repository gave %v", name, err)
			}
		})
	}
}

// TestJobLogsFailedRunsStopAtAFullPageOlderThanTheBound serves full pages of
// failed runs that are all older than the walk's bound, and a run whose jobs
// are gone.
func TestJobLogsFailedRunsStopAtAFullPageOlderThanTheBound(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/actions/runs"
	f := newFixtureServer(t)
	f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		if activityRunawayREST(w, f, path, 4) {
			return
		}
		_, _ = w.Write(runPage(t, 100, testNow.Add(-48*time.Hour), 5000))
	})
	runs, err := JobLogs{Walk: Walk{Pages: 3, Since: testNow.Add(-24 * time.Hour)}}.failedRuns(ctx(t), f.Client, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(path)); n != 1 || len(runs) != 100 {
		t.Errorf("made %d requests for %d runs, want 1 for 100", n, len(runs))
	}
	jobs, err := failedJobs(ctx(t), f.Client, testRepo, 5000)
	if err != nil || jobs != nil {
		t.Errorf("a run whose jobs are gone gave %v and %v", jobs, err)
	}
}

// TestPullRowsLeaveOutWhatTheGatewayDidNotSay reads a pull request whose
// first review has no date and whose review thread has no subject type.
func TestPullRowsLeaveOutWhatTheGatewayDidNotSay(t *testing.T) {
	t.Parallel()
	var pr pullNode
	mustUnmarshal(t, []byte(`{"number": 9, "state": "OPEN", "createdAt": "2026-09-07T10:00:00Z",
	  "timelineItems": {"nodes": [{"createdAt": null}]},
	  "reviewThreads": {"totalCount": 1, "nodes": [{"path": "main.go", "subjectType": "",
	    "comments": {"totalCount": 1, "nodes": [{"databaseId": 0, "createdAt": "2026-09-07T11:00:00Z", "author": null}]}}]}}`), &pr)
	points := Pulls{}.pullPoints([]pullNode{pr}, activityBase, testNow)
	checkPoints(t, points)
	if p := only(t, points, "gh_pull_request")[0]; hasField(p, "seconds_to_first_review") {
		t.Errorf("an undated first review gave seconds_to_first_review %v", p.Fields["seconds_to_first_review"])
	}
	thread := only(t, points, "gh_review_thread")[0]
	if hasField(thread, "subject_type") || thread.Tags["thread"] != noneTag {
		t.Errorf("a thread without subject type or id gave %v %v", thread.Tags, thread.Fields)
	}
}
