package dashboards

import (
	"strings"
	"testing"
)

// The first reading of the published dashboard against the owner's own store,
// on 2026-09-17: 152 panels photographed twice, at the dashboard's own range
// and at ten years, on an account of 59 repositories, 17 of them archived and
// 22 of them forks of other people's projects. Four things it found are pinned
// here. What they have in common is that no fixture could have shown any of
// them: the fixture has one repository, it is neither archived nor a fork, and
// it carries every field of every measurement while an account carries only
// what has happened to it.

// TestTheAlertTableNamesNoColumnOnlyADismissalWouldWrite: "Time to resolve an
// alert" selected `dismissed_reason`, which the collector writes only when an
// alert was dismissed rather than fixed. A column of these stores exists once
// a point has carried it, so on an account that has only ever fixed its alerts
// the name is rejected by the planner before a row is read, and Grafana drew
// "No data" over eight resolved alerts, under a corner badge nobody notices.
// COALESCE does not help, the name being refused before the rows. Every row of
// the measurement carries `alert_state`, and it answers the same question.
func TestTheAlertTableNamesNoColumnOnlyADismissalWouldWrite(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		panels := rendered(t, store)
		raw := asJSON(t, mustPanel(t, panels, "Time to resolve an alert"))
		for _, only := range []string{"dismissed_reason", "dismissed_by", "dismissed_comment"} {
			if strings.Contains(raw, only) {
				t.Errorf("%s: the alert table names %s, which exists only on an account "+
					"that has dismissed an alert", store, only)
			}
		}
		// Graphite keeps numbers and no strings, so its twin has never held
		// how an alert ended, and says so in its own description.
		if store != "graphite" && !strings.Contains(raw, "alert_state") {
			t.Errorf("%s: the alert table no longer says how the alert ended", store)
		}
	}
}

// theQueues are the four panels that read as a list of what to do next. Each
// was won outright by rows nothing can be done to on the account this was read
// against: the eight oldest open pull requests were dependabot's in two
// archived repositories, every workflow that had never run was in an archived
// one, and 1,083 of 1,208 branches were a fork's.
var theQueues = []string{
	"Open the longest", "Open issues the longest",
	"Workflows that never ran", "Stale branches",
}

// TestTheListsOfWhatToDoNextLeaveOutTheUnactionable: the two SQL stores join
// each row's repository to gh_repo, which is the one measurement carrying the
// fork and archived flags, and leave out what GitHub will not let anybody act
// on. A pull request in an archived repository cannot be merged and a workflow
// in one cannot run; the branches panel drops the forks as well, a fork's
// branch being the upstream project's history.
func TestTheListsOfWhatToDoNextLeaveOutTheUnactionable(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		panels := rendered(t, store)
		for _, title := range theQueues {
			sql := everySQL(t, mustPanel(t, panels, title))
			if !strings.Contains(sql, "FROM gh_repo WHERE") || !strings.Contains(sql, "LEFT JOIN") {
				t.Errorf("%s %s does not read the repository's own flags: %s", store, title, sql)
			}
			if !strings.Contains(sql, "f.archived") {
				t.Errorf("%s %s lists archived repositories: %s", store, title, sql)
			}
		}
		if sql := everySQL(t, mustPanel(t, panels, "Stale branches")); !strings.Contains(sql, "f.fork") {
			t.Errorf("%s Stale branches lists the branches of forks: %s", store, sql)
		}
	}
}

// TestTheThreeThatCannotJoinSaySo: Prometheus, Graphite and Elasticsearch have
// no join, so the rows the SQL dashboards leave out are in theirs, and the same
// title must not promise the same list in five dashboards. The workflow panel
// is not here: only the two SQL stores answer it at all.
func TestTheThreeThatCannotJoinSaySo(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"prometheus", "graphite", "elasticsearch"} {
		panels := rendered(t, store)
		for _, title := range []string{"Open the longest", "Stale branches"} {
			p := mustPanel(t, panels, title)
			desc, _ := p["description"].(string)
			if p["type"] == "text" {
				continue
			}
			if !strings.Contains(desc, noRepoFlagsHere) {
				t.Errorf("%s %s lists the archived repositories and the forks without "+
					"saying so: %q", store, title, desc)
			}
		}
	}
}

// TestNoDescriptionCountsWhatTheAccountHolds: two descriptions stated a count
// of the account they were written against. "Cache against the ceiling" said
// three repositories were over the ten gigabyte line where none was, and "Rate
// budget used" said its neighbor lists eleven buckets where it lists fifteen,
// which the neighbor's own description said. A count in a description is a
// second copy of the data kept by hand, and it goes stale the moment the
// account moves; the threshold, the rule and the unit do not.
func TestNoDescriptionCountsWhatTheAccountHolds(t *testing.T) {
	t.Parallel()
	counts := []string{
		"Three repositories", "three repositories",
		"all eleven", "Fifteen buckets", "fifteen buckets",
	}
	for _, store := range Names() {
		panels := rendered(t, store)
		for _, title := range []string{
			"Cache against the ceiling", "Rate budget used", "Every bucket",
		} {
			desc, _ := mustPanel(t, panels, title)["description"].(string)
			for _, c := range counts {
				if strings.Contains(desc, c) {
					t.Errorf("%s %s states %q, which is a count of one account's data: %q",
						store, title, c, desc)
				}
			}
		}
	}
}

// TestAReviewWaitNobodyEverHadReadsAsWords: of 925 pull requests in the range,
// none had a review by a person other than the author, and the value drew as a
// labeled hole in the middle of the six. A null that means something has to
// read as something.
func TestAReviewWaitNobodyEverHadReadsAsWords(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		raw := asJSON(t, mustPanel(t, rendered(t, store), "Merged and closed in range")["fieldConfig"])
		if !strings.Contains(raw, `"id":"noValue","value":"no human review"`) {
			t.Errorf("%s: the review wait draws an empty space where nobody reviewed: %s", store, raw)
		}
	}
}

// TestTheOverviewHeadlineCountsClonersNotClones: the third traffic tile read
// 186 K beside 2.89 K views, which reads as an audience and was one repository
// cloned 135,683 times in a fortnight by 1,807 cloners, by the account's own
// continuous integration. The clone count is still two panels of the Audience
// section, where it is the subject rather than a headline.
func TestTheOverviewHeadlineCountsClonersNotClones(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		// Prometheus holds GitHub's whole fourteen day window rather than the
		// range, and its panel is titled for that.
		title := "Traffic in range"
		if store == "prometheus" {
			title = "Traffic, 14-day window"
		}
		raw := asJSON(t, mustPanel(t, rendered(t, store), title))
		if !strings.Contains(raw, overviewUniqueCloners) {
			t.Errorf("%s: the traffic tiles do not count the cloners: %s", store, raw)
		}
		if strings.Contains(raw, `"Clones"`) {
			t.Errorf("%s: the traffic tiles still headline the clone count: %s", store, raw)
		}
	}
}

// TestEveryRepositoryEverOpensOnTheAccountsOwnWork: the table is ranked by
// commits and lists every repository, forks included, which is what its title
// promises. On this account that made its first eight rows forks of other
// people's projects, winget-pkgs at 370,296 commits over the account's own
// best at 3,385, and the fork and archived columns added in the round before
// labeled those rows without moving them. A leading sort on Fork opens it on
// the account's own work and hides nothing.
func TestEveryRepositoryEverOpensOnTheAccountsOwnWork(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		p := mustPanel(t, rendered(t, store), "Every repository, ever")
		options, _ := p["options"].(map[string]any)
		if got := asJSON(t, options["sortBy"]); got !=
			`[{"desc":false,"displayName":"Fork"},{"desc":true,"displayName":"Commits"}]` {
			t.Errorf("%s: the table opens sorted %s, want the forks under the account's own "+
				"work and both ranked by commits", store, got)
		}
	}
}
