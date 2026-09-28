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
	costCacheIdle      = "Days since use"
	costNoUsage        = "no usage"
)

// promByRepo is one column of the cost table in Prometheus: a billing field
// aggregated by repository, SKU and unit, with the charge that belongs to no
// repository left out.
//
// The exclusion is written here rather than inline because it is the fourth
// spelling of one rule and the one that was missed: the SQL stores compare the
// value, Graphite matches the path node and Elasticsearch negates a term, and
// all three were corrected to the sentinel the collectors really write while
// this store went on asking for a label that is not empty. The sentinel is not
// empty, so the Copilot seat stayed in the table as a repository called (none)
// here alone.
func promByRepo(field, aggregation string) string {
	return fmt.Sprintf("%s by (full_name, repo, sku, unit) (github_billing_usage_%s{repo!=%q})",
		aggregation, field, noneValue)
}

func cost(b *builder) []Panel {
	bu := "gh_billing_usage"

	perDay := "SELECT " + timeBin + ", product AS series," +
		" SUM(gross) AS cost FROM gh_billing_usage WHERE $__timeFilter(time)" +
		" GROUP BY 1, 2 ORDER BY 1"
	// A charge that belongs to no repository is not a repository called
	// (none), so it is left out of a table of repositories. The sentinel is
	// what the row carries: `repo <> ''` matched every row, so the Copilot
	// seat was listed here as a repository, and the three twins below missed
	// it in their own spellings.
	byRepo := `SELECT repo AS "Repository", SUM(gross) AS "Gross", sku AS "SKU",` +
		` SUM(quantity) AS "Quantity", MAX(unit) AS "Unit",` +
		` MAX(price_per_unit) AS "Price", SUM(net) AS "Net"` +
		" FROM gh_billing_usage WHERE $__timeFilter(time) AND repo <> " + noneSQL +
		" GROUP BY 1, full_name, 3 ORDER BY 2 DESC, full_name, 3 LIMIT 40"
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
	byRepoES, byRepoEStf := esTbl(bu, append(b.tmRepo(40), b.tm("sku", 50), b.tm("unit", 5)),
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
		[]string{"NOT repo.keyword:" + noneElasticsearch}, hideColumns(panelFullNameField))

	cacheGR, cacheGRtf := gTbl(rowsOf("keepLastValue("+rp("gh_actions_cache", "size_bytes")+")",
		gn("gh_actions_cache", "repo")), "Repository", []col{{"lastNotNull", "Cache"}})
	cacheES, cacheEStf := esTbl("gh_actions_cache", b.tmRepo(500),
		[]any{b.mNewest("size_bytes", "count")},
		[]named{{panelRepoField, "Repository"}, {"size_bytes", "Cache"}, {"count", "Entries"}},
		[]string{ESF}, hideColumns(panelFullNameField))

	return []Panel{
		statGroup("Spend in range", box{W: 24, H: 4, X: 0, Y: 0}, []Target{sqlT(
			`SELECT SUM(gross) AS "Gross", SUM(discount) AS "Covered by the plan",` +
				` SUM(net) AS "Actually billed",` +
				` SUM(CASE WHEN unit = 'Minutes' THEN quantity ELSE 0 END) AS "Actions minutes"` +
				" FROM gh_billing_usage WHERE $__timeFilter(time)",
		)}, &P{
			Prom: []Target{
				promAggregated("A", "Gross", "sum(github_billing_usage_gross)"),
				promAggregated("B", costCovered, "sum(github_billing_usage_discount)"),
				promAggregated("C", costBilled, "sum(github_billing_usage_net)"),
				promAggregated("D", costActionsMinutes, `sum(github_billing_usage_quantity{unit="Minutes"})`),
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
				esRef("A", b.esOverRange(bu, b.mSum("gross"))),
				esRef("B", b.esOverRange(bu, b.mSum("discount"))),
				esRef("C", b.esOverRange(bu, b.mSum("net"))),
				esRef("D", b.esOverRange(bu, b.mSum("quantity"), "unit:Minutes")),
			},
			ESOver: []any{
				frameName("A", "Gross"), frameName("B", costCovered),
				frameName("C", costBilled), frameName("D", costActionsMinutes),
			},
			Opts: Opts{"unit": "currencyUSD"},
			Overrides: []any{
				unitOf(costActionsMinutes, "short", 0),
				// GitHub answers the products that were used, so no row is no
				// usage.
				noValueOf("Gross", costNoUsage), noValueOf(costCovered, costNoUsage),
				noValueOf(costBilled, costNoUsage), noValueOf(costActionsMinutes, costNoUsage),
			},
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
				promTbl(promByRepo("quantity", "sum"), "A"),
				promTbl(promByRepo("price_per_unit", "max"), "B"),
				promTbl(promByRepo("gross", "sum"), "C"),
				promTbl(promByRepo("net", "sum"), "D"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "sku": "SKU", "unit": "Unit",
				panelValueA: "Quantity", panelValueB: "Price", panelValueC: "Gross",
				panelValueD: "Net",
			}, nil),

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
		cacheEntries(b),
		panel("table", "Cache against the ceiling", box{W: 24, H: 8, X: 0, Y: 29}, []Target{sqlT(
			// Each repository's newest row. A cache is what GitHub holds now,
			// and the largest of the range kept a repository at a size its
			// evictions had since brought down, beside a tile and a table
			// above that read the newest.
			`SELECT repo AS "Repository", size_bytes AS "Cache",` +
				` count AS "Entries" FROM (` +
				"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn" +
				" FROM gh_actions_cache WHERE $__timeFilter(time) AND " + RF +
				") x WHERE rn = 1 ORDER BY 2 DESC, full_name",
		)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("max by (full_name, repo) (github_actions_cache_size_bytes{%s})", PF), "A"),
				promTbl(fmt.Sprintf("max by (full_name, repo) (github_actions_cache_count{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", panelValueA: "Cache", panelValueB: "Entries",
			}, nil),

			Opts: Opts{"sort": "Cache"},
			Desc: "GitHub caps a repository at ten gigabytes and evicts the least recently " +
				"used entry past it. The bar is each repository's cache against that cap, so " +
				"one that has reached the red end is throwing caches away on every run and " +
				"the panel above says which key goes.",
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
			GR: cacheGR, GRTF: cacheGRtf, GRDesc: grRows,
			ES: cacheES, ESTF: cacheEStf,
		}),
	}
}

// cacheEntries is "Cache entries by key", out of cost for the length of what
// its five queries have to say.
func cacheEntries(b *builder) Panel {
	// A row of gh_actions_cache_entry is one cache on one ref, its entries
	// already summed into it, stamped at the start of the UTC day and
	// rewritten through the day. It is what GitHub holds now, so the panel
	// reads each repository's newest snapshot and nothing older: the rows of
	// the last day in the range the collector read that repository's caches.
	// Each ref's newest row, which is what this summed until 2.6.0 was
	// checked against GitHub, kept every ref GitHub had evicted since, at its
	// size on the last day it was listed. Measured on 2026-09-27 over thirty
	// days: jmrplens/gitlab-mcp-server's golangci-lint read 128 entries and
	// 9.46 GiB over 108 refs, the oldest row from 2026-09-18, where the rows
	// of that day held 44 entries and 3.08 GiB over 24 refs, byte for byte
	// what GitHub listed under that cache's keys. The rows share one
	// timestamp a day, so the newest snapshot is the rows at a repository's
	// newest timestamp.
	//
	// Elasticsearch takes the same rows with a terms bucket on the timestamp
	// under each repository, the newest one kept, and sums the caches under
	// it. Graphite cannot find each repository's newest day, so it reads the
	// last UTC day of the range: summarize makes a point a day, the refs with
	// nothing on the last one are dropped, and what is left is added up per
	// cache. The -1 is a stand-in for "nothing that hour", since filterSeries
	// reads the last value that is not null, and a size is never below zero;
	// the largest of a day's hours is then the day's row, or -1.
	//
	// The -1 goes in before the days are made, not after. graphite-web
	// 1.1.10 rounds the end of what it reads up to the next hour, and
	// summarize ends on the day after that end, so a range ending in the last
	// UTC hour of a day gains one more day, which holds no point. Filled with
	// -1 after summarize, that day was the last value of every ref, and the
	// table was empty for the hour. Filled before, it holds no point to fill
	// and stays null, which filterSeries passes over. Measured against
	// graphiteapp/graphite-statsd:1.1.10-5 with two refs written on the last
	// day and one the day before, ending at 22:30, 23:30 and 23:59 UTC: the
	// two refs, then nothing and nothing before; the two refs at all three
	// now. A timeSlice to "now" does not mend it: graphite-web counts a
	// sliced series' time from the start of the request and not from the
	// start of the day summarize aligned it to, so the day after the range
	// read as still inside it, and the table stayed empty.
	//
	// consolidateBy keeps a long range drawn on a narrow screen reading the
	// last day rather than the mean of the last two, and it names the series
	// after itself, so aliasByNode names each row by its repository and cache
	// again, the last two of the three nodes groupByNodes left: a repository
	// is grouped by its full name, which two owners cannot share, and named
	// by its short one.
	ce := "gh_actions_cache_entry"
	lastDay := fmt.Sprintf(`removeBelowValue(filterSeries(summarize(transformNull(%s, -1), "1d", "max"),`+
		` "last", ">=", 0), 0)`, rp(ce, "size_bytes"))
	entryGR, entryGRtf := gTbl(rowsOf(fmt.Sprintf(`consolidateBy(groupByNodes(%s, "sum", %d, %d, %d), "last")`,
		lastDay, gn(ce, "full_name"), gn(ce, "repo"), gn(ce, "cache")), 1, 2),
		"Repository, cache", []col{{"lastNotNull", "Size"}})
	entryES, entryEStf := esTbl(ce,
		append(b.tmRepo(500), b.terms("@timestamp", 1, "_key", "desc"), b.tm("cache", 50)),
		[]any{b.mSum("size_bytes"), b.mSum("caches"), b.mMin("days_since_use")},
		[]named{
			{panelRepoField, "Repository"},
			{"cache.keyword", "Cache"},
			{"s", "Size"},
			{"n", "Entries"},
			{"d", costCacheIdle},
		}, []string{ESF}, hideColumns(panelFullNameField, "@timestamp"))

	return panel("table", "Cache entries by key", box{W: 24, H: 8, X: 0, Y: 21}, []Target{sqlT(
		`SELECT cache AS "Cache", SUM(size_bytes) AS "Size", repo AS "Repository",` +
			` SUM(caches) AS "Entries",` +
			` MIN(days_since_use) AS "` + costCacheIdle + `" FROM (` +
			"SELECT time, full_name, repo, cache, size_bytes, caches, days_since_use," +
			" MAX(time) OVER (PARTITION BY full_name) AS newest" +
			" FROM gh_actions_cache_entry WHERE $__timeFilter(time) AND " + RF +
			") x WHERE time = newest GROUP BY 1, full_name, 3 ORDER BY 2 DESC, 1, full_name LIMIT 25",
	)}, &P{
		Prom: []Target{
			promTbl(fmt.Sprintf("topk(25, sum by (full_name, repo, cache) (github_actions_cache_entry_size_bytes{%s}))", PF), "A"),
			promTbl(fmt.Sprintf("min by (full_name, repo, cache) (github_actions_cache_entry_days_since_use{%s})", PF), "B"),
			promTbl(fmt.Sprintf("sum by (full_name, repo, cache) (github_actions_cache_entry_caches{%s})", PF), "C"),
		},
		PromTF: merged(map[string]string{
			"repo": "Repository", "cache": "Cache", panelValueA: "Size",
			panelValueB: costCacheIdle, panelValueC: "Entries",
		}, nil),

		Opts: Opts{"sort": "Size"},
		Desc: "The total says a repository holds twelve gigabytes. This says which key " +
			"holds them and which has not been touched for a week, which is what decides " +
			"what GitHub evicts at the ten gigabyte ceiling. It is what each repository " +
			"holds now: the entries of its newest snapshot in the range, the last day " +
			"the collector read its caches, and none of the refs GitHub has evicted " +
			"since an earlier one.",
		PromDesc: "In Prometheus each ref is the value last pushed for it, and a ref GitHub " +
			"evicted is never pushed again: the exporter drops it a day after the last " +
			"sweep that listed it, and the OTLP sink sends it again until the process " +
			"restarts, so until then it still counts here.",
		Overrides: []any{unitOf("Size", "bytes", 120), barCell("Entries", "short", 100)},
		GR:        entryGR, GRTF: entryGRtf,
		GRDesc: "Graphite cannot find each repository's newest day, so it reads the last " +
			"UTC day of the range: from midnight UTC until the day's first pass of the " +
			"`actions` family, within a quarter of an hour at the default cadence, the " +
			"table is empty, and a repository that pass could not read is missing from it. " +
			grRows,
		ES: entryES, ESTF: entryEStf,
	})
}
