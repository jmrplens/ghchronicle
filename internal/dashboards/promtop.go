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
