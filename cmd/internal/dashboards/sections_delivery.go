package dashboards

import "fmt"

// ── Delivery and access ─────────────────────────────────────────────────────

// deliveryNewestRow closes the window every table here opens over a snapshot:
// the rows are numbered newest first per item and only the first of each is
// kept, so a setting read on thirty sweeps is one row and not thirty. The
// security section reads its own snapshots with the same tail.
const deliveryNewestRow = ") x WHERE rn = 1"

// What the columns of this section are called wherever they are read. The same
// name has to reach the panel from all five stores, since the overrides, the
// units and the sorts below match a column by it; the security section names
// the age of a setting the same way.
const (
	deliveryLastChanged  = "Last changed"
	deliveryKeyUnused    = "Unused for"
	deliveryKeyReadOnly  = "Read only"
	deliveryHookEvents   = "Events subscribed"
	deliveryForcePush    = "Force push"
	deliveryBypassActors = "Bypass actors"
	deliveryTimeToStatus = "To status"
	deliveryTimeLive     = "Live for"
)

// collected marks targets as fetched but not drawn: the inputs a server-side
// expression reduces, each of which would otherwise be a line of its own on a
// panel that only wants the result.
func collected(ts ...Target) []Target {
	for i := range ts {
		ts[i].Hide = true
	}
	return ts
}

// delivery is how events leave the account, who is allowed in, what the way in
// is actually guarded by, and what reached the other end: the webhook
// deliveries, then the rulesets, deploy keys and environments that stand in
// front of a repository, and then the branches, the protections and the
// ruleset rules themselves alongside the deployments that arrived.
func delivery(b *builder) []Panel {
	out := append(webhookDeliveries(b), accessConfiguration(b)...)
	out = append(out, branchesAndProtections(b)...)
	return append(out, deploymentsToEnvironments(b)...)
}

// webhookDeliveries is whether the hooks got through: how many failed, what
// status codes came back over time, and which endpoints they came from.
func webhookDeliveries(b *builder) []Panel {
	hooks := `SELECT host AS "Endpoint",` +
		` SUM(CASE WHEN ok = 'false' THEN 1 ELSE 0 END) AS "Failed",` +
		` COUNT(*) AS "Deliveries", repo AS "Repository",` +
		` SUM(CASE WHEN redelivery THEN 1 ELSE 0 END) AS "Retried",` +
		` approx_percentile_cont(duration_seconds, 0.5) AS "Latency"` +
		" FROM gh_webhook_delivery WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 4 ORDER BY 2 DESC, 3 DESC LIMIT 25"
	overTime := "SELECT " + timeBin + ", code AS series," +
		" COUNT(*) AS n FROM gh_webhook_delivery WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 2 ORDER BY 1"
	failRate := "SELECT 100.0 * SUM(CASE WHEN ok = 'false' THEN 1 ELSE 0 END) / COUNT(*)" +
		" AS value FROM gh_webhook_delivery WHERE $__timeFilter(time) AND " + RF
	totalM := fmt.Sprintf("github_webhook_deliveries_total{%s}", PF)
	failed := fmt.Sprintf(`github_webhook_deliveries_total{ok="false",%s}`, PF)
	wd := "gh_webhook_delivery"
	deliv := rp(wd, "duration_seconds")
	failedPath := rp(wd, "duration_seconds", "ok", "false")

	hooksGR, hooksGRtf := gTbl(fmt.Sprintf(
		`limit(sortBy(groupByNodes(%s, "avg", %d, %d, %d), "count", true), 25)`,
		deliv, gn(wd, "host"), gn(wd, "repo"), gn(wd, "ok"),
	),
		"Endpoint, repository, ok", []col{{"count", "Deliveries"}, {"median", "Latency"}})
	hooksES, hooksEStf := esTbl(wd, []any{b.tm("host", 25), b.tm("repo", 50), b.tm("ok", 2)},
		[]any{b.mCount(), b.mPct("duration_seconds", 50)},
		[]named{
			{"host.keyword", "Endpoint"},
			{inventoryRepoTerm, "Repository"},
			{"ok.keyword", "OK"},
			{"n", "Deliveries"},
			{"l", "Latency"},
		}, []string{ESF})

	return []Panel{
		panel("gauge", "Webhook failure rate", box{W: 6, H: 8, X: 0, Y: 0}, []Target{sqlT(failRate)}, &P{
			Prom: []Target{promNow(fmt.Sprintf(
				"100 * sum(increase(%s[$__range])) / sum(increase(%s[$__range]))", failed, totalM,
			))},
			Opts: Opts{"thresholds": []any{
				map[string]any{"color": "green", "value": nil},
				map[string]any{"color": "orange", "value": 5},
				map[string]any{"color": "red", "value": 25},
			}},
			Desc: "Webhooks fail silently. Nothing tells you a hook has been answering 403 for " +
				"weeks except this.",
			PromDesc: sinceStart,
			GR: []Target{grq(fmt.Sprintf("asPercent(%s, %s)",
				total(countOf(failedPath)), total(countOf(deliv))))},
			// `ok` is a tag, so it is text in Elasticsearch and no boolean
			// mean exists: the two counts are reduced and divided server-side.
			ES: append(collected(
				b.esDaily(wd, b.mCount(), "", "", []string{"ok:false", ESF}, "A"),
				b.esDaily(wd, b.mCount(), "", "", []string{ESF}, "B"),
				exprT("C", "reduce", "A", map[string]any{
					"reducer": "sum", "settings": map[string]any{"mode": "dropNN"},
				}),
				exprT("D", "reduce", "B", map[string]any{
					"reducer": "sum", "settings": map[string]any{"mode": "dropNN"},
				}),
			), exprT("E", "math", "100 * $C / $D", nil)),
			ESDesc: "In Elasticsearch the failed and the total deliveries are counted per day, " +
				"summed and divided by a server-side expression.",
		}),
		panel("timeseries", "Deliveries by status code", box{W: 18, H: 8, X: 6, Y: 0},
			[]Target{sqlTS(overTime)}, &P{
				Prom: []Target{hourly(fmt.Sprintf("sum by (code) (increase(%s[1h]))", totalM),
					"{{code}}")},
				// Five units tall leaves 85 pixels of plot under a legend of a dozen
				// status codes; the code is in the tooltip, so the legend goes.
				Opts:     mergeOpts(Opts{"bars": true, "stack": true, "legend": "hidden"}, hourBins),
				SQLOpts:  seriesOpts,
				PromDesc: sinceStart,
				GR:       []Target{grq(perBucket("isNonNull("+deliv+")", gn(wd, "code"), "1h"))},
				ES:       []Target{b.esDaily(wd, b.mCount(), "code", "1h", []string{ESF}, "")},
				Desc:     bucketFollowsRange,
			}),
		panel("table", "Webhook endpoints", box{W: 12, H: 8, X: 0, Y: 8}, []Target{sqlT(hooks)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (repo, hook) (increase(%s[$__range]))", totalM), "A"),
				promTbl(fmt.Sprintf("sum by (repo, hook) (increase(%s[$__range]))", failed), "B"),
				promTbl(fmt.Sprintf("avg by (repo, hook) (github_webhook_deliveries_redelivery_mean{%s})", PF), "C"),
				promTbl(fmt.Sprintf("avg by (repo, hook) (github_webhook_deliveries_duration_seconds_mean{%s})", PF), "D"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "hook": "Hook", inventoryValueCol + "A": "Deliveries",
				inventoryValueCol + "B": "Failed", inventoryValueCol + "C": "Retried", inventoryValueCol + "D": "Latency",
			}, nil, map[string]int{"repo": 0, "hook": 1}),
			Opts: Opts{"sort": "Failed"},
			Desc: "Only the host is stored. The path of a webhook URL usually carries a secret. " +
				"A retried delivery is a different fact from a first one that failed, and " +
				"counting them together makes an endpoint look worse than it is.",
			PromDesc: "Prometheus keeps the hook name rather than the host, and the retries as " +
				"a fraction rather than a count. " + sinceStart,
			Overrides: []any{
				width("Endpoint", 150), barCell("Deliveries", "short", 120),
				width("Failed", 90), width("Retried", 90), unitOf("Latency", "s", 100),
			},
			GR: hooksGR, GRTF: hooksGRtf,
			GRDesc: "Graphite names each row host, repository and whether the delivery succeeded " +
				"from the path, so the failures are their own rows. " + grRows,
			ES: hooksES, ESTF: hooksEStf,
			ESDesc: "In Elasticsearch the failures are their own rows, split by the `ok` tag.",
		}),
	}
}

// accessConfiguration is what is allowed and by whom: the rulesets guarding
// a branch, the deploy keys, the webhooks that exist at all, and the
// environments a deployment can reach.
func accessConfiguration(b *builder) []Panel {
	rules := `SELECT repo AS "Repository", ruleset AS "Ruleset",` +
		` enforcement AS "Enforcement", target AS "Target",` +
		` days_since_change AS "Last changed", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, ruleset ORDER BY time DESC) AS rn" +
		" FROM gh_ruleset WHERE $__timeFilter(time) AND " + RF + deliveryNewestRow +
		" ORDER BY 1, 2"
	keys := `SELECT repo AS "Repository", days_since_use AS "Unused for", key AS "Key",` +
		` read_only AS "Read only" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, key ORDER BY time DESC) AS rn" +
		" FROM gh_deploy_key WHERE $__timeFilter(time) AND " + RF + deliveryNewestRow +
		" ORDER BY 2 DESC"
	rs, dk := "gh_ruleset", "gh_deploy_key"

	rulesGR, rulesGRtf := gTbl(rowsOf(rp(rs, "days_since_change"), gn(rs, "repo"),
		gn(rs, "ruleset"), gn(rs, "enforcement"), gn(rs, "target")),
		"Ruleset", []col{{"lastNotNull", deliveryLastChanged}})
	rulesES, rulesEStf := esTbl(rs, []any{
		b.tm("repo", 50), b.tm("ruleset", 50),
		b.tm("enforcement", 5), b.tm("target", 5), b.tmURL(),
	}, []any{b.mNewest("days_since_change")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"ruleset.keyword", "Ruleset"},
			{"enforcement.keyword", "Enforcement"},
			{"target.keyword", "Target"},
			{inventoryURLTerm, "Link"},
			{"d", deliveryLastChanged},
		}, []string{ESF})

	keysGR, keysGRtf := gTbl(rowsOf(rp(dk, "days_since_use"), gn(dk, "repo"),
		gn(dk, "key"), gn(dk, "read_only")), "Key", []col{{"lastNotNull", deliveryKeyUnused}})
	// A `max` rather than the newest reading, because `days_since_use` is
	// written only for a key GitHub has seen used and a top_metrics has no
	// value to hand back for the others. Grafana's Elasticsearch plugin
	// appends nothing at all in that case, so the metric column came back one
	// row shorter than the three bucket columns and the whole panel failed
	// with `frame has different field lengths, field 0 is len 2 but field 3 is
	// len 1`. Every other aggregation appends a null instead and the row keeps
	// its shape, which is what an unused key should look like: an empty cell.
	keysES, keysEStf := esTbl(dk, []any{b.tm("repo", 50), b.tm("key", 50), b.tm("read_only", 2)},
		[]any{b.mMax("days_since_use")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"key.keyword", "Key"},
			{"read_only.keyword", deliveryKeyReadOnly},
			{"d", deliveryKeyUnused},
		}, []string{ESF})

	whGR, whGRtf := gTbl(rowsOf("keepLastValue("+rp("gh_webhook", "events")+")",
		gn("gh_webhook", "repo"), gn("gh_webhook", "host")),
		"Repository, host", []col{{"lastNotNull", deliveryHookEvents}})
	whES, whEStf := esTbl("gh_webhook", []any{b.tm("repo", 500), b.tm("host", 50), b.tm("active", 2)},
		[]any{b.mNewest("events")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"host.keyword", "Host"},
			{"active.keyword", "Active"},
			{"events", deliveryHookEvents},
		}, []string{ESF})

	envGR, envGRtf := gTbl(rowsOf("keepLastValue("+rp("gh_environment", "days_since_change")+")",
		gn("gh_environment", "repo"), gn("gh_environment", "environment")),
		"Repository, environment", []col{{"lastNotNull", "Idle"}})
	envES, envEStf := esTbl("gh_environment", []any{b.tm("repo", 500), b.tm("environment", 50), b.tmURL()},
		[]any{b.mNewest("days_since_change")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"environment.keyword", "Environment"},
			{inventoryURLTerm, "Link"},
			{"days_since_change", "Idle"},
		}, []string{ESF})
	return []Panel{
		panel("table", "Rulesets", box{W: 12, H: 8, X: 12, Y: 8}, []Target{sqlT(rules)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (repo, ruleset, enforcement) (github_ruleset_active{%s})", PF), "A"),
				promTbl(fmt.Sprintf("sum by (repo, ruleset, enforcement) (github_ruleset_days_since_change{%s})", PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "ruleset": "Ruleset", "enforcement": "Enforcement",
				inventoryValueCol + "A": "Active", inventoryValueCol + "B": deliveryLastChanged,
			}, nil, map[string]int{"repo": 0, "ruleset": 1, "enforcement": 2}),
			Desc: "A 404 from branch protection does not mean unprotected: a repository can be " +
				"governed entirely by rulesets, which that endpoint knows nothing about.",
			Overrides: []any{
				width("Enforcement", 120), width("Target", 100),
				unitOf(deliveryLastChanged, "d", 130), ownerLinkOn("Ruleset", "the ruleset settings"),
			},
			PromOver: []any{width("Active", 80)},
			GR:       rulesGR, GRTF: rulesGRtf,
			GRDesc: "Graphite names each row repository, ruleset, enforcement and target from the path.",
			ES:     rulesES, ESTF: rulesEStf,
		}),
		panel("table", "Deploy keys", box{W: 8, H: 8, X: 0, Y: 16}, []Target{sqlT(keys)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"sum by (repo, key, read_only) (github_deploy_key_days_since_use{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "key": "Key", "read_only": deliveryKeyReadOnly,
				"Value": deliveryKeyUnused,
			}, nil, map[string]int{"repo": 0, "key": 1, "read_only": 2})},
			Opts:      Opts{"sort": deliveryKeyUnused},
			Desc:      "A write key nobody has used in a year is a credential to remove.",
			Overrides: []any{width(deliveryKeyReadOnly, 100), unitOf(deliveryKeyUnused, "d", 120)},
			GR:        keysGR, GRTF: keysGRtf,
			GRDesc: "Graphite names each row repository, key and whether it is read-only from the path.",
			ES:     keysES, ESTF: keysEStf,
			ESDesc: "Elasticsearch answers with the largest number of days inside the range " +
				"rather than with the newest reading. A key GitHub has never seen used has " +
				"no reading at all, in this dashboard or any of the others, and its cell " +
				"is empty.",
		}),
		panel("text", "Where failure output went", box{W: 16, H: 8, X: 8, Y: 16}, nil, &P{
			Opts: Opts{"content": logNote},
			Logs: &Logs{
				Selector: jobLogSelector,
				Desc: "The last lines of every job that failed, from the Loki sink, newest " +
					"first: the workflow, job and run of each line are in its logfmt tail, " +
					"and the details of a line list them. GitHub deletes job logs after " +
					"ninety days, so what is here is what was captured while it was there.",
			},
		}),
		panel("table", "Webhooks configured", box{W: 12, H: 8, X: 0, Y: 24}, []Target{sqlT(
			`SELECT repo AS "Repository", host AS "Host", hook AS "Hook",` +
				` active AS "Active", MAX(events) AS "Events subscribed" FROM (` +
				"SELECT repo, host, hook, active, events, ROW_NUMBER() OVER (" +
				"PARTITION BY repo, hook, host ORDER BY time DESC) AS rn FROM gh_webhook" +
				" WHERE $__timeFilter(time) AND " + RF + deliveryNewestRow +
				" GROUP BY 1, 2, 3, 4 ORDER BY 1, 2",
		)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"max by (repo, host, hook, active) (github_webhook_events{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "host": "Host", "hook": "Hook",
				"active": "Active", "Value": deliveryHookEvents,
			}, []string{"owner", "full_name", "instance", "job", "__name__"}, nil)},
			Desc: "The panel beside this one is built from deliveries, so a hook that has never " +
				"delivered anything appears in it nowhere. This is the inventory: an active " +
				"hook with no traffic is the interesting row. Hook is GitHub's id for it, the " +
				"number in its settings page: GitHub names every webhook \"web\", so the name " +
				"told nothing apart.",
			Overrides: []any{width("Host", 200), width("Active", 90), width(deliveryHookEvents, 150)},
			GR:        whGR, GRTF: whGRtf, GRDesc: grSlot,
			ES: whES, ESTF: whEStf,
		}),
		panel("table", "Environments", box{W: 12, H: 8, X: 12, Y: 24}, []Target{sqlT(
			`SELECT environment AS "Environment", MIN(days_since_change) AS "Idle",` +
				` repo AS "Repository", MAX(url) AS "Link" FROM gh_environment` +
				" WHERE $__timeFilter(time) AND " + RF +
				" GROUP BY 1, 3 ORDER BY 2 DESC",
		)}, &P{
			Prom: []Target{promTbl(fmt.Sprintf(
				"min by (repo, environment) (github_environment_days_since_change{%s})", PF,
			))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "environment": "Environment", "Value": "Idle",
			}, []string{"owner", "full_name", "instance", "job", "__name__"}, nil)},
			Opts: Opts{"sort": "Idle"},
			Desc: "The same question the deploy keys table asks, about deployment targets: one " +
				"environment here has not been touched in one thousand one hundred and " +
				"seventy seven days.",
			Overrides: []any{
				unitOf("Idle", "d", 100), width("Environment", 150),
				ownerLinkOn("Environment", "the deployment log"),
			},
			GR: envGR, GRTF: envGRtf, GRDesc: grSlot,
			ES: envES, ESTF: envEStf,
		}),
	}
}

// blockedOrAllowed renders the one boolean of this section that names what a
// protection permits rather than what it requires.
//
// Every other boolean column in these dashboards takes onOff, which paints the
// switched-on value green because switched on is the reassuring reading. Here
// it is the dangerous one, and a green "on" sitting in a row beside three
// other green "on"s would read as one more thing going right.
func blockedOrAllowed(name string, w int) any {
	return override(name, []any{
		map[string]any{"id": "custom.cellOptions", "value": map[string]any{"type": "color-text"}},
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "value", "options": map[string]any{
				"0": map[string]any{"text": "blocked", "color": "green", "index": 1},
				"1": map[string]any{"text": "allowed", "color": "red", "index": 0},
			},
		}}},
		map[string]any{"id": "custom.width", "value": w},
	})
}

// branchesAndProtections is what actually guards the way in: which branches
// exist and which were abandoned, what one classic protection enforces, and
// what a ruleset does and who may walk past it.
//
// All three measurements are daily snapshots. gh_branch, gh_branch_protection
// and gh_ruleset_rule each write one identical row per day per series, so every
// panel here takes the newest row per series the way "Rulesets" and "Deploy
// keys" beside them already do, and none of them counts: counting would answer
// with the number of days the collector has been running rather than with the
// number of branches. gh_deployment, in the pair of panels below, is the
// opposite case and is read the opposite way round.
func branchesAndProtections(b *builder) []Panel {
	gb, bp, rr := "gh_branch", "gh_branch_protection", "gh_ruleset_rule"

	branches := `SELECT branch AS "Branch", days_since_commit AS "Idle",` +
		` repo AS "Repository", is_default AS "Default" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, branch ORDER BY time DESC) AS rn" +
		" FROM gh_branch WHERE $__timeFilter(time) AND " + RF + deliveryNewestRow +
		" ORDER BY 2 DESC"
	protections := `SELECT repo AS "Repository", pattern AS "Pattern",` +
		` required_reviews AS "Reviews",` +
		` CAST(requires_commit_signatures AS INT) AS "Signatures",` +
		` CAST(requires_linear_history AS INT) AS "Linear",` +
		` CAST(allows_force_pushes AS INT) AS "Force push",` +
		` CAST(requires_conversation_resolution AS INT) AS "Threads",` +
		` required_checks AS "Checks", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, pattern ORDER BY time DESC) AS rn" +
		" FROM gh_branch_protection WHERE $__timeFilter(time) AND " + RF + deliveryNewestRow +
		" ORDER BY 1, 2"
	ruleRows := `SELECT rule AS "Rule", bypass_always AS "Always", repo AS "Repository",` +
		` ruleset AS "Ruleset", bypass_actors AS "Bypass actors",` +
		` bypass_sampled AS "Sampled" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY repo, ruleset, rule ORDER BY time DESC) AS rn" +
		" FROM gh_ruleset_rule WHERE $__timeFilter(time) AND " + RF + deliveryNewestRow +
		" ORDER BY 2 DESC, 3, 4, 1"
	branchGR, branchGRtf := gTbl(rowsOf(rp(gb, "days_since_commit"),
		gn(gb, "repo"), gn(gb, "branch"), gn(gb, "is_default")),
		"Repository, branch, default", []col{{"lastNotNull", "Idle"}})
	// A `max` rather than the newest reading, for the reason the deploy keys
	// table above carries one. `days_since_commit` is written only for a ref
	// whose target is a commit, and a ref that points at anything else is
	// stored with no age at all, which the collector has a test for. A
	// top_metrics hands back nothing for a bucket where the field never
	// appears, leaving the metric column shorter than the three bucket columns
	// and killing the whole panel with `frame has different field lengths`.
	// `max` appends a null instead and the row keeps its shape.
	branchES, branchEStf := esTbl(gb,
		[]any{b.tm("repo", 500), b.tm("branch", 500), b.tm("is_default", 2)},
		[]any{b.mMax("days_since_commit")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"branch.keyword", "Branch"},
			{"is_default.keyword", "Default"},
			{"days_since_commit", "Idle"},
		}, []string{ESF})

	// A boolean is not a metric in Graphite, so the four switches have no series
	// there to read at all, and a table there carries one column: the number of
	// status checks, which is the column a reader would sort this by.
	protGR, protGRtf := gTbl(rowsOf(rp(bp, "required_checks"), gn(bp, "repo"), gn(bp, "pattern")),
		"Repository, pattern", []col{{"lastNotNull", "Checks"}})
	// A `max` per column rather than the newest reading, for the reason the
	// repository settings table carries one: four of these six are booleans,
	// and Elasticsearch hands a boolean out of a top_metrics as the string
	// "true", which panics Grafana's plugin and takes the whole panel with it.
	// `max` is answered as a number over a boolean, and appends a null rather
	// than nothing where `required_reviews` was never written.
	protES, protEStf := esTbl(bp, []any{b.tm("repo", 500), b.tm("pattern", 100), b.tmURL()},
		[]any{
			b.mMax("required_reviews"), b.mMax("requires_commit_signatures"),
			b.mMax("requires_linear_history"), b.mMax("allows_force_pushes"),
			b.mMax("requires_conversation_resolution"), b.mMax("required_checks"),
		},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"pattern.keyword", "Pattern"},
			{inventoryURLTerm, "Link"},
			{"required_reviews", "Reviews"},
			{"requires_commit_signatures", "Signatures"},
			{"requires_linear_history", "Linear"},
			{"allows_force_pushes", deliveryForcePush},
			{"requires_conversation_resolution", "Threads"},
			{"required_checks", "Checks"},
		}, []string{ESF})

	// Graphite keeps one column of the three, so it keeps `bypass_actors`: the
	// exact total, which means the same thing standing alone as it does beside
	// the other two. The column the panel sorts by would be the misreading.
	ruleGR, ruleGRtf := gTbl(rowsOf(rp(rr, "bypass_actors"),
		gn(rr, "repo"), gn(rr, "ruleset"), gn(rr, "rule")),
		"Repository, ruleset, rule", []col{{"lastNotNull", deliveryBypassActors}})
	ruleES, ruleEStf := esTbl(rr,
		[]any{b.tm("repo", 500), b.tm("ruleset", 100), b.tm("rule", 100)},
		[]any{b.mMax("bypass_actors"), b.mMax("bypass_always"), b.mMax("bypass_sampled")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"ruleset.keyword", "Ruleset"},
			{"rule.keyword", "Rule"},
			{"bypass_actors", deliveryBypassActors},
			{"bypass_always", "Always"},
			{"bypass_sampled", "Sampled"},
		}, []string{ESF})

	return []Panel{
		panel("table", "Stale branches", box{W: 12, H: 8, X: 0, Y: 32}, []Target{sqlT(branches)}, &P{
			Prom: []Target{promTbl(promTop(50, fmt.Sprintf(
				"max by (repo, branch, is_default) (github_branch_days_since_commit{%s})", PF,
			)))},
			PromTF: []any{organize(map[string]string{
				"repo": "Repository", "branch": "Branch", "is_default": "Default",
				"Value": "Idle",
			}, nil, map[string]int{"repo": 0, "branch": 1, "is_default": 2})},
			Opts: Opts{"sort": "Idle"},
			PromDesc: "Prometheus has no LIMIT, so the twin is the fifty idlest branches " +
				"rather than every one of them.",
			Desc: "Nothing else answers which branches were abandoned: GitHub's own branch list " +
				"is ordered by name and keeps no history. The refs come back unordered and " +
				"capped at a hundred per repository, so `gh_repo_total.branches` is the true " +
				"count and is what makes the truncation visible. " + forksIncluded +
				" Of the 1,208 branches in the picker when this was measured, 1,083 were " +
				"a fork's; gh_branch carries no fork tag, so narrowing the picker is the " +
				"only way to read this as the account's own branches.",
			Overrides: []any{
				repoColumn(), width("Branch", 200), width("Default", 90),
				unitOf("Idle", "d", 100),
			},
			GR: branchGR, GRTF: branchGRtf,
			GRDesc: "Graphite names each row repository, branch and whether it is the default " +
				"from the path.",
			ES: branchES, ESTF: branchEStf,
			ESDesc: "Elasticsearch answers with the largest number of days inside the range " +
				"rather than with the newest reading. A branch whose ref points at something " +
				"that is not a commit has no age at all, in this dashboard or any of the " +
				"others, and its cell is empty.",
		}),
		panel("table", "Branch protection rules", box{W: 12, H: 8, X: 12, Y: 32},
			[]Target{sqlT(protections)}, &P{
				Prom: []Target{
					promTbl(fmt.Sprintf("max by (repo, pattern) (github_branch_protection_required_reviews{%s})", PF), "A"),
					promTbl(fmt.Sprintf("max by (repo, pattern) (github_branch_protection_requires_commit_signatures{%s})", PF), "B"),
					promTbl(fmt.Sprintf("max by (repo, pattern) (github_branch_protection_requires_linear_history{%s})", PF), "C"),
					promTbl(fmt.Sprintf("max by (repo, pattern) (github_branch_protection_allows_force_pushes{%s})", PF), "D"),
					promTbl(fmt.Sprintf("max by (repo, pattern) (github_branch_protection_requires_conversation_resolution{%s})", PF), "E"),
					promTbl(fmt.Sprintf("max by (repo, pattern) (github_branch_protection_required_checks{%s})", PF), "F"),
				},
				PromTF: merged(map[string]string{
					"repo": "Repository", "pattern": "Pattern",
					inventoryValueCol + "A": "Reviews", inventoryValueCol + "B": "Signatures",
					inventoryValueCol + "C": "Linear", inventoryValueCol + "D": deliveryForcePush,
					inventoryValueCol + "E": "Threads", inventoryValueCol + "F": "Checks",
				}, nil, map[string]int{"repo": 0, "pattern": 1}),
				Desc: "What each branch protection enforces. " +
					"`gh_repo_policy.branch_protection_rules` counts these and stops there, so a " +
					"protection that is switched on and asks for nothing looks the same as one " +
					"that blocks force pushes and demands three checks. Reviews is the number " +
					"of required approvals, Threads whether every conversation must be resolved, " +
					"and Checks how many status checks are required. Reviews is left " +
					"empty rather than filled with a zero where GitHub reports no count at all, " +
					"which is a different state from asking for none.",
				// Short headings, so the last column is inside a half-width
				// panel: "Conversation resolution" at 170 was off its edge.
				Overrides: []any{
					width("Pattern", 120), width("Reviews", 80),
					onOff("Signatures", 100),
					onOff("Linear", 80),
					blockedOrAllowed(deliveryForcePush, 100),
					onOff("Threads", 90),
					width("Checks", 80), ownerLinkOn("Pattern", "the branch settings"),
				},
				GR: protGR, GRTF: protGRtf,
				GRDesc: "Graphite names each row repository and pattern from the path, and a boolean " +
					"is not a metric there at all, so what the protection requires beyond the " +
					"number of status checks is missing. " + grRows,
				ES: protES, ESTF: protEStf,
				ESDesc: "In Elasticsearch all six columns are the largest value inside the range " +
					"rather than the newest reading, because a top_metrics over a boolean panics " +
					"the plugin: a protection switched off inside the range still reads as on " +
					"until the range has moved past the day it was on.",
			}),
		panel("table", "Ruleset rules and bypasses", box{W: 24, H: 8, X: 0, Y: 40},
			[]Target{sqlT(ruleRows)}, &P{
				Prom: []Target{
					promTbl(fmt.Sprintf("max by (repo, ruleset, rule) (github_ruleset_rule_bypass_actors{%s})", PF), "A"),
					promTbl(fmt.Sprintf("max by (repo, ruleset, rule) (github_ruleset_rule_bypass_always{%s})", PF), "B"),
					promTbl(fmt.Sprintf("max by (repo, ruleset, rule) (github_ruleset_rule_bypass_sampled{%s})", PF), "C"),
				},
				PromTF: merged(map[string]string{
					"repo": "Repository", "ruleset": "Ruleset", "rule": "Rule",
					inventoryValueCol + "A": deliveryBypassActors, inventoryValueCol + "B": "Always", inventoryValueCol + "C": "Sampled",
				}, nil, map[string]int{"repo": 0, "ruleset": 1, "rule": 2}),
				Opts: Opts{"sort": "Always"},
				Desc: "Ruleset rules, and who may walk past them. The Rulesets panel above says a " +
					"ruleset exists and is enforced. This says " +
					"what it does and who it does not apply to: a ruleset reported ACTIVE with " +
					"every actor on ALWAYS protects nobody, and neither that panel nor branch " +
					"protection would ever say so. Always is counted over a page of five while " +
					"Bypass actors is the exact total, so Sampled says how many were really " +
					"read, and the comparison is sound only while the three agree.",
				Overrides: []any{
					width("Ruleset", 150), width("Rule", 160), width(deliveryBypassActors, 120),
					width("Always", 90), width("Sampled", 90),
				},
				GR: ruleGR, GRTF: ruleGRtf,
				GRDesc: "Graphite names each row repository, ruleset and rule from the path, and " +
					"has no rows: each series is one number reduced over the range, so this " +
					"table keeps one of the three bypass columns. It keeps Bypass actors, the " +
					"exact total, and not the column the panel is sorted by: a count on ALWAYS " +
					"read alone, against a total this table cannot show beside it, is exactly " +
					"the misreading the panel exists to prevent.",
				ES: ruleES, ESTF: ruleEStf,
				ESDesc: "In Elasticsearch the three bypass numbers are the largest inside the range " +
					"rather than the newest reading, so they still agree with each other.",
			}),
		rulesetChanges(b),
	}
}

// rulesetChanges is the changelog behind the two ruleset panels above: one row
// per saved version of a ruleset, dated when GitHub saved it.
//
// gh_ruleset_version is the one measurement of the protections that is not a
// snapshot, so it is read the way the deployments below are: every row inside
// the range, never the newest per series. What the row answers is the one
// question the snapshots cannot: not what the protection is today but when it
// last changed, and whether the actor was the owner or an app.
func rulesetChanges(b *builder) Panel {
	rv := "gh_ruleset_version"
	// Neither the actor's id nor the version's is in the row: both are
	// GitHub's internal numbers, and with them the date lost its seconds.
	versions := `SELECT ruleset AS "Ruleset", time AS "Date", repo AS "Repository",` +
		` target AS "Target", actor_type AS "Actor", url AS "Link"` +
		" FROM gh_ruleset_version WHERE $__timeFilter(time) AND " + RF +
		" ORDER BY time DESC LIMIT 200"
	// Graphite has no rows and no dates to list by, so the versions become a
	// count per ruleset and actor type, named from the path.
	verGR, verGRtf := gTbl(fmt.Sprintf(`sortBy(groupByNodes(isNonNull(%s), "sum", %d, %d, %d), "sum", true)`,
		rp(rv, "versions"), gn(rv, "repo"), gn(rv, "ruleset"), gn(rv, "actor_type")),
		"Repository, ruleset, actor", []col{{"sum", "Versions"}})
	// The documents themselves, newest first, which is the one shape that
	// returns the target, the actor and the link as strings: a top_metrics
	// over any of them panics the datasource. A version is dated the moment
	// it was saved, which never moves, so its document id is stable and a
	// sweep replaces it rather than adding a duplicate row.
	verES, verEStf := b.esRaw(rv, 200, []named{
		{"@timestamp", "Date"},
		{"repo", "Repository"},
		{"ruleset", "Ruleset"},
		{"target", "Target"},
		{"actor_type", "Actor"},
		{"url", "Link"},
	}, []string{ESF})

	return panel("table", "Ruleset changes", box{W: 24, H: 8, X: 0, Y: 48}, []Target{sqlT(versions)}, &P{
		// The count of the last sweep, not increase() over the monotonic
		// total, for the reason the Sponsorships panel gives: every sweep
		// re-reads the same finite changelog, so the total rises once and an
		// increase() over it reads 0 for every range after the first.
		Prom: []Target{promTbl(fmt.Sprintf(
			"sum by (repo, ruleset, actor_type) (github_ruleset_versions_count{%s})", PF,
		))},
		PromTF: []any{organize(map[string]string{
			"repo": "Repository", "ruleset": "Ruleset", "actor_type": "Actor",
			"Value": "Versions",
		}, []string{"owner", "full_name", "instance", "job", "__name__"},
			map[string]int{"repo": 0, "ruleset": 1, "actor_type": 2})},
		Desc: "Every saved version of every ruleset, dated the moment GitHub saved it. The " +
			"Rulesets panel says a protection exists and how many days ago it last " +
			"changed; this is the changelog that number summarizes, and it is the only " +
			"record GitHub keeps of the moment a protection was switched off and back on. " +
			"GitHub names the actor by type and id and not by login, so the row does too. " +
			"Every store answers inside the dashboard range: a ruleset nobody has touched " +
			"in the range is absent here, not unprotected.",
		PromDesc: "Prometheus counts versions per ruleset and actor type and drops the " +
			"dates, because a series per version would never move again. " + sweepCount,
		Overrides: []any{
			when("Date"), width("Target", 90), width("Actor", 110), linkOn("Ruleset"),
		},
		PromOver: []any{width("Actor", 110), width("Versions", 100)},
		GR:       verGR, GRTF: verGRtf,
		GRDesc: "Graphite has no way to list by date, so this is the number of versions per " +
			"repository, ruleset and actor type over the range, named from the path.",
		ES: verES, ESTF: verEStf,
		ESDesc: "Elasticsearch lists the documents themselves, newest first.",
	})
}

// deploymentsToEnvironments is what reached the environments the section lists
// above: how many deployments a day each one took, and how they went.
//
// gh_deployment is the one measurement of this section that is not a snapshot.
// It is dated when the deployment was created and its row converges in place as
// the status moves, so counting rows over the range is the honest reading, and
// the newest-row-per-series subquery that the three panels above need would
// throw away every deployment but the last.
func deploymentsToEnvironments(b *builder) []Panel {
	dp := "gh_deployment"

	deploysPerDay := "SELECT " + timeBin + ", environment AS series," +
		" COUNT(*) AS deployments FROM gh_deployment WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 2 ORDER BY 1"
	deploys := `SELECT environment AS "Environment", COUNT(*) AS "Deployments",` +
		` repo AS "Repository",` +
		` approx_percentile_cont(seconds_to_status, 0.5) AS "To status",` +
		` approx_percentile_cont(seconds_live, 0.5) AS "Live for",` +
		` SUM(CASE WHEN success THEN 1 ELSE 0 END) AS "Successes",` +
		// outcome is a field: a deployment is pending before it is anything
		// else, and as the tag `state` the one that succeeded after a sweep
		// had seen it pending was two rows at its own instant for ever.
		` SUM(CASE WHEN outcome = 'pending' THEN 1 ELSE 0 END) AS "Pending",` +
		// The deployments page is one per repository; the environment's own
		// url is what the newest deployment put live, when it put anything.
		` MAX(url) AS "Link", MAX(environment_url) AS "Live"` +
		" FROM gh_deployment WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 3 ORDER BY 2 DESC"

	// The outcome is a bucket in Elasticsearch, so the pending deployments
	// are rows of their own there rather than a column. Graphite keeps no
	// strings and the outcome is one now, so its rows are the repository and
	// the environment, with the successes counted from the flag beside it.
	depGR, depGRtf := gTbl(fmt.Sprintf(`sortBy(groupByNodes(%s, "sum", %d, %d), "sum", true)`,
		rp(dp, "success"), gn(dp, "repo"), gn(dp, "environment")),
		"Repository, environment", []col{{"count", "Deployments"}, {"sum", "Successes"}})
	depES, depEStf := esTbl(dp,
		[]any{b.tm("repo", 500), b.tm("environment", 50), b.tm("outcome", 10), b.tmURL(), b.tmURL("environment_url")},
		[]any{b.mCount(), b.mPct("seconds_to_status", 50), b.mPct("seconds_live", 50)},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"environment.keyword", "Environment"},
			{"outcome.keyword", "Outcome"},
			{inventoryURLTerm, "Link"},
			{"environment_url.keyword", "Live"},
			{"n", "Deployments"},
			{"s", deliveryTimeToStatus},
			{"l", deliveryTimeLive},
		}, []string{ESF})

	return []Panel{
		panel("timeseries", "Deployments over time", box{W: 12, H: 8, X: 0, Y: 56},
			[]Target{sqlTS(deploysPerDay)}, &P{
				Prom: []Target{daily(fmt.Sprintf(
					"sum by (environment) (increase(github_deployments_total{%s}[1d]))", PF,
				),
					"{{environment}}")},
				Opts:    mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
				SQLOpts: seriesOpts,
				Desc: "By environment. Dated at each deployment's own creation, not at the sweep " +
					"that read it and not at its latest status: the status goes on moving for " +
					"years afterwards, and the deployment happened once. " + bucketFollowsRange,
				PromDesc: sinceStart,
				GR:       []Target{grq(perBucket("isNonNull("+rp(dp, "deployments")+")", gn(dp, "environment")))},
				ES:       []Target{b.esDaily(dp, b.mCount(), "environment", "", []string{ESF}, "")},
			}),
		panel("table", "Deployments by environment", box{W: 12, H: 8, X: 12, Y: 56}, []Target{sqlT(deploys)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf("sum by (repo, environment) (increase(github_deployments_total{%s}[$__range]))", PF), "A"),
				promTbl(fmt.Sprintf("avg by (repo, environment) (github_deployments_seconds_to_status_mean{%s})", PF), "B"),
				promTbl(fmt.Sprintf("avg by (repo, environment) (github_deployments_seconds_live_mean{%s})", PF), "C"),
				promTbl(fmt.Sprintf(`sum by (repo, environment) (increase(github_deployments_total{outcome="success",%s}[$__range]))`, PF), "D"),
				promTbl(fmt.Sprintf(`sum by (repo, environment) (increase(github_deployments_total{outcome="pending",%s}[$__range]))`, PF), "E"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", "environment": "Environment", inventoryValueCol + "A": "Deployments",
				inventoryValueCol + "B": deliveryTimeToStatus, inventoryValueCol + "C": deliveryTimeLive,
				inventoryValueCol + "D": "Successes", inventoryValueCol + "E": "Pending",
			}, nil, map[string]int{"repo": 0, "environment": 1}),
			Opts: Opts{"sort": "Deployments"},
			Desc: "To status is the median time the deployment took to report one; Live for " +
				"the median time it stayed the current one. They are the same subtraction against two " +
				"different meanings, so each keeps its own field and neither is written over " +
				"the other. The pending rows are kept on purpose: one deployment on this " +
				"account has been held at an approval for seventy four days, and a filter that " +
				"hid the states that are not terminal would delete the most interesting row.",
			PromDesc: sinceStart + " " + lastSweep,
			// Short headings, so Pending is inside a half-width panel.
			Overrides: []any{
				width("Environment", 120), barCell("Deployments", "short", 110),
				unitOf(deliveryTimeToStatus, "s", 90), unitOf(deliveryTimeLive, "s", 90),
				width("Successes", 90), width("Pending", 80),
				ownerLinkOn("Environment", "the deployments"), linkColumnAs("Live", "Open the environment"),
			},
			GR: depGR, GRTF: depGRtf,
			GRDesc: "Graphite names each row repository and environment from the path and keeps " +
				"no strings, so the outcome is out of reach there: Successes counts the " +
				"success flag, and the pending deployments are not told apart. " + grRows,
			ES: depES, ESTF: depEStf,
			ESDesc: "In Elasticsearch each outcome is its own row, split by the `outcome` field.",
		}),
	}
}
