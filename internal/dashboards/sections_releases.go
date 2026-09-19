package dashboards

import "fmt"

// ── Releases ────────────────────────────────────────────────────────────────

func releases(b *builder) []Panel {
	totalSQL := latestSumSQL("gh_release", "downloads", "repo, tag")
	// The release page rides beside the bars as a hidden column, which is
	// what a click on a bar opens.
	byTag := `SELECT repo || ' ' || tag AS "Release", downloads AS "Downloads", url AS "Page" FROM (` +
		"SELECT repo, tag, downloads, url, ROW_NUMBER() OVER (PARTITION BY repo, tag" +
		" ORDER BY time DESC) AS rn FROM gh_release WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 AND downloads > 0 ORDER BY 2 DESC LIMIT 12"
	// How many releases have been downloaded at all: the number beside the
	// total, so the tile above it is not the only thing in its column.
	countSQL := "SELECT COUNT(*) AS value FROM (SELECT repo, tag, downloads," +
		" ROW_NUMBER() OVER (PARTITION BY repo, tag ORDER BY time DESC) AS rn" +
		" FROM gh_release WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 AND downloads > 0"
	// An asset's url is the file itself, so the column says Download; the
	// release page comes from gh_release, joined on the tag, and is the
	// row's Link, so a reader can open the notes without fetching the binary.
	release := "SELECT repo, tag, url, ROW_NUMBER() OVER (PARTITION BY repo, tag" +
		" ORDER BY time DESC) AS rn FROM gh_release WHERE $__timeFilter(time) AND " + RF
	assets := `SELECT a.asset AS "Asset", a.downloads AS "Downloads", a.tag AS "Tag",` +
		` a.repo AS "Repository", a.size_bytes AS "Size",` +
		` a.url AS "Download", r.url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, tag, asset ORDER BY time DESC) AS rn" +
		" FROM gh_release_asset WHERE $__timeFilter(time) AND " + RF + ") a" +
		" LEFT JOIN (" + release + ") r ON r.repo = a.repo AND r.tag = a.tag AND r.rn = 1" +
		" WHERE a.rn = 1 ORDER BY a.downloads DESC LIMIT 40"
	rl, ra := "gh_release", "gh_release_asset"

	byTagGR, byTagGRtf := gTbl(topRows(rp(rl, "downloads"), 12, gn(rl, "repo"), gn(rl, "tag")),
		"Release", []col{{"lastNotNull", "Downloads"}})
	byTagES, byTagEStf := esTbl(rl, []any{b.tm("tag", 500), b.tm("repo", 50), b.tmURL()},
		[]any{b.mNewest("downloads")},
		[]named{
			{"tag.keyword", "Release"},
			{"repo.keyword", "Repository"},
			{"url.keyword", "Page"},
			{"d", "Downloads"},
		},
		[]string{ESF})

	assetsGR, assetsGRtf := gTbl(topRows(rp(ra, "downloads"), 40,
		gn(ra, "repo"), gn(ra, "tag"), gn(ra, "asset")),
		"Asset", []col{{"lastNotNull", "Downloads"}})
	// The same shape for the panel below, and it is the shape that was
	// missing. A Graphite target handed to a table panel with no reduction is
	// rendered as the frame it is, one row per storage slot: this drew seven
	// hundred and twenty hourly rows under a single column headed with the
	// whole expression, while its own description promised the one row per
	// series every other Graphite table here gives. The sum of a
	// nonNegativeDerivative over the range is what the SQL twin computes as
	// the last value less the first.
	gainedGR, gainedGRtf := gTbl(rowsOf(fmt.Sprintf(
		`limit(sortBy(nonNegativeDerivative(%s), "sum", true), 25)`,
		rp(ra, "downloads"),
	), gn(ra, "repo"), gn(ra, "tag"), gn(ra, "asset")),
		"Asset", []col{{"sum", "Gained"}})
	assetsES, assetsEStf := esTbl(ra, []any{b.tm("repo", 50), b.tm("tag", 500), b.tm("asset", 40), b.tmURL()},
		[]any{b.mNewest("downloads", "size_bytes")},
		[]named{
			{"repo.keyword", "Repository"},
			{"tag.keyword", "Tag"},
			{"asset.keyword", "Asset"},
			{"url.keyword", "Download"},
			{"downloads", "Downloads"},
			{"size_bytes", "Size"},
		}, []string{ESF})

	return []Panel{
		// One group in the column beside the chart, not two tiles with the
		// chart between them: on a phone, which stacks by position, the
		// second number arrived after the chart that explains it.
		statGroup("Downloads", box{W: 6, H: 8, X: 0, Y: 0}, []Target{
			sqlT(namedValue(totalSQL, "Total")),
			{Kind: "sql", Format: "table", Ref: "B", SQL: namedValue(countSQL, "Releases")},
		}, &P{
			Prom: []Target{
				promNamed("A", "Total", fmt.Sprintf("sum(github_release_downloads{%s})", PF)),
				promNamed("B", "Releases", fmt.Sprintf(
					"count(sum by (repo, tag) (github_release_downloads{%s}) > 0)", PF,
				)),
			},
			Desc: "Release asset downloads, counted by GitHub since each release was " +
				"published, and how many releases have been downloaded at all, as of the " +
				"newest reading of each.",
			GR: []Target{
				grNamed("A", "Total", latestSum(rp(rl, "downloads"))),
				// Graphite counts the series with a download in them; a series
				// is a release, so this is the same number.
				grNamed("B", "Releases", fmt.Sprintf(
					"countSeries(removeEmptySeries(removeBelowValue(keepLastValue(%s), 1)))",
					rp(rl, "downloads"),
				)),
			},
			ES: append(
				esRefs("A", b.esLatestSum(rl, "downloads", "tag")),
				esRef("B", b.esTotal(rl, b.mUniq("tag"), ESF)),
			),
			ESOver: []any{frameName("A", "Total"), frameName("B", "Releases")},
			ESOpts: Opts{"calc": "sum"},
			ESDesc: "In Elasticsearch the second counts distinct tags, downloaded or not: a " +
				"cardinality cannot be filtered on the newest value.",
		}),
		panel("barchart", "Downloads by release", box{W: 18, H: 8, X: 6, Y: 0}, []Target{sqlT(byTag)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				`label_join(topk(12, sum by (repo, tag) (github_release_downloads{%s}) > 0),`+
					` "release", " ", "repo", "tag")`, PF,
			))},
			PromTF: []any{organize(map[string]string{"release": "Release", "Value": "Downloads"},
				[]string{"repo", "tag"}, nil)},
			Desc:      "The twelve most downloaded releases. A click on a bar opens the release page.",
			Overrides: barLink("Downloads", "Page", "Open the release"),
			GR:        byTagGR, GRTF: byTagGRtf,
			ES: byTagES, ESTF: byTagEStf,
			ESDesc: "In Elasticsearch the bars are named by tag; the repository is the next column.",
		}),
		panel("table", "Release assets", box{W: 24, H: 9, X: 0, Y: 8}, []Target{sqlT(assets)}, &P{
			PromNote: cannot("every release asset with its download count and size, forty "+
				"at a time by downloads.",
				"The exporter skips `gh_release_asset`: one series per file ever "+
					"published (1,216 on the account this was measured on) for "+
					"counts the per-release gauge already carries, in the chart above."),
			Opts: Opts{"sort": "Downloads"},
			Desc: "Download fetches the file itself, which is what an asset's url is; the " +
				"asset's name opens the release page it was published on.",
			Overrides: []any{
				width("Asset", 200), unitOf("Size", "bytes", 120),
				barCell("Downloads", "short", 120), downloadColumn(), linkOn("Asset"),
			},
			GR: assetsGR, GRTF: assetsGRtf,
			GRDesc: "Graphite names each row repository, tag and asset from the path. " + grRows,
			ES:     assetsES, ESTF: assetsEStf,
		}),
		panel("table", "Downloads gained", box{W: 24, H: 8, X: 0, Y: 17}, []Target{sqlT(
			`SELECT asset AS "Asset", MAX(downloads) - MIN(downloads) AS "Gained",` +
				` MAX(downloads) AS "Total", tag AS "Tag", repo AS "Repository",` +
				` MAX(url) AS "Download"` +
				" FROM gh_release_asset WHERE $__timeFilter(time) AND " + RF +
				" GROUP BY 1, 4, 5 HAVING MAX(downloads) > MIN(downloads)" +
				" ORDER BY 2 DESC LIMIT 25",
		)}, &P{
			PromNote: cannot("what each release asset gained across the range, as the "+
				"difference of a cumulative counter.",
				"The exporter skips gh_release_asset: it is one series per file "+
					"ever published, 1,216 of them on the measured account, which "+
					"would be four fifths of everything it serves. The per-release "+
					"totals it does keep are in the panel above."),
			Opts: Opts{"sort": "Gained"},
			Desc: "The counter is cumulative, so what matters is the difference across the " +
				"range. A day of it here: ninety seven per cent of the downloads are one " +
				"Linux binary and its checksum file, which is an installer rather than a " +
				"person.",
			Overrides: []any{
				barCell("Gained", "short", 120), width("Asset", 200),
				width("Tag", 110), downloadColumn(),
			},
			GR: gainedGR, GRTF: gainedGRtf,
			GRDesc: "Graphite names each row repository, tag and asset from the path, and " +
				"differentiates the series itself. " + grRows,
			ESNote: cannot("the downloads each asset gained across the range, as the difference "+
				"between the first and last value of a cumulative counter.",
				"The Elasticsearch datasource has no derivative across a terms "+
					"bucket in Grafana's query builder: max minus min per asset needs a "+
					"bucket script it does not expose.", "elasticsearch"),
		}),
	}
}
