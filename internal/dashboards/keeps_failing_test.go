package dashboards

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestWorkflowsThatKeepFailingAskTheSameThresholdEverywhere is "Workflows
// that keep failing" in the 2.6.1 review. The two SQL stores and Prometheus
// listed a workflow past three failures in the range and nothing on the
// suite's runs; Graphite listed a workflow with one failure, counting the
// `failure` conclusion alone; Elasticsearch listed every workflow that ran,
// with a success rate beside it. Each store is held here to the same
// question: a failure is any conclusion but success, and a workflow is listed
// past keepsFailing of them.
func TestWorkflowsThatKeepFailingAskTheSameThresholdEverywhere(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := mustPanel(t, rendered(t, store.Name), "Workflows that keep failing")
		text := queriesOf(p)
		var want []string
		switch store.Name {
		case "influxdb", "postgres":
			want = []string{fmt.Sprintf("HAVING SUM(CASE WHEN conclusion <> 'success' THEN 1 ELSE 0 END) > %d", keepsFailing)}
		case "prometheus":
			want = []string{`conclusion!="success"`, fmt.Sprintf("> %d", keepsFailing)}
		case "graphite":
			want = []string{
				fmt.Sprintf(`exclude(%s, "%s")`, rp(ciRun, "duration_seconds"), notSucceeded()),
				fmt.Sprintf(`"sum", ">", %d)`, keepsFailing),
			}
			if strings.Contains(text, ".failure.") {
				t.Errorf("graphite counts the failure conclusion alone, not every one but success:\n%s", text)
			}
		case "elasticsearch":
			continue // evaluated below, on documents
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: Workflows that keep failing lacks %s, so it does not ask what the "+
					"other stores ask:\n%s", store.Name, w, text)
			}
		}
	}
}

// queriesOf is every query of a panel as its store reads it, one per line.
func queriesOf(p map[string]any) string {
	var out []string
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		for _, key := range []string{"rawSql", "expr", "target", "query"} {
			if s, _ := target[key].(string); s != "" {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "\n")
}

// TestElasticsearchKeepsOnlyTheWorkflowsThatKeepFailing evaluates the
// Elasticsearch twin over the runs of two workflows: one that failed four
// times in five, once of them canceled, and one that failed once in two. The
// first is the only row, with the failures and the share the SQL stores
// select.
func TestElasticsearchKeepsOnlyTheWorkflowsThatKeepFailing(t *testing.T) {
	t.Parallel()
	var docs []esDoc
	run := func(workflow, conclusion string) {
		docs = append(docs, esDoc{
			"full_name": "alice/site", "repo": "site", "workflow": workflow,
			"conclusion": conclusion, "success": conclusion == "success",
		})
	}
	for _, c := range []string{"failure", "failure", cancelledRun, "timed_out", "success"} {
		run("ci.yml", c)
	}
	run("pages.yml", "failure")
	run("pages.yml", "success")
	rows := evalESPanel(t, mustPanel(t, rendered(t, "elasticsearch"), "Workflows that keep failing"), docs)
	if len(rows) != 1 {
		t.Fatalf("Elasticsearch lists %d workflows, want the one past %d failures: %v", len(rows), keepsFailing, rows)
	}
	for column, want := range map[string]any{
		"Workflow": "ci.yml", "Failures": 4.0, "Runs": 5.0, ciFailureRate: 0.8,
	} {
		if got := rows[0][column]; got != want {
			t.Errorf("%s reads %v, want %v: %v", column, got, want, rows[0])
		}
	}
}

// TestGraphiteCountsEveryConclusionButSuccessAsAFailure holds the expression
// Graphite's exclude() drops the successful runs by to the conclusion's own
// node: a path is built the way the sink builds one, from the tag table, and
// a workflow that happens to be called success is not a successful run.
func TestGraphiteCountsEveryConclusionButSuccessAsAFailure(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(notSucceeded())
	path := func(conclusion, workflow string) string {
		values := map[string]string{
			"actor": "alice", "conclusion": conclusion, "event": "push", "full_name": "alice_site",
			"owner": "alice", "repo": "site", "workflow": workflow,
		}
		parts := []string{"github", strings.TrimPrefix(ciRun, "gh_")}
		for _, tag := range tagsOf(ciRun) {
			parts = append(parts, values[tag])
		}
		return strings.Join(append(parts, "duration_seconds"), ".")
	}
	for conclusion, dropped := range map[string]bool{
		"success": true, "failure": false, cancelledRun: false, "timed_out": false, "skipped": false,
	} {
		if p := path(conclusion, "ci"); re.MatchString(p) != dropped {
			t.Errorf("%s: dropped %v, want %v", p, !dropped, dropped)
		}
	}
	if p := path("failure", "success"); re.MatchString(p) {
		t.Errorf("%s is a failed run of a workflow called success, and it is dropped", p)
	}
}
