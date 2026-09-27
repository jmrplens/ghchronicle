package dashboards

import (
	"regexp"
	"slices"
	"testing"
)

// TestEveryGraphiteTableThatAddsItsPointsUpConsolidatesBySum is what a review
// of the 2.6.1 audit found in "Discussion answers", and the same hole in every
// Graphite table whose reducer is a sum.
//
// A table sends no maxDataPoints, so Grafana asks for the panel's width in
// pixels, and graphite-web fits the points into it by averaging neighbors.
// Against graphiteapp/graphite-statsd:1.1.10-5 and this repository's storage
// schema, three comments over now-30d read 3 with no maxDataPoints and 1.5 at
// 500, the width the dashboard checker sends: each comment's 1 averaged with
// the 0 isNonNull put beside it, then added up by the table. Consolidated by
// sum they read 3, and aliasSub takes back the name consolidateBy gives each
// series, which is the text of the row.
func TestEveryGraphiteTableThatAddsItsPointsUpConsolidatesBySum(t *testing.T) {
	t.Parallel()
	summed := regexp.MustCompile(`^removeEmptySeries\(aliasSub\(consolidateBy\(.*, "sum"\), ` +
		regexp.QuoteMeta(`"^consolidateBy\((.*),.sum.\)$", "\1"))`) + `$`)
	var titles []string
	for _, p := range renderedPanels(t, "graphite") {
		if p["type"] != "table" || !slices.Contains(graphiteReducers(p), any("sum")) {
			continue
		}
		title, _ := p["title"].(string)
		titles = append(titles, title)
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			expr, _ := target["target"].(string)
			if !summed.MatchString(expr) {
				t.Errorf("the Graphite table %q adds its points up and lets graphite-web average "+
					"them first, so a narrow panel halves its counts: %s", title, expr)
			}
		}
	}
	if !slices.Contains(titles, "Discussion answers") {
		t.Fatalf("Discussion answers is not among the Graphite tables that add up, %v, so this "+
			"no longer checks the table it was written for", titles)
	}
}

// TestNoDashboardTextNamesACadenceKeyTheParserRefuses: the Failed job output
// note told every reader to set `every.joblogs`, and since 2.6.0 the loader
// refuses a family's cadence anywhere but under every.families, with "every:
// is now three layers". The documentation pages were corrected and this
// generated text was not.
func TestNoDashboardTextNamesACadenceKeyTheParserRefuses(t *testing.T) {
	t.Parallel()
	flat := regexp.MustCompile("`every\\.([a-z_]+)")
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			text := asJSON(t, p)
			for _, m := range flat.FindAllStringSubmatch(text, -1) {
				if !slices.Contains([]string{"default", "groups", "families"}, m[1]) {
					title, _ := p["title"].(string)
					t.Errorf("%s: %q names `every.%s`, which the loader refuses: a family's "+
						"cadence is every.families.%s", store.Name, title, m[1], m[1])
				}
			}
		}
	}
}
