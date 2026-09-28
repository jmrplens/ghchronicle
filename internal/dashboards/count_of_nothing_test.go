package dashboards

import (
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestACountOfNothingIsZeroInEveryStore holds every count a stat or a gauge
// draws to the answer the SQL stores give over no rows, COUNT(*)'s 0. Comparing
// what the five stores draw, "Undecided runs" was a tile reading 0 in three
// dashboards and was missing from Graphite and Elasticsearch, and "Issues
// closed" from Elasticsearch: Graphite has no series to count for a path it
// has never held, and an Elasticsearch terms bucket keeps no term without a
// document. So a Graphite count falls back to the constant 0, and an
// Elasticsearch count asks for its empty buckets as well.
func TestACountOfNothingIsZeroInEveryStore(t *testing.T) {
	t.Parallel()
	if graphiteCountsFallBack(t)+elasticsearchCountsKeepEmptyBuckets(t) == 0 {
		t.Fatal("no count in any stat of either store, so this checked nothing")
	}
}

// graphiteCounts is every way a Graphite target counts: the points of a path
// added up, which is countTotal's, and the series that are left, which is how
// "Downloads" counts the releases anybody downloaded. countSeries answers
// nothing for a list that is empty once removeEmptySeries has run, and the
// Releases tile was missing from Graphite where the SQL stores read 0, over a
// range before any release and for a repository whose releases nobody
// downloaded (measured against graphite-web 1.1.10).
var graphiteCounts = []string{"summarize(sumSeries(isNonNull(", "countSeries("}

// graphiteCountsFallBack holds every count of a Graphite stat to falling back
// to the constant 0, and says how many it found.
func graphiteCountsFallBack(t *testing.T) int {
	t.Helper()
	counts := 0
	for _, p := range renderedPanels(t, "graphite") {
		if p["type"] != "stat" && p["type"] != "gauge" {
			continue
		}
		for _, target := range panelTargets(p) {
			expr, _ := target["target"].(string)
			for _, count := range graphiteCounts {
				for rest := expr; ; {
					at := strings.Index(rest, count)
					if at < 0 {
						break
					}
					counts++
					if !strings.HasSuffix(rest[:at], "fallbackSeries(") {
						t.Errorf("graphite %q counts with no fallback, so a path never held draws no tile "+
							"where the SQL stores draw 0:\n%s", p["title"], expr)
					}
					rest = rest[at+1:]
				}
			}
		}
	}
	return counts
}

// elasticsearchCountsKeepEmptyBuckets holds every total of an Elasticsearch
// stat to asking for an empty bucket when it counts and to not asking for one
// otherwise, and says how many counts it found.
func elasticsearchCountsKeepEmptyBuckets(t *testing.T) int {
	t.Helper()
	counts := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		if p["type"] != "stat" && p["type"] != "gauge" {
			continue
		}
		for _, target := range panelTargets(p) {
			metric, bucket := oneMetricOneBucket(target)
			if bucket["field"] != "measurement.keyword" {
				continue
			}
			settings, _ := bucket["settings"].(map[string]any)
			want := "1"
			if metric["type"] == "count" || metric["type"] == "cardinality" {
				want = "0"
				counts++
			}
			if settings["min_doc_count"] != want {
				t.Errorf("elasticsearch %q asks a %v for min_doc_count %v, want %s: a count of nothing "+
					"is 0, and anything else read over an empty bucket is 0 where it should be nothing",
					p["title"], metric["type"], settings["min_doc_count"], want)
			}
		}
	}
	return counts
}

// oneMetricOneBucket is the metric and the bucket of a target that has one of
// each, and nothing otherwise.
func oneMetricOneBucket(target map[string]any) (metric, bucket map[string]any) {
	metrics, _ := target["metrics"].([]any)
	buckets, _ := target["bucketAggs"].([]any)
	if len(metrics) != 1 || len(buckets) != 1 {
		return nil, nil
	}
	metric, _ = metrics[0].(map[string]any)
	bucket, _ = buckets[0].(map[string]any)
	return metric, bucket
}

func panelTargets(p map[string]any) []map[string]any {
	var out []map[string]any
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		if target, ok := raw.(map[string]any); ok {
			out = append(out, target)
		}
	}
	return out
}

// TestNoReleaseAnybodyDownloadedIsZeroReleasesInGraphite evaluates the
// Releases count of "Downloads" the way graphite-web does, over a repository
// whose one release nobody has downloaded: COUNT(*) reads 0 there, and
// Graphite drew no tile at all.
func TestNoReleaseAnybodyDownloadedIsZeroReleasesInGraphite(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		if store.Name != "graphite" {
			continue
		}
		allValue, _ := store.Variable["allValue"].(string)
		vars := grafana.Vars{Datasource: store.DS, AllValue: allValue}
		target := targetOf(t, mustPanel(t, rendered(t, store.Name), "Downloads"), "B")
		expr, _ := vars.Apply(target)["target"].(string)
		nobody := []grSeries{{name: "github.release.false.alice_x.alice.false.x.v1_0_0.downloads", value: 0}}
		if got := evalGraphite(t, expr, nobody); got != 0 {
			t.Errorf("%s counts %v releases downloaded where nobody downloaded one", expr, got)
		}
		return
	}
	t.Fatal("no graphite store")
}
