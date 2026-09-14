package collect

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func jobLogRoutes(t *testing.T, f *fixtureServer) {
	t.Helper()
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs_failed.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs_failed.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163100/jobs", "actions_jobs_failed.json")
	// GitHub answers the log request with a redirect to object storage.
	f.handle("/repos/octocat/hello-world/actions/jobs/2000000011/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/storage/2000000011.txt", http.StatusFound)
	})
	f.handle("/repos/octocat/hello-world/actions/jobs/2000000010/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/storage/2000000010.txt", http.StatusFound)
	})
	f.file("/storage/2000000011.txt", "job_log.txt")
	f.file("/storage/2000000010.txt", "job_log.txt")
}

func TestJobLogsOnlyFailedJobsTailOnly(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	jobLogRoutes(t, f)

	points, err := JobLogs{Since: testNow.AddDate(0, 0, -30), Tail: 3}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	runs := f.calls("/repos/octocat/hello-world/actions/runs")
	if len(runs) != 1 || runs[0].Query["status"] != "failure" {
		t.Errorf("run list calls = %d, query %v: GitHub filters the failures, not this", len(runs), runs[0].Query)
	}
	// The January run predates Since, so its jobs are never asked for.
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs/1000163100/jobs")); n != 0 {
		t.Errorf("jobs of a run older than Since fetched %d times", n)
	}
	// Only the failed job's log is fetched; the successful build's is not.
	if n := len(f.calls("/repos/octocat/hello-world/actions/jobs/2000000010/logs")); n != 0 {
		t.Errorf("the successful job's log was fetched %d times", n)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/jobs/2000000011/logs")); n != 1 {
		t.Errorf("the failed job's log was fetched %d times, want 1", n)
	}
	// The redirect to storage must arrive without the token.
	storage := f.calls("/storage/2000000011.txt")
	if len(storage) != 1 {
		t.Fatalf("storage fetched %d times", len(storage))
	}
	if got := storage[0].Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization %q followed the redirect to storage", got)
	}

	lines := only(t, points, "gh_job_log")
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want the tail of 3", len(lines))
	}
	for _, p := range lines {
		if p.Tags["workflow"] != ".github/workflows/ci.yml" || p.Tags["job_name"] != "test" || p.Tags["run"] != "1000163134" {
			t.Errorf("log tags = %v", p.Tags)
		}
		line, _ := p.Fields["line"].(string)
		if strings.ContainsRune(line, 0x1b) {
			t.Errorf("ANSI escape survived in %q", line)
		}
		if strings.HasPrefix(line, "2026-") {
			t.Errorf("timestamp not peeled off the line: %q", line)
		}
	}
	if got := lines[0].Fields["line"]; got != "    client_test.go:42: second request did not send If-None-Match" {
		t.Errorf("first tail line = %q", got)
	}
	if got := lines[2].Fields["line"]; got != "Error: Process completed with exit code 1." {
		t.Errorf("last tail line = %q, color codes must be stripped", got)
	}
	// Stamped at the moment the line was printed, from the line itself.
	if want := time.Date(2026, 9, 6, 14, 5, 55, 0, time.UTC); !lines[2].Time.Equal(want) {
		t.Errorf("last line stamped %s, want %s from the line", lines[2].Time, want)
	}
	if want := time.Date(2026, 9, 6, 14, 5, 53, 0, time.UTC); !lines[0].Time.Equal(want) {
		t.Errorf("first tail line stamped %s, want %s", lines[0].Time, want)
	}
}

// TestJobLogsAskForTheFailuresOfTheWindowOnly: the failure list is filtered
// on the server by creation date, from the day thirty one days before the
// window opened, which is as far back as a re-run can reach; the URL repeats
// within the day so the ETag holds; and the exact cut at Since, by when the
// run finished, still happens here.
func TestJobLogsAskForTheFailuresOfTheWindowOnly(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	jobLogRoutes(t, f)

	// The 8th of September at 15:04:05, minus two hours, minus thirty one
	// days, is the 8th of August at 13:04:05: the filter asks from that day's
	// midnight so that every sweep of the day asks for the same URL.
	since := testNow.Add(-2 * time.Hour)
	if _, err := (JobLogs{Since: since}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	calls := f.calls("/repos/octocat/hello-world/actions/runs")
	if len(calls) != 1 {
		t.Fatalf("made %d run list requests, want 1", len(calls))
	}
	if got, want := calls[0].Query["created"], ">=2026-08-08T00:00:00Z"; got != want {
		t.Errorf("created = %q, want %q: the day a re-run could still reach back to", got, want)
	}
	if calls[0].Query["status"] != "failure" {
		t.Errorf("status = %q, the filter must not replace the failure filter", calls[0].Query["status"])
	}
	// The window is two hours and the fixture's failure is two days old: the
	// server would have served it, and the cut at Since drops it.
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs/1000163134/jobs")); n != 0 {
		t.Errorf("jobs of a run that finished before Since fetched %d times", n)
	}

	// Since ten hours later, before the day turns: same URL.
	if _, err := (JobLogs{Since: since.Add(10 * time.Hour)}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	calls = f.calls("/repos/octocat/hello-world/actions/runs")
	if len(calls) != 2 || calls[1].Query["created"] != calls[0].Query["created"] {
		t.Errorf("a sweep ten hours later asked created=%q, want the same %q so the ETag holds",
			calls[1].Query["created"], calls[0].Query["created"])
	}

	// No window, no filter: a backfill without a bound wants the whole list.
	if _, err := (JobLogs{}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	calls = f.calls("/repos/octocat/hello-world/actions/runs")
	if _, filtered := calls[2].Query["created"]; filtered {
		t.Errorf("a collector without Since filtered by created=%q", calls[2].Query["created"])
	}
}

// TestJobLogsStillSeeTheReRunOfAnOldFailure: a re-run keeps the created_at
// of its first attempt, so a failure that finished inside the window may
// have been created weeks ago. The fake refuses a bound that would have
// hidden it, as GitHub would silently have.
func TestJobLogsStillSeeTheReRunOfAnOldFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	jobLogRoutes(t, f)
	// Created three weeks before the sweep, re-run and failed ten minutes ago.
	created := testNow.AddDate(0, 0, -21)
	f.handle("/repos/octocat/hello-world/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		bound := strings.TrimPrefix(r.URL.Query().Get("created"), ">=")
		from, err := time.Parse(time.RFC3339, bound)
		if err != nil || from.After(created) {
			// What GitHub answers a bound past the run: nothing.
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			return
		}
		_, _ = w.Write(repeat(t, "actions_runs_failed.json", "workflow_runs", 1, func(_ int, row map[string]any) {
			row["id"] = 1000163134
			row["run_attempt"] = 3
			row["created_at"] = created.Format(time.RFC3339)
			row["updated_at"] = testNow.Add(-10 * time.Minute).Format(time.RFC3339)
		}))
	})
	points, err := JobLogs{Since: testNow.Add(-2 * time.Hour)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(only(t, points, "gh_job_log")) == 0 {
		t.Error("the re-run of a three week old failure, which failed inside the window, produced no log lines")
	}
}

// The tag set has to be the one gh_workflow_run publishes, or the log lines
// cannot be grouped with the run they came from, and it has to stay bounded:
// the fixture's run is dynamically named after a pull request, which is the
// value that made the old tag one series per pull request.
func TestJobLogsAreTaggedByWorkflowPathAndNeverByBranch(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	jobLogRoutes(t, f)

	points, err := JobLogs{Since: testNow.AddDate(0, 0, -30), Tail: 3}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range only(t, points, "gh_job_log") {
		if got := p.Tags["workflow"]; got != ".github/workflows/ci.yml" {
			t.Errorf("workflow tag = %q, want the path the run list carries", got)
		}
		if got := p.Tags["workflow"]; strings.Contains(got, "PR #495") {
			t.Errorf("workflow tag = %q, which is the run's dynamic name", got)
		}
		if _, isTag := p.Tags["branch"]; isTag {
			t.Errorf("branch is a tag again: %v", p.Tags)
		}
		// Demoted rather than dropped, and renamed because InfluxDB 3 would
		// reject a field that reuses the old tag's column name.
		if got, _ := p.Fields["head_branch"].(string); got != "feature/x" {
			t.Errorf("head_branch field = %q, want the run's branch as a value", got)
		}
	}
}

func TestJobLogsStripsTheByteOrderMark(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	jobLogRoutes(t, f)
	points, err := JobLogs{Since: testNow.AddDate(0, 0, -30)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	lines := only(t, points, "gh_job_log")
	// Six non-empty lines in the fixture; the default tail of 40 keeps all.
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6", len(lines))
	}
	first := lines[0]
	// Without the BOM stripped the first line would be the only one whose
	// date fails to parse and it would fall back to the job's completion.
	if want := time.Date(2026, 9, 6, 14, 5, 50, 123456700, time.UTC); !first.Time.Equal(want) {
		t.Errorf("first line stamped %s, want %s parsed from the line", first.Time, want)
	}
	if line, _ := first.Fields["line"].(string); strings.HasPrefix(line, "\ufeff") || !strings.HasSuffix(line, "Run go test ./...") {
		t.Errorf("first line = %q", line)
	}
}

func TestJobLogsMaxJobs(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	jobLogRoutes(t, f)
	// Both runs inside the window, so two failed jobs are candidates. A cap
	// of one stops after the first.
	points, err := JobLogs{Since: testNow.AddDate(-1, 0, 0), MaxJobs: 1}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/jobs/2000000011/logs")); n != 1 {
		t.Errorf("fetched the log %d times with MaxJobs 1", n)
	}
	if n := len(f.calls("/repos/octocat/hello-world/actions/runs/1000163100/jobs")); n != 0 {
		t.Errorf("the second run's jobs were listed %d times after the cap was hit", n)
	}
	if len(points) != 6 {
		t.Errorf("got %d lines", len(points))
	}
}

func TestJobLogsExpiredLogIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs_failed.json")
	f.file("/repos/octocat/hello-world/actions/runs/1000163134/jobs", "actions_jobs_failed.json")
	f.handle("/repos/octocat/hello-world/actions/jobs/2000000011/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone) // GitHub deletes logs after ninety days
	})
	points, err := JobLogs{Since: testNow.AddDate(0, 0, -30)}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("a 410 is a deleted log, not a failure: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points", len(points))
	}
}

func TestJobLogsActionsDisabled(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := JobLogs{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("err=%v points=%d", err, len(points))
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[36;1m=== RUN   TestETag\x1b[0m", "=== RUN   TestETag"},
		{"\x1b[91mError:\x1b[0m done", "Error: done"},
		{"a\x1b[1;32;40mb\x1b[mc", "abc"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLastLines(t *testing.T) {
	t.Parallel()
	text := "\ufeffone\r\ntwo\r\n\r\n   \r\nthree\r\nfour\r\n"
	got := lastLines(text, 2)
	if len(got) != 2 || got[0] != "three" || got[1] != "four" {
		t.Errorf("lastLines = %q", got)
	}
	all := lastLines(text, 40)
	if len(all) != 4 || all[0] != "one" {
		t.Errorf("blank lines must be dropped and the BOM stripped: %q", all)
	}
	if empty := lastLines("", 5); len(empty) != 0 {
		t.Errorf("empty text gave %q", empty)
	}
}

func TestSplitLogLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, wantMsg string
		wantTime          time.Time
	}{
		{"timestamped", "2026-09-06T14:05:55.0000000Z FAIL", "FAIL", time.Date(2026, 9, 6, 14, 5, 55, 0, time.UTC)},
		{"no timestamp", "FAIL", "FAIL", time.Time{}},
		{"not a date", "hello world", "hello world", time.Time{}},
		{"leading space", " 2026-09-06T14:05:55Z x", " 2026-09-06T14:05:55Z x", time.Time{}},
		{"long token", strings.Repeat("x", 50) + " y", strings.Repeat("x", 50) + " y", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at, msg := splitLogLine(tc.in)
			if msg != tc.wantMsg || !at.Equal(tc.wantTime) {
				t.Errorf("splitLogLine(%q) = (%s, %q), want (%s, %q)", tc.in, at, msg, tc.wantTime, tc.wantMsg)
			}
		})
	}
}
