package dashboards

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestNoGraphiteChartIsFittedIntoBandsALateStep holds every Graphite chart
// over time to a point per bucket and to asking for more points than that, so
// that graphite-web never fits it into bands. Fitting it moved every point one
// step later, and in the step before each band boundary the newest point of a
// chart was past its right edge: the 2.6.1 review found four charts reading
// "Data outside time range" for it, and a point written at 15:30 UTC and read
// at 15:59 over thirty days came back stamped 16:00 (see grBin).
//
// A chart that follows the range bins by its floor's bucket variable, which
// is the bucket the SQL twins bin by; a chart pinned to its own range, or one
// over rows already a day or a week apart, bins by a fixed bucket, which
// cannot reach the points it asks for inside the twelve years the daily
// archive keeps.
func TestNoGraphiteChartIsFittedIntoBandsALateStep(t *testing.T) {
	t.Parallel()
	doc := graphiteDashboard(t)
	charts := 0
	for _, p := range renderedPanels(t, "graphite") {
		targets := panelTargets(p)
		if p["type"] != "timeseries" || len(targets) == 0 {
			continue
		}
		charts++
		floor, _ := p["interval"].(string)
		_, pinned := p["timeFrom"].(string)
		points, _ := p["maxDataPoints"].(int)
		if points < grMaxDataPoints {
			t.Errorf("%q asks for %d points, fewer than a binned chart can hold", p["title"], points)
		}
		for _, target := range targets {
			expr, _ := target["target"].(string)
			checkGraphiteBins(t, p, expr, floor != "" && !pinned, points)
		}
	}
	if charts < 30 {
		t.Fatalf("only %d Graphite charts over time, too few for this to have held anything", charts)
	}
	// And every bucket variable a chart reads is there, hidden, and computed
	// the way a panel's $__interval is: over thirty days a hundredth is seven
	// hours and twelve minutes, which Grafana rounds to six.
	got, err := grafana.AutoIntervals(doc, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{grBucketVar("1d"): "1d", grBucketVar("1h"): "6h", grBucketVar("5m"): "6h"}
	if len(got) != len(want) {
		t.Errorf("the Graphite dashboard's bucket variables come to %v over thirty days, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s comes to %q over thirty days, want %q", name, got[name], value)
		}
	}
	templating, _ := doc["templating"].(map[string]any)
	for _, raw := range templating["list"].([]any) {
		if v, _ := raw.(map[string]any); v["type"] == "interval" && v["hide"] != 2 {
			t.Errorf("the bucket variable %v is offered to the reader, who has nothing to pick in it", v["name"])
		}
	}
}

// checkGraphiteBins holds one target of a Graphite chart to summarizing every
// series it draws, by its floor's variable when the chart follows the range,
// and otherwise by a fixed bucket twelve years hold fewer of than the points
// the chart asks for.
func checkGraphiteBins(t *testing.T, p map[string]any, expr string, followsRange bool, points int) {
	t.Helper()
	floor, _ := p["interval"].(string)
	bins := summarizeBuckets(t, expr)
	if len(bins) == 0 {
		t.Errorf("%q draws a series at the storage step, which graphite-web fits into bands "+
			"past a width the reader's retention decides:\n%s", p["title"], expr)
	}
	for _, bin := range bins {
		if followsRange {
			if bin != grBin(floor) {
				t.Errorf("%q bins by %s, want its floor's variable %s, the bucket the SQL twins bin by:\n%s",
					p["title"], bin, grBin(floor), expr)
			}
			continue
		}
		length, err := time.ParseDuration(fixedBucket(t, bin))
		if err != nil || int(12*365*24*time.Hour/length) > points {
			t.Errorf("%q bins by %s, which twelve years cut into more points than the %d it asks for:\n%s",
				p["title"], bin, points, expr)
		}
	}
}

// graphiteDashboard is the exported Graphite dashboard, variables and all.
func graphiteDashboard(t *testing.T) map[string]any {
	t.Helper()
	for _, s := range AllStores() {
		if s.Name == "graphite" {
			return s.Build(nil)
		}
	}
	t.Fatal("no graphite store")
	return nil
}

// summarizeBuckets is the bucket of every summarize in a Graphite expression.
func summarizeBuckets(t *testing.T, expr string) []string {
	t.Helper()
	p := &exprParser{t: t, s: expr}
	tree := parseGraphiteCall(p)
	var out []string
	var walk func(c grCall)
	walk = func(c grCall) {
		if c.fn == "summarize" && len(c.args) > 1 {
			out = append(out, c.args[1].word)
		}
		for _, a := range c.args {
			walk(a)
		}
	}
	walk(tree)
	return out
}

// fixedBucket is a fixed Graphite bucket as a Go duration.
func fixedBucket(t *testing.T, bucket string) string {
	t.Helper()
	var n int
	var unit string
	if _, err := fmt.Sscanf(bucket, "%d%s", &n, &unit); err != nil || !slices.Contains([]string{"d", "h"}, unit) {
		return "not a fixed bucket: " + bucket
	}
	if unit == "d" {
		return fmt.Sprintf("%dh", n*24)
	}
	return fmt.Sprintf("%dh", n)
}
