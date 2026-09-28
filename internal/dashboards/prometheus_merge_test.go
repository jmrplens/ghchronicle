package dashboards

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestEveryRepositoryEverMergesIntoOneRowPerRepository is the Prometheus
// table of the 2.6.1 review that listed hello-world seven times, one number
// on each row. Its seven queries were selectors joined by `or` and `unless`,
// which hand the series back with their metric name, and Grafana's merge
// joins rows on every field the frames share: seven names, seven rows. Each
// query is evaluated here over overviewAccount as the exporter serves it and
// merged the way the transformation merges, on every label a sample carries.
func TestEveryRepositoryEverMergesIntoOneRowPerRepository(t *testing.T) {
	t.Parallel()
	store, _ := ByName("prometheus")
	vars := grafana.Vars{Datasource: store.DS, Repos: []string{"hello-world"}, AllValue: ".*"}
	p := panelOf(t, store.Build(nil), "Every repository, ever", "table")
	targets, _ := p["targets"].([]any)
	rows := map[string]map[string]bool{}
	answered := map[string]bool{}
	for _, raw := range targets {
		target := vars.Apply(raw.(map[string]any))
		ref, _ := target["refId"].(string)
		for _, s := range evalPromSeries(t, target["expr"].(string), overviewPromSeries()) {
			key := mergeKey(s.labels)
			if rows[key] == nil {
				rows[key] = map[string]bool{}
			}
			rows[key][ref] = true
			answered[ref] = true
		}
	}
	// overviewAccount holds two of the seven fields, commits and stars.
	if len(answered) < 2 {
		t.Fatalf("only %v of the queries answered over overviewAccount, so no merge was tried", answered)
	}
	if len(rows) != len(overviewAccount) {
		t.Fatalf("the queries merge into %d rows over %d repositories: %v",
			len(rows), len(overviewAccount), slices.Sorted(maps.Keys(rows)))
	}
	for key, refs := range rows {
		if len(refs) != len(answered) {
			t.Errorf("the row %s holds the values of %v alone, want those of %v",
				key, slices.Sorted(maps.Keys(refs)), slices.Sorted(maps.Keys(answered)))
		}
	}
}

// mergeKey is what Grafana's merge transformation joins two rows on: every
// field both frames have, which for an instant query in table format is every
// label of the series, its metric name among them when it kept one.
func mergeKey(labels map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		fmt.Fprintf(&b, "%s=%q,", k, labels[k])
	}
	return b.String()
}

// TestMergedPrometheusTablesJoinOnLabelsAlone holds every Prometheus table
// that merges several queries to the shape that makes the merge join them:
// each query, and each side of an `or` in it, is an aggregation, which drops
// the metric name. A selector, or a set operation over selectors, keeps it,
// and the merge then has a field that differs between every pair of queries.
func TestMergedPrometheusTablesJoinOnLabelsAlone(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, p := range renderedPanels(t, "prometheus") {
		if !merges(p) {
			continue
		}
		targets, _ := p["targets"].([]any)
		for _, raw := range targets {
			target, _ := raw.(map[string]any)
			expr, _ := target["expr"].(string)
			checked++
			for _, operand := range topLevelOr(expr) {
				if !aggregates(operand) {
					t.Errorf("%q: %q keeps its metric name, so the merge that joins this "+
						"table's queries draws a row per query:\n%s", p["title"], operand, expr)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Prometheus table merges its queries, so this checked nothing")
	}
}

func merges(p map[string]any) bool {
	tfs, _ := p["transformations"].([]any)
	for _, raw := range tfs {
		if tf, _ := raw.(map[string]any); tf["id"] == "merge" {
			return true
		}
	}
	return false
}

// topLevelOr splits an expression at every `or` outside parentheses. An
// outer pair of parentheses is looked through, since it changes nothing.
func topLevelOr(expr string) []string {
	expr = strings.TrimSpace(expr)
	for strings.HasPrefix(expr, "(") && closingParen(expr, 0) == len(expr)-1 {
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	var out []string
	depth, start := 0, 0
	for i := range len(expr) {
		switch expr[i] {
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		}
		if depth == 0 && strings.HasPrefix(expr[i:], " or ") {
			out = append(out, strings.TrimSpace(expr[start:i]))
			start = i + len(" or ")
		}
	}
	return append(out, strings.TrimSpace(expr[start:]))
}

// closingParen is the index of the parenthesis that closes the one at open.
func closingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// aggregates reports whether an operand starts with an aggregation operator,
// whose answer carries the labels it groups by and no metric name.
func aggregates(operand string) bool {
	for _, op := range []string{"sum", "avg", "max", "min", "count", "topk", "bottomk", "group", "quantile"} {
		rest, found := strings.CutPrefix(operand, op)
		if found && (strings.HasPrefix(rest, " by ") || strings.HasPrefix(rest, " without ") ||
			strings.HasPrefix(rest, "(")) {
			return true
		}
	}
	return false
}
