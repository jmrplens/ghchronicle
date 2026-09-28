package dashboards

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestEveryColumnAStoreLacksIsNamedInItsDescription holds every column the
// SQL stores draw in a table to being drawn by each other store that answers
// the table, or named in that store's own words about the panel, for every
// table there is.
//
// The containerised suite holds what is drawn, and can only hold a table the
// fixture fills in both SQL stores: a table the fixture leaves empty there,
// "Workflows that keep failing" with its one failed run against a threshold of
// three, was skipped in every store, and the Prometheus twin of it had no
// Failure rate its description named. So this holds the specification itself:
// the SQL stores' columns are the ones their statement selects and does not
// hide, a store draws the names its transformations give, and its own words
// are what its description adds to the one every store shares. The same rule
// as the suite's, grafana.DrawsColumn and grafana.NamesColumn, so the two
// cannot disagree about what counts as said.
func TestEveryColumnAStoreLacksIsNamedInItsDescription(t *testing.T) {
	t.Parallel()
	sqlTables := map[int]map[string]any{}
	for i, p := range renderedPanels(t, "influxdb") {
		if p["type"] == "table" && len(panelTargets(p)) > 0 && p["transformations"] == nil {
			sqlTables[i] = p
		}
	}
	held := 0
	for _, store := range []string{"prometheus", "graphite", "elasticsearch"} {
		for i, p := range renderedPanels(t, store) {
			sql, ok := sqlTables[i]
			if !ok || p["type"] != "table" || len(panelTargets(p)) == 0 {
				continue
			}
			held++
			selected := visibleSelected(sql)
			drawn := drawnColumns(p)
			own := grafana.OwnWords(descriptionOf(p), descriptionOf(sql))
			var unsaid []string
			for _, column := range selected {
				if !grafana.DrawsColumn(drawn, selected, column, store == "graphite") && !grafana.NamesColumn(own, column) {
					unsaid = append(unsaid, column)
				}
			}
			if len(unsaid) > 0 {
				t.Errorf("%q in %s draws no %s, which the SQL stores draw, and its description does not "+
					"say so: draw the column or name it where the description says what %s cannot hold. "+
					"What it adds to the shared text: %q", p["title"], store, strings.Join(unsaid, ", "), store, own)
			}
		}
	}
	if held < 150 {
		t.Fatalf("only %d tables were held to the SQL stores' columns, too few for this to have held anything", held)
	}
}

// visibleSelected is the columns a SQL table selects and draws: every alias of
// its statements but those its overrides hide, which is the url the row links
// from.
func visibleSelected(p map[string]any) []string {
	var sql []Target
	for _, target := range panelTargets(p) {
		s, _ := target["rawSql"].(string)
		sql = append(sql, Target{SQL: s})
	}
	hidden := hiddenColumns(p)
	var out []string
	for _, column := range selectedColumns(sql) {
		if !hidden[column] {
			out = append(out, column)
		}
	}
	return out
}

// drawnColumns is every name a table's transformations leave standing, in
// the order they run: what an organize renames a column to, less what one
// excludes, and what a calculation names its result, less the columns an
// override hides.
func drawnColumns(p map[string]any) map[string]bool {
	out := map[string]bool{}
	tfs, _ := p["transformations"].([]any)
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		rename, _ := options["renameByName"].(map[string]any)
		for _, to := range rename {
			if name, ok := to.(string); ok {
				out[name] = true
			}
		}
		exclude, _ := options["excludeByName"].(map[string]any)
		for name, gone := range exclude {
			if gone == true {
				delete(out, name)
			}
		}
		if alias, ok := options["alias"].(string); ok && tf["id"] == "calculateField" {
			out[alias] = true
		}
	}
	for name := range hiddenColumns(p) {
		delete(out, name)
	}
	return out
}

// hiddenColumns is every column a panel's overrides hide.
func hiddenColumns(p map[string]any) map[string]bool {
	out := map[string]bool{}
	config, _ := p["fieldConfig"].(map[string]any)
	overrides, _ := config["overrides"].([]any)
	for _, raw := range overrides {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		props, _ := o["properties"].([]any)
		if slices.ContainsFunc(props, func(raw any) bool {
			prop, _ := raw.(map[string]any)
			return prop["id"] == "custom.hidden" && prop["value"] == true
		}) {
			out[name] = true
		}
	}
	return out
}
