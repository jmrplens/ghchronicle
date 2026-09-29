package dashboards

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// twoOwnersReading is one reading of a measurement the newest-value sums add
// up: its field, the tags beside the repository's, and what each of the two
// owners' repositories holds.
type twoOwnersReading struct {
	measurement, field string
	tags               map[string]string
	alice, acme        float64
}

// twoOwnersReadings are the five measurements behind "Open alerts", the
// artifact and cache bytes of "Runs in range" and "Downloads", read for
// alice/dotfiles and acme/dotfiles, two owners' repositories of one name, as
// an account and an organization it belongs to each keep a .github.
var twoOwnersReadings = []twoOwnersReading{
	{"gh_dependabot_alert", "open", map[string]string{"severity": "high", "ecosystem": "npm"}, 2, 5},
	{"gh_code_scanning_alert", "open", map[string]string{"severity": "high", "tool": "CodeQL"}, 1, 3},
	{"gh_artifact_total", "live_bytes", nil, 100, 200},
	{"gh_actions_cache", "size_bytes", nil, 10, 20},
	{"gh_release", "downloads", map[string]string{"tag": "v1.0.0", "draft": "false", "prerelease": "false"}, 40, 60},
}

// twoOwnersPoints is each reading as the collectors write it, acme's an hour
// after alice's, so that a store keying the series by the short name keeps
// acme's reading and drops alice's.
func twoOwnersPoints() []sink.Point {
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	var out []sink.Point
	for _, r := range twoOwnersReadings {
		for i, owner := range []string{"alice", "acme"} {
			tags := map[string]string{"owner": owner, "repo": "dotfiles", "full_name": owner + "/dotfiles"}
			maps.Copy(tags, r.tags)
			value := []float64{r.alice, r.acme}[i]
			out = append(out, sink.Point{
				Measurement: r.measurement, Tags: tags,
				Fields: map[string]any{r.field: value}, Time: at.Add(time.Duration(i) * time.Hour),
			})
		}
	}
	return out
}

// twoOwnersPromSeries is what the exporter serves for those points: the
// gauges the reducer makes of them, named the way the exporter names them.
func twoOwnersPromSeries() []promSample {
	var out []promSample
	for _, g := range sink.Summarize(twoOwnersPoints()) {
		for field, raw := range g.Fields {
			v, ok := raw.(float64)
			if !ok {
				continue
			}
			labels := map[string]string{
				"__name__": "github_" + strings.TrimPrefix(g.Measurement, "gh_") + "_" + field,
			}
			maps.Copy(labels, g.Tags)
			out = append(out, promSample{labels, v})
		}
	}
	return out
}

// twoOwnersGraphiteSeries is the same points as the Graphite sink writes
// them: a node per tag in the order of the tag's name, each value made a node
// the way the sink makes one.
func twoOwnersGraphiteSeries() []grSeries {
	node := regexp.MustCompile(`[^A-Za-z0-9_:-]`)
	var out []grSeries
	for _, p := range twoOwnersPoints() {
		parts := []string{"github", strings.TrimPrefix(p.Measurement, "gh_")}
		for _, k := range slices.Sorted(maps.Keys(p.Tags)) {
			parts = append(parts, node.ReplaceAllString(p.Tags[k], "_"))
		}
		for field, raw := range p.Fields {
			v, _ := raw.(float64)
			out = append(out, grSeries{name: strings.Join(append(parts, field), "."), value: v})
		}
	}
	return out
}

// twoOwnersSum is one figure held to the two repositories: the panel and its
// query, and what the figure is with both counted. esOwn is why the
// Elasticsearch query is not held to it, where its description says so.
type twoOwnersSum struct {
	title, ref string
	want       float64
	esOwn      string
}

// twoOwnersSums are the figures of the three tiles #97 names and of the two
// breakdowns beside "Open alerts", whose bars add up to its Dependabot count.
var twoOwnersSums = []twoOwnersSum{
	{securityOpenAlerts, "A", 2 + 5, ""},
	{securityOpenAlerts, "B", 1 + 3, ""},
	{"Runs in range", "F", 100 + 200, ""},
	{"Runs in range", "G", 10 + 20, ""},
	{"Downloads", "A", 40 + 60, ""},
	{"Downloads", "B", 2, "a cardinality of the tag cannot be filtered on the newest value, " +
		"so it counts distinct tags, downloaded or not, as the panel says"},
	{"Alerts by severity", "A", 2 + 5, ""},
	{"Alerts by ecosystem", "A", 2 + 5, ""},
}

// TestTheNewestValueSumsCountTwoOwnersRepositoriesOfOneNameAsTwo is what the
// audit of 2.6.1 left for #97 in the sums of each repository's newest reading.
// esLatestSum and latestSumSQL took the newest reading per short repository
// name, so alice/dotfiles and acme/dotfiles were one series and the newer of
// the two readings stood for both: "Open alerts" read 5 where the two hold 7,
// and so did the artifact and cache bytes of "Runs in range" and the total of
// "Downloads". The exporter kept the same five measurements by `repo` alone,
// so Prometheus had one series for the two before any query ran. The
// Overview and the card key by full_name since 2.6.0 and 2.6.1.
//
// Prometheus and Graphite are evaluated over the two repositories as each
// store holds them: the gauges the exporter's own reducer makes of the
// points, and the paths the Graphite sink writes. Graphite keeps the full name
// in every path, so its sums never merged the two; it is held here all the
// same. The SQL stores and Elasticsearch keep the newest row of each series,
// which only a store can evaluate, so they are held to taking a series by the
// full name, and to the repository filter each query already had.
func TestTheNewestValueSumsCountTwoOwnersRepositoriesOfOneNameAsTwo(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		allValue, _ := store.Variable["allValue"].(string)
		vars := grafana.Vars{Datasource: store.DS, Repos: []string{"dotfiles"}, AllValue: allValue}
		panels := rendered(t, store.Name)
		for _, sum := range twoOwnersSums {
			target := targetOf(t, mustPanel(t, panels, sum.title), sum.ref)
			id := fmt.Sprintf("%s: %q query %s", store.Name, sum.title, sum.ref)
			switch store.Name {
			case "prometheus":
				expr, _ := vars.Apply(target)["expr"].(string)
				checkTwoOwnersSum(t, id, expr, sum.want, sumOf(evalPromSeries(t, expr, twoOwnersPromSeries())))
			case "graphite":
				expr, _ := vars.Apply(target)["target"].(string)
				var got float64
				for _, s := range evalGraphiteSeries(t, expr, twoOwnersGraphiteSeries()) {
					got += s.value
				}
				checkTwoOwnersSum(t, id, expr, sum.want, got)
			case "influxdb", "postgres":
				sql, _ := target["rawSql"].(string)
				checkBySQLFullName(t, id, sql)
			case "elasticsearch":
				if sum.esOwn == "" {
					checkByESFullName(t, id, target)
				}
			}
		}
	}
}

// targetOf is the query of a panel with the given refId.
func targetOf(t *testing.T, p map[string]any, ref string) map[string]any {
	t.Helper()
	for _, raw := range targetList(p) {
		target, _ := raw.(map[string]any)
		if target["refId"] == ref {
			return target
		}
	}
	t.Fatalf("%q has no query %s", p["title"], ref)
	return nil
}

func sumOf(samples []promSample) float64 {
	var out float64
	for _, s := range samples {
		out += s.value
	}
	return out
}

func checkTwoOwnersSum(t *testing.T, id, expr string, want, got float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s reads %v over alice/dotfiles and acme/dotfiles, which hold %v: %s", id, got, want, expr)
	}
}

// bySQLFullName is a window that takes the newest row of each series with the
// full name first among what it partitions by, dated per bucket or not.
var bySQLFullName = regexp.MustCompile(`ROW_NUMBER\(\) OVER \(PARTITION BY (\$__dateBin\(time\), )?full_name[,)\s]`)

func checkBySQLFullName(t *testing.T, id, sql string) {
	t.Helper()
	if !bySQLFullName.MatchString(sql) {
		t.Errorf("%s takes the newest row of each series by something other than the full name, "+
			"and two owners' repositories can share a name:\n%s", id, sql)
	}
	if !strings.Contains(sql, "repo IN (${repo:") {
		t.Errorf("%s no longer narrows to the picked repositories:\n%s", id, sql)
	}
}

func checkByESFullName(t *testing.T, id string, target map[string]any) {
	t.Helper()
	fields := bucketFieldsOf(asList(target["bucketAggs"]))
	if !slices.Contains(fields, any("full_name.keyword")) || slices.Contains(fields, any(inventoryRepoTerm)) {
		t.Errorf("%s buckets a repository by %v, want its full name: two owners' repositories "+
			"can share the short one", id, fields)
	}
	if query, _ := target["query"].(string); !strings.Contains(query, ESF) {
		t.Errorf("%s no longer narrows to the picked repositories: %s", id, query)
	}
}

// TestNoQueryTellsARepositoryByItsShortNameAlone is the rest of what #97
// found, in every panel of every store: a query that partitions, groups,
// joins, deduplicates or buckets by `repo` alone makes alice/dotfiles and
// acme/dotfiles one repository, as an account and an organization it belongs
// to each keep a .github. "Open the longest" listed alice/x#5 and acme/x#5 as
// one row, the newest reading of either stood for both in every table of
// snapshots, and every chart per repository drew the two as one series.
// Wherever a measurement carries the full name, and every one that names a
// repository does, a repository is keyed by it; the short name can still be
// what a row shows, and the picker still narrows by it.
//
// Each store is read in its own terms. The SQL stores for every window
// partition, grouping, join, DISTINCT and count of distinct values, a
// grouping by a column number read through the select list it names;
// Elasticsearch for a terms bucket on the short name that no bucket on the
// full name encloses, and a count of distinct short names; Graphite for a
// groupByNode or groupByNodes over the short name's node and not the full
// name's, following the nodes through every aliasByNode before it, and for a
// row named by the nodes of a sumSeries, which has already made every
// repository one series; and Prometheus for every `by` and `on` list, and for
// the exporter's own series under a query that lists repositories, since a
// rule that keeps `repo` alone has merged the two before any query runs.
func TestNoQueryTellsARepositoryByItsShortNameAlone(t *testing.T) {
	t.Parallel()
	fullNamed := promGaugesWithFullNames()
	for _, store := range AllStores() {
		held := 0
		for _, p := range renderedPanels(t, store.Name) {
			for _, raw := range targetList(p) {
				target, _ := raw.(map[string]any)
				var found []string
				switch store.Name {
				case "influxdb", "postgres":
					sql, _ := target["rawSql"].(string)
					found = sqlShortNameKeys(sql)
					held += strings.Count(sql, "full_name")
				case "elasticsearch":
					found = esShortNameKeys(asList(target["bucketAggs"]), asList(target["metrics"]))
					held += strings.Count(asJSON(t, target["bucketAggs"]), "full_name.keyword")
				case "graphite":
					expr, _ := target["target"].(string)
					found = graphiteShortNameKeys(t, expr)
					held += strings.Count(expr, "github.")
				case "prometheus":
					expr, _ := target["expr"].(string)
					found = promShortNameKeys(expr, fullNamed)
					held += strings.Count(expr, "full_name")
				}
				for _, f := range found {
					t.Errorf("%s: %q query %v %s, and two owners' repositories can share "+
						"a short name", store.Name, p["title"], target["refId"], f)
				}
			}
		}
		if held == 0 {
			t.Errorf("%s: no query names a full name or a path, so this checked nothing", store.Name)
		}
	}
}

// TestTheShortNameReadersFindWhatTheyLookFor keeps the four readers above
// honest on statements whose answer is known, so that the test above passing
// is about the dashboards and not about readers that find nothing.
func TestTheShortNameReadersFindWhatTheyLookFor(t *testing.T) {
	t.Parallel()
	for sql, want := range map[string]int{
		`SELECT repo AS "Repository", COUNT(*) AS n FROM t WHERE x GROUP BY 1 ORDER BY 2`:                      1,
		`SELECT repo AS "Repository", COUNT(*) AS n FROM t WHERE x GROUP BY full_name, repo ORDER BY 2`:        0,
		`SELECT * FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, number ORDER BY time DESC) AS rn)`:     1,
		`SELECT ` + repoNameSQL + ` AS name FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name) AS rn)`: 0,
		`SELECT a.x FROM a LEFT JOIN b ON b.repo = a.repo AND b.rn = 1`:                                        1,
		`SELECT COUNT(DISTINCT repo) AS n FROM t WHERE repo IN ('(x)')`:                                        1,
		`SELECT DISTINCT full_name, repo, workflow FROM t`:                                                     0,
	} {
		if got := sqlShortNameKeys(sql); len(got) != want {
			t.Errorf("%s: found %v, want %d", sql, got, want)
		}
	}
	traffic := gp("gh_traffic", "count")
	for expr, want := range map[string]int{
		fmt.Sprintf(`groupByNode(%s, %d, "sum")`, traffic, gn("gh_traffic", "repo")):            1,
		grGroupBy(traffic, "gh_traffic", "sum", "repo"):                                         0,
		rowsOf(traffic, gn("gh_traffic", "repo")):                                               0,
		rowsOf("sumSeries("+traffic+")", gn("gh_traffic", "repo")):                              1,
		fmt.Sprintf(`groupByNode(isNonNull(%s), %d, "sum")`, traffic, gn("gh_traffic", "kind")): 0,
	} {
		if got := graphiteShortNameKeys(t, expr); len(got) != want {
			t.Errorf("%s: found %v, want %d", expr, got, want)
		}
	}
	fullNamed := map[string]bool{"gh_traffic": true, "gh_issue_comments": false}
	for expr, want := range map[string]int{
		`sum by (repo) (github_traffic_count)`:                       1,
		`sum by (full_name, repo) (github_traffic_count)`:            0,
		`sum by (full_name) (increase(github_issue_comments_total))`: 1,
		`sum by (kind) (github_issue_comments_total)`:                0,
	} {
		if got := promShortNameKeys(expr, fullNamed); len(got) != want {
			t.Errorf("%s: found %v, want %d", expr, got, want)
		}
	}
	for _, c := range []struct {
		buckets []string
		want    int
	}{
		{[]string{"repo", "number"}, 1},
		{[]string{"full_name", "repo", "number"}, 0},
		{[]string{"tag", "repo"}, 1},
	} {
		b := &builder{}
		var buckets []any
		for _, tag := range c.buckets {
			buckets = append(buckets, b.tm(tag, 10))
		}
		if got := esShortNameKeys(buckets, nil); len(got) != c.want {
			t.Errorf("%v: found %v, want %d", c.buckets, got, c.want)
		}
	}
}

// ── SQL ─────────────────────────────────────────────────────────────────────

var (
	sqlPartitionBy = regexp.MustCompile(`(?s)\bPARTITION BY (.+?)(?: ORDER BY |$)`)
	sqlSelectList  = regexp.MustCompile(`(?s)^\s*SELECT (DISTINCT )?(.+?) FROM\b`)
	sqlGroupBy     = regexp.MustCompile(`(?s)\bGROUP BY (.+?)(?: HAVING | ORDER BY | LIMIT |$)`)
	sqlDistinctOf  = regexp.MustCompile(`(?s)^\s*DISTINCT (.+)$`)
	sqlJoinOnRepo  = regexp.MustCompile(`\b(?:\w+\.)?repo\s*=\s*(?:\w+\.)?repo\b`)
	sqlUnion       = regexp.MustCompile(`\bUNION(?: ALL)?\b`)
	sqlRepoColumn  = regexp.MustCompile(`^(?:\w+\.)?repo$`)
	sqlFullColumn  = regexp.MustCompile(`^(?:\w+\.)?full_name$`)
	sqlOrdinal     = regexp.MustCompile(`^[0-9]+$`)
)

// sqlShortNameKeys is every place a statement tells repositories apart by the
// short name with no full name beside it. repoNameSQL, which reads the short
// name only to tell whether two full names share it, is set aside first.
func sqlShortNameKeys(sql string) []string {
	s := blankQuoted(strings.ReplaceAll(withoutCollation(sql), repoNameSQL, "name"))
	var out []string
	for _, level := range sqlLevels(s) {
		for _, part := range sqlUnion.Split(level, -1) {
			out = append(out, sqlPartKeys(part)...)
		}
	}
	return out
}

// sqlPartKeys is sqlShortNameKeys over one SELECT with its parentheses
// folded away, so that each list below is at the depth it is written at.
func sqlPartKeys(part string) []string {
	var out []string
	check := func(clause string, keys []string) {
		repo, full := false, false
		for _, k := range keys {
			repo = repo || sqlRepoColumn.MatchString(k)
			full = full || sqlFullColumn.MatchString(k)
		}
		if repo && !full {
			out = append(out, fmt.Sprintf("%s %s", clause, strings.Join(keys, ", ")))
		}
	}
	for _, m := range sqlPartitionBy.FindAllStringSubmatch(part, -1) {
		check("partitions by", sqlItems(m[1]))
	}
	var selected []string
	if m := sqlSelectList.FindStringSubmatch(part); m != nil {
		selected = sqlItems(m[2])
		if m[1] != "" {
			check("selects distinct", selected)
		}
	}
	if m := sqlDistinctOf.FindStringSubmatch(part); m != nil && !strings.Contains(part, " FROM ") {
		check("counts distinct", sqlItems(m[1]))
	}
	for _, m := range sqlGroupBy.FindAllStringSubmatch(part, -1) {
		keys := sqlItems(m[1])
		for i, k := range keys {
			if !sqlOrdinal.MatchString(k) {
				continue
			}
			if n, _ := strconv.Atoi(k); n >= 1 && n <= len(selected) {
				keys[i] = selected[n-1]
			}
		}
		check("groups by", keys)
	}
	if m := sqlJoinOnRepo.FindString(part); m != "" {
		out = append(out, "joins on "+m)
	}
	return out
}

// sqlItems is a comma-separated list, each item without the alias it is given.
func sqlItems(list string) []string {
	var out []string
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if at := strings.Index(item, " AS "); at >= 0 {
			item = strings.TrimSpace(item[:at])
		}
		out = append(out, item)
	}
	return out
}

// sqlLevels is a statement cut at its parentheses: the text inside each pair,
// with every pair inside that folded to one character, and the text outside
// them all, folded the same way.
func sqlLevels(s string) []string {
	var out []string
	stack := []*strings.Builder{{}}
	for _, r := range s {
		top := stack[len(stack)-1]
		switch {
		case r == '(':
			top.WriteRune('§')
			stack = append(stack, &strings.Builder{})
		case r == ')' && len(stack) > 1:
			out = append(out, top.String())
			stack = stack[:len(stack)-1]
		default:
			top.WriteRune(r)
		}
	}
	return append(out, stack[0].String())
}

// blankQuoted empties every string literal and quoted identifier, whose text
// can hold a parenthesis or a comma, the way outsideQuotes finds them.
func blankQuoted(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		q := s[i]
		if q != '\'' && q != '"' {
			out.WriteByte(q)
			i++
			continue
		}
		j := i + 1
		for j < len(s) {
			if s[j] != q {
				j++
				continue
			}
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			break
		}
		out.WriteByte(q)
		out.WriteByte(q)
		i = j + 1
	}
	return out.String()
}

// ── Elasticsearch ───────────────────────────────────────────────────────────

// esShortNameKeys is every terms bucket on the short name that no bucket on
// the full name encloses, where the short name is one value per full name,
// there for the row to show, and every count of distinct short names.
func esShortNameKeys(buckets, metrics []any) []string {
	fields := bucketFieldsOf(buckets)
	var out []string
	for i, f := range fields {
		if f == inventoryRepoTerm && !slices.Contains(fields[:i], any(panelFullNameField)) {
			out = append(out, fmt.Sprintf("buckets by %v", fields))
		}
	}
	for _, raw := range metrics {
		if m := agg(raw); m["type"] == "cardinality" && m["field"] == inventoryRepoTerm {
			out = append(out, "counts the distinct short names")
		}
	}
	return out
}

// ── Graphite ────────────────────────────────────────────────────────────────

// grCall is one Graphite expression as a tree: a function and its arguments,
// or a path, a quoted string or a bare word.
type grCall struct {
	fn, word string
	quoted   bool
	args     []grCall
}

// parseGraphiteCall reads one expression from p, the way exprParser reads a
// path or a string, and every call inside it.
func parseGraphiteCall(p *exprParser) grCall {
	p.t.Helper()
	p.skip()
	if strings.HasPrefix(p.s[p.i:], `"`) {
		return grCall{word: p.quoted(), quoted: true}
	}
	// A path can hold a glob's braces and commas, which exprParser's word
	// stops at, so a path runs to the next parenthesis, comma or space.
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(`(), "`, rune(p.s[p.i])) {
		if p.s[p.i] == '{' {
			p.i += strings.IndexByte(p.s[p.i:], '}')
		}
		p.i++
	}
	word := p.s[start:p.i]
	if word == "" && !strings.HasPrefix(p.s[p.i:], "(") {
		p.t.Fatalf("%s: nothing to read at %q", p.s, p.s[p.i:])
	}
	if !p.eat("(") {
		return grCall{word: word}
	}
	call := grCall{fn: word}
	for !p.eat(")") {
		call.args = append(call.args, parseGraphiteCall(p))
		p.eat(",")
	}
	return call
}

// graphiteShortNameKeys is every groupByNode and groupByNodes that groups by
// the short name's node without the full name's.
func graphiteShortNameKeys(t *testing.T, expr string) []string {
	t.Helper()
	if expr == "" {
		return nil
	}
	p := &exprParser{t: t, s: expr}
	tree := parseGraphiteCall(p)
	if p.skip(); p.i != len(p.s) {
		t.Fatalf("%s: unparsed from %q", expr, p.s[p.i:])
	}
	var out []string
	graphiteNodeTags(tree, &out)
	return out
}

// graphiteNodeTags is the tag each node of a series name comes from, in order,
// or nil when the name is not made of tags, and it records every grouping
// over the short name alone as it goes. A function that only transforms the
// points keeps the names it was given, which is also what groupByNode reads
// through it: graphite-web takes the nodes from the path inside.
func graphiteNodeTags(c grCall, found *[]string) []string {
	if c.fn == "" {
		if c.quoted {
			return nil
		}
		return graphitePathTags(c.word)
	}
	var lists [][]string
	var nodes []int
	for _, a := range c.args {
		if n, err := strconv.Atoi(a.word); err == nil && a.fn == "" && !a.quoted {
			nodes = append(nodes, n)
			continue
		}
		if a.fn != "" || (!a.quoted && strings.HasPrefix(a.word, "github.")) {
			lists = append(lists, graphiteNodeTags(a, found))
		}
	}
	var first []string
	if len(lists) > 0 {
		first = lists[0]
	}
	switch c.fn {
	case "aliasByNode", "groupByNode", "groupByNodes":
		if len(first) == 1 && slices.Contains(graphiteAggregates, first[0]) {
			*found = append(*found, fmt.Sprintf("%s names rows by the nodes of the one series %s "+
				"made of all of them", c.fn, first[0]))
		}
	}
	switch c.fn {
	case "aliasByNode":
		return pickNodes(first, nodes)
	case "groupByNode", "groupByNodes":
		keys := pickNodes(first, nodes)
		if slices.Contains(keys, "repo") && !slices.Contains(keys, "full_name") {
			*found = append(*found, fmt.Sprintf("%s groups by %v", c.fn, keys))
		}
		return keys
	case "alias", "asPercent":
		return []string{c.fn}
	}
	if slices.Contains(graphiteAggregates, c.fn) {
		return []string{c.fn}
	}
	return first
}

// graphiteAggregates are the functions that answer one series for all the
// series they are given, named after the first of them: graphite-web
// 1.1.10 answered aliasByNode(sumSeries(isNonNull(a.*.x)), 1) with one row,
// "a", holding the count of every series, where the grouping meant a row
// each. A table that names rows after one of these has one row.
var graphiteAggregates = []string{"sumSeries", "maxSeries", "diffSeries", "countSeries", "percentileOfSeries"}

// graphitePathTags names the nodes of a path by the tags of the shape its
// depth matches, the measurement's current one or one it had before.
func graphitePathTags(path string) []string {
	parts := strings.Split(path, ".")
	if len(parts) < 3 || parts[0] != "github" {
		return nil
	}
	m := "gh_" + parts[1]
	shapes := [][]string{tags[m]}
	if former, ok := formerTags[m]; ok {
		shapes = append(shapes, former)
	}
	for _, shape := range shapes {
		if shape != nil && len(parts) == len(shape)+3 {
			return append(append([]string{"github", "measurement"}, shape...), "field")
		}
	}
	return nil
}

// pickNodes is the tags at the given nodes of a name, "?" where it is not known.
func pickNodes(names []string, nodes []int) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = "?"
		if n >= 0 && n < len(names) {
			out[i] = names[n]
		}
	}
	return out
}

// ── Prometheus ──────────────────────────────────────────────────────────────

var (
	promLabelList = regexp.MustCompile(`\b(by|on)\s*\(([^)]*)\)`)
	promMetric    = regexp.MustCompile(`\bgithub_[a-z0-9_]+`)
)

// promShortNameKeys is every `by` or `on` list that names the short name and
// not the full name, and every metric read under a list of repositories whose
// series the exporter does not keep apart by full name: a rule that keeps
// neither name makes the list one row for every repository there is.
func promShortNameKeys(expr string, fullNamed map[string]bool) []string {
	var out []string
	perRepository := false
	for _, m := range promLabelList.FindAllStringSubmatch(expr, -1) {
		labels := sqlItems(m[2])
		if !slices.Contains(labels, "repo") && !slices.Contains(labels, "full_name") {
			continue
		}
		perRepository = true
		if !slices.Contains(labels, "full_name") {
			out = append(out, fmt.Sprintf("aggregates %s (%s)", m[1], m[2]))
		}
	}
	if !perRepository {
		return out
	}
	for _, metric := range slices.Compact(slices.Sorted(slices.Values(promMetric.FindAllString(expr, -1)))) {
		if !promKeepsFullName(metric, fullNamed) {
			out = append(out, "reads "+metric+", which the exporter keys by the short name alone")
		}
	}
	return out
}

// promKeepsFullName reports whether the gauge a metric belongs to kept two
// owners' repositories of one name apart: the longest gauge name the metric
// starts with, since github_repo_ is a prefix of github_repo_total_.
func promKeepsFullName(metric string, fullNamed map[string]bool) bool {
	best, apart := "", false
	for name, ok := range fullNamed {
		prefix := "github_" + strings.TrimPrefix(name, "gh_") + "_"
		if strings.HasPrefix(metric, prefix) && len(prefix) > len(best) {
			best, apart = prefix, ok
		}
	}
	return apart
}

// promGaugesWithFullNames is, for every gauge the exporter makes of a
// measurement that names a repository, whether it keeps two owners'
// repositories of one name apart, the way twoOwnersPromSeries evaluates the
// reducer: one point of each, every other tag the same.
func promGaugesWithFullNames() map[string]bool {
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	var points []sink.Point
	for m, keys := range tags {
		if !slices.Contains(keys, "repo") || !slices.Contains(keys, "full_name") {
			continue
		}
		for i, owner := range []string{"alice", "acme"} {
			values := map[string]string{}
			for _, k := range keys {
				values[k] = "t"
			}
			maps.Copy(values, map[string]string{"owner": owner, "repo": "dotfiles", "full_name": owner + "/dotfiles"})
			points = append(points, sink.Point{
				Measurement: m, Tags: values, Fields: map[string]any{"v": 1.0},
				Time: at.Add(time.Duration(i) * time.Hour),
			})
		}
	}
	names := map[string]map[string]bool{}
	for _, g := range sink.Summarize(points) {
		if names[g.Measurement] == nil {
			names[g.Measurement] = map[string]bool{}
		}
		names[g.Measurement][g.Tags["full_name"]] = true
	}
	out := map[string]bool{}
	for m, seen := range names {
		out[m] = seen["alice/dotfiles"] && seen["acme/dotfiles"]
	}
	return out
}
