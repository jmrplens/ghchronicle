package dashboards

import "fmt"

// ── Security ────────────────────────────────────────────────────────────────

// security is what GitHub's own scanners have to say, in three parts: what is
// open right now, what the scans that found it did, and what is switched on to
// find any of it at all.
func security(b *builder) []Panel {
	return append(append(openAlerts(b), scanningAndResolution(b)...), posture(b)...)
}

// How the word columns below are drawn: the cell option that colors the text
// rather than the background, and the width each of those columns is pinned to.
const (
	securityCellOptions = "custom.cellOptions"
	securityColoredText = "color-text"
	securityCellWidth   = "custom.width"
)

// securityFrom is the FROM of a query whose measurement is a variable, which
// most of this section's are: the two alert families and the four posture
// snapshots are read by queries that differ in little else.
const securityFrom = " FROM "

// securitySetupBy keeps the identity of a code scanning setup on the
// Prometheus side: three queries group by the same four labels so the merge
// lines their columns up on one row per repository.
const securitySetupBy = "max by (full_name, repo, state, query_suite, schedule) "

// What this section calls its numbers and its columns wherever they appear.
// One value reaches a panel from five stores and each of them has to name it
// alike, and the overrides and units below match a field by that name.
const (
	securityOpenAlerts   = "Open alerts"
	securityCodeScanning = "Code scanning"
	securityResolveTime  = "Time to resolve"
	securityQuerySuite   = "Query suite"
	securityCanApprovePR = "Can approve pull requests"
	securityLastRotated  = "Last rotated"
	securityCVSS         = "CVSS"
	securityOutcome      = "Outcome"
)

// onOff renders a genuinely two-state boolean column as a word rather than as
// 1 and 0. SQL casts the flag, Prometheus and Graphite hold a boolean as 1 or
// 0, and an Elasticsearch metric over one reads 1 or 0 too. An Elasticsearch
// bucket on one does not: Grafana names a bucket that has another below it by
// the key's text, `true` or `false` (measured against Grafana 13.2.1 in the
// containerised suite), which is how Security features reads its flag there,
// and with the two numbers alone mapped the word was drawn bare. A boolean
// that is false for two different reasons is threeStates().
func onOff(name string, w int) any {
	return override(name, []any{
		map[string]any{"id": securityCellOptions, "value": map[string]any{"type": securityColoredText}},
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "value", "options": map[string]any{
				"0":     map[string]any{"text": "off", "color": "text", "index": 1},
				"1":     map[string]any{"text": "on", "color": "green", "index": 0},
				"false": map[string]any{"text": "off", "color": "text", "index": 3},
				"true":  map[string]any{"text": "on", "color": "green", "index": 2},
			},
		}}},
		map[string]any{"id": securityCellWidth, "value": w},
	})
}

// threeStates renders gh_security_setting, whose boolean is false for two
// different reasons.
//
// `disabled` and `unavailable` both reach every store as 0, so a column that
// spells 0 as `off` says the repository switched the feature off where GitHub
// only declined to say anything: it omits the whole security_and_analysis
// block on a private repository. Rendering the two alike is the confusion the
// measurement was collected to end, so the word for 0 is one that is true of
// both, and the colors go on `status`, which is the column that carries which
// of the two it is. Graphite has no such column, its status being a node of
// the row name, and an override matched byName against a field that is not
// there draws nothing.
func threeStates() []any {
	return []any{
		override("Status", []any{
			map[string]any{"id": securityCellOptions, "value": map[string]any{"type": securityColoredText}},
			map[string]any{"id": "mappings", "value": []any{map[string]any{
				"type": "value", "options": map[string]any{
					"enabled":     map[string]any{"text": "enabled", "color": "green", "index": 0},
					"disabled":    map[string]any{"text": "disabled", "color": "red", "index": 1},
					"unavailable": map[string]any{"text": "unavailable", "color": "text", "index": 2},
				},
			}}},
			map[string]any{"id": securityCellWidth, "value": 130},
		}),
		override("Enabled", []any{
			map[string]any{"id": securityCellOptions, "value": map[string]any{"type": securityColoredText}},
			map[string]any{"id": "mappings", "value": []any{map[string]any{
				"type": "value", "options": flagWords("no", "text"),
			}}},
			map[string]any{"id": securityCellWidth, "value": 100},
		}),
	}
}

// openAlerts is the state of the account as it stands: how many alerts are
// unfixed, split by severity, by ecosystem and over time, and which security
// features are switched on to find them at all.
func openAlerts(b *builder) []Panel {
	// The alert counts are a snapshot per repository, severity and ecosystem,
	// rewritten on every sweep. Summing them over the range counted each alert
	// once per sweep: the tile read 3.61K where the account had a few dozen.
	// One row per series, newest first, and then the sum. A series is a
	// repository by its full name, and the two breakdowns beside the tile
	// read the same series, so that their bars add up to it.
	dep := latestSumSQL("gh_dependabot_alert", "open", "severity", "ecosystem")
	scan := latestSumSQL("gh_code_scanning_alert", "open", "severity", "tool")
	bySev := `SELECT severity AS "Severity", SUM(open) AS "Open alerts" FROM (` +
		"SELECT severity, open, ROW_NUMBER() OVER (PARTITION BY full_name, severity, ecosystem" +
		" ORDER BY time DESC) AS rn FROM gh_dependabot_alert WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 GROUP BY 1 ORDER BY 2 DESC, 1"
	byEco := `SELECT ecosystem AS "Ecosystem", SUM(open) AS "Open alerts" FROM (` +
		"SELECT ecosystem, open, ROW_NUMBER() OVER (PARTITION BY full_name, severity, ecosystem" +
		" ORDER BY time DESC) AS rn FROM gh_dependabot_alert WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 GROUP BY 1 ORDER BY 2 DESC, 1"
	// The newest reading of each repository's feature, not the largest of the
	// range: MAX(enabled) read a feature switched off inside the range as on,
	// and MAX(open_alerts) an alert fixed inside it as still open, which is
	// the difference this table is here to show. By full name, since two
	// owners' repositories of one name are two rows with two answers.
	feats := `SELECT repo AS "Repository", feature AS "Feature",` +
		` CAST(enabled AS INT) AS "Enabled", open_alerts AS "Open alerts",` +
		` url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, feature ORDER BY time DESC) AS rn" +
		securityFrom + "gh_security_feature" + ciInRange + RF + deliveryNewestRow +
		" ORDER BY 1, 2, full_name"
	// A snapshot per repository, severity and ecosystem: the value of a
	// bucket is the newest row of each series inside it, and the severity's
	// line is those added up. A MAX per severity took the largest series
	// instead of the sum, and the curve ended at 4 under a tile saying 6.
	trend := "SELECT time, severity AS series, SUM(open) AS alerts FROM (" +
		"SELECT " + timeBin + ", severity, open, ROW_NUMBER() OVER (PARTITION BY" +
		" $__dateBin(time), full_name, severity, ecosystem ORDER BY time DESC) AS rn" +
		" FROM gh_dependabot_alert WHERE $__timeFilter(time) AND " + RF + ") x" +
		" WHERE rn = 1 GROUP BY 1, 2 ORDER BY 1"
	enabled := onOff("Enabled", 100)
	da, cs, sf := "gh_dependabot_alert", "gh_code_scanning_alert", "gh_security_feature"
	depOpen := rp(da, "open")

	// The alert counts are a snapshot per repository, severity and ecosystem;
	// the newest of each is read and then added up.
	byTag := func(tag, name string) (gr []Target, grtf []any, es []Target, estf []any) {
		gr, grtf = gTbl(fmt.Sprintf(`sortByMaxima(groupByNode(keepLastValue(%s), %d, "sum"))`,
			depOpen, gn(da, tag)), name, []col{{"lastNotNull", securityOpenAlerts}})
		other := "severity"
		if tag == "severity" {
			other = "ecosystem"
		}
		es, estf = esTbl(da, []any{b.tm(tag, 20), b.tm("full_name", 500), b.tm(other, 20)},
			[]any{b.mNewest("open")},
			[]named{{tag + ".keyword", name}, {"open", securityOpenAlerts}}, []string{ESF},
			groupSum(name, securityOpenAlerts, name, securityOpenAlerts)...)
		return gr, grtf, es, estf
	}

	sevGR, sevGRtf, sevES, sevEStf := byTag("severity", "Severity")
	ecoGR, ecoGRtf, ecoES, ecoEStf := byTag("ecosystem", "Ecosystem")

	// The last value of each series, and consolidated by the last value too:
	// a narrow panel asks for fewer points than the range holds, and the
	// average Graphite consolidates by otherwise drew the mean of an alert's
	// last two readings.
	featGR, featGRtf := gTbl(rowsOf(fmt.Sprintf(`consolidateBy(%s, "last")`, rp(sf, "open_alerts")),
		gn(sf, "repo"), gn(sf, "feature")),
		"Repository, feature", []col{{"lastNotNull", securityOpenAlerts}})
	// Each full name and feature, then its newest timestamp, which holds the
	// one document of the newest sweep: the flag and the count are read from
	// that, as the cache table reads its newest day. The flag stays a bucket,
	// since a top_metrics over a boolean hands the plugin a string.
	featES, featEStf := esTbl(sf, []any{
		b.tm("full_name", 500), b.tm("repo", 1), b.tm("feature", 5),
		b.terms(panelESTime, 1, "_key", "desc"),
		// A boolean is mapped as itself, with no keyword sub-field to ask for.
		b.terms("enabled", 2), b.tmURL(),
	}, []any{b.mMax("open_alerts")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"url.keyword", "Link"},
			{"feature.keyword", "Feature"},
			{"enabled", "Enabled"},
			{"o", securityOpenAlerts},
		}, []string{ESF}, hideColumns(panelFullNameField, panelESTime))

	return []Panel{
		statGroup(securityOpenAlerts, box{W: 12, H: 4, X: 0, Y: 0}, []Target{
			sqlT(namedValue(dep, "Dependabot")),
			{Kind: "sql", Format: "table", Ref: "B", SQL: namedValue(scan, securityCodeScanning)},
		}, &P{
			Prom: []Target{
				promAggregated("A", "Dependabot", fmt.Sprintf("sum(github_dependabot_alert_open{%s})", PF)),
				promAggregated("B", securityCodeScanning,
					fmt.Sprintf("sum(github_code_scanning_alert_open{%s})", PF)),
			},
			Desc: "Alerts still open, from the newest reading of each repository, which is " +
				"counted by its full name, so two owners' repositories of one name are two. " +
				"Both are colored by the same thresholds, so one value can be green beside a " +
				"red one.",
			GR: []Target{
				grNamed("A", "Dependabot", latestSum(depOpen)),
				grNamed("B", securityCodeScanning, latestSum(rp(cs, "open"))),
			},
			ES: append(
				esRefs("A", b.esLatestSum(da, "open", "severity", "ecosystem")),
				esRefs("B", b.esLatestSum(cs, "open", "severity", "tool"))...,
			),
			ESOver: []any{frameName("A", "Dependabot"), frameName("B", securityCodeScanning)},
			ESOpts: Opts{"calc": "sum"},
			ESDesc: esLeftOut("an alert count", esNewestAddedUp("alert")),
			Opts:   Opts{"thresholds": plainSteps},
			Overrides: []any{
				fieldThresholds("Dependabot", "short", alertThresholds),
				fieldThresholds(securityCodeScanning, "short", alertThresholds),
				// The collector writes a row per group of alerts that are open
				// and none for a group with none, so no row is none open.
				noValueOf("Dependabot", "none open"), noValueOf(securityCodeScanning, "none open"),
			},
		}),
		panel("barchart", "Alerts by severity", box{W: 6, H: 4, X: 12, Y: 0}, []Target{sqlT(bySev)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"sum by (severity) (github_dependabot_alert_open{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"severity": "Severity", "Value": securityOpenAlerts,
			}, nil)},
			GR: sevGR, GRTF: sevGRtf,
			ES: sevES, ESTF: sevEStf,
		}),
		panel("barchart", "Alerts by ecosystem", box{W: 6, H: 4, X: 18, Y: 0}, []Target{sqlT(byEco)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"sum by (ecosystem) (github_dependabot_alert_open{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"ecosystem": "Ecosystem", "Value": securityOpenAlerts,
			}, nil)},
			GR: ecoGR, GRTF: ecoGRtf,
			ES: ecoES, ESTF: ecoEStf,
		}),
		panel("timeseries", "Open alerts over time", box{W: 12, H: 8, X: 0, Y: 4}, []Target{sqlTS(trend)}, &P{
			Prom: []Target{promq(fmt.Sprintf(
				"sum by (severity) (github_dependabot_alert_open{%s})", PF,
			), legend("{{severity}}"))},
			Desc: "The open count per severity, added up across repositories and " +
				"ecosystems from the newest reading of each. A reading taken at each " +
				"sweep, so the curve starts the day the collector did. " + bucketFollowsRange,
			Opts:    mergeOpts(Opts{"stack": true}, dayBins),
			SQLOpts: seriesOpts,
			Overrides: []any{
				colorOf("critical", "red"), colorOf("high", "orange"),
				colorOf("medium", "yellow"), colorOf("low", "blue"),
			},
			// Each series reduced to its newest per bucket, then the group
			// summed: summing per series first is what keeps the sum from
			// counting one sweep twice.
			GR: []Target{grq(consolidated(fmt.Sprintf(`groupByNode(summarize(%s, %s, "last"), %d, "sum")`,
				depOpen, grBin("1d"), gn(da, "severity")), "max"))},
			ES: []Target{b.esDaily(da, b.mMax("open"), "severity", "", []string{ESF}, "")},
			ESDesc: "In Elasticsearch this is the largest single series of each severity " +
				"in the bucket rather than the sum across repositories: a date histogram " +
				"cannot take the newest of each series before adding them.",
		}),
		panel("table", "Security features", box{W: 12, H: 8, X: 12, Y: 4}, []Target{sqlT(feats)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (full_name, repo, feature) (github_security_feature_enabled{%s})", PF), "A"),
				promTbl(fmt.Sprintf("sum by (full_name, repo, feature) (github_security_feature_open_alerts{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "feature": "Feature", inventoryValueCol + "A": "Enabled",
				inventoryValueCol + "B": securityOpenAlerts,
			}, nil),

			Desc: "Recorded explicitly, so a repository with the feature switched off is " +
				"distinguishable from one with no alerts. Each row is the newest reading of " +
				"the repository's feature, so a feature switched off or an alert fixed inside " +
				"the range reads as it stands and not as it was at its highest.",
			Overrides: []any{enabled, width(securityOpenAlerts, 120), ownerLinkOn("Feature", "the security overview")},
			GR:        featGR, GRTF: featGRtf,
			GRDesc: "Graphite names each row repository and feature from the path and keeps one " +
				"number per row, the open alerts, so Enabled, whether the feature is on, is not " +
				"shown here.",
			ES: featES, ESTF: featEStf,
		}),
	}
}

// securityResolveDesc is what the Dependabot resolution table says about
// itself, out here rather than at its call site so that the function building
// the section stays inside the maintainability the linter holds it to.
const securityResolveDesc = "Dependabot alerts that were fixed or dismissed, newest " +
	"first, with the advisory each one was and how long it stayed open. Outcome is which " +
	"of the two it was, read from the alert's own state rather than from the reason it " +
	"was dismissed: an account that has only ever fixed its alerts has never written such " +
	"a reason, and a column these stores were never given does not exist to be selected. " +
	"The score is there because the word rounds it away: one alert in the measured " +
	"account scores 9.3 under a word whose mean is 6.1. It is the v4 score where the " +
	"advisory carries one."

// securityOldestDesc is what the table of open alerts says about itself, out
// here for the reason securityResolveDesc is.
const securityOldestDesc = "The alerts still open, oldest first, whatever the dashboard range: an alert " +
	"is listed while its row says it is open, however long ago it was raised. A " +
	"Dependabot alert names its package and advisory, a code scanning alert its " +
	"rule and tool. These are not always the rows the two counts at the top of the " +
	"section are made of: a sweep writes a row for each of the newest hundred " +
	"alerts of a repository in every state, and a repository with more has its " +
	"counts taken from a read of the open ones alone. There an open alert no sweep " +
	"has reached, behind a hundred newer ones since before the collector started, " +
	"is counted and not listed here, and an alert fixed while it sat behind the " +
	"newest hundred stays listed here as open, until a backfill reads the whole " +
	"list. Open for is counted from " +
	"the row's own date to now, so it is right when the panel is drawn rather than " +
	"when the last sweep ran. The " +
	"link opens the alert, which GitHub shows to the owner alone."

// scanningAndResolution is the other side of the same section: that the scans
// ran at all, what they returned, and how long an alert stayed open before it
// was fixed or dismissed.
func scanningAndResolution(b *builder) []Panel {
	// Eight tools and a remainder: with 57 repositories in the picker this
	// returned 14 series, and its legend hid 216 pixels of a 143 pixel plot
	// on a phone, which is the worst of the whole dashboard.
	analyses := topSeries("gh_code_scanning_analysis", "tool", "1", "analyses", RF)
	// One row per resolved alert rather than one per severity: the row says
	// which advisory it was, which a count by severity cannot. The score is
	// the v4 one where the advisory has it; `cvss` is absent on a v4-only
	// advisory, which is why the old MAX(cvss) read 0 for a whole bucket.
	// How the alert ended comes from `alert_state`, not from `dismissed_reason`.
	// A column of these stores exists once a point has carried it, and this
	// account has only ever fixed alerts, never dismissed one, so nothing had
	// ever written a reason: InfluxDB refused the whole statement with "Schema
	// error: No field named dismissed_reason" and the panel drew No data over
	// the eight resolved alerts it should have listed. COALESCE would not save
	// it either, the planner rejecting the name before a row is read. Every row
	// of this measurement carries `alert_state`, whatever the account has done
	// with its alerts, and fixed against dismissed is what the column was there
	// to say.
	resolve := `SELECT package AS "Package", time AS "Raised", repo AS "Repository",` +
		` severity AS "Severity", summary AS "Advisory",` +
		` COALESCE(cvss_v4, cvss) AS "` + securityCVSS + `", alert_state AS "` + securityOutcome + `",` +
		` seconds_to_resolve AS "Time to resolve", url AS "Link"` +
		" FROM gh_dependabot_alert_item WHERE $__timeFilter(time) AND " + RF +
		" AND seconds_to_resolve IS NOT NULL ORDER BY time DESC, full_name, number LIMIT 25"
	// Both alert families spell "no longer open" the same way. The state is a
	// field on the row, since it moves after the date the row carries, and
	// the exporter reads it back as a label.
	resolved := `alert_state!="open",` + PF
	csi := "gh_code_scanning_alert_item"
	scanResolve := `SELECT severity AS "Severity", COUNT(*) AS "Alerts",` +
		` median(CAST(seconds_to_resolve AS DOUBLE)) AS "Time to resolve"` +
		securityFrom + csi + ciInRange + RF +
		" AND seconds_to_resolve IS NOT NULL GROUP BY 1 ORDER BY 2 DESC, 1"
	an, di := "gh_code_scanning_analysis", "gh_dependabot_alert_item"
	// The alerts still open, oldest first, from both families in one list:
	// the two stats at the top count them and this is the one place that
	// names them. What is the package for a Dependabot alert and the rule
	// for a code scanning one. The row is dated when the alert was raised,
	// and an open alert is a state, not an event: bounded by the dashboard
	// range on that date the seven day view lost every alert older than a
	// week, which are the oldest ones the title promises. Whole history, as
	// the two stats above read it.
	//
	// How long it has been open is computed here and is no longer a field.
	// The collector used to write one, from the clock of the sweep that
	// wrote it, on to a row dated months earlier; this reads the row's own
	// date and is right at the moment the panel is drawn. date_part over an
	// interval is the one spelling both SQL stores take.
	openAlerts := " WHERE " + wholeHistory + " AND " + RF + " AND alert_state = 'open'"
	openFor := `date_part('epoch', now() - time) AS "Open for"`
	oldest := `SELECT package AS "What", time AS "Raised", 'dependabot' AS "Kind",` +
		` repo AS "Repository", severity AS "Severity", summary AS "Detail",` +
		" " + openFor + `, url AS "Link"` +
		securityFrom + di + openAlerts +
		" UNION ALL " +
		`SELECT rule AS "What", time AS "Raised", 'code scanning' AS "Kind",` +
		` repo AS "Repository", severity AS "Severity", tool AS "Detail",` +
		" " + openFor + `, url AS "Link"` +
		securityFrom + csi + openAlerts +
		" ORDER BY 2, 3, 4, 1, 8 LIMIT 25"

	scanResGR, scanResGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, %d, "avg")`,
		rp(csi, "seconds_to_resolve"), gn(csi, "severity")),
		"Severity", []col{{"count", "Alerts"}, {"median", securityResolveTime}})
	scanResES, scanResEStf := esTbl(csi, []any{b.tm("severity", 10)},
		[]any{b.mCount(), b.mPct("seconds_to_resolve", 50)},
		[]named{{"severity.keyword", "Severity"}, {"n", "Alerts"}, {"t", securityResolveTime}},
		[]string{ESF, "NOT alert_state:open"})

	resGR, resGRtf := gTbl(grGroupBy(rp(di, "seconds_to_resolve"), di, "avg", "repo", "severity", "package"),
		"Repository, severity, package", []col{{"count", "Alerts"}, {"median", securityResolveTime}})
	// The documents themselves: the advisory text is a string, and a
	// top_metrics over a string panics the plugin.
	resES, resEStf := b.esRaw(di, 25, []named{
		{"@timestamp", "Raised"},
		{"repo", "Repository"},
		{"severity", "Severity"},
		{"package", "Package"},
		{"summary", "Advisory"},
		{"cvss_v4", "CVSS v4"},
		{"cvss", securityCVSS},
		{"alert_state", securityOutcome},
		{"seconds_to_resolve", securityResolveTime},
		{"url", "Link"},
	}, []string{ESF, "_exists_:seconds_to_resolve"})

	// Buckets rather than the documents, so a fixture or an account whose
	// alerts carry no url still gets its rows: a document without the field
	// lands in the empty bucket, where a raw listing would have no column.
	// The advisory text is a string and stays out, as every ES table's do.
	// The date rather than the wait. How long an alert has been open is now()
	// less the row's own date, and a terms bucket cannot subtract; the date
	// itself it can, as the smallest @timestamp of the bucket, which for one
	// alert is the instant it was raised. That is what the panel's title is
	// about, and the shared override formats it as a date in every store.
	//
	// The order stays by document count. Ordering a terms bucket by a metric
	// requires the metric to be its own direct child, and this one sits four
	// buckets deeper, so Elasticsearch would refuse the order path outright.
	oldestES, oldestEStf := esTbl(di,
		append(b.tmRepo(50), b.tm("number", 25), b.tm("severity", 5), b.tm("package", 5), b.tmURL()),
		[]any{b.metric("min", "@timestamp", nil)},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"number.keyword", "Number"},
			{"severity.keyword", "Severity"},
			{"package.keyword", "What"},
			{"url.keyword", "Link"},
			{"m", "Raised"},
		}, []string{ESF, "alert_state:open"}, hideColumns(panelFullNameField))

	return []Panel{
		panel("timeseries", "Code scanning runs", box{W: 12, H: 8, X: 0, Y: 12}, []Target{sqlTS(analyses)}, &P{
			Prom: []Target{daily(promOtherOverTime(fmt.Sprintf(
				"sum by (tool) (increase(github_code_scanning_analyses_total{%s}[1d]))", PF,
			), "tool", "tool"), "{{tool}}")},
			Opts:    mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts: seriesOpts,
			Desc: "That the scan ran at all, which the alert list cannot tell you. GitHub " +
				"prunes these, so they only exist if they were captured. The eight tools " +
				"that ran the most in the range are named; the rest are `other`. " +
				bucketFollowsRange,
			PromDesc: sinceStart,
			GR:       []Target{grq(grOther(topSeriesKept, perBucket(nonNull(rp(an, "analyses")), gn(an, "tool"))))},
			ES:       []Target{b.esDaily(an, b.mCount(), "tool", "", []string{ESF}, "")},
			ESDesc:   esUnfolded(esDailyTerms, "tools", "series"),
		}),
		panel("table", "Scanning alerts resolved", box{W: 24, H: 7, X: 0, Y: 20},
			[]Target{sqlT(scanResolve)}, &P{
				Prom: []Target{
					promTbl(fmt.Sprintf("sum by (severity) (increase(github_code_scanning_alerts_total{%s}[$__range]))", resolved), "A"),
					promTbl(fmt.Sprintf("avg by (severity) (github_code_scanning_alerts_seconds_to_resolve_mean{%s})", resolved), "B"),
				},
				PromTF: merged(map[string]string{
					"severity": "Severity", inventoryValueCol + "A": "Alerts", inventoryValueCol + "B": securityResolveTime,
				}, nil),

				Opts: Opts{"sort": "Alerts"},
				Desc: "Code scanning alerts that were fixed or dismissed, and how long each " +
					"severity stayed open. The list was always downloaded whole and these dates " +
					"thrown away, so before this only the open count existed.",
				PromDesc:  sinceStart + " " + lastSweep,
				Overrides: []any{unitOf(securityResolveTime, "s", 190), barCell("Alerts", "short", 140)},
				GR:        scanResGR, GRTF: scanResGRtf, GRDesc: grSlot,
				ES: scanResES, ESTF: scanResEStf,
			}),
		panel("table", "Time to resolve an alert", box{W: 12, H: 8, X: 12, Y: 12}, []Target{sqlT(resolve)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (severity) (increase(github_dependabot_alerts_total{%s}[$__range]))", resolved), "A"),
				promTbl(fmt.Sprintf("max by (severity) (github_dependabot_alerts_cvss_mean{%s})", resolved), "B"),
				promTbl(fmt.Sprintf("avg by (severity) (github_dependabot_alerts_seconds_to_resolve_mean{%s})", resolved), "C"),
			},
			PromTF: merged(map[string]string{
				"severity": "Severity", inventoryValueCol + "A": "Alerts",
				inventoryValueCol + "B": "Worst CVSS", inventoryValueCol + "C": securityResolveTime,
			}, nil),

			// The count stands for the alerts the SQL stores list, and the
			// worst of the scores where they draw each one.
			PromAt: map[string]string{"Alerts": "Advisory", "Worst CVSS": securityCVSS},
			Opts:   Opts{"sort": "Raised"},
			Desc:   securityResolveDesc,
			PromDesc: "Prometheus keeps the severity only, so this is the alerts resolved per " +
				"severity, the worst mean score and the mean time, and no alert's Package, " +
				"Raised, Repository, Advisory, CVSS or Outcome survives the exporter. " +
				sinceStart + " " + lastSweep,
			Overrides: []any{
				when("Raised"), repoColumn(), width("Severity", 90),
				width("Package", 130), width(securityCVSS, 70), width(securityOutcome, 110),
				unitOf(securityResolveTime, "s", 130), ownerLinkOn("Package", "the alert"),
			},
			PromOver: []any{
				unitOf(securityResolveTime, "s", 170), barCell("Alerts", "short", 130),
				width("Worst CVSS", 120),
			},
			GR: resGR, GRTF: resGRtf, GRAt: map[string]string{"Alerts": "Advisory"},
			GRDesc: "Graphite names each row repository, severity and package from the path " +
				"and keeps no text, so the advisory is missing. " + grRows + " " + grSlot,
			ES: resES, ESTF: resEStf, ESDesc: esNewest,
		}),
		scanResultsByTool(b),
		panel("table", "Oldest open alerts", box{W: 24, H: 7, X: 0, Y: 34}, []Target{sqlT(oldest)}, &P{
			PromNote: cannot("the alerts still open, oldest first, from both families, with "+
				"the advisory or the rule and a link to each.",
				"The exporter reduces alerts to counts and means per repository and "+
					"severity; no alert survives, and the advisory and the url are strings."),
			GRNote: cannot("the alerts still open, oldest first, with a link to each.",
				"Graphite keeps no strings, and has no way to list by date.", "graphite"),
			Desc: securityOldestDesc,
			Overrides: []any{
				when("Raised"), width("Kind", 110), repoColumn(),
				width("Severity", 90), width("What", 160), unitOf("Open for", "s", 110),
				ownerLinkOn("What", "the alert"),
			},
			ES: oldestES, ESTF: oldestEStf,
			ESDesc: "Elasticsearch lists the Dependabot alerts alone, the two families " +
				"being two indices, so there is no Kind column, and the advisory is text, " +
				"which a bucket cannot show, so there is no Detail column. " +
				"It gives the date each alert was raised rather than Open for, how long it " +
				"has been open, that being the row's own date subtracted from now and not " +
				"something a bucket can compute, and it is not sorted by that date: ordering a bucket " +
				"by a metric needs the metric to be its own child, and this one is four " +
				"buckets deeper. It also lists only the " +
				"ones raised inside the dashboard range, since every Elasticsearch query " +
				"is bounded by it.",
		}),
	}
}

// posture is the question the two halves above do not ask: what is switched
// on, and what a workflow's own token may do with it.
//
// All four measurements are snapshots rewritten every sweep, three of them
// stamped at the start of the UTC day and gh_security_setting at `now`, so
// every panel here reads the newest row per series and none of them counts or
// adds anything over the range. Thirty days of a daily snapshot is thirty
// identical rows, which is how the open-alert tile above once read 3.61K.
//
// Elasticsearch cannot use a top_metrics for three of them: it panics on a
// boolean and shortens the frame on a number that is absent rather than zero.
// Those three take a max inside each series' newest document instead, which
// is the newest reading all the same (see newestDoc), and the fourth, whose
// numbers are always written, takes the newest with a top_metrics.
//
// Graphite reads the newest point of each series too, but a series there is
// the whole path, and the status, the state, suite and schedule, and the
// permissions are tags: a change inside the range leaves the old row beside
// the new one. Grouping those nodes away would keep one row and lose the words
// the row exists to show, so the three panels say it instead.
func posture(b *builder) []Panel {
	ss, csu := "gh_security_setting", "gh_code_scanning_setup"
	ap, sc := "gh_actions_policy", "gh_secret"

	settings := `SELECT repo AS "Repository", setting AS "Setting", status AS "Status",` +
		` CAST(enabled AS INT) AS "Enabled" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, setting ORDER BY time DESC) AS rn" +
		securityFrom + ss + ciInRange + RF + deliveryNewestRow +
		// The ones that are off first. Ordered by repository name the table
		// opened on whatever sorts first alphabetically, which on the account
		// this was measured against was an archived MATLAB repository, and
		// with 57 repositories in the picker it returns 285 rows and shows
		// six: the six the reader saw said nothing was wrong anywhere.
		" ORDER BY 4, 1, 2, full_name"
	setup := `SELECT repo AS "Repository", state AS "State", query_suite AS "Query suite",` +
		` schedule AS "Schedule", languages AS "Languages",` +
		` days_since_change AS "Last changed" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn" +
		securityFrom + csu + ciInRange + RF + deliveryNewestRow +
		" ORDER BY 1, full_name"
	policy := `SELECT repo AS "Repository", permissions AS "Permissions",` +
		` CAST(can_approve_pr AS INT) AS "Can approve pull requests" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn" +
		securityFrom + ap + ciInRange + RF + deliveryNewestRow +
		" ORDER BY 1, full_name"
	secrets := `SELECT secret AS "Secret", days_since_rotation AS "Last rotated",` +
		` age_days AS "Age", repo AS "Repository", kind AS "Kind" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name, kind, secret ORDER BY time DESC) AS rn" +
		securityFrom + sc + ciInRange + RF + deliveryNewestRow +
		" ORDER BY 2 DESC, 1, full_name, 5"

	// Graphite carries every tag as a path node, so the identity columns come
	// free; the number is the one column each table keeps.
	setGR, setGRtf := gTbl(rowsOf(rp(ss, "enabled"),
		gn(ss, "repo"), gn(ss, "setting"), gn(ss, "status")),
		"Repository, setting, status", []col{{"lastNotNull", "Enabled"}})
	csuGR, csuGRtf := gTbl(rowsOf(rp(csu, "days_since_change"),
		gn(csu, "repo"), gn(csu, "state"), gn(csu, "query_suite"), gn(csu, "schedule")),
		"Repository, state, query suite, schedule", []col{{"lastNotNull", deliveryLastChanged}})
	apGR, apGRtf := gTbl(rowsOf(rp(ap, "can_approve_pr"), gn(ap, "repo"), gn(ap, "permissions")),
		"Repository, permissions", []col{{"lastNotNull", securityCanApprovePR}})
	scGR, scGRtf := gTbl(rowsOf(rp(sc, "days_since_rotation"),
		gn(sc, "repo"), gn(sc, "kind"), gn(sc, "secret")),
		"Repository, kind, secret", []col{{"lastNotNull", securityLastRotated}})

	// `enabled` and `can_approve_pr` are booleans, and mNewest is what must not
	// read them: top_metrics hands a boolean back as the string "true", and the
	// Elasticsearch plugin panics on it with `interface {} is string, not
	// float64`, which is the bug gh_repo_community's has_* columns already met.
	// A max of a boolean is 1 or 0, which the word mapping reads, and taken
	// inside the newest document it is the newest reading. The status is below
	// that document, so a setting whose status changed inside the range is one
	// row, as it is in the SQL twin.
	setES, setEStf := esTbl(ss, append(b.tmRepo(500), b.tm("setting", 10), b.newestDoc(), b.tm("status", 5)),
		[]any{b.mMax("enabled")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"setting.keyword", "Setting"},
			{"status.keyword", "Status"},
			{"e", "Enabled"},
		}, []string{ESF}, hideColumns(panelFullNameField, panelESTime))
	// Both numbers are deliberately absent on some rows: GitHub sends no
	// change date for a setup it has never changed, and a repository that
	// answers 403 has neither that nor a language count. So they are asked for
	// with a max inside the newest document, as the deploy keys panel does: an
	// aggregation appends a null there and the row keeps its shape, where a
	// top_metrics appends nothing at all and the panel dies with `frame has
	// different field lengths`. The SQL twin reads one row per repository, so
	// the state, suite and schedule are of that document too.
	csuES, csuEStf := esTbl(csu, append(b.tmRepo(500),
		b.newestDoc(), b.tm("state", 5), b.tm("query_suite", 10), b.tm("schedule", 10),
	), []any{b.mMax("languages"), b.mMax("days_since_change")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"state.keyword", "State"},
			{"query_suite.keyword", securityQuerySuite},
			{"schedule.keyword", "Schedule"},
			{"l", "Languages"},
			{"d", deliveryLastChanged},
		}, []string{ESF}, hideColumns(panelFullNameField, panelESTime))
	apES, apEStf := esTbl(ap, append(b.tmRepo(500), b.newestDoc(), b.tm("permissions", 5)),
		[]any{b.mMax("can_approve_pr")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"permissions.keyword", "Permissions"},
			{"c", securityCanApprovePR},
		}, []string{ESF}, hideColumns(panelFullNameField, panelESTime))
	// The newest reading, and here with a top_metrics: `days_since_rotation`
	// climbs with the secret's age until somebody rotates the credential and
	// then restarts at zero, so a max over the range would hand back the value
	// from the day before the rotation, closing the gap between the two columns
	// and hiding the only rotation the panel exists to show. Both fields are
	// written on every point, so there is no absent value to shorten the frame.
	scES, scEStf := esTbl(sc, append(b.tmRepo(500), b.tm("kind", 5), b.tm("secret", 500)),
		[]any{b.mNewest("age_days", "days_since_rotation")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"kind.keyword", "Kind"},
			{"secret.keyword", "Secret"},
			{"a", "Age"},
			{"r", securityLastRotated},
		}, []string{ESF}, hideColumns(panelFullNameField))

	return []Panel{
		panel("table", "Security settings", box{W: 12, H: 12, X: 0, Y: 41}, []Target{sqlT(settings)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"max by (full_name, repo, setting, status) (github_security_setting_enabled{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "setting": "Setting", "status": "Status",
				"Value": "Enabled",
			}, nil)},
			Desc: "What the repository itself says about the five security_and_analysis keys, " +
				"which have three states and not two: `unavailable` is GitHub omitting the " +
				"whole block on a private repository, and that is not `disabled`. Read it " +
				"against the Security features panel above, where one is the repository's own " +
				"setting and the other is whether the listing answered at all: a row where " +
				"the two disagree is the row worth reading.",
			Overrides: append([]any{width("Setting", 200)}, threeStates()...),
			GR:        setGR, GRTF: setGRtf,
			GRDesc: "Graphite names each row repository, setting and status from the path, so a " +
				"setting whose status changed inside the range is two rows, the old status and " +
				"the new, each with its last reading.",
			ES: setES, ESTF: setEStf,
		}),
		panel("table", "Default code scanning setup", box{W: 12, H: 12, X: 12, Y: 41}, []Target{sqlT(setup)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf(securitySetupBy+
					"(github_code_scanning_setup_setups{%s})", PF), "A"),
				promTbl(fmt.Sprintf(securitySetupBy+
					"(github_code_scanning_setup_languages{%s})", PF), "B"),
				promTbl(fmt.Sprintf(securitySetupBy+
					"(github_code_scanning_setup_days_since_change{%s})", PF), "C"),
			},
			// The first query is asked for and then hidden: it is the one gauge
			// every repository has, so it is what keeps a repository whose setup
			// is unavailable on the screen at all, with the two columns it has
			// no reading for left empty, which is the answer the SQL stores give.
			PromTF: merged(map[string]string{
				"repo": "Repository", "state": "State", "query_suite": securityQuerySuite,
				"schedule": "Schedule", inventoryValueCol + "B": "Languages", inventoryValueCol + "C": deliveryLastChanged,
			}, []string{inventoryValueCol + "A"}),

			Desc: "GitHub's own default setup, and nothing else. A repository can answer " +
				"not-configured here and still run CodeQL from a workflow it wrote itself, " +
				"which the Code scanning runs panel above sees and this one does not. In " +
				"not-configured, `languages` is what GitHub detected in the repository, not " +
				"what anything analyzed.",
			Overrides: []any{
				width("State", 140), width(securityQuerySuite, 120), width("Schedule", 110),
				width("Languages", 110), unitOf(deliveryLastChanged, "d", 130),
			},
			GR: csuGR, GRTF: csuGRtf,
			GRDesc: grRows + " The number it keeps is `days_since_change`, which GitHub " +
				"gives only for a setup it has actually changed, so a repository that answers " +
				"not-configured or unavailable has no series here and no row at all: the other " +
				"four dashboards list it with the cell empty. The state, the query suite and " +
				"the schedule are part of the path, so a setup that changed one of them inside " +
				"the range is two rows, the old and the new.",
			ES: csuES, ESTF: csuEStf,
		}),
		panel("table", "Workflow token permissions", box{W: 12, H: 7, X: 0, Y: 53}, []Target{sqlT(policy)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"max by (full_name, repo, permissions) (github_actions_policy_can_approve_pr{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "permissions": "Permissions",
				"Value": securityCanApprovePR,
			}, nil)},
			Desc: "The default permissions of GITHUB_TOKEN, which is what a compromised action " +
				"inherits. `write` beside can-approve-pull-requests on is the supply-chain " +
				"row: that workflow can push a change and approve it. `read` means a workflow " +
				"cannot push at all.",
			Overrides: []any{
				width("Permissions", 130), onOff(securityCanApprovePR, 220),
			},
			GR: apGR, GRTF: apGRtf,
			GRDesc: "Graphite names each row repository and permissions from the path, so " +
				"permissions changed inside the range are two rows, the old and the new.",
			ES: apES, ESTF: apEStf,
		}),
		panel("table", "Secret rotation", box{W: 12, H: 7, X: 12, Y: 53},
			[]Target{sqlT(secrets)}, &P{
				Prom: []Target{
					promTbl(fmt.Sprintf("max by (full_name, repo, kind, secret) (github_secret_age_days{%s})", PF), "A"),
					promTbl(fmt.Sprintf("max by (full_name, repo, kind, secret) (github_secret_days_since_rotation{%s})", PF), "B"),
				},
				PromTF: merged(map[string]string{
					"repo": "Repository", "kind": "Kind", "secret": "Secret",
					inventoryValueCol + "A": "Age", inventoryValueCol + "B": securityLastRotated,
				}, nil),

				Opts: Opts{"sort": securityLastRotated},
				Desc: "Secrets, and when they were last rotated. Both numbers or the panel " +
					"says nothing: days_since_rotation equals " +
					"age_days until somebody rotates the secret, so the gap between the two " +
					"columns is the only evidence a credential was ever replaced. The names " +
					"are public already, written in the workflow that reads them; the values " +
					"never leave GitHub.",
				Overrides: []any{
					width("Kind", 110), width("Secret", 160),
					unitOf("Age", "d", 100), unitOf(securityLastRotated, "d", 120),
				},
				GR: scGR, GRTF: scGRtf, GRDesc: grRows,
				ES: scES, ESTF: scEStf,
			}),
	}
}

// scanResultsByTool is what each scanning tool found in each repository, the
// results that explain a jump in the alert count.
func scanResultsByTool(b *builder) Panel {
	an := "gh_code_scanning_analysis"
	toolGR, toolGRtf := gTbl(fmt.Sprintf(`limit(sortBy(%s, "sum", true), 25)`,
		grGroupBy(rp(an, "results"), an, "sum", "tool", "repo"),
	),
		"Tool, repository", []col{{"sum", "Results"}})
	toolES, toolEStf := esTbl(an, append([]any{b.tm("tool", 10)}, b.tmRepo(50)...),
		[]any{b.mCount(), b.mSum("results"), b.mMax("rules")},
		[]named{
			{"tool.keyword", "Tool"},
			{inventoryRepoTerm, "Repository"},
			{"n", "Runs"},
			{"r", "Results"},
			{"u", "Rules"},
		}, []string{ESF}, hideColumns(panelFullNameField))

	return panel("table", "Scan results by tool", box{W: 24, H: 7, X: 0, Y: 27}, []Target{sqlT(
		`SELECT tool AS "Tool", SUM(results) AS "Results", repo AS "Repository",` +
			` COUNT(*) AS "Runs", MAX(rules) AS "Rules"` +
			" FROM gh_code_scanning_analysis WHERE $__timeFilter(time) AND " + RF +
			" GROUP BY 1, full_name, 3 ORDER BY 2 DESC, 1, full_name LIMIT 25",
	)}, &P{
		Prom: []Target{
			promTbl(fmt.Sprintf("sum by (tool, full_name, repo) (increase(github_code_scanning_analyses_total{%s}[$__range]))", PF), "A"),
			promTbl(fmt.Sprintf("avg by (tool, full_name, repo) (github_code_scanning_analyses_results_mean{%s})", PF), "B"),
			promTbl(fmt.Sprintf("max by (tool, full_name, repo) (github_code_scanning_analyses_rules_mean{%s})", PF), "C"),
		},
		PromTF: merged(map[string]string{
			"tool": "Tool", "repo": "Repository", inventoryValueCol + "A": "Runs", inventoryValueCol + "B": "Results",
			inventoryValueCol + "C": "Rules",
		}, nil),

		Opts: Opts{"sort": "Results"},
		Desc: "The panel beside this one says the scan ran. This says what it found, which " +
			"is what explains a jump in the alert count: one tool here returns sixty " +
			"three results in three runs and another fifty two in nine hundred and " +
			"fifty three.",
		PromDesc: sinceStart + " " + lastSweep,
		Overrides: []any{
			barCell("Results", "short", 120), width("Runs", 100),
			width("Rules", 100),
		},
		GR: toolGR, GRTF: toolGRtf, GRDesc: grRows + " " + grSlotTotals,
		ES: toolES, ESTF: toolEStf,
	})
}
