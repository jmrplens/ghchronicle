package dashboards

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// saysItDoesNotFold is how a store's own words say it draws every series or
// bar where the SQL stores fold the tail into other.
var saysItDoesNotFold = regexp.MustCompile(`folds nothing|folds none|no remainder to fold`)

// TestEveryStoreFoldsTheRestIntoOtherOrSaysItDoesNot holds every panel the
// SQL stores fold, the busiest few named and the rest one series or bar
// called other, to folding the same way in every store that answers it, or
// to saying in that store's own words that it does not. The descriptions are
// shared and say the rest are other, and the 2.6.2 review found fifteen
// panels where Graphite and Elasticsearch drew every series under that
// sentence, and Prometheus eleven; three of them said so.
func TestEveryStoreFoldsTheRestIntoOtherOrSaysItDoesNot(t *testing.T) {
	t.Parallel()
	folded := map[int]map[string]any{}
	for i, p := range renderedPanels(t, "influxdb") {
		if strings.Contains(asJSON(t, p["targets"]), "'other'") {
			folded[i] = p
		}
	}
	if len(folded) < 15 {
		t.Fatalf("only %d panels fold in the SQL stores, too few for this to have held anything", len(folded))
	}
	for _, store := range []string{"prometheus", "graphite", "elasticsearch"} {
		for i, p := range renderedPanels(t, store) {
			sql, ok := folded[i]
			if !ok || len(panelTargets(p)) == 0 {
				continue
			}
			folds := strings.Contains(asJSON(t, p["targets"]), `\"other\"`)
			own := grafana.OwnWords(descriptionOf(p), descriptionOf(sql))
			if !folds && !saysItDoesNotFold.MatchString(own) {
				t.Errorf("%q folds the rest into other in the SQL stores, and %s draws every one of them "+
					"without saying so: %q", p["title"], store, own)
			}
			if folds && saysItDoesNotFold.MatchString(own) {
				t.Errorf("%q folds the rest into other in %s, and its description says it does not: %q",
					p["title"], store, own)
			}
		}
	}
}
