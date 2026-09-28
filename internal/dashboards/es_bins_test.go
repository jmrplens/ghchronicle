package dashboards

import "testing"

// TestEveryElasticsearchChartBinsByTheRange holds every Elasticsearch date
// histogram of a chart that has an interval floor to the interval "auto",
// which the datasource sends as the query's own interval: the range over the
// panel's maxDataPoints, never under the floor, the bucket the SQL stores bin
// by through $__dateBin and Graphite through its bucket variables. Written as
// the floor itself, "1h", it was a bucket of an hour at every range: the 2.6.2
// review measured "Events over time" at 721 hourly buckets over thirty days in
// Elasticsearch against 120 of six hours in InfluxDB.
//
// A histogram of a chart without a floor keeps its interval, and so does one
// whose interval is not the floor: the contribution calendar's day and the
// weekly commits' week are the rows themselves, and a stat's century-wide
// bucket is the whole range.
func TestEveryElasticsearchChartBinsByTheRange(t *testing.T) {
	t.Parallel()
	followed := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		floor, _ := p["interval"].(string)
		for _, target := range panelTargets(p) {
			buckets, _ := target["bucketAggs"].([]any)
			for _, raw := range buckets {
				bucket, _ := raw.(map[string]any)
				settings, _ := bucket["settings"].(map[string]any)
				if bucket["type"] != "date_histogram" {
					continue
				}
				interval, _ := settings["interval"].(string)
				switch {
				case floor != "" && interval == "auto":
					followed++
				case floor != "":
					t.Errorf("%q bins every range into buckets of %s where its floor is %s: the other "+
						"stores widen the bucket with the range", p["title"], interval, floor)
				case interval == "auto":
					t.Errorf("%q bins by the range with no floor to keep its buckets from going under "+
						"the rows' own resolution", p["title"])
				}
			}
		}
	}
	if followed < 30 {
		t.Fatalf("only %d Elasticsearch histograms follow the range, too few for this to have held anything", followed)
	}
}
