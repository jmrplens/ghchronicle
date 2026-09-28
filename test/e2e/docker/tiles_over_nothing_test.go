//go:build dockere2e

package docker

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// ── 4. A tile over nothing ──────────────────────────────────────────────────

// nothingRepository is a repository no sweep wrote anything for, which the
// picker hands every panel in place of the ones it lists: what a reader sees
// who narrows the page to a repository without releases, alerts or runs.
const nothingRepository = "nothing-was-written-here"

// tilesLeftOverNothing is every tile that is left out of its group, over a
// repository with nothing in it, in a store that cannot answer it with the
// nothing the SQL stores draw. Each reason is words of that store's own
// description of the panel, as in dashboardsDiffer.
var tilesLeftOverNothing = []dashboardDiffer{
	{
		title: "Repositories", kind: "stat", stores: []string{"elasticsearch"},
		reason: "leaves its group when the range holds no document of it",
		only:   []string{"Stars", "Forks"},
	},
	{
		title: "Runs in range", stores: []string{"elasticsearch"},
		reason: "leaves its group when the range holds no document of it",
		only: []string{
			"Success rate", "Run duration", "Queue wait", "Artifact storage walked", "Actions cache",
		},
	},
	{
		title: "Downloads", stores: []string{"elasticsearch"},
		reason: "leaves its group when the range holds no document of it", only: []string{"Total"},
	},
	{
		title: "Open alerts", stores: []string{"elasticsearch"},
		reason: "leaves its group when the range holds no document of it",
		only:   []string{"Dependabot", "Code scanning"},
	},
}

// TestEveryTileIsDrawnOverNothing asks every stat and gauge of the five
// dashboards about a repository that holds nothing, and holds every store to
// drawing the tiles the others draw, with the same words.
//
// Over no rows a SQL count is 0 and anything else is null, which a tile draws
// as the words its panel gives a value that is not there: "no issue closed",
// "none decided". The 2.6.2 review found Graphite and Elasticsearch drawing
// neither for most of them: a Graphite path nothing was written to answers no
// series at all, and an Elasticsearch bucket with no document in it is not
// returned, and a tile with no field is not drawn, so the group closed up
// around the gap. Over the suite's own range one such tile showed, "Time to
// close an issue" in Elasticsearch; over a repository that holds nothing,
// eighteen were missing from Graphite and twenty from Elasticsearch.
func TestEveryTileIsDrawnOverNothing(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	if run.promSkip != "" {
		t.Logf("Prometheus holds nothing on this machine, so its tiles are not asked: %s", run.promSkip)
	}
	pictures := map[int]map[string]grafana.Picture{}
	for _, store := range dashboardStores {
		for index, pic := range tilesDrawn(t, store.name, run.overNothing[store.name]) {
			if pictures[index] == nil {
				pictures[index] = map[string]grafana.Picture{}
			}
			pictures[index][store.name] = pic
		}
	}
	c := &dashboardComparison{
		run: run, entries: tilesLeftOverNothing, drift: sweepDrift(run.sweeps),
		used: map[int]bool{}, compared: map[string]map[string]bool{},
	}
	tiles := 0
	for _, index := range slices.Sorted(maps.Keys(pictures)) {
		if pic, ok := pictures[index]["influxdb"]; ok {
			tiles += len(pic.Tiles)
		}
		if failures := c.compare(index, pictures[index]); len(failures) > 0 {
			t.Errorf("panel %d %q draws a repository with nothing in it differently in stores whose "+
				"descriptions do not say why:\n  %s",
				index, dashboardPanelTitle(run, index), strings.Join(failures, "\n  "))
		}
	}
	stale, unasked := dashboardDifferProblems(tilesLeftOverNothing, run, c.used, c.compared)
	for _, problem := range stale {
		t.Error(problem)
	}
	for _, note := range unasked {
		t.Log(note)
	}
	t.Logf("%d tiles drawn by InfluxDB, %d pairs of stores compared, %d alike", tiles, c.pairs, c.agreed)
	// A floor rather than a count, measured at 52 tiles on the 2.6.2 branch,
	// so that a change that stops asking fails instead of passing with
	// nothing asked.
	if tiles < 40 {
		t.Errorf("InfluxDB drew only %d tiles over a repository with nothing in it, too few for "+
			"this to have asked anything", tiles)
	}
}

// askTilesOverNothing asks one store's stats and gauges about
// nothingRepository. dashboardsRunInto calls it, with the other questions.
func askTilesOverNothing(ctx context.Context, client grafana.Client, doc map[string]any,
	store dashboardStore,
) []grafana.Result {
	var tiles []grafana.PanelQuery
	for _, p := range grafana.Panels(doc["panels"]) {
		if (p.Type == "stat" || p.Type == "gauge") && (store.name != "prometheus" || !promNeedsHistory(&p)) {
			tiles = append(tiles, p)
		}
	}
	// Every form of the repository variable names the one repository, All's
	// own value included, which is what Graphite and Prometheus would
	// otherwise substitute.
	vars := dashboardVars(doc, store, nil)
	vars.AllValue, vars.Repos, vars.Text = "", []string{nothingRepository}, nothingRepository
	return client.CheckPanels(ctx, dashboardRange, "now", tiles, vars, grafana.Options{
		Timeout: 120 * time.Second, Workers: dashboardWorkers(store.name),
		IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
	})
}

// tilesDrawn is what each answer askTilesOverNothing had puts on the screen,
// by the panel's index.
func tilesDrawn(t *testing.T, store string, results []grafana.Result) map[int]grafana.Picture {
	t.Helper()
	out := map[int]grafana.Picture{}
	for _, r := range results {
		if r.Err != "" {
			t.Errorf("panel %d %q of %s fails over a repository with nothing in it: %s",
				r.Panel.Index, r.Panel.Title, store, r.Err)
			continue
		}
		pic, drawn, err := grafana.Drawing(r.Panel.Source, r.Answer)
		if err != nil {
			t.Errorf("panel %d %q of %s cannot be replayed: %v", r.Panel.Index, r.Panel.Title, store, err)
			continue
		}
		if drawn {
			out[r.Panel.Index] = pic
		}
	}
	return out
}
