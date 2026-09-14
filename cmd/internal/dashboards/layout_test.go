package dashboards

import (
	"fmt"
	"sort"
	"testing"
)

// TestEverySectionTilesItsRow: a section's panels cover the rectangle they
// sit in, with no cell claimed twice and none left empty.
//
// Grafana does not draw the grid it is given. The dashboard grid compacts
// upwards, so an empty cell is not blank space: every panel under it that
// nothing else blocks rises into it, and two tables written side by side
// arrive one above the other. Measured on the published dashboard on
// 2026-09-14, after a round that had grown eight panels without growing the
// panel beside them: "Secret rotation" rendered 152 pixels above "Workflow
// token permissions", "Dependency changes" 152 above "Dependencies by
// ecosystem", and "Workflows" 38 above "Slowest jobs". An overlap is the same
// bug from the other side: the panel is pushed down and every y below it is
// one more than the specification says.
//
// The specification had neither before that round, in 174 panels across
// seventeen sections, which is why this is a rule and not a preference.
func TestEverySectionTilesItsRow(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		list, ok := store.Build(nil)["panels"].([]map[string]any)
		if !ok {
			t.Fatalf("%s renders no panel list", store.Name)
		}
		groups := sections(t, list)
		if len(groups) < 17 {
			t.Fatalf("%s: walked %d sections, and the specification has seventeen",
				store.Name, len(groups))
		}
		for _, g := range groups {
			for _, complaint := range tiling(g.panels) {
				t.Errorf("%s, %s: %s", store.Name, g.title, complaint)
			}
		}
	}
}

// section is one row's panels, or the panels above the first row.
type section struct {
	title  string
	panels []map[string]any
}

// sections splits a dashboard into the groups that are laid out independently:
// each row's own panels, and the panels that sit above every row.
func sections(t *testing.T, list []map[string]any) []section {
	t.Helper()
	out := []section{{title: "above the first row"}}
	for _, p := range list {
		if p["type"] != "row" {
			out[0].panels = append(out[0].panels, p)
			continue
		}
		title, _ := p["title"].(string)
		inner, _ := p["panels"].([]map[string]any)
		if len(inner) > 0 {
			out = append(out, section{title: title, panels: inner})
		}
	}
	if len(out[0].panels) == 0 {
		t.Fatal("no panel sits above the first row, so this walked the wrong document")
	}
	return out
}

// tiling reports every cell of a section's rectangle that two panels claim or
// that none does, named by the panels involved so the message says which
// gridPos to change.
func tiling(panels []map[string]any) []string {
	type cell struct{ x, y int }
	held := map[cell]string{}
	var out []string
	top, bottom := 1<<30, 0
	for _, p := range panels {
		g, _ := p["gridPos"].(map[string]any)
		x, y, w, h := at(g, "x"), at(g, "y"), at(g, "w"), at(g, "h")
		title, _ := p["title"].(string)
		top, bottom = min(top, y), max(bottom, y+h)
		for dy := range h {
			for dx := range w {
				c := cell{x + dx, y + dy}
				if other, taken := held[c]; taken {
					out = append(out,
						fmt.Sprintf("%q overlaps %q at column %d of row %d", title, other, c.x, c.y))
				}
				held[c] = title
			}
		}
	}
	for y := top; y < bottom; y++ {
		var empty []int
		for x := range 24 {
			if _, taken := held[cell{x, y}]; !taken {
				empty = append(empty, x)
			}
		}
		if len(empty) > 0 {
			out = append(out, fmt.Sprintf(
				"row %d leaves columns %d to %d empty, and the grid compacts what is under them upwards",
				y, empty[0], empty[len(empty)-1],
			))
		}
	}
	sort.Strings(out)
	return dedup(out)
}

// at is one number of a gridPos, whichever numeric type the renderer wrote.
func at(g map[string]any, key string) int {
	switch v := g[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// dedup keeps one complaint per distinct message: an overlap of two panels is
// one mistake however many cells it covers.
func dedup(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
