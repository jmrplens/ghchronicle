package dashboards

import (
	"math"
	"strings"
	"testing"
)

// contributionMixTitle is the bar gauge of the four kinds of contribution.
const contributionMixTitle = "Contribution mix (last year)"

// TestTheContributionMixReadsTheNewestSnapshot holds every store's bars to
// the newest contributions snapshot of the range, the one the Contributions
// tile of Account reads, when that snapshot holds none of the four kinds and
// an older one in the range holds some: a year with nothing in it is no mix,
// and each bar says "not read" beside a Contributions tile of 0.
//
// The review of the 2.6.4 details wrote such a snapshot after the suite's own
// and found InfluxDB and Graphite drawing the older snapshot's mix, where
// Elasticsearch drew "not read". The SQL stores left out every row whose sum
// was 0 before taking the newest, and Graphite, whose share of a sum of 0 is
// null, reduced each bar to its last value that was not null, which is the
// older snapshot's share carried forward to that point. The statement is
// checked for its shape here, since only a store runs it, and the containerised
// suite runs it in both (TestTheContributionMixReadsTheNewestSnapshot there);
// the other three stores' bars are replayed over what their datasource
// answers.
func TestTheContributionMixReadsTheNewestSnapshot(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		sql := sqlOf(t, mustPanel(t, rendered(t, store), contributionMixTitle))
		if !strings.Contains(sql, "FROM gh_contributions_total"+overviewNewestRow+")") {
			t.Errorf("%s reads a row of the range other than its newest: %s", store, sql)
		}
		for _, part := range mixParts {
			if !strings.Contains(sql, "100.0 * "+part.From+" / NULLIF("+mixTotal+", 0)") {
				t.Errorf("%s divides %s by a sum it does not keep from being 0: %s", store, part.From, sql)
			}
		}
	}

	// graphite-web answers asPercent over a sum of 0 with null (its safeDiv),
	// and keepLastValue carries the counts of 0 to the end of the range, so
	// every point from that snapshot on is null.
	older := map[string]float64{"Commits": 80, "Pull requests": 10, "Issues": 6, "Code review": 4}
	nothingAfter, newest := map[string][]map[string]any{}, map[string][]map[string]any{}
	for i, part := range mixParts {
		share := older[part.To]
		nothingAfter[ref(i)] = []map[string]any{graphiteFrame(ref(i), part.To, nil, share, share, nil, nil)}
		newest[ref(i)] = []map[string]any{graphiteFrame(ref(i), part.To, nil, share, share)}
	}
	gr := tilesPanel(t, "graphite", contributionMixTitle)
	wantMix(t, "graphite, the newest snapshot holding none of the four", drawnTiles(t, gr, esAnswerOf(nothingAfter)), nil)
	wantMix(t, "graphite, the newest snapshot holding some", drawnTiles(t, gr, esAnswerOf(newest)), older)

	// Elasticsearch reads the newest document, and its panel divides each
	// count by their sum, 0 by 0.
	zeros := map[string][]map[string]any{"A": {
		esSeriesFrame("A", "Top Metrics commits", 0), esSeriesFrame("A", "Top Metrics pull_requests", 0),
		esSeriesFrame("A", "Top Metrics issues", 0), esSeriesFrame("A", "Top Metrics reviews", 0),
	}}
	es := tilesPanel(t, "elasticsearch", contributionMixTitle)
	wantMix(t, "elasticsearch, the newest document holding none of the four", drawnTiles(t, es, esAnswerOf(zeros)), nil)

	// Prometheus divides the newest value of each gauge, 0 by 0, and answers
	// the instant with NaN, as Grafana 13.2.1 hands it over.
	nan := map[string][]map[string]any{}
	for i, part := range mixParts {
		nan[ref(i)] = []map[string]any{promNaNFrame(ref(i), part.To)}
	}
	prom := tilesPanel(t, "prometheus", contributionMixTitle)
	wantMix(t, "prometheus, the newest gauges holding none of the four", drawnTiles(t, prom, esAnswerOf(nan)), nil)
}

// wantMix holds the bars drawn to want's percentages, or, for a nil want, to
// "not read" on every one of the four.
func wantMix(t *testing.T, over string, drawn map[string]any, want map[string]float64) {
	t.Helper()
	if len(drawn) != len(mixParts) {
		t.Errorf("%s: the mix draws %v, want its four bars", over, drawn)
	}
	for _, part := range mixParts {
		got := drawn[part.To]
		if want == nil {
			if got != notRead {
				t.Errorf("%s: %s reads %v, want %q; the bars read %v", over, part.To, got, notRead, drawn)
			}
			continue
		}
		if v, isNumber := got.(float64); !isNumber || math.Abs(v-want[part.To]) > 1e-9 {
			t.Errorf("%s: %s reads %v, want %v; the bars read %v", over, part.To, got, want[part.To], drawn)
		}
	}
}

// graphiteFrame is one series as the Graphite datasource answers it through
// Grafana 13.2.1: a time field and a value field named by the alias, a point
// an hour.
func graphiteFrame(ref, name string, values ...any) map[string]any {
	times := make([]any, len(values))
	for i := range values {
		times[i] = float64(i) * 3600e3
	}
	return map[string]any{
		"schema": map[string]any{"refId": ref, "fields": []any{
			map[string]any{"name": "time", "type": "time"},
			map[string]any{
				"name": "value", "type": "number", "labels": map[string]any{"name": name},
				"config": map[string]any{"displayNameFromDS": name},
			},
		}},
		"data": map[string]any{"values": []any{times, values}},
	}
}

// promNaNFrame is an instant query answering NaN as the Prometheus datasource
// hands it over through Grafana 13.2.1: a null value the entities say is NaN.
func promNaNFrame(ref, name string) map[string]any {
	return map[string]any{
		"schema": map[string]any{"refId": ref, "fields": []any{
			map[string]any{"name": "Time", "type": "time"},
			map[string]any{
				"name": "Value", "type": "number", "labels": map[string]any{},
				"config": map[string]any{"displayNameFromDS": name},
			},
		}},
		"data": map[string]any{
			"values":   []any{[]any{0.0}, []any{nil}},
			"entities": []any{nil, map[string]any{"NaN": []any{0.0}}},
		},
	}
}
