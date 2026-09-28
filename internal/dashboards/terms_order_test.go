package dashboards

import (
	"slices"
	"testing"
)

// TestEveryTermsBucketOrdersBySomethingItsQueryHas is what the audit of 2.6.1
// found in "Events by type", "Events by repository", "Notifications" and
// "Topics" on Elasticsearch. Each kept its top values by a metric, naming it
// in orderBy as "1", and no metric of any of the four queries was numbered 1:
// the builder hands out one sequence per walk and renumberES renumbers every
// metric and bucket after that, while nothing rewrote the reference. Grafana
// orders by a metric id only when a metric of the query carries it, and
// otherwise leaves the bucket in Elasticsearch's own order, by document count,
// so each table kept the values with the most documents rather than the
// largest sum or the most repositories, and nothing said so.
//
// Measured in the containerised suite's Grafana 13.2.1 and Elasticsearch
// 9.5.3, over one type with three documents of one event each and another
// with one document of ten: "Events by type" cut to one value kept the three
// events, and with the reference renumbered it keeps the ten.
//
// Every terms bucket of the generated dashboard orders by the document count,
// by its key, or by a metric of its own query. `_term` is not among them: it
// is the key's name from before Elasticsearch 6.0, and Elasticsearch 9.5.3
// answers it with "Cannot find aggregation named [_term]". It worked in three
// panels only because Grafana 13.2.1 rewrites it to `_key` before sending.
func TestEveryTermsBucketOrdersBySomethingItsQueryHas(t *testing.T) {
	t.Parallel()
	byMetric := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			var ids []string
			for _, m := range asList(target["metrics"]) {
				id, _ := agg(m)["id"].(string)
				ids = append(ids, id)
			}
			for _, b := range asList(target["bucketAggs"]) {
				bucket := agg(b)
				if bucket["type"] != "terms" {
					continue
				}
				settings, _ := bucket["settings"].(map[string]any)
				by, _ := settings["orderBy"].(string)
				switch {
				case by == "_count" || by == "_key":
				case slices.Contains(ids, by):
					byMetric++
				default:
					t.Errorf("elasticsearch: %q orders its %v bucket by %q, and the query's "+
						"metrics are %v", p["title"], bucket["field"], by, ids)
				}
			}
		}
	}
	if byMetric == 0 {
		t.Fatal("no terms bucket orders by a metric, so this checked nothing but the names")
	}
}

// TestRenumberingCarriesTheOrderToTheMetricsNewID: a terms bucket that orders
// by a metric names the metric's id, and renumberES gives the metric another
// one; the reference moves with it. A bucket that orders by anything else is
// left alone, and so is a reference to an id no metric of its own query holds,
// which is a mistake the test above names rather than one renumbering should
// hide by pointing it somewhere.
func TestRenumberingCarriesTheOrderToTheMetricsNewID(t *testing.T) {
	t.Parallel()
	sum := map[string]any{"type": "sum", "id": "40"}
	byMetric := map[string]any{"orderBy": "40", "order": "desc"}
	byCount := map[string]any{"orderBy": "_count", "order": "desc"}
	byOther := map[string]any{"orderBy": "1", "order": "desc"}
	other := map[string]any{"type": "count", "id": "1"}
	panels := []map[string]any{
		{"targets": []any{map[string]any{"metrics": []any{other}}}},
		{"targets": []any{map[string]any{
			"metrics": []any{sum},
			"bucketAggs": []any{
				map[string]any{"type": "terms", "id": "41", "settings": byMetric},
				map[string]any{"type": "terms", "id": "42", "settings": byCount},
				map[string]any{"type": "terms", "id": "43", "settings": byOther},
			},
		}}},
	}
	renumberES(panels)
	if byMetric["orderBy"] != sum["id"] {
		t.Errorf("the bucket orders by %v after the metric it named became %v", byMetric["orderBy"], sum["id"])
	}
	if byCount["orderBy"] != "_count" {
		t.Errorf("the bucket ordered by count now orders by %v", byCount["orderBy"])
	}
	if byOther["orderBy"] != "1" {
		t.Errorf("a reference to another query's metric was rewritten to %v", byOther["orderBy"])
	}
}
