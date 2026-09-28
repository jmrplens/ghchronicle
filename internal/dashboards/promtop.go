package dashboards

import (
	"fmt"
	"strings"
)

// Capping a Prometheus table.
//
// A SQL table ends in LIMIT n and shows n rows. Its Prometheus twin has no
// such clause: `sum by (author) (increase(github_commits_total[$__range]))`
// returns a row per author that ever existed. On the account this dashboard
// was measured against that was fine until the backfill brought in the forks,
// and then the review of 2026-09-14 counted 12,826 series in Commits by
// author, of which 12,824 were exactly zero, sorted alphabetically because
// the value did not separate them; the owner's own name was the third row.
// The same shape put 878 series in Workflows, 688 in Workflows that keep
// failing, 543 in Stale branches, 496 in Slowest jobs and 291 in Labels.
//
// topk is the cap. The subtlety is a table built from several queries merged
// on their labels: capping each one separately picks a different top set per
// column, and the merge then shows rows whose other columns are empty. So one
// query ranks and the rest are intersected onto the rows it kept.

// promTop is the ranking query of a capped table: its own n biggest series.
func promTop(n int, expr string) string {
	return fmt.Sprintf("topk(%d, %s)", n, expr)
}

// promWithin is a companion query of a capped table, kept to the series the
// ranking query keeps. `on` is the label set the two share, which is the same
// set the merge transformation joins them by.
func promWithin(n int, expr, rank string, on ...string) string {
	return fmt.Sprintf("%s and on (%s) %s", expr, strings.Join(on, ", "), promTop(n, rank))
}

// promOther is otherRows in Prometheus: the n biggest series of a sum by one
// label, and the rest added up as one series whose label reads other. The rest
// is the whole sum less the n, which is there only when the sum has more than
// n series, so a range of eight types or fewer has no other slice, as the SQL
// has no row for it. A tie at the cut is broken however topk breaks it, which
// is not by name as the SQL's ORDER BY breaks it.
//
// Only the series above 0 are ranked and counted. The SQL has a row for a
// value that had something in the range and none for one that had nothing,
// and the exporter keeps every series it has seen and republishes it, so an
// increase over a range where a type had no event is a series of 0. Counted,
// nine types ever seen made an other of 0 over a range where two had events,
// and ranked, six of the zeros were named slices beside them (measured with
// promtool of Prometheus 3.14.0).
func promOther(n int, expr, label string) string {
	kept := "(" + expr + ") > 0"
	return fmt.Sprintf(`topk(%d, %s) or label_replace((sum(%s) - sum(topk(%d, %s))) and on () (count(%s) > %d), %q, "other", "", "")`,
		n, kept, kept, n, kept, kept, n, label)
}

// promOtherOverTime is promOther for a chart over time: the topSeriesKept
// series with the most over the whole range named at every step, and the rest added up per
// step as one series whose label reads other, only at the steps where the
// rest had something. `perStep` is the increase a step draws, written with the
// window of daily or hourly, `[1d]` or `[1h]`, which those turn into the step;
// the ranking is the same increase over the dashboard's range, fixed at its
// end with the @ modifier, so the eight are the eight of the range, as the SQL
// stores rank them, and not a topk of each step, which would name other types
// at every bar. `on` is the labels a series is told apart by, and `label` the
// one its legend reads.
func promOtherOverTime(perStep, on, label string) string {
	rank := strings.NewReplacer("[1d]", "[$__range] @ end()", "[1h]", "[$__range] @ end()").Replace(perStep)
	top := fmt.Sprintf("topk(%d, (%s) > 0)", topSeriesKept, rank)
	return fmt.Sprintf(`(%s) and on (%s) %s or label_replace(sum((%s) unless on (%s) %s) > 0, %q, "other", "", "")`,
		perStep, on, top, perStep, on, top, label)
}
