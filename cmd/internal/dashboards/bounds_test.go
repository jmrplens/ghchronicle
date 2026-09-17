package dashboards

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The rule about a panel's own range, from the review of 2026-09-14, which
// ran every panel at seven days, ninety days, a year and five years: nine
// answered at ninety days and were refused at a year, every one of them over
// gh_commit.

// commitReaders is every measurement whose row count is one per commit, so a
// query over it opens one Parquet file per row. gh_commit is the only one
// today; naming it here rather than in the test body is what makes the next
// one a one line change.
var commitReaders = regexp.MustCompile(`\bgh_commit\b`)

// TestEveryCommitPanelCarriesItsOwnWindow: gh_commit holds 191,606 Parquet
// files for 445,765 rows, and InfluxDB 3 Core refuses a query that would open
// more than forty thousand. Measured: 270 days answers, 300 days is refused,
// and Grafana draws the refusal as "No data". A panel over it therefore
// carries a window narrow enough to answer, and says so.
func TestEveryCommitPanelCarriesItsOwnWindow(t *testing.T) {
	t.Parallel()
	found := 0
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			st := p.Stores["influxdb"]
			if st == nil || !readsCommits(st) {
				continue
			}
			found++
			if optString(p.Opts, "time_from", "") != commitWindow {
				t.Errorf("%s: %q reads gh_commit over the dashboard range, which is refused "+
					"past about ten months; it needs bounded(commitWindow)", sec.Title, p.Title)
			}
			if !strings.Contains(p.Desc, commitBound) {
				t.Errorf("%s: %q keeps its own range and does not say so", sec.Title, p.Title)
			}
		}
	}
	// The review counted nine, of which four were the single-number tiles
	// that are now one group. A change that took gh_commit out of all of them
	// would otherwise leave this test passing over nothing.
	if found != 6 {
		t.Fatalf("found %d panels over gh_commit, and there are 6", found)
	}
}

func readsCommits(st *store) bool {
	for i := range st.Q {
		if commitReaders.MatchString(st.Q[i].SQL) {
			return true
		}
	}
	return false
}

// TestNoTitleClaimsAPeriodTheBucketDoesNotKeep: $__dateBin widens the bucket
// with the range, which is right, and thirteen titles said "per day" anyway.
// Measured at five years: "Runs per day by outcome" drew a bar of 6.3K where
// the busiest day in the store holds 1,232 runs, because date_bin had made
// the bar a week. A panel whose bucket follows the range says so in the
// description and says "over time" in the title; a panel whose bucket is
// fixed, like the weekly commit rows GitHub serves anchored to a Sunday, may
// still name its period.
func TestNoTitleClaimsAPeriodTheBucketDoesNotKeep(t *testing.T) {
	t.Parallel()
	claims := []string{"per day", "per hour", "per week", "daily ", "Daily "}
	bound := 0
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			st := p.Stores["influxdb"]
			if st == nil || st.Q == nil || !strings.Contains(st.Q[0].SQL, "$__dateBin") {
				continue
			}
			bound++
			for _, claim := range claims {
				if strings.Contains(strings.ToLower(p.Title), strings.ToLower(claim)) {
					t.Errorf("%s: %q buckets by $__dateBin, so its bar is a week over a "+
						"year and the title claims %q", sec.Title, p.Title, strings.TrimSpace(claim))
				}
			}
			if !strings.Contains(p.Desc, bucketFollowsRange) {
				t.Errorf("%s: %q buckets by $__dateBin and does not say the bucket follows "+
					"the range", sec.Title, p.Title)
			}
		}
	}
	if bound < 12 {
		t.Fatalf("found %d panels bucketed by $__dateBin, and the review counted 13", bound)
	}
}

// TestThePanelsForksDistortSaySo: the repository picker is built from gh_repo
// and does not distinguish a fork from a repository the account wrote, so
// after the backfill of 2026-09 every count over the selection includes 22
// forks and 17 archived repositories. Measured over the ninety days to
// 2026-09-14: of 48,859 commits, 3,132 were to a repository the account owns
// and 35,738 were to one fork. Only gh_repo carries the fork and archived
// tags, so a panel over any other measurement cannot filter on them in
// Prometheus or Graphite, which have no join; saying it is what every store
// can do, and these are the panels whose title reads as the account's own
// work.
//
// These three count. "Stale branches" was the fourth and is not any more: a
// list of what to do next can leave the rows out instead of warning about
// them, which the two SQL stores now do (TestTheListsOfWhatToDoNextLeaveOutTheUnactionable).
func TestThePanelsForksDistortSaySo(t *testing.T) {
	t.Parallel()
	want := []string{
		"Commits", "Commits by author", "Every repository, ever",
	}
	seen := map[string]bool{}
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			if !slices.Contains(want, p.Title) {
				continue
			}
			seen[p.Title] = true
			if !strings.Contains(p.Desc, forksIncluded) {
				t.Errorf("%s: %q counts the forks and the archived repositories without "+
					"saying so", sec.Title, p.Title)
			}
		}
	}
	for _, title := range want {
		if !seen[title] {
			t.Errorf("no panel is called %q, so this checked nothing for it", title)
		}
	}
}

// The rule about a whole-history window, from the round of 2026-09-14, which
// measured every one of the twelve queries that asked for thirty years
// against the production database through system.parquet_files.

// timeBoundedBySubquery is the time column compared against a scalar
// subquery, in whichever direction. Both directions, because the canary the
// measurement was made with is the other one from the defect: the two curves
// carried `time < (SELECT MIN(time) ...)` and the count that proved the
// planner does not wait for a subquery was written
// `time > (SELECT MAX(time) - INTERVAL '30 days' FROM gh_commit)`. A guard
// that only knew the shape already fixed would have passed that one.
var timeBoundedBySubquery = regexp.MustCompile(`\btime\s*(?:<=|>=|<|>|=)\s*\(\s*SELECT\b`)

// TestNoPanelBoundsItselfWithASubquery: InfluxDB 3 Core counts the files a
// query would open while it plans it, before any subquery has a value, so a
// bound written as `time < (SELECT ...)` is not a bound at all. Measured on
// the production database with gh_commit as the canary: a count whose only
// bound is a scalar subquery worth thirty days is refused for exceeding the
// forty thousand file limit, where the same thirty days written as a literal
// opens 15,104 files and answers. The star and fork curves each carried one,
// and read their whole table at every range because of it.
func TestNoPanelBoundsItselfWithASubquery(t *testing.T) {
	t.Parallel()
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			for name, st := range p.Stores {
				if st == nil {
					continue
				}
				for i := range st.Q {
					if !timeBoundedBySubquery.MatchString(st.Q[i].SQL) {
						continue
					}
					t.Errorf("%s: %q in %s bounds itself with a scalar subquery, which the "+
						"file limit is counted before; the bound has to be a literal, which "+
						"$__timeFrom() is: %s", sec.Title, p.Title, name, st.Q[i].SQL)
				}
			}
		}
	}
}

// wholeHistoryPanels is every panel that reads its measurement from the
// beginning rather than over the dashboard range, and why a narrower bound
// would change what it answers. Measured against the production database on
// 2026-09-14 with system.parquet_files, against a limit of 40,000 files per
// scan: the largest of them opens 991, which is 2.5 per cent of it.
//
// The list is here so that a tenth is a deliberate act and so that the next
// round has the reasons in one place rather than spread over six files.
var wholeHistoryPanels = map[string]string{
	"Repositories created": "every repository the account ever created, and the table has no LIMIT; " +
		"gh_repo_created, 69 files",
	"Repositories archived": "every repository ever archived, the oldest of them the point; " +
		"gh_repo_archived, 15 files",
	"Contributions by year": "one row per year since the account was created; " +
		"gh_contribution_year, 24 files",
	"Latest discussions": "the newest fifty, and the store holds 25, so any narrower window " +
		"drops rows the panel promises; gh_discussion, 25 files",
	"Answers elsewhere": "the newest fifty of 189 comments reaching back to 2021; " +
		"gh_discussion_comment, 177 files",
	"Oldest open alerts": "an alert is dated when it was raised and the panel exists to show the " +
		"oldest, so a window drops exactly the rows it is for; gh_dependabot_alert_item 1,059 " +
		"files and gh_code_scanning_alert_item 673, re-measured on 2026-09-17, up from 726 and " +
		"347 three days earlier because every sweep rewrote every open alert to move its age " +
		"along. The collector no longer writes that age, so these two grow with the alerts " +
		"rather than with the clock",
	"Policy files": "each row is dated at the last commit that touched the path, so a file " +
		"nobody has touched in a year is outside any window; gh_policy_file, 149 files",
	"Dependabot ecosystems": "dated at the last commit to dependabot.yml, the same way; " +
		"gh_dependabot_ecosystem, 45 files",
	"Sponsorships": "dated the day the sponsorship began, one of them in 2021; " +
		"gh_sponsorship, 4 files",
}

// saysItKeepsItsOwnWindow is how a panel tells the reader that the range
// picker at the top of the page does not move it. A closed list of shapes
// rather than the bare word "range", which nearly every description holds
// already for an unrelated reason: `bucketFollowsRange` alone ends in "the
// bucket widens with the range", so a panel could pass a Contains check
// without ever saying that its own rows are not the page's. A new sentence
// that reads better than these belongs on this list, not around it.
var saysItKeepsItsOwnWindow = regexp.MustCompile(
	`whatever the (dashboard )?range|its own window|rather than the dashboard range|` +
		`outside any dashboard time range`,
)

// TestEveryWholeHistoryWindowIsOnTheList: the window that means "everything"
// is the shape that emptied the Code section once gh_commit outgrew the file
// limit, so every panel that carries one is named above with the reason a
// bound would change its answer, and says in its own description how it
// stands to the dashboard range.
//
// Every store, not the InfluxDB one alone: the window is written once in the
// shared SQL today, but a panel that is a text panel in InfluxDB and a query
// in PostgreSQL would otherwise carry it unwatched.
func TestEveryWholeHistoryWindowIsOnTheList(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			if !carriesWholeHistory(p) {
				continue
			}
			seen[p.Title] = true
			if _, ok := wholeHistoryPanels[p.Title]; !ok {
				t.Errorf("%s: %q reads its whole measurement and is not on the list in "+
					"bounds_test.go, which is where the reason a bound would change its "+
					"answer belongs", sec.Title, p.Title)
			}
			if !saysItKeepsItsOwnWindow.MatchString(p.Desc) {
				t.Errorf("%s: %q ignores the dashboard range and does not tell the reader "+
					"in any of the shapes saysItKeepsItsOwnWindow knows", sec.Title, p.Title)
			}
		}
	}
	for title := range wholeHistoryPanels {
		if !seen[title] {
			t.Errorf("no panel called %q carries the whole-history window, so its entry on "+
				"the list checked nothing", title)
		}
	}
}

// flagsLookup is the join repoFlagsJoin writes. It carries the unbounded
// window and is not the panel reading its own measurement from the beginning:
// it reads the newest gh_repo row per repository, a snapshot of 59 rows in one
// parquet file on the production store, while the panel's own rows still
// follow the dashboard range. So it is taken out before a panel is judged,
// and the rule above keeps meaning what it says. The reason it must be
// unbounded at all is in repoFlagsJoin.
//
// One pattern for one lookup, deliberately. A second unbounded lookup gets its
// own pattern here and its own entry in the file-limit accounting in bounds.go,
// rather than this one widened to match both: a pattern loose enough to cover
// two is loose enough to hide a panel that reads its whole measurement by
// accident, which is the thing the rule above exists to catch.
var flagsLookup = regexp.MustCompile(
	`LEFT JOIN \(SELECT repo, fork, archived FROM .*?\) f ON f\.repo = \w+\.repo`,
)

// carriesWholeHistory says whether any store of a panel reads from the
// beginning rather than over the dashboard range.
func carriesWholeHistory(p Panel) bool {
	for _, st := range p.Stores {
		if st == nil {
			continue
		}
		for i := range st.Q {
			if strings.Contains(flagsLookup.ReplaceAllString(st.Q[i].SQL, ""), wholeHistory) {
				return true
			}
		}
	}
	return false
}

// TestTheRepositoryFlagsAreReadOutsideTheRange: the four lists of what to do
// next leave out what nobody can act on by joining gh_repo, which is a
// snapshot, so the flags are only knowable where a sweep landed. Bounded by
// the page's range the join matched nothing outside one, every missing flag
// read as "not flagged", and all four panels reverted to the behavior they
// were changed to fix, with nothing on the screen to say so. Measured on the
// production store on 2026-09-17: gh_repo holds 59 rows at a single timestamp,
// so any range not containing the last sweep had no flags at all, and which
// ranges those are moves with the clock.
func TestTheRepositoryFlagsAreReadOutsideTheRange(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		panels := rendered(t, store)
		for _, title := range theQueues {
			sql := everySQL(t, mustPanel(t, panels, title))
			lookup := flagsLookup.FindString(sql)
			if lookup == "" {
				t.Fatalf("%s %s no longer joins the repository's own flags: %s", store, title, sql)
			}
			if strings.Contains(lookup, "$__timeFilter") || strings.Contains(lookup, "__timeGroup") ||
				!strings.Contains(lookup, "30 years") {
				t.Errorf("%s %s reads the flags inside the dashboard range, so the exclusion "+
					"turns itself off on a range holding no sweep: %s", store, title, lookup)
			}
		}
	}
}
