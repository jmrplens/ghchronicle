package dashboards

// advisories is the security advisories in both directions: the reports sent
// to these repositories, and the ones this account sent to other people's.
//
// An advisory still in triage or a draft is a daily snapshot, the same row
// rewritten through the day and a new one each day, and a published or closed
// one is a row dated when that happened. So both tables read the newest row
// of each advisory inside the range, the way "Work elsewhere" reads an item,
// and order by that row's date, with the advisory's identity breaking the tie
// among the many open ones stamped at the same midnight.
//
// The summary of an advisory that is not published is written only when
// advisories.unpublished_details is on, so a row of one may read
// "(not published)" on purpose. Its page opens for its author and the repository's
// maintainers alone until it is published.
func advisories(b *builder) []Panel {
	return []Panel{
		advisoryTable(b, "Security advisories", "gh_repo_advisory", 60, true),
		advisoryTable(b, "Advisories reported", "gh_advisory_reported", 69, false),
	}
}

// advisoryTable is one of the two tables. received reads the account's own
// repositories through the repository picker and names the comments; the
// reported one is about other people's repositories, which no picker lists,
// and says what the account was credited as.
func advisoryTable(b *builder, title, m string, y int, received bool) Panel {
	where, extra, extraES, extraName := " AND "+RF, `x.comments AS "Comments"`, b.mMax("comments"), "Comments"
	esWhere := []string{ESF}
	if !received {
		where, extra, extraName, esWhere = "", `x.credit AS "Credit"`, "Credit", nil
	}
	// Opened is the row's time less seconds_open, to the second, as "Work
	// elsewhere" reads it: the row's own time is the start of the day for
	// one still open, and drawn as When, every report open that day read
	// 00:00. Published or closed is the row's time only where it is that.
	query := `SELECT x.full_name AS "Repository", x.ghsa AS "GHSA",` +
		` ` + agoSQL("x.time", "x.seconds_open") + ` AS "Opened",` +
		` CASE WHEN x.advisory_state IN ('published', 'withdrawn', 'closed') THEN x.time END AS "Published or closed",` +
		` x.advisory_state AS "State", x.severity AS "Severity", x.summary AS "Summary", ` +
		extra + `, x.url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, ghsa ORDER BY time DESC) AS rn" +
		" FROM " + m + " WHERE $__timeFilter(time)" + where + ") x" +
		" WHERE x.rn = 1 ORDER BY x.time DESC, x.full_name, x.ghsa LIMIT 40"

	buckets := []any{
		b.tm("full_name", 100), b.tm("ghsa", 200), b.newestDoc(),
		b.tmURL("advisory_state"), b.tmURL("severity"), b.tmURL("summary"), b.tmURL(),
	}
	names := []named{
		{panelFullNameField, "Repository"},
		{"ghsa.keyword", "GHSA"},
		{"advisory_state.keyword", "State"},
		{"severity.keyword", "Severity"},
		{"summary.keyword", "Summary"},
		{panelURLField, "Link"},
	}
	var metrics []any
	if received {
		metrics = append(metrics, extraES)
		names = append(names, named{"c", extraName})
	} else {
		buckets = append(buckets, b.tmURL("credit"))
		names = append(names, named{"credit.keyword", extraName})
	}
	esT, esTF := esTbl(m, buckets, metrics, names, esWhere, hideColumns(panelESTime))

	desc := "Every security advisory of these repositories: the reports others sent " +
		"through private vulnerability reporting, and the drafts and publications of " +
		"their maintainers. No other endpoint lists a report, and the ones closed " +
		"without publication are seen nowhere else. Opened is when it was reported or " +
		"drafted, and Published or closed when it was published, withdrawn or closed, " +
		"empty while it is in triage or a draft. The Summary of one not yet published reads (not published) unless " +
		"advisories.unpublished_details is on."
	if !received {
		desc = "The advisories this account reported, is credited in or collaborates on in " +
			"other people's repositories, found without a list: the inbox, the issues and " +
			"pull requests it took part in, the repositories it contributed to and the " +
			"reviewed global database each name repositories to ask. Credit is what the " +
			"advisory credits the account as. Opened and Published or closed read as in the " +
			"table above, and the Summary of one not yet published reads (not published) unless " +
			"advisories.unpublished_details is on."
	}
	return panel("table", title, box{W: 24, H: 9, X: 0, Y: y}, []Target{sqlT(query)}, &P{
		PromNote: cannot("each advisory, newest first, with its state, severity and summary and a "+
			"link to it.",
			"An advisory's rows are dated in the past or are a daily snapshot, and the "+
				"exporter keeps neither; its state and summary are strings besides."),
		GRNote: cannot("each advisory, newest first, with its state, severity and summary and a "+
			"link to it.",
			"Graphite keeps no strings, and an advisory is its state, its severity and its summary.",
			"graphite"),
		Desc: desc,
		Overrides: []any{
			fullNameColumn(), width("GHSA", 170), when("Opened"), unitOf("Published or closed", "time: YYYY-MM-DD HH:mm", 165),
			width("State", 90),
			width("Severity", 90), width(extraName, 110), ownerLinkOn("GHSA", "the advisory"),
		},
		ES: esT, ESTF: esTF,
		ESDesc: "Elasticsearch reads each advisory's newest document in the range, over the " +
			"hundred repositories with the most documents. It has no Opened column, that being " +
			"the row's date less how long the advisory has been open, which a bucket cannot " +
			"subtract, and no Published or closed column, which is the row's date for some " +
			"states and nothing for the rest.",
	})
}
