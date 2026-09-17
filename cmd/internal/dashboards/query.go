package dashboards

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Every per-repository panel is filtered by one of these. Grafana expands a
// multi-value variable with the `singlequote` formatter into 'a','b', which is
// exactly what SQL's IN wants; the default formatter would produce {a,b} and
// fail. The Prometheus variable has `.*` as its "All" value, so a regex match
// is the one form that works for both a single selection and all of them.
// PostgreSQL's datasource has its own `sqlstring` formatter for the same job.
// Graphite wants a glob, which is the default formatter, so the variable is a
// bare path node. Elasticsearch wants Lucene: `("a" OR "b")` for several, and
// the `.keyword` field because a dynamically mapped string is analyzed text.
const (
	RF  = "repo IN (${repo:singlequote})"
	PF  = `repo=~"$repo"`
	PGF = "repo IN (${repo:sqlstring})"
	GRF = "$repo"
	ESF = "repo.keyword:${repo:lucene}"
)

// Stores names each store as the prose refers to it.
var Stores = map[string]string{
	"influxdb":      "InfluxDB",
	"prometheus":    "Prometheus",
	"postgres":      "PostgreSQL",
	"graphite":      "Graphite",
	"elasticsearch": "Elasticsearch",
}

// Target is one query, in whichever dialect its store speaks. Only the fields
// its kind uses are set; the renderer turns it into the datasource's own
// target shape.
type Target struct {
	Kind string // "sql", "prom", "gr", "es", "expr"
	Ref  string

	// SQL and PostgreSQL.
	Format string
	SQL    string

	// Prometheus.
	Expr    string
	Instant bool
	Legend  string
	HasLeg  bool
	Step    string
	Fmt     string

	// Graphite.
	GTarget string

	// Elasticsearch.
	Query   string
	Metrics []any
	Buckets []any
	Alias   string
	Hide    bool

	// Server-side expression.
	ExprType   string
	Expression string
	Settings   map[string]any
}

func sqlT(q string) Target {
	return Target{Kind: "sql", Format: "table", SQL: q, Ref: "A"}
}

func sqlTS(q string) Target {
	return Target{Kind: "sql", Format: "time_series", SQL: q, Ref: "A"}
}

func refOr(ref []string) string {
	if len(ref) > 0 {
		return ref[0]
	}
	return "A"
}

type promOpt func(*Target)

func withRef(ref string) promOpt { return func(t *Target) { t.Ref = ref } }
func instant() promOpt           { return func(t *Target) { t.Instant = true } }
func legend(s string) promOpt    { return func(t *Target) { t.Legend, t.HasLeg = s, true } }
func step(s string) promOpt      { return func(t *Target) { t.Step = s } }
func asTable() promOpt           { return func(t *Target) { t.Fmt = "table" } }
func promq(e string, o ...promOpt) Target {
	t := Target{Kind: "prom", Expr: e, Ref: "A"}
	for _, f := range o {
		f(&t)
	}
	return t
}

// promNow is an instant query: the value now rather than a series.
func promNow(e string) Target {
	return promq(e, instant())
}

// promTbl is an instant query rendered as a table, which is how a label set
// becomes a row rather than a line.
func promTbl(e string, ref ...string) Target {
	return promq(e, instant(), asTable(), withRef(refOr(ref)))
}

// daily is an increase over the panel's own step, floored at one day: one bar
// per bucket, the twin of $__dateBin on the SQL side. The window has to be
// the step and not a fixed day, because the step widens with the range (see
// binned in panels.go) and an increase over one day sampled every seven
// would count one day in seven.
//
// Callers write the window as the floor, `[1d]` or `[1h]`, which reads as
// what the panel shows at its narrowest; the step is what is sent.
func daily(e, leg string) Target {
	return promq(strings.ReplaceAll(e, "[1d]", binStep), legend(leg), step("1d"))
}

func hourly(e, leg string, ref ...string) Target {
	return promq(strings.ReplaceAll(e, "[1h]", binStep), legend(leg), step("1h"), withRef(refOr(ref)))
}

// binStep is the range selector a daily or hourly panel's increase takes.
const binStep = "[$__interval]"

// organize drops the columns Prometheus adds to every table frame and names
// the rest.
func organize(rename map[string]string, exclude []string, order map[string]int) any {
	excludeBy := map[string]any{}
	for _, k := range append([]string{
		"Time", "__name__", "job", "instance", "owner", "full_name",
	}, exclude...) {
		excludeBy[k] = true
	}
	if rename == nil {
		rename = map[string]string{}
	}
	idx := map[string]any{}
	for k, v := range order {
		idx[k] = v
	}
	return map[string]any{"id": "organize", "options": map[string]any{
		"excludeByName": excludeBy,
		"renameByName":  toAnyMap(rename),
		"indexByName":   idx,
	}}
}

// merged joins several instant queries into one table. `merge` unifies the
// columns the frames share (Time and the `by` labels, which must be identical
// across the queries) and keeps each Value #X as its own column.
func merged(rename map[string]string, exclude []string, order map[string]int) []any {
	return []any{
		map[string]any{"id": "merge", "options": map[string]any{}},
		organize(rename, exclude, order),
	}
}

func toAnyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cannot(what, why string, store ...string) string {
	s := "prometheus"
	if len(store) > 0 {
		s = store[0]
	}
	return fmt.Sprintf("**Not available from %s.** In the InfluxDB dashboard this panel "+
		"shows %s\n\n%s", Stores[s], what, why)
}

// ── PostgreSQL ──────────────────────────────────────────────────────────────

var pgBins = [][2]string{
	{"1 day", "1d"}, {"7 days", "7d"}, {"1 hour", "1h"}, {"5 minutes", "5m"},
}

var (
	pgPercentile = regexp.MustCompile(`approx_percentile_cont\(([a-z_]+), ([0-9.]+)\)`)
	pgReserved   = []string{"user", "by", "key", "limit", "check"}
	// The InfluxDB plugin's bucket macro and the PostgreSQL plugin's, which
	// takes the interval as an argument and reads $__interval for it.
	pgDateBinAlias = regexp.MustCompile(`\$__dateBin\(([a-z.]+)\) AS time`)
	pgDateBin      = regexp.MustCompile(`\$__dateBin\(([a-z.]+)\)`)
	// The PostgreSQL plugin reads a bare $__timeGroup followed by a comma as
	// the pre-5.3 spelling of the aliased one and appends AS "time" to it,
	// which inside a window's PARTITION BY list is a syntax error. Wrapping
	// the macro puts a parenthesis after it instead, which the plugin leaves
	// alone.
	pgBareBucket = regexp.MustCompile(`(\$__timeGroup\([^)]*\)),`)
	pgAgo        = regexp.MustCompile(`arrow_cast\(([a-z_.]+) \* 1000000000, 'Duration\(Nanosecond\)'\)`)
	// The series column, under every name it is referenced by rather than
	// only where it is aliased: a query that wraps a subquery selects it by
	// name, and renaming the alias alone left that outer reference dangling.
	pgSeries     = regexp.MustCompile(`\bseries\b`)
	pgReservedRe = func() map[string]*regexp.Regexp {
		out := make(map[string]*regexp.Regexp, len(pgReserved))
		for _, word := range pgReserved {
			out[word] = regexp.MustCompile(`\b` + word + `\b`)
		}
		return out
	}()
)

// outsideQuotes applies f to every span of a statement that is not inside a
// string literal or a quoted identifier, and leaves the quoted spans exactly
// as they were.
//
// Every rewrite below it works on bare identifiers, and a bare-identifier
// rewrite applied to the whole statement also edits the text a user reads:
// the reserved-word pass turned `AS "Covered by the plan"` into
// `AS "Covered "by" the plan"`, which PostgreSQL refuses.
func outsideQuotes(s string, f func(string) string) string {
	var out strings.Builder
	plain := 0
	for i := 0; i < len(s); {
		q := s[i]
		if q != '\'' && q != '"' {
			i++
			continue
		}
		out.WriteString(f(s[plain:i]))
		// Scan to the closing quote, where a doubled quote is an escape and
		// not the end.
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
			j++
			break
		}
		out.WriteString(s[i:j])
		i, plain = j, j
	}
	out.WriteString(f(s[plain:]))
	return out.String()
}

// toPG translates a panel's InfluxDB SQL into PostgreSQL.
//
// The SQL sink writes the same facts as one table per measurement with the
// same names, so the translation is dialect only: Grafana's time macro, the
// percentile aggregate, the series column the PostgreSQL datasource names
// `metric`, and the tag names that are reserved words there. The `number` tag
// is never NULL in PostgreSQL (a tag column is TEXT NOT NULL DEFAULT ”), so
// the identity test compares against the empty string.
func toPG(q string) string {
	s := q
	// A dollar in a replacement is a group reference, so the macros' own
	// dollars are doubled or the output reads "(time, )".
	s = pgDateBinAlias.ReplaceAllString(s, "$$__timeGroupAlias(${1}, $$__interval)")
	s = pgDateBin.ReplaceAllString(s, "$$__timeGroup(${1}, $$__interval)")
	s = pgBareBucket.ReplaceAllString(s, "(${1}),")
	s = pgAgo.ReplaceAllString(s, "${1} * INTERVAL '1 second'")
	// The anchors of the cumulative curves. Their bucket column is whatever
	// the bucket macro returns, and the two datasources disagree about that:
	// $__dateBin gives InfluxDB a timestamp, while $__timeGroupAlias gives
	// PostgreSQL the epoch seconds it turns back into a time field, so a
	// timestamp literal in the same UNION was cast to numeric and refused
	// ("invalid input syntax for type numeric", measured in the containerised
	// suite). The epoch spelling of the same two bounds is what matches it.
	// Only the anchors: $__timeFrom() is also compared against the time
	// column itself, where a timestamp is what it has to be.
	s = strings.ReplaceAll(s, "UNION ALL SELECT $__timeFrom(), 0", "UNION ALL SELECT $__unixEpochFrom(), 0")
	s = strings.ReplaceAll(s, "UNION ALL SELECT $__timeTo(), 0", "UNION ALL SELECT $__unixEpochTo(), 0")
	// The Sunday-aligned week of the contribution calendar: PostgreSQL's
	// date_bin takes the stride as an interval literal and the origin as a
	// plain timestamp.
	s = strings.ReplaceAll(s, sundayWeek, "date_bin('7 days', time, TIMESTAMP '1970-01-04')")
	for _, b := range pgBins {
		s = strings.ReplaceAll(s,
			fmt.Sprintf("date_bin(INTERVAL '%s', time) AS time", b[0]),
			fmt.Sprintf("$__timeGroupAlias(time, %s)", b[1]))
	}
	s = pgPercentile.ReplaceAllString(s, "percentile_cont($2) WITHIN GROUP (ORDER BY $1)")
	s = strings.ReplaceAll(s, "${repo:singlequote}", "${repo:sqlstring}")
	s = strings.ReplaceAll(s, identified, `number <> ''`)
	// Both of these rewrite bare identifiers, so both run outside the quoted
	// spans only: a value of a stat group is named in a quoted identifier
	// ("Covered by the plan"), and a string literal can hold anything.
	s = outsideQuotes(s, func(run string) string {
		run = pgSeries.ReplaceAllString(run, "metric")
		for _, word := range pgReserved {
			run = pgReservedRe[word].ReplaceAllString(run, `"`+word+`"`)
		}
		return run
	})
	// PostgreSQL has a date_bin of its own, which the Sunday week above is
	// written with; the DataFusion spelling is the one with INTERVAL.
	if strings.Contains(s, "date_bin(INTERVAL") || strings.Contains(s, "approx_percentile") ||
		strings.Contains(s, "$__dateBin") || strings.Contains(s, "arrow_cast") {
		panic("untranslated SQL for PostgreSQL: " + s)
	}
	return s
}

func pgOf(qs []Target) []Target {
	out := make([]Target, len(qs))
	copy(out, qs)
	for i := range out {
		out[i].SQL = toPG(out[i].SQL)
	}
	return out
}

// ── Graphite ────────────────────────────────────────────────────────────────

// gp is the path of one field: a wildcard for every tag not pinned by name.
func gp(m, field string, fixed ...string) string {
	pinned := map[string]string{}
	for i := 0; i+1 < len(fixed); i += 2 {
		pinned[fixed[i]] = fixed[i+1]
	}
	parts := []string{"github", strings.TrimPrefix(m, "gh_")}
	for _, tag := range tagsOf(m) {
		if v, ok := pinned[tag]; ok {
			parts = append(parts, v)
		} else {
			parts = append(parts, "*")
		}
	}
	return strings.Join(append(parts, field), ".")
}

// rp is the same, with `repo` as the dashboard variable.
func rp(m, field string, fixed ...string) string {
	return gp(m, field, append([]string{"repo", GRF}, fixed...)...)
}

// gn is the node index of a tag, for groupByNode and aliasByNode.
func gn(m, tag string) int {
	for i, t := range tagsOf(m) {
		if t == tag {
			return 2 + i
		}
	}
	panic("no tag " + tag + " on " + m)
}

func tagsOf(m string) []string {
	t, ok := tags[m]
	if !ok {
		panic("no tag list for " + m)
	}
	return t
}

// events is the path of one gh_event field. The collector writes `action` and
// `ref_type` on every row now, with the fallback where the payload has none,
// so the depth is fixed and the tag table applies; the callers still count
// their nodes from the end, where `repo` and `type` are the last two tags.
func events(field string) string { return gp("gh_event", field) }

func grq(expr string, ref ...string) Target {
	return Target{Kind: "gr", GTarget: expr, Ref: refOr(ref)}
}

// countOf is one point per fact, whatever the field's value: the twin of
// COUNT(*).
func countOf(path string) string { return "sumSeries(isNonNull(" + path + "))" }

// total makes the whole range one bucket, so a stat can show a sum over it.
// The bucket is aligned to the range start and outlives any range a dashboard
// will be given.
func total(expr string) string {
	return fmt.Sprintf(`summarize(%s, "100y", "sum", true)`, expr)
}

func medianTotal(path string) string {
	return fmt.Sprintf(`summarize(percentileOfSeries(%s, 50), "100y", "median", true)`, path)
}

// perBucket is one series per value of a tag node, one point per bucket.
// Counts are consolidated by sum so a long range does not average them away.
func perBucket(expr string, node int, spanHow ...string) string {
	span, how := "1d", "sum"
	if len(spanHow) > 0 {
		span = spanHow[0]
	}
	if len(spanHow) > 1 {
		how = spanHow[1]
	}
	return fmt.Sprintf(`consolidateBy(groupByNode(summarize(%s, %q, %q), %d, %q), %q)`,
		expr, span, how, node, how, how)
}

func medianBucket(path string, span ...string) string {
	s := "1d"
	if len(span) > 0 {
		s = span[0]
	}
	return fmt.Sprintf(`summarize(percentileOfSeries(%s, 50), %q, "median")`, path, s)
}

func worstBucket(path string, span ...string) string {
	s := "1d"
	if len(span) > 0 {
		s = span[0]
	}
	return fmt.Sprintf(`summarize(maxSeries(%s), %q, "max")`, path, s)
}

func latestSum(path string) string { return "sumSeries(keepLastValue(" + path + "))" }

func rowsOf(path string, nodes ...int) string {
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		parts[i] = strconv.Itoa(n)
	}
	return fmt.Sprintf("aliasByNode(%s, %s)", path, strings.Join(parts, ", "))
}

func topRows(path string, n int, nodes ...int) string {
	return topRowsBy(path, n, "sortByMaxima", nodes...)
}

func topRowsBy(path string, n int, by string, nodes ...int) string {
	return rowsOf(fmt.Sprintf("limit(%s(%s), %d)", by, path, n), nodes...)
}

var reducers = map[string]string{
	"lastNotNull": "Last *", "sum": "Total", "mean": "Mean", "median": "Median",
	"max": "Max", "count": "Count",
}

// gTbl renders a Graphite series list as a table: one row per series, one
// column per reducer, named as the panel wants them. `cols` is ordered because
// Grafana's reduce transformation emits the columns in the order given.
type col struct{ Reducer, Name string }

// removeEmptySeries drops the series that hold nothing inside the range.
//
// Graphite answers for every path that exists, whatever the range: a series
// whose only point is older than the window comes back as a full row of nulls
// rather than not at all. Reduced to a table that is a row per such path, and
// what it says depends on the reducer, none of it true: `NaN` from a mean or a
// last value, `0` from a count. Nine tables drew one, and each of them was a
// fork made in 2024, an alert closed in July, a sponsorship from 2021 or a
// contribution year before this one, listed beside the rows that do have a
// point with a number that reads like a measurement of them.
//
// The SQL stores have nothing to do here: no row in the range is no row in the
// result. This is the same sentence in Graphite, and it goes around every
// table rather than around the nine, because the next table would have the
// same hole and nothing would say so.
func removeEmptySeries(expr string) string {
	return "removeEmptySeries(" + expr + ")"
}

func gTbl(expr, name string, cols []col) (targets []Target, tf []any) {
	names := map[string]any{"Field": name}
	list := make([]any, len(cols))
	for i, c := range cols {
		list[i] = c.Reducer
		names[reducers[c.Reducer]] = c.Name
	}
	return []Target{grq(removeEmptySeries(expr))}, []any{
		map[string]any{"id": "reduce", "options": map[string]any{
			"mode": "seriesToRows", "reducers": list, "includeTimeField": false,
		}},
		map[string]any{"id": "organize", "options": map[string]any{
			"excludeByName": map[string]any{}, "indexByName": map[string]any{},
			"renameByName": names,
		}},
	}
}

// ── Elasticsearch ───────────────────────────────────────────────────────────

func idx(m string) string { return "ghchronicle-" + m }

// lq is the Lucene query of a target: the index that holds the measurement, so
// one datasource over ghchronicle-* serves every panel, then the filters.
func lq(m string, clauses ...string) string {
	return strings.Join(append([]string{"_index:" + idx(m)}, clauses...), " AND ")
}

func esq(m string, metrics, buckets []any, ref string, where []string, alias string) Target {
	return Target{
		Kind: "es", Query: lq(m, where...), Metrics: metrics, Buckets: buckets,
		Ref: ref, Alias: alias,
	}
}

// exprT is a server-side expression over other targets of the panel.
func exprT(ref, kind, expression string, settings map[string]any) Target {
	return Target{Kind: "expr", Ref: ref, ExprType: kind, Expression: expression, Settings: settings}
}

// metric is one metric aggregation. Its id comes from the builder, in the
// order the specification asks for them; see builder for why that order is
// the one the exported file keeps.
func (b *builder) metric(kind, field string, settings map[string]any) any {
	m := map[string]any{"type": kind, "id": b.nextESID()}
	if field != "" {
		m["field"] = field
	}
	if len(settings) > 0 {
		m["settings"] = settings
	}
	return m
}

func (b *builder) mCount() any        { return b.metric("count", "", nil) }
func (b *builder) mSum(f string) any  { return b.metric("sum", f, nil) }
func (b *builder) mAvg(f string) any  { return b.metric("avg", f, nil) }
func (b *builder) mMax(f string) any  { return b.metric("max", f, nil) }
func (b *builder) mUniq(f string) any { return b.metric("cardinality", f+".keyword", nil) }

func (b *builder) mPct(f string, percents ...int) any {
	list := make([]any, len(percents))
	for i, p := range percents {
		list[i] = strconv.Itoa(p)
	}
	return b.metric("percentiles", f, map[string]any{"percents": list})
}

// mNewest is the newest document's own values, which is how a snapshot is read
// back: the top metric by time.
func (b *builder) mNewest(fields ...string) any {
	list := make([]any, len(fields))
	for i, f := range fields {
		list[i] = f
	}
	return b.metric("top_metrics", "", map[string]any{
		"metrics": list, "order": "desc", "orderBy": "@timestamp",
	})
}

func (b *builder) mRaw(size int) any {
	return b.metric("raw_data", "", map[string]any{
		"size": strconv.Itoa(size), "order": "desc", "useTimeRange": true,
	})
}

func (b *builder) dh(span ...string) any {
	s := "1d"
	if len(span) > 0 {
		s = span[0]
	}
	return map[string]any{
		"type": "date_histogram", "id": b.nextESID(), "field": "@timestamp",
		"settings": map[string]any{"interval": s, "min_doc_count": "0", "trimEdges": "0"},
	}
}

// esDailyTerms is how many values of a tag a dated breakdown keeps. Every
// panel that asks for one takes this number, so a series that falls outside it
// falls outside it everywhere.
const esDailyTerms = 50

// tm is a terms bucket on a tag. Tags are dynamically mapped as text with a
// keyword sub-field, and only the keyword can be aggregated.
func (b *builder) tm(tag string, size int, order ...string) any {
	return b.terms(tag+".keyword", size, order...)
}

// terms is a terms bucket on a field as it is mapped, keeping `size` values.
// It orders by document count, largest first, unless `order` names what to
// order by and, after that, the direction.
func (b *builder) terms(field string, size int, order ...string) any {
	by, direction := "_count", "desc"
	if len(order) > 0 {
		by = order[0]
	}
	if len(order) > 1 {
		direction = order[1]
	}
	return map[string]any{
		"type": "terms", "id": b.nextESID(), "field": field,
		"settings": map[string]any{
			"size": strconv.Itoa(size), "order": direction, "orderBy": by, "min_doc_count": "1",
		},
	}
}

// tmURL is the url of an item as a bucket, which is the one way a string
// reaches an Elasticsearch table: a top_metrics over a string panics the
// plugin (see the note on Social accounts). One value per parent bucket,
// since an item has one url, and a document written without one lands in a
// bucket keyed by the empty string rather than out of the table, which is
// the empty cell the SQL stores draw for the same row.
func (b *builder) tmURL(field ...string) any {
	name := "url"
	if len(field) > 0 {
		name = field[0]
	}
	t := b.tm(name, 1)
	settings, _ := agg(t)["settings"].(map[string]any)
	settings["missing"] = ""
	return t
}

// one is a single bucket for the whole range: every document of an index
// carries the same `measurement`. A metric without a bucket is not a valid
// query, and a `filters` bucket is not rendered as a table.
func (b *builder) one() any { return b.tm("measurement", 1) }

// noneValue is the sentinel a collector writes for a tag GitHub gives nothing
// for, spelled here exactly as internal/collect writes it, and the three
// shapes a query needs to exclude it in.
//
// All three were wrong at once, each in its own way, and none of them excluded
// anything: SQL compared against the empty string, which the collector never
// writes; Graphite excluded a node named `none`, where the parentheses are not
// path characters and the node is `_none_`; and Elasticsearch asked whether
// the field exists, which it does on every document. So a charge that belongs
// to no repository was listed as a repository called (none) in all five
// stores. TestEveryStoreExcludesTheSentinelTheSameWay holds the three
// together.
const (
	noneValue         = "(none)"
	noneSQL           = "'" + noneValue + "'"
	noneGraphiteNode  = "_none_"
	noneElasticsearch = `"` + noneValue + `"`
)

var esNames = map[string]string{
	"count": "Count", "avg": "Average", "sum": "Sum", "max": "Max", "min": "Min",
	"percentiles": "Percentiles", "top_metrics": "Top Metrics", "cardinality": "Unique Count",
}

// esCols computes the column names Grafana's Elasticsearch datasource gives
// the metrics of a table, the way its response parser does: a type alone when
// it appears once, type and field when it appears twice, `pNN.0 field` for
// each percentile, and `Top Metrics field` when several fields are asked.
func esCols(metrics []any) [][]string {
	var out [][]string
	for _, raw := range metrics {
		m := agg(raw)
		t, _ := m["type"].(string)
		f, _ := m["field"].(string)
		switch t {
		case "count":
			out = append(out, []string{"Count"})
		case "percentiles":
			var group []string
			for _, p := range settingStrings(m, "percents") {
				group = append(group, fmt.Sprintf("p%s.0 %s", p, f))
			}
			out = append(out, group)
		case "top_metrics":
			fields := settingStrings(m, "metrics")
			var group []string
			for _, x := range fields {
				name := "Top Metrics"
				if len(fields) > 1 {
					name += " " + x
				}
				group = append(group, name)
			}
			out = append(out, group)
		default:
			same := 0
			for _, other := range metrics {
				o := agg(other)
				if o["type"] == t && o["id"] != m["id"] {
					same++
				}
			}
			name := esNames[t]
			if same > 0 {
				name += " " + f
			}
			out = append(out, []string{name})
		}
	}
	return out
}

// agg is one metric or bucket aggregation as metric(), terms() and dh() build
// it. Anything else in those lists is a mistake in the specification, which
// stops the generator the way a tag the measurement does not have does.
func agg(raw any) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		panic(fmt.Sprintf("an Elasticsearch aggregation is a map, not %T", raw))
	}
	return m
}

// settingStrings is a list of strings an aggregation's settings carry, the
// percents of mPct or the fields of mNewest. A column heading is made from each
// one, so a list that is missing or holds anything else is a mistake in the
// specification and stops the generator the way agg does.
func settingStrings(m map[string]any, key string) []string {
	settings, _ := m["settings"].(map[string]any)
	list, ok := settings[key].([]any)
	if !ok {
		panic(fmt.Sprintf("a %v aggregation has no %s list in its settings", m["type"], key))
	}
	out := make([]string, len(list))
	for i, v := range list {
		s, isString := v.(string)
		if !isString {
			panic(fmt.Sprintf("a %v aggregation's %s holds a %T, not a string", m["type"], key, v))
		}
		out[i] = s
	}
	return out
}

// bucketField is the field a bucket aggregation groups by. terms() and dh()
// always name one, and the table renames its column by that name.
func bucketField(raw any) string {
	b := agg(raw)
	field, ok := b["field"].(string)
	if !ok {
		panic(fmt.Sprintf("a %v bucket names no field to group by", b["type"]))
	}
	return field
}

// named is one column heading in the order the panel wants it.
type named struct{ From, To string }

// esTbl is an aggregation table: `names` maps each bucket field and, in order,
// each metric column to the column the panel wants.
func esTbl(m string, buckets, metrics []any, names []named, where []string, extra ...any) (targets []Target, tf []any) {
	var cols []string
	for _, group := range esCols(metrics) {
		cols = append(cols, group...)
	}
	bucketFields := map[string]bool{}
	for _, b := range buckets {
		bucketFields[bucketField(b)] = true
	}
	rename := map[string]any{}
	var rest []named
	for _, n := range names {
		if bucketFields[n.From] {
			rename[n.From] = n.To
		} else {
			rest = append(rest, n)
		}
	}
	for i, c := range cols {
		if i < len(rest) {
			rename[c] = rest[i].To
		}
	}
	tf = []any{map[string]any{"id": "organize", "options": map[string]any{
		"excludeByName": map[string]any{}, "indexByName": map[string]any{},
		"renameByName": rename,
	}}}
	tf = append(tf, extra...)
	return []Target{esq(m, metrics, buckets, "A", where, "")}, tf
}

// esRaw is the newest documents as rows. `names` maps document keys, in the
// order the columns should appear, to their headings.
func (b *builder) esRaw(m string, size int, names []named, where []string) (targets []Target, tf []any) {
	include := map[string]any{}
	index := map[string]any{}
	rename := map[string]any{}
	for i, n := range names {
		include[n.From] = true
		index[n.From] = i
		rename[n.From] = n.To
	}
	return []Target{esq(m, []any{b.mRaw(size)}, []any{}, "A", where, "")},
		[]any{map[string]any{"id": "organize", "options": map[string]any{
			"excludeByName": map[string]any{}, "includeByName": include,
			"indexByName": index, "renameByName": rename,
		}}}
}

// groupSum sums a column over the rows that share another, for the snapshots
// that are taken per repository and asked for per something else.
func groupSum(by, value, nameBy, nameValue string) []any {
	return []any{
		map[string]any{"id": "groupBy", "options": map[string]any{"fields": map[string]any{
			by:    map[string]any{"operation": "groupby", "aggregations": []any{}},
			value: map[string]any{"operation": "aggregate", "aggregations": []any{"sum"}},
		}}},
		map[string]any{"id": "organize", "options": map[string]any{
			"excludeByName": map[string]any{}, "indexByName": map[string]any{},
			"renameByName": map[string]any{by: nameBy, value + " (sum)": nameValue},
		}},
	}
}

// esSnapshotStack is a per-repository snapshot drawn as history: the largest
// reading of each repository in each day, one series per repository, for a
// panel that stacks them (esStacked) so the top of the stack is the account's
// total. gh_repo is written every hour, so a sum over the day counted each
// repository twenty four times and the star curve read 6,900 for an account
// with 288, and a date histogram cannot take the newest reading of each
// repository before adding them; a max per repository per day can, and for a
// count that only climbs it is the newest. Every repository is a bucket, not
// the fifty of esDailyTerms: a repository left out is stars missing from the
// total.
func (b *builder) esSnapshotStack(m, field string) Target {
	return esq(m, []any{b.mMax(field)}, []any{b.tm("repo", 500), b.dh()}, "A",
		[]string{ESF}, "{{term repo.keyword}}")
}

// esStacked is the panel side of esSnapshotStack: the series stacked, and no
// legend, since fifty repository names under the plot are not what the curve
// is about.
var esStacked = Opts{"stack": true, "legend": "hidden"}

// esLatestSum is the newest value per repository (and further tags), which the
// stat sums across rows: the twin of latestSum in SQL.
func (b *builder) esLatestSum(m, field string, by ...string) []Target {
	// The metric before the buckets, because the ids are handed out in the
	// order they are asked for and a panel's targets are compared as text.
	metrics := []any{b.mNewest(field)}
	buckets := []any{b.tm("repo", 500)}
	for _, tag := range by {
		buckets = append(buckets, b.tm(tag, 500))
	}
	return []Target{esq(m, metrics, buckets, "A", nil, "")}
}

func (b *builder) esTotal(m string, met any, where ...string) []Target {
	return []Target{esq(m, []any{met}, []any{b.one()}, "A", where, "")}
}

func (b *builder) esDaily(m string, met any, by, span string, where []string, ref string) Target {
	var buckets []any
	if by != "" {
		buckets = append(buckets, b.tm(by, esDailyTerms))
	}
	if span == "" {
		span = "1d"
	}
	buckets = append(buckets, b.dh(span))
	alias := ""
	if by != "" {
		alias = fmt.Sprintf("{{term %s.keyword}}", by)
	}
	if ref == "" {
		ref = "A"
	}
	return esq(m, []any{met}, buckets, ref, where, alias)
}
