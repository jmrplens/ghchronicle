package dashboards

import (
	"fmt"
	"strings"
)

// ── Organizations ───────────────────────────────────────────────────────────

// organizations is what each organization in targets.orgs says about itself
// and what it spent: gh_org, one row per organization stamped at each sweep,
// and gh_org_billing_usage, its own bill, which the Cost section does not add
// to the account's, since somebody else pays it.
//
// gh_org's counts are an owner's to read, most of them: an organization the
// token is only a member of writes the members, the teams, the projects and
// the repositories, and leaves out the rest. A table in InfluxDB or
// PostgreSQL names columns, and a column no row has written is refused, so
// these tables answer once one organization of the configuration is the
// account's to own, which is what targets.orgs is for.
func organizations(b *builder) []Panel {
	return append(orgTables(b), orgSpend(b)...)
}

// orgNewest is the newest gh_org row of each organization inside the range,
// as a FROM clause: a count can go down, and the largest of the range would
// keep a member who left.
const orgNewest = "(SELECT *, ROW_NUMBER() OVER (PARTITION BY org ORDER BY time DESC) AS rn" +
	" FROM gh_org WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY org"

// orgTwoFactor is the heading of the column drawn as on or off rather than at
// the width of a count.
const orgTwoFactor = "Two-factor"

// orgColumn is one column of a gh_org table: the field, its heading, the
// width it is drawn in, and whether it is text, which Prometheus does not
// carry and Elasticsearch buckets on rather than measures.
type orgColumn struct {
	field, name string
	w           int
	text        bool
}

// orgTables is the two tables of gh_org: the people and the machinery.
func orgTables(b *builder) []Panel {
	people := []orgColumn{
		{field: "members", name: "Members", w: 100},
		{field: "admins", name: "Owners", w: 90},
		{field: "teams", name: "Teams", w: 80},
		{field: "outside_collaborators", name: "Outside collaborators", w: 175},
		{field: "pending_invitations", name: "Invitations", w: 110},
		{field: "copilot_seats", name: "Copilot seats", w: 120},
		{field: "copilot_active", name: "Copilot used", w: 120},
		{field: "two_factor_required", name: orgTwoFactor, w: 100},
		{field: "plan", name: "Plan", w: 90, text: true},
	}
	machinery := []orgColumn{
		{field: "public_repos", name: "Public repositories", w: 160},
		{field: "private_repos", name: "Private repositories", w: 165},
		{field: "runners", name: "Runners", w: 90},
		{field: "secrets", name: "Secrets", w: 90},
		{field: "variables", name: "Variables", w: 90},
		{field: "hooks", name: "Webhooks", w: 100},
		{field: "installations", name: "Apps", w: 80},
		{field: "projects", name: "Projects", w: 90},
		{field: "actions_cache_bytes", name: "Actions cache", w: 130},
	}
	return []Panel{
		orgTable(b, "Organizations: people", 0, people,
			"Who is in each organization, newest row in the range: its members and how many "+
				"of them own it, its teams, the outside collaborators given a repository, the "+
				"invitations not yet answered, and the Copilot seats it pays for against the "+
				"ones used this cycle. Two-factor says whether the organization requires it "+
				"of its members. The outside collaborators, the invitations, Copilot and "+
				"two-factor answer an owner alone, and are empty for an organization the "+
				"account is only a member of.",
			[]any{onOff(orgTwoFactor, 100)}),
		orgTable(b, "Organizations: machinery", 5, machinery,
			"What the organization's owners set up, newest row in the range: its "+
				"repositories, its self-hosted runners, the Actions secrets and variables "+
				"shared by its repositories, its webhooks, the GitHub Apps installed on it, "+
				"its projects, and what all its repositories hold in the Actions cache "+
				"together. Each repository's own are in their sections. Every column but "+
				"the repositories and the projects answers an owner alone.",
			[]any{unitOf("Actions cache", "bytes", 130)}),
	}
}

// promValues is the value columns a Prometheus table of instant
// queries is drawn with here, one query each.
var promValues = []string{panelValueA, panelValueB, panelValueC, panelValueD, panelValueE, panelValueF, panelValueG}

// orgTable is one table of gh_org's newest rows.
func orgTable(b *builder, title string, y int, cols []orgColumn, desc string, overrides []any) Panel {
	var query strings.Builder
	query.WriteString(`SELECT org AS "Organization"`)
	var prom []Target
	var promLacks, promText []string
	rename := map[string]string{"org": "Organization"}
	buckets := []any{b.tm("org", 50), b.newestDoc()}
	names := []named{{"org.keyword", "Organization"}}
	var metrics []any
	for _, c := range cols {
		fmt.Fprintf(&query, `, %s AS %q`, c.field, c.name)
		switch {
		case c.text:
			promText = append(promText, c.name)
			buckets = append(buckets, b.tmURL(c.field))
			names = append(names, named{c.field + ".keyword", c.name})
		case len(prom) < len(promValues):
			ref := "ABCDEFG"[len(prom) : len(prom)+1]
			rename[promValues[len(prom)]] = c.name
			prom = append(prom, promTbl(fmt.Sprintf("max by (org) (github_org_%s)", c.field), ref))
		default:
			promLacks = append(promLacks, c.name)
		}
		if !c.text {
			metrics = append(metrics, b.mMax(c.field))
		}
		if c.name != orgTwoFactor {
			overrides = append(overrides, width(c.name, c.w))
		}
	}
	buckets = append(buckets, b.tmURL())
	names = append(names, named{panelURLField, "Link"})
	for _, c := range cols {
		if !c.text {
			names = append(names, named{c.field, c.name})
		}
	}
	query.WriteString(`, url AS "Link" FROM ` + orgNewest)
	esT, esTF := esTbl("gh_org", buckets, metrics, names, nil, hideColumns(panelESTime))
	var promDesc []string
	if len(promLacks) > 0 {
		promDesc = append(promDesc, "Prometheus draws seven counts, a query each, and leaves out "+
			strings.Join(promLacks, " and ")+".")
	}
	if len(promText) > 0 {
		promDesc = append(promDesc, "The exporter carries numbers alone, so there is no "+
			strings.Join(promText, " or ")+" column.")
	}
	return panel("table", title, box{W: 24, H: 5, X: 0, Y: y}, []Target{sqlT(query.String())}, &P{
		Prom:     prom,
		PromTF:   merged(rename, nil),
		PromDesc: strings.Join(promDesc, " "),
		GRNote: cannot("each organization with its counts beside one another.",
			"A Graphite table reduces one series to one row, and an organization is a series per count.",
			"graphite"),
		Desc:      desc,
		Opts:      Opts{"sort": "Organization"},
		Overrides: append(overrides, linkOn("Organization")),
		ES:        esT, ESTF: esTF,
		ESDesc: "Elasticsearch reads the newest document of each organization in the range.",
	})
}

// orgSpend is the organizations' bill: what it came to, by day, and by
// repository, the account's Cost section drawn over gh_org_billing_usage.
func orgSpend(b *builder) []Panel {
	ob := "gh_org_billing_usage"
	perDay := "SELECT " + timeBin + ", org AS series," +
		" SUM(gross) AS cost FROM gh_org_billing_usage WHERE $__timeFilter(time)" +
		" GROUP BY 1, 2 ORDER BY 1"
	minutes := "SELECT " + timeBin + ", org AS series," +
		" SUM(quantity) AS quantity FROM gh_org_billing_usage" +
		" WHERE $__timeFilter(time) AND unit = 'Minutes' GROUP BY 1, 2 ORDER BY 1"
	byRepo := `SELECT full_name AS "Repository", SUM(gross) AS "Gross", sku AS "SKU",` +
		` SUM(quantity) AS "Quantity", MAX(unit) AS "Unit",` +
		` MAX(price_per_unit) AS "Price", SUM(net) AS "Net", MAX(url) AS "Link"` +
		" FROM gh_org_billing_usage WHERE $__timeFilter(time) AND repo <> " + noneSQL +
		" GROUP BY 1, 3 ORDER BY 2 DESC, 1, 3 LIMIT 40"
	mins := gp(ob, "quantity", "unit", "Minutes")

	byRepoGR, byRepoGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(exclude(%s, "^`+noneGraphiteNode+`\.")), 40)`,
		rowsOf(gp(ob, "gross"), gn(ob, "full_name"), gn(ob, "sku")),
	), "Repository, SKU", []col{{"sum", "Gross"}})
	byRepoES, byRepoEStf := esTbl(ob,
		[]any{b.tm("full_name", 40), b.tm("sku", 50), b.tm("unit", 5), b.tmURL()},
		[]any{b.mSum("quantity"), b.mMax("price_per_unit"), b.mSum("gross"), b.mSum("net")},
		[]named{
			{panelFullNameField, "Repository"},
			{"sku.keyword", "SKU"},
			{"unit.keyword", "Unit"},
			{panelURLField, "Link"},
			{"q", "Quantity"},
			{"p", "Price"},
			{"g", "Gross"},
			{"n", "Net"},
		},
		[]string{"NOT repo.keyword:" + noneElasticsearch})

	return []Panel{
		statGroup("Organization spend in range", box{W: 24, H: 4, X: 0, Y: 10}, []Target{sqlT(
			`SELECT SUM(gross) AS "Gross", SUM(discount) AS "Covered by the plan",` +
				` SUM(net) AS "Actually billed",` +
				` SUM(CASE WHEN unit = 'Minutes' THEN quantity ELSE 0 END) AS "Actions minutes"` +
				" FROM gh_org_billing_usage WHERE $__timeFilter(time)",
		)}, &P{
			Prom: []Target{
				promAggregated("A", "Gross", "sum(github_org_billing_usage_gross)"),
				promAggregated("B", costCovered, "sum(github_org_billing_usage_discount)"),
				promAggregated("C", costBilled, "sum(github_org_billing_usage_net)"),
				promAggregated("D", costActionsMinutes, `sum(github_org_billing_usage_quantity{unit="Minutes"})`),
			},
			Desc: "The organizations' own bills, apart from the account's in Cost: a " +
				"repository an organization owns is billed to it. What the usage would have " +
				"cost at list price, what each plan covered, what was billed, and the runner " +
				"minutes behind it. Only an organization's owners may read its bill.",
			PromDesc: sweepCount,
			GR: []Target{
				grNamed("A", "Gross", total(costSumSeries+gp(ob, "gross")+")")),
				grNamed("B", costCovered, total(costSumSeries+gp(ob, "discount")+")")),
				grNamed("C", costBilled, total(costSumSeries+gp(ob, "net")+")")),
				grNamed("D", costActionsMinutes, total(costSumSeries+mins+")")),
			},
			ES: []Target{
				esRef("A", b.esOverRange(ob, b.mSum("gross"))),
				esRef("B", b.esOverRange(ob, b.mSum("discount"))),
				esRef("C", b.esOverRange(ob, b.mSum("net"))),
				esRef("D", b.esOverRange(ob, b.mSum("quantity"), "unit:Minutes")),
			},
			ESOver: []any{
				frameName("A", "Gross"), frameName("B", costCovered),
				frameName("C", costBilled), frameName("D", costActionsMinutes),
			},
			Opts: Opts{"unit": "currencyUSD"},
			Overrides: []any{
				unitOf(costActionsMinutes, "short", 0),
				noValueOf("Gross", costNoUsage), noValueOf(costCovered, costNoUsage),
				noValueOf(costBilled, costNoUsage), noValueOf(costActionsMinutes, costNoUsage),
			},
		}),
		panel("timeseries", "Organization cost over time", box{W: 12, H: 8, X: 0, Y: 14},
			[]Target{sqlTS(perDay)}, &P{
				Prom: []Target{promq("sum by (org) (github_org_billing_usage_gross)",
					legend("{{org}}"))},
				Opts:     mergeOpts(Opts{"stack": true, "bars": true, "unit": "currencyUSD"}, dayBins),
				SQLOpts:  seriesOpts,
				PromOpts: Opts{"bars": false},
				Desc:     "Gross cost per day, by organization. " + bucketFollowsRange,
				PromDesc: windowNote,
				GR:       []Target{grq(perBucket(gp(ob, "gross"), gn(ob, "org")))},
				ES:       []Target{b.esDaily(ob, b.mSum("gross"), "org", "", nil, "")},
			}),
		panel("timeseries", "Organization Actions minutes", box{W: 12, H: 8, X: 12, Y: 14},
			[]Target{sqlTS(minutes)}, &P{
				Prom: []Target{promq(`sum by (org) (github_org_billing_usage_quantity{unit="Minutes"})`,
					legend("{{org}}"))},
				Opts:     mergeOpts(Opts{"stack": true, "bars": true, "unit": "short"}, dayBins),
				SQLOpts:  seriesOpts,
				PromOpts: Opts{"bars": false},
				Desc:     "Runner minutes per day, by organization. " + bucketFollowsRange,
				PromDesc: windowNote,
				GR:       []Target{grq(perBucket(mins, gn(ob, "org")))},
				ES: []Target{b.esDaily(ob, b.mSum("quantity"), "org", "",
					[]string{"unit:Minutes"}, "")},
			}),
		panel("table", "Organization usage by repo", box{W: 24, H: 9, X: 0, Y: 22}, []Target{sqlT(byRepo)}, &P{
			Prom: []Target{
				promTbl(orgPromByRepo("quantity", "sum"), "A"),
				promTbl(orgPromByRepo("price_per_unit", "max"), "B"),
				promTbl(orgPromByRepo("gross", "sum"), "C"),
				promTbl(orgPromByRepo("net", "sum"), "D"),
			},
			PromTF: merged(map[string]string{
				"full_name": "Repository", "sku": "SKU", "unit": "Unit",
				panelValueA: "Quantity", panelValueB: "Price", panelValueC: "Gross",
				panelValueD: "Net",
			}, nil),
			Opts:     Opts{"sort": "Gross"},
			PromDesc: sweepCount,
			Desc: "Each organization repository's charges in the range, by SKU, read off the " +
				"organization's own bill, which each row links to. A charge that belongs to " +
				"no repository, a Copilot seat, is in the totals above and not here.",
			Overrides: []any{
				fullNameColumn(),
				unitOf("Gross", "currencyUSD", 110), unitOf("Net", "currencyUSD", 110),
				unitOf("Price", "currencyUSD", 100),
				width("SKU", 160), width("Unit", 90), barCell("Quantity", "short", 120),
				ownerLinkOn("Repository", "the bill"),
			},
			GR: byRepoGR, GRTF: byRepoGRtf,
			GRDesc: "Graphite names each row repository and SKU from the path. " + grRows,
			ES:     byRepoES, ESTF: byRepoEStf,
		}),
	}
}

// orgPromByRepo is promByRepo over the organizations' bill.
func orgPromByRepo(field, aggregation string) string {
	return fmt.Sprintf("%s by (full_name, sku, unit) (github_org_billing_usage_%s{repo!=%q})",
		aggregation, field, noneValue)
}
