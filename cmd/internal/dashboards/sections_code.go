package dashboards

import "fmt"

// ── Code ────────────────────────────────────────────────────────────────────

// code is the commit record and then the gates the commits went through.
func code(b *builder) []Panel {
	return append(commitsAndChurn(b), commitChecks(b)...)
}

// commitsAndChurn is what was written: how many commits, how many lines each
// way, how many were signed, by whom, and the pushes that rewrote a branch.
func commitsAndChurn(b *builder) []Panel {
	churn := "SELECT " + timeBin + ", 'added' AS series," +
		" SUM(additions) AS lines FROM gh_commit WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1 UNION ALL" +
		" SELECT " + timeBin + ", 'removed' AS series," +
		" -SUM(deletions) AS lines FROM gh_commit WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1 ORDER BY 1"
	perAuthor := `SELECT author AS "Author", COUNT(*) AS "Commits",` +
		` SUM(additions) AS "Added", SUM(deletions) AS "Removed",` +
		` approx_percentile_cont(churn, 0.5) AS "Lines per commit"` +
		" FROM gh_commit WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1 ORDER BY 2 DESC LIMIT 20"
	sigs := `SELECT signature AS "Signature", COUNT(*) AS "Commits" FROM gh_commit` +
		" WHERE $__timeFilter(time) AND " + RF + " GROUP BY 1 ORDER BY 2 DESC"
	activity := "SELECT " + timeBin + ", activity AS series," +
		" SUM(events) AS n FROM gh_repo_activity WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 2 ORDER BY 1"
	// ref_name is a field: a name minted per pull request was a series per
	// branch as the tag `ref`. One row here can name several branches, when
	// one push moved them all in the same second.
	force := `SELECT repo AS "Repository", time AS "When", ref_name AS "Branch",` +
		` actor AS "By" FROM gh_repo_activity WHERE $__timeFilter(time)` +
		" AND " + RF + " AND activity = 'force_push' ORDER BY time DESC LIMIT 25"

	totalM := fmt.Sprintf("github_commits_total{%s}", PF)
	// The exporter serves a mean per commit and a count per sweep. Their
	// product is the lines of the sweep, the nearest thing to a sum it has.
	added := fmt.Sprintf("sum(github_commits_additions_mean{%s} * github_commits_count{%s})", PF, PF)
	removed := fmt.Sprintf("sum(github_commits_deletions_mean{%s} * github_commits_count{%s})", PF, PF)
	c := "gh_commit"
	cpath := func(field string) string { return rp(c, field) }
	ra := "gh_repo_activity"
	act := rp(ra, "events")
	forcePath := rp(ra, "events", "activity", "force_push")
	negative := override("removed", []any{
		map[string]any{"id": "custom.transform", "value": "negative-Y"},
	})

	signedSteps := []any{
		map[string]any{"color": "red", "value": nil},
		map[string]any{"color": "orange", "value": 50},
		map[string]any{"color": "green", "value": 90},
	}

	authorGR, authorGRtf := gTbl(fmt.Sprintf(
		`limit(sortBy(groupByNode(%s, %d, "avg"), "count", true), 20)`,
		cpath("churn"), gn(c, "author"),
	), "Author",
		[]col{{"count", "Commits"}, {"median", "Lines per commit"}})
	authorES, authorEStf := esTbl(c, []any{b.tm("author", 20)},
		[]any{b.mCount(), b.mSum("additions"), b.mSum("deletions"), b.mPct("churn", 50)},
		[]named{
			{"author.keyword", "Author"},
			{"n", "Commits"},
			{"a", "Added"},
			{"r", "Removed"},
			{"l", "Lines per commit"},
		}, []string{ESF})

	sigsGR, sigsGRtf := gTbl(fmt.Sprintf(`sortByTotal(groupByNode(isNonNull(%s), %d, "sum"))`,
		cpath("churn"), gn(c, "signature")), "Signature", []col{{"sum", "Commits"}})
	sigsES, sigsEStf := esTbl(c, []any{b.tm("signature", 10)}, []any{b.mCount()},
		[]named{{"signature.keyword", "Signature"}, {"n", "Commits"}}, []string{ESF})

	// The branch is a field now (a name minted per pull request, never
	// reused, was a series per branch), so Graphite, which keeps no strings,
	// groups the force pushes by repository and actor only.
	forceGR, forceGRtf := gTbl(rowsOf(forcePath, gn(ra, "repo"), gn(ra, "actor")),
		"Repository, by", []col{{"sum", "Force pushes"}})
	forceES, forceEStf := b.esRaw(ra, 25, []named{
		{"@timestamp", "When"}, {"repo", "Repository"}, {"ref_name", "Branch"}, {"actor", "By"},
	}, []string{"activity:force_push", ESF})

	// One group of four rather than four tiles: on a phone each tile was its
	// own 144 pixel panel and the four opened the section with nothing else on
	// the screen. The signed share keeps its colors by naming its own
	// thresholds on its own field, since a stat's colors are otherwise the
	// whole panel's and would paint the three counts beside it.
	commitStats := `SELECT COUNT(*) AS "Commits", SUM(additions) AS "Lines added",` +
		` SUM(deletions) AS "Lines removed", 100.0 *` +
		` SUM(CASE WHEN signature = 'VALID' THEN 1 ELSE 0 END) / COUNT(*) AS "Signed commits"` +
		" FROM gh_commit WHERE $__timeFilter(time) AND " + RF

	return []Panel{
		statGroup("Commits", 24, 4, 0, 0, []Target{sqlT(commitStats)}, &P{
			Prom: []Target{
				promNamed("A", "Commits", fmt.Sprintf("sum(increase(%s[$__range]))", totalM)),
				promNamed("B", "Lines added", added),
				promNamed("C", "Lines removed", removed),
				promNamed("D", "Signed commits", fmt.Sprintf(
					`100 * sum(increase(github_commits_total{signature="VALID",%s}[$__range])) / sum(increase(%s[$__range]))`,
					PF, totalM,
				)),
			},
			Desc: "How many commits landed in the window, how many lines each way, and what " +
				"share carried a signature GitHub could verify. A commit with no signature " +
				"at all is a different fact from one whose signature failed to verify, and " +
				"both are counted separately in the chart below. " + forksIncluded + " " + commitBound,
			PromDesc: sinceStart + " " + sweepCount,
			GR: []Target{
				grNamed("A", "Commits", total(countOf(cpath("churn")))),
				grNamed("B", "Lines added", total(fmt.Sprintf("sumSeries(%s)", cpath("additions")))),
				grNamed("C", "Lines removed", total(fmt.Sprintf("sumSeries(%s)", cpath("deletions")))),
				grNamed("D", "Signed commits", fmt.Sprintf("asPercent(%s, %s)",
					total(countOf(rp(c, "churn", "signature", "VALID"))),
					total(countOf(cpath("churn"))))),
			},
			ES: []Target{
				esRef("A", b.esTotal(c, b.mCount(), ESF)),
				esRef("B", b.esTotal(c, b.mSum("additions"), ESF)),
				esRef("C", b.esTotal(c, b.mSum("deletions"), ESF)),
				esRef("D", b.esTotal(c, b.mAvg("signed"), ESF)),
			},
			ESOver: []any{
				frameName("A", "Commits"), frameName("B", "Lines added"),
				frameName("C", "Lines removed"), frameName("D", "Signed commits"),
				fieldThresholds("Signed commits", "percentunit", fractionOf(signedSteps)),
			},
			ESDesc: "In Elasticsearch the signed share is the mean of the boolean `signed` " +
				"field, as a fraction.",
			Opts:      mergeOpts(Opts{"thresholds": plainSteps}, bounded()),
			Overrides: []any{fieldThresholds("Signed commits", "percent", signedSteps)},
		}),
		panel("timeseries", "Lines changed over time", 12, 8, 0, 4, []Target{sqlTS(churn)}, &P{
			Prom: []Target{
				promq(added, withRef("A"), legend("added")),
				promq("-"+removed, withRef("B"), legend("removed")),
			},
			Opts:      mergeOpts(mergeOpts(Opts{"bars": true, "stack": true, "min_zero": false}, dayBins), bounded()),
			SQLOpts:   seriesOpts,
			PromOpts:  Opts{"bars": false},
			Overrides: []any{colorOf("added", "green"), colorOf("removed", "red")},
			Desc: "Additions above the axis, deletions below, per day. This is the answer to " +
				"GitHub's own code frequency endpoint, which returns 202 with an empty " +
				"body forever on a personal account. " + commitBound + " " + bucketFollowsRange,
			PromDesc: sweepCount,
			GR: []Target{
				grq(fmt.Sprintf(`alias(consolidateBy(summarize(sumSeries(%s), "1d", "sum"), "sum"), "added")`,
					cpath("additions")), "A"),
				grq(fmt.Sprintf(`alias(scale(consolidateBy(summarize(sumSeries(%s), "1d", "sum"), "sum"), -1), "removed")`,
					cpath("deletions")), "B"),
			},
			ES: []Target{
				esq(c, []any{b.mSum("additions")}, []any{b.dh()}, "A", []string{ESF}, "added"),
				esq(c, []any{b.mSum("deletions")}, []any{b.dh()}, "B", []string{ESF}, "removed"),
			},
			ESOver: []any{negative},
		}),
		panel("timeseries", "Repository activity", 12, 8, 12, 4, []Target{sqlTS(activity)}, &P{
			Prom: []Target{hourly(fmt.Sprintf(
				"sum by (activity) (increase(github_repo_activities_total{%s}[1h]))", PF,
			), "{{activity}}")},
			Opts:    mergeOpts(Opts{"bars": true, "stack": true}, hourBins),
			SQLOpts: seriesOpts,
			Desc: "Pushes, branch creations and deletions, merges and force pushes, per hour. " +
				"The only place a force push is recorded at all, and as perishable as " +
				"traffic: a hundred entries covered twenty six hours here. " + bucketFollowsRange,
			PromDesc: sinceStart,
			GR:       []Target{grq(perBucket(act, gn(ra, "activity"), "1h"))},
			ES:       []Target{b.esDaily(ra, b.mSum("events"), "activity", "1h", []string{ESF}, "")},
		}),
		panel("table", "Commits by author", 12, 8, 0, 12, []Target{sqlT(perAuthor)}, &P{
			Prom: func() []Target {
				rank := fmt.Sprintf("sum by (author) (increase(%s[$__range]))", totalM)
				return []Target{
					promTbl(promTop(20, rank), "A"),
					promTbl(promWithin(20, fmt.Sprintf(
						"avg by (author) (github_commits_churn_mean{%s})", PF,
					), rank, "author"), "B"),
				}
			}(),
			PromTF: merged(map[string]string{
				"author": "Author", "Value #A": "Commits", "Value #B": "Lines per commit",
			}, nil, nil),
			Opts: mergeOpts(Opts{"sort": "Commits"}, bounded()),
			Desc: "The twenty authors with the most commits in the window. " +
				forksIncluded + " On an account with forks of busy projects the upstream " +
				"authors are the top of this table, which is the honest answer to the " +
				"question as asked: narrow the picker to read it as the account's own work. " +
				commitBound,
			PromDesc: sinceStart + " " + lastSweep,
			Overrides: []any{
				barCell("Commits", "short", 130), width("Added", 110),
				width("Removed", 110), width("Lines per commit", 130),
			},
			GR: authorGR, GRTF: authorGRtf,
			GRDesc: "Graphite reduces the churn of each author: how many commits, and the median. " + grRows,
			ES:     authorES, ESTF: authorEStf,
		}),
		panel("barchart", "Commits by signature", 6, 8, 12, 12, []Target{sqlT(sigs)}, &P{
			Opts:     bounded(),
			Desc:     commitBound,
			Prom:     []Target{promTbl(fmt.Sprintf("sum by (signature) (increase(%s[$__range]))", totalM))},
			PromTF:   []any{organize(map[string]string{"signature": "Signature", "Value": "Commits"}, nil, nil)},
			PromDesc: sinceStart,
			GR:       sigsGR, GRTF: sigsGRtf,
			ES: sigsES, ESTF: sigsEStf,
		}),
		panel("table", "Force pushes", 6, 8, 18, 12, []Target{sqlT(force)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				`sum by (repo) (increase(github_repo_activities_total{activity="force_push",%s}[$__range])) > 0`, PF,
			))},
			PromTF: []any{organize(map[string]string{"repo": "Repository", "Value": "Force pushes"}, nil, nil)},
			Desc:   "Each force push, newest first.",
			PromDesc: "Prometheus keeps no branch or actor, so this counts them per " +
				"repository over the range. " + sinceStart,
			Overrides: []any{when("When")},
			PromOver:  []any{barCell("Force pushes", "short", 120)},
			GR:        forceGR, GRTF: forceGRtf,
			GRDesc: "Graphite has no way to sort by date, so this counts them per repository, " +
				"branch and actor over the range.",
			GROver: []any{barCell("Force pushes", "short", 120)},
			ES:     forceES, ESTF: forceEStf,
		}),
	}
}

// commitChecks is what the gates said about those commits: whether each came
// out green, what ran besides Actions, and which commits left a branch red.
func commitChecks(b *builder) []Panel {
	cc := "gh_commit_check"
	// gate is a field: the verdict lands after the commit's own date, and as
	// the tag `checks` a commit seen PENDING and then FAILURE was two rows.
	gate := "SELECT " + timeBin + ", gate AS series," +
		" COUNT(*) AS commits FROM gh_commit WHERE $__timeFilter(time) AND " + RF +
		" AND gate <> 'none' GROUP BY 1, 2 ORDER BY 1"
	otherChecks := `SELECT app AS "App", COUNT(*) AS "Runs", check AS "Check",` +
		` SUM(failed) AS "Failed"` +
		" FROM gh_commit_check WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 20"

	checksGR, checksGRtf := gTbl(rowsOf(countOf(rp(cc, "checks")), gn(cc, "app"), gn(cc, "check")),
		"App, check", []col{{"sum", "Runs"}})
	checksES, checksEStf := esTbl(cc, []any{b.tm("app", 20), b.tm("check", 40)},
		[]any{b.mCount(), b.mSum("failed")},
		[]named{
			{"app.keyword", "App"},
			{"check.keyword", "Check"},
			{"n", "Runs"},
			{"f", "Failed"},
		}, []string{ESF})

	return []Panel{
		panel("timeseries", "Commits by gate state", 12, 7, 0, 20, []Target{sqlTS(gate)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				`sum by (gate) (increase(github_commits_total{gate!="none",%s}[1d]))`, PF,
			), "{{gate}}")},
			Opts:    mergeOpts(mergeOpts(Opts{"bars": true, "stack": true}, dayBins), bounded()),
			SQLOpts: seriesOpts,
			// The words are GitHub's own, so the colors hold in every store;
			// the palette had put FAILURE in green.
			Overrides: []any{
				colorOf("SUCCESS", "green"), colorOf("FAILURE", "red"),
				colorOf("PENDING", "yellow"), colorOf("ERROR", "dark-red"),
				colorOf("EXPECTED", "blue"), colorOf("not failed", "green"),
			},
			Desc: "Whether the commit itself came out green. A workflow run that failed says a " +
				"job failed, which is not the same claim: of fifty commits measured here, " +
				"twenty seven left the default branch red. " + commitBound + " " + bucketFollowsRange,
			PromDesc: sinceStart,
			// Graphite keeps no strings, so the state itself is out of reach
			// there; the two numbers beside it are not. checks_total exists
			// only on a commit a gate ran on, and checks_failed is 1 on the
			// ones that came out red, so the gated commits split into the
			// red and the rest.
			GR: []Target{
				grq(fmt.Sprintf(`alias(consolidateBy(summarize(sumSeries(%s), "1d", "sum"), "sum"), "FAILURE")`,
					rp("gh_commit", "checks_failed")), "A"),
				grq(fmt.Sprintf(`alias(consolidateBy(summarize(diffSeries(%s, sumSeries(%s)), "1d", "sum"), "sum"), "not failed")`,
					countOf(rp("gh_commit", "checks_total")), rp("gh_commit", "checks_failed")), "B"),
			},
			GRDesc: "Graphite keeps no strings, so the split is red against not red: " +
				"the commits whose gate failed, and the other commits a gate ran on.",
			ES: []Target{b.esDaily("gh_commit", b.mCount(), "gate", "",
				[]string{ESF, "NOT gate:none"}, "")},
		}),
		panel("table", "Checks that are not Actions", 12, 7, 12, 20, []Target{sqlT(otherChecks)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"sum by (app, conclusion) (increase(github_commit_checks_total{%s}[$__range]))", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"app": "App", "conclusion": "Check", "Value": "Runs",
			}, nil, nil)},
			Opts: Opts{"sort": "Runs"},
			Desc: "Everything Actions runs is already in the two sections above, in far more " +
				"detail. These are the other gates: the code quality service, the dependency " +
				"bot, the commit statuses an older integration still writes.",
			PromDesc:  sinceStart,
			Overrides: []any{barCell("Runs", "short", 120)},
			GR:        checksGR, GRTF: checksGRtf, GRDesc: grSlot,
			ES: checksES, ESTF: checksEStf,
		}),
		panel("table", "Commits behind a red branch", 24, 8, 0, 27, []Target{sqlT(
			`SELECT c.repo AS "Repository", c.time AS "When",` +
				` c.author AS "Author", c.headline AS "Commit",` +
				// The run's url beside the commit's: the commit is what went in,
				// the run is the page that says why it went red, and MAX picks
				// one of the failed runs where several share the commit. Its
				// number is the "#1483" a person names a run by.
				` r.failures AS "Failed runs", r.run_number AS "Run number",` +
				` c.url AS "Link", r.url AS "Run" FROM (` +
				"SELECT repo, author, time, oid, headline, url FROM gh_commit" +
				" WHERE $__timeFilter(time) AND " + RF + " AND gate = 'FAILURE') c" +
				" JOIN (SELECT repo, head_sha, COUNT(*) AS failures, MAX(url) AS url," +
				" MAX(run_number) AS run_number FROM gh_workflow_run" +
				" WHERE $__timeFilter(time) AND " + RF + " AND conclusion <> 'success'" +
				" GROUP BY 1, 2) r" +
				" ON c.repo = r.repo AND c.oid = r.head_sha" +
				// Newest first: the cap is 25 rows, and ordered by the first
				// column it kept the alphabetically last repositories instead.
				" ORDER BY 2 DESC LIMIT 25",
		)}, &P{
			PromNote: cannot("each commit that left the default branch red, with its message, "+
				"its author and how many runs failed on it.",
				"It is a join between two measurements on a commit hash, and the "+
					"exporter carries neither the hash nor the message: both are "+
					"fields, and the reduction to gauges keeps only numbers."),
			GRNote: cannot("the commits that left the branch red, joined to the runs that "+
				"failed on them.",
				"Graphite has no join and no strings, so neither the hash nor the "+
					"commit message exists there.", "graphite"),
			ESNote: cannot("the commits that left the branch red, joined to the runs that "+
				"failed on them.",
				"The two are separate indices and an aggregation runs inside one.",
				"elasticsearch"),
			Opts: bounded(),
			Desc: "This is the panel that `oid`, `head_sha` and the gate state were stored for. " +
				"Each of the three on its own says nothing; together they answer the question " +
				"a red branch actually raises, which is which commit did it. " + commitBound,
			Overrides: []any{
				when("When"), repoColumn(), width("Author", 130),
				width("Failed runs", 110), width("Run number", 110),
				linkOn("Repository"), linkColumnAs("Run", "Open a failed run"),
			},
		}),
	}
}
