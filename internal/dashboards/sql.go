package dashboards

import (
	"fmt"
	"strings"
)

// ── SQL helpers ─────────────────────────────────────────────────────────────

// timeBin is the bucket every dated timeseries groups by. $__dateBin is the
// InfluxDB plugin's own macro: it expands to a date_bin over the interval
// Grafana computed for the panel from the range, the panel's `interval` floor
// and its `maxDataPoints` (see binned in panels.go), so two years draw as
// weeks where a fixed one-day bin drew 730 bars of one pixel. A literal
// `${__interval_ms}` in the SQL is not an alternative: the browser would
// substitute it, but /api/ds/query, which is what the checkers and the
// containerised suite post to, leaves it as text and InfluxDB rejects the
// interval (measured). toPG translates the macro for PostgreSQL.
const timeBin = "$__dateBin(time) AS time"

// The panel options that pair with timeBin: the floor the interval cannot go
// under, which is the resolution the rows were written at.
var (
	dayBins  = Opts{"interval": "1d"}
	hourBins = Opts{"interval": "1h"}
)

// topSeries is a stacked sum per bucket kept to the biggest values of one
// tag over the range, the rest folded into one series called `other`. A stack
// with a series per repository put thirty names in the legend and hid the
// busiest one under the panel's edge; eight names and a remainder is what a
// reader can tell apart. The ranking is over the same rows the stack sums,
// so the eight are the eight biggest in the range shown, not in all time.
//
// The rank is a window over the bucketed aggregate rather than a join back to
// the table: the shared repository filter names `repo` unqualified, and a
// self-join makes that reference ambiguous. The tag breaks the tie so that
// two equal totals still make two ranks and the cut stays where it is set.
// HAVING drops the buckets a series had nothing in, so a stacked bar of
// zeros is not a legend entry.
func topSeries(table, tag, field, alias, where string) string {
	return fmt.Sprintf("SELECT time, CASE WHEN rk <= %d THEN %s ELSE 'other' END AS series,"+
		" SUM(v) AS %s FROM (SELECT time, %s, v, DENSE_RANK() OVER (ORDER BY total DESC, %s) AS rk"+
		" FROM (SELECT %s, %s, SUM(%s) AS v, SUM(SUM(%s)) OVER (PARTITION BY %s) AS total"+
		" FROM %s WHERE $__timeFilter(time) AND %s GROUP BY 1, 2) y) x"+
		" GROUP BY 1, 2 HAVING SUM(v) > 0 ORDER BY 1",
		topSeriesKept, tag, alias, tag, tag, timeBin, tag, field, field, tag, table, where)
}

// topSeriesKept is how many series a stacked panel names before folding the
// rest: eight names and a remainder is what a legend a phone wide can show.
const topSeriesKept = 8

// agoSQL is a timestamp column less a column of seconds, which is how a row
// dated when it was last seen gives the day it was opened. DataFusion refuses
// an integer times an interval, so the seconds are cast to a duration; toPG
// writes the PostgreSQL form.
func agoSQL(ts, seconds string) string {
	return fmt.Sprintf("%s - arrow_cast(%s * 1000000000, 'Duration(Nanosecond)')", ts, seconds)
}

// otherRows folds a ranked list into its first n rows and one row named
// `other` holding the sum of the rest, which is what keeps a bar chart of
// twenty two repositories readable. `ranked` is a statement whose columns are
// the label, the value and `rn`, the row's rank by that value; the two column
// names are what the panel calls them. `other` sorts last because it takes
// the smallest rank of what it folded, which is n+1; the rank is carried out
// of the grouping as its own column and dropped by the outer select, since
// DataFusion will not order by an aggregate the projection has not kept.
func otherRows(ranked, label, value string, n int) string {
	return fmt.Sprintf(`SELECT "%s", "%s" FROM (SELECT CASE WHEN rn <= %d THEN "%s" ELSE 'other' END AS "%s",`+
		` SUM("%s") AS "%s", MIN(rn) AS o FROM (%s) z GROUP BY 1) w ORDER BY o`,
		label, value, n, label, label, value, value, ranked)
}

// latestPerRepo is the most recent row of each repository inside the selected
// range.
//
// gh_repo is a snapshot rewritten every sweep, so summing it directly would
// count each repository once per sweep. This keeps one row each.
func latestPerRepo(fields []string) string {
	return latestPerRepoWithin(fields, "$__timeFilter(time)")
}

// latestPerRepoWithin is the same with the window named, for the one caller
// that must not take the page's: see repoFlagsJoin.
func latestPerRepoWithin(fields []string, window string) string {
	return fmt.Sprintf("SELECT repo, %s FROM ("+
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo ORDER BY time DESC) AS rn"+
		" FROM gh_repo WHERE %s AND %s) x WHERE rn = 1",
		strings.Join(fields, ", "), window, RF)
}

// latestSumSQL is the sum of a snapshot field, one row per partition, newest
// wins.
func latestSumSQL(table, field string, partition ...string) string {
	p := "repo"
	if len(partition) > 0 {
		p = partition[0]
	}
	return fmt.Sprintf("SELECT SUM(%s) AS value FROM (SELECT %s,"+
		" ROW_NUMBER() OVER (PARTITION BY %s ORDER BY time DESC) AS rn"+
		" FROM %s WHERE $__timeFilter(time) AND %s) x WHERE rn = 1",
		field, field, p, table, RF)
}

// jobLogSelector is the stream the Loki sink writes the job logs to: its
// default `job` label and the kind lokiEvents gives gh_job_log.
const jobLogSelector = `{job="ghchronicle", kind="job_log"}`

// The one thing an exported dashboard cannot show. Job logs are text, so the
// InfluxDB sink excludes them by default and they go to a log store instead;
// a dashboard bound to one datasource cannot query two, and an importer may
// have no Loki. A dashboard built with one (Render's `logs`, the -loki flag
// of cmd/publish_dashboard) draws the lines in this panel's place.
const logNote = `### Failed job output

The last lines of every job that failed are collected too, but they are text
rather than measurements, so they go to a log store rather than here. With the
Loki sink enabled, this is the query:

` + "```" + `
` + jobLogSelector + ` |= "error"
` + "```" + `

Set ` + "`every.joblogs`" + ` to switch the collection on. GitHub deletes job logs after
the repository's retention period, ninety days by default, and answers 410
afterwards, so an older log cannot be backfilled: what exists is what was
captured while it was there.

Published to a Grafana that has a Loki datasource, with
` + "`cmd/publish_dashboard -loki <datasource-uid>`" + `, this panel shows those lines
instead of this note.
`

// ── The repository's own flags ──────────────────────────────────────────────

// repoFlagsJoin hangs `fork` and `archived` on a table of something else,
// under the alias `f`, matched on the repository name. `on` is the column of
// the outer query the repository is named by.
//
// Only gh_repo carries either: both are tags there and no other measurement
// has them at all. Reading them costs a join, which is why the panels that
// need them are the two SQL dashboards and why the other three say instead
// that they cannot.
//
// What they are for: with 59 repositories in the picker, 17 of them archived
// and 22 of them forks of other people's projects, every "oldest", "stalest"
// and "never" sort on the page was won by something nothing can be done to. A
// workflow declared in an archived repository cannot run, by definition; a
// pull request in one cannot be merged; a branch of a fork is upstream's
// history and not the account's. Measured on 2026-09-17: the eight oldest open
// pull requests were dependabot's in two archived repositories, every workflow
// in "Workflows that never ran" was in an archived one, and 1,083 of the 1,208
// branches in "Stale branches" were a fork's.
// The lookup takes wholeHistory and never the page's range, which is the whole
// difference between a fix and a fix that switches itself off. gh_repo is a
// snapshot, so a repository's flags are only knowable where a sweep landed: on
// a range holding none the join matches nothing, COALESCE reads every missing
// flag as "not flagged", and all four panels quietly revert to listing what
// nobody can act on. That is not a corner: measured on the production store on
// 2026-09-17, gh_repo holds 59 rows at exactly one timestamp, so every range
// that does not contain the last sweep has no flags at all, and which ranges
// those are moves with the clock. The scan is free, which is what lets the
// window be the unbounded one: one parquet file and 59 rows
// (system.parquet_files, same day) against the forty thousand file limit the
// other whole-history panels are measured against.
func repoFlagsJoin(on string) string {
	return " LEFT JOIN (" +
		latestPerRepoWithin([]string{"fork", "archived"}, wholeHistory) +
		") f ON f.repo = " + on
}

// The predicates that go with it.
//
// A LEFT JOIN answers NULL where the repository has no gh_repo row inside the
// range, and a PostgreSQL tag column is the empty string where nothing was
// written. Both read as "not flagged" here rather than as a row to drop: a
// row whose repository is unknown is not one the reader can be told to ignore.
const (
	notArchived = "COALESCE(f.archived, 'false') <> 'true'"
	notAFork    = "COALESCE(f.fork, 'false') <> 'true'"
)
