package dashboards

import (
	"strings"
	"testing"
)

// TestNoElasticsearchNameReachesTheScreenAsTheParserSpellsIt is the third of
// the 2.6.1 review's findings about raw names in Elasticsearch: the legends
// of "Time to merge over time" and "Run duration over time" read
// "p50.0 seconds_to_merge" and "p95.0 duration_seconds" where the SQL stores
// read Median and 95th percentile, and "Slowest steps" opened with a column
// headed full_name.keyword.
//
// So a series with no bucket to be named by carries a name of its own, the
// target's alias or the panel's display name, and every bucket a table groups
// by is renamed or dropped before the table is drawn.
func TestNoElasticsearchNameReachesTheScreenAsTheParserSpellsIt(t *testing.T) {
	t.Parallel()
	series, columns := 0, 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		switch p["type"] {
		case "timeseries":
			series += checkSeriesNamed(t, p)
		case "table":
			columns += checkBucketsRenamed(t, p)
		}
	}
	if series == 0 || columns == 0 {
		t.Fatalf("checked %d series and %d columns, so this checked nothing", series, columns)
	}
}

// checkBucketsRenamed holds every terms bucket of a table to leaving the
// screen under the name the SQL stores give it, or not at all, and reports
// how many it checked.
func checkBucketsRenamed(t *testing.T, p map[string]any) int {
	t.Helper()
	handled := tableRenames(p)
	checked := 0
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		buckets, _ := target["bucketAggs"].([]any)
		for _, b := range buckets {
			bucket, _ := b.(map[string]any)
			field, _ := bucket["field"].(string)
			if field == "" || bucket["type"] != "terms" {
				continue
			}
			checked++
			if !handled[field] {
				t.Errorf("%q draws the bucket %s under that name: rename it to the "+
					"column the SQL stores select, or hide it", p["title"], field)
			}
		}
	}
	return checked
}

// namedByBucket reports whether a target's series are named after a bucket's
// values, which is the data naming them rather than the parser.
func namedByBucket(target map[string]any) bool {
	buckets, _ := target["bucketAggs"].([]any)
	for _, b := range buckets {
		if b.(map[string]any)["type"] == "terms" {
			return true
		}
	}
	return false
}

// checkSeriesNamed holds every target of a chart that draws a series per
// metric, rather than per bucket, to a name the panel gives it, and reports
// how many it checked.
func checkSeriesNamed(t *testing.T, p map[string]any) int {
	t.Helper()
	config, _ := p["fieldConfig"].(map[string]any)
	defaults, _ := config["defaults"].(map[string]any)
	display, _ := defaults["displayName"].(string)
	checked := 0
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		if namedByBucket(target) || target["expression"] != nil {
			continue
		}
		checked++
		if alias, _ := target["alias"].(string); display != "" || alias != "" && !strings.Contains(alias, "{{") {
			continue
		}
		t.Errorf("%q, query %v: its series is drawn under the name the response parser "+
			"makes of %v; give the target the alias the SQL stores' column has",
			p["title"], target["refId"], target["metrics"])
	}
	return checked
}

// tableRenames is every field an Elasticsearch table's transformations rename,
// hide or fold away, which are the three ways a bucket leaves the screen.
func tableRenames(p map[string]any) map[string]bool {
	out := map[string]bool{}
	tfs, _ := p["transformations"].([]any)
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		for _, key := range []string{"renameByName", "excludeByName"} {
			m, _ := options[key].(map[string]any)
			for field := range m {
				out[field] = true
			}
		}
		if fields, _ := options["fields"].(map[string]any); tf["id"] == "groupBy" {
			for field := range fields {
				out[field] = true
			}
		}
	}
	return out
}
