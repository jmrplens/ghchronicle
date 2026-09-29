package dashboards

import (
	"slices"
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

// TestEveryStatValueIsDrawnOverNothing holds every value of every stat group,
// in every store, to something to draw over a range or a repository with
// nothing in it, where the SQL stores draw a count as 0 and anything else as
// the words its panel gives a value that is not there. The 2.6.2 review found
// Graphite and Elasticsearch drawing neither for most of them: over a
// repository with nothing in it eighteen tiles were missing from Graphite and
// twenty from Elasticsearch, and a group whose every value was a total, the
// traffic or the open alerts, was an empty panel in all five stores. The 2.6.3
// review found the account's own snapshots, read as the newest row of the
// range, drawing "No data" in all five over a range no sweep reached, where
// the groups beside them said what the range lacked.
func TestEveryStatValueIsDrawnOverNothing(t *testing.T) {
	t.Parallel()
	checked := sqlNullsHaveWords(t) + graphiteValuesFallBack(t) + prometheusAggregationsFallBack(t) +
		elasticsearchValuesAnswerNothing(t)
	if checked < 60 {
		t.Fatalf("only %d stat values checked, too few for this to have held the stat groups to anything", checked)
	}
}

// sqlNullsHaveWords holds every value a SQL stat reads as null over no rows to
// words of its own, everything but a count, and every statement that reads
// the newest row of a snapshot to answering a row over no rows, since no row
// is no tile.
func sqlNullsHaveWords(t *testing.T) int {
	t.Helper()
	joined, after, _ := strings.Cut(alwaysARow("\x00"), "\x00")
	checked := 0
	for _, p := range renderedPanels(t, "influxdb") {
		if p["type"] != "stat" {
			continue
		}
		for _, target := range panelTargets(p) {
			sql, _ := target["rawSql"].(string)
			inner, always := strings.CutPrefix(sql, joined)
			if always {
				sql = strings.TrimSuffix(inner, after)
			} else if strings.Contains(sql, "ORDER BY time DESC LIMIT 1") {
				t.Errorf("%q reads the newest row of a snapshot, which a range no sweep reached answers "+
					"with no row and so with no tile: %s", p["title"], sql)
			}
			for name, expr := range selectedValues(t, sql) {
				checked++
				if strings.HasPrefix(expr, "COUNT(") {
					continue
				}
				if !saysNothingAs(p, name) {
					t.Errorf("%q: %s is null over no rows and has no words for it, so a group of such "+
						"values over nothing is a panel with nothing in it: %s", p["title"], name, expr)
				}
			}
		}
	}
	return checked
}

// selectedValues is the columns the outer SELECT of a statement names, by
// alias, each with its expression.
func selectedValues(t *testing.T, sql string) map[string]string {
	t.Helper()
	body := strings.TrimPrefix(sql, "SELECT ")
	depth, from := 0, -1
	for i := 0; i < len(body) && from < 0; i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ' ':
			if depth == 0 && strings.HasPrefix(body[i:], " FROM ") {
				from = i
			}
		}
	}
	if from < 0 {
		t.Fatalf("no FROM in %s", sql)
	}
	out := map[string]string{}
	depth, start := 0, 0
	list := body[:from] + ","
	for i := range len(list) {
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth > 0 {
				continue
			}
			expr, name, ok := strings.Cut(strings.TrimSpace(list[start:i]), ` AS "`)
			if !ok {
				t.Fatalf("an unnamed column %q in %s", list[start:i], sql)
			}
			out[strings.TrimSuffix(name, `"`)] = expr
			start = i + 1
		}
	}
	return out
}

// saysNothingAs reports whether a panel draws a value of that name that is
// not there as words, which is a special value mapping of null.
func saysNothingAs(p map[string]any, name string) bool {
	config, _ := p["fieldConfig"].(map[string]any)
	overrides, _ := config["overrides"].([]any)
	for _, raw := range overrides {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		if matcher["id"] != "byName" || matcher["options"] != name {
			continue
		}
		props, _ := o["properties"].([]any)
		for _, rawProp := range props {
			prop, _ := rawProp.(map[string]any)
			mappings, _ := prop["value"].([]any)
			for _, rawMapping := range mappings {
				m, _ := rawMapping.(map[string]any)
				options, _ := m["options"].(map[string]any)
				if prop["id"] == "mappings" && m["type"] == "special" && options["match"] == "null+nan" {
					return true
				}
			}
		}
	}
	return false
}

// graphiteValuesFallBack holds every value of a Graphite stat to a fallback
// for a path the store has never held, which answers no series at all. A path
// Graphite holds answers a range it holds nothing in with nulls, which the
// fallback leaves as they are, a tile the panel's words are drawn in.
func graphiteValuesFallBack(t *testing.T) int {
	t.Helper()
	checked := 0
	for _, p := range renderedPanels(t, "graphite") {
		if p["type"] != "stat" {
			continue
		}
		for _, target := range panelTargets(p) {
			checked++
			if expr, _ := target["target"].(string); !strings.HasPrefix(expr, "alias(fallbackSeries(") {
				t.Errorf("graphite %q: a value with no fallback draws no tile for a path never held: %s",
					p["title"], expr)
			}
		}
	}
	return checked
}

// prometheusAggregationsFallBack holds every value a Prometheus stat reads to
// an aggregation with a value for no series at all: 0 for a count, NaN for
// the rest. A selector alone answers a range Prometheus holds nothing in with
// no series, and no tile.
func prometheusAggregationsFallBack(t *testing.T) int {
	t.Helper()
	checked := 0
	for _, p := range renderedPanels(t, "prometheus") {
		if p["type"] != "stat" {
			continue
		}
		for _, target := range panelTargets(p) {
			checked++
			if expr, _ := target["expr"].(string); !strings.HasSuffix(expr, ") or vector(0)") &&
				!strings.HasSuffix(expr, ") or vector(NaN)") {
				t.Errorf("prometheus %q: a value with no fallback draws no tile over no series: %s", p["title"], expr)
			}
		}
	}
	return checked
}

// elasticsearchValuesAnswerNothing holds every value of an Elasticsearch stat
// that reads the last value of each field to a bucket that is there when no
// document is, unless it counts, which the test above holds; the newest
// document of a snapshot to the one bucket that answers its fields over no
// document without failing, esNewest's; a value the panel adds up per name to
// a query that names its tile over nothing; and every stat that adds its
// values up to saying which of them leave the group over nothing.
func elasticsearchValuesAnswerNothing(t *testing.T) int {
	t.Helper()
	checked := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		if p["type"] != "stat" {
			continue
		}
		if sumsPerName(p) {
			checked += esNamedOverNothing(t, p)
			continue
		}
		options, _ := p["options"].(map[string]any)
		reduce, _ := options["reduceOptions"].(map[string]any)
		if calcs, _ := reduce["calcs"].([]any); len(calcs) > 0 && calcs[0] == "sum" {
			checked++
			if desc, _ := p["description"].(string); !strings.Contains(desc, "leaves its group when the range holds no document of it") {
				t.Errorf("elasticsearch %q adds its values up, which reads a value of nothing as 0, and does "+
					"not say which values leave the group instead: %q", p["title"], desc)
			}
			continue
		}
		for _, target := range panelTargets(p) {
			checked += esValueAnswersNothing(t, p["title"], target)
		}
	}
	return checked
}

// esValueAnswersNothing is elasticsearchValuesAnswerNothing for one target of
// a stat that reads the last value of each field, and says whether it held
// the target to anything.
func esValueAnswersNothing(t *testing.T, title any, target map[string]any) int {
	t.Helper()
	metrics, _ := target["metrics"].([]any)
	buckets, _ := target["bucketAggs"].([]any)
	if len(metrics) == 0 || len(buckets) == 0 {
		return 0
	}
	first, _ := metrics[0].(map[string]any)
	last, _ := buckets[len(buckets)-1].(map[string]any)
	settings, _ := last["settings"].(map[string]any)
	switch first["type"] {
	case "count", "cardinality":
		return 0
	case "top_metrics":
		if len(buckets) != 1 || last["type"] != "date_histogram" || settings["min_doc_count"] != "1" {
			t.Errorf("elasticsearch %q: the newest document over %v answers a range with no document "+
				"in it with no field, and no tile, or fails over it", title, buckets)
		}
		return 1
	}
	if last["type"] != "date_histogram" || settings["min_doc_count"] != "0" {
		t.Errorf("elasticsearch %q: a %v over the range has no bucket to answer nothing in: %v",
			title, first["type"], buckets)
	}
	return 1
}

// sumsPerName reports whether an Elasticsearch panel draws a field per name
// out of rows it makes of every field its queries answer, esSumPerName's
// transformations, which is what lets a query that answers a name and nothing
// else stand in for a value whose own query answers no field over nothing.
func sumsPerName(p map[string]any) bool {
	var ids []any
	for _, raw := range asList(p["transformations"]) {
		tf, _ := raw.(map[string]any)
		ids = append(ids, tf["id"])
	}
	return slices.Contains(ids, "reduce") && slices.Contains(ids, "groupBy") && slices.Contains(ids, "rowsToFields")
}

// esNamedOverNothing holds each query of a panel that sums per name either to
// answering its fields over no document, as esValueAnswersNothing holds a
// query, or to a table of the newest document of each item whose every
// column has a query of the panel that answers its name over any range, and
// says how many values it held to either.
func esNamedOverNothing(t *testing.T, p map[string]any) int {
	t.Helper()
	named := map[string]bool{}
	for _, target := range panelTargets(p) {
		if alias, _ := target["alias"].(string); alias != "" && answersNoValue(target) {
			named[alias] = true
		}
	}
	renamed := map[string]string{}
	for _, raw := range asList(p["transformations"]) {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		rename, _ := options["renameByName"].(map[string]any)
		for from, to := range rename {
			renamed[from], _ = to.(string)
		}
	}
	checked := 0
	for _, target := range panelTargets(p) {
		if _, alias := target["alias"]; alias && answersNoValue(target) {
			continue
		}
		metrics, _ := target["metrics"].([]any)
		buckets, _ := target["bucketAggs"].([]any)
		first, _ := metrics[0].(map[string]any)
		last, _ := buckets[len(buckets)-1].(map[string]any)
		if first["type"] != "top_metrics" || last["type"] == "date_histogram" {
			checked += esValueAnswersNothing(t, p["title"], target)
			continue
		}
		settings, _ := first["settings"].(map[string]any)
		for _, raw := range asList(settings["metrics"]) {
			field, _ := raw.(string)
			checked++
			if name := renamed["Top Metrics "+field]; !named[name] {
				t.Errorf("elasticsearch %q: %s, the newest document of each item over %v, answers a range "+
					"with no document in it with no field, and no query of the panel names its tile %q there",
					p["title"], field, bucketFieldsOf(buckets), name)
			}
		}
	}
	return checked
}

// answersNoValue reports whether a query is esNameOnly's: one century-wide
// bucket, and a script that answers null over it, beside the count it reads.
func answersNoValue(target map[string]any) bool {
	metrics, _ := target["metrics"].([]any)
	buckets, _ := target["bucketAggs"].([]any)
	if len(metrics) != 2 || len(buckets) != 1 {
		return false
	}
	count, _ := metrics[0].(map[string]any)
	script, _ := metrics[1].(map[string]any)
	bucket, _ := buckets[0].(map[string]any)
	settings, _ := script["settings"].(map[string]any)
	text, _ := settings["script"].(string)
	return count["type"] == "count" && count["hide"] == true && script["type"] == "bucket_script" &&
		bucket["type"] == "date_histogram" && strings.HasSuffix(text, ": null") &&
		strings.HasPrefix(text, "params.n < 0 ?")
}

// TestABucketScriptReadsTheMetricsOfItsOwnQuery holds every Elasticsearch
// bucket script to naming metrics its own query has. A bucket script reads the
// metrics it combines by id, and the ids are renumbered in panel order after
// the panels are built, so a reference the renumbering did not carry would
// read whichever metric now held the old number, or none, which the datasource
// answers with an error for the whole panel.
func TestABucketScriptReadsTheMetricsOfItsOwnQuery(t *testing.T) {
	t.Parallel()
	scripts := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		for _, target := range panelTargets(p) {
			metrics, _ := target["metrics"].([]any)
			ids := map[any]map[string]any{}
			for _, raw := range metrics {
				m, _ := raw.(map[string]any)
				ids[m["id"]] = m
			}
			for _, raw := range metrics {
				m, _ := raw.(map[string]any)
				variables, _ := m["pipelineVariables"].([]any)
				if m["type"] != "bucket_script" {
					continue
				}
				scripts++
				for _, rawVar := range variables {
					v, _ := rawVar.(map[string]any)
					if read, ok := ids[v["pipelineAgg"]]; !ok || read["type"] == "bucket_script" {
						t.Errorf("%q: a bucket script reads %v as %v, which is no metric of its query: %v",
							p["title"], v["pipelineAgg"], v["name"], metrics)
					}
				}
			}
		}
	}
	if scripts == 0 {
		t.Fatal("no bucket script in the Elasticsearch dashboard, so this checked nothing")
	}
}
