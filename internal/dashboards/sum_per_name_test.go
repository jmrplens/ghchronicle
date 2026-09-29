package dashboards

import (
	"fmt"
	"math"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestTheElasticsearchSnapshotsSayNotReadOverNothing replays two Elasticsearch
// panels over the answer the datasource gives a range no sweep reached, and
// over one with documents in it. The last review of 2.6.4 found both drawing
// "No data" there, where the groups beside them read "not read" in every
// store: the whole Repositories group, whose stars and forks add up the newest
// document of each repository and whose stat added all three values up, and
// the bar gauge of the contribution mix, which read the newest document in a
// bucket that has to hold one.
func TestTheElasticsearchSnapshotsSayNotReadOverNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		title string
		data  map[string][]map[string]any
		want  map[string]float64
	}{
		{
			title: "Repositories",
			data: map[string][]map[string]any{
				"A": {esSeriesFrame("A", "Top Metrics public_repos", 42)},
				// Two owners' repositories of one name, and one nobody starred.
				"B": {esTableFrame("B", []string{"alice/x", "acme/x", "alice/y"},
					map[string][]float64{"Top Metrics stars": {5, 0, 30}, "Top Metrics forks": {1, 0, 4}})},
				"C": {esSeriesFrame("C", "Stars")},
				"D": {esSeriesFrame("D", "Forks")},
			},
			want: map[string]float64{"Repositories": 42, "Stars": 35, "Forks": 5},
		},
		{
			title: "Repositories",
			data: map[string][]map[string]any{
				"A": {esSeriesFrame("A", "Top Metrics public_repos", 2)},
				"B": {esTableFrame("B", []string{"alice/x", "alice/y"},
					map[string][]float64{"Top Metrics stars": {0, 0}, "Top Metrics forks": {0, 0}})},
				"C": {esSeriesFrame("C", "Stars")},
				"D": {esSeriesFrame("D", "Forks")},
			},
			want: map[string]float64{"Repositories": 2, "Stars": 0, "Forks": 0},
		},
		{
			title: "Contribution mix (last year)",
			data: map[string][]map[string]any{"A": {
				esSeriesFrame("A", "Top Metrics commits", 80), esSeriesFrame("A", "Top Metrics pull_requests", 10),
				esSeriesFrame("A", "Top Metrics issues", 5), esSeriesFrame("A", "Top Metrics reviews", 5),
			}},
			want: map[string]float64{"Commits": 0.8, "Pull requests": 0.1, "Issues": 0.05, "Code review": 0.05},
		},
	} {
		p := tilesPanel(t, "elasticsearch", tc.title)
		said := drawnTiles(t, p, esAnswerOverNothing(t, p))
		for name := range tc.want {
			if said[name] != notRead {
				t.Errorf("%q over a range no sweep reached draws %s as %v, want %q; it draws %v",
					tc.title, name, said[name], notRead, said)
			}
		}
		got := drawnTiles(t, p, esAnswerOf(tc.data))
		for name, want := range tc.want {
			if v, isNumber := got[name].(float64); !isNumber || math.Abs(v-want) > 1e-9 {
				t.Errorf("%q draws %s as %v, want %v; it draws %v", tc.title, name, got[name], want, got)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q draws the tiles %v, want %d", tc.title, got, len(tc.want))
		}
	}
}

// drawnTiles is what a panel draws over an answer, by the tile's name: its
// number, or the text it shows for no value.
func drawnTiles(t *testing.T, p, answer map[string]any) map[string]any {
	t.Helper()
	pic, drawn, err := grafana.Drawing(p, answer)
	if err != nil || !drawn {
		t.Fatalf("%q cannot be replayed: %v", p["title"], err)
	}
	out := map[string]any{}
	for _, tile := range pic.Tiles {
		out[tile.Name] = tile.Value
		if text, mapped := tile.Field.Mapped(nil); tile.Value == nil && mapped {
			out[tile.Name] = text
		}
	}
	return out
}

// tilesPanel is the stat or the bar gauge of that title, which "Repositories"
// needs: the Inventory has a table of the name as well.
func tilesPanel(t *testing.T, store, title string) map[string]any {
	t.Helper()
	for _, p := range renderedPanels(t, store) {
		if p["title"] == title && drawsTiles(p) {
			return p
		}
	}
	t.Fatalf("no stat or bar gauge called %q in %s", title, store)
	return nil
}

// esAnswerOverNothing is what the Elasticsearch datasource answers a panel's
// queries over a range with no document in them, as measured against Grafana
// 13.2.1: a query whose buckets are all date histograms answers a series with
// no value per metric it shows, named by its alias or, for the newest
// document, per field; any other answers no frame at all.
func esAnswerOverNothing(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	frames := map[string][]map[string]any{}
	for _, target := range panelTargets(p) {
		ref, _ := target["refId"].(string)
		histogram := true
		for _, raw := range asList(target["bucketAggs"]) {
			bucket, _ := raw.(map[string]any)
			histogram = histogram && bucket["type"] == "date_histogram"
		}
		if !histogram {
			continue
		}
		for _, raw := range asList(target["metrics"]) {
			metric, _ := raw.(map[string]any)
			if metric["hide"] == true {
				continue
			}
			if alias, _ := target["alias"].(string); alias != "" {
				frames[ref] = append(frames[ref], esSeriesFrame(ref, alias))
				continue
			}
			if metric["type"] != "top_metrics" {
				t.Fatalf("%q: a %v over nothing, which this does not name", p["title"], metric["type"])
			}
			settings, _ := metric["settings"].(map[string]any)
			for _, field := range asList(settings["metrics"]) {
				frames[ref] = append(frames[ref], esSeriesFrame(ref, fmt.Sprint("Top Metrics ", field)))
			}
		}
	}
	return esAnswerOf(frames)
}

// esSeriesFrame is one series of a date histogram as the datasource answers
// it: the frame named after the series, a time and a value, and one point per
// value, stamped at the century bucket's start.
func esSeriesFrame(ref, name string, values ...float64) map[string]any {
	times, points := []any{}, []any{}
	for _, v := range values {
		times, points = append(times, 0.0), append(points, v)
	}
	return map[string]any{
		"schema": map[string]any{"name": name, "refId": ref, "fields": []any{
			map[string]any{"name": "Time", "type": "time"},
			map[string]any{"name": "Value", "type": "number"},
		}},
		"data": map[string]any{"values": []any{times, points}},
	}
}

// esTableFrame is a terms bucket on the full name with metrics under it, as
// the datasource answers it: a column per bucket field and one per metric.
func esTableFrame(ref string, names []string, columns map[string][]float64) map[string]any {
	fields := []any{map[string]any{"name": panelFullNameField, "type": "string"}}
	keys := []any{}
	for _, n := range names {
		keys = append(keys, n)
	}
	values := []any{keys}
	for _, name := range []string{"Top Metrics stars", "Top Metrics forks"} {
		column := []any{}
		for _, v := range columns[name] {
			column = append(column, v)
		}
		fields = append(fields, map[string]any{"name": name, "type": "number"})
		values = append(values, column)
	}
	return map[string]any{
		"schema": map[string]any{"refId": ref, "fields": fields},
		"data":   map[string]any{"values": values},
	}
}

func esAnswerOf(frames map[string][]map[string]any) map[string]any {
	results := map[string]any{}
	for ref, list := range frames {
		out := make([]any, len(list))
		for i := range list {
			out[i] = list[i]
		}
		results[ref] = map[string]any{"frames": out}
	}
	return map[string]any{"results": results}
}
