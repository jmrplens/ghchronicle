package dashboards

import (
	"strings"
	"testing"
)

// The documentation audit of 2.6.0 read every panel description against what
// the collector and the queries do, and found sentences that had been true of
// an earlier release or of one store and were read in all five. Each test here
// pins one of those corrections to the rendered dashboards, which is where a
// reader meets the sentence.

// descriptionOf is a rendered panel's description, the shared sentence and the
// store's own joined as the reader sees them.
func descriptionOf(p map[string]any) string {
	desc, _ := p["description"].(string)
	return desc
}

// TestEveryArtifactFloorNamesBothCauses: the three panels that say the
// artifact size is a floor blamed the five page cap alone. Since 2.6.0 a walk
// cut short by a failed page writes the pages it read, with a Walked short of
// Declared, and that row is a floor for the other reason.
func TestEveryArtifactFloorNamesBothCauses(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "prometheus", "postgres", "graphite", "elasticsearch"} {
		named := 0
		for title, p := range rendered(t, store) {
			desc := descriptionOf(p)
			if !strings.Contains(desc, "floor") || !strings.Contains(strings.ToLower(title+desc), "artifact") {
				continue
			}
			if !strings.Contains(desc, "five hundred") {
				continue // a panel that says floor about something else
			}
			named++
			if !strings.Contains(desc, "fail") {
				t.Errorf("%s: %q calls the artifact size a floor for the page cap alone: %q", store, title, desc)
			}
		}
		if named == 0 {
			t.Errorf("%s: no panel calls the artifact size a floor, so this checks nothing", store)
		}
	}
}
