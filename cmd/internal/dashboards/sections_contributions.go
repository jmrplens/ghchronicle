package dashboards

import (
	"fmt"
	"strings"
)

// ── Contributions ───────────────────────────────────────────────────────────

// contributions is the profile's own account of the work, and then the split
// the profile itself never shows: which repository each green day came from.
func contributions(b *builder) []Panel {
	return append(contributionTotals(b), commitsPerRepository(b)...)
}

// perFieldRow is how a measurement whose fields are one row of a table is read
// out of Prometheus: one query per field, the rename that turns the panelValueA
// columns Grafana names them back into the field names, and the field names
// themselves, for the Graphite path and the Elasticsearch metric. Both tables
// below are that shape and differ only in the metric and the label the sum
// keeps.
func perFieldRow(metric, by string, cols []named) (
	prom []Target, rename map[string]string, fields []string,
) {
	rename = map[string]string{}
	fields = make([]string, len(cols))
	for i, c := range cols {
		prom = append(prom, promTbl("sum by ("+by+") ("+metric+c.From+")", string(rune('A'+i))))
		rename[fmt.Sprintf("Value #%c", 'A'+i)] = c.To
		fields[i] = c.From
	}
	return prom, rename, fields
}

// contributionTotals is the calendar, the weekly commits and the totals
// GitHub publishes about them, as the profile counts them.
func contributionTotals(b *builder) []Panel {
	dailySQL := `SELECT time, contributions AS "Contributions" FROM gh_contribution_day` +
		" WHERE $__timeFilter(time) ORDER BY time"
	// No date_bin: the collector stamps each row at the Sunday its week
	// starts on, so the rows are already one per week and the panel sums the
	// repositories at that stamp. A seven-day bin is aligned to the epoch, a
	// Thursday, and moved every Sunday three days back: the current week fell
	// out of a seven-day range and every week over two years was mislabeled.
	weekly := `SELECT time, SUM(commits) AS "All commits",` +
		` SUM(owner_commits) AS "Own commits" FROM gh_commits_week` +
		" WHERE $__timeFilter(time) AND " + RF + " GROUP BY 1 ORDER BY 1"

	// The punch card is a whole-life snapshot per repository, rewritten every
	// sweep, so summing the range counted every commit once per sweep. One row
	// per repository and bucket, newest first, and then the sum.
	punchSQL := func(node, name, order string) string {
		return fmt.Sprintf(`SELECT %s AS "%s", SUM(commits) AS "Commits" FROM (`+
			"SELECT %s, commits, ROW_NUMBER() OVER (PARTITION BY repo, %s"+
			" ORDER BY time DESC) AS rn FROM gh_commit_punchcard"+
			" WHERE $__timeFilter(time) AND %s) x WHERE rn = 1 GROUP BY 1 ORDER BY %s",
			node, name, node, node, RF, order)
	}
	byHour := punchSQL("hour", "Hour", "1")
	byDay := punchSQL("weekday", "Weekday", "2 DESC")
	// The owner is stripped from the name: this is the one table with
	// `owner/` in front of every repository, and on a phone the prefix was
	// all that fit.
	byRepo := `SELECT regexp_replace(repo, '^[^/]+/', '') AS "Repository", commits AS "Commits", url AS "Link" FROM (` +
		"SELECT repo, commits, url, ROW_NUMBER() OVER (PARTITION BY repo ORDER BY time DESC) AS rn" +
		" FROM gh_contribution_repo WHERE $__timeFilter(time) AND kind = 'commits') x WHERE rn = 1" +
		" ORDER BY 2 DESC LIMIT 25"
	totals := `SELECT commits AS "Commits", pull_requests AS "Pull requests",` +
		` reviews AS "Reviews", issues AS "Issues", repositories AS "New repositories",` +
		` restricted AS "Private" FROM gh_contributions_total` +
		" WHERE $__timeFilter(time) ORDER BY time DESC LIMIT 1"

	totalCols := []named{
		{"commits", "Commits"},
		{"pull_requests", planningPullRequests},
		{"reviews", "Reviews"},
		{"issues", "Issues"},
		{"repositories", "New repositories"},
		{"restricted", "Private"},
	}
	yearCols := []named{
		{"contributions", "Contributions"},
		{"commits", "Commits"},
		{"pull_requests", planningPullRequests},
		{"reviews", "Reviews"},
		{"issues", "Issues"},
		{"repositories", "New repos"},
	}
	punchcardWhy := "The exporter skips `gh_commit_punchcard`: on an account with eighteen " +
		"repositories it is 1,217 series, four fifths of the whole exporter, for " +
		"a distribution GitHub serves as one snapshot."
	// Named, or the transposed table's header reads "Field" and "1". The
	// first field of the frame is a number, so Grafana labels the one value
	// column with its row index and displays it "Last year 1"; the organize
	// after it takes the index off. Every store's frame arrives with a number
	// first, so the rename holds for all of them.
	transpose := []any{
		map[string]any{"id": "transpose", "options": map[string]any{
			"firstFieldName": "Metric", "restFieldsName": "Last year",
		}},
		organize(map[string]string{"Last year 1": "Last year"}, nil, nil),
	}
	weekPath := func(field string) string { return rp("gh_commits_week", field) }

	// The punch card is a whole-life snapshot per repository that only grows,
	// so the newest value is the largest and a max over the range reads it;
	// the repositories are then added together.
	punch := func(node, name string) (gr []Target, grtf []any, es []Target, estf []any) {
		gr, grtf = gTbl(fmt.Sprintf(`sortByName(groupByNode(keepLastValue(%s), %d, "sum"))`,
			rp("gh_commit_punchcard", "commits"), gn("gh_commit_punchcard", node)),
			name, []col{{"lastNotNull", "Commits"}})
		es, estf = esTbl("gh_commit_punchcard",
			[]any{b.tm(node, 24, "_term", "asc"), b.tm("repo", 500)}, []any{b.mMax("commits")},
			[]named{
				{node + ".keyword", name},
				{"repo.keyword", "Repository"},
				{"commits", "Commits"},
			}, []string{ESF},
			groupSum(name, "Commits", name, "Commits")...)
		return gr, grtf, es, estf
	}

	promTotals, totalRename, totalFields := perFieldRow("github_contributions_total_", "user", totalCols)
	totalsGR, totalsGRtf := gTbl(rowsOf(gp("gh_contributions_total",
		"{"+strings.Join(totalFields, ",")+"}"), 3), "Field", []col{{"lastNotNull", "Value"}})
	totalsES, totalsEStf := b.esRaw("gh_contributions_total", 1, totalCols, nil)

	byRepoGR, byRepoGRtf := gTbl(topRows(gp("gh_contribution_repo", "commits", "kind", "commits"),
		25, gn("gh_contribution_repo", "repo")), "Repository", []col{{"lastNotNull", "Commits"}})
	byRepoES, byRepoEStf := esTbl("gh_contribution_repo", []any{b.tm("repo", 25), b.tmURL()},
		[]any{b.mNewest("commits")},
		[]named{{"repo.keyword", "Repository"}, {"url.keyword", "Link"}, {"commits", "Commits"}},
		[]string{"kind:commits"})

	hourGR, hourGRtf, hourES, hourEStf := punch("hour", "Hour")
	dayGR, dayGRtf, dayES, dayEStf := punch("weekday", "Weekday")

	promYears, yearRename, yearFields := perFieldRow("github_contribution_year_", "year", yearCols)
	yearRename["year"] = "Year"
	yearGR, yearGRtf := gTbl(rowsOf(gp("gh_contribution_year", "contributions"),
		gn("gh_contribution_year", "year")), "Year", []col{{"lastNotNull", "Contributions"}})
	yearES, yearEStf := esTbl("gh_contribution_year", []any{b.tm("year", 50, "_term")},
		[]any{b.mNewest(yearFields...)},
		append([]named{{"year.keyword", "Year"}}, yearCols...), nil)

	return []Panel{
		panel("timeseries", "Contributions over time", box{W: 24, H: 7, X: 0, Y: 0}, []Target{sqlTS(dailySQL)}, &P{
			Prom: []Target{promq("github_contributions_total_calendar_total",
				legend("Contributions, rolling year"))},
			Desc: "The profile calendar, one bar per day at that day's own date. This is the " +
				"only place the green squares exist as data.",
			PromDesc: "Prometheus cannot hold the calendar, so this is the rolling-year total " +
				"as it moves: it falls when a busy week ages out of the window.",
			Opts:     Opts{"bars": true, "fill": 60, "legend": "hidden"},
			PromOpts: Opts{"bars": false, "legend": "bottom"},
			GR: []Target{grq(fmt.Sprintf(`alias(%s, "Contributions")`,
				gp("gh_contribution_day", "contributions")))},
			ES: []Target{esq("gh_contribution_day", []any{b.mSum("contributions")},
				[]any{b.dh()}, "A", nil, "Contributions")},
		}),
		contributionCalendar(),
		contributionMix(b),
		panel("timeseries", "Commits per week", box{W: 12, H: 7, X: 0, Y: 12}, []Target{sqlTS(weekly)}, &P{
			Prom: []Target{promq(fmt.Sprintf("sum(increase(github_commits_total{%s}[7d]))", PF),
				legend("All commits"), step("7d"))},
			PromDesc: sinceStart,
			Desc: "One bar per week, at the Sunday the week starts on, which is how GitHub " +
				"serves it and how the rows are stamped.",
			Opts: Opts{"bars": true},
			GR: []Target{
				grq(fmt.Sprintf(`alias(summarize(sumSeries(%s), "7d", "sum"), "All commits")`,
					weekPath("commits")), "A"),
				grq(fmt.Sprintf(`alias(summarize(sumSeries(%s), "7d", "sum"), "Own commits")`,
					weekPath("owner_commits")), "B"),
			},
			ES: []Target{
				esq("gh_commits_week", []any{b.mSum("commits")}, []any{b.dh("7d")}, "A",
					[]string{ESF}, "All commits"),
				esq("gh_commits_week", []any{b.mSum("owner_commits")}, []any{b.dh("7d")}, "B",
					[]string{ESF}, "Own commits"),
			},
		}),
		panel("table", "Contribution totals", box{W: 12, H: 7, X: 12, Y: 12}, []Target{sqlT(totals)}, &P{
			Desc: "The last year, as the profile counts it: the same numbers GitHub shows " +
				"under the contribution calendar.",
			Prom:   promTotals,
			SQLTF:  transpose,
			PromTF: append(merged(totalRename, []string{"user"}, nil), transpose...),
			// One series per field, so the rows Graphite produces are the
			// transposed table the other stores arrive at.
			GR: totalsGR, GRTF: totalsGRtf,
			ES: totalsES, ESTF: append(totalsEStf, transpose...),
		}),
		panel("barchart", "Commits by hour of day", box{W: 12, H: 7, X: 0, Y: 19}, []Target{sqlT(byHour)}, &P{
			PromNote: cannot("commits by hour of the day, over the whole life of each "+
				"repository.", punchcardWhy),
			// Every other hour labeled: twenty four labels at phone width ran
			// together as one string of digits.
			Opts: Opts{"horizontal": false, "tick_spacing": 100},
			Desc: "The whole life of each repository, not the selected range: GitHub serves " +
				"this distribution as a single snapshot.",
			GR: hourGR, GRTF: hourGRtf,
			ES: hourES, ESTF: hourEStf,
		}),
		panel("barchart", "Commits by weekday", box{W: 6, H: 7, X: 12, Y: 19}, []Target{sqlT(byDay)}, &P{
			PromNote: cannot("commits by weekday, over the whole life of each repository.",
				punchcardWhy),
			GR: dayGR, GRTF: dayGRtf,
			ES: dayES, ESTF: dayEStf,
		}),
		panel("table", "Commits by repository", box{W: 6, H: 7, X: 18, Y: 19}, []Target{sqlT(byRepo)}, &P{
			Prom:      []Target{promTbl(`topk(25, sum by (repo) (github_contribution_repo_commits{kind="commits"}))`)},
			PromTF:    []any{organize(map[string]string{"repo": "Repository", "Value": "Commits"}, nil, nil)},
			Opts:      Opts{"sort": "Commits"},
			Overrides: []any{barCell("Commits", "short", 120), linkOn("Repository")},
			Desc: "Commits this account made in each repository over the last year, as the " +
				"profile counts them.",
			GR: byRepoGR, GRTF: byRepoGRtf,
			ES: byRepoES, ESTF: byRepoEStf,
		}),
		panel("table", "Contributions by year", box{W: 24, H: 8, X: 0, Y: 26}, []Target{sqlT(
			`SELECT year AS "Year", contributions AS "Contributions",` +
				` commits AS "Commits", pull_requests AS "Pull requests",` +
				` reviews AS "Reviews", issues AS "Issues", repositories AS "New repos"` +
				" FROM gh_contribution_year WHERE " + wholeHistory +
				" ORDER BY year DESC",
		)}, &P{
			Prom:      promYears,
			PromTF:    merged(yearRename, nil, nil),
			Opts:      Opts{"sort": "Year"},
			Desc:      yearsDesc,
			Overrides: []any{width("Year", 100), barCell("Contributions", "short", 150)},
			GR:        yearGR, GRTF: yearGRtf, GRDesc: grRange + " " + grRows,
			ES: yearES, ESTF: yearEStf, ESDesc: esRange,
		}),
	}
}

// yearsDesc is the Contributions by year panel's own prose, kept out of the
// function that builds the section only for its size.
const yearsDesc = "Every year, backfilled at one GraphQL point per year, reaching back to " +
	"the day the account was created; the year in progress is here too, rewritten " +
	"daily until it ends. A table rather than a chart because the past years sit " +
	"outside any dashboard time range you would pick for the rest of the page."

// commitsPerRepository is gh_contribution_day_repo: the green calendar split
// per repository per day, and the only surface that sees the private and
// third-party repositories a per-repository sweep never touches.
//
// `repo` here is owner/name rather than the bare name every per-repository
// measurement carries, so the $repo variable would match none of it: neither
// panel takes a repository filter in any store, the way "Commits by
// repository" above does not.
//
// The rows converge. Every sweep rewrites the same day of the same repository
// with the same tags, in Elasticsearch too, which derives a document id from
// the point's identity, so a plain date_bin plus SUM is exact and none of the
// newest-row-per-series machinery above applies here. What would end that is a
// tag changing value: a repository renamed, transferred or turned public
// writes its whole window again under a second tag set, and both sets survive
// at the same timestamps. The row carries no collection time to prefer the
// newer set by, so that is a property of the measurement rather than something
// a query can undo.
func commitsPerRepository(b *builder) []Panel {
	dayRepo := "gh_contribution_day_repo"
	perRepoDay := topSeries(dayRepo, "repo", "commits", "commits", "commits > 0")
	hidden := `SELECT CASE WHEN private = 'true' AND own = 'true' THEN 'Yours, private'` +
		` WHEN private = 'true' THEN 'Somebody else''s, private'` +
		` WHEN own = 'true' THEN 'Yours, public'` +
		` ELSE 'Somebody else''s, public' END AS "Where",` +
		` SUM(commits) AS "Commits" FROM ` + dayRepo +
		" WHERE $__timeFilter(time) GROUP BY 1 ORDER BY 2 DESC"

	why := "The exporter skips `gh_contribution_day_repo` for the reason it skips " +
		"`gh_contribution_day`: it is history with no current value, and counting it would " +
		"mint a series per repository per day. There is no such metric to look for."
	cap100 := "GraphQL caps the daily breakdown at a hundred repositories per window " +
		"and, inside each, a hundred distinct commit days returned newest first, so a " +
		"busier repository loses its oldest days silently: " +
		"`gh_contribution_repo.commits` minus `commits_dated` is exactly how many " +
		"commits never got a date."

	// Graphite names each bar from the path nodes it grouped by, so the label
	// is the two tag values joined by a dot, `own` first.
	hiddenGR, hiddenGRtf := gTbl(fmt.Sprintf(`groupByNodes(%s, "sum", %d, %d)`,
		gp(dayRepo, "commits"), gn(dayRepo, "own"), gn(dayRepo, "private")),
		"Where", []col{{"sum", "Commits"}})
	// One bucket, because a bar's label in Elasticsearch is a bucket's key and
	// a second bucket is a second column rather than part of that label. So
	// this store answers the half of the split the panel is named after, and
	// says so in its own description.
	hiddenES, hiddenEStf := esTbl(dayRepo, []any{b.tm("private", 2)}, []any{b.mSum("commits")},
		[]named{{"private.keyword", "Private"}, {"commits", "Commits"}}, nil)

	return []Panel{
		panel("timeseries", "Commits by repository, dated", box{W: 16, H: 8, X: 0, Y: 34},
			[]Target{sqlTS(perRepoDay)}, &P{
				PromNote: cannot("one bar per day, split by the repository the commits "+
					"belong to.", why),
				Opts: mergeOpts(Opts{"bars": true, "stack": true}, dayBins), SQLOpts: seriesOpts,
				Desc: "Where the green came from, for the part of it that is commits. The " +
					"eight repositories with the most commits in the range are named; the " +
					"rest are `other`. " +
					`"Contributions over time" at the top of this section counts issues, ` +
					"pull requests and reviews as well, so this stack sits under that " +
					"curve rather than repeating it: a whole year of these days sums to " +
					"`gh_contributions_total.commits`, the Commits of the totals above. " +
					"The repositories include the private and third-party ones, which " +
					`"Commits by repository" lists too, undated and unflagged. ` + cap100 +
					" " + bucketFollowsRange,
				GR: []Target{grq(perBucket(gp(dayRepo, "commits"), gn(dayRepo, "repo")))},
				ES: []Target{b.esDaily(dayRepo, b.mSum("commits"), "repo", "", nil, "")},
			}),
		panel("barchart", "Commits the profile hides", box{W: 8, H: 8, X: 16, Y: 34},
			[]Target{sqlT(hidden)}, &P{
				PromNote: cannot("commits over the range split by whether the repository is "+
					"private and whether it is yours.", why),
				Desc: "Commits the public profile cannot see. A visitor sees none of the " +
					"private half: a private repository is " +
					"not on the profile at all, and its commits reach the green calendar " +
					"only as the anonymous count the totals above call Private. The half " +
					"somebody else owns is public work that no list of this account's " +
					"repositories carries. Measured on this account, 8 of the 39 " +
					"repositories with commits are private and 6 belong to somebody " +
					"else. " + cap100,
				GR: hiddenGR, GRTF: hiddenGRtf,
				GRDesc: "Graphite labels each bar from the path nodes it grouped by, so the " +
					"two tag values arrive joined by a dot, `own` first.",
				ES: hiddenES, ESTF: hiddenEStf,
				ESDesc: "In Elasticsearch a bar is labeled by a bucket key, and a second " +
					"bucket would be a second column rather than part of the label, so this " +
					"is the private split alone with both owners added together.",
			}),
	}
}

// contributionsCalendarGrid is what the three stores that cannot draw the
// calendar are each missing, said the same way in all three notes.
const contributionsCalendarGrid = "the contribution calendar as GitHub draws it: a column per week, " +
	"a row per weekday, each cell shaded by that day's count."

// contributionCalendar is the profile's calendar as GitHub draws it, and the
// two SQL stores are the ones that can draw it: the other three say why not.
//
// Every number here is measured, on 2026-09-14, against the grid GitHub draws
// on the profile page itself: cells of ten pixels square with a three pixel
// gutter, 53 columns of seven rows, Monday, Wednesday and Friday named and the
// other four rows blank, the dark theme's four greens over a gray day, and a
// shade that is a function of the day's own count alone.
func contributionCalendar() Panel {
	// The calendar as GitHub draws it: one column per week, one row per
	// weekday, the cell colored by the day's count. The week is binned from
	// a Sunday, the day GitHub starts its columns on, and the weekday of a
	// row is date_part's, Sunday first. A row of the frame is a week, a
	// field a weekday, which is the grid the status history panel draws.
	calendar := "SELECT " + sundayWeek + " AS time," + weekdayColumns() +
		" FROM (SELECT time, date_part('dow', time) AS dow, " + calendarLevel +
		" AS level FROM gh_contribution_day WHERE $__timeFilter(time)) d GROUP BY 1 ORDER BY 1"
	return panel("status-history", "Contribution calendar", box{W: 10, H: 5, X: 0, Y: 7}, []Target{sqlT(calendar)}, &P{
		PromNote: cannot(contributionsCalendarGrid,
			"The exporter skips `gh_contribution_day`: it is history with no current "+
				"value, and a day of it a year ago is not a sample Prometheus can hold."),
		GRNote: cannot(contributionsCalendarGrid,
			"Graphite can bucket a series by week and cannot split it by weekday, and "+
				"the grid needs both at once.", "graphite"),
		ESNote: cannot(contributionsCalendarGrid,
			"A date histogram buckets by one interval, and the grid needs the week "+
				"along one axis and the weekday along the other.", "elasticsearch"),
		Desc: calendarDesc,
		// The one thing about the x axis a panel can set: the format a tick
		// is written in. Grafana chooses where the ticks go, see
		// calendarTickFormat.
		Overrides: []any{unitOf(calendarXField, calendarTickFormat, 0)},
		Opts: Opts{
			"thresholds": calendarShades, "unit": "none",
			// The cell GitHub draws, measured: ten pixels square with a gutter
			// of three. A status history cell is a fraction of the space one
			// column and one row get, so the fractions are what make the cell
			// square at this size, and the size is what makes 53 weeks of ten
			// pixel cells fit: at the full width of the dashboard the same
			// grid is a wall of cells 28 by 20, which is the shape the owner
			// said does not look like GitHub's.
			"col_width": 0.77, "row_height": 0.66,
			// GitHub's grid is an object of one year and the profile never
			// draws another range: at five years these 53 columns become 262
			// of six pixels, and at a week two bars. The panel keeps its own
			// year whatever the dashboard range is, which is also what makes
			// the shade below the year's own fifths rather than the range's.
			"time_from": calendarRange,
			// What the tooltip can say. The panel colors by the value it
			// carries, so the value is the level and the count cannot also be
			// in it; the level is at least named honestly, and Less and More
			// mark the ends the way the profile's key does.
			"mappings": []any{map[string]any{"type": "value", "options": map[string]any{
				"0": map[string]any{"text": "No contributions", "index": 0},
				"1": map[string]any{"text": "Level 1 (Less)", "index": 1},
				"2": map[string]any{"text": "Level 2", "index": 2},
				"3": map[string]any{"text": "Level 3", "index": 3},
				"4": map[string]any{"text": "Level 4 (More)", "index": 4},
			}}},
		},
	})
}

// calendarRange is the window the grid draws whatever the dashboard range is,
// the profile's own: a year of weeks, 53 columns.
const calendarRange = "1y"

// calendarXField is the column the weeks arrive in, which both SQL stores
// name `time`, and which the tick format below is hung on.
const calendarXField = "time"

// calendarTickFormat is what a tick under the grid says: the Sunday its
// column starts on, "5 Oct", and not the month.
//
// GitHub names each month once, over the first week of that month. A status
// history cannot: it builds the x ticks itself, one every Nth column, where N
// is uPlot's own increment divided by the gap between the first two columns
// (`xSplits`, Grafana 13.2.1). Every tick therefore lands on a week, the
// stride is the same all the way across, and how many there are is a function
// of the pixel width alone. Measured on the published panel on 2026-09-14: a
// tick every 10 weeks at 1920 in its place on the dashboard, every 3 weeks
// maximized, where seventeen ticks written as months read 2025-10, 2025-10,
// 2025-11, 2025-12, 2025-12 and on, five of the twelve months printed twice.
//
// A unit of `time:` is the one thing the panel can say about that axis: it
// makes the tick carry the field's own display instead of Grafana's interval
// format (`formatValue` in the timeline's addAxis). Naming the week's Sunday
// is what makes a repeat impossible at any width, since no two columns start
// on the same day, and it still names every month the ticks fall in: at 1920
// maximized the seventeen now read 5 Oct, 26 Oct, 16 Nov, 7 Dec and on, all
// twelve months among them and no label twice.
//
// The day leads because the rightmost tick is drawn centered on the last
// column and cut off at the panel's edge, so the half that survives has to
// read as unfinished rather than as another date. Measured at 430 on
// 2026-09-14, where the last column is the week of 13 September: written
// "MMM D" the axis drew "Sep 13" and showed "Sep 1", a whole date twelve days
// off, where the interval format it replaced at least showed "2026-" for
// "2026-09"; written "D MMM" it shows "13 Se". The ticks themselves are the
// same either way, four at 430 and seventeen maximized at 1920, because the
// format says what a tick reads and not where it falls.
const calendarTickFormat = "time:D MMM"

// calendarLevel is a day's shade, and it is GitHub's rule rather than a
// quartile of our own.
//
// Measured on 2026-09-14 against the 366 squares of the profile page: the
// boundaries fall at a fifth, two fifths and three fifths of the busiest day
// of the window, and applying exactly this to GitHub's own counts reproduced
// all 366 of its shades. The quartiles the panel used to compute with NTILE
// painted 54 days the darkest green where the profile paints 14, and gave the
// counts 4, 14 and 44 two different shades each, because a quartile is a rank
// among the days that had anything and not a function of the count.
//
// `gh_contribution_day.level` is not this, which is why it is not drawn here:
// it is GitHub's GraphQL `contributionLevel`, a quartile computed against the
// window the sweep that wrote the row asked for, and on the same 366 days it
// disagreed with the profile's own picture on 33 of them, 8 of those in the
// darkest green. It stays in the row as what GitHub said; the grid draws what
// GitHub draws.
const calendarLevel = "CASE WHEN contributions = 0 THEN 0" +
	" WHEN contributions * 5 >= 3 * " + calendarMax + " THEN 4" +
	" WHEN contributions * 5 >= 2 * " + calendarMax + " THEN 3" +
	" WHEN contributions * 5 >= " + calendarMax + " THEN 2 ELSE 1 END"

// calendarMax is the busiest day of the window the grid draws, the number
// the four bands are fifths of.
const calendarMax = "MAX(contributions) OVER ()"

// calendarDesc is the panel's own prose, kept out of the function that builds
// it only for its size.
const calendarDesc = "The profile's green squares, as GitHub draws them: a column per week " +
	"starting on Sunday, a row per weekday with Monday, Wednesday and Friday named " +
	"as the profile names them, and a cell shaded by that day's own count. The " +
	"four greens are GitHub's dark theme over the gray of a day with nothing, and " +
	"the bands are the fifths of the busiest day of the year, which is the rule the " +
	"profile page itself draws by: applied to GitHub's own counts it reproduced all " +
	"366 of its squares on 2026-09-14. The grid is the last year whatever range the " +
	"dashboard is set to, because that is the only range this shape is legible at " +
	"and the only one the profile has; the count itself, at any range, is the curve " +
	"above. Three things the panel cannot copy from the page. The axis names the week " +
	"each tick stands on, 5 Oct and 26 Oct, instead of naming a month over its first " +
	"week: Grafana builds the ticks itself, one every few columns, so a month written " +
	"there is printed twice as soon as the panel is wide, and the week it stands on " +
	"never is. The cell is a fraction of the band its row gets, not ten pixels: " +
	"the square is square at the size the panel is on the dashboard, and maximized, " +
	"where the panel fills the screen, the same cells stretch into tall bars, so the " +
	"grid is meant to be read in its place. And a cell's tooltip names the shade and " +
	"the Sunday its column starts on rather than the day and the count: the panel " +
	"colors by the value it shows, so the value has to be the shade, and a column is " +
	"a week."

// contributionMix is the radar the profile draws, as four shares of one sum.
func contributionMix(b *builder) Panel {
	// The four kinds of contribution as shares of their sum, the mix
	// GitHub draws as a radar on the profile. Computed here, so every store
	// hands the panel a percentage and the panel needs no arithmetic.
	mixOf := func(field string) string {
		return fmt.Sprintf("100.0 * %s / %s", field, mixTotal)
	}
	mix := `SELECT ` + mixOf("commits") + ` AS "Commits", ` + mixOf("pull_requests") +
		` AS "Pull requests", ` + mixOf("issues") + ` AS "Issues", ` + mixOf("reviews") +
		` AS "Code review" FROM gh_contributions_total WHERE $__timeFilter(time)` +
		" AND " + mixTotal + " > 0 ORDER BY time DESC LIMIT 1"
	// Graphite: each of the four fields as a percentage of the four summed,
	// the newest value of each.
	mixPath := gp("gh_contributions_total", "{commits,pull_requests,issues,reviews}")
	mixGR := make([]Target, len(mixParts))
	for i, part := range mixParts {
		mixGR[i] = grNamed(ref(i), part.To, fmt.Sprintf("asPercent(keepLastValue(%s), sumSeries(keepLastValue(%s)))",
			gp("gh_contributions_total", part.From), mixPath))
	}
	mixProm := make([]Target, len(mixParts))
	for i, part := range mixParts {
		mixProm[i] = promNamed(ref(i), part.To, fmt.Sprintf(
			"100 * github_contributions_total_%s / (github_contributions_total_commits"+
				" + github_contributions_total_pull_requests + github_contributions_total_issues"+
				" + github_contributions_total_reviews)", part.From,
		))
	}
	// Elasticsearch hands back the four counts of the newest document, and
	// the percentages are the panel's own arithmetic: the sum of the row,
	// then each count over it, then the counts and the sum dropped.
	mixES, mixEStf := esTbl("gh_contributions_total", []any{b.one()},
		[]any{b.mNewest(fieldsOf(mixParts)...)}, mixParts, nil)
	mixEStf = append(mixEStf, mixShares()...)
	return panel("bargauge", "Contribution mix (last year)", box{W: 14, H: 5, X: 10, Y: 7}, []Target{sqlT(mix)}, &P{
		Desc: "The four kinds of contribution as shares of their sum over the last " +
			"year, the mix the profile draws as a radar: commits, pull requests, " +
			"issues and code review. The percentages are computed from the totals " +
			"beside this, and the four add up to a hundred.",
		Prom: mixProm,
		GR:   mixGR,
		ES:   mixES, ESTF: mixEStf, ESOpts: Opts{"unit": "percentunit", "maxv": 1.0},
	})
}

// sundayWeek bins a time to the Sunday that starts its week. date_bin aligns
// to the epoch, a Thursday, unless given an origin, and the fourth of January
// 1970 is the first Sunday after it. toPG carries the spelling PostgreSQL's
// date_bin takes.
const sundayWeek = "date_bin(INTERVAL '7 days', time, TIMESTAMP '1970-01-04T00:00:00Z')"

// calendarRows is the grid's rows top to bottom, in the numbering
// date_part('dow') uses, named as the frame's columns.
//
// GitHub labels three of the seven, Monday, Wednesday and Friday, and hides
// the other four labels behind a zero radius clip path; the row is still
// there. A status history names a row by its field, so the four unnamed rows
// are one to four spaces: distinct names, since two fields of a frame cannot
// share one, and nothing to read. Seven labels took a third of the panel's
// width on a phone.
var calendarRows = []string{" ", "Mon", "  ", "Wed", "   ", "Fri", "    "}

// weekdayColumns is one column per weekday, each the level of that day of
// the week: a week has one row per day, so the SUM is that one value, and
// a day with no row leaves the cell null rather than zero, which is how a
// day still to come stays blank.
func weekdayColumns() string {
	cols := make([]string, len(calendarRows))
	for i, day := range calendarRows {
		cols[i] = fmt.Sprintf(" SUM(CASE WHEN dow = %d THEN level END) AS %q", i, day)
	}
	return strings.Join(cols, ",")
}

// calendarShades is GitHub's own palette for the dark theme, the one the
// profile draws on the page this grid is copied from, pale to bright over an
// empty day.
//
// The four greens are read off the page rather than remembered: the computed
// background of a `data-level` 1 to 4 square on github.com/jmrplens in the
// dark theme on 2026-09-14, and the same four counted among the pixels of a
// screenshot of that grid. They are not the palette this panel carried until
// then (`#0e4429`, `#006d32`, `#26a641`, `#39d353`), which is the ramp GitHub
// drew before it repainted the calendar; level 1 in particular went from
// `#0e4429` to a much darker `#033a16`.
//
// The empty day is not GitHub's `#151b23`: a Grafana panel's background is
// `#181b1f`, lighter than the `#0d1117` of the profile, and on it that color
// is darker than the panel and the empty days disappear, taking the grid's
// rectangle with them. `#21262d` is the same step above this background that
// GitHub's is above its own. That one deliberate lift is the whole
// difference from `render.darkHeat`, which carries the same four greens over
// GitHub's own `#151b23`: the two ramps are measured together and move
// together. The light palette was here before and read as a much brighter
// grid than the profile's, because four pale greens on a dark panel are a
// different picture from four pale greens on a white page.
var calendarShades = []any{
	map[string]any{"color": "#21262d", "value": nil},
	map[string]any{"color": "#033a16", "value": 1},
	map[string]any{"color": "#196c2e", "value": 2},
	map[string]any{"color": "#2ea043", "value": 3},
	map[string]any{"color": "#56d364", "value": 4},
}

// mixParts is the four kinds of contribution the profile's radar plots, in
// its order, and the field each is read from.
var mixParts = []named{
	{"commits", "Commits"},
	{"pull_requests", planningPullRequests},
	{"issues", "Issues"},
	{"reviews", "Code review"},
}

// mixTotal is the four added, the denominator of every share.
const mixTotal = "(commits + pull_requests + issues + reviews)"

// mixShares turns the four counts of one row into four shares of their sum,
// for the store that returns counts and no arithmetic: the row summed into
// Total, each count divided by it under its own name, and the counts and
// the sum dropped so the panel draws the four shares alone.
func mixShares() []any {
	names := make([]any, len(mixParts))
	for i, part := range mixParts {
		names[i] = part.To
	}
	tf := []any{map[string]any{"id": "calculateField", "options": map[string]any{
		"mode": "reduceRow", "alias": "Total",
		"reduce": map[string]any{"reducer": "sum", "include": names},
	}}}
	exclude := map[string]any{"Total": true}
	for _, part := range mixParts {
		exclude[part.To] = true
		tf = append(tf, map[string]any{"id": "calculateField", "options": map[string]any{
			"mode": "binary", "alias": part.To + " share",
			"binary": map[string]any{"left": part.To, "operator": "/", "right": "Total"},
		}})
	}
	rename := map[string]any{}
	for _, part := range mixParts {
		rename[part.To+" share"] = part.To
	}
	return append(tf, map[string]any{"id": "organize", "options": map[string]any{
		"excludeByName": exclude, "indexByName": map[string]any{}, "renameByName": rename,
	}})
}
