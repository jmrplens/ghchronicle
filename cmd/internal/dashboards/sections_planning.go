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
		"PARTITION BY repo, label ORDER BY time DESC) AS rn FROM gh_label" +
		" WHERE $__timeFilter(time) AND " + RF + planningNewestRow +
		" ORDER BY 2 DESC LIMIT 25"
	miles := `SELECT milestone AS "Milestone", progress AS "Progress", repo AS "Repository",` +
		` state AS "State", issues AS "Issues",` +
		` pull_requests AS "Pull requests", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, milestone ORDER BY time DESC) AS rn" +
		" FROM gh_milestone WHERE $__timeFilter(time) AND " + RF + planningNewestRow +
		" ORDER BY 2 DESC LIMIT 25"
	forks := topSeries("gh_fork", "repo", "1", "forks", RF)
	forkTbl := `SELECT by AS "By", time AS "Forked", repo AS "Repository",` +
		` advanced AS "Pushed to", days_since_push AS "Idle",` +
		` url AS "Link"` +
		" FROM gh_fork WHERE $__timeFilter(time) AND " + RF + " ORDER BY time DESC LIMIT 25"
	// seconds_to_answer is a field only once a discussion has been answered:
	// InfluxDB creates the column on first write, and naming it before then
	// fails the whole query. Same reason the milestone due date is not shown.
	lb, ms, fk := "gh_label", "gh_milestone", "gh_fork"

	labelsGR, labelsGRtf := gTbl(rowsOf(fmt.Sprintf(
		`limit(sortByMaxima(keepLastValue(%s)), 25)`, rp(lb, "used"),
	), gn(lb, "repo"), gn(lb, "label")), "Repository, label", []col{{"lastNotNull", "Used"}})
	labelsES, labelsEStf := esTbl(lb, []any{b.tm("repo", 50), b.tm("label", 25), b.tmURL()},
		[]any{b.mNewest("used", "issues", "pull_requests")},
		[]named{
			{panelRepoField, "Repository"},
			{"label.keyword", "Label"},
			{"url.keyword", "Link"},
			{"used", "Used"},
			{"issues", "Issues"},
			{"pull_requests", planningPullRequests},
		}, []string{ESF})

	milesGR, milesGRtf := gTbl(topRows(rp(ms, "progress"), 25,
		gn(ms, "repo"), gn(ms, "milestone"), gn(ms, "state")),
		"Milestone", []col{{"lastNotNull", "Progress"}})
	milesES, milesEStf := esTbl(ms, []any{b.tm("repo", 50), b.tm("milestone", 25), b.tm("state", 5), b.tmURL()},
		[]any{b.mNewest("progress", "issues", "pull_requests")},
		[]named{
			{panelRepoField, "Repository"},
			{"milestone.keyword", "Milestone"},
			{"state.keyword", "State"},
			{"url.keyword", "Link"},
			{"progress", "Progress"},
			{"issues", "Issues"},
			{"pull_requests", planningPullRequests},
		}, []string{ESF})

	forkGR, forkGRtf := gTbl(rowsOf(rp(fk, "days_since_push"), gn(fk, "by"), gn(fk, "repo")),
		"By, repository", []col{{"lastNotNull", "Idle"}})
	forkES, forkEStf := b.esRaw(fk, 25, []named{
		{panelESTime, "Forked"},
		{"by", "By"},
		{"repo", "Repository"},
		{"advanced", planningPushedTo},
		{"days_since_push", "Idle"},
		{"url", "Link"},
	}, []string{ESF})
	return []Panel{
		panel("table", "Labels", box{W: 12, H: 8, X: 0, Y: 0}, []Target{sqlT(labels)}, &P{
			Prom: func() []Target {
				rank := fmt.Sprintf("max by (repo, label) (github_label_used{%s})", PF)
				return []Target{
					promTbl(promTop(25, rank), "A"),
					promTbl(promWithin(25, fmt.Sprintf(
						"max by (repo, label) (github_label_issues{%s})", PF,
					), rank, "repo", "label"), "B"),
					promTbl(promWithin(25, fmt.Sprintf(
						"max by (repo, label) (github_label_pull_requests{%s})", PF,
					), rank, "repo", "label"), "C"),
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
				promTbl(fmt.Sprintf("sum by (repo, milestone, state) (github_milestone_progress{%s})", PF), "A"),
				promTbl(fmt.Sprintf("sum by (repo, milestone, state) (github_milestone_issues{%s})", PF), "B"),
				promTbl(fmt.Sprintf("sum by (repo, milestone, state) (github_milestone_pull_requests{%s})", PF), "C"),
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
				"sum by (repo) (increase(github_forks_seen_total{%s}[1d]))", PF,
			), "{{repo}}")},
			Opts:    mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts: seriesOpts,
			Desc: "Dated when each fork was created, which the fork count on a repository " +
				"never says. The eight repositories that gained the most in the range are " +
				"named; the rest are `other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			GR:       []Target{grq(perBucket("isNonNull("+rp(fk, "forks")+")", gn(fk, "repo")))},
			ES:       []Target{b.esDaily(fk, b.mCount(), "repo", "", []string{ESF}, "")},
		}),
		panel("table", "Forks", box{W: 12, H: 8, X: 12, Y: 8}, []Target{sqlT(forkTbl)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (repo) (increase(github_forks_seen_total{%s}[$__range]))", PF), "A"),
				promTbl(fmt.Sprintf("avg by (repo) (github_forks_seen_advanced_mean{%s})", PF), "B"),
				promTbl(fmt.Sprintf("avg by (repo) (github_forks_seen_days_since_push_mean{%s})", PF), "C"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", panelValueA: "Forks", panelValueB: planningPushedTo,
				panelValueC: "Idle",
			}, nil, nil),
			Desc: "Whether a fork was ever pushed to separates a derivative from a bookmark, " +
				"which most forks are.",
			PromDesc: "Prometheus keeps no forker, so this is per repository: forks seen over " +
				"the range, the share ever pushed to, and the mean idle time. " + sinceStart,
			Overrides: []any{
				when("Forked"), width(planningPushedTo, 110), unitOf("Idle", "d", 90),
				linkOn("By"),
			},
			PromOver: []any{
				unitOf(planningPushedTo, "percentunit", 110),
				barCell("Forks", "short", 120),
			},
			GR: forkGR, GRTF: forkGRtf,
			GRDesc: "Graphite names each row forker and repository from the path, and a boolean " +
				"is not a metric there, so whether it was pushed to is missing. " + grRows,
			ES: forkES, ESTF: forkEStf,
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
		` url AS "Link" FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, number` +
		" ORDER BY time DESC, comments DESC) AS rn FROM gh_discussion" +
		" WHERE " + wholeHistory + " AND " + RF + planningNewestRow +
		" ORDER BY 2 DESC LIMIT 50"
	// The comments this account left in discussions of repositories it does
	// not own, and whether each was marked the accepted answer. The comment
	// is dated when it was written; the link opens it in its thread. One row
	// per comment: is_answer is a tag, so a comment seen before the
	// maintainer accepted it and again after is two rows at one instant,
	// and the one whose answers field says accepted is the later state.
	elsewhere := `SELECT title AS "Title", time AS "When", repo AS "Repository",` +
		` answers AS "Accepted", url AS "Link" FROM (SELECT *, ROW_NUMBER() OVER` +
		" (PARTITION BY comment ORDER BY answers DESC) AS rn FROM gh_discussion_comment" +
		" WHERE " + wholeHistory + " AND own = 'false') x WHERE rn = 1" +
		" ORDER BY 2 DESC LIMIT 50"
	dcc := "gh_discussion_comment"
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
	elsewhereGR, elsewhereGRtf := gTbl(rowsOf(gp(dcc, "comments", "own", "false"), gn(dcc, "repo"),
		gn(dcc, "number"), gn(dcc, "is_answer")),
		"Repository, number, accepted", []col{{"lastNotNull", "Comments"}})
	elsewhereES, elsewhereEStf := b.esRaw(dcc, 50, []named{
		{"title", "Title"},
		{panelESTime, "When"},
		{"repo", "Repository"},
		{"is_answer", "Accepted"},
		{"url", "Link"},
	}, []string{"own:false"})
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

	commentsGR, commentsGRtf := gTbl(rowsOf(countOf(gp("gh_issue_comment", "comments")),
		gn("gh_issue_comment", "repo")), "Repository", []col{{"sum", "Comments"}})
	commentsES, commentsEStf := esTbl("gh_issue_comment", []any{b.tm("repo", 20)}, []any{b.mCount()},
		[]named{{panelRepoField, "Repository"}, {"n", "Comments"}}, nil)

	answersGR, answersGRtf := gTbl(rowsOf(countOf(gp("gh_discussion_comment", "comments")),
		gn("gh_discussion_comment", "repo")), "Repository", []col{{"sum", "Comments"}})
	answersES, answersEStf := esTbl("gh_discussion_comment", []any{b.tm("repo", 20)},
		[]any{b.mCount(), b.mSum("answers"), b.mSum("upvotes")},
		[]named{
			{panelRepoField, "Repository"},
			{"n", "Comments"},
			{"a", "Accepted answers"},
			{"u", "Upvotes"},
		}, nil)
	return []Panel{
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
			GR: []Target{grq(perBucket(countOf(rp("gh_issue_event", "events")),
				gn("gh_issue_event", "event")))},
			ES: []Target{b.esDaily("gh_issue_event", b.mCount(), "event", "", []string{ESF}, "")},
		}),
		panel("table", "Comments left", box{W: 12, H: 8, X: 12, Y: 24}, []Target{sqlT(
			`SELECT repo AS "Repository", COUNT(*) AS "Comments",` +
				` MAX(CASE WHEN own = 'false' THEN 1 ELSE 0 END) AS "Elsewhere"` +
				" FROM gh_issue_comment WHERE $__timeFilter(time)" +
				" GROUP BY 1 ORDER BY 2 DESC LIMIT 20",
		)}, &P{
			Prom:   []Target{promTbl("topk(20, sum by (repo) (increase(github_issue_comments_total[$__range])))")},
			PromTF: []any{organize(map[string]string{"repo": "Repository", "Value": "Comments"}, nil, nil)},
			Opts:   Opts{"sort": "Comments"},
			Desc: "Comments on issues and pull requests, in any repository. The ones outside " +
				"this account are the half a sweep over one's own repositories cannot see.",
			PromDesc:  sinceStart,
			Overrides: []any{barCell("Comments", "short", 120)},
			GR:        commentsGR, GRTF: commentsGRtf, GRDesc: grSlot,
			ES: commentsES, ESTF: commentsEStf,
		}),
		panel("table", "Discussion answers", box{W: 8, H: 8, X: 0, Y: 32}, []Target{sqlT(
			`SELECT repo AS "Repository", SUM(answers) AS "Accepted answers",` +
				` COUNT(*) AS "Comments", SUM(upvotes) AS "Upvotes"` +
				" FROM gh_discussion_comment WHERE $__timeFilter(time)" +
				" GROUP BY 1 ORDER BY 2 DESC, 3 DESC LIMIT 20",
		)}, &P{
			Prom:   []Target{promTbl("topk(20, sum by (repo) (increase(github_discussion_comments_total[$__range])))")},
			PromTF: []any{organize(map[string]string{"repo": "Repository", "Value": "Comments"}, nil, nil)},
			Opts:   Opts{"sort": "Accepted answers"},
			Desc: "Discussions are the one surface where the work is almost entirely in other " +
				"people's repositories: gh_discussion sees three rows inside this account, " +
				"and this sees sixty six across twenty six repositories.",
			PromDesc:  sinceStart,
			Overrides: []any{barCell("Comments", "short", 120)},
			GR:        answersGR, GRTF: answersGRtf, GRDesc: grSlot,
			ES: answersES, ESTF: answersEStf,
		}),
		panel("table", "Answers elsewhere", box{W: 16, H: 8, X: 8, Y: 32}, []Target{sqlT(elsewhere)}, &P{
			PromNote: cannot("the newest fifty comments this account left in other people's "+
				"discussions, whether each was accepted as the answer, and a link to each.",
				perItem),
			GRDesc: "Graphite names each row repository, number and whether the comment was " +
				"accepted from the path. " + grPerItem + " " + grRows,
			Desc: "Every comment this account left in a discussion of a repository it does " +
				"not own, newest first and whatever the range; Accepted says whether the " +
				"maintainer marked it the answer. The link opens the comment in its thread.",
			Opts: Opts{"sort": "When"},
			Overrides: []any{
				when("When"), repoColumn(),
				profileBool("Accepted", 100), linkOn("Title"),
			},
			GR: elsewhereGR, GRTF: elsewhereGRtf,
			ES: elsewhereES, ESTF: elsewhereEStf, ESDesc: esNewest + " " + esRange,
			ESOver: []any{width("Accepted", 100)},
		}),
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
