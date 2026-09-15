package dashboards

import (
	"fmt"
	"strings"
)

// ── Inventory ───────────────────────────────────────────────────────────────

// How the stores name what they hand back, which these sections address rather
// than spell out: Elasticsearch keeps a tag as a keyword sub-field, and Grafana
// names the value column of a table query after the query's own ref id, so a
// rename asks for "Value #A" and the panels that build their queries in a loop
// append the ref themselves. The sibling sections address the same three names.
const (
	inventoryRepoTerm = "repo.keyword"
	inventoryURLTerm  = "url.keyword"
	inventoryValueCol = "Value #"
)

// inventoryKeepLast carries a snapshot forward in Graphite. These measurements
// have a point only where a sweep wrote one, and a gap between two sweeps is
// not the repository losing the setting.
const inventoryKeepLast = "keepLastValue("

// What the columns of this section are called wherever they are read: the
// community score and its two template counts, the four repository settings,
// and the age of a key. Each one has to carry the same name in all five
// stores, since the overrides and the sorts below match a column by it.
const (
	inventoryCommunityScore  = "Community profile"
	inventoryIssueTemplates  = "Issue templates"
	inventoryPRTemplate      = "PR template"
	inventorySecurityPolicy  = "Security policy"
	inventoryDeleteOnMerge   = "Delete on merge"
	inventoryAutoMerge       = "Auto merge"
	inventoryProtectionRules = "Protection rules"
	inventoryKeyIdle         = "Days since use"
)

// inventory is the standing list of what the account holds: first the
// repositories and what they publish, then how they are configured, and last
// the files that configuration lives in and the dependency graph they govern.
func inventory(b *builder) []Panel {
	out := repositoryList(b)
	out = append(out, whatTheyPublish(b)...)
	out = append(out, settingsAndKeys(b)...)
	return append(out, policyAndDependencies(b)...)
}

// repositoryList is the repositories themselves: what they are written in,
// how complete their community files are, and one row each with the numbers
// GitHub keeps on them.
func repositoryList(b *builder) []Panel {
	langs := `SELECT language AS "Language", SUM(bytes) AS "Bytes" FROM (` +
		"SELECT repo, language, bytes, ROW_NUMBER() OVER (PARTITION BY repo, language" +
		" ORDER BY time DESC) AS rn FROM gh_repo_language WHERE $__timeFilter(time) AND " + RF +
		") x WHERE rn = 1 GROUP BY 1 ORDER BY 2 DESC LIMIT 12"
	repoFields := []string{
		"language", "stars", "forks", "network", "open_issues", "watchers",
		"size_kb", "age_days", "days_since_push", "visibility", "license", "url",
	}
	repos := `SELECT repo AS "Repository", stars AS "Stars", language AS "Language",` +
		` forks AS "Forks", network AS "Network", open_issues AS "Open issues",` +
		` watchers AS "Watchers",` +
		` size_kb AS "Size", age_days AS "Age", days_since_push AS "Idle",` +
		` visibility AS "Visibility", license AS "License",` +
		` url AS "Link"` +
		" FROM (" + latestPerRepo(repoFields) + ") ORDER BY stars DESC"
	// The six boxes the score is made of, which are stored and were shown
	// nowhere: eight of thirty-five repositories have no license, which the
	// percentage alone does not say. Five of them are GitHub's own flags.
	// The sixth is not: GitHub's community profile, like its GraphQL
	// issueTemplates, counts Markdown issue templates only and not YAML
	// issue forms, so it said "no" for every repository of an account whose
	// repositories carry four forms each. The count of templates a
	// repository actually has, forms and Markdown, is what the totals family
	// writes to gh_repo_policy.issue_templates, so the two SQL stores and
	// Prometheus take that column and join it per repository to the profile
	// row. Graphite and Elasticsearch cannot join two measurements in one
	// panel and keep GitHub's flag, under the flag's own name, saying so.
	healthFiles := []string{"readme", "license", "contributing", "code_of_conduct"}
	healthNames := []string{"Readme", "License", "Contributing", "Conduct"}
	healthCols := make([]string, len(healthFiles))
	hasFields := make([]string, len(healthFiles))
	for i, f := range healthFiles {
		healthCols[i] = fmt.Sprintf(`CAST(c.has_%s AS INT) AS %q`, f, healthNames[i])
		hasFields[i] = "has_" + f
	}
	health := `SELECT c.repo AS "Repository", c.health_percentage AS "Community profile", ` +
		strings.Join(healthCols, ", ") + `, p.issue_templates AS "Issue templates",` +
		` CAST(c.has_pull_request_template AS INT) AS "PR template", c.url AS "Link"` +
		" FROM (SELECT repo, health_percentage, url, " + strings.Join(hasFields, ", ") +
		", has_pull_request_template, ROW_NUMBER() OVER (PARTITION BY repo" +
		" ORDER BY time DESC) AS rn FROM gh_repo_community WHERE $__timeFilter(time) AND " + RF +
		") c LEFT JOIN (SELECT repo, issue_templates, ROW_NUMBER() OVER (PARTITION BY repo" +
		" ORDER BY time DESC) AS rn FROM gh_repo_policy WHERE $__timeFilter(time) AND " + RF +
		") p ON p.repo = c.repo AND p.rn = 1 WHERE c.rn = 1 ORDER BY 2 DESC"
	repoBy := "repo, language, visibility, license"
	repoCols := []named{
		{"stars", "Stars"},
		{"forks", "Forks"},
		{"network", "Network"},
		{"open_issues", "Open issues"},
		{"watchers", "Watchers"},
		{"size_kb", "Size"},
		{"age_days", "Age"},
		{"days_since_push", "Idle"},
	}
	rl, rc := "gh_repo_language", "gh_repo_community"

	langsGR, langsGRtf := gTbl(fmt.Sprintf(
		`limit(sortByMaxima(groupByNode(keepLastValue(%s), %d, "sum")), 12)`,
		rp(rl, "bytes"), gn(rl, "language"),
	), "Language", []col{{"lastNotNull", "Bytes"}})
	langsES, langsEStf := esTbl(rl, []any{b.tm("language", 12), b.tm("repo", 500)},
		[]any{b.mNewest("bytes")},
		[]named{{"language.keyword", "Language"}, {inventoryRepoTerm, "Repository"}, {"b", "Bytes"}},
		[]string{ESF}, groupSum("Language", "Bytes", "Language", "Bytes")...)

	var healthProm []Target
	healthProm = append(healthProm, promTbl(fmt.Sprintf(
		"sum by (repo) (github_repo_community_health_percentage{%s})", PF,
	), "A"))
	healthRename := map[string]string{"repo": "Repository", inventoryValueCol + "A": inventoryCommunityScore}
	for i, f := range healthFiles {
		ref := string(rune('B' + i))
		healthProm = append(healthProm, promTbl(fmt.Sprintf(
			"max by (repo) (github_repo_community_has_%s{%s})", f, PF,
		), ref))
		healthRename[inventoryValueCol+ref] = healthNames[i]
	}
	// The join Prometheus makes: the merge transformation lines the two
	// families up on the repo label, so the count sits beside the flags.
	healthProm = append(healthProm,
		promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_issue_templates{%s})", PF), "F"),
		promTbl(fmt.Sprintf("max by (repo) (github_repo_community_has_pull_request_template{%s})", PF), "G"),
	)
	healthRename[inventoryValueCol+"F"] = inventoryIssueTemplates
	healthRename[inventoryValueCol+"G"] = inventoryPRTemplate
	healthOver := []any{barCell(inventoryCommunityScore, "percent", 120)}
	for _, n := range healthNames {
		healthOver = append(healthOver, profileBool(n, 90))
	}
	// The count where a store can join it, the flag where it cannot: each
	// override matches its own column by name and is inert on the other.
	healthOver = append(healthOver, width(inventoryIssueTemplates, 135),
		profileBool("Issue template", 110), profileBool(inventoryPRTemplate, 100), linkOn("Repository"))
	healthGR, healthGRtf := gTbl(rowsOf(rp(rc, "health_percentage"), gn(rc, "repo")),
		"Repository", []col{{"lastNotNull", inventoryCommunityScore}})
	// One `max` per column and no top_metrics anywhere, which is what the six
	// boxes cost here. The sink writes them as booleans, Elasticsearch maps
	// them as `boolean`, and a top_metrics hands a boolean back as the string
	// "true": Grafana's Elasticsearch plugin converts a top_metrics value to a
	// float without checking the type, so the panel returned nothing but
	// `interface {} is string, not float64` from the plugin. A `max` over the
	// same field is answered as a number, 1 or 0, which is the value the SQL
	// panel casts to and the value the exporter publishes, so the columns
	// carry the same two numbers everywhere. What it costs here is the reading
	// itself, and this panel is the one place where that is a real difference:
	// the SQL twin takes the newest row per repository and `max` takes the
	// largest value inside the range, so the description says so.
	healthESMetrics := []any{b.mMax("health_percentage")}
	healthESNames := []named{
		{inventoryRepoTerm, "Repository"},
		{"health_percentage", inventoryCommunityScore},
	}
	for i, f := range hasFields {
		healthESMetrics = append(healthESMetrics, b.mMax(f))
		healthESNames = append(healthESNames, named{f, healthNames[i]})
	}
	healthESMetrics = append(healthESMetrics, b.mMax("has_issue_template"), b.mMax("has_pull_request_template"))
	healthESNames = append(healthESNames, named{"has_issue_template", "Issue template"},
		named{"has_pull_request_template", inventoryPRTemplate}, named{inventoryURLTerm, "Link"})
	healthES, healthEStf := esTbl(rc, []any{b.tm("repo", 500), b.tmURL()}, healthESMetrics,
		healthESNames, []string{ESF})

	var reposProm []Target
	reposRename := map[string]string{
		"repo": "Repository", "language": "Language",
		"visibility": "Visibility", "license": "License",
	}
	reposOrder := map[string]int{"repo": 0, "language": 1}
	for i, c := range repoCols {
		ref := string(rune('A' + i))
		reposProm = append(reposProm, promTbl(fmt.Sprintf(
			"sum by (%s) (github_repo_%s{%s})", repoBy, c.From, PF,
		), ref))
		reposRename[inventoryValueCol+ref] = c.To
		reposOrder[inventoryValueCol+ref] = 2 + i
	}
	reposOrder["visibility"] = 2 + len(repoCols)
	reposOrder["license"] = 3 + len(repoCols)
	repoFieldNames := make([]string, len(repoCols))
	for i, c := range repoCols {
		repoFieldNames[i] = c.From
	}
	reposGR, reposGRtf := gTbl(rowsOf(rp("gh_repo", "stars"), gn("gh_repo", "repo"),
		gn("gh_repo", "language"), gn("gh_repo", "visibility"), gn("gh_repo", "license")),
		"Repository, language, visibility, license", []col{{"lastNotNull", "Stars"}})
	reposES, reposEStf := esTbl("gh_repo", []any{
		b.tm("repo", 500), b.tm("language", 1),
		b.tm("visibility", 1), b.tm("license", 1), b.tmURL(),
	}, []any{b.mNewest(repoFieldNames...)},
		append([]named{
			{inventoryRepoTerm, "Repository"},
			{"language.keyword", "Language"},
			{"visibility.keyword", "Visibility"},
			{"license.keyword", "License"},
			{inventoryURLTerm, "Link"},
		}, repoCols...),
		[]string{ESF})

	return []Panel{
		panel("barchart", "Code by language", box{W: 12, H: 8, X: 0, Y: 0}, []Target{sqlT(langs)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"topk(12, sum by (language) (github_repo_language_bytes{%s}))", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"language": "Language", "Value": "Bytes",
			}, nil, nil)},
			Opts: Opts{"unit": "bytes"},
			GR:   langsGR, GRTF: langsGRtf,
			ES: langsES, ESTF: langsEStf,
		}),
		panel("table", inventoryCommunityScore, box{W: 12, H: 8, X: 12, Y: 0}, []Target{sqlT(health)}, &P{
			Prom:      healthProm,
			PromTF:    merged(healthRename, nil, map[string]int{"repo": 0}),
			Opts:      Opts{"sort": inventoryCommunityScore},
			Overrides: healthOver,
			// Shared by the five stores, so it names the column only where it
			// exists: Graphite has no such column and Elasticsearch has the flag.
			Desc: "GitHub's own score, and the boxes it is made of. The score alone hides " +
				"which one is missing: here eight of thirty-five repositories have no " +
				"license. GitHub's page and score do count a templates directory, but the " +
				"API's issue_template flag reports only the legacy single file, so it " +
				"reads false on a repository that scores 100 with four issue forms: on " +
				"this account seven repositories carry templates and the flag is false " +
				"on all of them (community/community#207706). In the InfluxDB, " +
				"PostgreSQL and Prometheus dashboards, which can join two measurements, " +
				"Issue templates is how many the repository actually has, forms and " +
				"Markdown, from gh_repo_policy.",
			GR: healthGR, GRTF: healthGRtf,
			GRDesc: "Graphite has the percentage; the boxes are not metrics there.",
			ES:     healthES, ESTF: healthEStf,
			ESDesc: "Elasticsearch answers every column with the largest reading inside the " +
				"range rather than with the newest one: it keeps the six boxes as booleans, " +
				"and the aggregation that would take the newest reading returns a boolean " +
				"as text, which the datasource cannot render. Issue template here is " +
				"the API's own flag, which reports only the legacy single file and not a " +
				"templates directory the page and the score count: " +
				"one panel cannot join the count in gh_repo_policy to this row.",
		}),
		panel("table", "Repositories", box{W: 24, H: 11, X: 0, Y: 8}, []Target{sqlT(repos)}, &P{
			Prom:   reposProm,
			PromTF: merged(reposRename, nil, reposOrder),
			Opts:   Opts{"sort": "Stars"},
			Overrides: []any{
				repoColumn(), width("Language", 110),
				width("Stars", 90), width("Forks", 80), width("Network", 90),
				width("Open issues", 105), width("Watchers", 95),
				unitOf("Size", "deckbytes", 95), unitOf("Age", "d", 80),
				unitOf("Idle", "d", 80), width("Visibility", 100),
				width("License", 100), linkOn("Repository"),
			},
			Desc: "Age is how long since the repository was created, idle how long since the " +
				"last push. Network is the whole fork tree, which the fork count of a " +
				"repository does not include: a fork of a fork is in one and not the other.",
			GR: reposGR, GRTF: reposGRtf,
			GRDesc: "Graphite names each row from the path. " + grRows,
			ES:     reposES, ESTF: reposEStf,
		}),
	}
}

// whatTheyPublish is everything the account puts out under those
// repositories: the topics they are filed under, the packages and gists, and
// the container tags each release pushed.
func whatTheyPublish(b *builder) []Panel {
	topics := `SELECT topic AS "Topic", COUNT(DISTINCT repo) AS "Repositories",` +
		` url AS "Link"` +
		" FROM gh_repo_topic WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 30"
	packages := `SELECT package AS "Package", versions AS "Versions", type AS "Type",` +
		` tagged_versions AS "Tagged",` +
		` days_since_update AS "Idle", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY package, type ORDER BY time DESC) AS rn" +
		" FROM gh_package WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 2 DESC"
	gists := `SELECT gist AS "Gist", description AS "Description", files AS "Files",` +
		` comments AS "Comments", size_bytes AS "Size",` +
		` days_since_update AS "Idle", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY gist ORDER BY time DESC) AS rn" +
		" FROM gh_gist WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 6 LIMIT 25"
	tags := `SELECT package AS "Package", time AS "Published", tag AS "Tag",` +
		` url AS "Link"` +
		" FROM gh_package_version WHERE $__timeFilter(time) ORDER BY time DESC LIMIT 30"
	rt, pk, gs, pv := "gh_repo_topic", "gh_package", "gh_gist", "gh_package_version"

	topicsGR, topicsGRtf := gTbl(fmt.Sprintf(
		`limit(sortByMaxima(groupByNode(keepLastValue(%s), %d, "sum")), 30)`,
		rp(rt, "present"), gn(rt, "topic"),
	), "Topic", []col{{"lastNotNull", "Repositories"}})
	topicsES, topicsEStf := esTbl(rt, []any{b.tm("topic", 30, "1"), b.tmURL()}, []any{b.mUniq("repo")},
		[]named{{"topic.keyword", "Topic"}, {inventoryURLTerm, "Link"}, {"r", "Repositories"}}, []string{ESF})

	pkgGR, pkgGRtf := gTbl(rowsOf(gp(pk, "versions"), gn(pk, "package"), gn(pk, "type")),
		"Package", []col{{"lastNotNull", "Versions"}})
	pkgES, pkgEStf := esTbl(pk, []any{b.tm("package", 100), b.tm("type", 10), b.tmURL()},
		[]any{b.mNewest("versions", "tagged_versions", "days_since_update")},
		[]named{
			{"package.keyword", "Package"},
			{"type.keyword", "Type"},
			{inventoryURLTerm, "Link"},
			{"versions", "Versions"},
			{"tagged_versions", "Tagged"},
			{"days_since_update", "Idle"},
		}, nil)

	gistGR, gistGRtf := gTbl(rowsOf(gp(gs, "files"), gn(gs, "gist")), "Gist",
		[]col{{"lastNotNull", "Files"}})
	gistES, gistEStf := esTbl(gs, []any{b.tm("gist", 25), b.tmURL()},
		[]any{b.mNewest("files", "comments", "size_bytes", "days_since_update")},
		[]named{
			{"gist.keyword", "Gist"},
			{inventoryURLTerm, "Link"},
			{"files", "Files"},
			{"comments", "Comments"},
			{"size_bytes", "Size"},
			{"days_since_update", "Idle"},
		}, nil)

	tagsGR, tagsGRtf := gTbl(fmt.Sprintf(`groupByNode(isNonNull(%s), %d, "sum")`,
		gp(pv, "published"), gn(pv, "package")), "Package",
		[]col{{"sum", "Tags published"}})
	tagsES, tagsEStf := b.esRaw(pv, 30, []named{
		{"@timestamp", "Published"},
		{"package", "Package"},
		{"tag", "Tag"},
		{"url", "Link"},
	}, nil)

	return []Panel{
		panel("table", "Topics", box{W: 6, H: 8, X: 0, Y: 19}, []Target{sqlT(topics)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"count by (topic) (github_repo_topic_present{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"topic": "Topic", "Value": "Repositories",
			}, nil, nil)},
			Opts:      Opts{"sort": "Repositories"},
			Overrides: []any{width("Repositories", 120), linkOn("Topic")},
			GR:        topicsGR, GRTF: topicsGRtf,
			ES: topicsES, ESTF: topicsEStf,
		}),
		panel("table", "Packages", box{W: 9, H: 8, X: 6, Y: 19}, []Target{sqlT(packages)}, &P{
			Prom: []Target{
				promTbl("sum by (package, type) (github_package_versions)", "A"),
				promTbl("sum by (package, type) (github_package_tagged_versions)", "B"),
				promTbl("sum by (package, type) (github_package_days_since_update)", "C"),
			},
			PromTF: merged(map[string]string{
				"package": "Package", "type": "Type", inventoryValueCol + "A": "Versions",
				inventoryValueCol + "B": "Tagged", inventoryValueCol + "C": "Idle",
			}, nil, nil),
			Opts: Opts{"sort": "Versions"},
			Desc: "Version counts come from walking the version list: the documented " +
				"version_count field arrives as zero for a personal account's packages.",
			Overrides: []any{
				width("Type", 100), width("Versions", 90), width("Tagged", 80),
				unitOf("Idle", "d", 70), linkOn("Package"),
			},
			GR: pkgGR, GRTF: pkgGRtf,
			GRDesc: "Graphite names each row package and type from the path. " + grRows,
			ES:     pkgES, ESTF: pkgEStf,
		}),
		panel("table", "Gists", box{W: 9, H: 8, X: 15, Y: 19}, []Target{sqlT(gists)}, &P{
			Prom: []Target{
				promTbl("sum by (gist) (github_gist_files)", "A"),
				promTbl("sum by (gist) (github_gist_comments)", "B"),
				promTbl("sum by (gist) (github_gist_size_bytes)", "C"),
				promTbl("sum by (gist) (github_gist_days_since_update)", "D"),
			},
			PromTF: merged(map[string]string{
				"gist": "Gist", inventoryValueCol + "A": "Files", inventoryValueCol + "B": "Comments",
				inventoryValueCol + "C": "Size", inventoryValueCol + "D": "Idle",
			}, nil, nil),
			PromDesc: "Prometheus carries no description: it is text.",
			Overrides: []any{
				width("Gist", 100), width("Description", 190), width("Files", 65),
				width("Comments", 85), unitOf("Size", "bytes", 80),
				unitOf("Idle", "d", 65), linkOn("Gist"),
			},
			PromOver: []any{width("Gist", 300)},
			GR:       gistGR, GRTF: gistGRtf,
			GRDesc: "Graphite carries no description: it is text. " + grRows,
			GROver: []any{width("Gist", 300)},
			ES:     gistES, ESTF: gistEStf,
			ESDesc: "Elasticsearch keeps the description as text, which a bucket cannot show.",
			ESOver: []any{width("Gist", 300)},
		}),
		panel("table", "Container tags published", box{W: 24, H: 8, X: 0, Y: 27}, []Target{sqlT(tags)}, &P{
			PromNote: cannot("every tagged package version, dated when it was published.",
				"The exporter skips `gh_package_version`: a publication date is "+
					"history, and the count of versions is already a field on "+
					"`gh_package`, shown in the Packages table."),
			Desc: "Every tagged package version, dated when it was published. Untagged versions " +
				"are counted in the table above but not listed: a build that leaves fifty " +
				"untagged layers would bury the releases nobody can name.",
			Overrides: []any{
				when("Published"), width("Package", 200),
				linkOn("Package"),
			},
			GR: tagsGR, GRTF: tagsGRtf,
			GRDesc: "Graphite has no way to list by date, so this counts the tags published per " +
				"package over the range.",
			GROver: []any{barCell("Tags published", "short", 200)},
			ES:     tagsES, ESTF: tagsEStf,
		}),
	}
}

// settingsAndKeys is how it is all configured: the per-repository policy, the
// keys on the account, the licenses the dependencies carry, the social
// accounts on the profile, and whatever changed inside the range.
func settingsAndKeys(b *builder) []Panel {
	polGR, polGRtf := gTbl(rowsOf(inventoryKeepLast+rp("gh_repo_policy", "branch_protection_rules")+")",
		gn("gh_repo_policy", "repo")), "Repository", []col{{"lastNotNull", inventoryProtectionRules}})
	// A `max` per column for the same reason the community profile above has
	// one: the first three of these are booleans, and a top_metrics reads a
	// boolean back as the string "true", which panics Grafana's Elasticsearch
	// plugin and takes the whole panel with it. Here `max` costs nothing at
	// all, which is why this panel carries no note about Elasticsearch: the
	// SQL twin below reduces the same range with MAX() and Prometheus asks
	// `max by (repo)`, so every dashboard that carries these six columns
	// answers them with the same numbers.
	polES, polEStf := esTbl("gh_repo_policy", []any{b.tm("repo", 500), b.tmURL()},
		[]any{
			b.mMax("security_policy"), b.mMax("delete_branch_on_merge"), b.mMax("auto_merge"),
			b.mMax("branch_protection_rules"), b.mMax("issue_templates"),
			b.mMax("codeowners_errors"),
		},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{inventoryURLTerm, "Link"},
			{"security_policy", inventorySecurityPolicy},
			{"delete_branch_on_merge", inventoryDeleteOnMerge},
			{"auto_merge", inventoryAutoMerge},
			{"branch_protection_rules", inventoryProtectionRules},
			{"issue_templates", inventoryIssueTemplates},
			{"codeowners_errors", "CODEOWNERS errors"},
		}, []string{ESF})

	keysGR, keysGRtf := gTbl(rowsOf(inventoryKeepLast+gp("gh_key", "days_since_use")+")",
		gn("gh_key", "key"), gn("gh_key", "kind")), "Key, kind",
		[]col{{"lastNotNull", inventoryKeyIdle}})
	keysES, keysEStf := esTbl("gh_key", []any{b.tm("key", 50), b.tm("kind", 5), b.tmURL()},
		[]any{b.mNewest("days_since_use", "never_used", "days_to_expiry")},
		[]named{
			{"key.keyword", "Key"},
			{"kind.keyword", "Kind"},
			{inventoryURLTerm, "Link"},
			{"days_since_use", inventoryKeyIdle},
			{"never_used", "Never used"},
			{"days_to_expiry", "Days to expiry"},
		}, nil)

	licGR, licGRtf := gTbl(fmt.Sprintf(
		`sortByMaxima(groupByNode(keepLastValue(%s), %d, "sum"))`,
		rp("gh_dependency_license", "packages"), gn("gh_dependency_license", "license"),
	),
		"License", []col{{"lastNotNull", "Packages"}})
	licES, licEStf := esTbl("gh_dependency_license", []any{b.tm("license", 12), b.tm("repo", 500)},
		[]any{b.mNewest("packages")},
		[]named{{"license.keyword", "License"}, {"packages", "Packages"}}, []string{ESF},
		groupSum("License", "Packages", "License", "Packages")...)

	socGR, socGRtf := gTbl(rowsOf(inventoryKeepLast+gp("gh_social_account", "present")+")",
		gn("gh_social_account", "provider")), "Provider", []col{{"lastNotNull", "Present"}})
	// The url as a bucket and never as a metric. An end to end run against a
	// real cluster found this panel returning nothing: `url` is a plain
	// string, dynamic mapping makes it `text`, and aggregating text needs
	// fielddata, which is off. Its keyword sub-field does carry fielddata, but
	// asking top_metrics for it only moves the failure, because Grafana's
	// Elasticsearch plugin panics on a top_metrics whose value is not a
	// number: `interface conversion: interface {} is string, not float64`.
	// A terms bucket has neither problem, and a provider has exactly one url,
	// so bucketing by it costs no rows. The metric is the numeric field the
	// measurement carries, which is what Prometheus shows too.
	socES, socEStf := esTbl("gh_social_account",
		[]any{b.tm("provider", 20), b.tm("url", 20)},
		[]any{b.mMax("present")},
		[]named{
			{"provider.keyword", "Provider"},
			{inventoryURLTerm, "URL"},
			{"m", "Present"},
		}, nil)

	return []Panel{
		panel("table", "Repository settings", box{W: 24, H: 8, X: 0, Y: 35}, []Target{sqlT(
			`SELECT repo AS "Repository",` +
				` MAX(CAST(security_policy AS INT)) AS "Security policy",` +
				` MAX(CAST(delete_branch_on_merge AS INT)) AS "Delete on merge",` +
				` MAX(CAST(auto_merge AS INT)) AS "Auto merge",` +
				` MAX(branch_protection_rules) AS "Protection rules",` +
				` MAX(issue_templates) AS "Issue templates",` +
				` MAX(codeowners_errors) AS "CODEOWNERS errors",` +
				` url AS "Link"` +
				" FROM gh_repo_policy WHERE $__timeFilter(time) AND " + RF +
				" GROUP BY 1, 8 ORDER BY 1",
		)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_security_policy{%s})", PF), "A"),
				promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_delete_branch_on_merge{%s})", PF), "B"),
				promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_auto_merge{%s})", PF), "C"),
				promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_branch_protection_rules{%s})", PF), "D"),
				promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_issue_templates{%s})", PF), "E"),
				promTbl(fmt.Sprintf("max by (repo) (github_repo_policy_codeowners_errors{%s})", PF), "F"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", inventoryValueCol + "A": inventorySecurityPolicy,
				inventoryValueCol + "B": inventoryDeleteOnMerge, inventoryValueCol + "C": inventoryAutoMerge,
				inventoryValueCol + "D": inventoryProtectionRules, inventoryValueCol + "E": inventoryIssueTemplates,
				inventoryValueCol + "F": "CODEOWNERS errors",
			}, []string{"owner", "full_name", "instance", "job", "__name__"},
				map[string]int{"repo": 0}),
			Desc: "What each repository allows, in the batch that already costs one point of " +
				"GraphQL. CODEOWNERS errors is the one that fails silently: a broken file " +
				"stops requesting reviews and says nothing.",
			Overrides: []any{
				profileBool(inventorySecurityPolicy, 120), profileBool(inventoryDeleteOnMerge, 120),
				profileBool(inventoryAutoMerge, 100), ownerLinkOn("Repository", "the repository settings"),
			},
			GR: polGR, GRTF: polGRtf, GRDesc: grSlot,
			ES: polES, ESTF: polEStf,
		}),
		panel("table", "Account keys", box{W: 12, H: 7, X: 0, Y: 43}, []Target{sqlT(
			`SELECT key AS "Key", kind AS "Kind",` +
				` MIN(days_since_use) AS "Days since use",` +
				` MAX(never_used) AS "Never used",` +
				` MIN(days_to_expiry) AS "Days to expiry", MAX(url) AS "Link"` +
				" FROM gh_key WHERE $__timeFilter(time) GROUP BY 1, 2 ORDER BY 2, 1",
		)}, &P{
			Prom: []Target{
				promTbl("min by (key, kind) (github_key_days_since_use)", "A"),
				promTbl("max by (key, kind) (github_key_never_used)", "B"),
				promTbl("min by (key, kind) (github_key_days_to_expiry)", "C"),
			},
			PromTF: merged(map[string]string{
				"key": "Key", "kind": "Kind", inventoryValueCol + "A": inventoryKeyIdle,
				inventoryValueCol + "B": "Never used", inventoryValueCol + "C": "Days to expiry",
			}, nil, map[string]int{"key": 0, "kind": 1}),
			Desc: "The keys that sign and open everything. Two of the SSH keys here have never " +
				"been used at all, and the expiry of the GPG key is the kind of date nobody " +
				"remembers until the signatures stop verifying. All of them are managed in " +
				"one place, the keys settings, which is where every row links.",
			Overrides: []any{ownerLinkOn("Key", "the keys settings")},
			GR:        keysGR, GRTF: keysGRtf, GRDesc: grSlot,
			ES: keysES, ESTF: keysEStf,
		}),
		// Eight bars and the rest folded: a license name can be a whole SPDX
		// expression, and twelve of them in seven units of height were cut off.
		panel("barchart", "Dependencies by license", box{W: 12, H: 7, X: 12, Y: 43}, []Target{sqlT(
			otherRows(`SELECT license AS "License", SUM(packages) AS "Packages",`+
				" ROW_NUMBER() OVER (ORDER BY SUM(packages) DESC) AS rn FROM ("+
				"SELECT license, packages, ROW_NUMBER() OVER (PARTITION BY repo, license"+
				" ORDER BY time DESC) AS rn FROM gh_dependency_license"+
				ciInRange+RF+") x WHERE rn = 1"+
				" GROUP BY 1", "License", "Packages", 8),
		)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"topk(8, sum by (license) (github_dependency_license_packages{%s}))", PF,
			))},
			PromDesc: "Prometheus shows the eight and folds nothing.",
			PromTF: []any{organize(map[string]string{
				"license": "License", "Value": "Packages",
			}, nil, nil)},
			Desc: "Every package the dependency graph knows about, by license. Undetermined is " +
				"GitHub saying it could not tell, which is a blind spot rather than a license. " +
				"Off by default: the SBOM is a megabyte or two per repository. The eight " +
				"commonest licenses are named; the rest are one bar called other.",
			GR: licGR, GRTF: licGRtf,
			ES: licES, ESTF: licEStf,
		}),
		panel("table", "Social accounts", box{W: 12, H: 7, X: 0, Y: 50}, []Target{sqlT(
			`SELECT provider AS "Provider", url AS "URL" FROM (` +
				"SELECT provider, url, ROW_NUMBER() OVER (PARTITION BY provider" +
				" ORDER BY time DESC) AS rn FROM gh_social_account" +
				" WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 1",
		)}, &P{
			Prom: []Target{promTbl("max by (provider) (github_social_account_present)")},
			PromTF: []any{organize(map[string]string{
				"provider": "Provider", "Value": "Present",
			}, []string{"user", "instance", "job", "__name__"}, nil)},
			Desc: "What the profile links to. It is small and it is a check rather than a " +
				"measurement: these are the same links a personal site publishes as sameAs, " +
				"and a row that disappears is the signal.",
			PromDesc:  "The exporter keeps the provider and whether it is present.",
			Overrides: []any{width("Provider", 140), rawURLColumn("URL")},
			GR:        socGR, GRTF: socGRtf,
			GRDesc: "Graphite has no strings either, so this is presence by provider.",
			ES:     socES, ESTF: socEStf,
		}),
		panel("table", "Configuration changes", box{W: 12, H: 7, X: 12, Y: 50}, []Target{sqlT(
			`SELECT repo AS "Repository",` +
				` COUNT(DISTINCT visibility) AS "Visibility",` +
				` COUNT(DISTINCT archived) AS "Archived",` +
				` COUNT(DISTINCT default_branch) AS "Default branch",` +
				` COUNT(DISTINCT license) AS "License",` +
				` COUNT(DISTINCT language) AS "Language",` +
				` COUNT(DISTINCT repo_id) AS "Identity", MAX(url) AS "Link"` +
				" FROM gh_repo WHERE $__timeFilter(time) AND " + RF + " GROUP BY 1" +
				" HAVING COUNT(DISTINCT visibility) > 1 OR COUNT(DISTINCT archived) > 1" +
				" OR COUNT(DISTINCT default_branch) > 1 OR COUNT(DISTINCT license) > 1" +
				" OR COUNT(DISTINCT language) > 1 ORDER BY 1",
		)}, &P{
			PromNote: cannot("the repositories whose visibility, archived flag, default branch, "+
				"license or main language changed inside the range, counted as "+
				"distinct values per repository.",
				"Those are labels on a gauge, and a change of label is a new "+
					"series rather than a value: counting distinct label values over a "+
					"range is `count(count by (visibility) (...))`, which cannot be "+
					"grouped per repository and per label at once."),
			GRNote: cannot("the repositories whose configuration changed inside the range.",
				"Each combination is its own path in Graphite, and counting how many "+
					"paths a repository has is not something the function list does.",
				"graphite"),
			ESNote: cannot("the repositories whose configuration changed inside the range.",
				"A cardinality aggregation counts distinct values of one field; six "+
					"of them per repository, with a filter on the result, is beyond what "+
					"the datasource builds.", "elasticsearch"),
			Desc: "The repositories whose configuration changed inside the range. Empty almost " +
				"always, and that is the point: No data means nothing changed. A row here " +
				"means a repository was " +
				"renamed, archived, made private, relicensed or had its default branch moved. " +
				"Identity counts distinct repo_id values, which is how a rename is told from a " +
				"new repository, since GitHub publishes no rename history at all.",
			Overrides: []any{repoColumn(), linkOn("Repository")},
		}),
	}
}

// policyAndDependencies is the configuration files a repository carries and
// the dependency graph they govern.
//
// The dating rule differs inside this one group, which is the whole risk of
// reading it:
//
//   - gh_policy_file, and gh_dependabot_ecosystem with it, is dated at the last
//     commit that touched the path, or at the deletion for a file that is gone,
//     and only a path that never existed is stamped at the start of the day. So
//     a narrow range hides every file nobody has touched lately, and the two
//     tables read the whole history instead of $__timeFilter. The stores that
//     cannot do that say so.
//   - gh_dependency is a daily snapshot, read the way the license histogram
//     beside it is: newest row per repository and ecosystem, and only then a
//     sum.
//   - gh_dependency_change is stamped `now` and is not a snapshot. Each row
//     covers a real commit range, from the previous sweep's head to this one,
//     so consecutive rows never overlap and summing them over the range is the
//     honest reading. A newest-row-per-series subquery there would throw away
//     every earlier bump. A sweep with nothing to report writes one row with
//     both counters at zero under a `change` of (none), which is what makes
//     the table exist before the first bump; every store leaves that series
//     out, because a bar of zeros is a legend entry and nothing else.
func policyAndDependencies(b *builder) []Panel {
	pf, de := "gh_policy_file", "gh_dependabot_ecosystem"
	dep, dc := "gh_dependency", "gh_dependency_change"

	// The whole history rather than the dashboard range, for the reason above.
	// Thirty years is what "Contributions by year" uses for the same job: it
	// outlives any account and stays one expression both SQL dialects plan.
	everSince := "WHERE " + wholeHistory + " AND " + RF
	policy := `SELECT repo AS "Repository", file AS "File",` +
		` CAST(present AS INT) AS "Present", path AS "Path",` +
		` changes AS "Changes", url AS "Link" FROM (` +
		"SELECT repo, file, present, path, changes, url," +
		" ROW_NUMBER() OVER (PARTITION BY repo, file ORDER BY time DESC) AS rn" +
		// The missing files first: ordered by repository the table showed six
		// rows of whichever repository sorts first, out of 228.
		" FROM " + pf + " " + everSince + ") x WHERE rn = 1 ORDER BY 3, 1, 2"
	// `interval` is a type name in PostgreSQL and a keyword in DataFusion, so
	// the tag is quoted here rather than left to toPG, whose list of reserved
	// words this one is not on.
	//
	// The newest version of the file, not the newest row per ecosystem. Every
	// block of one dependabot.yml is stamped at that file's last commit, so a
	// repository's current blocks all carry the same stamp and a block only an
	// earlier version had carries an older one. Newest row per (repo,
	// ecosystem, interval) would keep both, and the older one is a lie: an
	// ecosystem dropped from the file, or a schedule moved from daily to
	// weekly, would stand in this table as two current rows, one of them
	// describing something Dependabot stopped doing. Reading the whole history
	// is what makes that reachable, so the two go together.
	ecosystems := `SELECT repo AS "Repository", blocks AS "Blocks", ecosystem AS "Ecosystem",` +
		` "interval" AS "Interval" FROM (` +
		`SELECT repo, ecosystem, "interval", blocks, time,` +
		" MAX(time) OVER (PARTITION BY repo) AS newest" +
		" FROM " + de + " " + everSince + ") x WHERE time = newest ORDER BY 1, 2"
	packages := `SELECT ecosystem AS "Ecosystem", SUM(packages) AS "Packages" FROM (` +
		"SELECT ecosystem, packages, ROW_NUMBER() OVER (PARTITION BY repo, ecosystem" +
		" ORDER BY time DESC) AS rn FROM " + dep +
		ciInRange + RF + ") x WHERE rn = 1" +
		" GROUP BY 1 ORDER BY 2 DESC LIMIT 12"
	// Summed, not deduplicated: see the note on the function.
	changes := "SELECT " + timeBin + ", change AS series," +
		" SUM(packages) AS packages FROM " + dc +
		ciInRange + RF + " AND change <> '(none)' GROUP BY 1, 2 UNION ALL" +
		" SELECT " + timeBin + ", 'vulnerable' AS series," +
		" SUM(vulnerable) AS packages FROM " + dc +
		ciInRange + RF + " GROUP BY 1 ORDER BY 1"

	// One column, because gTbl reduces one series list and the second reducer
	// would only reduce the same numbers twice. `present` is the one the panel
	// is read for, so that is the one Graphite keeps.
	policyGR, policyGRtf := gTbl(rowsOf(inventoryKeepLast+rp(pf, "present")+")",
		gn(pf, "repo"), gn(pf, "file")), "Repository, file",
		[]col{{"lastNotNull", "Present"}})
	// `present` is a boolean, so max and never top_metrics: the sink writes a
	// Go bool as a JSON boolean, Elasticsearch hands it back out of a
	// top_metrics as the string "true", and the plugin panics converting that
	// to a float. `path` is a string and cannot be a metric at all, so it is a
	// terms bucket, which costs no rows here because one file in force has one
	// path.
	policyES, policyEStf := esTbl(pf,
		[]any{b.tm("repo", 500), b.tm("file", 10), b.tm("path", 50), b.tmURL()},
		[]any{b.mMax("present"), b.mMax("changes")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"file.keyword", "File"},
			{"path.keyword", "Path"},
			{inventoryURLTerm, "Link"},
			{"present", "Present"},
			{"changes", "Changes"},
		}, []string{ESF})

	ecoGR, ecoGRtf := gTbl(rowsOf(inventoryKeepLast+rp(de, "blocks")+")",
		gn(de, "repo"), gn(de, "ecosystem"), gn(de, "interval")),
		"Repository, ecosystem, interval", []col{{"lastNotNull", "Blocks"}})
	ecoES, ecoEStf := esTbl(de,
		[]any{b.tm("repo", 500), b.tm("ecosystem", 20), b.tm("interval", 10)},
		[]any{b.mMax("blocks")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"ecosystem.keyword", "Ecosystem"},
			{"interval.keyword", "Interval"},
			{"blocks", "Blocks"},
		}, []string{ESF})

	packGR, packGRtf := gTbl(fmt.Sprintf(
		`sortByMaxima(groupByNode(keepLastValue(%s), %d, "sum"))`,
		rp(dep, "packages"), gn(dep, "ecosystem"),
	),
		"Ecosystem", []col{{"lastNotNull", "Packages"}})
	packES, packEStf := esTbl(dep, []any{b.tm("ecosystem", 12), b.tm("repo", 500)},
		[]any{b.mNewest("packages")},
		[]named{{"ecosystem.keyword", "Ecosystem"}, {"packages", "Packages"}}, []string{ESF},
		groupSum("Ecosystem", "Packages", "Ecosystem", "Packages")...)

	// Off by default, and the panels say so rather than looking broken: the
	// SBOM is a megabyte or two per repository and has a rate limit bucket of
	// its own.
	const offByDefault = "Off by default: the SBOM is a megabyte or two per repository."

	// `vulnerable` is a share of the changes beside it, not a fifth kind of
	// change: a package that was added and carries an advisory is one package,
	// counted once under `added` and again here. Stacked it would raise the
	// bar by its own height, so the bar would stop being the number of changes
	// the range holds. It is drawn as a line across the stack instead, which
	// is the same reading in every store because every store names the series
	// `vulnerable`.
	vulnerableLine := override("vulnerable", []any{
		map[string]any{"id": "custom.stacking", "value": map[string]any{
			"mode": "none", "group": "A",
		}},
		map[string]any{"id": "custom.drawStyle", "value": "line"},
		map[string]any{"id": "custom.fillOpacity", "value": 0},
		map[string]any{"id": "custom.lineWidth", "value": 2},
	})

	return []Panel{
		panel("table", "Policy files", box{W: 12, H: 12, X: 0, Y: 57}, []Target{sqlT(policy)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("max by (repo, file) (github_policy_file_present{%s})", PF), "A"),
				promTbl(fmt.Sprintf("max by (repo, file) (github_policy_file_changes{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "file": "File",
				inventoryValueCol + "A": "Present", inventoryValueCol + "B": "Changes",
			}, nil, map[string]int{"repo": 0, "file": 1}),
			Desc: "Four rows per repository: dependabot, codeowners, security and funding. " +
				"Path is the one in force, or where the file would go if it existed, because " +
				"a file at the wrong path is a file that does nothing and gh_repo_policy does " +
				"not carry it. Changes counts every commit against that path over the whole " +
				"life of the repository, so a file that is gone but has a history was deleted " +
				"and this row is dated at the deletion. The crossing worth reading: a " +
				"dependabot row with no blocks, which is a repository absent from the table " +
				"beside this one, next to vulnerability alerts off in gh_repo_policy, is a " +
				"repository that receives no dependency updates by either route. Each row is " +
				"dated at the last commit that touched the path, so this reads the whole " +
				"history rather than the dashboard range: a file nobody has touched in a year " +
				"would otherwise vanish as you narrow the range.",
			PromDesc: "Prometheus keeps the two counters and not the path, which is a string.",
			Overrides: []any{
				repoColumn(), width("File", 100), profileBool("Present", 80),
				width("Path", 190), width("Changes", 90), linkOn("File"),
			},
			GR: policyGR, GRTF: policyGRtf,
			GRDesc: "Graphite has no path either: it is a string. " + grRows + " " + grRange,
			ES:     policyES, ESTF: policyEStf,
			ESDesc: "Elasticsearch answers Present and Changes with the largest reading inside " +
				"the range rather than with the newest row, because the aggregation that takes " +
				"the newest one returns a boolean as text, which the datasource cannot render. " +
				"A url is written only for a file that is there, so the absent rows are " +
				"kept by bucketing them under an empty one. " + esRange,
		}),
		panel("table", "Dependabot ecosystems", box{W: 12, H: 12, X: 12, Y: 57}, []Target{sqlT(ecosystems)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"max by (repo, ecosystem, interval) (github_dependabot_ecosystem_blocks{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "ecosystem": "Ecosystem",
				"interval": "Interval", "Value": "Blocks",
			}, nil, map[string]int{"repo": 0, "ecosystem": 1, "interval": 2})},
			Opts: Opts{"sort": "Blocks"},
			Desc: "What each dependabot.yml actually updates, and how often. One ecosystem " +
				"appears more than once when it is configured per directory, which is exactly " +
				"what blocks counts: five ecosystems across seven blocks is one repository " +
				"here. Dated from dependabot.yml itself, at the last commit that touched it, " +
				"so this reads the whole history rather than the dashboard range, and only " +
				"the newest version of each file: an ecosystem dropped from it, or a schedule " +
				"moved from daily to weekly, leaves no second row behind. A repository that " +
				"deleted the file altogether writes no block at all and so keeps its last " +
				"ones here; the dependabot row beside it in Policy files, with Present at " +
				"zero, is what says so.",
			Overrides: []any{
				repoColumn(), width("Ecosystem", 130),
				width("Interval", 100), barCell("Blocks", "short", 110),
			},
			GR: ecoGR, GRTF: ecoGRtf,
			GRDesc: grRows + " " + grRange + " Neither can it read one version of a file: a " +
				"block removed inside the range keeps its last value here, where the SQL " +
				"twin drops it.",
			ES: ecoES, ESTF: ecoEStf,
			ESDesc: esRange + " A block removed from a dependabot.yml inside the range still " +
				"has a document in it, where the SQL twin reads only the newest version of " +
				"the file and drops it.",
		}),
		panel("barchart", "Dependencies by ecosystem", box{W: 12, H: 7, X: 0, Y: 69},
			[]Target{sqlT(packages)}, &P{
				Prom: []Target{promTbl(fmt.Sprintf(
					"topk(12, sum by (ecosystem) (github_dependency_packages{%s}))", PF,
				))},
				PromTF: []any{organize(map[string]string{
					"ecosystem": "Ecosystem", "Value": "Packages",
				}, nil, nil)},
				Desc: "Every package the dependency graph knows about, by the package manager " +
					"that installs it, read out of the SBOM. A daily snapshot, so this is the " +
					"newest reading of each repository and ecosystem and only then a sum: " +
					"adding the rows up would count the same tree once per day. " + offByDefault,
				GR: packGR, GRTF: packGRtf,
				ES: packES, ESTF: packEStf,
			}),
		panel("timeseries", "Dependency changes", box{W: 12, H: 7, X: 12, Y: 69},
			[]Target{sqlTS(changes)}, &P{
				Prom: []Target{
					promq(fmt.Sprintf(`sum by (change) (github_dependency_change_packages{%s, change!="(none)"})`, PF),
						withRef("A"), legend("{{change}}")),
					promq(fmt.Sprintf("sum(github_dependency_change_vulnerable{%s})", PF),
						withRef("B"), legend("vulnerable")),
				},
				Opts:      mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
				SQLOpts:   seriesOpts,
				Overrides: []any{vulnerableLine},
				// Lines rather than bars, the way every other dated panel
				// renders its Prometheus twin: the value is the last sweep's
				// batch held until the next one, so a bar per scrape would
				// draw one bump as a week of them.
				PromOpts: Opts{"bars": false},
				Desc: "What entered and left the dependency graph, and how much of it carried a " +
					"known advisory. Each row covers a real commit range, from the head of the " +
					"previous sweep to this one, so the rows do not overlap and adding them up " +
					"over the range is the whole story rather than a double count. A sweep " +
					"with nothing to report writes a row of zeros, so the table exists from " +
					"the first sweep; that series is left out here. One bump measured 370 changes, one " +
					"of which removed a package with a known advisory, and six rows is all that " +
					"is kept of it. Vulnerable is a share of those same changes and not a kind " +
					"of its own, so it crosses the stack as a line rather than adding its own " +
					"height to it. " + offByDefault + " " + bucketFollowsRange,
				PromDesc: sweepCount,
				// The zero row's (none) is the node `_none_` in a Graphite path.
				GR: []Target{
					grq(perBucket(fmt.Sprintf(`exclude(%s, "\._none_\.")`, rp(dc, "packages")), gn(dc, "change")), "A"),
					grq(fmt.Sprintf(
						`alias(consolidateBy(summarize(sumSeries(%s), "1d", "sum"), "sum"), "vulnerable")`,
						rp(dc, "vulnerable"),
					), "B"),
				},
				ES: []Target{
					b.esDaily(dc, b.mSum("packages"), "change", "", []string{ESF, `NOT change.keyword:"(none)"`}, "A"),
					esq(dc, []any{b.mSum("vulnerable")}, []any{b.dh()}, "B", []string{ESF}, "vulnerable"),
				},
			}),
	}
}
