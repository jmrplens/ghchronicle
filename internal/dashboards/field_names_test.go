package dashboards

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestEveryFieldOverrideNamesAFieldThePanelDraws is the review of 2.6.1
// across the five stores. A byName override is looked up under the name a
// field carries at the moment Grafana reaches that override, and Grafana
// reaches them in the order the list gives: a unit or a noValue addressed to
// a name nothing draws is dropped without a word. The Elasticsearch stat
// groups named their values with byFrameRefID displayName overrides placed
// after the shared units, so each unit was looked for under the name the
// response parser gives, "p50.0 seconds_to_merge", and Time to merge read
// "194 K" where the other stores read 2.25 days; Run duration read "260" and
// Actions minutes "$214".
//
// So every byName override of a stat or a gauge, in every store, has to name
// a field the panel draws by the time it is reached: a column the statement
// aliases, a legend or an alias the query gives, a column a transformation
// renames to, or a name an override earlier in the list gave.
//
// A table is held to the same order, and to less than the rest of it. Its
// overrides are one list for five stores, and a store that does not select a
// column leaves that column's width and unit addressed to nothing, which
// draws nothing wrong: of the 1,758 byName overrides on tables on the 2.6.2
// branch, 261 match no name their own store draws, and every one of them
// names a column another store does. So a table's override has to name a
// field some store draws, and where its own store draws the name, to be
// reached after the field has it. The charts are left out, since their series
// are named by the data.
func TestEveryFieldOverrideNamesAFieldThePanelDraws(t *testing.T) {
	t.Parallel()
	checked := 0
	tables := map[string]map[string]bool{}
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			switch p["type"] {
			case "stat", "gauge":
				checked += checkOverridesFind(t, store.Name, p, true)
			case "table":
				checked += checkOverridesFind(t, store.Name, p, false)
				title, _ := p["title"].(string)
				if tables[title] == nil {
					tables[title] = map[string]bool{}
				}
				maps.Copy(tables[title], everDrawn(p))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no byName override in any store, so this checked nothing")
	}
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			title, _ := p["title"].(string)
			if p["type"] != "table" {
				continue
			}
			for _, name := range byNameTargets(p) {
				if !tables[title][name] {
					t.Errorf("%s %q: an override addresses %q, which no store's table draws, so it "+
						"is dropped in all five", store.Name, title, name)
				}
			}
		}
	}
}

// checkOverridesFind walks a panel's overrides in Grafana's order, holding
// each byName one to a field that carries the name by then, and reports how
// many it checked. Not strict, a name the panel never draws at all is let
// through, which is a table's column its store does not select.
func checkOverridesFind(t *testing.T, store string, p map[string]any, strict bool) int {
	t.Helper()
	names, ever := drawnNames(p), everDrawn(p)
	checked := 0
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		if matcher["id"] == "byName" {
			checked++
			if !names[name] && (strict || ever[name]) {
				t.Errorf("%s %q: an override addresses %q, which no field carries when "+
					"Grafana reaches it, so its %v are dropped; the panel draws %v",
					store, p["title"], name, propertyIDs(o), slices.Sorted(keysOf(names)))
			}
		}
		if renamed := displayNameOf(o); renamed != "" {
			if matcher["id"] == "byName" {
				delete(names, name)
			}
			names[renamed] = true
		}
	}
	return checked
}

// everDrawn is every name a panel's fields carry at any point: what the
// queries and the transformations give them, and what any override renames
// them to.
func everDrawn(p map[string]any) map[string]bool {
	names := drawnNames(p)
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		if renamed := displayNameOf(o); renamed != "" {
			names[renamed] = true
		}
	}
	return names
}

// byNameTargets is the name every byName override of a panel addresses.
func byNameTargets(p map[string]any) []string {
	var out []string
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		if name, _ := matcher["options"].(string); matcher["id"] == "byName" {
			out = append(out, name)
		}
	}
	return out
}

// The names a query gives its fields, one pattern per way of giving one.
var (
	sqlFieldAlias = regexp.MustCompile(`\bAS "([^"]+)"`)
	graphiteAlias = regexp.MustCompile(`^alias\(.*, "([^"]+)"\)$`)
)

// drawnNames is every name a panel's fields carry once its queries answer and
// its transformations have run, before any override.
func drawnNames(p map[string]any) map[string]bool {
	names := map[string]bool{}
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		for _, n := range targetNames(target) {
			names[n] = true
		}
	}
	tfs, _ := p["transformations"].([]any)
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		switch tf["id"] {
		case "organize":
			rename, _ := options["renameByName"].(map[string]any)
			for from, to := range rename {
				if s, _ := to.(string); s != "" {
					delete(names, from)
					names[s] = true
				}
			}
		case "calculateField":
			if alias, _ := options["alias"].(string); alias != "" {
				names[alias] = true
			}
		}
	}
	return names
}

// targetNames is every name one query gives its fields: the aliases of a
// statement, a legend or alias that is not a template, the name Graphite's
// alias() gives, and the columns of an Elasticsearch answer.
func targetNames(target map[string]any) []string {
	var out []string
	if sql, _ := target["rawSql"].(string); sql != "" {
		for _, m := range sqlFieldAlias.FindAllStringSubmatch(sql, -1) {
			out = append(out, m[1])
		}
	}
	for _, key := range []string{"legendFormat", "alias"} {
		if s, _ := target[key].(string); s != "" && !strings.Contains(s, "{{") {
			out = append(out, s)
		}
	}
	if g, _ := target["target"].(string); g != "" {
		if m := graphiteAlias.FindStringSubmatch(g); m != nil {
			out = append(out, m[1])
		}
	}
	return append(out, esParserNames(target)...)
}

// esParserNames is what Grafana's Elasticsearch response parser names the
// columns of one target: the bucket fields by their own names and the metrics
// the way esCols works them out. An expression target is Grafana's own and
// is named by its refId.
func esParserNames(target map[string]any) []string {
	if target["type"] != nil && target["expression"] != nil {
		ref, _ := target["refId"].(string)
		return []string{ref}
	}
	metrics, _ := target["metrics"].([]any)
	if len(metrics) == 0 {
		return nil
	}
	var out []string
	buckets, _ := target["bucketAggs"].([]any)
	for _, raw := range buckets {
		b, _ := raw.(map[string]any)
		if f, _ := b["field"].(string); f != "" {
			out = append(out, f)
		}
	}
	if metrics[0].(map[string]any)["type"] == "raw_data" {
		return out
	}
	for _, group := range esCols(metrics) {
		out = append(out, group...)
	}
	return out
}

// displayNameOf is the name an override gives the fields it matches, or the
// empty string for one that names nothing.
func displayNameOf(o map[string]any) string {
	props, _ := o["properties"].([]any)
	for _, raw := range props {
		prop, _ := raw.(map[string]any)
		if prop["id"] == "displayName" {
			s, _ := prop["value"].(string)
			return s
		}
	}
	return ""
}

func propertyIDs(o map[string]any) []any {
	props, _ := o["properties"].([]any)
	out := make([]any, 0, len(props))
	for _, raw := range props {
		prop, _ := raw.(map[string]any)
		out = append(out, prop["id"])
	}
	return out
}

func keysOf(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
