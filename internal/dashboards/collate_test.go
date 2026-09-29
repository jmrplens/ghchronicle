package dashboards

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// withoutCollation is a PostgreSQL statement without the collations
// byteOrder gave it, for a test about which keys a statement names rather
// than about how PostgreSQL compares them.
func withoutCollation(sql string) string { return strings.ReplaceAll(sql, collateC, "") }

// TestThePostgreSQLTranslationComparesTextByItsBytes: InfluxDB compares text
// by its bytes and PostgreSQL by the database's collation, which on a Debian
// PostgreSQL put "(ghost)" after "alice" and "VALID" after "unsigned" in the
// panels the first case is taken from. Each case is the InfluxDB statement
// and its PostgreSQL translation.
func TestThePostgreSQLTranslationComparesTextByItsBytes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ why, in, want string }{
		{
			"a key by position takes the collation on the column it names, which PostgreSQL " +
				"refuses on a position; the count beside it is a number and is left alone",
			`SELECT author AS "Author", COUNT(*) AS "Pull requests" FROM gh_pull_request GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 15`,
			`SELECT author COLLATE "C" AS "Author", COUNT(*) AS "Pull requests" FROM gh_pull_request GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 15`,
		},
		{
			"a CASE is text when one of its results is, and a string literal inside one is not SQL",
			`SELECT CASE WHEN bot = 'true' THEN reviewer || ' (bot, ORDER BY 1)' ELSE reviewer END AS "Reviewer", ` +
				`COUNT(*) AS n FROM gh_pull_request_review GROUP BY 1 ORDER BY 2 DESC, 1`,
			`SELECT CASE WHEN bot = 'true' THEN reviewer || ' (bot, ORDER BY 1)' ELSE reviewer END COLLATE "C" AS "Reviewer", ` +
				`COUNT(*) AS n FROM gh_pull_request_review GROUP BY 1 ORDER BY 2 DESC, 1`,
		},
		{
			"a qualified column is collated where it stands, every tag being text",
			`SELECT x.number AS "Number" FROM (SELECT * FROM gh_issue) x ORDER BY x.seconds_open DESC, x.full_name, x.number LIMIT 25`,
			`SELECT x.number AS "Number" FROM (SELECT * FROM gh_issue) x ORDER BY x.seconds_open DESC, x.full_name COLLATE "C", x.number COLLATE "C" LIMIT 25`,
		},
		{
			"a window's ORDER BY decides which row is kept, and the time before the text is left alone",
			`SELECT * FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, number ORDER BY time DESC, state) AS rn FROM gh_x) x WHERE rn = 1`,
			`SELECT * FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, number ORDER BY time DESC, state COLLATE "C") AS rn FROM gh_x) x WHERE rn = 1`,
		},
		{
			"an alias is typed by what defines it: total is a sum",
			`SELECT DENSE_RANK() OVER (ORDER BY total DESC, full_name) AS rk FROM (SELECT full_name, SUM(SUM(count)) OVER (PARTITION BY full_name) AS total FROM gh_x GROUP BY 1) y`,
			`SELECT DENSE_RANK() OVER (ORDER BY total DESC, full_name COLLATE "C") AS rk FROM (SELECT full_name, SUM(SUM(count)) OVER (PARTITION BY full_name) AS total FROM gh_x GROUP BY 1) y`,
		},
		{
			"the greatest of a text is the greatest by bytes; of a number, a flag or a row number it is left alone",
			`SELECT MAX(title) AS t, MAX(stars) AS stars, MIN(rn) AS o FROM (SELECT *, ROW_NUMBER() OVER (ORDER BY stars DESC, name) AS rn FROM gh_x) x`,
			`SELECT MAX(title COLLATE "C") AS t, MAX(stars) AS stars, MIN(rn) AS o FROM (SELECT *, ROW_NUMBER() OVER (ORDER BY stars DESC, name COLLATE "C") AS rn FROM gh_x) x`,
		},
		{
			"a DISTINCT list refuses an ORDER BY expression it does not select, so a key naming an " +
				"output column collates the column",
			`SELECT DISTINCT repo, full_name FROM gh_x ORDER BY full_name`,
			`SELECT DISTINCT repo, full_name COLLATE "C" FROM gh_x ORDER BY full_name`,
		},
		{
			"a number made of a flag, and a percentile, are not text",
			`SELECT CAST(active AS INT) AS "Active", median(CAST(duration_seconds AS DOUBLE)) AS d FROM gh_x GROUP BY 1 ORDER BY 1, 2`,
			`SELECT CAST(active AS INT) AS "Active", percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_seconds) AS d FROM gh_x GROUP BY 1 ORDER BY 1, 2`,
		},
		{
			"the time bucket is a macro for a time",
			`SELECT $__dateBin(time) AS time, COUNT(*) AS n FROM gh_x WHERE $__timeFilter(time) GROUP BY 1 ORDER BY 1`,
			`SELECT $__timeGroupAlias(time, $__interval), COUNT(*) AS n FROM gh_x WHERE $__timeFilter(time) GROUP BY 1 ORDER BY 1`,
		},
	} {
		if got := toPG(c.in); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.why, got, c.want)
		}
		if again := byteOrder(c.want); again != c.want {
			t.Errorf("%s: a second pass changed it:\n%s", c.why, again)
		}
	}
}

// TestThePostgreSQLTranslationStopsAtAKeyItCannotType: a key read as a
// number would be compared by the database's collation if it held text, and
// one read as text would be refused if it held a number, so a name the
// translation has no type for stops the build instead of guessing.
func TestThePostgreSQLTranslationStopsAtAKeyItCannotType(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		`SELECT mystery FROM gh_x ORDER BY mystery`,
		`SELECT MAX(mystery) AS m FROM gh_x`,
		`SELECT x FROM gh_x ORDER BY some_function(x)`,
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "cannot tell whether") {
					t.Errorf("%s: want a stop naming the key, got %v", sql, r)
				}
			}()
			toPG(sql)
		}()
	}
}

// TestTheMeasuredPanelsCompareTextByItsBytes pins the three panels that drew
// their rows in another order in PostgreSQL 18.6 on Debian than in InfluxDB,
// with the containerised suite's sweep, and keeps the InfluxDB dialect, which
// has no COLLATE, free of it.
func TestTheMeasuredPanelsCompareTextByItsBytes(t *testing.T) {
	t.Parallel()
	pg := rendered(t, "postgres")
	for title, want := range map[string]string{
		"Pull requests by author": `author COLLATE "C" AS "Author"`,
		"Reviewers":               `END COLLATE "C" AS "Reviewer"`,
		"Commits by signature":    `signature COLLATE "C" AS "Signature"`,
	} {
		if sql := allSQL(mustPanel(t, pg, title)); !strings.Contains(sql, want) {
			t.Errorf("postgres: %q does not compare its label by bytes, want %s in:\n%s", title, want, sql)
		}
	}
	for _, p := range renderedPanels(t, "influxdb") {
		if sql := allSQL(p); strings.Contains(sql, "COLLATE") {
			t.Errorf("influxdb: %q names a collation, which InfluxDB refuses:\n%s", p["title"], sql)
		}
	}
}

// TestEveryListedFieldIsOneAPanelSortsBy keeps textFields and numberFields to
// what the dashboards need, the way the tag table is kept to its collectors.
func TestEveryListedFieldIsOneAPanelSortsBy(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, p := range renderedPanels(t, "postgres") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			sql, _ := target["rawSql"].(string)
			st := scanSQL(withoutCollation(sql))
			for _, site := range st.sites() {
				st.textual(st.s[site[0]:site[1]], nil)
			}
			for name := range st.typed {
				used[name] = true
			}
		}
	}
	for _, name := range append(slices.Clone(textFields), numberFields...) {
		if !used[name] {
			t.Errorf("%s is listed for byteOrder and no panel sorts by it or takes its least or greatest", name)
		}
	}
	for _, name := range textFields {
		if slices.Contains(numberFields, name) || tagNames[name] {
			t.Errorf("%s is listed twice", name)
		}
	}
}
