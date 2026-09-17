package dashboards

import "fmt"

// ── Cost ────────────────────────────────────────────────────────────────────

// billingPage is the one url every gh_billing_usage row carries, the bill
// itself: the collector writes it as a constant, and a panel link is the
// only way a table of it can be opened from, since no row is a page.
const billingPage = "https://github.com/settings/billing/summary"

// The three money columns of the bill, named the same in the SQL panel and in
// its Prometheus and Graphite twins so a store swap keeps the same table, and
// the Graphite function that adds a path's series up.
const (
	costCovered        = "Covered by the plan"
	costBilled         = "Actually billed"
	costActionsMinutes = "Actions minutes"
	costSumSeries      = "sumSeries("
)

func cost(b *builder) []Panel {
	bu := "gh_billing_usage"

	perDay := "SELECT " + timeBin + ", product AS series," +
		" SUM(gross) AS cost FROM gh_billing_usage WHERE $__timeFilter(time)" +
		" GROUP BY 1, 2 ORDER BY 1"
	// A charge that belongs to no repository is not a repository called
	// (none), so it is left out of a table of repositories. The sentinel is
	// what the row carries: `repo <> ''` matched every row, so the Copilot
	// seat was listed here as a repository, and the two twins below missed it
	// in their own spellings.
	byRepo := `SELECT repo AS "Repository", SUM(gross) AS "Gross", sku AS "SKU",` +
		` SUM(quantity) AS "Quantity", MAX(unit) AS "Unit",` +
		` MAX(price_per_unit) AS "Price", SUM(net) AS "Net"` +
		" FROM gh_billing_usage WHERE $__timeFilter(time) AND repo <> " + noneSQL +
		" GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 40"
	minutes := "SELECT " + timeBin + ", sku AS series," +
		" SUM(quantity) AS quantity FROM gh_billing_usage" +
		" WHERE $__timeFilter(time) AND unit = 'Minutes' GROUP BY 1, 2 ORDER BY 1"
	mins := gp(bu, "quantity", "unit", "Minutes")

	// The (none) of a charge attributed to no repository is the node
	// `_none_` in a Graphite path: the parentheses are not path characters
	// and become underscores, so a pattern written for a bare `none` matched
	// nothing and listed the charge as a repository.
	byRepoGR, byRepoGRtf := gTbl(fmt.Sprintf(
		`limit(sortByTotal(exclude(%s, "^`+noneGraphiteNode+`\.")), 40)`,
		rowsOf(gp(bu, "gross"), gn(bu, "repo"), gn(bu, "sku")),
	),
		"Repository, SKU", []col{{"sum", "Gross"}})
	byRepoES, byRepoEStf := esTbl(bu, []any{b.tm("repo", 40), b.tm("sku", 50), b.tm("unit", 5)},
		[]any{b.mSum("quantity"), b.mMax("price_per_unit"), b.mSum("gross"), b.mSum("net")},
		[]named{
			{panelRepoField, "Repository"},
			{"sku.keyword", "SKU"},
			{"unit.keyword", "Unit"},
			{"q", "Quantity"},
			{"p", "Price"},
			{"g", "Gross"},
			{"n", "Net"},
		},
		[]string{"NOT repo.keyword:" + noneElasticsearch})

	entryGR, entryGRtf := gTbl(rowsOf("keepLastValue("+rp("gh_actions_cache_entry", "size_bytes")+")",
		gn("gh_actions_cache_entry", "repo"), gn("gh_actions_cache_entry", "cache")),
		"Repository, cache", []col{{"lastNotNull", "Size"}})
	entryES, entryEStf := esTbl("gh_actions_cache_entry",
		[]any{b.tm("repo", 500), b.tm("cache", 50)},
		[]any{b.mSum("size_bytes"), b.mCount(), b.mMax("days_since_use")},
		[]named{
			{panelRepoField, "Repository"},
			{"cache.keyword", "Cache"},
			{"s", "Size"},
			{"n", "Entries"},
			{"d", "Days since use"},
		}, []string{ESF})

	cacheGR, cacheGRtf := gTbl(rowsOf("keepLastValue("+rp("gh_actions_cache", "size_bytes")+")",
		gn("gh_actions_cache", "repo")), "Repository", []col{{"lastNotNull", "Cache"}})
	cacheES, cacheEStf := esTbl("gh_actions_cache", []any{b.tm("repo", 500)},
		[]any{b.mNewest("size_bytes", "count")},
		[]named{{panelRepoField, "Repository"}, {"size_bytes", "Cache"}, {"count", "Entries"}},
		[]string{ESF})

	return []Panel{
		statGroup("Spend in range", box{W: 24, H: 4, X: 0, Y: 0}, []Target{sqlT(
			`SELECT SUM(gross) AS "Gross", SUM(discount) AS "Covered by the plan",` +
				` SUM(net) AS "Actually billed",` +
				` SUM(CASE WHEN unit = 'Minutes' THEN quantity ELSE 0 END) AS "Actions minutes"` +
				" FROM gh_billing_usage WHERE $__timeFilter(time)",
		)}, &P{
			Prom: []Target{
				promNamed("A", "Gross", "sum(github_billing_usage_gross)"),
				promNamed("B", costCovered, "sum(github_billing_usage_discount)"),
				promNamed("C", costBilled, "sum(github_billing_usage_net)"),
				promNamed("D", costActionsMinutes, `sum(github_billing_usage_quantity{unit="Minutes"})`),
			},
			Desc: "What the usage would have cost at list price, what the plan covered, and " +
				"what was actually billed, which is not always zero because the monthly " +
				"credit shows up there. Then the runner minutes behind it, as a count " +
				"rather than a duration: these are minutes of machine time, not elapsed " +
				"time, and a \"2.3 days\" would be a lie.",
			PromDesc: sweepCount,
			GR: []Target{
				grNamed("A", "Gross", total(costSumSeries+gp(bu, "gross")+")")),
				grNamed("B", costCovered, total(costSumSeries+gp(bu, "discount")+")")),
				grNamed("C", costBilled, total(costSumSeries+gp(bu, "net")+")")),
				grNamed("D", costActionsMinutes, total(costSumSeries+mins+")")),
			},
			ES: []Target{
				esRef("A", b.esTotal(bu, b.mSum("gross"))),
				esRef("B", b.esTotal(bu, b.mSum("discount"))),
				esRef("C", b.esTotal(bu, b.mSum("net"))),
				esRef("D", b.esTotal(bu, b.mSum("quantity"), "unit:Minutes")),
			},
			ESOver: []any{
				frameName("A", "Gross"), frameName("B", costCovered),
				frameName("C", costBilled), frameName("D", costActionsMinutes),
			},
			Opts:      Opts{"unit": "currencyUSD"},
			Overrides: []any{unitOf(costActionsMinutes, "short", 0)},
		}),
		panel("timeseries", "Cost over time by product", box{W: 12, H: 8, X: 0, Y: 4},
			[]Target{sqlTS(perDay)}, &P{
				Prom: []Target{promq("sum by (product) (github_billing_usage_gross)",
					legend("{{product}}"))},
				Opts:     mergeOpts(Opts{"stack": true, "bars": true, "unit": "currencyUSD"}, dayBins),
				SQLOpts:  seriesOpts,
				PromOpts: Opts{"bars": false},
				Desc:     "Gross cost per day. " + bucketFollowsRange,
				PromDesc: windowNote,
				GR:       []Target{grq(perBucket(gp(bu, "gross"), gn(bu, "product")))},
				ES:       []Target{b.esDaily(bu, b.mSum("gross"), "product", "", nil, "")},
			}),
		panel("timeseries", "Actions minutes over time", box{W: 12, H: 8, X: 12, Y: 4},
			[]Target{sqlTS(minutes)}, &P{
				Prom: []Target{promq(`sum by (sku) (github_billing_usage_quantity{unit="Minutes"})`,
					legend("{{sku}}"))},
				Opts:     mergeOpts(Opts{"stack": true, "bars": true, "unit": "short"}, dayBins),
				SQLOpts:  seriesOpts,
				PromOpts: Opts{"bars": false},
				Desc:     "Runner minutes per day, by SKU. " + bucketFollowsRange,
				PromDesc: windowNote,
				GR:       []Target{grq(perBucket(mins, gn(bu, "sku")))},
				ES: []Target{b.esDaily(bu, b.mSum("quantity"), "sku", "",
					[]string{"unit:Minutes"}, "")},
			}),
		panel("table", "Usage by repository", box{W: 24, H: 9, X: 0, Y: 12}, []Target{sqlT(byRepo)}, &P{
			Prom: []Target{
				promTbl(`sum by (repo, sku, unit) (github_billing_usage_quantity{repo!=""})`, "A"),
				promTbl(`max by (repo, sku, unit) (github_billing_usage_price_per_unit{repo!=""})`, "B"),
				promTbl(`sum by (repo, sku, unit) (github_billing_usage_gross{repo!=""})`, "C"),
				promTbl(`sum by (repo, sku, unit) (github_billing_usage_net{repo!=""})`, "D"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "sku": "SKU", "unit": "Unit",
				panelValueA: "Quantity", panelValueB: "Price", panelValueC: "Gross",
				panelValueD: "Net",
			}, nil, nil),
			Opts:     mergeOpts(Opts{"sort": "Gross"}, ownerPageLink("Open the bill", billingPage)),
			PromDesc: sweepCount,
			Desc: "Price is what explains a small quantity costing more than a large one: " +
				"thirty thousand macOS minutes cost more than two hundred and forty thousand " +
				"Linux ones. A repository can appear here and in no other panel, because the " +
				"list that bills and the list that is swept are not the same list. Every " +
				"figure here is itemized on the bill itself, which the panel links to.",
			Overrides: []any{
				unitOf("Gross", "currencyUSD", 110), unitOf("Net", "currencyUSD", 110),
				unitOf("Price", "currencyUSD", 100),
				width("SKU", 160), width("Unit", 90), barCell("Quantity", "short", 120),
			},
			GR: byRepoGR, GRTF: byRepoGRtf,
			GRDesc: "Graphite names each row repository and SKU from the path. " + grRows,
			ES:     byRepoES, ESTF: byRepoEStf,
		}),
		panel("table", "Cache entries by key", box{W: 24, H: 8, X: 0, Y: 21}, []Target{sqlT(
			`SELECT cache AS "Cache", SUM(size_bytes) AS "Size", repo AS "Repository",` +
				` COUNT(*) AS "Entries",` +
				` MIN(days_since_use) AS "Days since use" FROM (` +
				"SELECT repo, cache, ref, size_bytes, days_since_use," +
				" ROW_NUMBER() OVER (PARTITION BY repo, cache, ref ORDER BY time DESC) AS rn" +
				" FROM gh_actions_cache_entry WHERE $__timeFilter(time) AND " + RF +
				") x WHERE rn = 1 GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 25",
		)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("topk(25, sum by (repo, cache) (github_actions_cache_entry_size_bytes{%s}))", PF), "A"),
				promTbl(fmt.Sprintf("min by (repo, cache) (github_actions_cache_entry_days_since_use{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "cache": "Cache", panelValueA: "Size",
				panelValueB: "Days since use",
			}, nil, map[string]int{"repo": 0, "cache": 1}),
			Opts: Opts{"sort": "Size"},
			Desc: "The total says a repository holds twelve gigabytes. This says which key " +
				"holds them and which has not been touched for a week, which is what decides " +
				"what GitHub evicts at the ten gigabyte ceiling.",
			Overrides: []any{unitOf("Size", "bytes", 120), barCell("Entries", "short", 100)},
			GR:        entryGR, GRTF: entryGRtf, GRDesc: grSlot,
			ES: entryES, ESTF: entryEStf,
		}),
		panel("table", "Cache against the ceiling", box{W: 24, H: 8, X: 0, Y: 29}, []Target{sqlT(
			`SELECT repo AS "Repository", MAX(size_bytes) AS "Cache",` +
				` MAX(count) AS "Entries"` +
				" FROM gh_actions_cache WHERE $__timeFilter(time) AND " + RF +
				" GROUP BY 1 ORDER BY 2 DESC",
		)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("max by (repo) (github_actions_cache_size_bytes{%s})", PF), "A"),
				promTbl(fmt.Sprintf("max by (repo) (github_actions_cache_count{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", panelValueA: "Cache", panelValueB: "Entries",
			}, nil, nil),
			Opts: Opts{"sort": "Cache"},
			Desc: "GitHub caps a repository at ten gigabytes and evicts the least recently used " +
				"entry past it. Three repositories here are over the line and are throwing " +
				"caches away on every run; the panel above says which key goes.",
			Overrides: []any{override("Cache", []any{
				map[string]any{"id": "unit", "value": "bytes"},
				map[string]any{"id": "custom.cellOptions", "value": map[string]any{
					"type": "gauge", "mode": "gradient",
				}},
				map[string]any{"id": "max", "value": 10737418240},
				map[string]any{"id": "thresholds", "value": map[string]any{
					"mode": "absolute", "steps": []any{
						map[string]any{"color": "green", "value": nil},
						map[string]any{"color": "orange", "value": 8589934592},
						map[string]any{"color": "red", "value": 10737418240},
					},
				}},
				map[string]any{"id": "custom.width", "value": 180},
			}), width("Entries", 110)},
			GR: cacheGR, GRTF: cacheGRtf, GRDesc: grSlot,
			ES: cacheES, ESTF: cacheEStf,
		}),
	}
}
