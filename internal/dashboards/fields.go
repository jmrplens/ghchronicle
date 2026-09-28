package dashboards

import (
	"fmt"
	"strings"
)

// Field overrides that let one stat panel hold several values of different
// kinds.
//
// A stat's unit, thresholds and color mode are panel-wide: a single tile
// colored orange at 88.5% is the panel colored orange. Grouping the tiles,
// which is what a phone needs, therefore has to move both down to the field,
// or the success rate would paint the run count beside it. So a group carries
// plainSteps, which asks Grafana to color by field and resolves to the
// ordinary text color, and each value that is not a plain number names its
// own unit and its own thresholds.

// plainSteps is the panel-wide threshold list of a group: one step, the
// reading color, so a value with no override of its own looks exactly as it
// did as a tile of its own. Its presence is what turns the stat's color mode
// from "none" to "value", which is what makes a field's own thresholds paint.
var plainSteps = []any{map[string]any{"color": "text", "value": nil}}

// fieldThresholds paints one value of a group by its own thresholds, in its
// own unit.
func fieldThresholds(name, unit string, steps []any) any {
	return override(name, []any{
		map[string]any{"id": "unit", "value": unit},
		map[string]any{"id": "color", "value": map[string]any{"mode": "thresholds"}},
		map[string]any{"id": "thresholds", "value": map[string]any{"mode": "absolute", "steps": steps}},
	})
}

// fractionOf is the same thresholds against a value Elasticsearch answers as
// a fraction where the SQL stores answer as a percentage.
func fractionOf(steps []any) []any {
	scaled, _ := pctThresholds(steps)["thresholds"].([]any)
	return scaled
}

// esRef is one Elasticsearch query of a group under the refId the group's
// overrides name it by. The helpers that build a total return a list of one,
// because everywhere else a total is the panel's only query.
func esRef(ref string, t []Target) Target {
	t[0].Ref = ref
	return t[0]
}

// namedValue renames the one column a "SELECT ... AS value" statement
// selects, so two such statements can be two named values of one group.
func namedValue(sql, name string) string {
	return strings.Replace(sql, " AS value ", fmt.Sprintf(" AS %q ", name), 1)
}

// esRefs is esRef for a value whose Elasticsearch answer is more than one
// target, which is what a sum over the newest reading of each series is.
func esRefs(ref string, ts []Target) []Target {
	for i := range ts {
		ts[i].Ref = ref
	}
	return ts
}

// flagWords is the value mapping that draws a flag as a word, yes in green
// for set and `no` in noColor for unset. The SQL twins cast a flag to
// 1 or 0, the exporter publishes a gauge of one that way, and an
// Elasticsearch max or terms bucket over a boolean field answers a number.
// Two answers carry words instead: Elasticsearch's raw documents hand back
// the JSON true and false, and an exporter label is the text "true" or
// "false". Grafana matches a value mapping against the value's text, so both
// spellings are mapped: measured on the 2.6.1 review, "Sponsorships" read
// true and false in Elasticsearch and "Discussions" false and true in
// Prometheus beside the SQL stores' yes and no.
func flagWords(no, noColor string) map[string]any {
	return map[string]any{
		"1":     map[string]any{"text": "yes", "color": "green", "index": 0},
		"0":     map[string]any{"text": no, "color": noColor, "index": 1},
		"true":  map[string]any{"text": "yes", "color": "green", "index": 2},
		"false": map[string]any{"text": no, "color": noColor, "index": 3},
	}
}

// repoWidth is what a Repository column of a per-repository table is worth on
// a phone.
//
// It was 150. Measured at 430 pixels on 2026-09-14, that column sat in the
// middle of fifteen tables and pushed the column that actually names the row
// off the screen: "Largest merged pull requests" showed Number, Lines changed
// and Repository, and thirty pixels of Title; "Slowest steps" showed Step,
// Duration and Job and lost Repository itself. In most of them every visible
// row held the same repository. At 110 a name is cut exactly where it was cut
// at 150 (the longest here, gitlab-mcp-server, does not fit either way) and
// the forty pixels go to the column beside it.
const repoWidth = 110

// repoColumn is the Repository column of a per-repository table.
func repoColumn() any { return width("Repository", repoWidth) }

// fullNameWidth is the narrowest a Repository column of full names is drawn.
// repoWidth is measured against short names, and an owner and a slash cost
// most of it: at 110 "Work elsewhere" read another/projec and "Latest
// notifications" octocat/hello-w on the 2.6.1 review, on a desktop with room
// to spare. At 160, an owner and a name of twenty characters between them
// fit, and on a phone the column and the one beside it still leave the third
// in view.
const fullNameWidth = 160

// fullNameColumn is the Repository column of a table that names each
// repository in full, because the rows are mostly other people's. A minimum
// rather than a width, so on a desktop it takes a share of the room the
// fixed columns leave, as the title beside it does.
func fullNameColumn() any {
	return override("Repository", []any{map[string]any{"id": "custom.minWidth", "value": fullNameWidth}})
}
