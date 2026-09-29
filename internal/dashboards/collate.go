package dashboards

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// byteOrder makes every comparison of text that decides where a row goes, or
// which row a statement keeps, compare bytes in the PostgreSQL translation, as
// InfluxDB does. InfluxDB compares strings by their UTF-8 bytes; PostgreSQL
// compares them by the database's collation, which on the official Debian
// image, and on any PostgreSQL built on glibc whose database was created under
// a locale such as en_US.UTF-8, is a linguistic one that sets case and
// punctuation aside on its first pass. Measured on 2026-09-29
// against postgres:18.6 (Debian, en_US.utf8) loaded with the containerised
// suite's sweep: "Pull requests by author" and "Reviewers" put "(ghost)" after
// "alice" and "bob", and "Commits by signature" put "VALID" after "unsigned",
// where InfluxDB draws both the other way round. postgres:18.6-alpine agreed
// with InfluxDB only because musl's collation is the byte order whatever the
// locale is called.
//
// So a text key of an ORDER BY, of a window's ORDER BY, and the argument of a
// MIN or a MAX, is given COLLATE "C". A key that names an output column, by
// its position or by its name, takes the collation on that column instead:
// PostgreSQL refuses a position or an output name inside an expression, and a
// DISTINCT list refuses an ORDER BY expression it does not select. A
// non-text key is left alone, since PostgreSQL refuses a collation on a number
// or a timestamp, and a key the translation cannot type stops the build, so
// that a panel sorting by a new column is typed when it is written rather
// than found sorting by the database's collation later.
func byteOrder(s string) string {
	st := scanSQL(s)
	var ends []int
	for _, site := range st.sites() {
		if st.textual(s[site[0]:site[1]], nil) {
			ends = append(ends, site[1])
		}
	}
	return collateAt(s, ends)
}

// collateAt gives COLLATE "C" to the expressions ending at each of ends that
// do not carry it yet.
func collateAt(s string, ends []int) string {
	ends = slices.Compact(slices.Sorted(slices.Values(ends)))
	var out strings.Builder
	last := 0
	for _, end := range ends {
		if strings.HasSuffix(s[:end], collateC) {
			continue
		}
		out.WriteString(s[last:end])
		out.WriteString(collateC)
		last = end
	}
	out.WriteString(s[last:])
	return out.String()
}

// CollationProbe is one PostgreSQL statement of these dashboards with one of
// the expressions it sorts by, which byteOrder left to the database's
// collation, given COLLATE "C".
type CollationProbe struct {
	// Key is the expression, as the statement writes it.
	Key string
	SQL string
}

// CollationProbes is a probe for every expression a PostgreSQL statement of
// these dashboards sorts by, or takes the least or the greatest of, without
// COLLATE "C". It types nothing, which is its point: PostgreSQL refuses a
// collation on anything but text, so a probe it accepts is a text key
// byteOrder read as a number, and the containerised suite runs every probe
// to hold byteOrder to that whatever the rows. A Grafana macro is left out,
// being a time and expanding to more than one expression.
func CollationProbes(sql string) []CollationProbe {
	st := scanSQL(sql)
	var out []CollationProbe
	for _, site := range st.sites() {
		key := sql[site[0]:site[1]]
		if strings.HasSuffix(key, collateC) || strings.HasPrefix(key, "$__") {
			continue
		}
		probe := CollationProbe{Key: key, SQL: collateAt(sql, []int{site[1]})}
		if !slices.ContainsFunc(out, func(p CollationProbe) bool { return p.SQL == probe.SQL }) {
			out = append(out, probe)
		}
	}
	return out
}

const collateC = ` COLLATE "C"`

// textFields are the fields that hold text which a panel sorts by or takes the
// least or the greatest of. Every tag is text, the SQL sink writing a tag as
// TEXT NOT NULL, so tags are not listed. numberFields are the fields and
// columns of the same kind that hold numbers, flags or times. A name a panel
// sorts by that is in neither, and is no tag and no alias of its statement,
// stops byteOrder.
//
// A name is read the same in every measurement, and six are a tag of one and
// a number or a flag of another (active, attempt, labels, prerelease, private
// and public). Read as text, such a name sorting the second would be refused
// by PostgreSQL, a collation on a number being an error, which the
// containerised suite reports; it cannot be sorted by the wrong order
// unnoticed.
var (
	textFields = []string{
		"environment_url", "name", "oid", "referrer_url", "title", "url",
	}
	numberFields = []string{
		"answers", "blocks", "bypass_always", "churn", "comments",
		"commits", "contributions", "count", "days_since_change", "days_since_commit",
		"days_since_rotation", "days_since_update", "days_since_use", "downloads",
		"duration_seconds", "events", "health_percentage", "items", "limit", "live_bytes", "percent",
		"position", "price_per_unit", "progress", "queued_seconds", "remaining", "repos",
		"rules", "run_number", "runs", "seconds_open", "size_bytes", "stars", "time",
		"tier_number", "upvotes", "used", "used_ratio", "versions",
	}
)

// tagNames is every tag of every measurement, in every shape it has had.
var tagNames = func() map[string]bool {
	out := map[string]bool{}
	for _, set := range tags {
		for _, t := range set {
			out[t] = true
		}
	}
	for _, set := range formerTags {
		for _, t := range set {
			out[t] = true
		}
	}
	return out
}()

// sqlScan is what byteOrder reads of a statement: every select list and every
// ORDER BY, each with its position in the text, and every MIN and MAX
// argument. The scan runs over a mask of the statement in which everything
// inside quotes is blanked, so a keyword, a comma or a parenthesis inside a
// string literal or a quoted alias is not read as SQL.
type sqlScan struct {
	s, mask string
	// scope is, for every position, the innermost open parenthesis around it,
	// or -1 at the top level.
	scope    []int
	selects  []selectClause
	orders   []orderClause
	extremes [][2]int
	// typed is every name typed from textFields and numberFields, which is
	// how a test finds an entry no panel needs any more.
	typed map[string]bool
}

type selectClause struct {
	scope int
	items []selectColumn
}

// selectColumn is one expression of a select list: the span of its expression,
// without the alias, and the name the column goes by.
type selectColumn struct {
	expr [2]int
	name string
}

// orderClause is one ORDER BY. statement is true for a statement's own, whose
// keys can name an output column, and false for a window's or an
// aggregate's, whose keys are expressions.
type orderClause struct {
	scope     int
	statement bool
	keys      [][2]int
}

var (
	selectWord    = regexp.MustCompile(`\bSELECT (DISTINCT )?`)
	orderWord     = regexp.MustCompile(`\bORDER BY `)
	extremeWord   = regexp.MustCompile(`\b(MIN|MAX)\(`)
	sortDirection = regexp.MustCompile(`(?i)(\s+(ASC|DESC))?(\s+NULLS\s+(FIRST|LAST))?$`)
	// orderEnd is what ends an ORDER BY list at its own level, beside the
	// parenthesis that closes it and the end of the statement.
	orderEnd = []string{" LIMIT ", " OFFSET ", " ROWS ", " RANGE ", " UNION "}
)

func scanSQL(s string) *sqlScan {
	st := &sqlScan{s: s, mask: maskQuoted(s), typed: map[string]bool{}}
	st.scope = make([]int, len(s))
	var open []int
	for i := range len(st.mask) {
		switch st.mask[i] {
		case '(':
			st.scope[i] = innermost(open)
			open = append(open, i)
			continue
		case ')':
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		}
		st.scope[i] = innermost(open)
	}
	for _, m := range selectWord.FindAllStringIndex(st.mask, -1) {
		st.selects = append(st.selects, st.selectAt(m[0], m[1]))
	}
	for _, m := range orderWord.FindAllStringIndex(st.mask, -1) {
		if list, ok := st.orderAt(m[0], m[1]); ok {
			st.orders = append(st.orders, list)
		}
	}
	for _, m := range extremeWord.FindAllStringIndex(st.mask, -1) {
		open := m[1] - 1
		st.extremes = append(st.extremes, [2]int{open + 1, st.closing(open)})
	}
	return st
}

// sites is the span of every expression whose text order decides a row: each
// key of every ORDER BY, the output column's own expression where the key
// names one, and each argument of a MIN or a MAX.
func (st *sqlScan) sites() [][2]int {
	var out [][2]int
	for _, list := range st.orders {
		for _, key := range list.keys {
			if list.statement {
				if item, ok := st.outputColumn(list.scope, st.s[key[0]:key[1]]); ok {
					key = item
				}
			}
			out = append(out, key)
		}
	}
	return append(out, st.extremes...)
}

func innermost(open []int) int {
	if len(open) == 0 {
		return -1
	}
	return open[len(open)-1]
}

// maskQuoted blanks what is inside every string literal and quoted
// identifier, keeping the quotes and the length, with a doubled quote read
// as an escape as outsideQuotes reads it.
func maskQuoted(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		q := b[i]
		if q != '\'' && q != '"' {
			continue
		}
		j := i + 1
		for j < len(b) {
			if b[j] != q {
				b[j] = '_'
				j++
				continue
			}
			if j+1 < len(b) && b[j+1] == q {
				b[j], b[j+1] = '_', '_'
				j += 2
				continue
			}
			break
		}
		i = j
	}
	return string(b)
}

// closing is the parenthesis that closes the one open at open, which is the
// first after it back at its level, or the end of the statement for the top
// level or a parenthesis nothing closes.
func (st *sqlScan) closing(open int) int {
	if open < 0 {
		return len(st.mask)
	}
	for j := open + 1; j < len(st.mask); j++ {
		if st.mask[j] == ')' && st.scope[j] == st.scope[open] {
			return j
		}
	}
	return len(st.mask)
}

// level reports whether position j is at the level of scope and is not a
// parenthesis of its own.
func (st *sqlScan) level(j, scope int) bool {
	return st.scope[j] == scope && st.mask[j] != '(' && st.mask[j] != ')'
}

// listEnd is where a list that starts at from, inside scope, ends: at the
// first of the words given at its own level, or where its scope closes.
func (st *sqlScan) listEnd(from, scope int, words []string) int {
	end := st.closing(scope)
	for j := from; j < end; j++ {
		if !st.level(j, scope) {
			continue
		}
		for _, w := range words {
			if strings.HasPrefix(st.mask[j:], w) {
				return j
			}
		}
	}
	return end
}

// split cuts the span [from, to) at the commas of its own level and trims
// each part.
func (st *sqlScan) split(from, to, scope int) [][2]int {
	var out [][2]int
	start := from
	for j := from; j <= to; j++ {
		if j < to && (st.mask[j] != ',' || !st.level(j, scope)) {
			continue
		}
		a, b := start, j
		for a < b && st.mask[a] == ' ' {
			a++
		}
		for b > a && st.mask[b-1] == ' ' {
			b--
		}
		if a < b {
			out = append(out, [2]int{a, b})
		}
		start = j + 1
	}
	return out
}

func (st *sqlScan) selectAt(start, end int) selectClause {
	scope := st.scope[start]
	to := st.listEnd(end, scope, []string{" FROM ", " UNION "})
	list := selectClause{scope: scope}
	for _, item := range st.split(end, to, scope) {
		expr := item
		var name string
		if as := st.lastAt(item, " AS "); as >= 0 {
			expr = [2]int{item[0], as}
			name = strings.Trim(st.s[as+len(" AS "):item[1]], `"`)
		} else {
			name = bareName(strings.TrimSuffix(st.s[item[0]:item[1]], collateC))
		}
		list.items = append(list.items, selectColumn{expr: expr, name: name})
	}
	return list
}

// lastAt is the last position of w at the level of the span's start, or -1.
func (st *sqlScan) lastAt(span [2]int, w string) int {
	scope := st.scope[span[0]]
	for j := span[1] - len(w); j >= span[0]; j-- {
		if st.level(j, scope) && strings.HasPrefix(st.mask[j:], w) {
			return j
		}
	}
	return -1
}

func (st *sqlScan) orderAt(start, end int) (orderClause, bool) {
	scope := st.scope[start]
	list := orderClause{scope: scope, statement: scope < 0}
	if scope >= 0 {
		before := strings.TrimRight(st.mask[:scope], " ")
		switch {
		case strings.HasSuffix(before, "WITHIN GROUP"):
			// A percentile's argument, which is a number.
			return list, false
		case strings.HasSuffix(before, "OVER"):
		default:
			list.statement = st.selectOf(scope) >= 0
		}
	}
	to := st.listEnd(end, scope, orderEnd)
	for _, key := range st.split(end, to, scope) {
		m := sortDirection.FindStringIndex(st.mask[key[0]:key[1]])
		key[1] = key[0] + m[0]
		list.keys = append(list.keys, key)
	}
	return list, true
}

// selectOf is the first select list of a scope, the one whose columns the
// scope's ORDER BY names by position, or -1.
func (st *sqlScan) selectOf(scope int) int {
	for i, sel := range st.selects {
		if sel.scope == scope {
			return i
		}
	}
	return -1
}

// outputColumn is the expression of the output column a statement's ORDER BY
// key names by its position or by its name, if it names one.
func (st *sqlScan) outputColumn(scope int, key string) ([2]int, bool) {
	i := st.selectOf(scope)
	if i < 0 {
		return [2]int{}, false
	}
	items := st.selects[i].items
	if n, err := strconv.Atoi(key); err == nil {
		if n < 1 || n > len(items) {
			panic(fmt.Sprintf("ORDER BY %d of a list of %d columns: %s", n, len(items), st.s))
		}
		return items[n-1].expr, true
	}
	if strings.Contains(key, ".") {
		return [2]int{}, false
	}
	name := strings.Trim(key, `"`)
	for _, item := range items {
		if item.name == name {
			return item.expr, true
		}
	}
	return [2]int{}, false
}

var identifier = regexp.MustCompile(`^(?:\w+\.)?("?)(\w+)"?$`)

// bareName is the name of a column an unaliased expression is selected
// under, when the expression is a column.
func bareName(expr string) string {
	if m := identifier.FindStringSubmatch(strings.TrimSpace(expr)); m != nil {
		return m[2]
	}
	return ""
}

// textual reports whether an expression of the statement is text. seen is
// the aliases already being resolved, so that `MAX(stars) AS stars` reads
// its argument as the column rather than as itself.
func (st *sqlScan) textual(expr string, seen []string) bool {
	e := strings.TrimSpace(expr)
	m := maskQuoted(e)
	for strings.HasPrefix(m, "(") && closingIn(m, 0) == len(m)-1 {
		e, m = strings.TrimSpace(e[1:len(e)-1]), strings.TrimSpace(m[1:len(m)-1])
	}
	switch {
	case strings.HasSuffix(e, collateC):
		return true
	case strings.HasPrefix(e, "'") && strings.Count(m, "'") == 2 && strings.HasSuffix(e, "'"):
		return true
	case numeric(e), strings.HasPrefix(e, "$__"), e == "NULL":
		return false
	case strings.HasPrefix(m, "CASE ") && strings.HasSuffix(m, " END"):
		return st.caseTextual(e, m, seen)
	case topLevel(m, " || "):
		return true
	case topLevel(m, " + "), topLevel(m, " - "), topLevel(m, " * "), topLevel(m, " / "):
		return false
	}
	if fn, args, ok := callOf(e, m); ok {
		return st.callTextual(fn, args, e, seen)
	}
	if name := bareName(e); name != "" {
		return st.nameTextual(name, seen)
	}
	panic(fmt.Sprintf("byteOrder cannot tell whether %q is text: %s", e, st.s))
}

func numeric(e string) bool {
	_, err := strconv.ParseFloat(e, 64)
	return err == nil
}

// closingIn is the parenthesis closing the one at open in a masked string.
func closingIn(m string, open int) int {
	depth := 0
	for j := open; j < len(m); j++ {
		switch m[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// topLevel reports whether w occurs in a masked expression outside every
// parenthesis.
func topLevel(m, w string) bool {
	depth := 0
	for j := range len(m) {
		switch m[j] {
		case '(':
			depth++
		case ')':
			depth--
		default:
			if depth == 0 && strings.HasPrefix(m[j:], w) {
				return true
			}
		}
	}
	return false
}

var (
	callHead = regexp.MustCompile(`^(\w+)\(`)
	// What can follow a call and still leave the expression that call.
	callSuffixes = []string{"OVER (", "WITHIN GROUP (", "FILTER ("}
)

// callOf splits an expression that is one function call, optionally over a
// window, into the function's name and its argument text.
func callOf(e, m string) (fn, args string, ok bool) {
	h := callHead.FindStringSubmatch(m)
	if h == nil {
		return "", "", false
	}
	open := len(h[0]) - 1
	end := closingIn(m, open)
	if end < 0 {
		return "", "", false
	}
	if rest := strings.TrimSpace(m[end+1:]); rest != "" && !slices.ContainsFunc(callSuffixes, func(w string) bool {
		return strings.HasPrefix(rest, w)
	}) {
		return "", "", false
	}
	return strings.ToUpper(h[1]), e[open+1 : end], true
}

var (
	numberFunctions = []string{
		"ABS", "AVG", "CEIL", "COUNT", "DATE_BIN", "DATE_PART", "DATE_TRUNC", "DENSE_RANK",
		"EXTRACT", "FLOOR", "LENGTH", "NOW", "PERCENTILE_CONT", "RANK", "ROUND", "ROW_NUMBER",
		"SUM", "TO_TIMESTAMP",
	}
	textFunctions = []string{
		"CONCAT", "LEFT", "LOWER", "REPLACE", "RIGHT", "SPLIT_PART", "STRING_AGG", "SUBSTR",
		"SUBSTRING", "TO_CHAR", "TRIM", "UPPER",
	}
	// Functions whose value is of their first argument's type.
	firstArgFunctions = []string{
		"COALESCE", "FIRST_VALUE", "GREATEST", "LAST_VALUE", "LEAST", "MAX", "MIN", "NULLIF",
	}
	castTo = regexp.MustCompile(`(?i) AS (\w+)(\s*\(\d+\))?$`)
)

func (st *sqlScan) callTextual(fn, args, e string, seen []string) bool {
	switch {
	case slices.Contains(numberFunctions, fn):
		return false
	case slices.Contains(textFunctions, fn):
		return true
	case slices.Contains(firstArgFunctions, fn):
		first := args
		if parts := splitTop(args); len(parts) > 0 {
			first = parts[0]
		}
		return st.textual(first, seen)
	case fn == "CAST":
		to := castTo.FindStringSubmatch(maskQuoted(args))
		if to == nil {
			break
		}
		switch strings.ToUpper(to[1]) {
		case "TEXT", "VARCHAR", "CHAR", "STRING":
			return true
		default:
			return false
		}
	}
	panic(fmt.Sprintf("byteOrder cannot tell whether %q is text: %s", e, st.s))
}

// splitTop cuts an argument list at the commas outside every parenthesis and
// quote.
func splitTop(args string) []string {
	m := maskQuoted(args)
	var out []string
	depth, start := 0, 0
	for j := range len(m) {
		switch m[j] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, args[start:j])
				start = j + 1
			}
		}
	}
	return append(out, args[start:])
}

var caseArm = regexp.MustCompile(` (THEN|ELSE) `)

// caseTextual types a CASE by its results: text if any of them is.
func (st *sqlScan) caseTextual(e, m string, seen []string) bool {
	// Only the arms of this CASE, not of one nested in it.
	var bounds []int
	for _, loc := range caseArm.FindAllStringIndex(m, -1) {
		if depthAt(m, loc[0]) == 0 {
			bounds = append(bounds, loc[1])
		}
	}
	for _, from := range bounds {
		to := len(m) - len(" END")
		for _, w := range []string{" WHEN ", " ELSE "} {
			if at := indexTop(m, w, from); at >= 0 && at < to {
				to = at
			}
		}
		if st.textual(e[from:to], seen) {
			return true
		}
	}
	return false
}

func depthAt(m string, at int) int {
	return strings.Count(m[:at], "(") - strings.Count(m[:at], ")")
}

// indexTop is the first occurrence of w at or after from outside every
// parenthesis, or -1.
func indexTop(m, w string, from int) int {
	for j := from; j < len(m); j++ {
		if depthAt(m, j) == 0 && strings.HasPrefix(m[j:], w) {
			return j
		}
	}
	return -1
}

// nameTextual types a column by its name: by what it is defined as where the
// statement defines it with AS, and otherwise as a tag or one of the fields
// listed above. A name the statement defines and the tables also hold is
// typed both ways, and the two have to agree.
func (st *sqlScan) nameTextual(name string, seen []string) bool {
	var kinds []bool
	if !slices.Contains(seen, name) {
		for _, sel := range st.selects {
			for _, item := range sel.items {
				expr := st.s[item.expr[0]:item.expr[1]]
				if item.name == name && bareName(strings.TrimSuffix(expr, collateC)) != name {
					kinds = append(kinds, st.textual(expr, append(seen, name)))
				}
			}
		}
	}
	switch {
	case tagNames[name]:
		kinds = append(kinds, true)
	case slices.Contains(textFields, name):
		kinds = append(kinds, true)
		st.typed[name] = true
	case slices.Contains(numberFields, name):
		kinds = append(kinds, false)
		st.typed[name] = true
	}
	if len(kinds) == 0 {
		panic(fmt.Sprintf("byteOrder cannot tell whether %s is text: name it in textFields or "+
			"numberFields: %s", name, st.s))
	}
	for _, k := range kinds[1:] {
		if k != kinds[0] {
			panic(fmt.Sprintf("byteOrder reads %s as text in one place and not in another: %s", name, st.s))
		}
	}
	return kinds[0]
}
