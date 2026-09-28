package dashboards

import "fmt"

// activityItsStars is how many stars the starred repository itself carries,
// as against the star this account gave it; the Elasticsearch, Graphite and
// Prometheus twins rename their value column to the same words.
const activityItsStars = "Its stars"

// eachTypeASlice turns the table "Events by type" answers, a row per type
// with its name and its count, into one field per type, named by the type,
// which is the shape a pie colors slice by slice.
//
// With the rows as they come, every slice was the one count field, and the
// palette gives a color per field: the 2.6.1 review found every slice the same
// green in all five stores. It named every slice "Events" in Graphite and
// Elasticsearch as well, since a rename is a display name on the count field
// and the pie names a row by it before the row's own text. A field made of a
// row carries neither.
var eachTypeASlice = map[string]any{"id": "rowsToFields", "options": map[string]any{"mappings": []any{
	map[string]any{"fieldName": "Type", "handlerKey": "field.name"},
	map[string]any{"fieldName": "Events", "handlerKey": "field.value"},
}}}

// esNoOther is what the Elasticsearch pie says in place of folding, since the
// types past the busiest eight have no slice to go into (see esUnfolded). The
// pie keeps thirty of them rather than eight, since a pie of the eight alone
// would draw each share of those eight and read larger than it is.
var esNoOther = esUnfolded(30, "types", "slice")

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
		" ROW_NUMBER() OVER (ORDER BY SUM(events) DESC, type) AS rn FROM gh_event"+
		" WHERE $__timeFilter(time) GROUP BY 1", "Type", "Events", topSeriesKept)
	// Ten bars and the rest folded: twenty labels in seven units of height
	// could not be read at all.
	// Named in full here and in every other panel over a measurement that can
	// hold more than one owner. `repo` is the short name on every measurement
	// now, and the feed, the inbox, the outbound contributions and the stars
	// given are mostly about other people's repositories, where a bare name
	// identifies nothing and two owners using the same one become one row.
	byRepo := otherRows(`SELECT full_name AS "Repository", SUM(events) AS "Events",`+
		" ROW_NUMBER() OVER (ORDER BY SUM(events) DESC, full_name) AS rn FROM gh_event"+
		" WHERE $__timeFilter(time) GROUP BY 1", "Repository", "Events", 10)
	notif := "SELECT " + timeBin + ", reason AS series," +
		" SUM(notifications) AS notifications FROM gh_notification" +
		" WHERE $__timeFilter(time) GROUP BY 1, 2 ORDER BY 1"
	notifTbl := `SELECT reason AS "Reason", SUM(notifications) AS "Notifications",` +
		` subject_type AS "Kind" FROM gh_notification` +
		" WHERE $__timeFilter(time) GROUP BY 1, 3 ORDER BY 2 DESC, 1, 3 LIMIT 25"
	// The threads themselves, newest first: the table above counts them by
	// reason and the curve by day, and neither can open the comment that
	// caused one. `notifications` is how many updates the thread had in the
	// sweep, which is what makes a busy thread stand out from a quiet one.
	latest := `SELECT title AS "Title", time AS "Updated", full_name AS "Repository",` +
		` subject_type AS "Kind", reason AS "Reason", notifications AS "Updates",` +
		` url AS "Link"` +
		" FROM gh_notification WHERE $__timeFilter(time) ORDER BY time DESC, full_name, subject_type, reason LIMIT 25"
	ev, nt := "gh_event", "gh_notification"
	notes := gp(nt, "notifications")

	typeGR, typeGRtf := gTbl(grOther(topSeriesKept, fmt.Sprintf(`groupByNode(%s, -2, "sum")`, events("events"))),
		"Type", []col{{"sum", "Events"}})
	typeEvents := b.mSum("events")
	typeES, typeEStf := esTbl(ev, []any{b.tmBy("type", 30, typeEvents)}, []any{typeEvents},
		[]named{{"type.keyword", "Type"}, {"e", "Events"}}, nil)

	repoGR, repoGRtf := gTbl(grOther(10, fmt.Sprintf(`groupByNode(%s, %d, "sum")`, events("events"), gn(ev, "full_name"))),
		"Repository", []col{{"sum", "Events"}})
	repoEvents := b.mSum("events")
	repoES, repoEStf := esTbl(ev, []any{b.tmBy("full_name", 10, repoEvents)}, []any{repoEvents},
		[]named{{"full_name.keyword", "Repository"}, {"e", "Events"}}, nil)

	notifGR, notifGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(groupByNodes(%s, "sum", %d, %d)), 25)`,
		notes, gn(nt, "reason"), gn(nt, "subject_type"),
	),
		"Reason, kind", []col{{"sum", "Notifications"}})
	notified := b.mSum("notifications")
	notifES, notifEStf := esTbl(nt, []any{b.tmBy("reason", 25, notified), b.tm("subject_type", 10)},
		[]any{notified},
		[]named{
			{"reason.keyword", "Reason"},
			{"subject_type.keyword", "Kind"},
			{"n", "Notifications"},
		}, nil)

	latestES, latestEStf := b.esRaw(nt, 25, []named{
		{panelESTime, "Updated"},
		{"full_name", "Repository"},
		{"subject_type", "Kind"},
		{"reason", "Reason"},
		{"title", "Title"},
		{"notifications", "Updates"},
		{"url", "Link"},
	}, nil)

	return append([]Panel{
		panel("timeseries", "Events over time", box{W: 16, H: 8, X: 0, Y: 0}, []Target{sqlTS(perHour)}, &P{
			Prom: []Target{hourly(promOtherOverTime(
				"sum by (type) (increase(github_events_total[1h]))", "type", "type",
			), "{{type}}")},
			Opts: mergeOpts(Opts{"bars": true, "stack": true}, hourBins), SQLOpts: seriesOpts,
			Desc: "GitHub keeps only the last 300 events, none older than thirty days, " +
				"so this is only as complete as the sweep interval allowed. The eight " +
				"busiest types in the range are named; the rest are `other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			GR:       []Target{grq(grOther(topSeriesKept, perBucket(events("events"), -2, "1h")))},
			ES:       []Target{b.esDaily(ev, b.mSum("events"), "type", "1h", nil, "")},
			ESDesc:   esUnfolded(esDailyTerms, "types", "series"),
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
		panel("piechart", "Events by type", box{W: 8, H: 16, X: 16, Y: 0}, []Target{sqlT(byType)}, &P{
			Prom: []Target{promTbl(promOther(topSeriesKept,
				"sum by (type) (increase(github_events_total[$__range]))", "type"))},
			PromTF: []any{
				organize(map[string]string{"type": "Type", "Value": "Events"}, nil),
				eachTypeASlice,
			},
			PromDesc: sinceStart,
			Desc: "The share of each event type in the range, in the legend. The eight " +
				"busiest types are named; the rest are one slice called `other`.",
			Opts:  Opts{"legend": "bottom"},
			SQLTF: []any{eachTypeASlice},
			GR:    typeGR, GRTF: append(typeGRtf, eachTypeASlice),
			ES: typeES, ESTF: append(typeEStf, eachTypeASlice),
			ESDesc: esNoOther,
		}),
		panel("barchart", "Events by repository", box{W: 8, H: 8, X: 0, Y: 8}, []Target{sqlT(byRepo)}, &P{
			Desc: "The ten repositories with the most events; the rest are one bar called other.",
			PromNote: cannot("the ten repositories with the most events in the range.",
				"The exporter keeps only `type` on events: a repository label "+
					"would multiply the series by every repository the feed touches, "+
					"including other people's."),
			GR: repoGR, GRTF: repoGRtf,
			ES: repoES, ESTF: repoEStf, ESDesc: esUnfolded(10, "repositories", "bar"),
		}),
		panel("table", "Notifications", box{W: 8, H: 8, X: 8, Y: 8}, []Target{sqlT(notifTbl)}, &P{
			Prom: []Target{promTbl(
				"sum by (reason, subject_type) (increase(github_notifications_total[$__range]))",
			)},
			PromTF: []any{organize(map[string]string{
				"reason": "Reason", "subject_type": "Kind", "Value": "Notifications",
			}, nil)},
			Opts: Opts{"sort": "Notifications"}, PromDesc: sinceStart,
			Overrides: []any{barCell("Notifications", "short", 120)},
			GR:        notifGR, GRTF: notifGRtf,
			ES: notifES, ESTF: notifEStf,
		}),
		panel("timeseries", "Notifications over time", box{W: 24, H: 8, X: 0, Y: 16}, []Target{sqlTS(notif)}, &P{
			Prom: []Target{daily("sum by (reason) (increase(github_notifications_total[1d]))",
				"{{reason}}")},
			Opts: mergeOpts(Opts{"bars": true, "stack": true}, dayBins), SQLOpts: seriesOpts, PromDesc: sinceStart,
			GR:   []Target{grq(perBucket(notes, gn(nt, "reason")))},
			ES:   []Target{b.esDaily(nt, b.mSum("notifications"), "reason", "", nil, "")},
			Desc: bucketFollowsRange,
		}),
		panel("table", "Latest notifications", box{W: 24, H: 7, X: 0, Y: 24}, []Target{sqlT(latest)}, &P{
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
				when("Updated"), fullNameColumn(), width("Kind", 110),
				width("Reason", 120), width("Updates", 90), width("Title", 200), linkOn("Title"),
			},
			ES: latestES, ESTF: latestEStf, ESDesc: esNewest,
		}),
		workElsewhere(b),
	}, starsGiven(b)...)
}

// workElsewhere is the pull requests and issues this account opened in other
// people's repositories, one row per item. It is a function of its own
// because each store reads it in its own way, and inside activity() the five
// of them pushed that function past the maintainability the linter holds it to.
func workElsewhere(b *builder) Panel {
	// An open item is a row per day at midnight while it stays open, so its
	// row's time is when it was seen and not when it was opened; the day it
	// was opened is the row's time minus how long it has been open, and the
	// newest row of each item is the one listed.
	//
	// The repository's stars are not on the item's row, which is dated when
	// the item closed: they are gh_upstream_repo's, stamped at each sweep,
	// and the newest of those inside the range is joined on. Inside the
	// range and not the whole history, because that measurement is a row
	// per repository per sweep, and because a range in the past then shows
	// the count as it stood then.
	//
	// The state is a tag, so an item has a row for each state it was seen in
	// and the newest decides. Two of those rows share an instant only when an
	// item closed at the very midnight its last open row is stamped with,
	// since GitHub's searches for each state are disjoint and date a closed
	// item when it closed; the state's name makes the pick the same in both
	// SQL stores then, as the Elasticsearch buckets, ordered by their key,
	// already make it.
	//
	// The rows themselves tie on the instant as a rule: every item still
	// open is stamped at the start of the day it was last seen, so all of
	// them are one Seen, and the order among them was whatever each store's
	// sort left, which was not the same in InfluxDB and PostgreSQL. The
	// item's own identity breaks that tie.
	external := `SELECT x.full_name AS "Repository", ` + agoSQL("x.time", "x.seconds_open") + ` AS "Opened",` +
		` x.time AS "Seen", x.kind AS "Kind",` +
		` x.state AS "State", x.title AS "Title", u.stars AS "Stars", x.comments AS "Comments",` +
		` x.url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, kind, number ORDER BY time DESC, state) AS rn" +
		" FROM gh_external_contribution WHERE $__timeFilter(time)) x" +
		" LEFT JOIN (SELECT full_name, stars, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn" +
		" FROM gh_upstream_repo WHERE $__timeFilter(time)) u ON u.full_name = x.full_name AND u.rn = 1" +
		" WHERE x.rn = 1 ORDER BY x.time DESC, x.full_name, x.kind, x.number LIMIT 40"
	ec := "gh_external_contribution"
	extGR, extGRtf := gTbl(rowsOf(gp(ec, "comments"), gn(ec, "full_name"), gn(ec, "kind"),
		gn(ec, "number"), gn(ec, "state")),
		"Repository, kind, number, state", []col{{"lastNotNull", "Comments"}})
	// One row per item, its newest document, as the SQL stores read it. The
	// newest forty documents listed an open item once for every day it was
	// seen open, since each of those days is a document of its own, and left
	// the older items out to make room. So each item is a bucket, its newest
	// timestamp the one below it, and what the row shows is that document's:
	// its state, its title and its url as one-value buckets, the way a string
	// reaches an Elasticsearch table, and its comments. Seen is the newest
	// timestamp again as a metric, since a bucket above the last one reaches
	// the table as its key's text and not as a date the override can draw,
	// the way "Oldest open alerts" reads the date an alert was raised.
	extES, extEStf := esTbl(ec, []any{
		b.tm("full_name", 100), b.tm("kind", 2), b.tm("number", 200),
		b.terms(panelESTime, 1, "_key", "desc"),
		b.tm("state", 1), b.tmURL("title"), b.tmURL(),
	}, []any{b.mMax(panelESTime), b.mMax("comments")},
		[]named{
			{panelFullNameField, "Repository"},
			{"kind.keyword", "Kind"},
			{"state.keyword", "State"},
			{"title.keyword", "Title"},
			{panelURLField, "Link"},
			{"s", "Seen"},
			{"c", "Comments"},
		}, nil, hideColumns("number.keyword", panelESTime))

	return panel("table", "Work elsewhere", box{W: 24, H: 9, X: 0, Y: 31},
		[]Target{sqlT(external)}, &P{
			Prom: []Target{
				promTbl("sum by (full_name) (increase(github_external_contributions_total[$__range]))", "A"),
				promTbl("avg by (full_name) (github_external_contributions_merged_mean)", "B"),
				promTbl("avg by (full_name) (github_external_contributions_comments_mean)", "C"),
				promTbl("max by (full_name) (github_upstream_repo_stars)", "D"),
			},
			PromTF: merged(map[string]string{
				"full_name": "Repository", panelValueA: "Contributions", panelValueB: "Merged",
				panelValueC: "Comments", panelValueD: "Stars",
			}, nil),

			// The counts of the items the SQL stores list one per row.
			PromAt: map[string]string{"Contributions": "Kind", "Merged": "State"},
			Desc: "Pull requests and issues opened in repositories this account does not own, " +
				"with the state each ended in. Nothing else sees them: they are not in these " +
				"repositories, and the event feed keeps only its last three hundred events, " +
				"none older than thirty days. Seen is the " +
				"row's own date, which for an open item is the day it was last seen open; " +
				"Opened is when it was opened. Stars is the repository's own count at the " +
				"last sweep inside the range.",
			PromDesc: "Prometheus keeps the repository only, so this is contributions per " +
				"repository over the range, the share merged and the mean comments, with no " +
				"Kind, State or Title column, and no Seen or Opened column, since an item's " +
				"dates do not survive the exporter. " +
				"Its Stars is the count the exporter holds at the end of the range, whenever " +
				"the sweep that read it ran. " + sinceStart,
			Overrides: []any{
				when("Opened"), when("Seen"), fullNameColumn(),
				width("Kind", 110), width("State", 90), width("Comments", 100),
				unitOf("Stars", "short", 90), linkOn("Repository"),
			},
			PromOver: []any{
				barCell("Contributions", "short", 160),
				unitOf("Merged", "percentunit", 100),
			},
			GR: extGR, GRTF: extGRtf,
			GRDesc: "Graphite names each row from the path and keeps one number per row, the " +
				"comments, so the title and the Seen and Opened dates are missing, and it cannot " +
				"join the repository's stars, which sit under another measurement's path, onto " +
				"an item's row. The state is part of the path, so an item that was seen open and " +
				"then closed inside the range is a row for each.",
			ES: extES, ESTF: extEStf, ESOpts: Opts{"sort": "Seen"},
			ESDesc: "Elasticsearch reads each item's newest document in the range, over the " +
				"hundred repositories with the most documents, rather than the newest forty " +
				"items. It has no Opened column, that being the row's date less how long the " +
				"item was open, which a bucket cannot subtract, and it cannot join the " +
				"repository's stars, which are documents of another index, onto an item.",
		})
}

// starsGiven is the outbound direction of the section: what this account
// starred in other people's repositories, by language and by repository.
func starsGiven(b *builder) []Panel {
	// A star is a 1 added up per language, and not countOf, which added
	// every star into one series first: the grouping had one series to group
	// and drew one bar.
	langGR, langGRtf := gTbl(fmt.Sprintf(
		`limit(sortByMaxima(groupByNode(%s, %d, "sum")), 12)`,
		counted(gp("gh_star_given", "stars")), gn("gh_star_given", "language"),
	),
		"Language", []col{{"sum", overviewStarsGiven}})
	langES, langEStf := esTbl("gh_star_given", []any{b.tm("language", 12)}, []any{b.mCount()},
		[]named{{"language.keyword", "Language"}, {"n", overviewStarsGiven}}, nil)

	starGR, starGRtf := gTbl(rowsOf("keepLastValue("+gp("gh_star_given", "repo_stars")+")",
		gn("gh_star_given", "full_name")), "Repository", []col{{"lastNotNull", activityItsStars}})
	starES, starEStf := b.esRaw("gh_star_given", 25, []named{
		{panelESTime, "When"},
		{"full_name", "Repository"},
		{"language", "Language"},
		{"repo_stars", activityItsStars},
		{"url", "Link"},
	}, nil)
	return []Panel{
		panel("barchart", "Languages starred", box{W: 12, H: 8, X: 0, Y: 40}, []Target{sqlT(
			`SELECT language AS "Language", COUNT(*) AS "Stars given"` +
				" FROM gh_star_given WHERE $__timeFilter(time) AND language <> ''" +
				" GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 12",
		)}, &P{
			Prom: []Target{promTbl(
				"topk(12, sum by (language) (increase(github_stars_given_total[$__range])))",
			)},
			PromTF: []any{organize(map[string]string{
				"language": "Language", "Value": overviewStarsGiven,
			}, nil)},
			Desc: "The mirror of the stars received: what this account was reading, dated when " +
				"it starred it. The only measurement here about somebody else's work.",
			PromDesc: sinceStart,
			GR:       langGR, GRTF: langGRtf,
			ES: langES, ESTF: langEStf,
		}),
		panel("table", "Recently starred", box{W: 12, H: 8, X: 12, Y: 40}, []Target{sqlT(
			`SELECT full_name AS "Repository", time AS "When",` +
				` language AS "Language", repo_stars AS "Its stars",` +
				` url AS "Link"` +
				" FROM gh_star_given WHERE $__timeFilter(time)" +
				" ORDER BY time DESC, full_name LIMIT 25",
		)}, &P{
			Prom: []Target{promTbl("topk(25, github_stars_given_repo_stars_mean)")},
			PromTF: []any{organize(map[string]string{
				"language": "Language", "Value": activityItsStars,
			}, []string{"user"})},
			Desc: "Whether the account stars small projects or famous ones, which the language " +
				"breakdown cannot say.",
			PromDesc: "The exporter keeps the language of a star and not its repository, so in " +
				"Prometheus a row is a language the account starred in, and Its stars the mean " +
				"over the repositories starred in it, with no Repository or When column. " + lastSweep,
			Overrides: []any{
				when("When"), width("Language", 120), fullNameColumn(),
				barCell(activityItsStars, "short", 120), linkOn("Repository"),
			},
			GR: starGR, GRTF: starGRtf,
			GRDesc: grRows,
			ES:     starES, ESTF: starEStf,
		}),
	}
}
