package dashboards

import "fmt"

// planningNewestRow closes the window that numbered each partition's rows:
// every table here is rewritten whole each sweep, so only row one is today's.
// The two titles are shared with the panels' Elasticsearch and Prometheus
// twins, which rename their columns to them and show nothing if they differ.
const (
	planningNewestRow    = ") x WHERE rn = 1"
	planningPullRequests = "Pull requests"
	planningPushedTo     = "Pushed to"
	planningPushedAfter  = "Pushed after"
)

// ── Planning and community ──────────────────────────────────────────────────

// planning is the shape of the work and the conversation around it.
func planning(b *builder) []Panel {
	return append(labelsMilestonesAndForks(b), discussionAndComments(b)...)
}

// labelsMilestonesAndForks is how the work is filed and who copied it: the
// labels in use, how far each milestone got, and the forks people took.
func labelsMilestonesAndForks(b *builder) []Panel {
	// One row per repository and label rather than per label across the
	// account: a label's url is its issue list in one repository, so a row
	// summed across repositories had nothing to link to.
	labels := `SELECT label AS "Label", used AS "Used", repo AS "Repository",` +
		` issues AS "Issues", pull_requests AS "Pull requests", url AS "Link" FROM (` +
		"SELECT repo, label, used, issues, pull_requests, url, ROW_NUMBER() OVER (" +
		"PARTITION BY full_name, label ORDER BY time DESC) AS rn FROM gh_label" +
		" WHERE $__timeFilter(time) AND " + RF + planningNewestRow +
		" ORDER BY 2 DESC LIMIT 25"
	miles := `SELECT milestone AS "Milestone", progress AS "Progress", repo AS "Repository",` +
		` state AS "State", issues AS "Issues",` +
		` pull_requests AS "Pull requests", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, milestone ORDER BY time DESC) AS rn" +
		" FROM gh_milestone WHERE $__timeFilter(time) AND " + RF + planningNewestRow +
		" ORDER BY 2 DESC LIMIT 25"
	forks := topRepoSeries("gh_fork", "1", "forks", RF)
	// Idle is computed rather than stored: the row is dated when the fork was
	// created and `seconds_to_push` says how long after that its last push
	// came, so the time since that push is the row's own age less that. The
	// collector used to store the idle days themselves, which meant one more
	// day every day written back on to a row dated years ago.
	forkIdle := `date_part('epoch', now() - time) - seconds_to_push AS "Idle"`
	forkTbl := `SELECT by AS "By", time AS "Forked", repo AS "Repository",` +
		` advanced AS "` + planningPushedTo + `", ` + forkIdle + `,` +
		` url AS "Link"` +
		" FROM gh_fork WHERE $__timeFilter(time) AND " + RF + " ORDER BY time DESC LIMIT 25"
	// seconds_to_answer is a field only once a discussion has been answered:
	// InfluxDB creates the column on first write, and naming it before then
	// fails the whole query. Same reason the milestone due date is not shown.
	lb, ms, fk := "gh_label", "gh_milestone", "gh_fork"

	labelsGR, labelsGRtf := gTbl(rowsOf(fmt.Sprintf(
		`limit(sortByMaxima(keepLastValue(%s)), 25)`, rp(lb, "used"),
	), gn(lb, "repo"), gn(lb, "label")), "Repository, label", []col{{"lastNotNull", "Used"}})
	labelsES, labelsEStf := esTbl(lb, append(b.tmRepo(50), b.tm("label", 25), b.tmURL()),
		[]any{b.mNewest("used", "issues", "pull_requests")},
		[]named{
			{panelRepoField, "Repository"},
			{"label.keyword", "Label"},
			{"url.keyword", "Link"},
			{"used", "Used"},
			{"issues", "Issues"},
			{"pull_requests", planningPullRequests},
		}, []string{ESF}, hideColumns(panelFullNameField))

	milesGR, milesGRtf := gTbl(topRows(rp(ms, "progress"), 25,
		gn(ms, "repo"), gn(ms, "milestone"), gn(ms, "state")),
		"Milestone", []col{{"lastNotNull", "Progress"}})
	milesES, milesEStf := esTbl(ms, append(b.tmRepo(50), b.tm("milestone", 25), b.tm("state", 5), b.tmURL()),
		[]any{b.mNewest("progress", "issues", "pull_requests")},
		[]named{
			{panelRepoField, "Repository"},
			{"milestone.keyword", "Milestone"},
			{"state.keyword", "State"},
			{"url.keyword", "Link"},
			{"progress", "Progress"},
			{"issues", "Issues"},
			{"pull_requests", planningPullRequests},
		}, []string{ESF}, hideColumns(panelFullNameField))

	// Neither store can subtract the row's own date from now, so both show
	// the static number instead: how long after the fork its last push came.
	forkGR, forkGRtf := gTbl(rowsOf(rp(fk, "seconds_to_push"), gn(fk, "by"), gn(fk, "repo")),
		"By, repository", []col{{"lastNotNull", planningPushedAfter}})
	forkES, forkEStf := b.esRaw(fk, 25, []named{
		{panelESTime, "Forked"},
		{"by", "By"},
		{"repo", "Repository"},
		{"advanced", planningPushedTo},
		{"seconds_to_push", planningPushedAfter},
		{"url", "Link"},
	}, []string{ESF})
	return []Panel{
		panel("table", "Labels", box{W: 12, H: 8, X: 0, Y: 0}, []Target{sqlT(labels)}, &P{
			Prom: func() []Target {
				rank := fmt.Sprintf("max by (full_name, repo, label) (github_label_used{%s})", PF)
				return []Target{
					promTbl(promTop(25, rank), "A"),
					promTbl(promWithin(25, fmt.Sprintf(
						"max by (full_name, repo, label) (github_label_issues{%s})", PF,
					), rank, "full_name", "repo", "label"), "B"),
					promTbl(promWithin(25, fmt.Sprintf(
						"max by (full_name, repo, label) (github_label_pull_requests{%s})", PF,
					), rank, "full_name", "repo", "label"), "C"),
				}
			}(),
			PromTF: merged(map[string]string{
				"repo": "Repository", "label": "Label", panelValueA: "Used",
				panelValueB: "Issues", panelValueC: planningPullRequests,
			}, nil, map[string]int{"repo": 0, "label": 1}),
			Opts: Opts{"sort": "Used"},
			Desc: "The labels in use, per repository, and the link is that label's issue " +
				"list. Only the labels something carries are collected, so a label nobody " +
				"applied is not a row.",
			Overrides: []any{
				barCell("Used", "short", 120), width("Issues", 100),
				width(planningPullRequests, 130), linkOn("Label"),
			},
			GR: labelsGR, GRTF: labelsGRtf,
			GRDesc: "Graphite names each row repository and label from the path. " + grRows,
			ES:     labelsES, ESTF: labelsEStf,
		}),
		panel("table", "Milestones", box{W: 12, H: 8, X: 12, Y: 0}, []Target{sqlT(miles)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (full_name, repo, milestone, state) (github_milestone_progress{%s})", PF), "A"),
				promTbl(fmt.Sprintf("sum by (full_name, repo, milestone, state) (github_milestone_issues{%s})", PF), "B"),
				promTbl(fmt.Sprintf("sum by (full_name, repo, milestone, state) (github_milestone_pull_requests{%s})", PF), "C"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "milestone": "Milestone", "state": "State",
				panelValueA: "Progress", panelValueB: "Issues", panelValueC: planningPullRequests,
			}, nil, map[string]int{"repo": 0, "milestone": 1, "state": 2}),
			Opts: Opts{"sort": "Progress"},
			Desc: "The due date is collected as a field when a milestone has one, but it is not " +
				"shown here: InfluxDB creates a column the first time it is written, so a " +
				"query naming it fails outright on a database where no milestone ever had a " +
				"due date.",
			Overrides: []any{
				barCell("Progress", "percent", 130),
				width("State", 90), width("Issues", 90), width(planningPullRequests, 130),
				linkOn("Milestone"),
			},
			GR: milesGR, GRTF: milesGRtf,
			GRDesc: "Graphite names each row repository, milestone and state from the path. " + grRows,
			ES:     milesES, ESTF: milesEStf,
		}),
		panel("timeseries", "Forks gained over time", box{W: 12, H: 8, X: 0, Y: 8}, []Target{sqlTS(forks)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				"sum by (full_name, repo) (increase(github_forks_seen_total{%s}[1d]))", PF,
			), "{{repo}}")},
			Opts:    mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts: seriesOpts,
			Desc: "Dated when each fork was created, which the fork count on a repository " +
				"never says. The eight repositories that gained the most in the range are " +
				"named; the rest are `other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			GR:       []Target{grq(perRepoBucket(nonNull(rp(fk, "forks")), fk))},
			ES:       []Target{b.esDaily(fk, b.mCount(), "repo", "", []string{ESF}, "")},
		}),
		panel("table", "Forks", box{W: 12, H: 8, X: 12, Y: 8}, []Target{sqlT(forkTbl)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (full_name, repo) (increase(github_forks_seen_total{%s}[$__range]))", PF), "A"),
				promTbl(fmt.Sprintf("avg by (full_name, repo) (github_forks_seen_advanced_mean{%s})", PF), "B"),
				promTbl(fmt.Sprintf("avg by (full_name, repo) (github_forks_seen_seconds_to_push_mean{%s})", PF), "C"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", panelValueA: "Forks", panelValueB: planningPushedTo,
				panelValueC: planningPushedAfter,
			}, nil, nil),
			Desc: "Whether a fork was ever pushed to separates a derivative from a bookmark, " +
				"which most forks are. Idle is the time since that push, counted from the " +
				"row's own date, so it is right when the panel is drawn rather than when the " +
				"last sweep ran.",
			PromDesc: "Prometheus keeps no forker, so this is per repository: forks seen over " +
				"the range, the share ever pushed to, and on average how long after the fork " +
				"its last push came, which is negative for a fork nobody has pushed to at " +
				"all: GitHub gives such a fork the parent's own last push, which usually " +
				"predates it, and two thirds of the forks measured here are that. How long a " +
				"fork has been idle is the row's own date " +
				"subtracted from now, which only the two SQL stores can do. " + sinceStart,
			Overrides: []any{
				when("Forked"), width(planningPushedTo, 110), unitOf("Idle", "s", 90),
				linkOn("By"),
			},
			PromOver: []any{
				unitOf(planningPushedTo, "percentunit", 110),
				barCell("Forks", "short", 120),
				unitOf(planningPushedAfter, "s", 110),
			},
			GROver: []any{unitOf(planningPushedAfter, "s", 110)},
			ESOver: []any{unitOf(planningPushedAfter, "s", 110)},
			GR:     forkGR, GRTF: forkGRtf,
			GRDesc: "Graphite names each row forker and repository from the path and keeps one " +
				"number per row, so whether it was pushed to is missing. It cannot " +
				"subtract the row's own date from now either, so the column is how long " +
				"after the fork its last push came, which is negative for a fork nobody has " +
				"pushed to: those inherit the parent's last push. " + grRows,
			ES: forkES, ESTF: forkEStf,
			ESDesc: "Elasticsearch lists the documents themselves and cannot subtract the " +
				"row's own date from now, so the column is how long after the fork its last " +
				"push came. A negative is a fork nobody has pushed to, which inherits the " +
				"parent's own last push and is most of them; Pushed to says the same thing " +
				"as a word.",
		}),
	}
}

// discussionAndComments is the conversation around that work: the discussion
// categories and whether they were answered, the discussions themselves, the
// moments an issue changed, where the comments were left, and the answers
// given in other people's discussions.
func discussionAndComments(b *builder) []Panel {
	// has_answer is a field: the answer comes after the date the row
	// carries, and as the tag `answered` a question answered after the first
	// sweep was two rows for ever. A boolean, so it groups the same way.
	disc := `SELECT category AS "Category", COUNT(*) AS "Discussions",` +
		` CAST(has_answer AS INT) AS "Answered", AVG(comments) AS "Comments each",` +
		` AVG(upvotes) AS "Upvotes each" FROM gh_discussion` +
		" WHERE $__timeFilter(time) AND " + RF + " GROUP BY 1, 3 ORDER BY 2 DESC"
	dc := "gh_discussion"
	// The discussions one by one, whatever the range: an account has a
	// handful and a thirty day range hid all but one of them under a row
	// that read "Ideas". One row per discussion: the row is dated when it
	// was opened and rewritten in place, but category and answerable are
	// tags, so a discussion moved to another category is two rows at one
	// instant, and the one with more comments is the later state. Answered
	// is null where the category takes no answer at all, which the cell
	// says in words.
	latest := `SELECT title AS "Title", time AS "Opened", repo AS "Repository",` +
		` category AS "Category", comments AS "Comments",` +
		` CASE WHEN answerable = 'false' THEN NULL WHEN has_answer THEN 1 ELSE 0 END AS "Answered",` +
		` url AS "Link" FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, number` +
		" ORDER BY time DESC, comments DESC) AS rn FROM gh_discussion" +
		" WHERE " + wholeHistory + " AND " + RF + planningNewestRow +
		" ORDER BY 2 DESC LIMIT 50"
	latestGR, latestGRtf := gTbl(rowsOf(gp(dc, "comments"), gn(dc, "repo"), gn(dc, "number"),
		gn(dc, "category")), "Repository, number, category", []col{{"lastNotNull", "Comments"}})
	latestES, latestEStf := b.esRaw(dc, 50, []named{
		{"title", "Title"},
		{panelESTime, "Opened"},
		{"repo", "Repository"},
		{"category", "Category"},
		{"comments", "Comments"},
		{"has_answer", "Answered"},
		{"url", "Link"},
	}, []string{ESF})
	perItem := "The exporter reduces discussions to counts per category and comments to " +
		"counts per repository; no item survives, and the title and the url are strings."
	grPerItem := "Graphite keeps no strings, so neither the title nor the url exists there."

	// Graphite keeps the flag as a 0 or 1 leaf and cannot group by it, so
	// each category is one row and the mean of the leaf is the share answered.
	discGR, discGRtf := gTbl(fmt.Sprintf(`groupByNodes(%s, "avg", %d)`,
		rp(dc, "has_answer"), gn(dc, "category")),
		"Category", []col{{"count", "Discussions"}, {"mean", "Answered"}})
	// A boolean has no .keyword sub-field: the terms bucket is on the field.
	discES, discEStf := esTbl(dc, []any{b.tm("category", 50), b.terms("has_answer", 2)},
		[]any{b.mCount(), b.mAvg("comments"), b.mAvg("upvotes")},
		[]named{
			{"category.keyword", "Category"},
			{"has_answer", "Answered"},
			{"n", "Discussions"},
			{"c", "Comments each"},
			{"u", "Upvotes each"},
		}, []string{ESF})

	// Both of these are mostly other people's repositories, so the row names
	// one in full: `repo` is the short name on every measurement now, and two
	// owners using the same one would be one row here. Each comment is a 1
	// added up per repository; countOf added every comment into one series
	// first, and the table drew that as one row.
	commentsGR, commentsGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, %d, "sum")`,
		counted(gp("gh_issue_comment", "comments")), gn("gh_issue_comment", "full_name")),
		"Repository", []col{{"sum", "Comments"}})
	commentsES, commentsEStf := esTbl("gh_issue_comment", []any{b.tm("full_name", 20)}, []any{b.mCount()},
		[]named{{panelFullNameField, "Repository"}, {"n", "Comments"}}, nil)

	return append([]Panel{
		panel("table", "Discussions", box{W: 8, H: 8, X: 0, Y: 16}, []Target{sqlT(disc)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (category, has_answer) (increase(github_discussions_total{%s}[$__range]))", PF), "A"),
				promTbl(fmt.Sprintf("avg by (category, has_answer) (github_discussions_comments_mean{%s})", PF), "B"),
				promTbl(fmt.Sprintf("avg by (category, has_answer) (github_discussions_upvotes_mean{%s})", PF), "C"),
			},
			PromTF: merged(map[string]string{
				"category": "Category", "has_answer": "Answered", panelValueA: "Discussions",
				panelValueB: "Comments each", panelValueC: "Upvotes each",
			}, nil, map[string]int{"category": 0, "has_answer": 1}),
			Opts:     Opts{"sort": "Discussions"},
			Desc:     "Discussions opened in the range, by category and whether they were answered.",
			PromDesc: sinceStart + " " + lastSweep,
			Overrides: []any{
				width("Category", 150), width("Answered", 100),
				barCell("Discussions", "short", 120),
			},
			SQLOver: []any{profileBool("Answered", 100)},
			GR:      discGR, GRTF: discGRtf,
			GROver: []any{unitOf("Answered", "percentunit", 100)},
			GRDesc: "Graphite keeps no strings and cannot group by the answered flag, so each " +
				"category is one row and Answered is the share of its discussions that have " +
				"an answer. " + grRows,
			ES: discES, ESTF: discEStf,
		}),
		panel("table", "Latest discussions", box{W: 16, H: 8, X: 8, Y: 16}, []Target{sqlT(latest)}, &P{
			PromNote: cannot("the newest fifty discussions one by one, with their category, "+
				"comment count, whether they were answered, and a link to each.", perItem),
			GRDesc: "Graphite names each row repository, number and category from the path " +
				"and keeps no text, so the title is missing. " + grRows,
			Desc: "The newest fifty, whatever the range: an account has a handful, and a " +
				"range of a month hid all but one of them. Answered reads n/a where the " +
				"category takes no answer at all, an announcement or an idea.",
			Opts: Opts{"sort": "Opened"},
			Overrides: []any{
				when("Opened"), repoColumn(), width("Category", 120),
				width("Comments", 100), answeredCell("Answered", 100), linkOn("Title"),
			},
			GR: latestGR, GRTF: latestGRtf,
			ES: latestES, ESTF: latestEStf, ESDesc: esNewest + " " + esRange,
			// Elasticsearch's raw documents carry the JSON boolean, which
			// reads as true and false without a mapping.
			ESOver: []any{width("Answered", 100)},
		}),
		// The eight commonest transitions of the range and the rest as
		// `other`: the legend listed eighty one series.
		panel("timeseries", "Transitions over time", box{W: 12, H: 8, X: 0, Y: 24}, []Target{sqlTS(
			topSeries("gh_issue_event", "event", "events", "n", RF),
		)}, &P{
			Prom: []Target{daily(fmt.Sprintf(
				"sum by (event) (increase(github_issue_events_total{%s}[1d]))", PF,
			), "{{event}}")},
			Opts:    mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts: seriesOpts,
			Desc: "Labeled, closed, reopened, review requested, renamed: the moment something " +
				"changed, which the state of an issue does not record. A reopening exists in " +
				"no other measurement at all. The eight commonest in the range are named; " +
				"the rest are `other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			GR: []Target{grq(perBucket(nonNull(rp("gh_issue_event", "events")),
				gn("gh_issue_event", "event")))},
			ES: []Target{b.esDaily("gh_issue_event", b.mCount(), "event", "", []string{ESF}, "")},
		}),
		panel("table", "Comments left", box{W: 12, H: 8, X: 12, Y: 24}, []Target{sqlT(
			`SELECT full_name AS "Repository", COUNT(*) AS "Comments",` +
				` MAX(CASE WHEN own = 'false' THEN 1 ELSE 0 END) AS "Elsewhere"` +
				" FROM gh_issue_comment WHERE $__timeFilter(time)" +
				" GROUP BY 1 ORDER BY 2 DESC LIMIT 20",
		)}, &P{
			Prom:   []Target{promTbl("topk(20, sum by (full_name) (increase(github_issue_comments_total[$__range])))")},
			PromTF: []any{organize(map[string]string{"full_name": "Repository", "Value": "Comments"}, nil, nil)},
			Opts:   Opts{"sort": "Comments"},
			Desc: "Comments on issues and pull requests, in any repository. The ones outside " +
				"this account are the half a sweep over one's own repositories cannot see.",
			PromDesc:  sinceStart,
			Overrides: []any{barCell("Comments", "short", 120)},
			GR:        commentsGR, GRTF: commentsGRtf, GRDesc: grRows + " " + grSlotCounts,
			ES: commentsES, ESTF: commentsEStf,
		}),
	}, answersGiven(b, perItem, grPerItem)...)
}

// answersGiven is the two tables over gh_discussion_comment: the comments of
// the range per repository, and the ones left in other people's discussions
// one by one, with whether each was accepted. Both read one row per comment
// in every shape a store can hold the measurement in, which is most of what
// they are made of, so they are built here and not among the discussions.
func answersGiven(b *builder, perItem, grPerItem string) []Panel {
	// The comments this account left in discussions of repositories it does
	// not own, and whether each was marked the accepted answer. The comment
	// is dated when it was written; the link opens it in its thread.
	//
	// One row per comment, accepted when any of its rows says so. Until 2.6.1
	// the accepted answer was also the tag is_answer, so a comment read
	// before the maintainer accepted it and again after is two rows at one
	// instant, and a store written by 2.6.0 and by 2.6.1 holds a third, the
	// comment without the tag, until the measurement is dropped and filled
	// again. The field answers is on every row of every shape, and the tag is
	// named nowhere: after that drop no row carries it, and InfluxDB 3 refuses
	// a query naming a column no row has.
	elsewhere := `SELECT title AS "Title", time AS "When", full_name AS "Repository",` +
		` answers AS "Accepted", url AS "Link" FROM (SELECT *, ROW_NUMBER() OVER` +
		" (PARTITION BY comment ORDER BY answers DESC) AS rn FROM gh_discussion_comment" +
		" WHERE " + wholeHistory + " AND own = 'false') x WHERE rn = 1" +
		" ORDER BY 2 DESC LIMIT 50"
	// The comments of the range per repository, one row per comment for the
	// same reason: counted row by row, a comment read before and after it was
	// accepted was two comments and two accepted answers.
	answersSQL := `SELECT full_name AS "Repository", SUM(answers) AS "Accepted answers",` +
		` COUNT(*) AS "Comments", SUM(upvotes) AS "Upvotes" FROM (SELECT full_name,` +
		" answers, upvotes, ROW_NUMBER() OVER (PARTITION BY comment ORDER BY answers DESC," +
		" upvotes DESC) AS rn FROM gh_discussion_comment WHERE $__timeFilter(time)) x" +
		" WHERE rn = 1 GROUP BY 1 ORDER BY 2 DESC, 3 DESC LIMIT 20"
	dcc := "gh_discussion_comment"
	// What the list of comments says about the rows it is made of; the
	// table of counts points at it, since in Prometheus it has no rows to
	// speak of.
	onceEach := "One row per comment, accepted when any of its rows says so. Until 2.6.1 " +
		"whether a comment was accepted was part of its row's identity, so a store written " +
		"before then holds a comment read before and after it was accepted as two rows, and " +
		"one written by 2.6.0 and 2.6.1 holds a third without that part. An answer accepted " +
		"before 2.6.1 and taken back since reads accepted until gh_discussion_comment is " +
		"dropped and filled again with a backfill."
	// Graphite keeps the paths of both shapes, one node apart in depth, so
	// each comment is read in both and the two series of one comment are
	// joined by its name before the row is named by repository and number.
	elsewhereGR, elsewhereGRtf := gTbl(rowsOf(fmt.Sprintf(`groupByNodes(%s, "max", 0, 1, 2)`,
		everyShape(dcc, "answers", []string{"full_name", "number", "comment"}, "own", "false")), 0, 1),
		"Repository, number", []col{{"lastNotNull", "Accepted"}})
	// Elasticsearch keeps a document per shape, since a document's id is its
	// tags and its time, and a raw query cannot collapse them; the table does,
	// one row per comment, and then keeps the newest fifty.
	elsewhereES, elsewhereEStf := b.esRaw(dcc, 100, []named{
		{"title", "Title"},
		{panelESTime, "When"},
		{"full_name", "Repository"},
		{"answers", "Accepted"},
		{"url", "Link"},
		{"comment", "Comment"},
	}, []string{"own:false"})
	elsewhereEStf = append(elsewhereEStf, onePerComment(50)...)
	// One series per comment across both shapes, then the comments of each
	// repository added up, bucket by bucket.
	answersGR, answersGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, 0, "sum")`,
		counted(fmt.Sprintf(`groupByNodes(%s, "max", 0, 1)`, everyShape(dcc, "comments", []string{"full_name", "comment"})))),
		"Repository", []col{{"sum", "Comments"}})
	// A bucket per comment inside each repository's, each the largest of its
	// documents, and the table adds them up per repository: a sum over the
	// documents counted a comment once per shape it was written in.
	answersES, answersEStf := esTbl(dcc, []any{b.tm("full_name", 20), b.tm("comment", 1000)},
		[]any{b.mMax("answers"), b.mMax("upvotes")},
		[]named{
			{panelFullNameField, "Repository"},
			{"comment.keyword", "Comment"},
			{"a", "Accepted"},
			{"u", "Up"},
		}, nil, perRepositoryFromComments()...)
	return []Panel{
		panel("table", "Discussion answers", box{W: 8, H: 8, X: 0, Y: 32}, []Target{sqlT(answersSQL)}, &P{
			Prom:   []Target{promTbl("topk(20, sum by (full_name) (increase(github_discussion_comments_total[$__range])))")},
			PromTF: []any{organize(map[string]string{"full_name": "Repository", "Value": "Comments"}, nil, nil)},
			Opts:   Opts{"sort": "Accepted answers"},
			Desc: "Discussions are the one surface where the work is almost entirely in other " +
				"people's repositories: gh_discussion sees three rows inside this account, " +
				"and this sees sixty six across twenty six repositories. Each comment counts " +
				"once, however many rows a store holds of it, which Answers elsewhere explains.",
			PromDesc:  sinceStart,
			Overrides: []any{barCell("Comments", "short", 120)},
			GR:        answersGR, GRTF: answersGRtf,
			GRDesc: "Graphite has no rows: each series is one number, so this table keeps the " +
				"comments of each repository and drops the accepted answers and the upvotes.",
			ES: answersES, ESTF: answersEStf,
			ESDesc: "In Elasticsearch each comment is a bucket inside its repository's, " +
				"a thousand at most per repository, and the table adds the buckets up.",
		}),
		panel("table", "Answers elsewhere", box{W: 16, H: 8, X: 8, Y: 32}, []Target{sqlT(elsewhere)}, &P{
			PromNote: cannot("the newest fifty comments this account left in other people's "+
				"discussions, whether each was accepted as the answer, and a link to each.",
				perItem),
			GRDesc: "Graphite names each row by repository and number from the path, and " +
				"Accepted is the comment's answers leaf. " + grPerItem + " " + grRows,
			Desc: "Every comment this account left in a discussion of a repository it does " +
				"not own, newest first and whatever the range; Accepted says whether the " +
				"maintainer marked it the answer. The link opens the comment in its thread. " +
				onceEach,
			Opts: Opts{"sort": "When"},
			Overrides: []any{
				when("When"), repoColumn(),
				profileBool("Accepted", 100), linkOn("Title"),
			},
			GR: elsewhereGR, GRTF: elsewhereGRtf,
			ES: elsewhereES, ESTF: elsewhereEStf,
			ESDesc: "In Elasticsearch this reads the newest hundred documents and keeps the " +
				"newest fifty comments among them. " + esRange,
		}),
	}
}

// onePerComment folds the rows of Answers elsewhere in Elasticsearch, a raw
// document each, into one per comment, accepted when any of its documents
// says so, and keeps the newest `limit`. A document's id is its tags and its
// time, so a comment read before 2.6.1, when whether it was accepted was a
// tag, and again since is a document in each shape, and one accepted before
// then two in the old one. The documents come newest first and a comment's
// share one timestamp, so the first of each group is as good as any and the
// groups come out in the documents' order.
func onePerComment(limit int) []any {
	take := func(how string) map[string]any {
		return map[string]any{"operation": "aggregate", "aggregations": []any{how}}
	}
	return []any{
		map[string]any{"id": "groupBy", "options": map[string]any{"fields": map[string]any{
			"Comment":    map[string]any{"operation": "groupby", "aggregations": []any{}},
			"Title":      take("first"),
			"When":       take("first"),
			"Repository": take("first"),
			"Accepted":   take("max"),
			"Link":       take("first"),
		}}},
		map[string]any{"id": "organize", "options": map[string]any{
			"excludeByName": map[string]any{"Comment": true},
			"indexByName": map[string]any{
				"Title (first)": 0, "When (first)": 1, "Repository (first)": 2,
				"Accepted (max)": 3, "Link (first)": 4,
			},
			"renameByName": map[string]any{
				"Title (first)": "Title", "When (first)": "When", "Repository (first)": "Repository",
				"Accepted (max)": "Accepted", "Link (first)": "Link",
			},
		}},
		map[string]any{"id": "limit", "options": map[string]any{"limitField": limit}},
	}
}

// perRepositoryFromComments adds up Discussion answers in Elasticsearch, whose
// query answers a row per comment inside each repository, the largest of the
// comment's documents, into a row per repository: a sum over the documents
// themselves counted a comment once per shape it was written in, for the
// reason onePerComment gives.
func perRepositoryFromComments() []any {
	return []any{
		map[string]any{"id": "groupBy", "options": map[string]any{"fields": map[string]any{
			"Repository": map[string]any{"operation": "groupby", "aggregations": []any{}},
			"Comment":    map[string]any{"operation": "aggregate", "aggregations": []any{"count"}},
			"Accepted":   map[string]any{"operation": "aggregate", "aggregations": []any{"sum"}},
			"Up":         map[string]any{"operation": "aggregate", "aggregations": []any{"sum"}},
		}}},
		map[string]any{"id": "organize", "options": map[string]any{
			"excludeByName": map[string]any{},
			"indexByName": map[string]any{
				"Repository": 0, "Accepted (sum)": 1, "Comment (count)": 2, "Up (sum)": 3,
			},
			"renameByName": map[string]any{
				"Accepted (sum)": "Accepted answers", "Comment (count)": "Comments", "Up (sum)": "Upvotes",
			},
		}},
	}
}

// answeredCell is profileBool with a third state: a discussion in a category
// that takes no answer has neither yes nor no to show, and the SQL leaves the
// cell null for it. Three letters, because the column is a hundred pixels and
// the description says what they stand for.
func answeredCell(name string, w int) any {
	return override(name, []any{
		map[string]any{"id": "mappings", "value": []any{
			map[string]any{"type": "value", "options": map[string]any{
				"0": map[string]any{"text": "no", "color": "text", "index": 1},
				"1": map[string]any{"text": "yes", "color": "green", "index": 0},
			}},
			map[string]any{"type": "special", "options": map[string]any{
				"match":  "null",
				"result": map[string]any{"text": "n/a", "color": "text", "index": 2},
			}},
		}},
		map[string]any{"id": "custom.cellOptions", "value": map[string]any{"type": "color-text"}},
		map[string]any{"id": "custom.width", "value": w},
	})
}
