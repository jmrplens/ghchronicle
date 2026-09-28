//go:build dockere2e

package docker

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// ── 5. A Prometheus query over series the sweep does not write ────────────

// The containerised dashboards are asked about one sweep of one fixture, and a
// Prometheus query that is wrong only for series that fixture never makes is
// right as far as they can tell. The exporter makes a series for a label value
// once it has seen one and keeps every series it has seen, so two shapes the
// fixture has none of are everyday ones: a label value that never occurred, a
// signature nobody gave, and a series that stood still over the range, a type
// of event seen once weeks ago. Both reached the screen in 2.6.2: a Signed
// commits tile reading "no commits" beside a Commits tile of 57, and a pie of
// event types naming six types that had no event and a slice of other at 0.
//
// So each case here takes the panel's own query out of the committed
// Prometheus dashboard, puts it to promtool's rule test inside the stack's
// Prometheus over series written for the case, and holds it to what the SQL
// stores answer over the same facts.

// promqlCase is one query of one panel over series of the case's own.
type promqlCase struct {
	title, ref string
	// series is promtool's input: a series and its values, a sample every
	// five minutes, the range ending an hour in.
	series map[string]string
	// want is every sample the query answers, by its label set as promtool
	// prints it, which is what the SQL stores answer over the same facts.
	want map[string]float64
}

// promqlCases are the shapes the fixture has none of.
var promqlCases = map[string]promqlCase{
	// An account that never signed a commit has no series of a valid
	// signature: 0 of its commits are signed, as the SQL stores read it.
	"signed commits over unsigned ones": {
		title: "Commits", ref: "D",
		series: map[string]string{`github_commits_total{signature="UNSIGNED",repo="r"}`: "0+1x12"},
		want:   map[string]float64{"{}": 0},
	},
	"signed commits over no commit in the range": {
		title: "Commits", ref: "D",
		series: map[string]string{`github_commits_total{signature="VALID",repo="r"}`: "3x12"},
		want:   map[string]float64{"{}": nan},
	},
	"a success rate over failed runs alone": {
		title: "Runs in range", ref: "B",
		series: map[string]string{`github_workflow_runs_total{conclusion="failure",repo="r"}`: "0+1x12"},
		want:   map[string]float64{"{}": 0},
	},
	"a success rate over canceled runs alone": {
		title: "Runs in range", ref: "B",
		series: map[string]string{`github_workflow_runs_total{conclusion="cancel` + `led",repo="r"}`: "0+1x12"},
		want:   map[string]float64{"{}": nan},
	},
	"a failure rate over deliveries that got through": {
		title: "Webhook failure rate", ref: "A",
		series: map[string]string{`github_webhook_deliveries_total{ok="true",repo="r"}`: "0+1x12"},
		want:   map[string]float64{"{}": 0},
	},
	"minutes wasted by a repository whose runs all succeeded": {
		title: "Minutes spent on failed runs", ref: "A",
		series: map[string]string{
			`github_workflow_runs_total{conclusion="success",full_name="a/r",repo="r"}`: "0+1x12",
			`github_workflow_runs_duration_seconds_mean{full_name="a/r",repo="r"}`:      "60x12",
		},
		want: map[string]float64{`{full_name="a/r", repo="r"}`: 0},
	},
	"the share wasted by a repository whose runs all succeeded": {
		title: "Minutes spent on failed runs", ref: "C",
		series: map[string]string{
			`github_workflow_runs_total{conclusion="success",full_name="a/r",repo="r"}`: "0+1x12",
			`github_workflow_runs_duration_seconds_mean{full_name="a/r",repo="r"}`:      "60x12",
		},
		want: map[string]float64{`{full_name="a/r", repo="r"}`: 0},
	},
	// Nine types seen, two of them with events in the range: the SQL stores
	// have a row for each of the two and nothing else.
	"event types of which most stood still": {
		title: "Events by type", ref: "A",
		series: map[string]string{
			`github_events_total{type="A"}`: "0+12x12", `github_events_total{type="B"}`: "0+6x12",
			`github_events_total{type="C"}`: "5x12", `github_events_total{type="D"}`: "5x12",
			`github_events_total{type="E"}`: "5x12", `github_events_total{type="F"}`: "5x12",
			`github_events_total{type="G"}`: "5x12", `github_events_total{type="H"}`: "5x12",
			`github_events_total{type="I"}`: "5x12",
		},
		want: map[string]float64{`{type="A"}`: 144, `{type="B"}`: 72},
	},
	// Nine types over time, the smallest over the range folded: at each step
	// the eight of the range are named and the ninth is other, as the SQL
	// stores fold a chart, rather than a topk of each step.
	"a chart of nine event types": {
		title: "Events over time", ref: "A",
		series: func() map[string]string {
			out := map[string]string{}
			for i := 1; i <= 9; i++ {
				out[fmt.Sprintf(`github_events_total{type="T%d"}`, i)] = fmt.Sprintf("0+%dx12", i)
			}
			return out
		}(),
		want: func() map[string]float64 {
			// Each step is ten minutes, two samples of the five minutes the
			// series are written at.
			out := map[string]float64{`{type="other"}`: 2}
			for i := 2; i <= 9; i++ {
				out[fmt.Sprintf(`{type="T%d"}`, i)] = float64(2 * i)
			}
			return out
		}(),
	},
}

// nan is what a case wants where the SQL stores answer null: a stat of the
// store draws NaN as it draws null, as the words its panel gives a value
// that is not there.
var nan = math.NaN()

func TestPrometheusAnswersSeriesTheFixtureHasNoneOfAsTheSQLStoresDo(t *testing.T) {
	s := Start(t)
	doc, err := dashboardDocument("prometheus")
	if err != nil {
		t.Fatal(err)
	}
	panels := grafana.Panels(doc["panels"])
	for name, c := range promqlCases {
		t.Run(name, func(t *testing.T) {
			expr := promqlOf(t, panels, c.title, c.ref)
			file := "promql-" + strings.NewReplacer(" ", "-", ",", "").Replace(name) + ".yml"
			local := filepath.Join("out", "prometheus-targets", file)
			if writeErr := os.WriteFile(local, []byte(promtoolTest(expr, c)), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			t.Cleanup(func() { _ = os.Remove(local) })
			out, testErr := s.Exec(t.Context(), "prometheus", "promtool", "test", "rules", "/etc/prometheus/targets/"+file)
			if testErr != nil {
				t.Errorf("%q query %s over %v does not answer what the SQL stores answer over the same "+
					"facts, %v:\n%s", c.title, c.ref, c.series, c.want, out)
			}
		})
	}
}

// promqlOf is one query of a panel as a render sends it over an hour, every
// repository picked and a bucket of ten minutes.
func promqlOf(t *testing.T, panels []grafana.PanelQuery, title, ref string) string {
	t.Helper()
	for _, p := range panels {
		if p.Title != title {
			continue
		}
		for _, target := range p.Targets {
			if target["refId"] != ref {
				continue
			}
			expr, _ := target["expr"].(string)
			return strings.NewReplacer("$__range", "1h", "$__interval", "10m", "$repo", ".*").Replace(expr)
		}
	}
	t.Fatalf("the Prometheus dashboard has no query %s in a panel called %q", ref, title)
	return ""
}

// promtoolTest is the rule test file for one case: its series, and the query
// evaluated an hour in with the samples it has to answer.
//
// promtool holds a NaN to be different from a NaN (measured with Prometheus
// 3.14.0: "exp: {} NaN, got: {} NaN" fails), so a case that wants the null the
// SQL stores answer asks whether the answer differs from itself, which only a
// NaN does, and wants 1.
func promtoolTest(expr string, c promqlCase) string {
	var b strings.Builder
	b.WriteString("tests:\n  - interval: 5m\n    input_series:\n")
	for series, values := range c.series {
		fmt.Fprintf(&b, "      - series: %q\n        values: %q\n", series, values)
	}
	for _, value := range c.want {
		if math.IsNaN(value) {
			expr = fmt.Sprintf("(%s) != bool (%s)", expr, expr)
		}
	}
	fmt.Fprintf(&b, "    promql_expr_test:\n      - expr: %q\n        eval_time: 1h\n        exp_samples:\n", expr)
	for labels, value := range c.want {
		if math.IsNaN(value) {
			value = 1
		}
		fmt.Fprintf(&b, "          - labels: %q\n            value: %v\n", labels, value)
	}
	return b.String()
}
