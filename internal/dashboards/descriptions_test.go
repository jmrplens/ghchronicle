package dashboards

import (
	"slices"
	"strings"
	"testing"
)

// The documentation audit of 2.6.0 read every panel description against what
// the collector and the queries do, and found sentences that had been true of
// an earlier release or of one store and were read in all five. Each test here
// pins one of those corrections to the rendered dashboards, which is where a
// reader meets the sentence.

// descriptionOf is a rendered panel's description, the shared sentence and the
// store's own joined as the reader sees them.
func descriptionOf(p map[string]any) string {
	desc, _ := p["description"].(string)
	return desc
}

// TestEveryArtifactFloorNamesBothCauses: the three panels that say the
// artifact size is a floor blamed the five page cap alone. Since 2.6.0 a walk
// cut short by a failed page writes the pages it read, with a Walked short of
// Declared, and that row is a floor for the other reason.
func TestEveryArtifactFloorNamesBothCauses(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "prometheus", "postgres", "graphite", "elasticsearch"} {
		named := 0
		for title, p := range rendered(t, store) {
			desc := descriptionOf(p)
			if !strings.Contains(desc, "floor") || !strings.Contains(strings.ToLower(title+desc), "artifact") {
				continue
			}
			if !strings.Contains(desc, "five hundred") {
				continue // a panel that says floor about something else
			}
			named++
			if !strings.Contains(desc, "fail") {
				t.Errorf("%s: %q calls the artifact size a floor for the page cap alone: %q", store, title, desc)
			}
		}
		if named == 0 {
			t.Errorf("%s: no panel calls the artifact size a floor, so this checks nothing", store)
		}
	}
}

// TestEveryBucketReadsTheExtremesOfTheRange: the SQL stores put the most any
// reading in the range had used and the least any had left under Most used
// and Lowest remaining, while Graphite and Elasticsearch put the newest
// reading there and Prometheus the value as it stands, so a bucket spent to
// its last request read as untouched in three stores once it had refilled.
func TestEveryBucketReadsTheExtremesOfTheRange(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Every bucket", "table")
		targets := targetList(p)
		switch store.Name {
		case "influxdb", "postgres":
			sql := allSQL(p)
			if !strings.Contains(sql, `MAX(used) AS "Most used"`) ||
				!strings.Contains(sql, `MIN(remaining) AS "Lowest remaining"`) {
				t.Errorf("%s: Every bucket does not take the extremes of the range: %s", store.Name, sql)
			}
		case "graphite":
			expr, _ := targets[0].(map[string]any)["target"].(string)
			if got := graphiteReducers(p); !slices.Equal(got, []any{"min"}) ||
				!strings.Contains(expr, `consolidateBy(`+gp("gh_rate_limit", "remaining")+`, "min")`) {
				t.Errorf("graphite: Every bucket reduces the remaining count by %v over %s, want "+
					"the least of the range", got, expr)
			}
		case "elasticsearch":
			metrics, _ := targets[0].(map[string]any)["metrics"].([]any)
			var got []string
			for _, raw := range metrics {
				m, _ := raw.(map[string]any)
				field, _ := m["field"].(string)
				kind, _ := m["type"].(string)
				got = append(got, kind+" "+field)
			}
			if want := []string{"max limit", "min remaining", "max used"}; !slices.Equal(got, want) {
				t.Errorf("elasticsearch: Every bucket reads %v, want %v", got, want)
			}
		case "prometheus":
			exprs := asJSON(t, targets)
			for _, want := range []string{
				"min_over_time(github_rate_limit_remaining[$__range])",
				"max_over_time(github_rate_limit_used[$__range])",
			} {
				if !strings.Contains(exprs, want) {
					t.Errorf("prometheus: Every bucket does not ask %s: %s", want, exprs)
				}
			}
		}
	}
}
