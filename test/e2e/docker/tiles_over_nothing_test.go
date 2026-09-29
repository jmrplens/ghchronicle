//go:build dockere2e

package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
		only:   []string{"Stars", "Forks", "Repositories"},
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
// dashboards about nothing, twice: about a repository that holds nothing, and
// over a range no sweep reached, and holds every store to drawing the tiles
// the others draw, with the same words.
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
//
// The range is the second question because the account's own snapshots do
// not follow the repository picker, so a repository with nothing in it
// leaves them full: over a range four hundred days back, "Community",
// "Account" and "Since the account began" read "No data" in the SQL stores
// and Elasticsearch, and Graphite, whose paths answer a range they hold
// nothing in with nulls, drew three panels with nothing in them, not even the
// names of their tiles.
//
// Five stores that agree on drawing nothing agree, so the comparison alone
// let the 2.6.2 answer to that stand: those groups, "Sponsorship" and the
// repository count read "No data" in all five, beside groups that said what
// the range lacked (the 2.6.3 review). So InfluxDB, which every other store is
// held to, is held to drawing over nothing every tile it draws over the
// dashboard's own range.
func TestEveryTileIsDrawnOverNothing(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	if run.promSkip != "" {
		t.Logf("Prometheus holds nothing on this machine, so its tiles are not asked: %s", run.promSkip)
	}
	c := &dashboardComparison{
		run: run, entries: tilesLeftOverNothing, drift: sweepDrift(run.sweeps),
		used: map[int]bool{}, compared: map[string]map[string]bool{},
	}
	questions := []struct {
		name    string
		answers map[string][]grafana.Result
	}{
		{"a repository with nothing in it", run.overNothing},
		{fmt.Sprintf("the range %s to %s, which no sweep reached", run.quiet[0].Format(time.DateOnly),
			run.quiet[1].Format(time.DateOnly)), run.overQuiet},
	}
	for _, q := range questions {
		pictures := tilesByPanel(t, q.answers)
		tiles, pairs := 0, c.pairs
		for _, index := range slices.Sorted(maps.Keys(pictures)) {
			if pic, ok := pictures[index]["influxdb"]; ok {
				tiles += len(pic.Tiles)
				if gone := tilesGone(t, run, index, &pic); len(gone) > 0 {
					t.Errorf("panel %d %q of influxdb draws %v over the dashboard's range and not over %s, "+
						"where a tile says what the range lacks", index, dashboardPanelTitle(run, index), gone, q.name)
				}
			}
			if failures := c.compare(index, pictures[index]); len(failures) > 0 {
				t.Errorf("panel %d %q draws %s differently in stores whose descriptions do not say why:\n  %s",
					index, dashboardPanelTitle(run, index), q.name, strings.Join(failures, "\n  "))
			}
		}
		t.Logf("over %s: %d tiles drawn by InfluxDB, %d pairs of stores compared", q.name, tiles, c.pairs-pairs)
		// A floor rather than a count, measured on the 2.6.2 branch at 52
		// tiles over the repository and 19 over the range, and on the 2.6.4
		// one at 52 over each, where 2.6.3 drew 31 over the range because the
		// tiles of the account's snapshots had no row to draw there, so that a
		// change that stops asking fails instead of passing with nothing asked.
		if tiles < 15 {
			t.Errorf("InfluxDB drew only %d tiles over %s, too few for this to have asked anything", tiles, q.name)
		}
	}
	stale, unasked := dashboardDifferProblems(tilesLeftOverNothing, run, c.used, c.compared)
	for _, problem := range append(stale, dashboardDifferTooWide(tilesLeftOverNothing, c.alike)...) {
		t.Error(problem)
	}
	for _, note := range unasked {
		t.Log(note)
	}
	t.Logf("%d pairs of stores compared, %d alike", c.pairs, c.agreed)
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

// askTilesOverQuiet asks one store's stats and gauges over quiet, a range
// quietRange found, with the repositories the sweep wrote, as the first
// question asks them over the dashboard's own range.
func askTilesOverQuiet(ctx context.Context, client grafana.Client, doc map[string]any,
	store dashboardStore, repos []string, quiet [2]time.Time,
) []grafana.Result {
	var tiles []grafana.PanelQuery
	for _, p := range grafana.Panels(doc["panels"]) {
		if (p.Type == "stat" || p.Type == "gauge") && (store.name != "prometheus" || !promNeedsHistory(&p)) {
			tiles = append(tiles, p)
		}
	}
	from, to := strconv.FormatInt(quiet[0].UnixMilli(), 10), strconv.FormatInt(quiet[1].UnixMilli(), 10)
	vars := dashboardVars(doc, store, repos)
	if vars.TimeFilter != "" {
		// Closed at both ends, as the InfluxDB plugin writes the macro out
		// for a range that does not end now.
		vars.TimeFilter = fmt.Sprintf("time >= '%s' AND time <= '%s'",
			quiet[0].Format(time.RFC3339), quiet[1].Format(time.RFC3339))
	}
	return client.CheckPanels(ctx, from, to, tiles, vars, grafana.Options{
		Timeout: 120 * time.Second, Workers: dashboardWorkers(store.name),
		IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
	})
}

// quietRange is thirty days no point of any sweep falls in, the newest such
// that ends at least four hundred days ago: older than Graphite's hourly
// archive, so that Graphite answers it out of the daily one as it answers a
// reader's range from before the collector ran, and younger than its daily
// one, whose twelve years the paths still answer with nulls.
func quietRange(sweeps [][]sqlStoresPoint) [2]time.Time {
	to := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -400)
	for {
		from := to.AddDate(0, 0, -30)
		quiet := true
		for _, points := range sweeps {
			for _, p := range points {
				if at, err := time.Parse(time.RFC3339Nano, p.Time); err == nil && !at.Before(from) && at.Before(to) {
					quiet = false
				}
			}
		}
		if quiet {
			return [2]time.Time{from, to}
		}
		to = from
	}
}

// tilesByPanel is what every store draws of each panel it was asked, by the
// panel's index and then by the store.
func tilesByPanel(t *testing.T, answers map[string][]grafana.Result) map[int]map[string]grafana.Picture {
	t.Helper()
	pictures := map[int]map[string]grafana.Picture{}
	for _, store := range dashboardStores {
		for index, pic := range tilesDrawn(t, store.name, answers[store.name]) {
			if pictures[index] == nil {
				pictures[index] = map[string]grafana.Picture{}
			}
			pictures[index][store.name] = pic
		}
	}
	return pictures
}

// tilesGone is every tile InfluxDB draws of a panel over the dashboard's own
// range and not in over, what it drew of the panel over nothing. A panel of
// one value draws it without a name, so for one only its absence counts.
func tilesGone(t *testing.T, run *dashboardRun, index int, over *grafana.Picture) []string {
	t.Helper()
	o, ok := run.outcomes["influxdb"][index]
	if !ok || o.err != "" {
		return nil // a query that failed is the first question's business
	}
	full, drawn, err := grafana.Drawing(o.panel.Source, o.answer)
	if err != nil || !drawn {
		return nil
	}
	if len(full.Tiles) == 1 && len(over.Tiles) > 0 {
		return nil
	}
	var gone []string
	for _, name := range full.Names() {
		if !slices.Contains(over.Names(), name) {
			gone = append(gone, name)
		}
	}
	return gone
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
