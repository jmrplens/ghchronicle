package dashboards

import (
	"regexp"
	"strings"
	"testing"
)

// indexTerm is every _index a query names, with whatever follows it up to
// the next space or bracket.
var indexTerm = regexp.MustCompile(`_index:([^\s()]+)`)

// TestEveryElasticsearchPanelNamesItsIndexExactly: a migration sets an index
// aside by cloning it to <index>-<instant> under the same prefix, and one
// datasource over <prefix>-* serves every panel, so the clone is read by any
// panel that does not name its index whole. Measured on 9.5.3 with a clone
// of three documents beside the new index of one, the panels' query counted
// one. A wildcard, or a query with no _index at all, would count the clone's
// rows twice for the day it is kept.
func TestEveryElasticsearchPanelNamesItsIndexExactly(t *testing.T) {
	t.Parallel()
	queries := 0
	for _, p := range renderedPanels(t, "elasticsearch") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			query, isQuery := target["query"].(string)
			if !isQuery {
				continue
			}
			queries++
			terms := indexTerm.FindAllStringSubmatch(query, -1)
			if len(terms) == 0 {
				t.Errorf("%q asks every index under the prefix: %s", p["title"], query)
			}
			for _, term := range terms {
				if strings.ContainsAny(term[1], "*?") || !strings.HasPrefix(term[1], "ghchronicle-gh_") {
					t.Errorf("%q names its index by a pattern, which a set-aside copy matches: %s", p["title"], query)
				}
			}
		}
	}
	if queries == 0 {
		t.Fatal("no Elasticsearch query was read")
	}
}
