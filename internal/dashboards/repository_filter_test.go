package dashboards

import (
	"strings"
	"testing"
)

// TestElasticsearchNarrowsToThePickedRepositoriesWhereSQLDoes is what the
// audit of 2.6.1 found in the Security row's "Open alerts". Its Elasticsearch
// tile read the newest open count of every repository in the store whatever
// the picker held, while the SQL tile of the other dashboards read the picked
// ones, and so did every other Elasticsearch panel of the row. The query came
// from esLatestSum, which put no repository clause in it, so the artifact and
// cache bytes of "Runs in range" and the total of "Downloads" ignored the
// picker the same way.
//
// A query of the InfluxDB dashboard that names the repository variable is
// held to the query of the same letter in the same Elasticsearch panel, which
// has to name it too.
func TestElasticsearchNarrowsToThePickedRepositoriesWhereSQLDoes(t *testing.T) {
	t.Parallel()
	type query struct{ title, kind, ref string }
	idOf := func(p, target map[string]any) query {
		title, _ := p["title"].(string)
		kind, _ := p["type"].(string)
		ref, _ := target["refId"].(string)
		return query{title, kind, ref}
	}
	narrowed := map[query]bool{}
	for _, p := range renderedPanels(t, "influxdb") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			if sql, _ := target["rawSql"].(string); strings.Contains(sql, RF) {
				narrowed[idOf(p, target)] = true
			}
		}
	}
	checked := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			lucene, isSearch := target["query"].(string)
			id := idOf(p, target)
			if !isSearch || !narrowed[id] {
				continue
			}
			checked++
			if !strings.Contains(lucene, ESF) {
				t.Errorf("elasticsearch: %s %q reads every repository in %s, where the SQL "+
					"query %s reads the picked ones: %s", id.kind, id.title, id.ref, id.ref, lucene)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Elasticsearch query has an SQL twin that names the repository variable, " +
			"so this checked nothing")
	}
}
