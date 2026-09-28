//go:build dockere2e

package docker

import (
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestAnEntryNoStoreComparedIsNotCalledStale needs no stack. The entries that
// name Prometheus alone can only be used by a pair with Prometheus in it, and
// where the exporter could not be loaded there is no such pair: they were
// reported as drawn alike, with the advice to take them out. An entry is
// stale only when one of its stores was compared on the panel and nothing
// needed it.
func TestAnEntryNoStoreComparedIsNotCalledStale(t *testing.T) {
	reason := "Prometheus counts sponsorships per direction and drops the other party"
	entries := []dashboardDiffer{{title: "Sponsorships", stores: []string{"prometheus"}, reason: reason}}
	run := &dashboardRun{outcomes: map[string]map[int]dashboardOutcome{
		"prometheus": {7: {panel: grafana.PanelQuery{Index: 7, Title: "Sponsorships", Description: "So. " + reason + "."}}},
		"influxdb":   {7: {panel: grafana.PanelQuery{Index: 7, Title: "Sponsorships"}}},
	}}

	withoutPrometheus := map[string]map[string]bool{"Sponsorships": {"influxdb": true, "postgres": true}}
	problems, unasked := dashboardDifferProblems(entries, run, map[int]bool{}, withoutPrometheus)
	if len(problems) > 0 {
		t.Errorf("an entry whose one store was never compared is reported: %v", problems)
	}
	if len(unasked) != 1 {
		t.Errorf("an entry whose one store was never compared is not said to be unasked: %v", unasked)
	}

	withPrometheus := map[string]map[string]bool{"Sponsorships": {"influxdb": true, "prometheus": true}}
	problems, _ = dashboardDifferProblems(entries, run, map[int]bool{}, withPrometheus)
	if len(problems) != 1 || !strings.Contains(problems[0], "take the entry out") {
		t.Errorf("an entry nothing needed once its store was compared is not called stale: %v", problems)
	}

	problems, _ = dashboardDifferProblems(entries, run, map[int]bool{0: true}, withPrometheus)
	if len(problems) > 0 {
		t.Errorf("an entry that excused a difference is reported: %v", problems)
	}
}

// TestAnEntryOfOneKindIsNotHeldToAnotherPanelOfItsTitle needs no stack either.
// "Repositories" is the Overview's stat group and the Inventory table, and an
// entry about a tile of the first was held to the second's description, which
// has no reason to say anything about tiles, and called wrong; nor may it
// excuse a difference in the table.
func TestAnEntryOfOneKindIsNotHeldToAnotherPanelOfItsTitle(t *testing.T) {
	reason := "leaves its group when the range holds no document of it"
	entries := []dashboardDiffer{{
		title: "Repositories", kind: "stat", stores: []string{"elasticsearch"}, reason: reason,
		only: []string{"Stars"},
	}}
	run := &dashboardRun{outcomes: map[string]map[int]dashboardOutcome{"elasticsearch": {
		1:  {panel: grafana.PanelQuery{Index: 1, Title: "Repositories", Type: "stat", Description: "So " + reason + "."}},
		40: {panel: grafana.PanelQuery{Index: 40, Title: "Repositories", Type: "table", Description: "A table."}},
	}}}
	compared := map[string]map[string]bool{"Repositories": {"elasticsearch": true}}
	if problems, _ := dashboardDifferProblems(entries, run, map[int]bool{0: true}, compared); len(problems) > 0 {
		t.Errorf("an entry about a stat is held to a table of the same title: %v", problems)
	}
	table := func(stars float64) grafana.Picture {
		return grafana.Picture{Kind: "table", Columns: []*grafana.Field{
			{Name: "Stars", Display: "Stars", Type: "number", Values: []any{stars}},
		}}
	}
	a, b := table(1), table(2)
	if dashboardExcused(entries, "Repositories", "influxdb", "elasticsearch", &a, &b, grafana.Likeness{}, map[int]bool{}) {
		t.Error("an entry about a stat excuses a table of the same title")
	}
}

// TestAnEntryNamingAStoreThatDrawsAlikeIsTooWide needs no stack. An entry
// excuses a pair when it names either store, so a store named beside the one
// that differs is excused of whatever it draws, and no pair would ever show
// it. "Commits by repository" named Graphite, Elasticsearch and Prometheus;
// each has to be the one drawing the difference.
func TestAnEntryNamingAStoreThatDrawsAlikeIsTooWide(t *testing.T) {
	entries := []dashboardDiffer{{
		title: "Commits by repository", stores: []string{"graphite", "prometheus"},
		reason: "Named in full here", only: []string{"Repository"},
	}}
	key := dashboardPanelKey{"Commits by repository", "table"}
	alike := map[dashboardPanelKey]map[[2]string]bool{key: {
		{"influxdb", "graphite"}:   false,
		{"influxdb", "prometheus"}: true,
		{"graphite", "prometheus"}: false,
	}}
	problems := dashboardDifferTooWide(entries, alike)
	if len(problems) != 1 || !strings.Contains(problems[0], "take prometheus out") {
		t.Errorf("a store drawing the panel as the unnamed ones do is not called out: %v", problems)
	}
	alike[key][[2]string{"influxdb", "prometheus"}] = false
	if wide := dashboardDifferTooWide(entries, alike); len(wide) > 0 {
		t.Errorf("an entry both of whose stores differ is called too wide: %v", wide)
	}
	onlyNamed := map[dashboardPanelKey]map[[2]string]bool{key: {{"graphite", "prometheus"}: true}}
	if wide := dashboardDifferTooWide(entries, onlyNamed); len(wide) > 0 {
		t.Errorf("stores compared only with each other are held to a pair that cannot tell: %v", wide)
	}
}
