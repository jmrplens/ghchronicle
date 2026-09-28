package dashboards

import (
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
// So every byName override of a stat, a gauge or a table, in every store, has
// to name a field the panel draws by the time it is reached: a column the
// statement aliases, a legend or an alias the query gives, a column a
// transformation renames to, or a name an override earlier in the list gave.
// The charts are left out, since their series are named by the data.
func TestEveryFieldOverrideNamesAFieldThePanelDraws(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			if p["type"] == "stat" || p["type"] == "gauge" {
				checked += checkOverridesFind(t, store.Name, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no byName override in any store, so this checked nothing")
	}
}

// checkOverridesFind walks a panel's overrides in Grafana's order, holding
// each byName one to a field that carries the name by then, and reports how
// many it checked.
func checkOverridesFind(t *testing.T, store string, p map[string]any) int {
	t.Helper()
	names := drawnNames(p)
	checked := 0
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		if matcher["id"] == "byName" {
			checked++
			if !names[name] {
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
