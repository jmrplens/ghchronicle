//go:build dockere2e

package docker

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// The two punch cards, read back through Grafana from the four stores that
// draw them, against what the sweep's own points add up to.
//
// A punch card is a grid, a point per weekday and hour, and one read stamps
// every cell of it with the same instant. So a panel that picks one row per
// repository and hour, or per repository and weekday, keeps one of the cells
// that tie on that instant and draws a number that looks like a count and is
// not. That is what both SQL stores and Elasticsearch drew until the second
// review of 2.6.1: with Monday's two cells in the fixture at 3 and 5, Monday
// read 3 in InfluxDB and PostgreSQL and 5 in Elasticsearch, and only Graphite,
// which adds every cell up, read 8. The fixture holds a weekday with two hours
// and an hour with two weekdays, so each panel has a bar that only a sum
// draws right.
//
// TestTheStoresDrawTheSameValues cannot see this: it holds the stores to each
// other, and three of the four drew the same wrong number. So this reads every
// bar against the sweep's own points, after the one step of the panel's own
// transformations that decides the number in Elasticsearch, the sum over the
// rows a bar is made of.
func TestThePunchCardsAddUpEveryCellInEveryStore(t *testing.T) {
	s := Start(t)
	dashboardsRun(t, s)
	points := sqlStoresPoints(t, sqlStoresRun(t.Context(), t, s))

	charts := []struct{ title, tag, column string }{
		{"Commits by hour of day", "hour", "Hour"},
		{"Commits by weekday", "weekday", "Weekday"},
	}
	for _, store := range dashboardStores {
		if store.name == "prometheus" {
			continue // the exporter skips the punch card, and the panel says so
		}
		doc, err := dashboardDocument(store.name)
		if err != nil {
			t.Fatalf("reading the %s dashboard: %v", store.name, err)
		}
		var asked []grafana.PanelQuery
		for _, p := range grafana.Panels(doc["panels"]) {
			for _, c := range charts {
				if p.Title == c.title {
					asked = append(asked, p)
				}
			}
		}
		if len(asked) != len(charts) {
			t.Fatalf("%s: found %d of the %d punch card panels", store.name, len(asked), len(charts))
		}
		client := grafana.Client{URL: s.GrafanaURL, Token: s.GrafanaToken}
		results := client.CheckPanels(t.Context(), dashboardRange, "now", asked,
			dashboardVars(doc, store, dashboardRepos(points)), grafana.Options{
				Timeout: 120 * time.Second, Workers: 1,
				IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
			})
		for i := range results {
			r := &results[i]
			c := charts[slices.IndexFunc(charts, func(c struct{ title, tag, column string }) bool {
				return c.title == r.Panel.Title
			})]
			if r.Err != "" {
				t.Errorf("%s: %q failed: %s", store.name, r.Panel.Title, grafana.Trim(r.Err, 400))
				continue
			}
			want := punchCardTotals(points, c.tag)
			got := punchCardBars(r.Answer, &r.Panel, c.column)
			if !punchCardSame(got, want) {
				t.Errorf("%s: %q draws %v, want every cell of each repository's newest grid "+
					"added up, %v", store.name, r.Panel.Title, got, want)
			}
		}
	}
}

// punchCardTotals is what the panel should draw: the cells of each
// repository's newest read, added up by one of the two tags.
func punchCardTotals(points []sqlStoresPoint, tag string) map[string]float64 {
	newest := map[string]time.Time{}
	var cells []sqlStoresPoint
	for _, p := range points {
		if p.Measurement != "gh_commit_punchcard" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, p.Time)
		if err != nil {
			continue
		}
		if at.After(newest[p.Tags["full_name"]]) {
			newest[p.Tags["full_name"]] = at
		}
		cells = append(cells, p)
	}
	out := map[string]float64{}
	for _, p := range cells {
		at, _ := time.Parse(time.RFC3339Nano, p.Time)
		if !at.Equal(newest[p.Tags["full_name"]]) {
			continue
		}
		n, _ := p.Fields["commits"].(float64)
		out[p.Tags[tag]] += n
	}
	return out
}

// punchCardBars reads a bar per value of the charted tag out of one answer,
// in the two shapes the stores give it. A time series per bar is Graphite's,
// and the bar is the series' newest value, which is what the panel's
// lastNotNull reduces it to. A table is everyone else's, and a bar there is
// the sum of the rows that carry its value: one row per bar in the SQL
// stores, and in Elasticsearch one per repository and bar, which the panel's
// groupBy adds up.
func punchCardBars(res map[string]any, p *grafana.PanelQuery, column string) map[string]float64 {
	out := map[string]float64{}
	results, _ := res["results"].(map[string]any)
	for _, ref := range slices.Sorted(maps.Keys(results)) {
		answer, _ := results[ref].(map[string]any)
		frames, _ := answer["frames"].([]any)
		for _, raw := range frames {
			frame, _ := raw.(map[string]any)
			punchCardFrame(frame, p, column, out)
		}
	}
	return out
}

func punchCardFrame(frame map[string]any, p *grafana.PanelQuery, column string, out map[string]float64) {
	schema, _ := frame["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	data, _ := frame["data"].(map[string]any)
	values, _ := data["values"].([]any)
	var labels []any
	var counts []any
	series := ""
	for i, raw := range fields {
		field, _ := raw.(map[string]any)
		if i >= len(values) || field["type"] == "time" {
			continue
		}
		name, _ := field["name"].(string)
		if renamed, ok := p.Renames[name]; ok {
			name = renamed
		}
		cells, _ := values[i].([]any)
		switch {
		case name == column:
			labels = cells
		case field["type"] == "number":
			counts = cells
			config, _ := field["config"].(map[string]any)
			series, _ = config["displayNameFromDS"].(string)
			if series == "" {
				series = name
			}
		}
	}
	if labels == nil {
		// One series per bar, named by the value it stands for.
		for _, v := range slices.Backward(counts) {
			if n, ok := v.(float64); ok {
				out[series] += n
				return
			}
		}
		return
	}
	for i, label := range labels {
		if i < len(counts) {
			if n, ok := counts[i].(float64); ok {
				out[fmt.Sprint(label)] += n
			}
		}
	}
}

func punchCardSame(got, want map[string]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || math.Abs(g-w) > 1e-9 {
			return false
		}
	}
	return true
}
