package dashboards

import (
	"math"
	"testing"
)

// TestElasticsearchWeighsTheWastedShareByMinutes is "Minutes spent on failed
// runs" in Elasticsearch, evaluated over the runs of two repositories. The
// 2.6.2 review found it drawing the success rate of the runs where the SQL
// stores draw the minutes the failed ones took and their share of all the
// minutes, under a description calling one the complement of the other. It is
// not: two runs of 100 and 50 seconds, one of each outcome, are half the runs
// and a third of the minutes. The table has to draw the SQL stores' columns
// with their values: a canceled run is time wasted as a failed one is, and a
// repository whose runs all succeeded wasted none, which it read as NaN once
// the merge had left its failed runs undefined.
func TestElasticsearchWeighsTheWastedShareByMinutes(t *testing.T) {
	t.Parallel()
	var docs []esDoc
	run := func(repo, conclusion string, seconds int) {
		docs = append(docs, esDoc{
			"full_name": "alice/" + repo, "repo": repo, "conclusion": conclusion,
			"success": conclusion == "success", "duration_seconds": seconds,
		})
	}
	run("site", "success", 100)
	run("site", "failure", 50)
	run("site", cancelledRun, 30)
	run("docs", "success", 20)
	rows := evalESPanel(t, mustPanel(t, rendered(t, "elasticsearch"), "Minutes spent on failed runs"), docs)
	want := map[string]map[string]float64{
		"site": {"Wasted": 80, "Total": 180, "Share": 80.0 / 180},
		"docs": {"Wasted": 0, "Total": 20, "Share": 0},
	}
	if len(rows) != len(want) {
		t.Fatalf("Elasticsearch draws %d rows for %d repositories: %v", len(rows), len(want), rows)
	}
	for _, row := range rows {
		repo, _ := row["Repository"].(string)
		columns, known := want[repo]
		if !known {
			t.Errorf("a row for %q, which ran nothing: %v", repo, row)
			continue
		}
		for column, value := range columns {
			// NaN is compared as what it is: no number is within any
			// distance of it, so a distance alone would pass it.
			got, isNumber := row[column].(float64)
			if !isNumber || math.IsNaN(got) || math.Abs(got-value) > 1e-9 {
				t.Errorf("%s: %s reads %v, want %v as the SQL stores select it: %v",
					repo, column, row[column], value, row)
			}
		}
	}
}
