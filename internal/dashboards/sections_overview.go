package dashboards

import (
	"fmt"
	"maps"
	"strings"
)

// overviewNewestRow ends the queries over gh_account: the table is rewritten
// whole every sweep, so the account as it stands is the last row of it, never
// a sum over the range. The titles beside it are what the Elasticsearch,
// Graphite and Prometheus twins rename their own columns to.
const (
	overviewNewestRow      = " WHERE $__timeFilter(time) ORDER BY time DESC LIMIT 1"
	overviewAccountAge     = "Account age"
	overviewStarsGiven     = "Stars given"
	overviewUniqueVisitors = "Unique visitors"
	overviewUniqueCloners  = "Unique cloners"
)

// ── Overview ────────────────────────────────────────────────────────────────

// A stat panel of several numbers. On a desktop Grafana lays the values out
// in one row and the panel reads as the row of tiles it replaces; on a phone,
// where every panel is one column, one tile per number was 144 pixels each
// and the Overview alone was four screens of single numbers. Grouped, each
// panel is one screen-width block of a few values and the section is one
// screen. Every value is its own query in every store, named in the query
// language that store has for it: a column alias in SQL, a legend in
// Prometheus, alias() in Graphite, and for Elasticsearch a rename of the
// column its response parser makes, or an override on the query's refId
// where two queries would make the same column name.
func statGroup(title string, at box, sql []Target, p *P) Panel {
	opts := Opts{"text_mode": "value_and_name"}
	maps.Copy(opts, p.Opts)
	p.Opts = opts
	return panel("stat", title, at, sql, p)
}

// promNamed is one Prometheus instant query drawn under its own name.
func promNamed(ref, name, expr string) Target {
	return promq(expr, instant(), legend(name), withRef(ref))
}

// grNamed is one Graphite series drawn under its own name.
func grNamed(ref, name, expr string) Target {
	return grq(fmt.Sprintf("alias(%s, %q)", expr, name), ref)
}

// frameName names every field a query returns, for the stores whose response
// parser gives two queries the same column name.
func frameName(ref, name string) any {
	return map[string]any{
		"matcher":    map[string]any{"id": "byFrameRefID", "options": ref},
		"properties": []any{map[string]any{"id": "displayName", "value": name}},
	}
}

// fieldGroup is a group of fields of one account-wide snapshot as one stat
// panel: one SQL statement with a column per field, one Prometheus query per
// field on the exporter's gauge of it, one Graphite alias per field, and one
// Elasticsearch top_metrics over them all, whose columns are renamed the way
// esTbl names them. The newest row is the value: the measurement is rewritten
// every sweep and a range is never summed.
func fieldGroup(b *builder, m, title string, at box, fields []named, p *P) Panel {
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = fmt.Sprintf("%s AS %q", f.From, f.To)
		p.Prom = append(p.Prom, promNamed(ref(i), f.To, "github_"+strings.TrimPrefix(m, "gh_")+"_"+f.From))
		p.GR = append(p.GR, grNamed(ref(i), f.To, fmt.Sprintf("keepLastValue(%s)", gp(m, f.From))))
	}
	p.ES, p.ESTF = esTbl(m, []any{b.one()}, []any{b.mNewest(fieldsOf(fields)...)}, fields, nil)
	return statGroup(title, at, []Target{sqlT(fmt.Sprintf(
		"SELECT %s FROM %s WHERE $__timeFilter(time) ORDER BY time DESC LIMIT 1",
		strings.Join(cols, ", "), m,
	))}, p)
}

func fieldsOf(fields []named) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f.From
	}
	return out
}

// ref is the refId of the i-th query of a panel: A, B, C and so on. No panel
// asks for more values than the alphabet has, and one that did would have
// stopped being readable long before.
func ref(i int) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	return letters[i : i+1]
}

func overview(b *builder) []Panel {
	// The three traffic values are the one place two dashboards showed two
	// numbers under one word: 1.06K views over seven days here, 2.25K in
	// Prometheus, which holds GitHub's whole fourteen day window as one gauge.
	// So the title names the range on the stores that sum rows, and the
	// window on the one that cannot, and all carry the same sentence.
	trafficDesc := "Page views, unique visitors and the people who cloned, summed over " +
		"the dashboard range, so they move with the range picker. GitHub only keeps 14 " +
		"days of traffic, so a range wider than that shows what was captured while it " +
		"was still there. Cloners rather than clones because a clone is counted per " +
		"pull and continuous integration pulls all day: on the account this was measured " +
		"against one repository was cloned 135,683 times in a fortnight by 1,807 cloners, " +
		"and 186 K beside 2.89 K views read as an audience. The clone count itself is two " +
		"panels of the Audience section, \"Clones over time\" and \"Clone amplification\", " +
		"where it is the subject rather than a headline."
	trafficSQL := func(kind, field string) string {
		return fmt.Sprintf("SUM(CASE WHEN kind = '%s' THEN %s ELSE 0 END)", kind, field)
	}
	trafficProm := func(ref, name, kind, field string) Target {
		return promNamed(ref, name, fmt.Sprintf(`sum(github_traffic_%s{kind=%q,%s})`, field, kind, PF))
	}
	trafficGR := func(ref, name, kind, field string) Target {
		return grNamed(ref, name, total(fmt.Sprintf("sumSeries(%s)", rp("gh_traffic", field, "kind", kind))))
	}
	trafficES := func(ref, kind, field string) Target {
		t := b.esTotal("gh_traffic", b.mSum(field), "kind:"+kind, ESF)[0]
		t.Ref = ref
		return t
	}

	// Stars and forks add up the newest row per repository; the repository
	// count is the account's own. In Elasticsearch the per-repository rows
	// are summed by the panel, and the one account row sums to itself.
	//
	// A live repository's row is its gh_repo and an archived one's its
	// gh_repo_total. A repository the filter sets aside for being archived
	// gets no gh_repo row from a sweep, but it gets a gh_repo_total from every
	// totals sweep, and people still star and fork it. Measured on 2026-09-26
	// against the account of issue #78: the seventeen archived repositories
	// its filter sets aside held 80 stars and 24 forks that the tile left out.
	// Not gh_repo_total for every repository, though: that family runs every
	// twelve hours and gh_repo every hour, so the live stars would lag the
	// Inventory table and the card by up to half a day and a range of six
	// hours would hold none of them. Under All every store lets the archived
	// rows through, the SQL ones by RFA and the rest by a wildcard; with
	// repositories picked, only those count.
	//
	// One row per full_name and not per repo, which is a name two owners can
	// both use: alice/.github and acme/.github are two repositories with stars
	// of their own. The SQL stores and Elasticsearch keep the newest row of
	// each. Prometheus and Graphite cannot say which of two series is newer,
	// so they keep the larger, and that is what stops a repository archived
	// while the collector runs from counting twice, once under the live
	// series it had, which keepLastValue holds up until it leaves the range
	// and the OTLP sink until the process restarts, and once under the
	// archived one.
	rt := "gh_repo_total"
	reposES, reposEStf := esTbl(rt, []any{b.tm("full_name", 500)},
		[]any{b.mNewest("stars", "forks")},
		[]named{{"stars", "Stars"}, {"forks", "Forks"}}, nil)
	reposES[0].Query = liveOrArchivedES()
	oneEach := func(field string) string {
		return fmt.Sprintf(`sum(max by (full_name) (github_repo_%[1]s{archived="false",%[2]s}`+
			` or github_repo_total_%[1]s{archived="true",%[2]s}))`, field, PF)
	}
	oneEachGR := func(field string) string {
		named := func(m, archived string) string {
			return fmt.Sprintf("aliasByNode(keepLastValue(%s), %d)",
				rp(m, field, "archived", archived), gn(m, "full_name"))
		}
		return fmt.Sprintf(`sumSeries(groupByNode(group(%s, %s), 0, "max"))`,
			named("gh_repo", "false"), named(rt, "true"))
	}
	countES, countEStf := esTbl("gh_account", []any{b.one()}, []any{b.mNewest("public_repos")},
		[]named{{"public_repos", "Repositories"}}, nil)
	reposES[0].Ref = "B"
	// The Account group reads two measurements, so two queries; each organize
	// renames the columns of the frame that has them and leaves the other's
	// alone.
	contribES, contribEStf := esTbl("gh_contributions_total", []any{b.one()},
		[]any{b.mNewest("calendar_total")}, []named{{"calendar_total", "Contributions"}}, nil)
	acctES, acctEStf := esTbl("gh_account", []any{b.one()},
		[]any{b.mNewest("account_age_days", "watching", "starred", "gists", "packages")},
		[]named{
			{"account_age_days", overviewAccountAge},
			{"watching", "Watching"},
			{"starred", overviewStarsGiven},
			{"gists", "Gists"},
			{"packages", "Packages"},
		}, nil)
	acctES[0].Ref = "B"

	return []Panel{
		brandPanel(),
		statGroup("Repositories", box{W: 8, H: 4, X: 0, Y: brandHeight}, []Target{
			sqlT(`SELECT public_repos AS "Repositories" FROM gh_account` +
				overviewNewestRow),
			{Kind: "sql", Format: "table", Ref: "B", SQL: `SELECT SUM(stars) AS "Stars",` +
				` SUM(forks) AS "Forks" FROM (` +
				liveOrArchived([]string{"stars", "forks"}) + ")"},
		}, &P{
			Prom: []Target{
				promNamed("A", "Repositories", "github_account_public_repos"),
				promNamed("B", "Stars", oneEach("stars")),
				promNamed("C", "Forks", oneEach("forks")),
			},
			Desc: "Public repositories of the account, and the current stars and forks of " +
				"the selected ones, summed over the newest row of each repository rather " +
				"than over every row in the range. With All selected that includes the " +
				"archived repositories the default filter sets aside, which the picker " +
				"stops listing once the last backfill is behind it: people still star and " +
				"fork them, and each totals sweep reads their counts again, twelve hours " +
				"apart by default, so a range shorter than that can leave them out. Each " +
				"repository counts once, by its full name. The repository count is GitHub's " +
				"own count of the account's public repositories and not the set the sums " +
				"are taken over: those are the repositories the sweeps collect and the " +
				"archived ones above, private ones included, and forks left out unless " +
				"`include_forks` is on.",
			GR: []Target{
				grNamed("A", "Repositories", gp("gh_account", "public_repos")),
				grNamed("B", "Stars", oneEachGR("stars")),
				grNamed("C", "Forks", oneEachGR("forks")),
			},
			ES: append(countES, reposES...), ESTF: append(countEStf, reposEStf...),
			ESOpts: Opts{"calc": "sum"},
		}),
		statGroup("Traffic in range", box{W: 8, H: 4, X: 8, Y: brandHeight}, []Target{sqlT(
			"SELECT " + trafficSQL("views", "count") + ` AS "Views", ` +
				trafficSQL("views", "uniques") + ` AS "` + overviewUniqueVisitors + `", ` +
				trafficSQL("clones", "uniques") + ` AS "` + overviewUniqueCloners + `"` +
				" FROM gh_traffic WHERE $__timeFilter(time) AND " + RF,
		)}, &P{
			Prom: []Target{
				trafficProm("A", "Views", "views", "count"),
				trafficProm("B", overviewUniqueVisitors, "views", "uniques"),
				trafficProm("C", overviewUniqueCloners, "clones", "uniques"),
			},
			PromTitle: "Traffic, 14-day window",
			Desc:      trafficDesc,
			PromDesc:  windowNote,
			GR: []Target{
				trafficGR("A", "Views", "views", "count"),
				trafficGR("B", overviewUniqueVisitors, "views", "uniques"),
				trafficGR("C", overviewUniqueCloners, "clones", "uniques"),
			},
			ES: []Target{
				trafficES("A", "views", "count"),
				trafficES("B", "views", "uniques"),
				trafficES("C", "clones", "uniques"),
			},
			ESOver: []any{
				frameName("A", "Views"), frameName("B", overviewUniqueVisitors),
				frameName("C", overviewUniqueCloners),
			},
		}),
		fieldGroup(b, "gh_account", "Community", box{W: 8, H: 4, X: 16, Y: brandHeight}, []named{
			{"followers", "Followers"},
			{"following", "Following"},
			{"sponsors", "Sponsors"},
			{"sponsoring", "Sponsoring"},
		}, &P{
			Desc: "Followers of the account and the accounts it follows, as of the last " +
				"sweep; the two are different numbers and the second is usually much " +
				"smaller. Then who sponsors the account and whom it sponsors.",
		}),
		statGroup("Account", box{W: 24, H: 5, X: 0, Y: brandHeight + 4}, []Target{
			sqlT(`SELECT calendar_total AS "Contributions" FROM gh_contributions_total` +
				overviewNewestRow),
			{Kind: "sql", Format: "table", Ref: "B", SQL: `SELECT account_age_days AS "Account age",` +
				` watching AS "Watching", starred AS "Stars given", gists AS "Gists",` +
				` packages AS "Packages" FROM gh_account` +
				overviewNewestRow},
		}, &P{
			Prom: []Target{
				promNamed("A", "Contributions", "github_contributions_total_calendar_total"),
				promNamed("B", overviewAccountAge, "github_account_account_age_days"),
				promNamed("C", "Watching", "github_account_watching"),
				promNamed("D", overviewStarsGiven, "github_account_starred"),
				promNamed("E", "Gists", "github_account_gists"),
				promNamed("F", "Packages", "github_account_packages"),
			},
			Desc: "Contributions in the last year, the number behind the green squares on " +
				"the profile; the days since the account was created; what it watches, " +
				"what it has starred, its gists and its packages. Packages are counted " +
				"from the package listing, the same rows the Packages table under " +
				"Inventory lists: GraphQL reports zero packages for a personal account " +
				"and is not what this reads.",
			GR: []Target{
				grNamed("A", "Contributions", gp("gh_contributions_total", "calendar_total")),
				grNamed("B", overviewAccountAge, gp("gh_account", "account_age_days")),
				grNamed("C", "Watching", gp("gh_account", "watching")),
				grNamed("D", overviewStarsGiven, gp("gh_account", "starred")),
				grNamed("E", "Gists", gp("gh_account", "gists")),
				grNamed("F", "Packages", gp("gh_account", "packages")),
			},
			ES: append(contribES, acctES...), ESTF: append(contribEStf, acctEStf...),
			Overrides: []any{unitOf(overviewAccountAge, "d", 0)},
		}),
	}
}

// ── Audience ────────────────────────────────────────────────────────────────

func audience(b *builder) []Panel {
	// The eight busiest repositories of the range and the rest as `other`:
	// a series per repository put the busiest one, phonometry, under the
	// panel's edge behind thirty legend entries. topSeries drops the days a
	// repository had nothing, so a stacked bar of zeros is not a legend entry.
	perDay := func(field, kind, alias string) string {
		return topSeries("gh_traffic", "repo", field, alias, "kind = '"+kind+"' AND "+RF)
	}
	window := func(field, kind, title string, at box, desc string) Panel {
		alias := lowerFirstWord(title)
		return panel("timeseries", title, at,
			[]Target{sqlTS(perDay(field, kind, alias))}, &P{
				Prom: []Target{promq(fmt.Sprintf(`sum by (repo) (github_traffic_%s{kind=%q,%s})`,
					field, kind, PF), legend("{{repo}}"))},
				Desc: desc + " The eight repositories with the most in the range are named; " +
					"the rest are `other`. " + bucketFollowsRange, PromDesc: windowNote,
				Opts:     mergeOpts(Opts{"stack": true, "bars": true}, dayBins),
				SQLOpts:  seriesOpts,
				PromOpts: Opts{"bars": false},
				GR: []Target{grq(perBucket(rp("gh_traffic", field, "kind", kind),
					gn("gh_traffic", "repo")))},
				ES: []Target{b.esDaily("gh_traffic", b.mSum(field), "repo", "",
					[]string{"kind:" + kind, ESF}, "")},
			})
	}

	// Referrers and paths are a snapshot of the same fourteen days, rewritten
	// daily. Adding the days together would report the window once per day it
	// was captured, so the newest snapshot per repository is taken first and
	// only then summed across repositories.
	// referrer_url is the host as a page, the same for every repository it
	// sent visitors to; a search engine GitHub names without a host has none
	// and its cell stays empty.
	refs := `SELECT referrer AS "Referrer", SUM(count) AS "Views",` +
		` SUM(uniques) AS "Unique", MAX(referrer_url) AS "Link" FROM (` +
		"SELECT repo, referrer, count, uniques, referrer_url, ROW_NUMBER() OVER (" +
		"PARTITION BY repo, referrer ORDER BY time DESC) AS rn" +
		" FROM gh_traffic_referrer WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 GROUP BY 1 ORDER BY 2 DESC LIMIT 25"
	// The column the table is sorted by comes second, so it is on the screen
	// beside the identifier on a phone; the title, which can be long, after.
	paths := `SELECT path AS "Path", SUM(count) AS "Views", SUM(uniques) AS "Unique",` +
		` MAX(title) AS "Title", url AS "Link" FROM (` +
		"SELECT repo, path, title, count, uniques, url, ROW_NUMBER() OVER (" +
		"PARTITION BY repo, path ORDER BY time DESC) AS rn" +
		" FROM gh_traffic_path WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 GROUP BY 1, 5 ORDER BY 2 DESC LIMIT 25"

	refsGR, refsGRtf := gTbl(fmt.Sprintf(`limit(sortByMaxima(groupByNode(%s, %d, "sum")), 25)`,
		rp("gh_traffic_referrer", "count"), gn("gh_traffic_referrer", "referrer")),
		"Referrer", []col{{"lastNotNull", "Views"}})
	refsES, refsEStf := esTbl("gh_traffic_referrer",
		[]any{b.tm("referrer", 25), b.tm("repo", 50), b.tmURL("referrer_url")}, []any{b.mNewest("count", "uniques")},
		[]named{
			{"referrer.keyword", "Referrer"},
			{panelRepoField, "Repository"},
			{"referrer_url.keyword", "Link"},
			{"count", "Views"},
			{"uniques", "Unique"},
		}, []string{ESF})

	pathsGR, pathsGRtf := gTbl(fmt.Sprintf(`limit(sortByMaxima(groupByNode(%s, %d, "sum")), 25)`,
		rp("gh_traffic_path", "count"), gn("gh_traffic_path", "path")),
		"Path", []col{{"lastNotNull", "Views"}})
	pathsES, pathsEStf := esTbl("gh_traffic_path",
		[]any{b.tm("path", 25), b.tm("repo", 50), b.tm("title", 1), b.tmURL()}, []any{b.mNewest("count", "uniques")},
		[]named{
			{"path.keyword", "Path"},
			{panelRepoField, "Repository"},
			{"title.keyword", "Title"},
			{"url.keyword", "Link"},
			{"count", "Views"},
			{"uniques", "Unique"},
		},
		[]string{ESF})

	cloneGR, cloneGRtf := gTbl(rowsOf(fmt.Sprintf("sumSeries(%s)",
		rp("gh_traffic", "count", "kind", "clones")), gn("gh_traffic", "repo")),
		"Repository", []col{{"sum", "Clones"}})
	cloneES, cloneEStf := esTbl("gh_traffic", []any{b.tm("repo", 500), b.tmURL()},
		[]any{b.mSum("count"), b.mSum("uniques")},
		[]named{{panelRepoField, "Repository"}, {"url.keyword", "Link"}, {"c", "Clones"}, {"u", "Cloners"}},
		[]string{ESF, "kind:clones"})

	return []Panel{
		window("count", "views", "Views over time", box{W: 12, H: 8, X: 0, Y: 0},
			"Split by repository. GitHub serves a rolling 14-day window and it is "+
				"rewritten on every sweep, so a gap means the collector was down for "+
				"longer than that window."),
		window("uniques", "views", "Unique visitors over time", box{W: 12, H: 8, X: 12, Y: 0},
			"Split by repository."),
		window("count", "clones", "Clones over time", box{W: 12, H: 8, X: 0, Y: 8},
			"Its own panel because continuous integration clones a repository "+
				"thousands of times for every human visit, and on a shared axis the "+
				"visits become a flat line at zero."),
		panel("table", "Top referrers", box{W: 12, H: 8, X: 12, Y: 8}, []Target{sqlT(refs)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (referrer) (github_traffic_referrer_count{%s})", PF), "A"),
				promTbl(fmt.Sprintf("sum by (referrer) (github_traffic_referrer_uniques{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"referrer": "Referrer", panelValueA: "Views", panelValueB: "Unique",
			}, nil, nil),
			Opts:      Opts{"sort": "Views"},
			Overrides: []any{barCell("Views", "short", 120), width("Unique", 90), rowLinkOn("Referrer", "Open the referrer")},
			Desc: "Where the visitors came from over GitHub's trailing fourteen days. GitHub " +
				"aggregates by host, not by URL, and attaches no dates, so this is the most " +
				"recent snapshot rather than a sum over the selected range. The link opens " +
				"the host; a search engine GitHub names without one has no link.",
			GR: refsGR, GRTF: refsGRtf, GRDesc: grRows,
			ES: refsES, ESTF: refsEStf, ESDesc: esPerRepo,
		}),
		panel("table", "Top paths", box{W: 24, H: 8, X: 0, Y: 16}, []Target{sqlT(paths)}, &P{
			PromNote: cannot(
				"the most visited paths of the trailing fourteen days, with their titles.",
				"The exporter skips `gh_traffic_path`: it is a dated top-ten that changes "+
					"every day, and as gauges it would be a series per path ever visited.",
			),
			Opts: Opts{"sort": "Views"},
			Overrides: []any{
				width("Path", 200), barCell("Views", "short", 120),
				width("Unique", 90), linkOn("Path"),
			},
			GR: pathsGR, GRTF: pathsGRtf,
			GRDesc: "Graphite keeps no text, so there is no title. " + grRows,
			ES:     pathsES, ESTF: pathsEStf, ESDesc: esPerRepo,
		}),
		panel("table", "Clone amplification", box{W: 24, H: 8, X: 0, Y: 24}, []Target{sqlT(
			`SELECT repo AS "Repository",` +
				// The 1.0 is the ratio itself. count and uniques are integer
				// columns in InfluxDB, and DataFusion answers an integer
				// division by truncating: 817 clones over 205 cloners drew 3
				// where PostgreSQL, whose columns are double precision, drew
				// 3.99. This is the one panel about a ratio.
				` SUM(CASE WHEN kind = 'clones' THEN count ELSE 0 END) * 1.0` +
				` / NULLIF(SUM(CASE WHEN kind = 'clones' THEN uniques ELSE 0 END), 0) AS "Clones each",` +
				` SUM(CASE WHEN kind = 'clones' THEN count ELSE 0 END) AS "Clones",` +
				` SUM(CASE WHEN kind = 'clones' THEN uniques ELSE 0 END) AS "Cloners",` +
				` SUM(CASE WHEN kind = 'views' THEN count ELSE 0 END) AS "Views",` +
				` MAX(url) AS "Link"` +
				" FROM gh_traffic WHERE $__timeFilter(time) AND " + RF +
				" GROUP BY 1 ORDER BY 2 DESC",
		)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf(`sum by (repo) (github_traffic_count{kind="clones",%s})`, PF), "A"),
				promTbl(fmt.Sprintf(`sum by (repo) (github_traffic_uniques{kind="clones",%s})`, PF), "B"),
				promTbl(fmt.Sprintf(`sum by (repo) (github_traffic_count{kind="views",%s})`, PF), "C"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", panelValueA: "Clones", panelValueB: "Cloners",
				panelValueC: "Views",
			}, nil, nil),
			Opts: Opts{"sort": "Clones each"},
			Desc: "Clones divided by the people who made them. There are panels for clones and " +
				"for views and none for the ratio, which is the only thing that separates " +
				"adoption from machinery: one repository here is cloned seventy three times " +
				"per unique cloner and viewed six hundred times in the same month.",
			PromDesc: windowNote,
			Overrides: []any{
				barCell("Clones each", "short", 120), width("Clones", 100),
				width("Cloners", 100), width("Views", 100), ownerLinkOn("Repository", "the traffic graph"),
			},
			GR: cloneGR, GRTF: cloneGRtf,
			GRDesc: "Graphite divides series, not columns, so the ratio is not in this table. " + grRows,
			ES:     cloneES, ESTF: cloneEStf,
			ESDesc: "In Elasticsearch this is clones and cloners; the ratio is the one column " +
				"a bucket aggregation cannot divide.",
		}),
	}
}

// lowerFirstWord is the column alias a window panel gives its value: the first
// word of the title, lowercased, as the Python that wrote these files did.
func lowerFirstWord(title string) string {
	for i := 0; i < len(title); i++ {
		if title[i] == ' ' {
			title = title[:i]
			break
		}
	}
	out := []byte(title)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}
