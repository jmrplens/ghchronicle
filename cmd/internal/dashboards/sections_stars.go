package dashboards

import "fmt"

// ── Stars and forks ─────────────────────────────────────────────────────────

func stars(b *builder) []Panel {
	// Eight repositories and a remainder, like every other stacked panel
	// here. Of the 17 repositories that gained a star in two years, nine
	// gained between one and three, against 40 and 37 for the two busiest:
	// the legend ran to 17 entries and hid 86 pixels of the plot behind them.
	perDay := topSeries("gh_star", "repo", "1", "stars", RF)
	// The curve starts from the stars given before the range, so it is the
	// real star count and sits on the same scale as the Prometheus gauge. The
	// forks are drawn the same way from gh_fork, each dated when the fork was
	// made: gh_repo.forks is a reading per sweep, and a curve of it began the
	// day the collector did and was a dot at the edge over two years.
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
	climb := func(table, name string) string {
		return "SELECT time, (SELECT COUNT(*) FROM " + table + " WHERE time < $__timeFrom()" +
			" AND " + RF + `) + SUM(n) OVER (ORDER BY time) AS "` + name + `" FROM (` +
			"SELECT time, SUM(n) AS n FROM (" +
			"SELECT " + timeBin + ", COUNT(*) AS n FROM " + table +
			" WHERE $__timeFilter(time) AND " + RF + " GROUP BY 1" +
			" UNION ALL SELECT $__timeFrom(), 0" +
			" UNION ALL SELECT $__timeTo(), 0) a GROUP BY 1) x ORDER BY time"
	}
	curve := climb("gh_star", "Stars")
	forksTS := climb("gh_fork", "Forks")
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
	starPath := rp("gh_star", "starred")

	byRepoGR, byRepoGRtf := gTbl(fmt.Sprintf("sortByMaxima(%s)",
		rowsOf(rp("gh_repo", "stars"), gn("gh_repo", "repo"))),
		"Repository", []col{{"lastNotNull", "Stars"}})
	byRepoES, byRepoEStf := esTbl("gh_repo", []any{b.tm("repo", 500)}, []any{b.mNewest("stars")},
		[]named{{"repo.keyword", "Repository"}, {"stars", "Stars"}}, []string{ESF})

	newestGR, newestGRtf := gTbl(fmt.Sprintf(`groupByNode(isNonNull(%s), %d, "sum")`,
		starPath, gn("gh_star", "repo")), "Repository", []col{{"sum", "Stars"}})
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
			Desc: "Each star is dated by when it was given, recovered once per repository " +
				"with a full walk of the stargazer list and kept from then on. The eight " +
				"repositories that gained the most in the range are named; the rest are " +
				"`other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			Opts:     mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts:  seriesOpts,
			GR:       []Target{grq(perBucket("isNonNull("+starPath+")", gn("gh_star", "repo")))},
			ES:       []Target{b.esDaily("gh_star", b.mCount(), "repo", "", []string{ESF}, "")},
		}),
		panel("timeseries", "Stars over time", box{W: 12, H: 8, X: 12, Y: 0}, []Target{sqlTS(curve)}, &P{
			Prom: []Target{promq(fmt.Sprintf("sum(github_repo_stars{%s})", PF), legend("Stars"))},
			Desc: "The star count as it climbed. The rows go all the way back to the first " +
				"star, so widening the range shows more of the curve, and it holds its " +
				"last value to the end of the range. " + bucketFollowsRange,
			Opts:     dayBins,
			PromDesc: "In Prometheus the curve starts the day the exporter did.",
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
			Desc:   "The newest stargazers, one row each.",
			PromDesc: "Prometheus keeps no stargazer identity, so this is the stars gained " +
				"per repository over the range instead. " + sinceStart,
			Overrides: []any{when("Starred at"), rowLinkOn("User", "Open the profile")},
			PromOver:  []any{barCell("Stars", "short", 200)},
			GR:        newestGR, GRTF: newestGRtf,
			GRDesc: "Graphite has no way to sort by date, so this is the stars gained per " +
				"repository over the range instead.",
			GROver: []any{barCell("Stars", "short", 200)},
			ES:     newestES, ESTF: newestEStf,
		}),
		panel("timeseries", "Forks over time", box{W: 24, H: 7, X: 0, Y: 16}, []Target{sqlTS(forksTS)}, &P{
			Prom: []Target{promq(fmt.Sprintf("sum(github_repo_forks{%s})", PF), legend("Forks"))},
			Desc: "The fork count as it climbed, rebuilt from each fork's own date the way " +
				"the star curve is, so it reaches back to the first fork rather than to " +
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
