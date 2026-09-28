package dashboards

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEverySQLListOrdersItsRowsCompletely holds each statement a table, a bar
// chart, a pie or a bar gauge draws rows from to an ORDER BY that leaves no
// two of them tied, as far as the statement says what a row is:
//
//   - a statement that groups names every grouping key, since those are what
//     tells two of its rows apart;
//   - one that lists the newest row of each item, by a ROW_NUMBER over a
//     partition kept at rn = 1, names every key of that partition;
//   - a rank without a partition, the one that folds the smallest slices of
//     a pie into "other", names something after the value it ranks by.
//
// A tie left in the ORDER BY is broken by each store's sort, and InfluxDB and
// PostgreSQL break it differently, which the containerised suite measured on
// as many as twelve panels: the same rows in another order, and with a LIMIT
// or a fold, possibly other rows. The key a row shows is not always its identity, so a
// repository is held to its full name, of which its short name and its owner
// are a part. A list of raw rows says nothing a test can read about what a row
// is, and TestTheSQLStoresDrawTheRowsInOneOrder in the containerised suite is
// what holds those.
func TestEverySQLListOrdersItsRowsCompletely(t *testing.T) {
	t.Parallel()
	held := 0
	for _, p := range renderedPanels(t, "influxdb") {
		switch p["type"] {
		case "table", "barchart", "piechart", "bargauge":
		default:
			continue
		}
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			sql, _ := target["rawSql"].(string)
			if sql == "" {
				continue
			}
			held++
			for _, gap := range orderGaps(sql) {
				t.Errorf("%q %s, and each store breaks the tie its own way:\n%s", p["title"], gap, sql)
			}
		}
	}
	if held < 50 {
		t.Errorf("only %d statements were read, so this held almost nothing", held)
	}
}

// TestTheOrderReaderFindsWhatItLooksFor keeps orderGaps honest on statements
// whose answer is known, so the test above passing is about the dashboards.
func TestTheOrderReaderFindsWhatItLooksFor(t *testing.T) {
	t.Parallel()
	const (
		newest    = `(SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn FROM t) x WHERE rn = 1`
		newestPR  = `(SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, number ORDER BY time DESC) AS rn FROM t) x`
		repoFlags = ` LEFT JOIN (SELECT full_name, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn` +
			` FROM u) f ON f.full_name = x.full_name AND f.rn = 1`
		ranked = `(SELECT type AS "T", ROW_NUMBER() OVER (ORDER BY SUM(events) DESC%s) AS rn FROM t GROUP BY 1) z`
	)
	for _, c := range []struct {
		sql  string
		gaps int
	}{
		{`SELECT author AS "Author", COUNT(*) AS n FROM t GROUP BY 1 ORDER BY 2 DESC LIMIT 15`, 1},
		{`SELECT author AS "Author", COUNT(*) AS n FROM t GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 15`, 0},
		{`SELECT repo AS "R", COUNT(*) AS n FROM t GROUP BY full_name, repo ORDER BY 2 DESC`, 1},
		{`SELECT repo AS "R", COUNT(*) AS n FROM t GROUP BY full_name, repo ORDER BY 2 DESC, full_name`, 0},
		{`SELECT step AS "S", COUNT(*) AS n, repo AS "R" FROM t GROUP BY 1, full_name, 3 ORDER BY 2 DESC, 1`, 1},
		{`SELECT step AS "S", COUNT(*) AS n, repo AS "R" FROM t GROUP BY 1, r.full_name, 3 ORDER BY 2 DESC, 1, r.full_name`, 0},
		{`SELECT repo AS "R", n AS "N" FROM ` + newest + ` ORDER BY 2 DESC`, 1},
		{`SELECT repo AS "R", n AS "N" FROM ` + newest + ` ORDER BY 2 DESC, full_name`, 0},
		{`SELECT x.number AS "N" FROM ` + newestPR + repoFlags + ` WHERE x.rn = 1 ORDER BY x.seconds_open DESC, x.full_name`, 1},
		{`SELECT x.number AS "N" FROM ` + newestPR + repoFlags + ` WHERE x.rn = 1 ORDER BY x.seconds_open DESC, x.full_name, 1`, 0},
		{`SELECT "T", "E" FROM (SELECT "T", MIN(rn) AS o FROM ` + fmt.Sprintf(ranked, "") + ` GROUP BY 1) w ORDER BY o`, 1},
		{`SELECT "T", "E" FROM (SELECT "T", MIN(rn) AS o FROM ` + fmt.Sprintf(ranked, ", type") + ` GROUP BY 1) w ORDER BY o`, 0},
		{`SELECT repo AS "R", stars AS "S" FROM (SELECT repo, full_name, stars FROM ` + newest + `) ORDER BY stars DESC`, 1},
		{`SELECT user AS "User", time AS "At" FROM gh_star WHERE x ORDER BY time DESC LIMIT 50`, 0},
	} {
		if got := orderGaps(c.sql); len(got) != c.gaps {
			t.Errorf("%s: found %v, want %d", c.sql, got, c.gaps)
		}
	}
}

// orderGaps is every way a statement's ORDER BY can leave two of its rows
// tied, by the rules TestEverySQLListOrdersItsRowsCompletely gives.
func orderGaps(sql string) []string {
	q := blankLiterals(sql)
	var gaps []string
	for _, keys := range rankOrders(q) {
		if len(keys) < 2 {
			gaps = append(gaps, fmt.Sprintf("ranks by %s alone", strings.Join(keys, ", ")))
		}
	}
	outer, groups := foldParens(q)
	at := strings.LastIndex(outer, " ORDER BY ")
	if at < 0 {
		return gaps
	}
	order := orderKeys(outer[at+len(" ORDER BY "):])
	identity, what := rowIdentity(outer[:at], groups)
	selected := selectedExprs(outer[:at])
	for _, key := range identity {
		// The short name and the owner are the full name's to name, and a
		// gap in it is said once, under the full name.
		if identityOf[resolveKey(key, selected)] && slices.ContainsFunc(identity, func(k string) bool {
			return resolveKey(k, selected) == "full_name"
		}) {
			continue
		}
		if !orderNames(order, selected, key) {
			gaps = append(gaps, fmt.Sprintf("orders by %s without the %s %s",
				strings.Join(order, ", "), what, key))
		}
	}
	return gaps
}

// rowIdentity is what tells two rows of a statement apart, read from its
// outermost level: the grouping keys, or the partition of the newest row of
// each item, looked for through a subquery that only passes rows on. A list
// of raw rows answers nothing.
func rowIdentity(head string, groups []string) (keys []string, what string) {
	if at := strings.LastIndex(head, " GROUP BY "); at >= 0 {
		list, _, _ := strings.Cut(head[at+len(" GROUP BY "):], " HAVING ")
		return splitKeys(list), "grouping key"
	}
	from := strings.Index(head, " FROM ")
	if from < 0 {
		return nil, ""
	}
	where := strings.LastIndex(head, " WHERE ")
	if m := newestRow.FindStringSubmatch(head[max(where, 0):]); where > from && m != nil {
		sub := subqueryAliased(head[from:], groups, strings.Count(head[:from], "§"), m[1])
		if p := partitionBy.FindStringSubmatch(sub); p != nil {
			return splitKeys(p[1]), "partition key"
		}
		return nil, ""
	}
	// SELECT ... FROM (...) with nothing after it but an alias: the rows are
	// the subquery's, and so is what tells them apart.
	rest := strings.TrimSpace(head[from+len(" FROM "):])
	if strings.HasPrefix(rest, "§") && !strings.ContainsAny(strings.TrimSpace(rest[len("§"):]), " §") {
		sub := groups[strings.Count(head[:from], "§")]
		inner, innerGroups := foldParens(sub)
		// Only a partition, whose keys are columns the outer statement can
		// name too; a grouping key is a position in the inner select list.
		if inherited, how := rowIdentity(inner, innerGroups); how == "partition key" {
			return inherited, how
		}
	}
	return nil, ""
}

var (
	newestRow   = regexp.MustCompile(`\bWHERE (?:.* )?(?:(\w+)\.)?rn = 1\b`)
	partitionBy = regexp.MustCompile(`\bPARTITION BY (.+?) ORDER BY `)
	sortSuffix  = regexp.MustCompile(`(?i)\s+(ASC|DESC)(\s+NULLS\s+(FIRST|LAST))?$`)
	aliasPrefix = regexp.MustCompile(`^\w+\.`)
	identityOf  = map[string]bool{"repo": true, "owner": true}
)

// subqueryAliased is the text of the subquery of a FROM clause that carries
// the alias, or the first one when there is no alias to look for. before is
// how many subqueries the statement holds ahead of the FROM clause.
func subqueryAliased(from string, groups []string, before int, alias string) string {
	n := before
	for i := range len(from) {
		if !strings.HasPrefix(from[i:], "§") {
			continue
		}
		after := strings.TrimSpace(from[i+len("§"):])
		if alias == "" || strings.HasPrefix(after, alias+" ") || after == alias {
			return groups[n]
		}
		n++
	}
	return ""
}

// orderNames reports whether an ORDER BY names a key of the row's identity,
// by the expression, by its position in the select list, or, for the short
// name and the owner, by the full name they are part of.
func orderNames(order, selected []string, key string) bool {
	want := resolveKey(key, selected)
	for _, k := range order {
		if k == key || resolveKey(k, selected) == want {
			return true
		}
	}
	if identityOf[want] {
		prefix := aliasPrefix.FindString(strings.TrimSpace(key))
		return orderNames(order, selected, prefix+"full_name")
	}
	return false
}

// resolveKey is what a key of an ORDER BY or a GROUP BY names: the expression
// at its position in the select list, or itself, without a table's alias.
func resolveKey(key string, selected []string) string {
	if n, err := strconv.Atoi(key); err == nil && n >= 1 && n <= len(selected) {
		key = selected[n-1]
	}
	return aliasPrefix.ReplaceAllString(strings.TrimSpace(key), "")
}

// selectedExprs is the expressions of the outermost SELECT of a statement
// whose parentheses are folded, each without its alias.
func selectedExprs(head string) []string {
	list, ok := strings.CutPrefix(strings.TrimSpace(head), "SELECT ")
	if !ok {
		return nil
	}
	list, _, _ = strings.Cut(list, " FROM ")
	list = strings.TrimPrefix(list, "DISTINCT ")
	var out []string
	for item := range strings.SplitSeq(list, ",") {
		expr, _, _ := strings.Cut(strings.TrimSpace(item), " AS ")
		out = append(out, strings.TrimSpace(expr))
	}
	return out
}

// orderKeys is an ORDER BY list, each key without its direction.
func orderKeys(list string) []string {
	list, _, _ = strings.Cut(list, " LIMIT ")
	return splitKeys(list)
}

func splitKeys(list string) []string {
	var out []string
	for key := range strings.SplitSeq(list, ",") {
		out = append(out, sortSuffix.ReplaceAllString(strings.TrimSpace(key), ""))
	}
	return out
}

// rankOrders is the ORDER BY of every ROW_NUMBER, RANK and DENSE_RANK taken
// over no partition.
func rankOrders(q string) [][]string {
	var out [][]string
	for _, fn := range []string{"ROW_NUMBER() OVER (ORDER BY ", "RANK() OVER (ORDER BY "} {
		for rest := q; ; {
			at := strings.Index(rest, fn)
			if at < 0 {
				break
			}
			rest = rest[at+len(fn):]
			depth, end := 0, len(rest)
		scan:
			for i, r := range rest {
				switch r {
				case '(':
					depth++
				case ')':
					if depth == 0 {
						end = i
						break scan
					}
					depth--
				}
			}
			folded, _ := foldParens(rest[:end])
			out = append(out, splitKeys(folded))
		}
	}
	return out
}

// blankLiterals empties every string literal and keeps every quoted
// identifier, with what in it could read as SQL, a comma or a parenthesis,
// turned into an underscore: blankQuoted empties both, and two columns named
// "Type" and "Events" would then be one column here.
func blankLiterals(s string) string {
	var out strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
			out.WriteByte(c)
		case quote != 0 && c == quote && i+1 < len(s) && s[i+1] == quote:
			i++
		case quote != 0 && c == quote:
			quote = 0
			out.WriteByte(c)
		case quote == '\'':
		case quote == '"' && strings.IndexByte(",()", c) >= 0:
			out.WriteByte('_')
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// foldParens is a statement with every outermost pair of parentheses and what
// they hold folded to one character, and what each pair held, in order.
func foldParens(s string) (outer string, groups []string) {
	var o, g strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '(':
			if depth == 0 {
				o.WriteString("§")
				g.Reset()
			} else {
				g.WriteRune(r)
			}
			depth++
		case r == ')' && depth > 0:
			depth--
			if depth == 0 {
				groups = append(groups, g.String())
			} else {
				g.WriteRune(r)
			}
		case depth == 0:
			o.WriteRune(r)
		default:
			g.WriteRune(r)
		}
	}
	return o.String(), groups
}
