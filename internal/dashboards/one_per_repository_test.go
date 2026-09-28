package dashboards

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// overviewRepo is one repository of the account the Overview's star and fork
// sums are held against, as the series the stores key by their tags hold it.
type overviewRepo struct {
	owner, name string
	// live is its gh_repo series with archived false, or nil when it has
	// none. stale is its gh_repo series with archived true, which only a
	// backfill writes and nothing rewrites.
	live, stale *[2]float64
	// totals are its gh_repo_total series, by the archived tag they carry.
	totals map[string][2]float64
	// age is how long before the end of the range its series were last
	// written: zero for a repository the collector reads every sweep.
	age time.Duration
}

// overviewAccount is an account with everything the sums have to get right at
// once. Two repositories share a short name under two owners, as every
// account with an organization and a .github repository has, and the first
// was starred after the last totals sweep, which gh_repo has seen and
// gh_repo_total has not. One was archived while the collector ran, so it has
// a live series and an archived one side by side, and GitHub's count moved
// from 5 to 6 after the archive. One is set aside for being archived and was
// walked by a backfill, whose gh_repo row still says 7 where the account's
// own totals say 4.
var overviewAccount = []overviewRepo{
	{
		owner: "alice", name: ".github", live: &[2]float64{2, 1},
		totals: map[string][2]float64{"false": {1, 1}},
	},
	{
		owner: "acme", name: ".github", live: &[2]float64{10, 3},
		totals: map[string][2]float64{"false": {10, 3}},
	},
	{
		owner: "bob", name: "moved", live: &[2]float64{5, 2},
		totals: map[string][2]float64{"false": {5, 2}, "true": {6, 2}},
	},
	{
		owner: "bob", name: "old", stale: &[2]float64{7, 1},
		totals: map[string][2]float64{"true": {4, 1}},
	},
}

// overviewFields are the fields each series carries: the two the Overview
// sums, and the one "Every repository, ever" ranks by.
var overviewFields = []string{"stars", "forks", "commits"}

// overviewValue is one field of a series: stars and forks as the account
// holds them, and commits a hundred more than the stars, which is enough to
// tell the rows of one repository apart.
func overviewValue(field string, v [2]float64, i int) float64 {
	if field == "commits" {
		return 100 + v[0]
	}
	return v[i]
}

// overviewWant is what each sum has to be: every repository once, each at its
// newest count, which for the one archived while the collector ran is also
// its larger.
var overviewWant = map[string]float64{"Stars": 2 + 10 + 6 + 4, "Forks": 1 + 3 + 2 + 1}

// TestTheOverviewCountsEachRepositoryOnce evaluates the Overview's Stars and
// Forks, as the committed Prometheus and Graphite dashboards render them under
// All, against the series overviewAccount holds. Those are the two stores
// that key a series by its tags, so each of the repositories above is several
// series there and the query is what makes it one. The account has 22 stars.
// Summed as they come its gh_repo_total series hold 26; one series per full
// name of that table alone is 21, a star behind gh_repo, which the repo
// family writes at a moment of its own; one per short name, as the
// Graphite query did before the review of #78, makes the two .github
// repositories one and is 20; gh_repo alone, as 2.5.1 read it, is 24.
//
// The SQL stores and Elasticsearch keep the newest row of each repository,
// which only a store can evaluate; the containerised suite runs them, and
// here they are held to naming the repository by full_name.
func TestTheOverviewCountsEachRepositoryOnce(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		doc := store.Build(nil)
		allValue, _ := store.Variable["allValue"].(string)
		var names []string
		for _, r := range overviewAccount {
			names = append(names, r.name)
		}
		vars := grafana.Vars{Datasource: store.DS, Repos: names, AllValue: allValue}
		p := panelOf(t, doc, "Repositories", "stat")
		for _, raw := range p["targets"].([]any) {
			target := vars.Apply(raw.(map[string]any))
			switch store.Name {
			case "prometheus":
				checkOverviewSum(t, store.Name, target["legendFormat"], func() float64 {
					return evalProm(t, target["expr"].(string), overviewPromSeries())
				})
			case "graphite":
				expr := target["target"].(string)
				name := regexp.MustCompile(`, "([^"]+)"\)$`).FindStringSubmatch(expr)
				if name == nil {
					t.Fatalf("graphite target %s has no alias", expr)
				}
				checkOverviewSum(t, store.Name, name[1], func() float64 {
					return evalGraphite(t, expr, overviewGraphiteSeries())
				})
			default:
				text := asJSON(t, target)
				if !strings.Contains(text, "stars") {
					continue
				}
				if !strings.Contains(text, "PARTITION BY full_name") && !strings.Contains(text, `"full_name.keyword"`) {
					t.Errorf("%s: the Overview's stars take one row per something other than "+
						"the full name, and two owners can share a repository's name:\n%s", store.Name, text)
				}
			}
		}
	}
}

// checkOverviewSum holds one named value of the tile to overviewWant.
func checkOverviewSum(t *testing.T, store string, name any, got func() float64) {
	t.Helper()
	want, isSum := overviewWant[fmt.Sprint(name)]
	if !isSum {
		return // the repository count, which is the account's own row
	}
	if g := got(); g != want {
		t.Errorf("%s: the Overview's %v are %v over an account that has %v", store, name, g, want)
	}
}

// overviewPromSeries is overviewAccount as the exporter's gauges: every label
// promRules keeps for the two measurements, and the field in the name.
func overviewPromSeries() []promSample {
	var out []promSample
	add := func(m string, r overviewRepo, archived string, v [2]float64, extra map[string]string) {
		for i, field := range overviewFields {
			labels := map[string]string{
				"__name__": "github_" + m + "_" + field, "owner": r.owner, "repo": r.name,
				"full_name": r.owner + "/" + r.name, "archived": archived, "fork": "false",
				"visibility": "public",
			}
			maps.Copy(labels, extra)
			out = append(out, promSample{labels, overviewValue(field, v, i)})
		}
	}
	repoOnly := map[string]string{"language": "Go", "license": "MIT", "default_branch": "main"}
	for _, r := range overviewAccount {
		if r.live != nil {
			add("repo", r, "false", *r.live, repoOnly)
		}
		if r.stale != nil {
			add("repo", r, "true", *r.stale, repoOnly)
		}
		for archived, v := range r.totals {
			add("repo_total", r, archived, v, nil)
		}
	}
	return out
}

// overviewGone is an archived repository the collector no longer writes: an
// archived fork that a backfill under an earlier configuration read nine days
// before the end of the range, as one read jmrplens/portainer-mcp-enhanced on
// 2026-09-18, and that nothing has read since. Its one row is still in the
// store, and it is not the account's.
var overviewGone = overviewRepo{
	owner: "bob", name: "gone", totals: map[string][2]float64{"true": {8, 3}},
	age: 9 * 24 * time.Hour,
}

// overviewGraphiteSeries is overviewAccount as the Graphite sink writes it:
// one path per series, a node per tag in the order tags.go gives, each value
// made a node the way the sink makes it.
func overviewGraphiteSeries() []grSeries {
	return graphiteSeriesOf(overviewAccount)
}

// graphiteSeriesOf is overviewGraphiteSeries for any list of repositories.
func graphiteSeriesOf(repos []overviewRepo) []grSeries {
	node := regexp.MustCompile(`[^A-Za-z0-9_:-]`)
	var out []grSeries
	add := func(m string, r overviewRepo, archived string, v [2]float64) {
		values := map[string]string{
			"archived": archived, "default_branch": "main", "fork": "false",
			"full_name": r.owner + "/" + r.name, "language": "Go", "license": "MIT",
			"owner": r.owner, "repo": r.name, "visibility": "public",
		}
		parts := []string{"github", strings.TrimPrefix(m, "gh_")}
		for _, tag := range tagsOf(m) {
			parts = append(parts, node.ReplaceAllString(values[tag], "_"))
		}
		for i, field := range overviewFields {
			out = append(out, grSeries{
				name:  strings.Join(append(slices.Clone(parts), field), "."),
				value: overviewValue(field, v, i), age: r.age,
			})
		}
	}
	for _, r := range repos {
		if r.live != nil {
			add("gh_repo", r, "false", *r.live)
		}
		if r.stale != nil {
			add("gh_repo", r, "true", *r.stale)
		}
		for archived, v := range r.totals {
			add("gh_repo_total", r, archived, v)
		}
	}
	return out
}

// TestTheOverviewEvaluatorsAgreeWithTheSumsTheyReplace keeps the two small
// evaluators honest on the queries the review of #78 found wrong, so that the
// test above failing is about the dashboard and not about them.
func TestTheOverviewEvaluatorsAgreeWithTheSumsTheyReplace(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]float64{
		`sum(github_repo_total_stars{repo=~".*"})`:                                   26,
		`sum(max by (full_name) (github_repo_total_stars{repo=~".*"}))`:              21,
		`sum(github_repo_stars{repo=~".*"})`:                                         2 + 10 + 5 + 7,
		`sum(max by (repo) (github_repo_total_stars{repo=~"moved|old"}))`:            10,
		`sum(github_repo_stars{archived="false"} or github_repo_total_stars{})`:      2 + 10 + 5 + 1 + 10 + 5 + 6 + 4,
		`max by (full_name) (github_repo_stars{full_name="bob/moved",fork!="true"})`: 5,
	} {
		if got := evalProm(t, expr, overviewPromSeries()); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	for expr, want := range map[string]float64{
		`sumSeries(keepLastValue(github.repo_total.*.*.*.*.*.*.stars))`:                        26,
		`sumSeries(groupByNode(keepLastValue(github.repo_total.*.*.*.*.*.*.stars), 6, "max"))`: 20,
		`sumSeries(groupByNode(keepLastValue(github.repo_total.*.*.*.*.*.*.stars), 4, "max"))`: 21,
		`alias(sumSeries(github.repo_total.true.*.*.*.moved.*.stars), "x")`:                    6,
	} {
		if got := evalGraphite(t, expr, overviewGraphiteSeries()); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

// ── A PromQL evaluator for the shapes these panels use ──────────────────────

// promSample is one series at one instant.
type promSample struct {
	labels map[string]string
	value  float64
}

// evalProm evaluates an instant query made of selectors, `or` and `unless`
// with an optional `on`, sum, max or count with an optional `by`, a filter by
// `>` a number and a constant vector(), which is every form the panels held by
// it take, and answers the one value it has to come to.
func evalProm(t *testing.T, expr string, series []promSample) float64 {
	t.Helper()
	out := evalPromSeries(t, expr, series)
	if len(out) != 1 {
		t.Fatalf("%s answered %d series, want one sum", expr, len(out))
	}
	return out[0].value
}

// evalPromSeries is evalProm for a query that answers a series per row.
func evalPromSeries(t *testing.T, expr string, series []promSample) []promSample {
	t.Helper()
	p := &exprParser{t: t, s: expr}
	out := p.promOr(series)
	if p.skip(); p.i != len(p.s) {
		t.Fatalf("%s: unparsed from %q", expr, p.s[p.i:])
	}
	return out
}

// exprParser is a cursor over one expression, shared by both evaluators.
type exprParser struct {
	t *testing.T
	s string
	i int
}

func (p *exprParser) skip() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *exprParser) eat(tok string) bool {
	p.skip()
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

func (p *exprParser) want(tok string) {
	p.t.Helper()
	if !p.eat(tok) {
		p.t.Fatalf("%s: want %q at %q", p.s, tok, p.s[p.i:])
	}
}

// word is an identifier, a Graphite path or a number: everything up to the
// next delimiter.
func (p *exprParser) word() string {
	p.skip()
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(`(){},"= !~|`, rune(p.s[p.i])) {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *exprParser) quoted() string {
	p.t.Helper()
	p.want(`"`)
	end := strings.IndexByte(p.s[p.i:], '"')
	if end < 0 {
		p.t.Fatalf("%s: unterminated string", p.s)
	}
	out := p.s[p.i : p.i+end]
	p.i += end + 1
	return out
}

// promOr is a chain of `or` and `unless`, which bind alike and from the left.
// Both match on every label but the name, or on the labels `on` lists.
func (p *exprParser) promOr(series []promSample) []promSample {
	left := p.promAbove(series)
	for {
		var unless bool
		switch {
		case p.eat("or "):
		case p.eat("unless "):
			unless = true
		default:
			return left
		}
		var on []string
		if p.eat("on") {
			on = p.labelList()
		}
		right := p.promAbove(series)
		inRight := map[string]bool{}
		for _, s := range right {
			inRight[signature(s.labels, on...)] = true
		}
		if unless {
			var kept []promSample
			for _, s := range left {
				if !inRight[signature(s.labels, on...)] {
					kept = append(kept, s)
				}
			}
			left = kept
			continue
		}
		inLeft := map[string]bool{}
		for _, s := range left {
			inLeft[signature(s.labels, on...)] = true
		}
		for _, s := range right {
			if !inLeft[signature(s.labels, on...)] {
				left = append(left, s)
			}
		}
	}
}

// labelList is a parenthesised list of label names.
func (p *exprParser) labelList() []string {
	p.want("(")
	var out []string
	for !p.eat(")") {
		out = append(out, p.word())
		p.eat(",")
	}
	return out
}

// signature is a series' identity for vector matching: the labels named, or
// every label but the metric name when none are.
func signature(labels map[string]string, on ...string) string {
	keys := on
	if len(on) == 0 {
		keys = slices.Sorted(maps.Keys(labels))
	}
	var b strings.Builder
	for _, k := range keys {
		if k != "__name__" {
			fmt.Fprintf(&b, "%s=%q,", k, labels[k])
		}
	}
	return b.String()
}

// promAbove is a term and, when a `>` follows it, only the series whose
// value is above the number after it, which binds tighter than `or`.
func (p *exprParser) promAbove(series []promSample) []promSample {
	p.t.Helper()
	out := p.promTerm(series)
	if !p.eat("> ") {
		return out
	}
	bound, err := strconv.ParseFloat(p.word(), 64)
	if err != nil {
		p.t.Fatalf("%s: a comparison with no number: %v", p.s, err)
	}
	var kept []promSample
	for _, s := range out {
		if s.value > bound {
			kept = append(kept, s)
		}
	}
	return kept
}

func (p *exprParser) promTerm(series []promSample) []promSample {
	p.skip()
	if p.eat("(") {
		out := p.promOr(series)
		p.want(")")
		return out
	}
	name := p.word()
	if name == "vector" {
		p.want("(")
		v, err := strconv.ParseFloat(p.word(), 64)
		if err != nil {
			p.t.Fatalf("%s: a vector of no number: %v", p.s, err)
		}
		p.want(")")
		return []promSample{{labels: map[string]string{}, value: v}}
	}
	if name == "sum" || name == "max" || name == "count" {
		var by []string
		if p.eat("by") {
			by = p.labelList()
		}
		p.want("(")
		inner := p.promOr(series)
		p.want(")")
		return aggregate(name, by, inner)
	}
	matchers := map[string]func(string) bool{"__name__": func(v string) bool { return v == name }}
	if p.eat("{") {
		for !p.eat("}") {
			label := p.word()
			var match func(string) bool
			switch {
			case p.eat("=~"):
				re := regexp.MustCompile("^(?:" + p.quoted() + ")$")
				match = re.MatchString
			case p.eat("!="):
				v := p.quoted()
				match = func(s string) bool { return s != v }
			default:
				p.want("=")
				v := p.quoted()
				match = func(s string) bool { return s == v }
			}
			previous := matchers[label]
			matchers[label] = func(s string) bool { return match(s) && (previous == nil || previous(s)) }
			p.eat(",")
		}
	}
	var out []promSample
	for _, s := range series {
		if everyMatch(matchers, s.labels) {
			out = append(out, s)
		}
	}
	return out
}

func everyMatch(matchers map[string]func(string) bool, labels map[string]string) bool {
	for label, match := range matchers {
		if !match(labels[label]) {
			return false
		}
	}
	return true
}

func aggregate(how string, by []string, in []promSample) []promSample {
	groups := map[string]*promSample{}
	var order []string
	for _, s := range in {
		labels := map[string]string{}
		for _, l := range by {
			labels[l] = s.labels[l]
		}
		key := signature(labels)
		value := s.value
		if how == "count" {
			value = 1
		}
		g, seen := groups[key]
		if !seen {
			groups[key] = &promSample{labels, value}
			order = append(order, key)
			continue
		}
		switch how {
		case "sum", "count":
			g.value += value
		case "max":
			g.value = max(g.value, value)
		}
	}
	out := make([]promSample, len(order))
	for i, key := range order {
		out[i] = *groups[key]
	}
	return out
}

// ── A Graphite evaluator for the functions these panels use ──────────────────

// grSeries is one Graphite series reduced to the value keepLastValue leaves at
// the end of the range, which is the value a stat of it draws, and how long
// before the end of the range that value was written, which is what timeSlice
// reads.
type grSeries struct {
	name  string
	value float64
	age   time.Duration
}

// evalGraphite evaluates a target made of paths and the functions the two
// panels here use, and answers the one value it has to come to.
func evalGraphite(t *testing.T, expr string, series []grSeries) float64 {
	t.Helper()
	out := evalGraphiteSeries(t, expr, series)
	if len(out) != 1 {
		t.Fatalf("%s answered %d series, want one sum", expr, len(out))
	}
	return out[0].value
}

func (p *exprParser) graphite(series []grSeries) []grSeries {
	p.t.Helper()
	word := p.word()
	if !p.eat("(") {
		return globbed(word, series)
	}
	var lists [][]grSeries
	var args []string
	for !p.eat(")") {
		p.skip()
		switch {
		case strings.HasPrefix(p.s[p.i:], `"`):
			args = append(args, p.quoted())
		case strings.ContainsAny(p.s[p.i:p.i+1], "0123456789"):
			args = append(args, p.word())
		default:
			lists = append(lists, p.graphite(series))
		}
		p.eat(",")
	}
	if out, known := graphiteOfLists(word, lists, args); known {
		return out
	}
	var in []grSeries
	for _, l := range lists {
		in = append(in, l...)
	}
	switch word {
	case "keepLastValue", "group", "removeEmptySeries", "sortByMaxima":
		return in
	case "removeBelowValue":
		return atLeast(in, args[0])
	case "timeSlice":
		return p.sliced(in, args)
	case "alias":
		out := slices.Clone(in)
		for i := range out {
			out[i].name = args[0]
		}
		return out
	case "aliasByNode":
		return aliasedByNode(in, args)
	case "sumSeries":
		total := 0.0
		for _, s := range in {
			total += s.value
		}
		return []grSeries{{name: "sumSeries", value: total, age: newestOf(in)}}
	case "groupByNode":
		// groupByNode(series, node, how).
		return grouped(in, args[len(args)-1], args[:len(args)-1])
	case "groupByNodes":
		// groupByNodes(series, how, node...).
		return grouped(in, args[0], args[1:])
	}
	p.t.Fatalf("%s: no evaluator for %s()", p.s, word)
	return nil
}

// graphiteOfLists evaluates the functions that read their arguments as
// separate lists, or as none, rather than as one list of series, and reports
// whether word is one of them.
func graphiteOfLists(word string, lists [][]grSeries, args []string) ([]grSeries, bool) {
	switch word {
	case "countSeries":
		// graphite-web 1.1.10 answers no series at all for a list that is
		// empty, and not the 0 its own countSeries() of no argument draws.
		var in []grSeries
		for _, l := range lists {
			in = append(in, l...)
		}
		if len(in) == 0 {
			return nil, true
		}
		return []grSeries{{name: "countSeries", value: float64(len(in)), age: newestOf(in)}}, true
	case "fallbackSeries":
		if len(lists[0]) > 0 {
			return lists[0], true
		}
		return lists[1], true
	case "constantLine":
		n, _ := strconv.ParseFloat(args[0], 64)
		return []grSeries{{name: "constantLine", value: n}}, true
	}
	return nil, false
}

// aliasedByNode is aliasByNode(series, node...): each series named by the
// nodes of its path the arguments pick, joined by dots.
func aliasedByNode(in []grSeries, args []string) []grSeries {
	out := slices.Clone(in)
	for i := range out {
		nodes := strings.Split(out[i].name, ".")
		var picked []string
		for _, a := range args {
			n, _ := strconv.Atoi(a)
			picked = append(picked, nodes[n])
		}
		out[i].name = strings.Join(picked, ".")
	}
	return out
}

// atLeast is removeBelowValue(series, n), which empties every point under n:
// a series reduced to its last point has nothing left when that one is.
func atLeast(in []grSeries, bound string) []grSeries {
	n, _ := strconv.ParseFloat(bound, 64)
	var kept []grSeries
	for _, s := range in {
		if s.value >= n {
			kept = append(kept, s)
		}
	}
	return kept
}

// sliced is timeSlice(series, "-Nd"), which nulls every point before N days
// ago, so a series last written before then has nothing left to carry.
func (p *exprParser) sliced(in []grSeries, args []string) []grSeries {
	p.t.Helper()
	if len(args) != 1 {
		p.t.Fatalf("%s: timeSlice(%v) is not a start N days back", p.s, args)
	}
	days, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(args[0], "-"), "d"))
	if err != nil {
		p.t.Fatalf("%s: timeSlice(%v) is not a start N days back", p.s, args)
	}
	var kept []grSeries
	for _, s := range in {
		if s.age <= time.Duration(days)*24*time.Hour {
			kept = append(kept, s)
		}
	}
	return kept
}

// grouped is groupByNode and groupByNodes: the series that share the nodes
// named become one, reduced by how, and named by those nodes.
func grouped(in []grSeries, how string, nodes []string) []grSeries {
	samples := make([]promSample, len(in))
	members := map[string][]grSeries{}
	for i, s := range in {
		parts := strings.Split(s.name, ".")
		var key []string
		for _, a := range nodes {
			n, _ := strconv.Atoi(a)
			key = append(key, parts[n])
		}
		joined := strings.Join(key, ".")
		samples[i] = promSample{map[string]string{"key": joined}, s.value}
		members[joined] = append(members[joined], s)
	}
	var out []grSeries
	for _, g := range aggregate(how, []string{"key"}, samples) {
		key := g.labels["key"]
		out = append(out, grSeries{name: key, value: g.value, age: newestOf(members[key])})
	}
	return out
}

// newestOf is when the newest of several series was last written, which is
// when a series made of them was.
func newestOf(in []grSeries) time.Duration {
	if len(in) == 0 {
		return 0
	}
	newest := in[0].age
	for _, s := range in[1:] {
		newest = min(newest, s.age)
	}
	return newest
}

// globbed is every series a path matches, node by node.
func globbed(pattern string, series []grSeries) []grSeries {
	want := strings.Split(pattern, ".")
	var out []grSeries
	for _, s := range series {
		nodes := strings.Split(s.name, ".")
		if len(nodes) != len(want) {
			continue
		}
		matched := true
		for i := range nodes {
			if ok, _ := path.Match(want[i], nodes[i]); !ok {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, s)
		}
	}
	return out
}

// TestEveryRepositoryEverListsARepositoryArchivedInsideTheRangeOnce holds
// "Every repository, ever" to a row per repository over overviewAccount, whose
// bob/moved was archived while the collector ran and so has a live row and an
// archived one inside the range. Since a sweep writes the archived row of a
// repository the default filter sets aside, that is every archive, and a
// table that listed a row per tag set showed it twice, once as live with the
// counts it had before. The SQL stores take its newest row, which is the
// archived one; the others are held to the same answer here: one row for it,
// archived, with the newer counts.
func TestEveryRepositoryEverListsARepositoryArchivedInsideTheRangeOnce(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		doc := store.Build(nil)
		allValue, _ := store.Variable["allValue"].(string)
		vars := grafana.Vars{Datasource: store.DS, Repos: []string{"hello-world"}, AllValue: allValue}
		p := panelOf(t, doc, "Every repository, ever", "table")
		for _, raw := range p["targets"].([]any) {
			checkEveryRepositoryTarget(t, store.Name, vars.Apply(raw.(map[string]any)))
		}
	}
}

// checkEveryRepositoryTarget holds one target of "Every repository, ever",
// rendered under All, to a row per repository: evaluated over overviewAccount
// where the store keys a series by its tags, and read for the one setting
// that decides it where it is an aggregation or a statement.
func checkEveryRepositoryTarget(t *testing.T, store string, target map[string]any) {
	t.Helper()
	switch store {
	case "prometheus":
		expr := target["expr"].(string)
		if strings.Contains(expr, "_commits") || strings.Contains(expr, "_stars") {
			checkOneRowEach(t, store, expr, promRows(evalPromSeries(t, expr, overviewPromSeries())))
		}
	case "graphite":
		expr := target["target"].(string)
		checkOneRowEach(t, store, expr, graphiteRows(evalGraphiteSeries(t, expr, overviewGraphiteSeries())))
	case "elasticsearch":
		for _, raw := range target["bucketAggs"].([]any) {
			bucket := raw.(map[string]any)
			if bucket["field"] != "archived.keyword" {
				continue
			}
			settings := bucket["settings"].(map[string]any)
			if settings["size"] != "1" || settings["orderBy"] != "_key" || settings["order"] != "desc" {
				t.Errorf("elasticsearch: the archived bucket of Every repository, ever is %v, "+
					"so a repository archived inside the range is a row per value of it; "+
					"want the one value that sorts last, true, as MAX(archived) is", settings)
			}
		}
	default:
		if sql := target["rawSql"].(string); !newestRowEach.MatchString(sql) {
			t.Errorf("%s: Every repository, ever does not read the newest row of each "+
				"repository, so one archived inside the range is not one row flagged "+
				"archived:\n%s", store, sql)
		}
	}
}

// TestEveryRepositoryEverReadsEachRepositorysNewestRow is what checking 2.6.0
// against GitHub panel by panel found in the Lifetime table: it took MAX() of
// every column over the range, and stars, branches, tags, releases and open
// issues go down, so it drew the range's peak as the count. Measured on
// 2026-09-27 over thirty days, jmrplens/FFT2octave read 4 stars, from a
// backfill's row of 2026-09-18, where GitHub and its newest row said 3. Each
// store is held to the newest row of each repository, one per full name, in
// its own terms: the SQL stores number the rows of each full name newest first,
// keep the first and take no column's MAX(); Elasticsearch buckets by the full
// name and reads the newest document of each; Prometheus asks instant queries,
// which answer each series' current value. Graphite draws commits alone, which
// only grow, as each series' last value, and groups by the full name, which
// the test above evaluates.
func TestEveryRepositoryEverReadsEachRepositorysNewestRow(t *testing.T) {
	t.Parallel()
	peak := regexp.MustCompile(`\bMAX\((commits|pulls_merged|issues|releases|stars|branches|tags)\)`)
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Every repository, ever", "table")
		for _, raw := range p["targets"].([]any) {
			target := raw.(map[string]any)
			switch store.Name {
			case "influxdb", "postgres":
				sql, _ := target["rawSql"].(string)
				if m := peak.FindString(sql); m != "" || !newestRowEach.MatchString(sql) {
					t.Errorf("%s: Every repository, ever does not read each repository's newest "+
						"row (takes %q):\n%s", store.Name, m, sql)
				}
			case "elasticsearch":
				checkNewestPerFullName(t, target)
			case "prometheus":
				if target["instant"] != true {
					t.Errorf("prometheus: %v is not an instant query, so it is not the current value", target["expr"])
				}
			}
		}
	}
}

// checkNewestPerFullName holds the Elasticsearch target of Every repository,
// ever to a bucket per full name, so that alice/.github and acme/.github are
// two rows, and to the newest document of each.
func checkNewestPerFullName(t *testing.T, target map[string]any) {
	t.Helper()
	buckets, _ := target["bucketAggs"].([]any)
	if len(buckets) == 0 || buckets[0].(map[string]any)["field"] != "full_name.keyword" {
		t.Errorf("elasticsearch: Every repository, ever buckets first by %v, want one row per "+
			"full name", bucketFieldsOf(buckets))
	}
	metrics, _ := target["metrics"].([]any)
	for _, raw := range metrics {
		m := raw.(map[string]any)
		settings, _ := m["settings"].(map[string]any)
		if m["type"] != "top_metrics" || settings["order"] != "desc" || settings["orderBy"] != "@timestamp" {
			t.Errorf("elasticsearch: Every repository, ever reads %v, want the newest document", m)
		}
	}
}

// bucketFieldsOf names the fields of a list of bucket aggregations, for a message.
func bucketFieldsOf(buckets []any) []any {
	out := make([]any, len(buckets))
	for i, raw := range buckets {
		b, _ := raw.(map[string]any)
		out[i] = b["field"]
	}
	return out
}

// newestRowEach is a statement that keeps the newest row of each repository,
// by its full name, and draws the Archived column from that row.
var newestRowEach = regexp.MustCompile(`(?s)^SELECT .*\barchived AS "Archived".*` +
	`ROW_NUMBER\(\) OVER \(PARTITION BY full_name ORDER BY time DESC\) AS rn.*\) x WHERE rn = 1\b`)

// overviewRow is one row of a table as the checks below read it: the
// repository it names, whether it says archived where the store can say, and
// the value.
type overviewRow struct {
	name, archived string
	value          float64
}

func promRows(samples []promSample) []overviewRow {
	out := make([]overviewRow, len(samples))
	for i, s := range samples {
		out[i] = overviewRow{s.labels["full_name"], s.labels["archived"], s.value}
	}
	return out
}

// graphiteRows reads the rows Graphite draws, which are named by the short
// name alone and say nothing of the archive.
func graphiteRows(series []grSeries) []overviewRow {
	out := make([]overviewRow, len(series))
	for i, s := range series {
		out[i] = overviewRow{s.name, "", s.value}
	}
	return out
}

// checkOneRowEach holds one query's rows to overviewAccount: four rows, the
// one for bob/moved archived and at the counts from after its archive.
func checkOneRowEach(t *testing.T, store, expr string, rows []overviewRow) {
	t.Helper()
	if len(rows) != len(overviewAccount) {
		t.Errorf("%s: %s draws %d rows over %d repositories: %v", store, expr, len(rows), len(overviewAccount), rows)
		return
	}
	for _, r := range rows {
		if r.name != "bob/moved" && r.name != "moved" {
			continue
		}
		if r.archived != "" && r.archived != "true" {
			t.Errorf("%s: %s lists bob/moved as archived=%s, where it was archived inside the range", store, expr, r.archived)
		}
		if r.value != 106 && r.value != 6 {
			t.Errorf("%s: %s lists bob/moved at %v, the counts from before its archive", store, expr, r.value)
		}
	}
}

// evalGraphiteSeries is evalGraphite for a target that draws a series per row.
func evalGraphiteSeries(t *testing.T, expr string, series []grSeries) []grSeries {
	t.Helper()
	p := &exprParser{t: t, s: expr}
	out := p.graphite(series)
	if p.skip(); p.i != len(p.s) {
		t.Fatalf("%s: unparsed from %q", expr, p.s[p.i:])
	}
	return out
}
