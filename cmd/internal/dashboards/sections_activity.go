package dashboards

import "fmt"

// ── Activity ────────────────────────────────────────────────────────────────

func activity(b *builder) []Panel {
	// The eight busiest event types of the range and the rest as `other`:
	// GitHub has around thirty types, and a legend of that many hid the
	// plot on a phone.
	perHour := topSeries("gh_event", "type", "events", "events", "events > 0")
	// Eight types and the rest folded. palette-classic hands out five
	// families of color and then starts again a shade darker, so at eleven
	// types six of the slices were only told apart by counting the legend:
	// PullRequestReviewEvent and PullRequestReviewCommentEvent were two reds
	// side by side, PushEvent and IssueCommentEvent two greens. The three
	// smallest were one or two pixels of arc, which no finger can hit.
	byType := otherRows(`SELECT type AS "Type", SUM(events) AS "Events",`+
		" ROW_NUMBER() OVER (ORDER BY SUM(events) DESC) AS rn FROM gh_event"+
		" WHERE $__timeFilter(time) GROUP BY 1", "Type", "Events", topSeriesKept)
	// Ten bars and the rest folded: twenty labels in seven units of height
	// could not be read at all.
	byRepo := otherRows(`SELECT repo AS "Repository", SUM(events) AS "Events",`+
		" ROW_NUMBER() OVER (ORDER BY SUM(events) DESC) AS rn FROM gh_event"+
		" WHERE $__timeFilter(time) GROUP BY 1", "Repository", "Events", 10)
	notif := "SELECT " + timeBin + ", reason AS series," +
		" SUM(notifications) AS notifications FROM gh_notification" +
		" WHERE $__timeFilter(time) GROUP BY 1, 2 ORDER BY 1"
	notifTbl := `SELECT reason AS "Reason", SUM(notifications) AS "Notifications",` +
		` subject_type AS "Kind" FROM gh_notification` +
		" WHERE $__timeFilter(time) GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 25"
	// The threads themselves, newest first: the table above counts them by
	// reason and the curve by day, and neither can open the comment that
	// caused one. `notifications` is how many updates the thread had in the
	// sweep, which is what makes a busy thread stand out from a quiet one.
	latest := `SELECT title AS "Title", time AS "Updated", repo AS "Repository",` +
		` subject_type AS "Kind", reason AS "Reason", notifications AS "Updates",` +
		` url AS "Link"` +
		" FROM gh_notification WHERE $__timeFilter(time) ORDER BY time DESC LIMIT 25"
	// An open item is a row per day at midnight while it stays open, so its
	// row's time is when it was seen and not when it was opened; the day it
	// was opened is the row's time minus how long it has been open, and the
	// newest row of each item is the one listed.
	external := `SELECT repo AS "Repository", ` + agoSQL("time", "seconds_open") + ` AS "Opened",` +
		` time AS "Seen", kind AS "Kind",` +
		` state AS "State", title AS "Title", comments AS "Comments",` +
		` url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, kind, number ORDER BY time DESC) AS rn" +
		" FROM gh_external_contribution WHERE $__timeFilter(time)) x WHERE rn = 1" +
		" ORDER BY time DESC LIMIT 40"
	ev, nt, ec := "gh_event", "gh_notification", "gh_external_contribution"
	notes := gp(nt, "notifications")

	typeGR, typeGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, -2, "sum")`, events("events")),
		"Type", []col{{"sum", "Events"}})
	typeES, typeEStf := esTbl(ev, []any{b.tm("type", 30, "1")}, []any{b.mSum("events")},
		[]named{{"type.keyword", "Type"}, {"e", "Events"}}, nil)

	repoGR, repoGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(groupByNode(%s, -3, "sum")), 20)`, events("events"),
	),
		"Repository", []col{{"sum", "Events"}})
	repoES, repoEStf := esTbl(ev, []any{b.tm("repo", 20, "1")}, []any{b.mSum("events")},
		[]named{{"repo.keyword", "Repository"}, {"e", "Events"}}, nil)

	notifGR, notifGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(groupByNodes(%s, "sum", %d, %d)), 25)`,
		notes, gn(nt, "reason"), gn(nt, "subject_type"),
	),
		"Reason, kind", []col{{"sum", "Notifications"}})
	notifES, notifEStf := esTbl(nt, []any{b.tm("reason", 25, "1"), b.tm("subject_type", 10)},
		[]any{b.mSum("notifications")},
		[]named{
			{"reason.keyword", "Reason"},
			{"subject_type.keyword", "Kind"},
			{"n", "Notifications"},
		}, nil)

	latestES, latestEStf := b.esRaw(nt, 25, []named{
		{"@timestamp", "Updated"},
		{"repo", "Repository"},
		{"subject_type", "Kind"},
		{"reason", "Reason"},
		{"title", "Title"},
		{"notifications", "Updates"},
		{"url", "Link"},
	}, nil)

	extGR, extGRtf := gTbl(rowsOf(gp(ec, "comments"), gn(ec, "repo"), gn(ec, "kind"),
		gn(ec, "number"), gn(ec, "state")),
		"Repository, kind, number, state", []col{{"lastNotNull", "Comments"}})
	extES, extEStf := b.esRaw(ec, 40, []named{
		{"@timestamp", "Seen"},
		{"repo", "Repository"},
		{"kind", "Kind"},
		{"state", "State"},
		{"title", "Title"},
		{"comments", "Comments"},
		{"url", "Link"},
	}, nil)

	return append([]Panel{
		panel("timeseries", "Events over time", 16, 8, 0, 0, []Target{sqlTS(perHour)}, &P{
			Prom: []Target{hourly("sum by (type) (increase(github_events_total[1h]))", "{{type}}")},
			Opts: mergeOpts(Opts{"bars": true, "stack": true}, hourBins), SQLOpts: seriesOpts,
			Desc: "GitHub keeps only the last 300 events and drops the rest whatever their " +
				"date, so this is only as complete as the sweep interval allowed. The eight " +
				"busiest types in the range are named; the rest are `other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			GR:       []Target{grq(perBucket(events("events"), -2, "1h"))},
			ES:       []Target{b.esDaily(ev, b.mSum("events"), "type", "1h", nil, "")},
		}),
		// The pie takes the whole height of the hourly chart and of the two
		// tables under it. The account's feed carries nine event types in a
		// week and eleven in a month (measured; the events API knows
		// seventeen), and the legend is a list under the donut with the
		// share of each: on a phone, where Grafana puts every legend under
		// the chart at 35 per cent of the panel, a list placed bottom wraps,
		// and sixteen units is the height at which the eleven fit at 360
		// pixels; a legend placed right or a table is a column there that
		// showed seven and scrolled. At 1920 the donut is 400 pixels over
		// three lines of legend, which reads; a legend beside it did not
		// survive the phone.
		panel("piechart", "Events by type", 8, 16, 16, 0, []Target{sqlT(byType)}, &P{
			Prom: []Target{promTbl("sum by (type) (increase(github_events_total[$__range]))")},
			// No rename: a displayName on the value column would name every
			// slice "Events"; without it the pie names each slice by its row.
			PromTF: []any{organize(nil, nil, nil)}, PromDesc: sinceStart,
			Desc: "The share of each event type in the range, in the legend. The eight " +
				"busiest types are named; the rest are one slice called `other`.",
			Opts: Opts{"legend": "bottom"},
			GR:   typeGR, GRTF: typeGRtf,
			ES: typeES, ESTF: typeEStf,
		}),
		panel("barchart", "Events by repository", 8, 8, 0, 8, []Target{sqlT(byRepo)}, &P{
			Desc: "The ten repositories with the most events; the rest are one bar called other.",
			PromNote: cannot("the ten repositories with the most events in the range.",
				"The exporter keeps only `type` on events: a repository label "+
					"would multiply the series by every repository the feed touches, "+
					"including other people's."),
			GR: repoGR, GRTF: repoGRtf,
			ES: repoES, ESTF: repoEStf,
		}),
		panel("table", "Notifications", 8, 8, 8, 8, []Target{sqlT(notifTbl)}, &P{
			Prom: []Target{promTbl(
				"sum by (reason, subject_type) (increase(github_notifications_total[$__range]))",
			)},
			PromTF: []any{organize(map[string]string{
				"reason": "Reason", "subject_type": "Kind", "Value": "Notifications",
			}, nil, nil)},
			Opts: Opts{"sort": "Notifications"}, PromDesc: sinceStart,
			Overrides: []any{barCell("Notifications", "short", 120)},
			GR:        notifGR, GRTF: notifGRtf,
			ES: notifES, ESTF: notifEStf,
		}),
		panel("timeseries", "Notifications over time", 24, 8, 0, 16, []Target{sqlTS(notif)}, &P{
			Prom: []Target{daily("sum by (reason) (increase(github_notifications_total[1d]))",
				"{{reason}}")},
			Opts: mergeOpts(Opts{"bars": true, "stack": true}, dayBins), SQLOpts: seriesOpts, PromDesc: sinceStart,
			GR:   []Target{grq(perBucket(notes, gn(nt, "reason")))},
			ES:   []Target{b.esDaily(nt, b.mSum("notifications"), "reason", "", nil, "")},
			Desc: bucketFollowsRange,
		}),
		panel("table", "Latest notifications", 24, 7, 0, 24, []Target{sqlT(latest)}, &P{
			PromNote: cannot("the newest notification threads, with their titles and a link "+
				"to each.",
				"The exporter reduces notifications to a count per reason and subject "+
					"type; no thread survives, and the title and the url are strings."),
			GRNote: cannot("the newest notification threads, with their titles and a link "+
				"to each.",
				"Graphite keeps no strings, and has no way to list by date.", "graphite"),
			Desc: "The threads behind the counts above, newest first. Updates is how many " +
				"times the thread moved in one sweep; the link opens the comment or " +
				"review that caused the newest one.",
			Overrides: []any{
				when("Updated"), repoColumn(), width("Kind", 110),
				width("Reason", 120), width("Updates", 90), width("Title", 200), linkOn("Title"),
			},
			ES: latestES, ESTF: latestEStf, ESDesc: esNewest,
		}),
		panel("table", "Work elsewhere", 24, 9, 0, 31,
			[]Target{sqlT(external)}, &P{
				Prom: []Target{
					promTbl("sum by (repo) (increase(github_external_contributions_total[$__range]))", "A"),
					promTbl("avg by (repo) (github_external_contributions_merged_mean)", "B"),
					promTbl("avg by (repo) (github_external_contributions_comments_mean)", "C"),
				},
				PromTF: merged(map[string]string{
					"repo": "Repository", "Value #A": "Contributions", "Value #B": "Merged",
					"Value #C": "Comments",
				}, nil, nil),
				Desc: "Pull requests and issues opened in repositories this account does not own, " +
					"with the state each ended in. Nothing else sees them: they are not in these " +
					"repositories and the event feed forgets them in three days. Seen is the " +
					"row's own date, which for an open item is the day it was last seen open; " +
					"Opened is when it was opened.",
				PromDesc: "Prometheus keeps the repository only, so this is contributions per " +
					"repository over the range, the share merged and the mean comments. " +
					sinceStart,
				Overrides: []any{
					when("Opened"), when("Seen"), repoColumn(),
					width("Kind", 110), width("State", 90), width("Comments", 100), linkOn("Repository"),
				},
				PromOver: []any{
					barCell("Contributions", "short", 160),
					unitOf("Merged", "percentunit", 100),
				},
				GR: extGR, GRTF: extGRtf,
				GRDesc: "Graphite names each row from the path and keeps no text, so the title is missing. " + grRows,
				ES:     extES, ESTF: extEStf,
			}),
	}, starsGiven(b)...)
}

// starsGiven is the outbound direction of the section: what this account
// starred in other people's repositories, by language and by repository.
func starsGiven(b *builder) []Panel {
	langGR, langGRtf := gTbl(fmt.Sprintf(
		`limit(sortByMaxima(groupByNode(%s, %d, "sum")), 12)`,
		countOf(gp("gh_star_given", "stars")), gn("gh_star_given", "language"),
	),
		"Language", []col{{"sum", "Stars given"}})
	langES, langEStf := esTbl("gh_star_given", []any{b.tm("language", 12)}, []any{b.mCount()},
		[]named{{"language.keyword", "Language"}, {"n", "Stars given"}}, nil)

	starGR, starGRtf := gTbl(rowsOf("keepLastValue("+gp("gh_star_given", "repo_stars")+")",
		gn("gh_star_given", "repo")), "Repository", []col{{"lastNotNull", "Its stars"}})
	starES, starEStf := b.esRaw("gh_star_given", 25, []named{
		{"@timestamp", "When"},
		{"repo", "Repository"},
		{"language", "Language"},
		{"repo_stars", "Its stars"},
		{"url", "Link"},
	}, nil)
	return []Panel{
		panel("barchart", "Languages starred", 12, 8, 0, 40, []Target{sqlT(
			`SELECT language AS "Language", COUNT(*) AS "Stars given"` +
				" FROM gh_star_given WHERE $__timeFilter(time) AND language <> ''" +
				" GROUP BY 1 ORDER BY 2 DESC LIMIT 12",
		)}, &P{
			Prom: []Target{promTbl(
				"topk(12, sum by (language) (increase(github_stars_given_total[$__range])))",
			)},
			PromTF: []any{organize(map[string]string{
				"language": "Language", "Value": "Stars given",
			}, nil, nil)},
			Desc: "The mirror of the stars received: what this account was reading, dated when " +
				"it starred it. The only measurement here about somebody else's work.",
			PromDesc: sinceStart,
			GR:       langGR, GRTF: langGRtf,
			ES: langES, ESTF: langEStf,
		}),
		panel("table", "Recently starred", 12, 8, 12, 40, []Target{sqlT(
			`SELECT repo AS "Repository", time AS "When",` +
				` language AS "Language", repo_stars AS "Its stars",` +
				` url AS "Link"` +
				" FROM gh_star_given WHERE $__timeFilter(time)" +
				" ORDER BY time DESC LIMIT 25",
		)}, &P{
			Prom: []Target{promTbl("topk(25, github_stars_given_repo_stars_mean)")},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "language": "Language", "Value": "Its stars",
			}, []string{"user", "instance", "job", "__name__"}, nil)},
			Desc: "Whether the account stars small projects or famous ones, which the language " +
				"breakdown cannot say.",
			PromDesc: lastSweep,
			Overrides: []any{
				when("When"), width("Language", 120),
				barCell("Its stars", "short", 120), linkOn("Repository"),
			},
			GR: starGR, GRTF: starGRtf, GRDesc: grSlot,
			ES: starES, ESTF: starEStf,
		}),
	}
}
