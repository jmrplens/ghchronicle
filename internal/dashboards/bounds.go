package dashboards

// Panels whose range is their own, and why.
//
// Grafana's range picker moves the whole page, which is right for a
// measurement that accumulates and wrong for two kinds of panel. One kind
// cannot be answered at all past a point: InfluxDB 3 Core refuses a query
// that would open more than forty thousand Parquet files, and gh_commit,
// which tags every row with its own sha and is partitioned into ten minute
// slots, holds 191,606 files for 445,765 rows. Measured on 2026-09-14 against
// the production database: 270 days of it opens 39,256 files and answers,
// 300 days is refused, and the reader sees "No data" rather than the error.
// The other kind is a measurement GitHub itself keeps for days, drawn on a
// canvas of years as a one pixel stripe at the right edge.
//
// The fix is the panel's own timeFrom, and the description says what the
// badge in the header only hints at.
//
// The second kind turned out not to exist. The review of 2026-09-14 read the
// event feed, the webhook deliveries and the notifications as permanently
// short and proposed pinning them; measured against the store, each one grows
// from the day it was first collected (gh_event holds 1 point on 2026-09-03,
// 162 on 2026-09-11 and 366 on 2026-09-12), so a window pinned today would
// throw away real history in a month. What is short is the backfill: these
// three cannot be walked back past what GitHub still serves, and the panels
// already say so.

const (
	// commitWindow is the range every gh_commit panel is pinned to. Ninety
	// days opens 14,485 files, a third of the limit, so the section still
	// answers after another full backfill roughly doubles the count; a
	// hundred and eighty days would be 28,627 and would not.
	commitWindow = "90d"
)

// wholeHistory is the window a panel carries when its subject is the whole of
// a measurement rather than the page's range: every repository ever created,
// the oldest alert still open, a sponsorship made in 2021. Thirty years is
// longer than GitHub has existed, so it means "all of it", and it is written
// once here rather than at each call site because it is the same shape that
// emptied the Code section when gh_commit outgrew the file limit.
//
// It is a bound the planner can use, which is what keeps it honest: measured
// on 2026-09-14 against the production database, a count over gh_commit with
// this window is refused while the same count over ninety days answers, so
// now() minus an interval is folded before the files are counted. What it
// bounds is everything, so the protection is in the size of the tables behind
// it, and that is measured: the largest of the nine, gh_star, opens 991 files
// against a limit of 40,000, and the whole set together opens 2,969. Each
// panel is on the list in bounds_test.go with the reason a narrower window
// would change its answer.
//
// A tenth reader is not a panel: repoFlagsJoin reads gh_repo on this window so
// that the four lists of what to do next can leave out what nobody can act on
// whatever the page's range is set to. It is the cheapest of the ten, one
// parquet file and 59 rows measured the same way on 2026-09-17, and it is
// counted here because the budget is over readers and not over panels.
const wholeHistory = "time > now() - INTERVAL '30 years'"

// The sentence each bounded panel carries, so the badge in the header is
// explained rather than merely noticed.
const (
	commitBound = "This panel keeps its own range of ninety days whatever the page is set to. " +
		"Every commit is a row of its own in the store, and at about ten months of them " +
		"the query is refused for opening too many files, which the reader would see as " +
		"an empty panel rather than as an error."
)

// bounded is the option that pins a panel to the commit window. It is the
// only window there is today; the next measurement to outgrow the file limit
// gets its own constant beside commitWindow and its own helper here.
func bounded() Opts { return Opts{"time_from": commitWindow} }

// forksIncluded is what a panel says when its subject reads as the account's
// own work and its numbers are not. The repository variable is built from
// gh_repo, which does not distinguish a fork from a repository the account
// wrote, and after the backfill of 2026-09 the picker holds 57 repositories:
// 18 of the account's own that are live, 17 archived and 22 forks. Measured
// over the ninety days to 2026-09-14, 48,859 commits reached these panels and
// 3,132 of them, 6.4%, were to a repository the account owns; 35,738 were to
// one fork of a Microsoft repository. Only gh_repo carries the fork and
// archived tags, so a panel over any other measurement cannot filter on them
// in Prometheus or Graphite, which have no join. Saying it is what every
// store can do.
//
// Saying it is not all the two SQL stores can do, and where the panel is a
// list of what to do next rather than a count they join gh_repo and leave the
// unactionable rows out instead: see repoFlagsJoin. This sentence stays on the
// panels that count, where leaving rows out would be answering a different
// question from the one the title asks.
//
// What the picker holds is what the sweeps collect, so the sentence names the
// settings and not a number: with include_forks and include_archived off, the
// default, it lists neither kind, and it lists the archived ones for a while
// after a backfill, which walks them whatever the setting says. How long
// depends on the store: seven days of gh_repo in the SQL stores, the range in
// Elasticsearch and Prometheus, and as long as the path exists in Graphite.
const forksIncluded = "The repository picker holds the repositories the sweeps collect: " +
	"forks too when `include_forks` is on, and archived ones when `include_archived` is on " +
	"and for a while after a backfill, which collects them either way. A count here is " +
	"over every repository the picker holds until it is narrowed."

// archivedLeftOut is what a list of what to do next says where it leaves the
// archived repositories out. A pull request in one cannot be merged, an issue
// in one cannot be worked on and a workflow in one cannot run: GitHub refuses
// all three until the repository is unarchived. The rows are not gone from the
// dashboard, they are in the panels that count and in "Repositories archived".
const archivedLeftOut = "Archived repositories are left out, since nothing in one can be " +
	"merged, closed or run until it is unarchived; narrowing the picker to one shows an " +
	"empty table here rather than rows nobody can act on."

// noRepoFlagsHere is what a store says in place of leaving them out. It is on
// the Prometheus, Graphite and Elasticsearch side of every panel whose two SQL
// twins filter on the flags, so the same title does not promise the same list
// in five dashboards.
const noRepoFlagsHere = "Only gh_repo carries the fork and archived flags and this store " +
	"cannot join one measurement to another, so the rows the SQL dashboards leave out " +
	"are listed here."

// bucketFollowsRange is what a dated chart says instead of "per day" in its
// title.
//
// Every one of them groups by $__dateBin, which is the plugin's own macro for
// the interval Grafana computed from the range, the panel's interval floor
// and its maxDataPoints. That is the right behavior: a fixed one-day bin
// drew two years as 730 bars of one pixel. The titles did not follow it.
// Measured on 2026-09-14 over five years, "Runs per day by outcome" peaked at
// 6.3K where the busiest single day in the store holds 1,232 runs and the
// median day holds 44: the bar was a week, aligned to the epoch by
// date_bin(INTERVAL '7 days', time), under a title that said day. Thirteen
// panels said it. The bucket is not the thing to change, so the titles are.
const bucketFollowsRange = "One bar is one bucket, and the bucket widens with the range: " +
	"a day over a month, a week over a year."
