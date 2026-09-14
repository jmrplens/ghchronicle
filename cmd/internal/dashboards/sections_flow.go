package dashboards

import "fmt"

// ── Pull requests and issues ────────────────────────────────────────────────

// The measurement this section is mostly about, and the ways its panels
// address it: the path with the state pinned to merged, the path with any
// state, and the Prometheus and Lucene filters for a merged pull request.
// Both halves of the section need all four.
const pr = "gh_pull_request"

func mergedPath(field string) string { return rp(pr, field, "state", "MERGED") }

func anyPath(field string) string { return rp(pr, field) }

var (
	promMerged = `state="MERGED",` + PF
	esMerged   = []string{"state:MERGED", ESF, esIdentified}
)

// stateWord is the legend a per-day panel gives each state of a pull request
// or an issue: a word rather than the tag's MERGED, and "open that day" for
// the daily rows an open one writes, which count a state and not an event.
const stateWord = "CASE WHEN state = 'MERGED' THEN 'Merged' WHEN state = 'CLOSED' THEN 'Closed'" +
	" ELSE 'Open that day' END"

// stateOverrides draws those three the way each is meant: the two events as
// stacked bars in the colors the words carry, the open count as a line on its
// own, outside the stack. They match the words the SQL spells, so they go to
// the two SQL stores alone.
var stateOverrides = []any{
	colorOf("Merged", "purple"),
	colorOf("Closed", "red"),
	override("Open that day", []any{
		map[string]any{"id": "custom.drawStyle", "value": "line"},
		map[string]any{"id": "custom.stacking", "value": map[string]any{"mode": "none", "group": "A"}},
		map[string]any{"id": "custom.fillOpacity", "value": 0},
		map[string]any{"id": "color", "value": map[string]any{"mode": "fixed", "fixedColor": "orange"}},
	}),
}

// flow is how much moved and how fast, and then which pull requests and
// which people moved it.
func flow(b *builder) []Panel {
	return append(flowRates(b), pullsAndReviewers(b)...)
}

// flowRates is the volume and the pace: how many pull requests and issues
// closed, how long each took, and the same numbers per day.
func flowRates(b *builder) []Panel {
	median := func(table, field, where string) string {
		return fmt.Sprintf("SELECT approx_percentile_cont(%s, 0.5) AS value FROM %s"+
			" WHERE $__timeFilter(time) AND %s AND %s", field, table, RF, where)
	}
	mergeTime50 := median(pr, "seconds_to_merge", "state = 'MERGED'")
	reviewTime50 := median(pr, "seconds_to_first_human_review",
		"seconds_to_first_human_review IS NOT NULL")
	prSize50 := median(pr, "churn", "state = 'MERGED'")
	mergedPRs := "SELECT COUNT(*) AS value FROM gh_pull_request WHERE $__timeFilter(time)" +
		" AND " + RF + " AND " + identified + " AND state = 'MERGED'"
	closedIssues := "SELECT COUNT(*) AS value FROM gh_issue WHERE $__timeFilter(time)" +
		" AND " + RF + " AND " + identified + " AND state = 'CLOSED'"
	issueClose := "SELECT approx_percentile_cont(seconds_to_close, 0.5) AS value FROM gh_issue" +
		" WHERE $__timeFilter(time) AND " + RF + " AND state = 'CLOSED'"
	// A closed pull request is one row dated when it closed; an open one is a
	// row per day at midnight while it stays open, and those rows are not
	// retired when it closes. So the open series counts distinct numbers per
	// bucket and reads "open that day", and it is drawn as a line beside the
	// stack of what actually happened rather than stacked on top of it: the
	// stack read 45 open pull requests in a week that had 28.
	perDay := "SELECT " + timeBin + ", " + stateWord + " AS series," +
		" COUNT(DISTINCT number) AS pulls FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF +
		" AND " + identified + " GROUP BY 1, 2 ORDER BY 1"
	mergeTime := "SELECT " + timeBin + "," +
		` approx_percentile_cont(seconds_to_merge, 0.5) AS "Median",` +
		` approx_percentile_cont(seconds_to_merge, 0.9) AS "90th percentile"` +
		" FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF +
		" AND state = 'MERGED' GROUP BY 1 ORDER BY 1"
	sizeTime := "SELECT " + timeBin + "," +
		` approx_percentile_cont(churn, 0.5) AS "Median lines changed"` +
		" FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF +
		" AND state = 'MERGED' GROUP BY 1 ORDER BY 1"
	issuesDay := "SELECT " + timeBin + ", " + stateWord + " AS series," +
		" COUNT(DISTINCT number) AS issues FROM gh_issue WHERE $__timeFilter(time) AND " + RF +
		" AND " + identified + " GROUP BY 1, 2 ORDER BY 1"
	issuePath := func(field string) string { return rp("gh_issue", field) }

	return []Panel{
		// Six values in one panel rather than six tiles: each tile was its
		// own 144 pixel panel on a phone and the six opened the section with
		// nothing else on the screen. Three of them are durations, so those
		// name their unit on their own field.
		statGroup("Merged and closed in range", 24, 5, 0, 0, []Target{
			sqlT(namedValue(mergedPRs, "Pull requests merged")),
			{Kind: "sql", Format: "table", Ref: "B", SQL: namedValue(mergeTime50, "Time to merge")},
			{Kind: "sql", Format: "table", Ref: "C", SQL: namedValue(reviewTime50, "Time to first review")},
			{Kind: "sql", Format: "table", Ref: "D", SQL: namedValue(closedIssues, "Issues closed")},
			{Kind: "sql", Format: "table", Ref: "E", SQL: namedValue(issueClose, "Time to close an issue")},
			{Kind: "sql", Format: "table", Ref: "F", SQL: namedValue(prSize50, "Lines per pull request")},
		}, &P{
			Prom: []Target{
				promNamed("A", "Pull requests merged", fmt.Sprintf(
					"sum(increase(github_pull_requests_total{%s}[$__range]))", promMerged,
				)),
				promNamed("B", "Time to merge", fmt.Sprintf(
					"avg(github_pull_requests_seconds_to_merge_mean{%s})", promMerged,
				)),
				promNamed("C", "Time to first review", fmt.Sprintf(
					"avg(github_pull_requests_seconds_to_first_human_review_mean{%s})", PF,
				)),
				promNamed("D", "Issues closed", fmt.Sprintf(
					`sum(increase(github_issues_total{state="CLOSED",%s}[$__range]))`, PF,
				)),
				promNamed("E", "Time to close an issue", fmt.Sprintf(
					`avg(github_issues_seconds_to_close_mean{state="CLOSED",%s})`, PF,
				)),
				promNamed("F", "Lines per pull request", fmt.Sprintf(
					"avg(github_pull_requests_churn_mean{%s})", promMerged,
				)),
			},
			Desc: "How much closed in the range and how long each took. Every duration is a " +
				"median: half of what merged or closed took less than the number shown. " +
				"Time to first review is the wait before a person other than the author " +
				"looked at a pull request, so review bots, which answer in seconds, and " +
				"the author's own replies are left out; a range where nobody else " +
				"reviewed anything reads No data, which is the honest answer. Lines per " +
				"pull request is the median of added plus removed by a merged one.",
			PromDesc: sinceStart + " " + lastSweep,
			GR: []Target{
				grNamed("A", "Pull requests merged", total(countOf(mergedPath("churn")))),
				grNamed("B", "Time to merge", medianTotal(mergedPath("seconds_to_merge"))),
				grNamed("C", "Time to first review",
					medianTotal(anyPath("seconds_to_first_human_review"))),
				grNamed("D", "Issues closed",
					total(countOf(rp("gh_issue", "comments", "state", "CLOSED")))),
				grNamed("E", "Time to close an issue",
					medianTotal(rp("gh_issue", "seconds_to_close", "state", "CLOSED"))),
				grNamed("F", "Lines per pull request", medianTotal(mergedPath("churn"))),
			},
			GRDesc: grSlot,
			ES: []Target{
				esRef("A", b.esTotal(pr, b.mCount(), esMerged...)),
				esRef("B", b.esTotal(pr, b.mPct("seconds_to_merge", 50), ESF, "state:MERGED")),
				esRef("C", b.esTotal(pr, b.mPct("seconds_to_first_human_review", 50), ESF,
					"_exists_:seconds_to_first_human_review")),
				esRef("D", b.esTotal("gh_issue", b.mCount(), "state:CLOSED", ESF, esIdentified)),
				esRef("E", b.esTotal("gh_issue", b.mPct("seconds_to_close", 50), "state:CLOSED", ESF)),
				esRef("F", b.esTotal(pr, b.mPct("churn", 50), "state:MERGED", ESF)),
			},
			ESOver: []any{
				frameName("A", "Pull requests merged"), frameName("B", "Time to merge"),
				frameName("C", "Time to first review"), frameName("D", "Issues closed"),
				frameName("E", "Time to close an issue"), frameName("F", "Lines per pull request"),
			},
			Overrides: []any{
				unitOf("Time to merge", "s", 0), unitOf("Time to first review", "s", 0),
				unitOf("Time to close an issue", "s", 0),
			},
		}),
		panel("timeseries", "Pull requests over time", 12, 8, 0, 5, []Target{sqlTS(perDay)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				"sum by (state) (increase(github_pull_requests_total{%s}[1d]))", PF,
			), "{{state}}")},
			Desc: "Merged and closed are dated at the moment it happened and stacked. " +
				"Open that day is the line: how many were open on that day, which is a " +
				"state and not an event, so it is not part of the stack. " + bucketFollowsRange,
			PromDesc: sinceStart,
			Opts:     mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts:  seriesOpts,
			SQLOver:  stateOverrides,
			GR:       []Target{grq(perBucket("isNonNull("+anyPath("churn")+")", gn(pr, "state")))},
			ES:       []Target{b.esDaily(pr, b.mCount(), "state", "", []string{ESF, esIdentified}, "")},
		}),
		panel("timeseries", "Time to merge over time", 12, 8, 12, 5, []Target{sqlTS(mergeTime)}, &P{
			Prom: []Target{promq(fmt.Sprintf("avg(github_pull_requests_seconds_to_merge_mean{%s})", promMerged),
				legend("Mean of the sweep"))},
			Opts:     mergeOpts(Opts{"unit": "s"}, dayBins),
			PromDesc: lastSweep,
			GR: []Target{
				grq(fmt.Sprintf(`alias(%s, "Median")`, medianBucket(mergedPath("seconds_to_merge"))), "A"),
				grq(fmt.Sprintf(`alias(%s, "Worst")`, worstBucket(mergedPath("seconds_to_merge"))), "B"),
			},
			GRDesc: grWorst + " " + grSlot,
			ES: []Target{esq(pr, []any{b.mPct("seconds_to_merge", 50, 90)}, []any{b.dh()}, "A",
				[]string{"state:MERGED", ESF}, "")},
			Desc: bucketFollowsRange,
		}),
		panel("timeseries", "Issues over time", 12, 7, 0, 13, []Target{sqlTS(issuesDay)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				"sum by (state) (increase(github_issues_total{%s}[1d]))", PF,
			), "{{state}}")},
			Desc: "Closed is dated at the moment it happened. Open that day is the line: " +
				"how many were open on that day, a state rather than an event, so it " +
				"is not stacked. " + bucketFollowsRange,
			PromDesc: sinceStart,
			Opts:     mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts:  seriesOpts,
			SQLOver:  stateOverrides,
			GR: []Target{grq(perBucket("isNonNull("+issuePath("comments")+")",
				gn("gh_issue", "state")))},
			ES: []Target{b.esDaily("gh_issue", b.mCount(), "state", "", []string{ESF, esIdentified}, "")},
		}),
		panel("timeseries", "Pull request size", 12, 7, 12, 13, []Target{sqlTS(sizeTime)}, &P{
			Prom: []Target{promq(fmt.Sprintf("avg(github_pull_requests_churn_mean{%s})", promMerged),
				legend("Mean lines changed"))},
			Desc:     "Lines added plus removed per merged pull request. " + bucketFollowsRange,
			PromDesc: lastSweep,
			Opts:     dayBins,
			GR: []Target{grq(fmt.Sprintf(`alias(%s, "Median lines changed")`,
				medianBucket(mergedPath("churn"))))},
			GRDesc: grSlot,
			ES: []Target{esq(pr, []any{b.mPct("churn", 50)}, []any{b.dh()}, "A",
				[]string{"state:MERGED", ESF}, "Median lines changed")},
		}),
	}
}

// pullsAndReviewers is the individual rows behind those numbers: the biggest
// pull requests, who opened and who reviewed them, and the ones still open.
func pullsAndReviewers(b *builder) []Panel {
	// The number first and the sorted column beside it: on a phone those two
	// and the repository are what fits, and the number is the cell the link
	// hangs on.
	biggest := `SELECT number AS "Number", churn AS "Lines changed", repo AS "Repository",` +
		` title AS "Title", time AS "Closed", author AS "Author",` +
		` author_association AS "Association", label_names AS "Labels",` +
		` changed_files AS "Files", reviews AS "Reviews", seconds_to_merge AS "Time to merge",` +
		` url AS "Link"` +
		" FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF + " AND " + identified +
		" AND state = 'MERGED' ORDER BY churn DESC LIMIT 25"
	authors := `SELECT author AS "Author", COUNT(*) AS "Pull requests"` +
		" FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF + " AND " + identified +
		" GROUP BY 1 ORDER BY 2 DESC LIMIT 15"
	byRepo := `SELECT repo AS "Repository", COUNT(*) AS "Merged",` +
		` approx_percentile_cont(seconds_to_merge, 0.5) AS "Time to merge",` +
		` approx_percentile_cont(churn, 0.5) AS "Lines changed"` +
		" FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF + " AND " + identified +
		" AND state = 'MERGED' GROUP BY 1 ORDER BY 2 DESC"
	// Who is a reviewer: a bot is named as one, and an author answering a
	// review on their own pull request is not reviewing it, so those rows
	// read "own" rather than the reviewer's login. Otherwise the busiest
	// reviewer on a solo account is the owner, replying to the bots.
	who := "CASE WHEN self = 'true' THEN 'own pull request' WHEN bot = 'true'" +
		" THEN reviewer || ' (bot)' ELSE reviewer END"
	reviewers := `SELECT ` + who + ` AS "Reviewer", COUNT(*) AS "Reviews",` +
		` approx_percentile_cont(seconds_to_review, 0.5) AS "Wait"` +
		" FROM gh_pull_request_review WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1 ORDER BY 2 DESC LIMIT 20"
	reviewsDay := "SELECT " + timeBin + ", " + who + " AS series," +
		" COUNT(*) AS reviews FROM gh_pull_request_review WHERE $__timeFilter(time)" +
		" AND " + RF + " GROUP BY 1, 2 ORDER BY 1"
	reviewPath := rp("gh_pull_request_review", "seconds_to_review")

	biggestGR, biggestGRtf := gTbl(topRows(mergedPath("churn"), 25,
		gn(pr, "repo"), gn(pr, "number"), gn(pr, "author")),
		"Pull request", []col{{"max", "Lines changed"}})
	biggestES, biggestEStf := b.esRaw(pr, 500, []named{
		{"@timestamp", "Closed"},
		{"repo", "Repository"},
		{"number", "Number"},
		{"title", "Title"},
		{"author", "Author"},
		{"author_association", "Association"},
		{"label_names", "Labels"},
		{"churn", "Lines changed"},
		{"changed_files", "Files"},
		{"reviews", "Reviews"},
		{"seconds_to_merge", "Time to merge"},
		{"url", "Link"},
	}, esMerged)

	authorsGR, authorsGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(groupByNode(isNonNull(%s), %d, "sum")), 15)`,
		anyPath("churn"), gn(pr, "author"),
	), "Author", []col{{"sum", "Pull requests"}})
	authorsES, authorsEStf := esTbl(pr, []any{b.tm("author", 15)}, []any{b.mCount()},
		[]named{{"author.keyword", "Author"}, {"n", "Pull requests"}},
		[]string{ESF, esIdentified})

	byRepoGR, byRepoGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, %d, "avg")`,
		mergedPath("seconds_to_merge"), gn(pr, "repo")), "Repository",
		[]col{{"count", "Merged"}, {"median", "Time to merge"}})
	byRepoES, byRepoEStf := esTbl(pr, []any{b.tm("repo", 50)},
		[]any{b.mCount(), b.mPct("seconds_to_merge", 50), b.mPct("churn", 50)},
		[]named{
			{"repo.keyword", "Repository"},
			{"n", "Merged"},
			{"t", "Time to merge"},
			{"c", "Lines changed"},
		}, esMerged)

	reviewersGR, reviewersGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(groupByNode(%s, %d, "avg")), 20)`,
		reviewPath, gn("gh_pull_request_review", "reviewer"),
	), "Reviewer",
		[]col{{"count", "Reviews"}, {"median", "Wait"}})
	reviewersES, reviewersEStf := esTbl("gh_pull_request_review", []any{b.tm("reviewer", 20)},
		[]any{b.mCount(), b.mPct("seconds_to_review", 50)},
		[]named{{"reviewer.keyword", "Reviewer"}, {"n", "Reviews"}, {"w", "Wait"}},
		[]string{ESF})

	return append([]Panel{
		panel("table", "Largest merged pull requests", 16, 9, 0, 20, []Target{sqlT(biggest)}, &P{
			PromNote: cannot("the twenty-five largest merged pull requests, with author, "+
				"files, reviews and time to merge.",
				"The exporter reduces pull requests to a count and means per "+
					"repository and state; no individual pull request survives."),
			Opts: Opts{"sort": "Lines changed"},
			Overrides: []any{
				when("Closed"), repoColumn(), width("Number", 80),
				width("Author", 120), width("Association", 110),
				barCell("Lines changed", "short", 120),
				width("Files", 70), width("Reviews", 85),
				unitOf("Time to merge", "s", 130), linkOn("Number"),
			},
			GR: biggestGR, GRTF: biggestGRtf,
			GRDesc: "Graphite names each row repository, number and author from the path. " + grRows,
			ES:     biggestES, ESTF: biggestEStf, ESDesc: esNewest,
		}),
		panel("barchart", "Pull requests by author", 8, 9, 16, 20, []Target{sqlT(authors)}, &P{
			PromNote: cannot("pull requests per author over the range.",
				"The exporter keeps only `repo` and `state` on pull requests: "+
					"an author label would be a series per contributor per state."),
			GR: authorsGR, GRTF: authorsGRtf,
			ES: authorsES, ESTF: authorsEStf,
		}),
		panel("table", "Pull requests by repository", 12, 8, 0, 29, []Target{sqlT(byRepo)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (repo) (increase(github_pull_requests_total{%s}[$__range]))", promMerged), "A"),
				promTbl(fmt.Sprintf("avg by (repo) (github_pull_requests_seconds_to_merge_mean{%s})", promMerged), "B"),
				promTbl(fmt.Sprintf("avg by (repo) (github_pull_requests_churn_mean{%s})", promMerged), "C"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "Value #A": "Merged",
				"Value #B": "Time to merge", "Value #C": "Lines changed",
			}, nil, nil),
			Opts:     Opts{"sort": "Merged"},
			PromDesc: sinceStart + " " + lastSweep,
			Overrides: []any{
				barCell("Merged", "short", 120), unitOf("Time to merge", "s", 140),
				width("Lines changed", 120),
			},
			GR: byRepoGR, GRTF: byRepoGRtf,
			GRDesc: "Graphite reduces the merge times of each repository: how many, and their median. " + grRows,
			ES:     byRepoES, ESTF: byRepoEStf,
		}),
		panel("table", "Reviewers", 12, 8, 12, 29, []Target{sqlT(reviewers)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (reviewer) (increase(github_reviews_total{%s}[$__range]))", PF), "A"),
				promTbl(fmt.Sprintf("avg by (reviewer) (github_reviews_seconds_to_review_mean{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"reviewer": "Reviewer", "Value #A": "Reviews", "Value #B": "Wait",
			}, nil, nil),
			Opts: Opts{"sort": "Reviews"},
			Desc: "Who actually reviews, and how long a review waited. The count on a pull " +
				"request cannot say whether the work is spread across people or resting on " +
				"one, and an automated reviewer answering in seconds is visible here as a " +
				"median rather than hidden in an average. A bot is marked as one, and the " +
				"author's own replies on their pull request are one row called own pull " +
				"request rather than a reviewer.",
			PromDesc:  sinceStart + " " + lastSweep,
			Overrides: []any{barCell("Reviews", "short", 120), unitOf("Wait", "s", 120)},
			GR:        reviewersGR, GRTF: reviewersGRtf, GRDesc: grSlot,
			ES: reviewersES, ESTF: reviewersEStf,
		}),
		panel("timeseries", "Reviews over time", 24, 8, 0, 37, []Target{sqlTS(reviewsDay)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				"sum by (reviewer) (increase(github_reviews_total{%s}[1d]))", PF,
			), "{{reviewer}}")},
			PromDesc: sinceStart,
			Opts:     mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts:  seriesOpts,
			GR: []Target{grq(perBucket("isNonNull("+reviewPath+")",
				gn("gh_pull_request_review", "reviewer")))},
			ES: []Target{b.esDaily("gh_pull_request_review", b.mCount(), "reviewer", "",
				[]string{ESF}, "")},
			Desc: bucketFollowsRange,
		}),
	}, append(stillOpen(b), reviewDebt(b)...)...)
}

// reviewThread is the objection rather than the review: one row per review
// thread, dated at the comment that opened it.
const reviewThread = "gh_review_thread"

// reviewDebt is which pull requests still carry unanswered objections, and how
// much of that debt is already resolved or has gone outdated.
//
// gh_pull_request_review counts submissions and gh_pull_request carries only a
// total of threads, so neither of them can say which conversation is still
// open. This measurement can, and these two panels are the only place in the
// five dashboards where it is read.
//
// `resolved` and `outdated` are 0/1 integer fields and deliberately not tags.
// A thread's row is dated at its first comment and that timestamp never moves,
// so as tags a resolution would open a second series at the same instant and
// the unresolved row would sit beside it for ever. As fields the row converges
// instead: InfluxDB's tag set, PostgreSQL's primary key and the Elasticsearch
// document id all resolve to the same row, so there is exactly one row per
// thread however many sweeps have seen it. That is what makes SUM(1 -
// resolved) the open debt rather than a snapshot, and what lets the exporter
// publish the mean of the same field as the share already paid off. It is also
// why nothing here takes the newest row per series: there is only ever one.
// The identity that makes the convergence work is the `thread` tag, so no
// query drops it from the row it counts, only from the grouping it reports.
func reviewDebt(b *builder) []Panel {
	openedDay := "SELECT " + timeBin + ", CASE WHEN bot = 'true' THEN 'Bot' ELSE 'Human' END AS series," +
		" COUNT(*) AS threads FROM gh_review_thread WHERE $__timeFilter(time)" +
		" AND " + RF + " GROUP BY 1, 2 ORDER BY 1"
	// Every column after Threads is multiplied by the same `1 - resolved`,
	// which is what makes the row read as one sentence: of this many threads,
	// this many are still owed, this much of that is outdated and this many
	// comments are sitting in it. Summing `outdated` over every thread instead
	// answers a different question, and on a repository reviewed by a bot that
	// resolves what it raises it answers a startling one: the fixture's pull
	// request 42 has two resolved-and-outdated threads and one open, so the
	// column printed 2 next to an Unresolved of 1, under a description calling
	// it the part of the debt nobody has closed. Elasticsearch, which can only
	// aggregate the unresolved documents it filtered, was already answering
	// the honest 0 to the same panel.
	debt := `SELECT number AS "Pull request", SUM(1 - resolved) AS "Unresolved",` +
		` repo AS "Repository", COUNT(*) AS "Threads",` +
		` SUM(outdated * (1 - resolved)) AS "Outdated",` +
		` SUM(comments * (1 - resolved)) AS "Comments"` +
		" FROM gh_review_thread WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 25"

	// `comments` is on every thread, so counting the points of that leaf is
	// one point per thread; `resolved` is the flag the debt is computed from.
	openedPath := rp(reviewThread, "comments")
	resolvedPath := rp(reviewThread, "resolved")

	// Graphite has no rows and no SUM(1 - resolved): the open debt is the
	// resolved flag inverted per thread series, `1 - resolved`, and then summed
	// by pull request. `path` and `resolved_by` are strings, which Graphite
	// does not keep at all, and the remaining numbers are one series each, so
	// the table keeps the column it sorts by.
	//
	// total() before the grouping is what makes the number right rather than
	// nearly right. Without it each thread keeps its own point and Graphite
	// consolidates the group's points into the render step by average, so a
	// pull request whose resolved threads and whose open one land in the same
	// bucket answered the mean of 0 and 1 and reported half an objection.
	// Summarizing each thread over the whole range first leaves one point per
	// series and nothing to average. It goes inside groupByNodes, not around
	// it, because the node indices are read off the series name and the name
	// groupByNodes writes is the clean `repo.number` the table shows.
	debtGR, debtGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(groupByNodes(%s, "sum", %d, %d)), 25)`,
		total(fmt.Sprintf("offset(scale(%s,-1),1)", resolvedPath)),
		gn(reviewThread, "repo"), gn(reviewThread, "number"),
	),
		"Repository, pull request", []col{{"sum", "Unresolved"}})

	// Elasticsearch has no arithmetic between two metrics of one bucket, so
	// the debt is asked for as the documents that are the debt: the filter is
	// the unresolved threads and the count of them is the column the table
	// sorts by. Filtering first is also why its Outdated and Comments need no
	// multiplication to mean what the SQL ones mean. `path`, `subject_type`
	// and `resolved_by` are string fields and stay out of it entirely: a
	// top_metrics over a string panics the plugin.
	debtES, debtEStf := esTbl(reviewThread, []any{b.tm("repo", 50), b.tm("number", 25)},
		[]any{b.mCount(), b.mSum("outdated"), b.mSum("comments")},
		[]named{
			{"repo.keyword", "Repository"},
			{"number.keyword", "Pull request"},
			{"n", "Unresolved"},
			{"o", "Outdated"},
			{"c", "Comments"},
		},
		[]string{ESF, "resolved:0"})

	return []Panel{
		panel("timeseries", "Review threads over time", 12, 7, 0, 53,
			[]Target{sqlTS(openedDay)}, &P{
				Prom: []Target{daily(fmt.Sprintf(
					"sum by (bot) (increase(github_review_threads_total{%s}[1d]))", PF,
				), "{{bot}}")},
				Desc: "Dated at the thread's first comment, which is when the objection was " +
					"raised. Split by whether the reviewer is a bot: on this account the " +
					"reviewers are review bots, and a backlog of bot threads reads differently " +
					"from a backlog of human ones. " + bucketFollowsRange,
				PromDesc: sinceStart,
				Opts:     mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
				SQLOpts:  seriesOpts,
				GR: []Target{grq(perBucket("isNonNull("+openedPath+")",
					gn(reviewThread, "bot")))},
				ES: []Target{b.esDaily(reviewThread, b.mCount(), "bot", "", []string{ESF}, "")},
			}),
		panel("table", "The review debt", 12, 7, 12, 53, []Target{sqlT(debt)}, &P{
			Desc: "A review thread is one objection, which GitHub keeps open until somebody " +
				"resolves it. Threads is the whole conversation on that pull request; " +
				"Unresolved, Outdated and Comments describe only what is still owed on it. " +
				"An outdated unresolved thread is one the code has already moved past: debt " +
				"nobody is going to pay and nobody has closed either.",
			PromNote: cannot("the pull requests carrying unresolved review threads, with how "+
				"many of those threads are outdated and how many comments they hold.",
				"The exporter reduces review threads to a count and means per repository "+
					"and `bot`. `number` is not among the labels it keeps, so no pull "+
					"request survives the reduction to be a row here."),
			Opts: Opts{"sort": "Unresolved"},
			Overrides: []any{
				repoColumn(), width("Pull request", 100), width("Threads", 90),
				barCell("Unresolved", "short", 120), width("Outdated", 100),
				width("Comments", 110),
			},
			GR: debtGR, GRTF: debtGRtf,
			GRDesc: "Graphite names each row repository and pull request from the path, and " +
				"inverts the resolved flag of each thread to count the open ones. " + grRows,
			ES: debtES, ESTF: debtEStf,
			ESDesc: "In Elasticsearch the rows are the unresolved threads themselves, which is " +
				"how the store answers a subtraction it has no operator for. That is the " +
				"same set the other three multiply out, so the three debt columns are the " +
				"same numbers; only the pull request's own thread total is absent.",
		}),
	}
}

// stillOpenNote is what the two stores that cannot read an item from its
// newest row say instead of pretending they did: their answer is the open
// rows as written, which the InfluxDB table below reads past.
const stillOpenNote = "This store cannot read each item from its newest row, so one that " +
	"closed inside the range stays listed as open, at the reading of its last open day, " +
	"until the range moves past that day."

// stillOpen is what has not closed yet: the pull requests and the issues
// that have waited longest, each one a row a reader can open.
func stillOpen(b *builder) []Panel {
	// The newest row of each pull request in the range, and only then the
	// ones whose newest row is still open: a merged pull request keeps its
	// daily open rows behind it, and reading those as open listed three
	// merged ones as open for hours.
	openest := `SELECT number AS "Number", seconds_open AS "Open for", repo AS "Repository",` +
		` title AS "Title", author AS "Author", label_names AS "Labels",` +
		` comments AS "Comments", reviews AS "Reviews", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, number ORDER BY time DESC) AS rn" +
		" FROM gh_pull_request WHERE $__timeFilter(time) AND " + RF + " AND " + identified +
		") x WHERE rn = 1 AND state = 'OPEN' ORDER BY seconds_open DESC LIMIT 25"
	// The twin for issues, read the same way: until this table no issue was
	// reachable by unit from any panel, only counted. `label_names` is the
	// field the collector writes for what the issue is about, which is what
	// decides whether an old open issue is a bug or a wish.
	openIssues := `SELECT number AS "Number", seconds_open AS "Open for", repo AS "Repository",` +
		` author AS "Author", comments AS "Comments",` +
		` label_names AS "Labels", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, number ORDER BY time DESC) AS rn" +
		" FROM gh_issue WHERE $__timeFilter(time) AND " + RF + " AND " + identified +
		") x WHERE rn = 1 AND state = 'OPEN' ORDER BY seconds_open DESC LIMIT 25"

	openGR, openGRtf := gTbl(fmt.Sprintf(`limit(sortBy(groupByNodes(%s, "max", %d, %d), "max", true), 25)`,
		rp("gh_pull_request", "seconds_open", "state", "OPEN"),
		gn("gh_pull_request", "repo"), gn("gh_pull_request", "number")),
		"Repository, number", []col{{"max", "Open for"}})
	// The url as a bucket, never as a metric: a top_metrics over a string
	// panics the plugin, and a pull request has exactly one url.
	openES, openEStf := esTbl("gh_pull_request", []any{b.tm("repo", 50), b.tm("number", 25), b.tm("url", 1)},
		[]any{b.mMax("seconds_open"), b.mMax("comments"), b.mMax("reviews")},
		[]named{
			{"repo.keyword", "Repository"},
			{"number.keyword", "Number"},
			{"url.keyword", "Link"},
			{"s", "Open for"},
			{"c", "Comments"},
			{"r", "Reviews"},
		},
		[]string{ESF, "state:OPEN"})

	openIssuesGR, openIssuesGRtf := gTbl(fmt.Sprintf(`limit(sortBy(groupByNodes(%s, "max", %d, %d), "max", true), 25)`,
		rp("gh_issue", "seconds_open", "state", "OPEN"),
		gn("gh_issue", "repo"), gn("gh_issue", "number")),
		"Repository, number", []col{{"max", "Open for"}})
	openIssuesES, openIssuesEStf := esTbl("gh_issue", []any{b.tm("repo", 50), b.tm("number", 25), b.tm("url", 1)},
		[]any{b.mMax("seconds_open"), b.mMax("comments")},
		[]named{
			{"repo.keyword", "Repository"},
			{"number.keyword", "Number"},
			{"url.keyword", "Link"},
			{"s", "Open for"},
			{"c", "Comments"},
		},
		[]string{ESF, "state:OPEN"})
	return []Panel{
		panel("table", "Open the longest", 12, 8, 0, 45, []Target{sqlT(openest)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf(`topk(25, max by (repo, number, author) (github_pull_requests_seconds_open_mean{state="OPEN",%s}))`, PF), "A"),
				promTbl(fmt.Sprintf(`max by (repo, number, author) (github_pull_requests_comments_mean{state="OPEN",%s})`, PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "number": "Number", "author": "Author",
				"Value #A": "Open for", "Value #B": "Comments",
			}, nil, map[string]int{"repo": 0, "number": 1, "author": 2}),
			Opts: Opts{"sort": "Open for"},
			Desc: "Time to merge only counts what merged. This is the other half: what is " +
				"still open and how long it has been, which is the number that decides what " +
				"to do next rather than describing what already happened. Each pull request " +
				"is read from its newest row, so one that merged inside the range is not " +
				"here any more.",
			PromDesc: lastSweep,
			Overrides: []any{
				repoColumn(), width("Number", 80), width("Author", 120),
				unitOf("Open for", "s", 130),
				width("Comments", 100), width("Reviews", 90), linkOn("Number"),
			},
			GR: openGR, GRTF: openGRtf, GRDesc: grSlot + " " + stillOpenNote,
			ES: openES, ESTF: openEStf, ESDesc: stillOpenNote,
		}),
		panel("table", "Open issues the longest", 12, 8, 12, 45, []Target{sqlT(openIssues)}, &P{
			PromNote: cannot("the twenty-five open issues that have waited longest, with "+
				"their labels and comment counts.",
				"The exporter reduces issues to a count and means per repository and "+
					"state; no individual issue survives."),
			Opts: Opts{"sort": "Open for"},
			Desc: "The same question for issues: what is still open and for how long. Labels " +
				"is what the issue was filed as, so an old one reads as a bug nobody fixed " +
				"or a wish nobody granted. Each issue is read from its newest row, so one " +
				"closed inside the range is not here any more.",
			Overrides: []any{
				unitOf("Open for", "s", 130), width("Number", 80),
				width("Comments", 110), width("Labels", 160), linkOn("Number"),
			},
			GR: openIssuesGR, GRTF: openIssuesGRtf, GRDesc: grSlot + " " + stillOpenNote,
			ES: openIssuesES, ESTF: openIssuesEStf, ESDesc: stillOpenNote,
		}),
	}
}
