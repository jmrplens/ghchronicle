package dashboards

import "fmt"

// ── Stars and forks ─────────────────────────────────────────────────────────

func stars(b *builder) []Panel {
	// The star counts read gh_star_day, the daily history GitHub serves for
	// every repository, and not gh_star: Stars gained over time in every
	// store but Prometheus, and Stars over time in InfluxDB and PostgreSQL.
	// Since July 2026 the stargazer list behind gh_star is served only to a
	// repository's admins and collaborators, so a count made from it left
	// out every repository the token holds neither role on, while the
	// history answers for all of them. Prometheus cannot follow: the
	// reduction skips gh_star_day, a history with no current value, so its
	// Stars gained still counts gh_star, and its curve, like Graphite's and
	// Elasticsearch's, is the gh_repo reading, which every repository has
	// as well. No panel reads both: the two disagree on the day (the history
	// buckets by GitHub's Pacific calendar day, gh_star by the UTC instant)
	// and on an unstar (the history takes it off, gh_star keeps the row), so
	// a panel adding one to the other would count a star twice. gh_star
	// stays the source of the names in Recent stars, which the history does
	// not carry.
	//
	// Eight repositories and a remainder, like every other stacked panel
	// here. Of the 17 repositories that gained a star in two years, nine
	// gained between one and three, against 40 and 37 for the two busiest:
	// the legend ran to 17 entries and hid 86 pixels of the plot behind them.
	perDay := topSeries("gh_star_day", "repo", "stars", "stars", RF)
	// The curve starts from the stars given before the range, so it is a
	// star count rather than the range's gains, on the same scale as the
	// Prometheus gauge. The forks are drawn the same way from gh_fork, each
	// dated when the fork was made: gh_repo.forks is a reading per sweep, and
	// a curve of it began the day the collector did and was a dot at the edge
	// over two years.
	//
	// $__timeFrom() rather than the first row inside the range, which is what
	// this counted up to before. The two are the same number, since no row
	// sits between the start of the range and the first row in it, but only
	// one of them is a bound: Grafana substitutes the macro with a timestamp
	// before the query is sent, where a subquery has no value until the query
	// runs and InfluxDB counts the files it would open while it plans. So the
	// old form read the whole table at every range. Measured on 2026-09-14,
	// gh_star: 991 files whatever the page said, against 918 at thirty days
	// and 187 at five years now, and the same count in all 48 comparisons of
	// the two forms across six ranges and four repository selections.
	//
	// The two bucket rows of zero are the curve's end points. A running count
	// over dated rows has a point only in a bucket that holds one, so the line
	// began at the first star of the range and stopped at the last: a fork
	// curve over a year whose last fork was in June was drawn to June and left
	// the rest of the range blank, which reads as collection having stopped
	// rather than as nothing having been forked since. A cumulative count is
	// defined at every instant, so both edges are anchored and the line is
	// drawn flat from the start of the range to its end at the value it holds
	// there. Adding nothing keeps it a count and not an interpolation, and the
	// GROUP BY folds an anchor that lands on a bucket that already exists,
	// which would otherwise be two points at one timestamp.
	//
	// `before` is what the rows ahead of the range add up to and `per` what a
	// bucket inside it adds, because a row is not worth the same in the two
	// tables: a fork is one row, a day of the star history carries its count
	// in `stars`. The COALESCE is load-bearing. COUNT over no rows is 0 but
	// SUM over no rows is NULL, and NULL plus the running sum is NULL at
	// every point, so a range that begins before a repository's first star
	// would draw no curve at all. The forks keep COUNT(*) on both sides and
	// their statement is what it was before the stars moved.
	//
	// The pre-range subquery over gh_star_day opens a file per day that
	// holds a star plus one per day of the thirty weeks every sweep
	// rewrites, zeros included: about six hundred on the account this was
	// written against, estimated rather than measured, where gh_star opened
	// 991.
	climb := func(table, name, before, per string) string {
		return "SELECT time, (SELECT " + before + " FROM " + table + " WHERE time < $__timeFrom()" +
			" AND " + RF + `) + SUM(n) OVER (ORDER BY time) AS "` + name + `" FROM (` +
			"SELECT time, SUM(n) AS n FROM (" +
			"SELECT " + timeBin + ", " + per + " AS n FROM " + table +
			" WHERE $__timeFilter(time) AND " + RF + " GROUP BY 1" +
			" UNION ALL SELECT $__timeFrom(), 0" +
			" UNION ALL SELECT $__timeTo(), 0) a GROUP BY 1) x ORDER BY time"
	}
	curve := climb("gh_star_day", "Stars", "COALESCE(SUM(stars), 0)", "SUM(stars)")
	forksTS := climb("gh_fork", "Forks", "COUNT(*)", "COUNT(*)")
	// user_url is the collector's, the one url the dashboards used to build
	// by hand: every other link is a value GitHub returned, so this one is too.
	newest := `SELECT user AS "User", time AS "Starred at", repo AS "Repository",` +
		` user_url AS "Link"` +
		" FROM gh_star WHERE $__timeFilter(time) AND " + RF + " ORDER BY time DESC LIMIT 50"
	// Twelve bars and the rest folded: the axis of a bar per repository was
	// cut off at the bottom of the panel.
	byRepo := otherRows(`SELECT repo AS "Repository", stars AS "Stars",`+
		" ROW_NUMBER() OVER (ORDER BY stars DESC) AS rn FROM ("+
		latestPerRepo([]string{"stars"})+") WHERE stars > 0", "Repository", "Stars", 12)
	// One series per repository, where gh_star had one per stargazer, and
	// the value is the day's count rather than a 1 to be counted.
	dayPath := rp("gh_star_day", "stars")
	dayRepo := gn("gh_star_day", "repo")

	byRepoGR, byRepoGRtf := gTbl(fmt.Sprintf("sortByMaxima(%s)",
		rowsOf(rp("gh_repo", "stars"), gn("gh_repo", "repo"))),
		"Repository", []col{{"lastNotNull", "Stars"}})
	byRepoES, byRepoEStf := esTbl("gh_repo", []any{b.tm("repo", 500)}, []any{b.mNewest("stars")},
		[]named{{"repo.keyword", "Repository"}, {"stars", "Stars"}}, []string{ESF})

	// Graphite keeps no names, so its twin of the newest stargazers was
	// always a count per repository; reading the history makes it the same
	// count Stars gained over time draws, repositories with a hidden list
	// included, rather than a count of its own.
	newestGR, newestGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, %d, "sum")`,
		dayPath, dayRepo), "Repository", []col{{"sum", "Stars"}})
	newestES, newestEStf := b.esRaw("gh_star", 50, []named{
		{"@timestamp", "Starred at"},
		{"user", "User"},
		{"repo", "Repository"},
		{"user_url", "Link"},
	}, []string{ESF})

	return []Panel{
		panel("timeseries", "Stars gained over time", box{W: 12, H: 8, X: 0, Y: 0}, []Target{sqlTS(perDay)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				"sum by (repo) (increase(github_stars_gained_total{%s}[1d]))", PF,
			), "{{repo}}")},
			Desc: "Stars per day from GitHub's daily star history, which it serves for " +
				"every repository whatever the token may see of its stargazers. Each day " +
				"is GitHub's Pacific calendar day, so a star given in a European morning " +
				"can sit a day before the UTC date it was given. An unstar of a star " +
				"given in the last thirty weeks is taken off the day it was given. Against " +
				"GitHub Enterprise Server, which does not serve the history, the bars are " +
				"empty. The eight repositories that gained the most in the range are " +
				"named; the rest are `other`. " + bucketFollowsRange,
			// Desc is about the history and is shared, so Prometheus, which
			// reads gh_star, would inherit every sentence of it about days,
			// unstars and GitHub Enterprise Server. Its first words set all
			// of them aside before saying what holds instead: its counter only
			// ever adds a stargazer it has seen, and GitHub Enterprise Server
			// serves the list.
			PromDesc: "None of that about the daily history holds in Prometheus, which " +
				"counts the stargazer list: an unstar is never taken off, GitHub " +
				"Enterprise Server has bars as well, and only a repository whose list the " +
				"token may read is here, since July 2026 GitHub serves that list only to " +
				"a repository's admins and collaborators. " + sinceStart + " Stars over " +
				"time holds every repository's count.",
			Opts:    mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts: seriesOpts,
			GR:      []Target{grq(perBucket(dayPath, dayRepo))},
			// Only the days that hold a star. The sums are the same without
			// the filter, but the terms bucket ranks repositories by document
			// count, and every repository writes a zero row for each day of
			// the thirty weeks a sweep re-reads: unfiltered, they all tie, so
			// past fifty repositories the ones kept are arbitrary, and below
			// it every starless one stands in the legend at zero, which the
			// SQL stores drop with HAVING.
			ES: []Target{b.esDaily("gh_star_day", b.mSum("stars"), "repo", "", []string{ESF, "stars:>0"}, "")},
		}),
		panel("timeseries", "Stars over time", box{W: 12, H: 8, X: 12, Y: 0}, []Target{sqlTS(curve)}, &P{
			Prom: []Target{promq(fmt.Sprintf("sum(github_repo_stars{%s})", PF), legend("Stars"))},
			// Shared by every store, and only InfluxDB and PostgreSQL rebuild
			// the curve from the history, so the sentences about it name the
			// two; the other three say what their curve is in their own note.
			Desc: "The star count as it climbed, holding its last value to the end of the " +
				"range. In InfluxDB and PostgreSQL it is rebuilt from GitHub's daily star " +
				"history for every repository, back to the first star, so widening the " +
				"range shows more of it. There it usually sits at or a little below the " +
				"star count GitHub shows, since that count also includes accounts GitHub " +
				"no longer lists, but a star given more than thirty weeks ago and later " +
				"taken back can stay in it, so it can also sit above. Against GitHub " +
				"Enterprise Server, which does not serve the history, the curve is empty " +
				"in those two stores. " + bucketFollowsRange,
			Opts: dayBins,
			PromDesc: "In Prometheus the curve is the repositories' star count as the " +
				"exporter read it, so it starts the day the exporter did and sits at " +
				"GitHub's count.",
			GR: []Target{grq(fmt.Sprintf(`alias(%s, "Stars")`,
				latestSum(rp("gh_repo", "stars"))))},
			GRDesc: grSnapshot,
			ES:     []Target{b.esSnapshotStack("gh_repo", "stars")},
			ESOpts: esStacked, ESDesc: esSnapshot,
		}),
		panel("barchart", "Stars by repository", box{W: 12, H: 8, X: 0, Y: 8}, []Target{sqlT(byRepo)}, &P{
			Prom:     []Target{promTbl(fmt.Sprintf("topk(12, sum by (repo) (github_repo_stars{%s}) > 0)", PF))},
			Desc:     "The twelve most starred; the rest are one bar called other.",
			PromDesc: "Prometheus shows the twelve and folds nothing.",
			PromTF:   []any{organize(map[string]string{"repo": "Repository", "Value": "Stars"}, nil, nil)},
			GR:       byRepoGR, GRTF: byRepoGRtf,
			ES: byRepoES, ESTF: byRepoEStf,
		}),
		panel("table", "Recent stars", box{W: 12, H: 8, X: 12, Y: 8}, []Target{sqlT(newest)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"sum by (repo) (increase(github_stars_gained_total{%s}[$__range])) > 0", PF,
			))},
			PromTF: []any{organize(map[string]string{"repo": "Repository", "Value": "Stars"}, nil, nil)},
			Desc: "The newest stargazers, one row each. A repository whose stargazer list " +
				"GitHub hides from this token (since July 2026 it serves the list only to " +
				"a repository's admins and collaborators) has its stars in the panels " +
				"above and no names here.",
			PromDesc: "Prometheus keeps no stargazer identity, so this is the stars gained " +
				"per repository over the range instead. " + sinceStart + " The table counts the " +
				"stargazer list, as Stars gained over time does here, so a repository " +
				"whose list is hidden is in neither and has its stars only in Stars over " +
				"time and Stars by repository.",
			Overrides: []any{when("Starred at"), rowLinkOn("User", "Open the profile")},
			PromOver:  []any{barCell("Stars", "short", 200)},
			GR:        newestGR, GRTF: newestGRtf,
			GRDesc: "Graphite has no way to sort by date, so this is the stars gained per " +
				"repository over the range instead, from the daily star history, which " +
				"counts every repository. GitHub Enterprise Server does not serve that " +
				"history, so against it this table is empty.",
			GROver: []any{barCell("Stars", "short", 200)},
			ES:     newestES, ESTF: newestEStf,
		}),
		panel("timeseries", "Forks over time", box{W: 24, H: 7, X: 0, Y: 16}, []Target{sqlTS(forksTS)}, &P{
			Prom: []Target{promq(fmt.Sprintf("sum(github_repo_forks{%s})", PF), legend("Forks"))},
			Desc: "The fork count as it climbed, a running count of the forks each dated " +
				"when it was made, so it reaches back to the first fork rather than to " +
				"the day the collector started, and holds its last value to the end of " +
				"the range. " + bucketFollowsRange,
			Opts:     dayBins,
			PromDesc: "In Prometheus the curve starts the day the exporter did.",
			GR: []Target{grq(fmt.Sprintf(`alias(%s, "Forks")`,
				latestSum(rp("gh_repo", "forks"))))},
			GRDesc: "In Graphite the curve is the daily fork count as collected, so it starts the day the collector did.",
			ES:     []Target{b.esSnapshotStack("gh_repo", "forks")},
			ESOpts: esStacked,
			ESDesc: "In Elasticsearch the curve is the daily fork count as collected, so it starts " +
				"the day the collector did: " + esStackNote,
		}),
	}
}
