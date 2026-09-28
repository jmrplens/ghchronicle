//go:build dockere2e

package docker

import (
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestAnEntryNoStoreComparedIsNotCalledStale needs no stack. The entries that
// name Prometheus alone can only be used by a pair with Prometheus in it, and
// where the exporter could not be loaded there is no such pair: they were
// reported as drawn alike, with the advice to take them out. An entry is
// stale only when one of its stores was compared on the panel and nothing
// needed it.
func TestAnEntryNoStoreComparedIsNotCalledStale(t *testing.T) {
	reason := "Prometheus counts sponsorships per direction and drops the other party"
	entries := []dashboardDiffer{{title: "Sponsorships", stores: []string{"prometheus"}, reason: reason}}
	run := &dashboardRun{outcomes: map[string]map[int]dashboardOutcome{
		"prometheus": {7: {panel: grafana.PanelQuery{Index: 7, Title: "Sponsorships", Description: "So. " + reason + "."}}},
		"influxdb":   {7: {panel: grafana.PanelQuery{Index: 7, Title: "Sponsorships"}}},
	}}

	withoutPrometheus := map[string]map[string]bool{"Sponsorships": {"influxdb": true, "postgres": true}}
	problems, unasked := dashboardDifferProblems(entries, run, map[int]bool{}, withoutPrometheus)
	if len(problems) > 0 {
		t.Errorf("an entry whose one store was never compared is reported: %v", problems)
	}
	if len(unasked) != 1 {
		t.Errorf("an entry whose one store was never compared is not said to be unasked: %v", unasked)
	}

	withPrometheus := map[string]map[string]bool{"Sponsorships": {"influxdb": true, "prometheus": true}}
	problems, _ = dashboardDifferProblems(entries, run, map[int]bool{}, withPrometheus)
	if len(problems) != 1 || !strings.Contains(problems[0], "take the entry out") {
		t.Errorf("an entry nothing needed once its store was compared is not called stale: %v", problems)
	}

	problems, _ = dashboardDifferProblems(entries, run, map[int]bool{0: true}, withPrometheus)
	if len(problems) > 0 {
		t.Errorf("an entry that excused a difference is reported: %v", problems)
	}
}
