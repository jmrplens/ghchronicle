package grafana

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// PanelQuery is one panel and every target it renders with.
//
// The panel rather than the target is the unit here because that is how
// Grafana asks: one /api/ds/query request carries every target of the panel,
// and a server-side expression target reads its inputs from the others by
// refId. A runner that posted one target per request would report the gauge
// that divides two counts as broken while it renders perfectly.
type PanelQuery struct {
	// Index is the panel's ordinal in the dashboard, counting the text panels
	// a store that cannot answer a panel is given. Every store's dashboard is
	// built from one specification and keeps the same layout, so this is the
	// key that identifies the same panel across all five of them, which two
	// panels sharing a title are not.
	Index int
	Title string
	// Description is the panel's own prose. Where a store answers a panel
	// differently from the others the specification says so here, so this is
	// the allowance a comparison between two stores reads rather than a list
	// kept somewhere else.
	Description string
	Type        string
	Targets     []map[string]any
	// Links is every column the panel draws as a link to the cell's own
	// value, read from its field overrides; Renames is what the panel's
	// organize transformations call the fields the datasource returns. A
	// link column is only ever the name after the rename, and the frame only
	// ever carries the name before it, so checking one needs both.
	Links   []string
	Renames map[string]string
	// From is the panel's own range, its `timeFrom`, or the empty string
	// when it takes the dashboard's. Grafana replaces the request's `from`
	// with this before the query is sent, so a checker that ignored it asked
	// a question no render ever asks: the Code panels are pinned to ninety
	// days because gh_commit cannot be read further back, and posted at two
	// years they came back with the file limit error under a dashboard that
	// draws them perfectly.
	From string
}

// Panels walks a dashboard document and yields every panel that queries
// something, descending into rows, which carry their own panels. A panel with
// no targets is skipped: that is the text panel the specification emits for a
// store that cannot answer, and it is not a query.
func Panels(panels any) []PanelQuery {
	seen := 0
	return appendPanels(nil, panels, &seen)
}

// appendPanels is Panels with the ordinal carried through the rows.
func appendPanels(out []PanelQuery, panels any, seen *int) []PanelQuery {
	for _, raw := range list(panels) {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if p["type"] == "row" {
			out = appendPanels(out, p["panels"], seen)
			continue
		}
		index := *seen
		*seen++
		targets := list(p["targets"])
		if len(targets) == 0 {
			continue
		}
		q := PanelQuery{Index: index, Targets: make([]map[string]any, 0, len(targets))}
		q.Title, _ = p["title"].(string)
		q.Description, _ = p["description"].(string)
		q.Type, _ = p["type"].(string)
		q.Links = LinkColumns(p)
		q.Renames = Renames(p)
		q.From, _ = p["timeFrom"].(string)
		for _, t := range targets {
			if tgt, isObject := t.(map[string]any); isObject {
				q.Targets = append(q.Targets, tgt)
			}
		}
		if len(q.Targets) > 0 {
			out = append(out, q)
		}
	}
	return out
}

// LinkHref is the data link every link column carries: the cell's own raw
// value. It is what makes a link column checkable at all, since the href is
// then the value the store returned and nothing else.
const LinkHref = "${__value.raw}"

// LinkColumns is every column a panel's field overrides read a url from: the
// cell's own value, which is how a table draws a url, or another column of
// the row, which is how a chart hangs one on a bar. Any other data link, one
// built from a template say, is left alone: nothing here could say where it
// goes.
func LinkColumns(panel map[string]any) []string {
	config, _ := panel["fieldConfig"].(map[string]any)
	var out []string
	for _, raw := range list(config["overrides"]) {
		o, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		if matcher["id"] != "byName" || name == "" {
			continue
		}
		for _, column := range linkedColumns(o, name) {
			if !slices.Contains(out, column) {
				out = append(out, column)
			}
		}
	}
	return out
}

// linkedColumns is the columns an override's data links read.
func linkedColumns(override map[string]any, name string) []string {
	var out []string
	for _, raw := range list(override["properties"]) {
		prop, ok := raw.(map[string]any)
		if !ok || prop["id"] != "links" {
			continue
		}
		for _, l := range list(prop["value"]) {
			link, _ := l.(map[string]any)
			url, _ := link["url"].(string)
			if url == LinkHref {
				out = append(out, name)
			} else if column, found := strings.CutPrefix(url, "${__data.fields."); found {
				out = append(out, strings.TrimSuffix(column, "}"))
			}
		}
	}
	return out
}

// Renames is the renameByName of every organize transformation a panel
// carries, field as returned to column as drawn.
func Renames(panel map[string]any) map[string]string {
	out := map[string]string{}
	for _, raw := range list(panel["transformations"]) {
		tf, ok := raw.(map[string]any)
		if !ok || tf["id"] != "organize" {
			continue
		}
		options, _ := tf["options"].(map[string]any)
		rename, _ := options["renameByName"].(map[string]any)
		for from, to := range rename {
			if s, isString := to.(string); isString {
				out[from] = s
			}
		}
	}
	return out
}

// Vars is what a dashboard render substitutes into a panel's query before the
// query reaches a datasource. /api/ds/query performs none of it: template
// variables belong to the dashboard, and the dashboard is not in the request.
type Vars struct {
	// Datasource is what the ${DS_*} placeholder an exported dashboard carries
	// resolves to: usually map[string]any{"type": ..., "uid": ...}.
	Datasource any
	// Repos is what the repository variable expands to when All is selected
	// and the variable has no allValue of its own.
	Repos []string
	// AllValue is the variable's allValue. Grafana substitutes that string
	// verbatim, formatter and all, so a variable that has one never expands to
	// a list: the Graphite and Prometheus dashboards ask for `*` and `.*`.
	AllValue string
	// TimeFilter is what $__timeFilter(time) becomes. Left empty the macro
	// stays in the query for the datasource to expand, which is what the
	// PostgreSQL plugin does with it and with $__timeGroupAlias.
	TimeFilter string
	// TimeFilterFormat is TimeFilter with the window left open, taking one
	// verb: "time >= now() - INTERVAL '%s'". It is what a panel with a range
	// of its own gets instead, since a substitution made once from the
	// dashboard's range would hand that panel the range it is pinned away
	// from. Only the store whose plugin cannot expand the macro needs it, so
	// left empty the panel's own range still moves the request's from and to
	// and the plugin does the rest.
	TimeFilterFormat string
}

// The repository variable, in each of the forms the five dashboards ask for.
// Longest first, so the bare name cannot eat the head of a formatted one.
var repoTokens = []struct{ token, format string }{
	{"${repo:singlequote}", "singlequote"},
	{"${repo:sqlstring}", "sqlstring"},
	{"${repo:lucene}", "lucene"},
	{"${repo}", "glob"},
	{"$repo", "glob"},
}

// Apply renders one target. Everything it does not know about is left alone
// for the datasource plugin, which is what expands $__timeGroupAlias and
// $__range in a real render.
func (v Vars) Apply(target map[string]any) map[string]any {
	out := make(map[string]any, len(target))
	maps.Copy(out, target)
	for k, val := range out {
		if k == "datasource" {
			continue
		}
		out[k] = mapStrings(val, v.text)
	}
	// An expression target is answered by Grafana itself, and its datasource
	// reference is what selects that, so it is the one that must not be
	// pointed at a store.
	if !isExpression(target) {
		out["datasource"] = v.Datasource
	}
	return out
}

// text substitutes into one string.
func (v Vars) text(s string) string {
	if v.TimeFilter != "" {
		s = strings.ReplaceAll(s, "$__timeFilter(time)", v.TimeFilter)
	}
	for _, r := range repoTokens {
		if !strings.Contains(s, r.token) {
			continue
		}
		value := v.AllValue
		if value == "" {
			value = repoList(v.Repos, r.format)
		}
		s = strings.ReplaceAll(s, r.token, value)
	}
	return s
}

// repoList renders the repository list the way Grafana's own formatters do:
// singlequote escapes an apostrophe with a backslash and sqlstring by doubling
// it, lucene joins with OR, and the default is the Graphite brace list.
func repoList(repos []string, format string) string {
	quoted := make([]string, len(repos))
	for i, r := range repos {
		switch format {
		case "singlequote":
			quoted[i] = "'" + strings.ReplaceAll(r, "'", `\'`) + "'"
		case "sqlstring":
			quoted[i] = "'" + strings.ReplaceAll(r, "'", "''") + "'"
		case "lucene":
			quoted[i] = `"` + r + `"`
		default:
			quoted[i] = r
		}
	}
	if format == "lucene" {
		if len(quoted) == 1 {
			return quoted[0]
		}
		return "(" + strings.Join(quoted, " OR ") + ")"
	}
	if format == "glob" {
		if len(quoted) == 1 {
			return quoted[0]
		}
		return "{" + strings.Join(quoted, ",") + "}"
	}
	return strings.Join(quoted, ",")
}

// Unrendered reports every template variable a render should have replaced and
// did not. It matters because a leftover $repo in a Graphite path or a Lucene
// filter matches nothing and the panel comes back empty rather than failing,
// which is indistinguishable from the store simply not holding the data.
func Unrendered(target map[string]any) []string {
	var found []string
	for _, r := range repoTokens {
		mapStrings(target, func(s string) string {
			if strings.Contains(s, r.token) && !slices.Contains(found, r.token) {
				found = append(found, r.token)
			}
			return s
		})
	}
	return found
}

// mapStrings rebuilds a decoded JSON value with every string in it passed
// through f. It rebuilds rather than rewrites because the value belongs to the
// dashboard document, which is read once and rendered per store.
func mapStrings(x any, f func(string) string) any {
	switch t := x.(type) {
	case string:
		return f(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = mapStrings(v, f)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = mapStrings(v, f)
		}
		return out
	default:
		return x
	}
}

// isExpression reports whether a target is one of Grafana's own server-side
// expressions rather than a query for a store.
func isExpression(target map[string]any) bool {
	ds, ok := target["datasource"].(map[string]any)
	if !ok {
		return false
	}
	return ds["uid"] == "__expr__" || ds["type"] == "__expr__"
}

// Options paces a run of the panels.
type Options struct {
	// Timeout is one panel's whole request.
	Timeout time.Duration
	// Workers is how many panels are in flight at once. A dashboard here is
	// around 150 panels and a whole store's worth is minutes at one at a time.
	Workers int
	// IntervalMs and MaxDataPoints are what a rendered panel sends: the width
	// of the browser's graph decides them, and several queries interpolate
	// $__interval and $__rate_interval out of them.
	IntervalMs    int
	MaxDataPoints int
}

// Result is what one panel answered.
type Result struct {
	Panel PanelQuery
	// Rows is every row of every frame every query of the panel returned.
	Rows int
	// Err is what Grafana reported, empty when it reported nothing.
	Err string
	// Answer is the whole reply, for a caller that wants the values and not
	// just how many there were.
	Answer map[string]any
}

// CheckPanels runs every panel through /api/ds/query the way a rendered
// dashboard does, and returns one result per panel, in the order given.
func (c Client) CheckPanels(ctx context.Context, from, to string,
	panels []PanelQuery, vars Vars, opt Options,
) []Result {
	out := make([]Result, len(panels))
	workers := max(opt.Workers, 1)
	queue := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range queue {
				out[i] = c.checkPanel(ctx, from, to, &panels[i], vars, opt)
			}
		})
	}
	for i := range panels {
		queue <- i
	}
	close(queue)
	wg.Wait()
	return out
}

// checkPanel posts one panel's targets as one request, which is what a render
// does.
func (c Client) checkPanel(ctx context.Context, from, to string,
	panel *PanelQuery, vars Vars, opt Options,
) Result {
	r := Result{Panel: *panel}
	// The panel's own range wins over the dashboard's, which is what Grafana
	// does with timeFrom. `to` stays: timeFrom sets the start of a window
	// that still ends now.
	if panel.From != "" {
		from = "now-" + panel.From
		if vars.TimeFilterFormat != "" {
			// vars is a value, so this is this panel's substitution alone.
			vars.TimeFilter = fmt.Sprintf(vars.TimeFilterFormat, panel.From)
		}
	}
	queries := make([]any, 0, len(panel.Targets))
	for _, t := range panel.Targets {
		q := vars.Apply(t)
		if opt.IntervalMs > 0 {
			q["intervalMs"] = opt.IntervalMs
		}
		if opt.MaxDataPoints > 0 {
			q["maxDataPoints"] = opt.MaxDataPoints
		}
		if left := Unrendered(q); len(left) > 0 {
			r.Err = "the render left " + strings.Join(left, ", ") + " in the query"
			return r
		}
		queries = append(queries, q)
	}
	res, err := c.Queries(ctx, from, to, queries, opt.Timeout)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.Answer = res
	r.Rows, r.Err = Answer(res)
	if r.Err == "" && r.Rows > 0 {
		r.Err = linksAnswered(res, panel)
	}
	return r
}

// linkValue is what a link column may hold: an absolute url, which the value
// mapping turns into the word and the data link opens, or nothing.
var linkValue = regexp.MustCompile(`^https?://.+`)

// linksAnswered holds a panel's answer to its link columns: each one has to
// be in the frame, under the name the datasource gives it, and every value in
// it has to be an absolute url or nothing. Nothing is a null from the SQL
// stores and the empty string from Elasticsearch, which has no null and
// buckets a document without a url under "" so the row stays in the table;
// Grafana draws no link for either. An override matched byName against a
// column the frame lacks draws nothing and reports nothing, and a value that
// is not a url is drawn as itself with a link to the Grafana host, so neither
// is visible anywhere but here.
func linksAnswered(res map[string]any, panel *PanelQuery) string {
	for _, column := range panel.Links {
		values, found := linkColumn(res, column, panel.Renames)
		if !found {
			return fmt.Sprintf("the %s column the panel links from is not in the answer", column)
		}
		for _, v := range values {
			if v == nil || v == "" {
				continue
			}
			s, isString := v.(string)
			if !isString || !linkValue.MatchString(s) {
				return fmt.Sprintf("the %s column holds %q, which is not an absolute url",
					column, trim(fmt.Sprint(v), 120))
			}
		}
	}
	return ""
}

// linkColumn is the values of the field that becomes the named column, in
// every frame of every query of the answer, and whether any frame had it.
func linkColumn(res map[string]any, column string, renames map[string]string) (values []any, found bool) {
	sources := []string{column}
	for from, to := range renames {
		if to == column {
			sources = append(sources, from)
		}
	}
	results, _ := res["results"].(map[string]any)
	for _, answer := range results {
		a, _ := answer.(map[string]any)
		for _, f := range list(a["frames"]) {
			frame, _ := f.(map[string]any)
			schema, _ := frame["schema"].(map[string]any)
			data, _ := frame["data"].(map[string]any)
			columns := list(data["values"])
			for i, entry := range list(schema["fields"]) {
				field, _ := entry.(map[string]any)
				name, _ := field["name"].(string)
				if !slices.Contains(sources, name) {
					continue
				}
				found = true
				if i < len(columns) {
					values = append(values, list(columns[i])...)
				}
			}
		}
	}
	return values, found
}

// Answer counts the rows of every query in one panel's reply and reports the
// first error any of them carries. Frames answers for one refId; a panel asks
// with several and a failure in any of them is a broken panel.
func Answer(res map[string]any) (rows int, errText string) {
	results, _ := res["results"].(map[string]any)
	if len(results) == 0 {
		if m := res["message"]; truthy(m) {
			return 0, text(m)
		}
		return 0, ""
	}
	for _, ref := range slices.Sorted(maps.Keys(results)) {
		n, e := Frames(res, ref)
		rows += n
		if errText == "" && e != "" {
			errText = ref + ": " + e
		}
	}
	return rows, errText
}
