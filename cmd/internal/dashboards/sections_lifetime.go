package dashboards

import "fmt"

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

	// gh_repo_created tags `repo` with owner/name rather than the bare name, so
	// the $repo variable never matches it and the panel carries no repository
	// filter in any store: gp() with a wildcard node here, the way the
	// gh_contribution_repo panel does it, and no ESF on the documents.
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
		"Repository", []col{{"lastNotNull", "Age at archive"}})
	archivedES, archivedEStf := b.esRaw(rar, 500, []named{
		{"@timestamp", "Archived"},
		{"repo", "Repository"},
		{"age_days_at_archive", "Age at archive"},
		{"url", "Link"},
	}, nil)

	runsGR, runsGRtf := gTbl(rowsOf(fmt.Sprintf("keepLastValue(%s)", rp(wrt, "runs")),
		gn(wrt, "repo")), "Repository", []col{{"lastNotNull", "Runs"}})
	runsES, runsEStf := esTbl(wrt, []any{b.tm("repo", 500)}, []any{b.mNewest("runs")},
		[]named{{"repo.keyword", "Repository"}, {"runs", "Runs"}}, []string{ESF})

	return []Panel{
		fieldGroup(b, "gh_account_total", "Since the account began", 24, 5, 0, 0, []named{
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
		panel("table", "Every repository, ever", 24, 12, 0, 5,
			[]Target{sqlT(repos)}, &P{
				Prom: promRepos,
				PromTF: merged(map[string]string{
					"repo": "Repository", "fork": "Fork", "archived": "Archived",
					"Value #A": "Commits", "Value #B": "Merged",
					"Value #C": "Issues", "Value #D": "Releases", "Value #E": "Stars",
					"Value #F": "Branches", "Value #G": "Tags",
				}, []string{
					"owner", "full_name", "visibility", "instance", "job", "__name__",
				}, map[string]int{"repo": 0, "Value #A": 1, "fork": 2, "archived": 3}),
				Opts: Opts{"sort": "Commits"},
				Desc: "The whole life of each repository in one row: not what happened in the " +
					"dashboard range, but everything there has ever been. " + forksIncluded +
					" Sorted by commits, a fork of a busy project outranks anything the " +
					"account wrote, so the fork and archived flags are columns here and a " +
					"click on either sorts by it.",
				Overrides: []any{
					linkOn("Repository"), width("Fork", 70), width("Archived", 90),
				},
				GR: reposGR, GRTF: reposGRtf, GRDesc: grSlot,
				ES: reposES, ESTF: reposEStf,
			}),
		panel("table", "Repositories created", 8, 8, 0, 17, []Target{sqlT(created)}, &P{
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
		panel("table", "Repositories archived", 8, 8, 8, 17, []Target{sqlT(archived)}, &P{
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
				"Value #A": "Repositories archived", "Value #B": "Age at archive",
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
				when("Archived"), unitOf("Age at archive", "d", 130), linkOn("Repository"),
			},
			GR: archivedGR, GRTF: archivedGRtf,
			GRDesc: grRange + " Graphite names each row from the path, so the column is the " +
				"age at archive and the date it was archived on is gone.",
			ES: archivedES, ESTF: archivedEStf, ESDesc: esRange,
		}),
		panel("barchart", "Workflow runs, ever", 8, 8, 16, 17, []Target{sqlT(runsEver)}, &P{
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
	// still lists all eleven with their Most used at 0, which is where the
	// absence belongs.
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
		gn(rl, "resource")), "Bucket", []col{{"lastNotNull", "Lowest remaining"}})
	bucketsES, bucketsEStf := esTbl(rl, []any{b.tm("resource", 20)},
		[]any{b.mNewest("limit", "remaining", "used")},
		[]named{
			{"resource.keyword", "Bucket"},
			{"limit", "Limit"},
			{"remaining", "Lowest remaining"},
			{"used", "Most used"},
		}, nil)

	return []Panel{
		panel("timeseries", "Rate budget used", 12, 11, 0, 0,
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
					"zero was still a legend entry; \"Every bucket\" beside this lists all " +
					"eleven, and a Most used of 0 is where that absence belongs. A reading " +
					"taken at each sweep, so the curve starts the day the collector did. " +
					bucketFollowsRange,
				GR: []Target{grq(fmt.Sprintf("aliasByNode(keepLastValue(%s), %d)",
					gp(rl, "used_ratio"), gn(rl, "resource")))},
				ES:     []Target{b.esDaily(rl, b.mMax("used_ratio"), "resource", "5m", nil, "")},
				ESDesc: "In Elasticsearch each point is the largest reading of its five minutes.",
			}),
		panel("table", "Every bucket", 12, 11, 12, 0, []Target{sqlT(tableQ)}, &P{
			Prom: []Target{
				promTbl("max by (resource) (github_rate_limit_limit)", "A"),
				promTbl("min by (resource) (github_rate_limit_remaining)", "B"),
				promTbl("max by (resource) (github_rate_limit_used)", "C"),
			},
			PromTF: merged(map[string]string{
				"resource": "Bucket", "Value #A": "Limit",
				"Value #B": "Lowest remaining", "Value #C": "Most used",
			}, nil, nil),
			Opts: Opts{"sort": "Most used"},
			Desc: "Fifteen buckets, and the one that runs out first decides what a sweep " +
				"can collect. Reading them costs nothing: GET /rate_limit is free.",
			GR: bucketsGR, GRTF: bucketsGRtf, GRDesc: grSlot,
			ES: bucketsES, ESTF: bucketsEStf,
		}),
	}
}
