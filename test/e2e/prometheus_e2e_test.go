package e2e

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scrape fetches the exporter once. An error means it is not up yet, which is
// the normal state while the first sweep is running.
func scrape(ctx context.Context, addr, path string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, http.NoBody)
	if err != nil {
		return "", false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", false
	}
	return string(body), true
}

func TestPrometheusExporterServesTheReducedGauges(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	addr := freeAddr(t)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), `  prometheus:
    listen: `+addr+`
    path: /metrics`)

	// Serve mode, not -once: a one-shot run skips the exporter on purpose,
	// because it would exit before anything could scrape it.
	proc := serveInBackground(t, cfg)

	// The families are written as they finish, so a scrape taken mid-sweep is
	// a partial page. Wait for the sweep to end before reading it.
	awaitSweep(t, proc, 60*time.Second)
	var body string
	if !waitFor(30*time.Second, func() bool {
		b, up := scrape(t.Context(), addr, "/metrics")
		if !up || !strings.Contains(b, "github_") {
			return false
		}
		body = b
		return true
	}) {
		t.Fatalf("the exporter served nothing on %s:\n%s", addr, proc.Output())
	}

	samples, types := parseExposition(t, body)
	if len(samples) < 20 {
		t.Fatalf("the exporter served %d samples:\n%s", len(samples), body)
	}
	byName := samplesByName(t, samples, types)
	assertWorkflowRunsWereReduced(t, byName)
	if len(byName["github_stars_gained_count"]) == 0 {
		t.Errorf("stars were not reduced to a count; metrics served: %v", sortedNames(byName))
	}
	assertReducedNamesAreGone(t, byName)

	if out := proc.Stop(); strings.Contains(out, "sink write failed") {
		t.Errorf("a sink failed while serving:\n%s", out)
	}
}

// samplesByName groups the scrape by metric name, checking as it goes the two
// things every served series has to have: the exporter's own naming, and a
// TYPE line, without which Prometheus reads it as untyped.
func samplesByName(t *testing.T, samples []promSample, types map[string]string) map[string][]promSample {
	t.Helper()
	byName := map[string][]promSample{}
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
		if !strings.HasPrefix(s.Name, "github_") {
			t.Errorf("metric %q does not follow github_<measurement>_<field>", s.Name)
		}
		if types[s.Name] == "" {
			t.Errorf("metric %q was served without a TYPE line", s.Name)
		}
	}
	return byName
}

// assertWorkflowRunsWereReduced: a per-item measurement arrives reduced, as a
// count and a running total, not as one series per workflow run.
func assertWorkflowRunsWereReduced(t *testing.T, byName map[string][]promSample) {
	t.Helper()
	runs := byName["github_workflow_runs_count"]
	if len(runs) == 0 {
		t.Fatalf("no github_workflow_runs_count; metrics served: %v", sortedNames(byName))
	}
	if len(byName["github_workflow_runs_total"]) == 0 {
		t.Errorf("the counter github_workflow_runs_total was not served")
	}
	if len(runs) > 4 {
		t.Errorf("github_workflow_runs_count has %d series, so the reduction did not happen", len(runs))
	}
	for _, s := range runs {
		// The tags the rule keeps became labels, and the ones it drops are
		// what stops the series count exploding.
		if s.Labels["repo"] != "hello-world" {
			t.Errorf("github_workflow_runs_count labels = %v, want the repository tag", s.Labels)
		}
		for k := range s.Labels {
			switch k {
			case "repo", "workflow", "conclusion":
			default:
				t.Errorf("label %q survived the reduction: %v", k, s.Labels)
			}
		}
	}
}

// assertReducedNamesAreGone: the measurements the rules skip, and the per-item
// names the reduction renames away, must not be served at all. The one name
// that survives under a forbidden prefix is checked here too, so the exemption
// and the reason for it are read together.
func assertReducedNamesAreGone(t *testing.T, byName map[string][]promSample) {
	t.Helper()
	// gh_workflow_run_total is a different measurement from gh_workflow_run: a
	// per-repository aggregate the rules keep whole, whose exported name sits
	// under the per-item measurement's prefix. Both halves are pinned, because
	// the prefix below cannot tell them apart.
	const runTotal = "github_workflow_run_total_runs"
	if len(byName[runTotal]) == 0 {
		t.Errorf("%s was not served, so the aggregate the rules keep went with the per-run series", runTotal)
	}
	// gh_star_list is the same shape under the star prefix: the lists the
	// account files its stars into, a daily snapshot the rules keep whole,
	// where gh_star itself is the per-star history reduced to a count.
	const starList = "github_star_list_"
	if !anyWithPrefix(byName, starList) {
		t.Errorf("no %s* metric was served, so the snapshot the rules keep went with the per-star series", starList)
	}
	for _, forbidden := range []string{
		"github_workflow_run_", "github_star_", "github_commits_week_",
		"github_job_log_", "github_commit_punchcard_",
	} {
		for name := range byName {
			if name == runTotal || strings.HasPrefix(name, starList) {
				continue
			}
			if strings.HasPrefix(name, forbidden) {
				t.Errorf("%q was served, but %q is skipped or reduced away", name, forbidden)
			}
		}
	}
}

// anyWithPrefix reports whether the scrape served a metric under the prefix.
func anyWithPrefix(byName map[string][]promSample, prefix string) bool {
	for name := range byName {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// TestPrometheusExporterIsSkippedByOnce pins the documented reason -once has no
// exporter: a one-shot run would bind the port a long-running instance holds
// and then exit before a scrape could arrive.
func TestPrometheusExporterIsSkippedByOnce(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	addr := freeAddr(t)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), `  prometheus:
    listen: `+addr+`
    path: /metrics`)

	sweepOnce(t, cfg)

	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err == nil {
		// Nothing should have answered, so this connection is the failure
		// itself. Closing it is tidying up after a test that is about to
		// fail on the next line, and there is nothing to do if that fails.
		_ = conn.Close()
		t.Errorf("something is still listening on %s after a one-shot run", addr)
	}
}
