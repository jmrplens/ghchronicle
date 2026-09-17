package dashboards

import "fmt"

// Column titles the archive and rate limit tables share with their
// Elasticsearch, Graphite and Prometheus twins: the twin renames its value
// column to one of these, and a column named anything else arrives empty.
const (
	lifetimeAgeAtArchive    = "Age at archive"
	lifetimeLowestRemaining = "Lowest remaining"
	lifetimeMostUsed        = "Most used"
)

// everyRepositoryDesc is what the widest table on the dashboard says about
// itself, out here rather than at its call site so that the function building
// the section stays inside the maintainability the linter holds it to.
const everyRepositoryDesc = "The whole life of each repository in one row: not what " +
	"happened in the dashboard range, but everything there has ever been. " + forksIncluded +
	" Sorted by commits, a fork of a busy project outranks anything the account wrote, " +
	"370,296 commits against 3,385 on the account this was measured on, so the table " +
	"opens with the account's own repositories first and the forks under them, both " +
	"ranked by commits. The fork and archived flags are columns here rather than a " +
	"filter, the title being what it is, and a click on either sorts by it."

// ── Lifetime ────────────────────────────────────────────────────────────────

// lifetime is the numbers that are true since the beginning, as one row each.
//
// Every other section counts rows over the dashboard range. These are asked of
// GitHub, which counts them itself: search reports a total for any query and
// GraphQL reports one for any connection. That is what makes them instant
// here, right on the first sweep of a fresh install, and what stops a store
// being asked to scan its whole history to answer "how many ever".
func lifetime(b *builder) []Panel {
	// Fork and Archived are columns, not a filter. The backfill of 2026-09
	// brought 22 forks and 17 archived repositories into the picker, and this
	// table ranks by commits, so its first eight rows became forks of other
	// people's projects with 0 merged, 0 issues, 0 releases and 0 stars, the
	// account's own first repository ninth. Filtering them out is not right
	// either: the title says every repository, ever. So the two tags GitHub
	// already gives are shown, and one click on either column sorts by them.
	// The columns were not enough on their own: labeled or not, the first
	// screen was still eight rows of somebody else's project. The table now
	// opens sorted by Fork and then by commits, which puts the account's own
	// work first and still lists every repository (see sort_leading).
	// Third and fourth, not last: the sorted column has to stay beside the
	// first for the phone, so Commits keeps second place and the two flags
	// take the next two. At 430 pixels that is Repository, Commits and Fork
	// inside the 380 the table has.
	repos := `SELECT repo AS "Repository", MAX(commits) AS "Commits",` +
		` MAX(fork) AS "Fork", MAX(archived) AS "Archived",` +
		` MAX(pulls_merged) AS "Merged", MAX(issues) AS "Issues",` +
		` MAX(releases) AS "Releases", MAX(stars) AS "Stars",` +
		` MAX(branches) AS "Branches", MAX(tags) AS "Tags",` +
		// MAX(url) rather than grouping by it: a repository renamed inside
		// the range has two urls and is still one row.
		` MAX(url) AS "Link"` +
		" FROM gh_repo_total WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1 ORDER BY 2 DESC"
	rt := "gh_repo_total"

	reposGR, reposGRtf := gTbl(rowsOf(fmt.Sprintf("keepLastValue(%s)", rp(rt, "commits")),
		gn(rt, "repo")), "Repository", []col{{"lastNotNull", "Commits"}})
	reposES, reposEStf := esTbl(rt, []any{
		b.tm("repo", 500), b.tmURL(), b.tm("fork", 2), b.tm("archived", 2),
	},
		[]any{b.mNewest("commits", "pulls_merged", "issues", "releases", "stars", "branches", "tags")},
		[]named{
			{"repo.keyword", "Repository"},
			{"url.keyword", "Link"},
			{"fork.keyword", "Fork"},
			{"archived.keyword", "Archived"},
			{"commits", "Commits"},
			{"pulls_merged", "Merged"},
			{"issues", "Issues"},
			{"releases", "Releases"},
			{"stars", "Stars"},
			{"branches", "Branches"},
			{"tags", "Tags"},
		},
		[]string{ESF})

	var promRepos []Target
	for i, field := range []string{
		"commits", "pulls_merged", "issues", "releases", "stars", "branches", "tags",
	} {
		promRepos = append(promRepos, promTbl(
			fmt.Sprintf("github_repo_total_%s{%s}", field, PF), string(rune('A'+i)),
		))
	}

	// The two dated measurements on the row below sit years outside any range a
	// reader picks for the rest of the page, so both tables name their own
	// window the way "Contributions by year" does rather than take the
	// dashboard's. Not date_bin either: toPG has no translation for a one year
	// bin and panics the generator.
	const everSince = " WHERE " + wholeHistory
	created := `SELECT repo AS "Repository", time AS "Created", fork AS "Fork",` +
		` private AS "Private", url AS "Link" FROM gh_repo_created` + everSince +
		" ORDER BY time DESC"
	// No repository filter, in any store: the $repo variable lists the
	// repositories a sweep collects, and these rows are the ones the filter
	// set aside, so "All" would name none of them and the table would be
	// empty on the account it was made for. Measured: seventeen rows in the
	// store, none on the panel, until the filter came off.
	archived := `SELECT repo AS "Repository", time AS "Archived",` +
		` age_days_at_archive AS "Age at archive", url AS "Link"` +
		" FROM gh_repo_archived" + everSince + " ORDER BY time DESC"
	// gh_workflow_run_total is current state rewritten on every sweep, so the
	// newest row per repository is the count; adding the range up would
	// multiply it by however many sweeps landed in the range.
	// Ten bars and the rest folded: twenty two labels in eight units of height
	// were seven pixels each and overlapped.
	runsEver := otherRows(`SELECT repo AS "Repository", runs AS "Runs",`+
		" ROW_NUMBER() OVER (ORDER BY runs DESC) AS rn FROM ("+
		"SELECT repo, runs, ROW_NUMBER() OVER (PARTITION BY repo ORDER BY time DESC) AS rn"+
		" FROM gh_workflow_run_total WHERE $__timeFilter(time) AND "+RF+
		") x WHERE rn = 1", "Repository", "Runs", 10)
	rcr, rar, wrt := "gh_repo_created", "gh_repo_archived", "gh_workflow_run_total"

	// No repository filter in any store: gp() with a wildcard node here, the
	// way the gh_contribution_repo panel does it, and no ESF on the documents.
	// `repo` is the bare name here like everywhere else now, so the $repo
	// variable would match it, but the variable is built from gh_repo, which
	// is the repositories the sweep collects. This measurement is here for the
	// ones it does not: forks and private repositories that include_forks off
	// never discovers. Filtering by that list would hide exactly the rows the
	// panel exists for.
	createdGR, createdGRtf := gTbl(fmt.Sprintf(`groupByNode(isNonNull(%s), %d, "sum")`,
		gp(rcr, "created"), gn(rcr, "repo")), "Repository", []col{{"sum", "Created"}})
	// esRaw rather than a bucket aggregation, and that is what makes the two
	// string columns possible: `fork` and `private` reach Grafana as the
	// document's own values, where a top_metrics over either would hand the
	// plugin a string and panic it.
	createdES, createdEStf := b.esRaw(rcr, 500, []named{
		{"@timestamp", "Created"},
		{"repo", "Repository"},
		{"fork", "Fork"},
		{"private", "Private"},
		{"url", "Link"},
	}, nil)

	// gh_repo_archived does carry the bare `repo`, so it takes the filter
	// normally.
	archivedGR, archivedGRtf := gTbl(rowsOf(gp(rar, "age_days_at_archive"), gn(rar, "repo")),
		"Repository", []col{{"lastNotNull", lifetimeAgeAtArchive}})
	archivedES, archivedEStf := b.esRaw(rar, 500, []named{
		{"@timestamp", "Archived"},
		{"repo", "Repository"},
		{"age_days_at_archive", lifetimeAgeAtArchive},
		{"url", "Link"},
	}, nil)

	runsGR, runsGRtf := gTbl(rowsOf(fmt.Sprintf("keepLastValue(%s)", rp(wrt, "runs")),
		gn(wrt, "repo")), "Repository", []col{{"lastNotNull", "Runs"}})
	runsES, runsEStf := esTbl(wrt, []any{b.tm("repo", 500)}, []any{b.mNewest("runs")},
		[]named{{"repo.keyword", "Repository"}, {"runs", "Runs"}}, []string{ESF})

	return []Panel{
		fieldGroup(b, "gh_account_total", "Since the account began", box{W: 24, H: 5, X: 0, Y: 0}, []named{
			{"pulls_merged", "Pull requests merged"},
			{"pulls_reviewed", "Pull requests reviewed"},
			{"commits", "Commits"},
			{"issues_opened", "Issues opened"},
			{"pulls_merged_elsewhere", "Merged elsewhere"},
			{"commented_elsewhere", "Comments elsewhere"},
		}, &P{
			Desc: "Every pull request this account has had merged, anywhere, and every one " +
				"it has reviewed, including in repositories it does not own; the public " +
				"commits attributed to it, from the commit search index; the issues it " +
				"opened; and the two numbers a sweep over one's own repositories cannot " +
				"see at all, pull requests merged and comments left in other people's " +
				"repositories. All counted by GitHub's own search rather than by adding " +
				"up rows.",
		}),
		panel("table", "Every repository, ever", box{W: 24, H: 12, X: 0, Y: 5},
			[]Target{sqlT(repos)}, &P{
				Prom: promRepos,
				PromTF: merged(map[string]string{
					"repo": "Repository", "fork": "Fork", "archived": "Archived",
					panelValueA: "Commits", panelValueB: "Merged",
					panelValueC: "Issues", panelValueD: "Releases", panelValueE: "Stars",
					panelValueF: "Branches", panelValueG: "Tags",
				}, []string{
					"owner", "full_name", "visibility", "instance", "job", "__name__",
				}, map[string]int{"repo": 0, panelValueA: 1, "fork": 2, "archived": 3}),
				Opts: Opts{"sort": "Commits", "sort_leading": "Fork"},
				Desc: everyRepositoryDesc,
				Overrides: []any{
					linkOn("Repository"), width("Fork", 70), width("Archived", 90),
				},
				GR: reposGR, GRTF: reposGRtf, GRDesc: grSlot,
				ES: reposES, ESTF: reposEStf,
			}),
		panel("table", "Repositories created", box{W: 8, H: 8, X: 0, Y: 17}, []Target{sqlT(created)}, &P{
			// The exporter reduces this measurement to a count, so the answer
			// here is a real one and it is a smaller one. Said rather than
			// replaced by a text panel: how many repositories were created, and
			// how many of them were forks, is the part that survives.
			Prom: []Target{promTbl("sum by (fork) (github_repos_created_count)")},
			PromTF: []any{organize(map[string]string{
				"fork": "Fork", "Value": "Repositories",
			}, []string{"user"}, nil)},
			PromDesc: "The exporter reduces `gh_repo_created` to a count by user and fork, so " +
				"Prometheus can say how many repositories were created and how many of " +
				"them were forks, and can name none of them: the repository is not a " +
				"label on that gauge. It is the last sweep's count, so it is that one " +
				"trailing year and never the years the other stores have kept.",
			Desc: "Each repository the account created, dated when it was created rather than " +
				"when a sweep noticed it. Forks and private repositories are included, " +
				"which is the point: those are exactly the ones a sweep with " +
				"`include_forks` off never discovers. The contributions collection offers " +
				"one trailing year of them at a time, so a fresh install sees a year here " +
				"and the table reaches further back with every year the rows are kept. It " +
				"names its own window rather than taking the dashboard's, so narrowing " +
				"the range at the top of the page does not narrow this table.",
			Overrides: []any{
				when("Created"), width("Fork", 70), width("Private", 80), linkOn("Repository"),
			},
			PromOver: []any{barCell("Repositories", "short", 200)},
			GR:       createdGR, GRTF: createdGRtf,
			GRDesc: grRange + " Graphite has no way to list by date either, so each " +
				"repository created inside the range is one row named from the path, and " +
				"the column counts the creation itself, which is one on every row.",
			ES: createdES, ESTF: createdEStf, ESDesc: esRange,
		}),
		panel("table", "Repositories archived", box{W: 8, H: 8, X: 8, Y: 17}, []Target{sqlT(archived)}, &P{
			// Counted, so the count is not all that survives: the reduction
			// takes the mean of every number the archived rows carried, and
			// `age_days_at_archive` is one of them. Two of the panel's four
			// columns rather than one, on the `owner` both queries carry, which
			// is what the merge joins them on before it is dropped.
			Prom: []Target{
				promTbl("sum by (owner) (github_repos_archived_count)", "A"),
				promTbl("avg by (owner) (github_repos_archived_age_days_at_archive_mean)", "B"),
			},
			PromTF: merged(map[string]string{
				panelValueA: "Repositories archived", panelValueB: lifetimeAgeAtArchive,
			}, nil, nil),
			PromDesc: "The exporter reduces `gh_repo_archived` to a count by owner, which names " +
				"the account and not the repository, so Prometheus holds how many have " +
				"been archived and how long they lived on average, and neither which they " +
				"were nor when.",
			Desc: "`gh_repo_total` tags `archived` as a boolean stamped now, so eighteen " +
				"archived repositories read as one undifferentiated fact. Dated at " +
				"`archivedAt` the clear-outs are visible as the batches they were: eight " +
				"of them within eight seconds of each other on one day. The default " +
				"filter leaves archived repositories out of every collector, but not " +
				"out of this table: the listing a sweep already pays for says which " +
				"are archived, and each gets this one row from one query per `totals` " +
				"sweep, so the table exists from the first sweep. An archived fork under the " +
				"default fork rule is the one kind left out. A live repository produces " +
				"no row, so counting the rows is counting the archive. It names its own " +
				"window as well, so the dashboard's range leaves this table alone, and it " +
				"takes no repository filter: the variable lists the repositories a sweep " +
				"collects, and these are the ones it set aside.",
			Overrides: []any{
				when("Archived"), unitOf(lifetimeAgeAtArchive, "d", 130), linkOn("Repository"),
			},
			GR: archivedGR, GRTF: archivedGRtf,
			GRDesc: grRange + " Graphite names each row from the path, so the column is the " +
				"age at archive and the date it was archived on is gone.",
			ES: archivedES, ESTF: archivedEStf, ESDesc: esRange,
		}),
		panel("barchart", "Workflow runs, ever", box{W: 8, H: 8, X: 16, Y: 17}, []Target{sqlT(runsEver)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"topk(10, max by (repo) (github_workflow_run_total_runs{%s}))", PF,
			))},
			PromDesc: "Prometheus shows the ten and folds nothing.",
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "Value": "Runs",
			}, nil, nil)},
			Desc: "The run listing's own total_count, which is every run the repository has " +
				"ever had. The run walk sees the newest few hundred by design, so this is " +
				"the only place the whole history is counted, and it is the answer " +
				"InfluxDB 3 Core will not let a reader compute from the rows. A snapshot " +
				"rewritten every sweep, so this is the newest value per repository rather " +
				"than the sweeps added up. The ten repositories with the most runs are " +
				"named; the rest are one bar called other.",
			GR: runsGR, GRTF: runsGRtf,
			ES: runsES, ESTF: runsEStf,
		}),
	}
}

// ── The collector itself ────────────────────────────────────────────────────

// collector is what the collector has left to spend.
//
// GitHub runs fifteen independent budgets and this tool reads them in order to
// brake. A family skipped because a bucket was spent looks exactly like a
// family with nothing to report, and this section is the only place the
// difference shows.
func collectorSection(b *builder) []Panel {
	// The share of each bucket that is spent, as the newest reading in each
	// bin. The remaining count was the wrong axis: scim at 15,000 and the
	// runner registration bucket at 10,000 flattened core and graphql, the
	// two at 5,000 that actually brake a sweep, into the bottom of the chart.
	// Only the buckets that were ever used. Eleven series for eight buckets
	// this account has never touched (actions_runner_registration,
	// audit_log_streaming, integration_manifest, enterprise_token_inventory,
	// dependency_snapshots and the rest) put eleven entries into 125 pixels
	// of legend above an empty 243 pixel plot: over two years the mean share
	// per weekly bucket rounds to zero and the axis topped out at 0.25 per
	// cent. The three that are used are the panel. "Every bucket" beside it
	// still lists every bucket the store holds a reading of, with their Most
	// used at 0, which is where the absence belongs.
	budget := "SELECT time, series, used FROM (" +
		"SELECT time, resource AS series, used_ratio AS used," +
		" MAX(used_ratio) OVER (PARTITION BY resource) AS ever FROM (" +
		"SELECT " + timeBin + ", resource, used_ratio, ROW_NUMBER() OVER (PARTITION BY" +
		" $__dateBin(time), resource ORDER BY time DESC) AS rn FROM gh_rate_limit" +
		" WHERE $__timeFilter(time) AND limit > 30) x WHERE rn = 1) y" +
		" WHERE ever > 0 ORDER BY 1"
	tableQ := `SELECT resource AS "Bucket", MAX(used) AS "Most used", MAX(limit) AS "Limit",` +
		` MIN(remaining) AS "Lowest remaining"` +
		" FROM gh_rate_limit WHERE $__timeFilter(time) GROUP BY 1 ORDER BY 2 DESC"
	rl := "gh_rate_limit"

	bucketsGR, bucketsGRtf := gTbl(rowsOf(fmt.Sprintf("keepLastValue(%s)", gp(rl, "remaining")),
		gn(rl, "resource")), "Bucket", []col{{"lastNotNull", lifetimeLowestRemaining}})
	bucketsES, bucketsEStf := esTbl(rl, []any{b.tm("resource", 20)},
		[]any{b.mNewest("limit", "remaining", "used")},
		[]named{
			{"resource.keyword", "Bucket"},
			{"limit", "Limit"},
			{"remaining", lifetimeLowestRemaining},
			{"used", lifetimeMostUsed},
		}, nil)

	// What the sweep says about itself, which until this row had it it said
	// to nobody but the journal. A family writes one row per sweep whether or
	// not anything failed, and one more per repository it could not collect,
	// so the left table is every family that ran and the right one is what
	// each of them lost. Neither takes the repository variable: the left rows
	// belong to no repository, and a repository a sweep could not collect may
	// be one the variable does not list, since the variable is built from
	// gh_repo and that is a row the same sweep may have failed to write.
	cf := "gh_collector_family"
	// Grouped by the reason as well as by the family, because the two kinds of
	// failure a reader has to tell apart land in the same column otherwise: a
	// search budget spent twice a day is not the 502 that cost a repository
	// its whole history, and sorted by the same number they read the same.
	// Sweeps is the denominator that makes Failures a proportion rather than a
	// number that grows with the range: a family that fails on every sweep
	// reads 2880 of 2880 over thirty days, not 2880 out of a repository count
	// it has nothing to do with.
	ranQ := `SELECT family AS "Family", SUM(failed) AS "Failures", reason AS "Why",` +
		` COUNT(*) AS "Sweeps", MAX(repos) AS "Repositories"` +
		" FROM " + cf + " WHERE $__timeFilter(time) AND scope = 'family'" +
		" GROUP BY 1, 3 ORDER BY 2 DESC, 1"
	lostQ := `SELECT time AS "When", family AS "Family", repo AS "Repository",` +
		` reason AS "Why", error AS "What GitHub said" FROM ` + cf +
		" WHERE $__timeFilter(time) AND scope = 'repo' ORDER BY time DESC LIMIT 100"

	ranGR, ranGRtf := gTbl(rowsOf(gp(cf, "failed", "scope", scopeFamilyTag),
		gn(cf, "family"), gn(cf, "reason")),
		"Family, why", []col{{"sum", collectorFailures}})
	ranES, ranEStf := esTbl(cf, []any{b.tm("family", 40), b.tm("reason", 20)},
		[]any{b.mSum("failed"), b.mCount(), b.mMax("repos")},
		[]named{
			{"family.keyword", "Family"},
			{"reason.keyword", "Why"},
			{"f", collectorFailures},
			{"s", "Sweeps"},
			{"r", "Repositories"},
		}, []string{`scope.keyword:"` + scopeFamilyTag + `"`})

	lostGR, lostGRtf := gTbl(rowsOf(gp(cf, "failed", "scope", scopeRepoTag),
		gn(cf, "family"), gn(cf, "repo"), gn(cf, "reason")),
		"Family, repository, why", []col{{"sum", collectorFailures}})
	lostES, lostEStf := b.esRaw(cf, 100, []named{
		{panelESTime, "When"},
		{"family", "Family"},
		{"repo", "Repository"},
		{"reason", "Why"},
		{"error", "What GitHub said"},
	}, []string{`scope.keyword:"` + scopeRepoTag + `"`})

	return []Panel{
		panel("timeseries", "Rate budget used", box{W: 12, H: 11, X: 0, Y: 0},
			[]Target{sqlTS(budget)}, &P{
				Prom: []Target{promq("github_rate_limit_used_ratio and on (resource) (github_rate_limit_limit > 30)",
					legend("{{resource}}"))},
				Opts:    Opts{"unit": "percentunit", "fill": 0, "interval": "5m"},
				SQLOpts: seriesOpts,
				Desc: "The share of each bucket spent, which puts a bucket of five thousand " +
					"and one of fifteen thousand on the same axis. The buckets worth " +
					"watching, which are the ones with more than thirty requests in them: " +
					"search has thirty a minute and would flatten the axis. A bucket this " +
					"account never touched is left out of the chart entirely, since its flat " +
					"zero was still a legend entry; \"Every bucket\" beside this lists every " +
					"one of them, and a Most used of 0 is where that absence belongs. A reading " +
					"taken at each sweep, so the curve starts the day the collector did. " +
					bucketFollowsRange,
				GR: []Target{grq(fmt.Sprintf("aliasByNode(keepLastValue(%s), %d)",
					gp(rl, "used_ratio"), gn(rl, "resource")))},
				ES:     []Target{b.esDaily(rl, b.mMax("used_ratio"), "resource", "5m", nil, "")},
				ESDesc: "In Elasticsearch each point is the largest reading of its five minutes.",
			}),
		panel("table", "Every bucket", box{W: 12, H: 11, X: 12, Y: 0}, []Target{sqlT(tableQ)}, &P{
			Prom: []Target{
				promTbl("max by (resource) (github_rate_limit_limit)", "A"),
				promTbl("min by (resource) (github_rate_limit_remaining)", "B"),
				promTbl("max by (resource) (github_rate_limit_used)", "C"),
			},
			PromTF: merged(map[string]string{
				"resource": "Bucket", panelValueA: "Limit",
				panelValueB: lifetimeLowestRemaining, panelValueC: lifetimeMostUsed,
			}, nil, nil),
			Opts: Opts{"sort": lifetimeMostUsed},
			Desc: "Every budget GitHub reports, and the one that runs out first decides what " +
				"a sweep can collect. Reading them costs nothing: GET /rate_limit is free.",
			GR: bucketsGR, GRTF: bucketsGRtf, GRDesc: grSlot,
			ES: bucketsES, ESTF: bucketsEStf,
		}),
		panel("table", "Every family", box{W: 12, H: 9, X: 0, Y: 11}, []Target{sqlT(ranQ)}, &P{
			Prom: []Target{
				promTbl("sum by (family, reason) (github_collector_family_failed{scope=\""+scopeFamilyTag+"\"})", "A"),
				promTbl("sum by (family, reason) (github_collector_family_repos{scope=\""+scopeFamilyTag+"\"})", "B"),
			},
			PromTF: merged(map[string]string{
				"family": "Family", "reason": "Why",
				panelValueA: collectorFailures, panelValueB: "Repositories",
			}, []string{"scope"}, nil),
			PromDesc: sweepCount + " There is no Sweeps column here for the same reason: " +
				"the exporter holds one sweep, so the count would be one on every row.",
			Opts: Opts{"sort": collectorFailures},
			Overrides: []any{
				barCell(collectorFailures, "short", 100),
				width("Why", 110), width("Sweeps", 80), width("Repositories", 100),
			},
			Desc: "Every collector that ran in the range, with what stopped it where " +
				"something did, how many sweeps it ran in, and how many repositories it " +
				"was asked about. A family with no row here did not run at all, which is " +
				"the one thing an empty panel could never say: not due, switched off, or " +
				"skipped because a rate budget was spent. Why is what separates the two " +
				"kinds of failure that would otherwise read alike: a search budget spent " +
				"twice a day sorts apart from the 502 that cost a repository its history, " +
				"and a family that had both has a row for each. Failures is the sweep's " +
				"own count, so a family that failed on some of its repositories and was " +
				"still marked as having run says so here; a batched query that failed " +
				"names no repository and counts as one. One row here is not a family: " +
				"`discover` is the repository listing, which every family depends on, and " +
				"it appears only when it failed, because a listing that worked is stated " +
				"by every other row of the same sweep. No repository filter: these rows " +
				"belong to the collector rather than to a repository.",
			GR: ranGR, GRTF: ranGRtf, GRDesc: grRows,
			ES: ranES, ESTF: ranEStf,
		}),
		panel("table", "What failed, and where", box{W: 12, H: 9, X: 12, Y: 11}, []Target{sqlT(lostQ)}, &P{
			Prom: []Target{promTbl(
				"github_collector_family_failed{scope=\"" + scopeRepoTag + "\"}",
			)},
			PromTF: []any{organize(map[string]string{
				"family": "Family", "repo": "Repository", "reason": "Why",
				"Value": collectorFailures,
			}, []string{"scope"}, nil)},
			PromDesc: "In Prometheus this is the last sweep's failures rather than the " +
				"range's, and the exporter carries no message, so the What GitHub said " +
				"column of the InfluxDB dashboard is absent here.",
			Overrides: []any{when("When"), repoColumn(), width("Why", 90)},
			Desc: "One row per repository one collector could not collect, newest first: " +
				"which family, which repository, and what GitHub answered. This is where " +
				"an empty row of the dashboard gets its explanation. Measured on this " +
				"account on 2026-09-16, before there was anything to read it from: five " +
				"repositories held no workflow run or job at all, each because one call " +
				"for the jobs of one run had answered 502 once, and the Continuous " +
				"integration row was drawn over an account missing its five busiest " +
				"repositories. An empty table is the good case. No repository filter: a " +
				"repository a sweep could not collect may be one the filter does not " +
				"list, since the filter is built from rows the same sweep writes.",
			GR: lostGR, GRTF: lostGRtf, GRDesc: grRows,
			ES: lostES, ESTF: lostEStf,
			ESDesc: "In Elasticsearch this lists the newest 100 documents and the panel " +
				"sorts them.",
		}),
	}
}
