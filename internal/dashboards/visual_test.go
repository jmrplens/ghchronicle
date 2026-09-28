package dashboards

import (
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
