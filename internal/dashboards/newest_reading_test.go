package dashboards

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestSecurityFeaturesReadTheNewestReadingOfEachFeature is what the
// documentation audit of 2.6.0 found in "Security features". The table took
// MAX(enabled) and MAX(open_alerts) over the range, so an alert fixed inside
// the range counted at its peak, and a feature switched off inside it read as
// on: the one fact the table exists to say, turned around for as long as the
// range held a reading from before.
//
// Each store is held to the newest reading of each repository's feature, in
// its own terms: the SQL stores number the rows of each full name and feature
// newest first and keep the first; Elasticsearch buckets each full name and
// feature by its newest timestamp before it reads the flag and the count;
// Graphite reduces each series to its last value, consolidated by the last
// value too, so a narrow panel that fits two readings into one point does not
// draw their mean; and Prometheus asks an instant query, which is the value
// as it stands and nothing older.
func TestSecurityFeaturesReadTheNewestReadingOfEachFeature(t *testing.T) {
	t.Parallel()
	newest := regexp.MustCompile(`ROW_NUMBER\(\) OVER \(PARTITION BY full_name, feature ORDER BY time DESC\) AS rn` +
		`.*\) x WHERE rn = 1\b`)
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Security features", "table")
		targets := targetList(p)
		if len(targets) == 0 {
			t.Fatalf("%s: Security features has no query", store.Name)
		}
		switch store.Name {
		case "influxdb", "postgres":
			sql := allSQL(p)
			if !newest.MatchString(sql) || strings.Contains(sql, "MAX(") {
				t.Errorf("%s: Security features does not read the newest row of each "+
					"repository's feature:\n%s", store.Name, sql)
			}
		case "elasticsearch":
			checkNewestFeatureDocument(t, p, targets[0].(map[string]any))
		case "graphite":
			expr, _ := targets[0].(map[string]any)["target"].(string)
			if got := graphiteReducers(p); !slices.Equal(got, []any{"lastNotNull"}) ||
				!strings.Contains(expr, `consolidateBy(`+rp("gh_security_feature", "open_alerts")+`, "last")`) {
				t.Errorf("graphite: Security features reduces each series by %v over %s, want "+
					"its last value, consolidated by the last", got, expr)
			}
		case "prometheus":
			checkInstant(t, "Security features", targets)
		}
	}
}

// checkNewestFeatureDocument holds the Elasticsearch target of Security
// features to a bucket per full name and feature, then its one newest
// timestamp, then the flag and the url of that document, and the table to
// words for the flag as the bucket key's text.
func checkNewestFeatureDocument(t *testing.T, p, target map[string]any) {
	t.Helper()
	buckets, _ := target["bucketAggs"].([]any)
	fields := bucketFieldsOf(buckets)
	want := []any{"full_name.keyword", "repo.keyword", "feature.keyword", "@timestamp", "enabled", "url.keyword"}
	if !slices.Equal(fields, want) {
		t.Fatalf("elasticsearch: Security features buckets by %v, want %v: each full name "+
			"and feature, then its newest timestamp, and the flag and the url of that", fields, want)
	}
	settings, _ := buckets[3].(map[string]any)["settings"].(map[string]any)
	if settings["size"] != "1" || settings["orderBy"] != "_key" || settings["order"] != "desc" {
		t.Errorf("elasticsearch: the timestamp bucket of Security features keeps %v, "+
			"want the one newest", settings)
	}
	// The flag reaches the table as the bucket key's text, since the url is
	// bucketed below it, and the words are mapped from that.
	if raw := asJSON(t, p["fieldConfig"]); !strings.Contains(raw, `"true":{"color":"green","index":2,"text":"on"}`) ||
		!strings.Contains(raw, `"false":{"color":"text","index":3,"text":"off"}`) {
		t.Errorf("elasticsearch: Security features does not map the flag's text to a word: %s", raw)
	}
}

// checkInstant holds every Prometheus target of a table to an instant query
// of the value as it stands, over no range.
func checkInstant(t *testing.T, title string, targets []any) {
	t.Helper()
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		expr, _ := target["expr"].(string)
		if target["instant"] != true || strings.Contains(expr, "_over_time") {
			t.Errorf("prometheus: %s asks %s over a range rather than as it stands", title, expr)
		}
	}
}

// TestArtifactStorageCountedReadsTheNewestRowOfEachRepository: the table put
// MAX() of each of its four counts over the range side by side, so the live
// size was the largest the range had held, above the newest one the tile
// beside it reads, and Walked could come from a sweep that read further than
// the one Declared came from. A walk cut short by a failed page writes a row
// whose Walked is short on purpose, which is how that row says its live size
// is a floor, and the largest Walked of the range hid it.
//
// The four columns now come from one row, the newest of each repository in
// the SQL stores. Elasticsearch already read the newest document's own four
// numbers and Prometheus the four as they stand, and Graphite keeps the live
// size alone, the last value of its series.
func TestArtifactStorageCountedReadsTheNewestRowOfEachRepository(t *testing.T) {
	t.Parallel()
	newest := regexp.MustCompile(`ROW_NUMBER\(\) OVER \(PARTITION BY full_name ORDER BY time DESC\) AS rn` +
		`.*\) x WHERE rn = 1\b`)
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Artifact storage counted", "table")
		targets := targetList(p)
		switch store.Name {
		case "influxdb", "postgres":
			sql := allSQL(p)
			if !newest.MatchString(sql) || strings.Contains(sql, "MAX(") {
				t.Errorf("%s: Artifact storage counted does not take its four counts from "+
					"each repository's newest row:\n%s", store.Name, sql)
			}
		case "elasticsearch":
			metrics, _ := targets[0].(map[string]any)["metrics"].([]any)
			if len(metrics) != 1 || metrics[0].(map[string]any)["type"] != "top_metrics" {
				t.Errorf("elasticsearch: Artifact storage counted reads %v, want the newest "+
					"document's own numbers", metrics)
			}
		case "graphite":
			if got := graphiteReducers(p); !slices.Equal(got, []any{"lastNotNull"}) {
				t.Errorf("graphite: Artifact storage counted reduces its series by %v, want "+
					"the last value", got)
			}
		case "prometheus":
			checkInstant(t, "Artifact storage counted", targets)
		}
	}
}

// graphiteReducers is the reducers of a Graphite table's reduce
// transformation, which is how each series becomes the one number of its row.
func graphiteReducers(p map[string]any) []any {
	transformations, _ := p["transformations"].([]any)
	for _, raw := range transformations {
		tf, _ := raw.(map[string]any)
		if tf["id"] != "reduce" {
			continue
		}
		options, _ := tf["options"].(map[string]any)
		reducers, _ := options["reducers"].([]any)
		return reducers
	}
	return nil
}
