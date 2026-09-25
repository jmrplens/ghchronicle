package collect

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// runPage builds a page of n completed runs, newest first, each a minute
// older than the previous, starting at newest.
func runPage(t *testing.T, n int, newest time.Time, firstID int64) []byte {
	t.Helper()
	return repeat(t, "actions_runs.json", "workflow_runs", n, func(i int, row map[string]any) {
		at := newest.Add(-time.Duration(i) * time.Minute)
		row["id"] = firstID - int64(i)
		row["created_at"] = at.Add(-5 * time.Minute).Format(time.RFC3339)
		row["run_started_at"] = at.Add(-4 * time.Minute).Format(time.RFC3339)
		row["updated_at"] = at.Format(time.RFC3339)
	})
}

func TestActionsPaginatesTheRunList(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	newest := testNow.Add(-time.Hour)
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write(runPage(t, 100, newest, 5000))
		case "2":
			_, _ = w.Write(runPage(t, 100, newest.Add(-100*time.Minute), 4900))
		default:
			f.write(w, "actions_runs.json") // four runs, one still in progress
		}
	})
	f.file("/repos/octocat/hello-world/actions/cache/usage", "actions_cache.json")

	points, err := Actions{Walk: Walk{Pages: 5}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/repos/octocat/hello-world/actions/runs")
	if len(calls) != 3 {
		t.Fatalf("made %d run list requests, want 3: two full pages and the short one that ends the walk", len(calls))
	}
	checkRunListRequests(t, calls, 100)
	runs := only(t, points, "gh_workflow_run")
	// 200 from the full pages plus the three completed runs of the short page;
	// the in-progress one has no duration yet.
	if len(runs) != 203 {
		t.Errorf("got %d run points, want 203", len(runs))
	}
	seen := runIDs(t, runs)
	if !seen[5000] || !seen[4901] || !seen[4801] || !seen[1000163135] {
		t.Error("runs from every page must be present")
	}
	// The whole history, which the walk never reaches, taken from the count
	// the list declares. Current state, so stamped now.
	total := only(t, points, "gh_workflow_run_total")[0]
	if fieldInt(t, total, "runs") != 4 || !total.Time.Equal(testNow) {
		t.Errorf("run total = %v at %s, want the declared 4 stamped now", total.Fields, total.Time)
	}
	cache := only(t, points, "gh_actions_cache")[0]
	if fieldInt(t, cache, "size_bytes") != 356210012 || fieldInt(t, cache, "count") != 7 || !cache.Time.Equal(testNow) {
		t.Errorf("cache = %v at %s", cache.Fields, cache.Time)
	}
}

// checkRunListRequests holds every run list request to consecutive pages of
// perPage runs, none of them filtered by date.
func checkRunListRequests(t *testing.T, calls []request, perPage int) {
	t.Helper()
	for i, c := range calls {
		if c.Query["page"] != strconv.Itoa(i+1) || c.Query["per_page"] != strconv.Itoa(perPage) {
			t.Errorf("call %d asked for page=%s per_page=%s, want page=%d per_page=%d",
				i, c.Query["page"], c.Query["per_page"], i+1, perPage)
		}
		if _, filtered := c.Query["created"]; filtered {
			t.Error("the run list must not be filtered server side, the newest runs are always worth having")
		}
	}
}

// TestActionsPageSizeIsWhatWasAskedFor asks for pages of thirty, which is
// what an ordinary sweep does: the page is a third of the bytes, and a page
// full of runs newer than Since still pages on, so a busy two hours loses
// nothing to the smaller page.
func TestActionsPageSizeIsWhatWasAskedFor(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	newest := testNow.Add(-time.Minute)
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write(runPage(t, 30, newest, 5000))
		case "2":
			_, _ = w.Write(runPage(t, 30, newest.Add(-30*time.Minute), 4970))
		default:
			_, _ = w.Write(runPage(t, 3, newest.Add(-60*time.Minute), 4940))
		}
	})
	points, err := Actions{Since: testNow.Add(-2 * time.Hour), PerPage: 30, Walk: Walk{Pages: 5}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/repos/octocat/hello-world/actions/runs")
	if len(calls) != 3 {
		t.Fatalf("made %d run list requests, want 3: two full pages of thirty and the short one that ends the walk", len(calls))
	}
	checkRunListRequests(t, calls, 30)
	if runs := only(t, points, "gh_workflow_run"); len(runs) != 63 {
		t.Errorf("got %d runs, want the 63 of all three pages", len(runs))
	}
}

// TestActionsDoesNotListTheJobsOfARunAlreadyWritten gives two sweeps the same
// memory: the second sees the same completed runs and lists no jobs at all,
// while still publishing every run. A re-run of one of them is a new attempt
// with new jobs, and is listed again.
func TestActionsDoesNotListTheJobsOfARunAlreadyWritten(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163135/jobs", "actions_jobs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163132/jobs", "actions_jobs.json")
	jobListings := func() int {
		n := 0
		for _, id := range []string{"1000163135", "1000163134", "1000163132"} {
			n += len(f.calls("/repos/octocat/hello-world/actions/runs/" + id + "/jobs"))
		}
		return n
	}
	expanded := map[RunKey]struct{}{}
	sweep := Actions{Jobs: true, Expanded: expanded, Walk: Walk{Pages: 1}}

	points, err := sweep.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := jobListings(); n != 3 {
		t.Fatalf("the first sweep listed jobs %d times, want once per completed run", n)
	}
	if len(only(t, points, "gh_workflow_job")) == 0 {
		t.Fatal("the first sweep wrote no jobs")
	}
	if _, ok := expanded[RunKey{ID: 1000163134, Attempt: 2}]; !ok {
		t.Errorf("the re-run is remembered by its attempt, got %v", expanded)
	}

	points, err = sweep.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := jobListings(); n != 3 {
		t.Errorf("the second sweep listed jobs again: %d listings in all, want the 3 of the first sweep", n)
	}
	if n := len(only(t, points, "gh_workflow_run")); n != 3 {
		t.Errorf("the second sweep published %d runs, want all 3 completed ones whatever their jobs", n)
	}
	if n := len(byMeasurement(points)["gh_workflow_job"]); n != 0 {
		t.Errorf("the second sweep published %d jobs it did not list", n)
	}

	// A third attempt of the re-run: same id, new jobs.
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(repeat(t, "actions_runs.json", "workflow_runs", 1, func(_ int, row map[string]any) {
			row["id"] = 1000163134
			row["run_attempt"] = 3
		}))
	})
	if _, err = sweep.Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs/1000163134/jobs")); n != 2 {
		t.Errorf("the re-run's jobs were listed %d times, want 2: once per attempt", n)
	}
}

// TestASweepThatFailedKeepsTheJobsItHadAlreadyCollected is the collector's
// half of the defect a real store showed: one 502 on one run's job listing
// used to cost a repository every run and job the family had already
// rendered. The rows come back with the error now, so the runs behind them
// are remembered too: they have been written, and listing them again next
// sweep is the round trip the memory exists to save.
func TestASweepThatFailedKeepsTheJobsItHadAlreadyCollected(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163135/jobs", "actions_jobs.json")
	f.status("/repos/octocat/hello-world/actions/runs/1000163134/jobs", http.StatusBadGateway, "boom")
	f.file("/repos/octocat/hello-world/actions/runs/1000163132/jobs", "actions_jobs.json")
	expanded := map[RunKey]struct{}{}
	sweep := Actions{Jobs: true, Expanded: expanded, Walk: Walk{Pages: 1}}

	points, err := sweep.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil {
		t.Fatal("a job listing answered 502 and the sweep did not fail")
	}
	if len(only(t, points, "gh_workflow_job")) == 0 {
		t.Error("the 502 on the second run threw away the jobs of the first")
	}
	if _, kept := expanded[RunKey{ID: 1000163135, Attempt: 1}]; !kept || len(expanded) != 1 {
		t.Errorf("remembered %v, want the one run whose jobs were written", expanded)
	}

	// Every job listing answers now and the cache totals do not: the two runs
	// left are expanded, and their rows survive the later failure as well.
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs.json")
	f.status("/repos/octocat/hello-world/actions/cache/usage", http.StatusBadGateway, "boom")
	points, err = sweep.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil {
		t.Fatal("the cache totals answered 502 and the sweep did not fail")
	}
	if len(only(t, points, "gh_workflow_run")) == 0 {
		t.Error("the 502 on the cache totals threw away the runs collected before it")
	}
	if len(expanded) != 3 {
		t.Errorf("remembered %d runs after the cache failed, want the 3 that were written", len(expanded))
	}

	f.file("/repos/octocat/hello-world/actions/cache/usage", "actions_cache.json")
	if _, err = sweep.Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 3 {
		t.Errorf("the sweep that succeeded remembered %d runs, want its 3", len(expanded))
	}
}

// TestActionsSkippedRunsDoNotCountAgainstTheCap: the cap bounds what a sweep
// pays, so a run already written leaves room for one that is not, and a
// window wider than the cap fills in over sweeps instead of stopping at the
// newest MaxJobRuns for ever.
func TestActionsSkippedRunsDoNotCountAgainstTheCap(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163135/jobs", "actions_jobs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163132/jobs", "actions_jobs.json")
	sweep := Actions{Jobs: true, MaxJobRuns: 1, Expanded: map[RunKey]struct{}{}, Walk: Walk{Pages: 1}}
	for _, want := range []string{"1000163135", "1000163134", "1000163132"} {
		if _, err := sweep.Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls("/repos/octocat/hello-world/actions/runs/" + want + "/jobs")); n != 1 {
			t.Errorf("run %s listed %d times after its sweep, want once", want, n)
		}
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs/1000163135/jobs")); n != 1 {
		t.Errorf("the newest run was listed %d times over three sweeps", n)
	}
}

// runIDs is the set of run ids the points carry, reporting any run that was
// produced twice.
func runIDs(t *testing.T, runs []sink.Point) map[int64]bool {
	t.Helper()
	seen := map[int64]bool{}
	for _, p := range runs {
		id := fieldInt(t, p, "run_id")
		if seen[id] {
			t.Errorf("run %d produced twice", id)
		}
		seen[id] = true
	}
	return seen
}

func TestActionsSinceStopsPagingButNeverFiltersTheFirstPage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// Every run on the first page is older than Since. All hundred must
	// still be published; only the second page is not fetched.
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(runPage(t, 100, testNow.AddDate(0, 0, -10), 9000))
	})
	points, err := Actions{Since: testNow.Add(-2 * time.Hour), Walk: Walk{Pages: 5}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs")); n != 1 {
		t.Errorf("made %d requests, want 1: the last run of page one predates Since", n)
	}
	if runs := only(t, points, "gh_workflow_run"); len(runs) != 100 {
		t.Errorf("got %d runs, want all 100 of the first page whatever Since says", len(runs))
	}
}

func TestActionsRunFieldsAndDating(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163135/jobs", "actions_jobs.json")
	// A run whose job listing is gone: the sweep skips it rather than
	// failing, which is what the t.Fatal below would catch.
	f.status("/repos/octocat/hello-world/actions/runs/1000163134/jobs", 404, "Not Found")
	f.file("/repos/octocat/hello-world/actions/cache/usage", "actions_cache.json")

	points, err := Actions{Jobs: true, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_workflow_run", "gh_workflow_job", "gh_workflow_step", "gh_actions_cache")

	checkRunRow(t, points)
	checkRunIdentityIsTheWorkflowFile(t, points)
	checkJobRow(t, points)
	checkStepRows(t, points)
	checkEveryAttemptWasAskedFor(t, f)
}

// checkRunRow reads the completed CI run: what it is dated at, and which of
// its unbounded values stayed fields rather than becoming series.
func checkRunRow(t *testing.T, points []sink.Point) {
	t.Helper()
	run := find(t, points, "gh_workflow_run", map[string]string{
		"workflow": ".github/workflows/ci.yml", "event": "push",
	})
	// Stamped when it finished.
	if want := time.Date(2026, 9, 7, 10, 4, 10, 0, time.UTC); !run.Time.Equal(want) {
		t.Errorf("run stamped %s, want updated_at %s", run.Time, want)
	}
	// Both unbounded, both fields: they exist so a retry can be paired with
	// what it retried, not to be grouped by.
	if _, ok := run.Fields["run_id"].(int64); !ok {
		t.Errorf("run_id must be an int64 field, got %T", run.Fields["run_id"])
	}
	if run.Fields["head_sha"] != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Errorf("head_sha field = %v", run.Fields["head_sha"])
	}
	for _, k := range []string{"run_id", "head_sha", "runner"} {
		if _, isTag := run.Tags[k]; isTag {
			t.Errorf("%s must not be a tag", k)
		}
	}
	if fieldInt(t, run, "duration_seconds") != 220 || fieldInt(t, run, "queued_seconds") != 30 || run.Fields["success"] != true {
		t.Errorf("run fields = %v", run.Fields)
	}
	if run.Tags["conclusion"] != "success" || run.Tags["actor"] != "jmrplens" {
		t.Errorf("run tags = %v", run.Tags)
	}
	// A branch is unbounded here: one per pull request and per bump, never
	// reused. It is a value to read, not a series to group by.
	if _, isTag := run.Tags["branch"]; isTag {
		t.Error("branch must be a field, not a tag")
	}
	// And it is published as head_branch, the API's own name. `branch` was a
	// tag on this measurement, and a field that reused the name would be the
	// one change InfluxDB 3 answers by rejecting every write to the table.
	if hasField(run, "branch") {
		t.Error("the demoted branch must not reuse the old tag's column name")
	}
	if run.Fields["head_branch"] != "main" || run.Fields["name"] != "CI" || fieldInt(t, run, "workflow_id") != 217658648 {
		t.Errorf("run fields = %v", run.Fields)
	}
	// The run's own title differs from the workflow name, so it is kept.
	if run.Fields["title"] != "Drop the arrow from two links" {
		t.Errorf("title field = %v", run.Fields["title"])
	}
	if hasField(run, "initial_actor") {
		t.Error("initial_actor is only for a run whose attempt was triggered by someone else")
	}
	checkRunReaderFields(t, run)
}

// checkRunReaderFields reads what a reader wants beside the number: the
// "#1483" GitHub shows, the pull request the run belongs to, and the first
// line of the commit.
func checkRunReaderFields(t *testing.T, run sink.Point) {
	t.Helper()
	if fieldInt(t, run, "run_number") != 1483 || fieldInt(t, run, "pull_request") != 495 || fieldInt(t, run, "pull_requests") != 1 {
		t.Errorf("run_number/pull_request = %v %v %v", run.Fields["run_number"], run.Fields["pull_request"], run.Fields["pull_requests"])
	}
	if run.Fields["headline"] != "Drop the arrow from two links" {
		t.Errorf("headline = %v, want the first line only", run.Fields["headline"])
	}
	// The run came from the repository itself, so there is nothing to say.
	if hasField(run, "head_repo") {
		t.Errorf("head_repo is only for a run from a fork, got %v", run.Fields["head_repo"])
	}
	for _, k := range []string{"run_number", "pull_request", "headline", "head_repo"} {
		if _, isTag := run.Tags[k]; isTag {
			t.Errorf("%s must be a field, not a tag", k)
		}
	}
}

// checkRunIdentityIsTheWorkflowFile reads the two runs that would break a
// naive identity: the dynamically named one and the re-run.
func checkRunIdentityIsTheWorkflowFile(t *testing.T, points []sink.Point) {
	t.Helper()
	// The tag is the workflow file, never the run's name: on a dynamically
	// named run that name is the pull request title, one series per pull
	// request for ever.
	dyn := find(t, points, "gh_workflow_run", map[string]string{"event": "dynamic"})
	if dyn.Tags["workflow"] != "dynamic/github-code-scanning/codeql" {
		t.Errorf("dynamic run tagged %q, want its stable path", dyn.Tags["workflow"])
	}
	if dyn.Fields["name"] != "PR #495" {
		t.Errorf("the run's own name must survive as a field, got %v", dyn.Fields["name"])
	}
	if dyn.Tags["actor"] != "github-advanced-security[bot]" {
		t.Errorf("dynamic run actor = %q", dyn.Tags["actor"])
	}
	retry := find(t, points, "gh_workflow_run", map[string]string{"event": "pull_request"})
	if fieldInt(t, retry, "attempt") != 2 || retry.Fields["success"] != false {
		t.Errorf("retry fields = %v", retry.Fields)
	}
	// Dependabot opened it, a human asked for the second attempt: the tag is
	// who burns the runner, the field says who started it.
	if retry.Tags["actor"] != "jmrplens" || retry.Fields["initial_actor"] != "dependabot[bot]" {
		t.Errorf("re-run actor = %q, initial = %v", retry.Tags["actor"], retry.Fields["initial_actor"])
	}
	// A re-run keeps the run's created_at, so its "queue" would be the
	// minutes a person took to press the button: measured, 1,747 s on
	// average against 0.4 s on first attempts. The field is not written.
	if hasField(retry, "queued_seconds") {
		t.Errorf("queued_seconds on attempt 2 = %v, want absent", retry.Fields["queued_seconds"])
	}
	// GitHub links no pull request to a run from a fork, and the fork is
	// named so the reader knows why.
	if hasField(retry, "pull_request") || hasField(retry, "pull_requests") {
		t.Errorf("a run with no linked pull request must carry no pull_request field: %v", retry.Fields)
	}
	if retry.Fields["head_repo"] != "dependabot/hello-world" || fieldInt(t, retry, "run_number") != 1480 {
		t.Errorf("fork run fields = %v", retry.Fields)
	}
	// The in-progress release run has no duration yet and must wait.
	for _, p := range only(t, points, "gh_workflow_run") {
		if p.Tags["workflow"] == ".github/workflows/release.yml" {
			t.Error("an unfinished run must not be published")
		}
	}
}

// checkJobRow reads the job level, which is the only place the wait for a
// runner is separable from the time spent executing.
func checkJobRow(t *testing.T, points []sink.Point) {
	t.Helper()
	jobs := only(t, points, "gh_workflow_job")
	if len(jobs) != 2 {
		t.Fatalf("got %d job points, want 2", len(jobs))
	}
	build := find(t, points, "gh_workflow_job", map[string]string{"job_name": "build"})
	// A hosted runner is named per run; as a tag it would be a series per
	// job ever run.
	if _, isTag := build.Tags["runner"]; isTag {
		t.Error("runner must be a field, not a tag")
	}
	if build.Fields["runner"] != "GitHub Actions 1000163135" {
		t.Errorf("runner field = %v", build.Fields["runner"])
	}
	if build.Tags["runner_group"] != "GitHub Actions" || build.Tags["labels"] != "ubuntu-latest" || build.Tags["conclusion"] != "success" {
		t.Errorf("job tags = %v", build.Tags)
	}
	// Nothing else on the job says which workflow it belongs to, and job names
	// collide across workflows. The identity comes from the run, not from the
	// job's own workflow_name, which is the dynamic title again.
	if build.Tags["workflow"] != ".github/workflows/ci.yml" || build.Tags["attempt"] != "1" {
		t.Errorf("job tags = %v", build.Tags)
	}
	if fieldInt(t, build, "run_id") != 1000163135 ||
		build.Fields["head_sha"] != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" ||
		build.Fields["head_branch"] != "main" {
		t.Errorf("job fields = %v", build.Fields)
	}
	if fieldInt(t, build, "duration_seconds") != 145 || fieldInt(t, build, "queued_seconds") != 30 || fieldInt(t, build, "steps") != 2 {
		t.Errorf("job fields = %v", build.Fields)
	}
	if want := time.Date(2026, 9, 7, 10, 3, 0, 0, time.UTC); !build.Time.Equal(want) {
		t.Errorf("job stamped %s, want completed_at %s", build.Time, want)
	}
}

// checkStepRows reads the steps, which carry the run's identity down from the
// job so a slow step can be attributed.
func checkStepRows(t *testing.T, points []sink.Point) {
	t.Helper()
	steps := only(t, points, "gh_workflow_step")
	if len(steps) != 3 {
		t.Errorf("got %d step points, want 3", len(steps))
	}
	slow := find(t, points, "gh_workflow_step", map[string]string{"step": "go test ./..."})
	if fieldInt(t, slow, "duration_seconds") != 130 || slow.Tags["job_name"] != "build" {
		t.Errorf("step = %v %v", slow.Tags, slow.Fields)
	}
	if slow.Tags["workflow"] != ".github/workflows/ci.yml" || slow.Tags["attempt"] != "1" {
		t.Errorf("step tags = %v", slow.Tags)
	}
	if fieldInt(t, slow, "step_number") != 2 {
		t.Errorf("step_number = %v", slow.Fields["step_number"])
	}
}

// checkEveryAttemptWasAskedFor reads the request log: one jobs call per
// completed run, and filter=all, because the default hides earlier attempts.
func checkEveryAttemptWasAskedFor(t *testing.T, f *fixtureServer) {
	t.Helper()
	calls := f.calls("/repos/octocat/hello-world/actions/runs/1000163135/jobs")
	if len(calls) != 1 {
		t.Errorf("jobs fetched %d times, want 1", len(calls))
	}
	if calls[0].Query["filter"] != "all" {
		t.Errorf("jobs asked with filter=%q; the default (latest) hides every earlier attempt",
			calls[0].Query["filter"])
	}
}

// The tag has one job: never to be the run's name. A response missing the
// path (an older API shape, and the e2e fixture) has to fall back to something
// stable rather than to the pull request title it was replaced for.
func TestWorkflowTagNeverFallsBackToTheRunName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		run  runRow
		want string
	}{
		{
			"the file path is the identity",
			runRow{
				Name: "PR #495", Path: "dynamic/github-code-scanning/codeql", WorkflowID: 228473356,
			},
			"dynamic/github-code-scanning/codeql",
		},
		{"no path leaves the numeric workflow", runRow{
			Name: "Bump astro from 7.0.1 to 7.0.2", WorkflowID: 217658648,
		}, "217658648"},
		{"neither is not the title", runRow{Name: "Bump astro from 7.0.1 to 7.0.2"}, noneTag},
	} {
		if got := workflowTag(&c.run); got != c.want {
			t.Errorf("%s: workflowTag = %q, want %q", c.name, got, c.want)
		} else if got == c.run.Name {
			t.Errorf("%s: the tag took the run's name, which is one series per pull request", c.name)
		}
	}
}

// A re-run is the one case where a job listing has more than one attempt in
// it, and the default filter shows only the last. Measured against the live
// API on one run: 9 jobs with the default, 18 with filter=all, and the nine it
// hid were the first attempt with its failures.
func TestActionsKeepsEveryAttemptOfARerun(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs_retried.json")

	points, err := Actions{Jobs: true, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	// Both tries of `build` are present, and the attempt tag is what tells
	// them apart: without it they are one series and only completed_at
	// separates them, which is exactly the flaky test that cannot be seen.
	first := find(t, points, "gh_workflow_job", map[string]string{"job_name": "build", "attempt": "1"})
	second := find(t, points, "gh_workflow_job", map[string]string{"job_name": "build", "attempt": "2"})
	if first.Fields["success"] != true || second.Fields["success"] != false {
		t.Errorf("attempt 1 = %v, attempt 2 = %v", first.Fields, second.Fields)
	}
	// Same commit, two outcomes. That pairing is the whole point of the
	// run_id and head_sha fields.
	if first.Fields["head_sha"] != second.Fields["head_sha"] {
		t.Errorf("the two attempts must carry the same head_sha, got %v and %v",
			first.Fields["head_sha"], second.Fields["head_sha"])
	}
	if fieldInt(t, first, "run_id") != 1000163134 {
		t.Errorf("run_id = %v", first.Fields["run_id"])
	}
	// The job's own head_branch is the pull request ref, not the run's
	// branch, and it is a field for the same reason the run's branch is.
	if first.Fields["head_branch"] != "refs/pull/495/head" {
		t.Errorf("head_branch field = %v", first.Fields["head_branch"])
	}
	if _, isTag := first.Tags["branch"]; isTag {
		t.Error("branch must be a field on a job too")
	}
	// The job listing names the workflow with the dynamic title as well, so
	// the tag has to come from the run.
	if first.Tags["workflow"] != ".github/workflows/ci.yml" {
		t.Errorf("job workflow tag = %q, want the run's path", first.Tags["workflow"])
	}
	if n := len(only(t, points, "gh_workflow_job")); n != 3 {
		t.Errorf("got %d jobs, want the 3 of both attempts", n)
	}
	// Steps of two attempts are separated by the same tag.
	steps := only(t, points, "gh_workflow_step")
	if len(steps) != 3 {
		t.Errorf("got %d steps, want one per job", len(steps))
	}
	firstStep := find(t, points, "gh_workflow_step", map[string]string{
		"job_name": "build", "step": "go test ./...", "attempt": "1",
	})
	if firstStep.Fields["duration_seconds"] != 150 {
		t.Errorf("first attempt step = %v", firstStep.Fields)
	}
}

func TestActionsMaxJobRunsCapsTheExpansion(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163135/jobs", "actions_jobs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs.json")
	points, err := Actions{Jobs: true, MaxJobRuns: 1, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs/1000163134/jobs")); n != 0 {
		t.Errorf("second run's jobs fetched %d times with MaxJobRuns 1", n)
	}
	if len(only(t, points, "gh_workflow_job")) != 2 {
		t.Error("only the first run's jobs should be present")
	}
}

func TestActionsDisabledIsNotAnError(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/actions/runs", 404, "Not Found")
	points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points from a repository with Actions off", len(points))
	}
}

func artifactPage(t *testing.T, n, total, firstID int) []byte {
	t.Helper()
	b := repeat(t, "artifacts.json", "artifacts", n, func(i int, row map[string]any) {
		row["id"] = firstID + i
		row["name"] = fmt.Sprintf("artifact-%d", firstID+i)
		row["size_in_bytes"] = 1000
		row["expired"] = i%2 == 1
	})
	// repeat keeps the envelope's total_count; override it through a small
	// rewrite so the declared total can exceed what is served.
	var page struct {
		TotalCount int   `json:"total_count"`
		Artifacts  []any `json:"artifacts"`
	}
	mustUnmarshal(t, b, &page)
	page.TotalCount = total
	return mustMarshal(t, page)
}

func TestArtifactsWalkedVersusCount(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/actions/artifacts", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write(artifactPage(t, 100, 350, 1000))
		case "2":
			_, _ = w.Write(artifactPage(t, 100, 350, 1100))
		default:
			_, _ = w.Write(artifactPage(t, 100, 350, 1200))
		}
	})
	points, err := Artifacts{Walk: Walk{Pages: 2}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := len(f.calls("/repos/octocat/hello-world/actions/artifacts")); n != 2 {
		t.Errorf("made %d requests, want the 2 the page cap allows", n)
	}
	if len(only(t, points, "gh_artifact")) != 200 {
		t.Errorf("got %d artifact points, want 200", len(only(t, points, "gh_artifact")))
	}
	total := only(t, points, "gh_artifact_total")[0]
	// The page cap was hit: the declared total and what was walked disagree,
	// and the dashboard is told so instead of being handed a floor as a total.
	if fieldInt(t, total, "count") != 350 || fieldInt(t, total, "walked") != 200 {
		t.Errorf("total = %v, want count 350 and walked 200", total.Fields)
	}
	if fieldInt(t, total, "live_bytes") != 100*1000 {
		t.Errorf("live_bytes = %v, only the unexpired half of 200 artifacts at 1000 bytes", total.Fields["live_bytes"])
	}
	// The size travels with the count it is the size of. Without it a panel
	// reads a floor over 200 walked artifacts beside a declared 350 that
	// counts the expired ones too, and nothing says the two are different
	// denominators.
	if fieldInt(t, total, "live_count") != 100 {
		t.Errorf("live_count = %v, want the 100 unexpired artifacts live_bytes adds up", total.Fields["live_count"])
	}

	if !total.Time.Equal(testNow) {
		t.Errorf("the total is current state and must be stamped now, got %s", total.Time)
	}
}

func TestArtifactsShortPageEndsTheWalk(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/artifacts", "artifacts.json")
	points, err := Artifacts{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := len(f.calls("/repos/octocat/hello-world/actions/artifacts")); n != 1 {
		t.Errorf("made %d requests, a short page ends the walk", n)
	}
	total := only(t, points, "gh_artifact_total")[0]
	if fieldInt(t, total, "count") != 2 || fieldInt(t, total, "walked") != 2 || fieldInt(t, total, "live_bytes") != 204800 {
		t.Errorf("total = %v", total.Fields)
	}
	// Everything GitHub declared was walked, so the live figures are totals.
	if fieldInt(t, total, "live_count") != 1 {
		t.Errorf("total = %v, want the one live artifact", total.Fields)
	}
	live := find(t, points, "gh_artifact", map[string]string{"artifact": "coverage"})
	checkArtifactDemotedTags(t, live)
	if live.Fields["head_branch"] != "main" || fieldInt(t, live, "run_id") != 1000163135 ||
		live.Fields["head_sha"] != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Errorf("artifact fields = %v", live.Fields)
	}
	// The retention actually applied, which is what the type comment promised
	// and never stored. GitHub sets expires_at a few seconds short of a whole
	// number of days, so a day of retention must not read as zero.
	if fieldInt(t, live, "retention_days") != 1 {
		t.Errorf("retention_days = %v, want 1 day and not the 90 of the default setting",
			live.Fields["retention_days"])
	}
	if live.Fields["digest"] != "sha256:2e1ce4d604478ef02097867346afc125d33951a2108b26e41f68989cb02c0747" {
		t.Errorf("digest = %v", live.Fields["digest"])
	}
	if want := time.Date(2026, 9, 7, 10, 3, 0, 0, time.UTC); !live.Time.Equal(want) {
		t.Errorf("artifact stamped %s, want created_at %s", live.Time, want)
	}
	old := find(t, points, "gh_artifact", map[string]string{"artifact": "binaries"})
	if old.Fields["live"] != false || old.Fields["head_branch"] != "release/1.2" {
		t.Errorf("expired artifact = %v %v", old.Tags, old.Fields)
	}
	if fieldInt(t, old, "retention_days") != 90 {
		t.Errorf("retention_days = %v", old.Fields["retention_days"])
	}
	// An artifact from before GitHub computed digests carries none, and an
	// empty string field would be a value that reads as a real one.
	if hasField(old, "digest") {
		t.Errorf("digest = %v, want no field at all when the API sends none", old.Fields["digest"])
	}
}

// checkArtifactDemotedTags reads the two values that were tags and are
// fields, each under a new name so a database holding the old column keeps
// accepting writes.
func checkArtifactDemotedTags(t *testing.T, live sink.Point) {
	t.Helper()
	// Expiry happens after the row's own date, so as a tag it opened a second
	// series at the same instant and the artifact counted twice for ever.
	if _, isTag := live.Tags["expired"]; isTag {
		t.Error("expired must be a field, not a tag: the row is dated at creation and expiry comes later")
	}
	if hasField(live, "expired") {
		t.Error("the demoted flag must not reuse the old tag's column name")
	}
	if live.Fields["live"] != true {
		t.Errorf("artifact fields = %v, want live = true", live.Fields)
	}
	// One branch per pull request, never reused: the same unbounded value the
	// run carries, and a field for the same reason.
	if _, isTag := live.Tags["branch"]; isTag {
		t.Error("branch must be a field, not a tag")
	}
	if hasField(live, "branch") {
		t.Error("the demoted branch must not reuse the old tag's column name")
	}
}

func TestArtifactsDisabledStillWritesATotal(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := Artifacts{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	total := only(t, points, "gh_artifact_total")[0]
	if fieldInt(t, total, "count") != 0 || fieldInt(t, total, "walked") != 0 {
		t.Errorf("total = %v", total.Fields)
	}
	if fieldInt(t, total, "live_count") != 0 {
		t.Errorf("total = %v, want nothing live", total.Fields)
	}
}

func TestActionsCacheEntriesNameWhatHoldsTheSpace(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/cache/usage", "actions_cache.json")
	f.file("/repos/octocat/hello-world/actions/caches", "actions_caches.json")

	points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	entries := only(t, points, "gh_actions_cache_entry")
	if len(entries) != 2 {
		t.Fatalf("got %d cache entries, want one per key", len(entries))
	}
	// The total says a repository holds twelve gigabytes. Only this says which
	// key holds them, which is what decides what gets evicted.
	big := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": "node-cache-Linux-x64-pnpm"})
	if fieldInt(t, big, "size_bytes") != 344766379 || big.Tags["ref"] != "refs/heads/main" {
		t.Errorf("largest entry = %v %v", big.Tags, big.Fields)
	}
	if fieldInt(t, big, "days_since_use") != 1 {
		t.Errorf("days_since_use = %v", big.Fields["days_since_use"])
	}
	// A snapshot of what is stored now, so a day's sweeps rewrite one row.
	if want := testNow.UTC().Truncate(24 * time.Hour); !big.Time.Equal(want) {
		t.Errorf("entry stamped %s, want the start of the day %s", big.Time, want)
	}
}

// TestActionsJobWithoutARunnerKeepsEveryTag reads a job that never reached a
// runner. GitHub answers an empty runner group and an empty label list for
// those, and an empty tag value is dropped on the way into InfluxDB, so the
// row would land in a series carrying neither tag rather than beside the jobs
// that name both.
//
// Measured on 2026-09-10 over two repositories: 25 of 349 jobs were in this
// state, 20 canceled and 5 skipped.
func TestActionsJobWithoutARunnerKeepsEveryTag(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163135/jobs", "actions_jobs_cancelled.json")
	points, err := Actions{Jobs: true, MaxJobRuns: 1, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	job := find(t, points, "gh_workflow_job", map[string]string{"job_name": "docs"})
	if job.Tags["runner_group"] != noneTag || job.Tags["labels"] != noneTag {
		t.Errorf("a job that never ran must still carry both tags, got %v", job.Tags)
	}
	// Canceled before a runner took it, it ran no steps, and that zero is a
	// fact: only a job that ran to an end has had its steps withheld.
	if !hasField(job, "steps") || fieldInt(t, job, "steps") != 0 {
		t.Errorf("a job canceled before it ran must keep steps 0, got %v", job.Fields)
	}
}

// stepsJobs is a run whose jobs GitHub has stopped serving the steps of, beside
// the two shapes whose count still means what it says: a job that ran and
// lists its steps, and a skipped job, which ran none.
const stepsJobs = `{"total_count": 5, "jobs": [
  {"name": "old-pass", "status": "completed", "conclusion": "success", "run_attempt": 1,
   "started_at": "2026-03-02T10:00:35Z", "completed_at": "2026-03-02T10:03:00Z", "steps": []},
  {"name": "old-fail", "status": "completed", "conclusion": "failure", "run_attempt": 1,
   "started_at": "2026-03-02T10:00:35Z", "completed_at": "2026-03-02T10:02:00Z", "steps": []},
  {"name": "old-timeout", "status": "completed", "conclusion": "timed_out", "run_attempt": 1,
   "started_at": "2026-03-02T10:00:35Z", "completed_at": "2026-03-02T16:00:35Z", "steps": []},
  {"name": "skipped", "status": "completed", "conclusion": "skipped", "run_attempt": 1,
   "started_at": "2026-03-02T10:00:05Z", "completed_at": "2026-03-02T10:00:05Z", "steps": []},
  {"name": "listed", "status": "completed", "conclusion": "success", "run_attempt": 1,
   "started_at": "2026-09-07T10:00:35Z", "completed_at": "2026-09-07T10:03:00Z",
   "steps": [
     {"name": "one", "number": 1, "conclusion": "success", "started_at": "2026-09-07T10:00:35Z", "completed_at": "2026-09-07T10:01:00Z"},
     {"name": "two", "number": 2, "conclusion": "success", "started_at": "2026-09-07T10:01:00Z", "completed_at": "2026-09-07T10:02:50Z"},
     {"name": "three", "number": 3, "conclusion": "success", "started_at": "2026-09-07T10:02:50Z", "completed_at": "2026-09-07T10:03:00Z"}
   ]}
]}`

// TestActionsJobWhoseStepsGitHubDroppedHasNoStepCount reads the one shape where
// an empty steps list is not a fact about the job. Measured on 2026-09-24:
// every job of a run created before about 12 April came back with its times,
// runner and conclusion but an empty steps list, while every later run's jobs
// carried theirs. A job that ran to success, failure or a timeout ran at least
// one step, so zero there is GitHub's retention talking, and written as a
// count it pulls every mean of steps toward zero for as far back as the
// backfill reached. A skipped job runs none (measured: 0 steps), so its zero
// is the truth and stays.
func TestActionsJobWhoseStepsGitHubDroppedHasNoStepCount(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.handle("/repos/octocat/hello-world/actions/runs/1000163135/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(stepsJobs))
	})
	points, err := Actions{Jobs: true, MaxJobRuns: 1, Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	for _, name := range []string{"old-pass", "old-fail", "old-timeout"} {
		job := find(t, points, "gh_workflow_job", map[string]string{"job_name": name})
		if hasField(job, "steps") {
			t.Errorf("%s ran and lists no steps, yet wrote steps %v", name, job.Fields["steps"])
		}
		// Only the count goes: the job is still a job, with its duration.
		if !hasField(job, "duration_seconds") {
			t.Errorf("%s lost its duration with its steps: %v", name, job.Fields)
		}
	}
	for name, want := range map[string]int64{"skipped": 0, "listed": 3} {
		job := find(t, points, "gh_workflow_job", map[string]string{"job_name": name})
		if !hasField(job, "steps") {
			t.Errorf("%s wrote no steps, want %d", name, want)
			continue
		}
		if got := fieldInt(t, job, "steps"); got != want {
			t.Errorf("%s wrote steps %d, want %d", name, got, want)
		}
	}
	if n := len(only(t, points, "gh_workflow_step")); n != 3 {
		t.Errorf("got %d step points, want the 3 the listed job carries", n)
	}
}

// TestOnlyAJobThatRanToAnEndHasItsEmptyStepsWithheld holds stepsWithheld to
// its contract over every conclusion the jobs endpoint documents, and the
// empty one a null decodes to. The fixtures carry five of them, so a withheld
// set widened to neutral or action_required would pass every other test,
// and a job whose zero may be the truth would silently lose its count.
func TestOnlyAJobThatRanToAnEndHasItsEmptyStepsWithheld(t *testing.T) {
	t.Parallel()
	withheld := map[string]bool{"success": true, "failure": true, "timed_out": true}
	// GitHub's spelling, assembled because the linter's dictionary is American
	// and would correct it to a value no job carries.
	cancelledJob := "cancel" + "led"
	for _, conclusion := range []string{
		"success", "failure", "timed_out", "neutral", cancelledJob, "skipped", "action_required", "",
		// A run's conclusion and a check suite's, never a job's. They stand
		// for one GitHub may add: nothing says such a job ran a step, so its
		// zero is kept until somebody measures otherwise.
		"startup_failure", "stale",
	} {
		if got := stepsWithheld(conclusion, 0); got != withheld[conclusion] {
			t.Errorf("an empty steps list concluded %q: withheld %v, want %v", conclusion, got, withheld[conclusion])
		}
		// A listed step is GitHub still serving them, whatever the job did.
		if stepsWithheld(conclusion, 1) {
			t.Errorf("a job concluded %q that lists a step had its count withheld", conclusion)
		}
	}
}
