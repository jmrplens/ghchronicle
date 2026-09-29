package dashboards

import (
	"fmt"
	"math"
	"slices"
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

// TestTheElasticsearchTotalsDrawEveryTileOverNothing replays the three
// Elasticsearch groups that add up the newest document of each repository,
// release or alert over what the datasource answers a range no sweep reached,
// and over documents. Until the last round of 2.6.4 they drew no tile for
// such a value where the other stores drew one without a value: the artifact
// storage and the cache of "Runs in range", with the success rate, the run
// duration and the queue wait beside them, since that stat added every value
// up and would have read a value of nothing as 0; the total of "Downloads";
// and both counts of "Open alerts".
func TestTheElasticsearchTotalsDrawEveryTileOverNothing(t *testing.T) {
	t.Parallel()
	runs := []string{ciRunCount, ciSuccessRate, ciUndecidedRuns, ciRunTime, ciQueueWait, ciArtifactStorage, ciCacheSize}
	alerts := []string{"Dependabot", securityCodeScanning}
	for _, tc := range []struct {
		title, about string
		read         map[string][]any
		want         map[string]any
		order        []string
	}{
		{
			title: "Runs in range", about: "a range no sweep reached", order: runs,
			want: map[string]any{
				ciRunCount: 0.0, ciSuccessRate: "none decided", ciUndecidedRuns: 0.0, ciRunTime: "no runs",
				ciQueueWait: "no jobs", ciArtifactStorage: notRead, ciCacheSize: notRead,
			},
		},
		{
			// Two repositories, whose newest bytes are added up.
			title: "Runs in range", about: "documents", order: runs,
			read: map[string][]any{
				ciRunCount: {12.0}, ciSuccessRate: {0.75}, ciUndecidedRuns: {3.0}, ciRunTime: {260.0},
				ciQueueWait: {40.0}, ciArtifactStorage: {100.0, 200.0}, ciCacheSize: {10.0, 20.0},
			},
			want: map[string]any{
				ciRunCount: 12.0, ciSuccessRate: 0.75, ciUndecidedRuns: 3.0, ciRunTime: 260.0,
				ciQueueWait: 40.0, ciArtifactStorage: 300.0, ciCacheSize: 30.0,
			},
		},
		{
			// A repository whose runs were all canceled, and that stores
			// neither an artifact nor a cache: a share of no decided run is
			// no number, and a total nothing was read for is none either.
			title: "Runs in range", about: "runs and nothing else", order: runs,
			read: map[string][]any{
				ciRunCount: {2.0}, ciSuccessRate: {nil}, ciUndecidedRuns: {2.0}, ciRunTime: {30.0}, ciQueueWait: {5.0},
			},
			want: map[string]any{
				ciRunCount: 2.0, ciSuccessRate: "none decided", ciUndecidedRuns: 2.0, ciRunTime: 30.0,
				ciQueueWait: 5.0, ciArtifactStorage: notRead, ciCacheSize: notRead,
			},
		},
		{
			title: "Downloads", about: "a range no sweep reached", order: []string{"Total", "Releases"},
			want: map[string]any{"Total": "no releases", "Releases": 0.0},
		},
		{
			// Two releases, one of each owner's repository of one name.
			title: "Downloads", about: "documents", order: []string{"Total", "Releases"},
			read: map[string][]any{"Total": {40.0, 60.0}, "Releases": {2.0}},
			want: map[string]any{"Total": 100.0, "Releases": 2.0},
		},
		{
			title: securityOpenAlerts, about: "a range no sweep reached", order: alerts,
			want: map[string]any{"Dependabot": "none open", securityCodeScanning: "none open"},
		},
		{
			title: securityOpenAlerts, about: "documents", order: alerts,
			read: map[string][]any{"Dependabot": {2.0, 5.0}, securityCodeScanning: {1.0, 3.0}},
			want: map[string]any{"Dependabot": 7.0, securityCodeScanning: 4.0},
		},
		{
			// The tile of the value read first keeps its place.
			title: securityOpenAlerts, about: "code scanning alone", order: alerts,
			read: map[string][]any{securityCodeScanning: {1.0, 3.0}},
			want: map[string]any{"Dependabot": "none open", securityCodeScanning: 4.0},
		},
	} {
		p := tilesPanel(t, "elasticsearch", tc.title)
		answer := esAnswerOverNothing(t, p)
		if tc.read != nil {
			answer = esAnswerByAlias(p, tc.read)
		}
		pic, drawn, err := grafana.Drawing(p, answer)
		if err != nil || !drawn {
			t.Fatalf("%q cannot be replayed: %v", tc.title, err)
		}
		got := drawnTiles(t, p, answer)
		for name, want := range tc.want {
			v, isNumber := got[name].(float64)
			if w, wantNumber := want.(float64); wantNumber && (!isNumber || math.Abs(v-w) > 1e-9) ||
				!wantNumber && got[name] != want {
				t.Errorf("%q over %s draws %s as %v, want %v; it draws %v", tc.title, tc.about, name, got[name], want, got)
			}
		}
		if names := pic.Names(); !slices.Equal(names, tc.order) {
			t.Errorf("%q over %s draws the tiles %v, want %v", tc.title, tc.about, names, tc.order)
		}
	}
}

// esAnswerByAlias is what the Elasticsearch datasource answers a panel whose
// every value is a series named by its query's alias: a series of one point
// per value `read` holds under that name, one per repository, release or alert
// where the query buckets by them, and none for a name it holds nothing
// under, and an empty series for a query that answers its name alone. A
// query with no alias answers nothing.
func esAnswerByAlias(p map[string]any, read map[string][]any) map[string]any {
	frames := map[string][]map[string]any{}
	for _, target := range panelTargets(p) {
		ref, _ := target["refId"].(string)
		alias, _ := target["alias"].(string)
		switch {
		case alias == "":
		case answersNoValue(target):
			frames[ref] = append(frames[ref], esSeriesFrameOf(ref, alias))
		default:
			for _, v := range read[alias] {
				frames[ref] = append(frames[ref], esSeriesFrameOf(ref, alias, v))
			}
		}
	}
	return esAnswerOf(frames)
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
// 13.2.1: a query whose buckets are all date histograms answers a series per
// metric it shows, named by its alias or, for the newest document, per field;
// any other answers no frame at all. The series holds no point where the
// bucket has to hold a document and where a bucket script answers null, and
// otherwise the one bucket a century wide, which is there whether or not a
// document falls in it: a count of it is 0, and anything else of it null.
func esAnswerOverNothing(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	frames := map[string][]map[string]any{}
	for _, target := range panelTargets(p) {
		ref, _ := target["refId"].(string)
		histogram, empty := true, true
		for _, raw := range asList(target["bucketAggs"]) {
			bucket, _ := raw.(map[string]any)
			settings, _ := bucket["settings"].(map[string]any)
			histogram = histogram && bucket["type"] == "date_histogram"
			empty = empty && settings["min_doc_count"] == "0"
		}
		if !histogram {
			continue
		}
		for _, raw := range asList(target["metrics"]) {
			metric, _ := raw.(map[string]any)
			if metric["hide"] == true {
				continue
			}
			point := esPointOverNothing(metric, empty)
			if alias, _ := target["alias"].(string); alias != "" {
				frames[ref] = append(frames[ref], esSeriesFrameOf(ref, alias, point...))
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

// esPointOverNothing is what a metric of the century-wide bucket reads over a
// range with no document in it: no point where the bucket has to hold a
// document or a bucket script answers null, 0 for a count, and null for
// anything else.
func esPointOverNothing(metric map[string]any, empty bool) []any {
	switch {
	case !empty || metric["type"] == "bucket_script":
		return nil
	case metric["type"] == "count" || metric["type"] == "cardinality":
		return []any{0.0}
	}
	return []any{nil}
}

// esSeriesFrame is one series of a date histogram as the datasource answers
// it: the frame named after the series, a time and a value, and one point per
// value, stamped at the century bucket's start.
func esSeriesFrame(ref, name string, values ...float64) map[string]any {
	points := make([]any, len(values))
	for i, v := range values {
		points[i] = v
	}
	return esSeriesFrameOf(ref, name, points...)
}

// esSeriesFrameOf is esSeriesFrame with a point that can be null, as the
// median or the mean of a bucket with no document in it is.
func esSeriesFrameOf(ref, name string, values ...any) map[string]any {
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
