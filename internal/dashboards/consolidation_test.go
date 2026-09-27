package dashboards

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestEveryGraphiteTableThatAddsItsPointsUpConsolidatesBySum is what a review
// of the 2.6.1 audit found in "Discussion answers", and the same hole in every
// Graphite table whose reducer is a sum.
//
// A table sends no maxDataPoints, so Grafana asks for the panel's width in
// pixels, and graphite-web fits the points into it by averaging neighbors.
// Against graphiteapp/graphite-statsd:1.1.10-5 and this repository's storage
// schema, three comments over now-30d read 3 with no maxDataPoints and 1.5 at
// 500, the width the dashboard checker sends: each comment's 1 averaged with
// the 0 isNonNull put beside it, then added up by the table. Consolidated by
// sum they read 3, and aliasSub takes back the name consolidateBy gives each
// series, which is the text of the row.
func TestEveryGraphiteTableThatAddsItsPointsUpConsolidatesBySum(t *testing.T) {
	t.Parallel()
	summed := regexp.MustCompile(`^removeEmptySeries\(aliasSub\(consolidateBy\(.*, "sum"\), ` +
		regexp.QuoteMeta(`"^consolidateBy\((.*),.sum.\)$", "\1"))`) + `$`)
	var titles []string
	for _, p := range renderedPanels(t, "graphite") {
		if p["type"] != "table" || !slices.Contains(graphiteReducers(p), any("sum")) {
			continue
		}
		title, _ := p["title"].(string)
		titles = append(titles, title)
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			expr, _ := target["target"].(string)
			if !summed.MatchString(expr) {
				t.Errorf("the Graphite table %q adds its points up and lets graphite-web average "+
					"them first, so a narrow panel halves its counts: %s", title, expr)
			}
		}
	}
	if !slices.Contains(titles, "Discussion answers") {
		t.Fatalf("Discussion answers is not among the Graphite tables that add up, %v, so this "+
			"no longer checks the table it was written for", titles)
	}
}

// TestNoDashboardTextNamesACadenceKeyTheParserRefuses: the Failed job output
// note told every reader to set `every.joblogs`, and since 2.6.0 the loader
// refuses a family's cadence anywhere but under every.families, with "every:
// is now three layers". The documentation pages were corrected and this
// generated text was not.
func TestNoDashboardTextNamesACadenceKeyTheParserRefuses(t *testing.T) {
	t.Parallel()
	flat := regexp.MustCompile("`every\\.([a-z_]+)")
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			text := asJSON(t, p)
			for _, m := range flat.FindAllStringSubmatch(text, -1) {
				if !slices.Contains([]string{"default", "groups", "families"}, m[1]) {
					title, _ := p["title"].(string)
					t.Errorf("%s: %q names `every.%s`, which the loader refuses: a family's "+
						"cadence is every.families.%s", store.Name, title, m[1], m[1])
				}
			}
		}
	}
}

// TestNoGraphiteSeriesIsDrawnUnderTheNameConsolidateByGivesIt is what the
// audit of 2.6.1 found in the Graphite charts. graphite-web renames every
// series consolidateBy is applied to, to consolidateBy(name,"sum"), and
// Grafana draws that name as it is. Against graphiteapp/graphite-statsd:
// 1.1.10-5 and Grafana 13.2.1, "Views over time" had two legend entries,
// consolidateBy(r1,"sum") and consolidateBy(r2,"sum"), and "Artifact storage
// over time" one, removeBelowValue(consolidateBy(r1,"max"), 1). So did the
// twenty three charts perBucket builds, the artifact curve among them, and
// "Open alerts over time", while the tables had been given their names back.
//
// Each consolidateBy is held to a function above it that names the series
// again: alias, which names it outright; aliasByNode, which graphite-web reads
// off the first path inside a name that ends in a parenthesis (measured there
// too: aliasByNode(consolidateBy(github.rate_limit.*.remaining, "min"), 2)
// reads core); or aliasSub directly around it, with the pattern that undoes
// exactly the name consolidateBy gave.
func TestNoGraphiteSeriesIsDrawnUnderTheNameConsolidateByGivesIt(t *testing.T) {
	t.Parallel()
	charts := 0
	for _, p := range renderedPanels(t, "graphite") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			expr, _ := target["target"].(string)
			for _, call := range consolidateCalls(t, expr) {
				if p["type"] == "timeseries" {
					charts++
				}
				if !call.renamed() {
					t.Errorf("graphite: %q draws its series as consolidateBy names them, inside %v: %s",
						p["title"], call.enclosing, expr)
				}
			}
		}
	}
	if charts == 0 {
		t.Fatal("no Graphite chart consolidates its series, so this checked none of the ones it was written for")
	}
}

// consolidateCall is one consolidateBy of a Graphite target: the function it
// consolidates by, the calls around it from the innermost out, and the text
// that follows its closing parenthesis.
type consolidateCall struct {
	how       string
	enclosing []string
	after     string
}

// renamed says whether a function above the call names its series again.
func (c consolidateCall) renamed() bool {
	for i, name := range c.enclosing {
		switch name {
		case "alias", "aliasByNode":
			return true
		case "aliasSub":
			undo := `, "^consolidateBy\((.*),.` + c.how + `.\)$", "\1")`
			return i == 0 && strings.HasPrefix(c.after, undo)
		}
	}
	return false
}

// consolidateCalls is every consolidateBy of a Graphite target, read by
// following its parentheses outside the quoted arguments.
func consolidateCalls(t *testing.T, expr string) []consolidateCall {
	t.Helper()
	type open struct {
		name  string
		start int
	}
	var stack []open
	var out []consolidateCall
	quote := byte(0)
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case quote != 0 && c == '\\':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			// A parenthesis inside a quoted argument opens and closes nothing.
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			start := i
			for start > 0 && isIdentifier(expr[start-1]) {
				start--
			}
			stack = append(stack, open{expr[start:i], i + 1})
		case c == ')':
			if len(stack) == 0 {
				t.Fatalf("an unbalanced parenthesis in %s", expr)
			}
			call := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if call.name != "consolidateBy" {
				continue
			}
			how := consolidatedHow.FindStringSubmatch(expr[call.start:i])
			if how == nil {
				t.Fatalf("a consolidateBy with no function to consolidate by in %s", expr)
			}
			var enclosing []string
			for _, around := range slices.Backward(stack) {
				enclosing = append(enclosing, around.name)
			}
			out = append(out, consolidateCall{how[1], enclosing, expr[i+1:]})
		}
	}
	return out
}

var consolidatedHow = regexp.MustCompile(`,\s*"([a-z]+)"\s*$`)

func isIdentifier(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
