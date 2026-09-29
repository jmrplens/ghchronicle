//go:build dockere2e

package docker

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/dashboards"
	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestThePostgreSQLDashboardLeavesNoTextToTheCollation holds the PostgreSQL
// dashboard to InfluxDB's order of text whatever the rows hold. The
// translation gives COLLATE "C" to every text key it sorts by, and to every
// text argument of a MIN or a MAX, and leaves the rest alone; which is which it
// reads from the names, and a name it reads wrongly would leave that text to
// the database's collation. TestTheSQLStoresDrawTheRowsInOneOrder sees that
// only where the sweep wrote two values the collation orders the other way
// round: three panels of 49 on 2026-09-29.
//
// So every expression the dashboard sorts by without the collation is asked
// once more with it. PostgreSQL refuses a collation on a number, a flag or a
// time before it reads a row ("collations are not supported by type"), so a
// probe it answers is a text key the translation left to the collation, found
// from the statement alone. Measured on 2026-09-29 against the dashboard as
// it was before the translation compared text by its bytes: 218 probes of 103
// panels were answered, where the order comparison saw three panels. A panel
// the store refuses as it stands, for a table or a column this sweep never
// wrote (dashboardMissing), refuses its probes for the same reason, and is
// left out.
func TestThePostgreSQLDashboardLeavesNoTextToTheCollation(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	doc, err := dashboardDocument("postgres")
	if err != nil {
		t.Fatal(err)
	}
	var store dashboardStore
	for _, st := range dashboardStores {
		if st.name == "postgres" {
			store = st
		}
	}
	var probes []grafana.PanelQuery
	var keys []string
	unasked := 0
	for _, p := range grafana.Panels(doc["panels"]) {
		if run.outcomes["postgres"][p.Index].err != "" {
			unasked++
			continue
		}
		for _, target := range p.Targets {
			sql, _ := target["rawSql"].(string)
			for _, probe := range dashboards.CollationProbes(sql) {
				q := maps.Clone(target)
				q["rawSql"] = probe.SQL
				// No links: a probe is asked for its error, not for its rows.
				probes = append(probes, grafana.PanelQuery{
					Index: p.Index, Title: p.Title, Type: p.Type, Targets: []map[string]any{q},
					From: p.From, MaxDataPoints: p.MaxDataPoints,
				})
				keys = append(keys, probe.Key)
			}
		}
	}
	client := grafana.Client{URL: s.GrafanaURL, Token: s.GrafanaToken}
	results := client.CheckPanels(t.Context(), dashboardRange, "now", probes,
		dashboardVars(doc, store, dashboardRepos(run.sweeps[0])), grafana.Options{
			Timeout: 60 * time.Second, Workers: 8,
			IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
		})
	refused := 0
	for i, r := range results {
		switch {
		case strings.Contains(r.Err, "collations are not supported by type"):
			refused++
		case r.Err == "":
			t.Errorf("panel %d %q sorts by %s, which is text, in the database's collation, where "+
				"InfluxDB compares its bytes: byteOrder read it as something else", r.Panel.Index, r.Panel.Title, keys[i])
		default:
			t.Errorf("panel %d %q: the probe of %s failed for another reason, so it tells nothing: %s",
				r.Panel.Index, r.Panel.Title, keys[i], r.Err)
		}
	}
	t.Logf("%d expressions the PostgreSQL dashboard sorts by without the collation, each refused one; "+
		"%d panels the store refuses as they stand were not probed", refused, unasked)
	// A floor rather than a count, so a change that stops the probes from
	// being made fails instead of passing with nothing asked. Measured at 220
	// on 2026-09-29.
	if refused < 200 {
		t.Errorf("only %d probes were refused, which is too few for this to have asked anything", refused)
	}
}
