package dashboards

import (
	"regexp"
	"strings"
	"testing"
)

// The visual half of the 2.6.1 review: the five dashboards were imported into
// the containerised stack's Grafana, every panel was photographed, and what a
// reader saw was compared across the stores. Each test here pins one of the
// answers to the rendered JSON, in every store.

// TestASeriesOfOnePointIsDrawn: a line needs two points, and under
// `showPoints: never` a series whose range holds one value drew nothing while
// its legend read that value. "Time to merge over time" was an empty grid in
// all five stores, and a snapshot written once was one in Graphite and
// Elasticsearch.
func TestASeriesOfOnePointIsDrawn(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			if p["type"] != "timeseries" {
				continue
			}
			checked++
			if got := customOf(t, p)["showPoints"]; got != "auto" {
				t.Errorf("%s %q draws its points %v: a series of one value is drawn as nothing",
					store.Name, p["title"], got)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no timeseries panel was checked")
	}
}

// TestATileOfNothingSaysWhatIsMissing: a stat or a gauge whose value is a
// median, or a share of a count, has no number in a range that holds nothing
// to measure, and Grafana drew it as an empty space under the value's label:
// "Time to close an issue" in InfluxDB, PostgreSQL and Graphite, and the
// success rate, the signed share and the webhook failure rate in Prometheus.
// Each such value carries a sentence for that range, in every store.
func TestATileOfNothingSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	nullable := nullableTiles(t)
	if len(nullable) == 0 {
		t.Fatal("no stat value is a median or a share, so this checked nothing")
	}
	for _, store := range AllStores() {
		panels := rendered(t, store.Name)
		for title, names := range nullable {
			p := mustPanel(t, panels, title)
			if p["type"] == "text" {
				continue
			}
			said := noValues(t, p)
			for _, name := range names {
				if said[name] == "" {
					t.Errorf("%s %q: %s is empty in a range with nothing to measure, and says nothing",
						store.Name, title, name)
				}
			}
		}
	}
}

// nullableMeasure is an aggregate the SQL answers with null over no rows
// rather than with zero: a median, or a count divided by a count.
var nullableMeasure = regexp.MustCompile(`median\(|/ NULLIF\(|/ COUNT\(`)

// nullableTiles is, per stat and gauge panel of the InfluxDB dashboard, the
// name of every value whose SQL can answer null: the column's alias, or
// "value" for a panel of one value.
func nullableTiles(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for title, p := range rendered(t, "influxdb") {
		if p["type"] != "stat" && p["type"] != "gauge" {
			continue
		}
		targets, _ := p["targets"].([]any)
		for _, raw := range targets {
			target, _ := raw.(map[string]any)
			sql, _ := target["rawSql"].(string)
			for _, item := range selectList(sql) {
				// The alias is the last AS of the item: a CAST has one of its own.
				at := strings.LastIndex(item, " AS ")
				if at >= 0 && nullableMeasure.MatchString(item[:at]) {
					out[title] = append(out[title], strings.Trim(item[at+len(" AS "):], `"`))
				}
			}
		}
	}
	return out
}

// selectList is the items of a statement's outermost SELECT, split on the
// commas outside any parenthesis.
func selectList(sql string) []string {
	body, found := strings.CutPrefix(sql, "SELECT ")
	if !found {
		return nil
	}
	var items []string
	depth, start := 0, 0
	for i := range len(body) {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				items = append(items, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
		if depth == 0 && strings.HasPrefix(body[i:], " FROM ") {
			return append(items, strings.TrimSpace(body[start:i]))
		}
	}
	return items
}

// noValues is what a panel reads where a value is missing: per field name
// its override's noValue, and under "value" the panel's own default.
func noValues(t *testing.T, p map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	if v, ok := defaultsOf(t, p)["noValue"].(string); ok {
		out["value"] = v
	}
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		props, _ := o["properties"].([]any)
		for _, rawProp := range props {
			if prop, _ := rawProp.(map[string]any); prop["id"] == "noValue" {
				out[name], _ = prop["value"].(string)
			}
		}
	}
	return out
}
