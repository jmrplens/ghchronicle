package dashboards

import (
	"fmt"
	"strings"
)

// The profile's own repeated words: the SQL that reaches a table, the
// Graphite function that carries a daily reading forward, and the column
// titles a panel and its Elasticsearch and Prometheus twins must agree on
// letter for letter or the twin arrives with an empty column.
const (
	profileFrom       = " FROM "
	profileKeepLast   = "keepLastValue("
	profileNextTier   = "Next tier at"
	profilePageAgrees = "Page agrees"
	profileOneTime    = "One-time"
	profileLastAdded  = "Last added"
)

// ── Profile and sponsorship ─────────────────────────────────────────────────

// profileSection is what the profile advertises and what it earns.
//
// Five measurements share a question none of the other sections asks.
// gh_pinned_item and gh_profile_flag are the profile page itself as data, and
// gh_sponsorship is the only dated record of the money: Overview shows
// gh_account.sponsors and gh_account.sponsoring, which are counts as of now
// that say neither when nor to whom, and the Cost section is GitHub's billing
// spend, which is a different pocket entirely.
//
// Only gh_sponsorship is dated when the thing happened. The listing, the pins
// and the flags are stamped now and the tiers at the start of the UTC day, all
// of them rewritten every sweep, so every panel over those asks for the newest
// row per series and never for a sum over the range: summing
// lifetime_received_cents would report the lifetime total once per sweep,
// which is the 3.61K alert tile in a new costume.
//
// Some Elasticsearch columns cannot be read with a top_metrics, and each says
// so where it is built. A top_metrics hands a boolean back as the string
// "true" and panics Grafana's plugin, and for a document that is missing the
// field it appends nothing at all rather than a null, which leaves the metric
// column shorter than the bucket columns and fails the whole panel. Those
// columns take a max inside each item's newest document instead, which is
// the newest reading all the same (see newestDoc). The largest reading of the
// range stood there before, and it was not the newest for the two that can
// fall back: `enabled` on a flag somebody switches off, and `days_since_push`
// on a pin somebody pushes to.
func profileSection(b *builder) []Panel {
	out := append(sponsorship(b), profileStanding(b)...)
	return append(out, starLists(b), achievements(b), achievementProgress(b))
}

// achievements is the badge shelf of the profile: one row per badge, its
// tier and the color GitHub gives the tier. A snapshot a day, read from the
// profile page every hour since no API lists them, so the newest row per
// badge is the shelf as last seen.
func achievements(b *builder) Panel {
	const ac = "gh_achievement"
	rows := `SELECT name AS "Achievement", tier_number AS "Tier", tier_name AS "Level",` +
		` url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY achievement ORDER BY time DESC) AS rn" +
		profileFrom + ac + " WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 2 DESC, 1, achievement"
	gr, grtf := gTbl(rowsOf(profileKeepLast+gp(ac, "tier_number")+")", gn(ac, "achievement")),
		"Achievement", []col{{"lastNotNull", "Tier"}})
	es, estf := esTbl(ac, []any{b.tm("achievement", 50, "_key", "asc"), b.tmURL()},
		[]any{b.mNewest("tier_number")},
		[]named{
			{"achievement.keyword", "Achievement"},
			{panelURLField, "Link"},
			{"tier_number", "Tier"},
		}, nil)
	// Ten rows high: the profile it was written against has eight badges,
	// and a shelf that scrolls inside a six-row table hides half of them.
	// Nine was measured at 1920 and at 430 to show seven of the eight, the
	// eighth reached only by a scroll inside the table.
	return panel("table", "Achievements", box{W: 24, H: 10, X: 0, Y: 31}, []Target{sqlT(rows)}, &P{
		Prom:   []Target{promTbl("max by (achievement) (github_achievement_tier_number)")},
		PromTF: []any{organize(map[string]string{"achievement": "Achievement", "Value": "Tier"}, []string{"user"})},
		Opts:   Opts{"sort": "Tier"},
		Desc: "The badges on the profile, highest tier first: Pull Shark, Pair " +
			"Extraordinaire, YOLO and the rest, read every hour from the public " +
			"profile page because no API lists them. Tier is the number on the " +
			"badge's label, one where it has none; Level is the color GitHub gives " +
			"that tier. Empty until the achievements family has run once.",
		PromDesc: "Prometheus keeps the badge by its slug and its tier; the display name " +
			"and Level, the tier's color, are strings and do not survive as metrics.",
		Overrides: []any{width("Tier", 80), width("Level", 100), linkOn("Achievement")},
		GR:        gr, GRTF: grtf,
		GRDesc: "Graphite names each row by the badge's slug and keeps its tier. " + grRows,
		ES:     es, ESTF: estf,
		ESDesc: "Elasticsearch names each row by the badge's slug; the display name and " +
			"Level, the tier's color, are strings a top metric cannot carry.",
	})
}

// achievementProgress is how far each tiered badge is from its next tier:
// the count that decides it today, the threshold the next tier starts at,
// and the distance between as a bar, beside the tier the profile page shows
// and whether the rule agrees with it. Four rows, one per badge that has
// tiers; the single-tier badges are on the shelf above and have no next tier
// to measure against. A daily snapshot like the shelf, so the newest row per
// badge is the answer.
func achievementProgress(b *builder) Panel {
	const ap = "gh_achievement_progress"
	// The bar third, right after the name: on a phone the table shows its
	// first three columns, and the bar is what the panel is for.
	rows := `SELECT image AS "Badge", name AS "Achievement", percent AS "Progress", count AS "Count",` +
		` next_threshold AS "Next tier at", tier_number AS "Tier", agrees AS "Page agrees",` +
		` url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY achievement ORDER BY time DESC) AS rn" +
		profileFrom + ap + " WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 3 DESC, 2, achievement"
	gr, grtf := gTbl(rowsOf(profileKeepLast+gp(ap, "percent")+")", gn(ap, "achievement")),
		"Achievement", []col{{"lastNotNull", "Progress"}})
	// The badge image and the url reach the table as buckets of one value
	// each, the way the url does everywhere: a top metric over a string
	// panics the plugin. The badge is named by its slug, as on the shelf,
	// and the numbers are the newest document's own, each a max inside it
	// rather than a top_metrics: a badge whose page disagrees with its count
	// carries no percent and no next threshold, and a top_metrics appends
	// nothing for them. Measured against Grafana 13.2.1 and Elasticsearch
	// 9.5.3 with one badge of each kind, the whole panel failed with "frame
	// has different field lengths"; a max answers null. See newestDoc.
	es, estf := esTbl(ap, []any{
		b.tm("achievement", 50, "_key", "asc"), b.newestDoc(), b.tmURL("image"), b.tmURL(),
	}, []any{
		b.mMax("percent"), b.mMax("count"), b.mMax("next_threshold"),
		b.mMax("tier_number"), b.mMax("agrees"),
	},
		[]named{
			{"achievement.keyword", "Achievement"},
			{"image.keyword", "Badge"},
			{panelURLField, "Link"},
			{"percent", "Progress"},
			{"count", "Count"},
			{"next_threshold", profileNextTier},
			{"tier_number", "Tier"},
			{"agrees", profilePageAgrees},
		}, nil, hideColumns(panelESTime))
	var prom []Target
	for i, field := range []string{"percent", "count", "next_threshold", "tier_number", "agrees"} {
		prom = append(prom, promTbl(fmt.Sprintf("max by (achievement) (github_achievement_progress_%s)", field), string(rune('A'+i))))
	}
	overrides := []any{
		badgeCell("Badge"), width("Achievement", 170), width("Tier", 100), width("Count", 90),
		width(profileNextTier, 110), progressCell("Progress"), agreesCell(profilePageAgrees),
		linkOn("Achievement"), tierNames("Tier"), topTier(profileNextTier),
	}
	// Eight rows high: four badges drawn at the larger cell height the badge
	// image needs to be a badge and not a dot.
	return panel("table", "Achievement progress", box{W: 24, H: 8, X: 0, Y: 41}, []Target{sqlT(rows)}, &P{
		Prom: prom,
		PromTF: merged(map[string]string{
			"achievement": "Achievement", panelValueA: "Progress", panelValueB: "Count",
			panelValueC: profileNextTier, panelValueD: "Tier", panelValueE: profilePageAgrees,
		}, []string{"user"}),

		// No sort option: the SQL stores order by progress, and on a phone
		// a sorted column has to be one of the first two, which are the
		// badge and its name. Four rows read in any order.
		Opts: Opts{"cell_height": "lg"},
		Desc: "How far each tiered badge is from its next tier. Count is what decides " +
			"the badge today: merged pull requests for Pull Shark, accepted discussion " +
			"answers for Galaxy Brain, the stars on the most starred repository for " +
			"Starstruck, and merged pull requests in public repositories with a " +
			"co-authored commit for Pair Extraordinaire. That last count is a tally the " +
			"state file keeps: each hourly pass adds the pull requests merged since the " +
			"last day it covers, and the whole history is walked again once a week, which " +
			"is when a count that went down, a repository made private or deleted, comes " +
			"down here. Without a state file every start walks it whole. Next tier " +
			"at is the community-observed threshold (Schweinepriester/github-profile-" +
			"achievements), and Progress is Count against it, full at the top tier. " +
			"Tier is what the count implies; Page agrees says whether the profile page " +
			"shows the same tier, and a row that disagrees is a rule the page " +
			"contradicts, so it carries no target and no bar. The single-tier badges are " +
			"on the shelf above. Empty until the achievements family has run once.",
		PromDesc: "Prometheus keeps the badge by its slug and its numbers; the display " +
			"name and the badge image are strings and do not survive as metrics.",
		Overrides: overrides,
		GR:        gr, GRTF: grtf,
		GRDesc: "Graphite names each row by the badge's slug and keeps its progress. " + grRows,
		ES:     es, ESTF: estf,
		ESDesc: "Elasticsearch names each row by the badge's slug and reads the badge " +
			"image and the url as one-value buckets beside it; the numbers are the " +
			"newest document's own.",
	})
}

// badgeCell draws a column of image urls as the images, at the width a badge
// needs to be recognizable.
func badgeCell(name string) any {
	return override(name, []any{
		map[string]any{"id": panelCellOptionsField, "value": map[string]any{
			"type": "image", "alt": "badge", "title": "The badge as the profile shows it",
		}},
		map[string]any{"id": panelWidthField, "value": 72},
	})
}

// progressCell is a bar from zero to a hundred per cent, whatever the other
// rows read: a badge at its top tier is a full bar, and a badge at ten per
// cent is a tenth of the cell, not a tenth of the longest bar in the column.
func progressCell(name string) any {
	return override(name, []any{
		map[string]any{"id": panelCellOptionsField, "value": map[string]any{
			"type": "gauge", "mode": "gradient",
		}},
		map[string]any{"id": "unit", "value": "percent"},
		map[string]any{"id": "decimals", "value": 1},
		map[string]any{"id": "min", "value": 0},
		map[string]any{"id": "max", "value": 100},
		map[string]any{"id": "thresholds", "value": map[string]any{
			"mode": "absolute", "steps": []any{map[string]any{"color": barColor, "value": nil}},
		}},
		// A floor rather than a width: at the table's ninety pixel minimum
		// the bar was a stub beside its number on a phone, and on a desktop
		// the column takes what the fixed ones leave.
		map[string]any{"id": "custom.minWidth", "value": 160},
	})
}

// agreesCell renders the agreement as a word, and a disagreement in red:
// it is the one cell of the row that says the rest may be wrong.
func agreesCell(name string) any {
	return override(name, []any{
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "value", "options": flagWords("disagrees", "red"),
		}}},
		map[string]any{"id": panelCellOptionsField, "value": map[string]any{"type": "color-text"}},
		map[string]any{"id": panelWidthField, "value": 110},
	})
}

// tierNames renders the implied tier the way the shelf labels it: the
// multiplier and the color of the label.
func tierNames(name string) any {
	return override(name, []any{
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "value", "options": map[string]any{
				"0": map[string]any{"text": "not yet", "index": 0},
				"1": map[string]any{"text": "x1", "index": 1},
				"2": map[string]any{"text": "x2 bronze", "index": 2},
				"3": map[string]any{"text": "x3 silver", "index": 3},
				"4": map[string]any{"text": "x4 gold", "index": 4},
			},
		}}},
	})
}

// topTier says so where there is no next threshold: the collector writes
// zero at the top tier, and a target of zero reads as a bug.
func topTier(name string) any {
	return override(name, []any{
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "value", "options": map[string]any{
				"0": map[string]any{"text": "top tier", "index": 0},
			},
		}}},
	})
}

// The money is in cents, and the four stats divide it in all five dialects, so
// no store quietly disagrees with its neighbor by a hundred: InfluxDB and
// PostgreSQL by 100.0, Prometheus in the expression, Graphite with scale() and
// Elasticsearch with a server-side math expression over the reduced query.
//
// The two money columns of the two tables are the exception, and it is a shape
// rather than a choice. Elasticsearch's arithmetic is that same server-side
// expression, which reduces a series to a single number and has nothing to say
// about a table of rows; the aggregation that fills the rows returns the
// stored field and no more. So those two columns keep cents and say so in
// their own heading, which is the other honest branch of the rule.
const esCents = "Elasticsearch hands the stored field back as it is. The arithmetic in the " +
	"stats above is a server-side expression, and those reduce a series to one number, " +
	"which is not a shape a table of rows has: so the money column here keeps cents and " +
	"its heading says so rather than reading as a hundredfold overcharge."

const esMoney = "In Elasticsearch each day is reduced to its largest reading, the last day that " +
	"has one is taken, and the division into dollars is a server-side expression."

// profileBool renders a stored boolean as a word.
func profileBool(name string, w int) any {
	return override(name, []any{
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "value", "options": flagWords("no", "text"),
		}}},
		map[string]any{"id": panelCellOptionsField, "value": map[string]any{"type": "color-text"}},
		map[string]any{"id": panelWidthField, "value": w},
	})
}

// sponsorship is the money: what the sponsors listing holds right now, every
// sponsorship that was ever made in either direction, and the tiers on offer.
func sponsorship(b *builder) []Panel {
	sl, sp, st := "gh_sponsors_listing", "gh_sponsorship", "gh_sponsors_tier"

	// The newest row of a snapshot that has one series, not a sum over the
	// range: the listing is rewritten every sweep and its totals are already
	// totals. Four of them in one panel rather than four tiles, because four
	// tiles were 640 pixels of a phone for what was, at the time of the
	// review, fifteen dollars.
	sponsorFields := []named{
		{"lifetime_received_cents", "Lifetime received"},
		{"monthly_income_cents", "Monthly income"},
		{"next_payout_cents", "Next payout"},
		{"sponsor_spend_cents", "Spent sponsoring"},
	}
	money := &P{
		Desc: "Every cent this account has ever been paid through Sponsors, then GitHub's " +
			"own estimate of what the sponsorships active right now pay per month, what " +
			"is waiting for the next payout date, and what this account pays out to the " +
			"projects it sponsors. The lifetime total is the one that keeps the history: " +
			"the monthly estimate goes to 0 the moment the last sponsorship lapses, and " +
			"the money that did arrive stops being visible anywhere else. What is spent " +
			"sponsoring is not the spend in the Cost section, which is what GitHub " +
			"charges for Actions, packages and storage; this is money that leaves for " +
			"somebody else's work. The payout date itself sits on the same row as a " +
			"string, which no store here can draw as a number. All four are the newest " +
			"reading of a snapshot rewritten every sweep, not a sum over the range, " +
			"which would report the lifetime total once per sweep.",
		Opts:   Opts{"unit": "currencyUSD"},
		ESDesc: esMoney,
	}
	var moneyCols []string
	for i, f := range sponsorFields {
		moneyCols = append(moneyCols, fmt.Sprintf("%s / 100.0 AS %q", f.From, f.To))
		money.Prom = append(money.Prom,
			promNamed(ref(i), f.To, "github_sponsors_listing_"+f.From+" / 100"))
		money.GR = append(money.GR,
			grNamed(ref(i), f.To, fmt.Sprintf("scale(%s, 0.01)", gp(sl, f.From))))
		// A max per day and the last of those, not a top_metrics: the
		// server-side expressions read a series, and an end to end run
		// found that a top_metrics inside a date histogram is not one it
		// can reduce, which failed all four of these panels with
		// sse.dependencyError. A max is the same shape the webhook failure
		// gauge feeds its expressions, and the only day it decides is the
		// newest one that has a reading at all, since an empty bucket is
		// null and dropNN drops it. Three targets per value, so the four
		// walk the alphabet in threes and only the last of each is drawn.
		q, reduce, math := ref(3*i), ref(3*i+1), ref(3*i+2)
		money.ES = append(money.ES, append(collected(
			b.esDaily(sl, b.mMax(f.From), "", "", nil, q),
			exprT(reduce, "reduce", "$"+q, map[string]any{
				"reducer": "last", "settings": map[string]any{"mode": "dropNN"},
			}),
		), exprT(math, "math", "$"+reduce+" / 100", nil))...)
		money.ESOver = append(money.ESOver, frameName(math, f.To))
	}

	// Two hundred rows because that is the collector's own ceiling: it reads
	// first: 100 from each of the two connections and both land in this one
	// table, so a hundred would drop the older half of a busy maintainer's
	// history while the description above it promises every sponsorship.
	sponsorships := `SELECT sponsorable AS "Sponsorable", time AS "Date",` +
		` direction AS "Direction", tier AS "Tier",` +
		` amount_cents / 100.0 AS "Amount",` +
		` CAST(active AS INT) AS "Active", CAST(one_time AS INT) AS "One-time",` +
		` url AS "Link"` +
		profileFrom + sp + " WHERE " + wholeHistory + " ORDER BY time DESC, direction, sponsorable LIMIT 200"
	tiers := `SELECT tier AS "Tier", price_cents / 100.0 AS "Price",` +
		` CAST(one_time AS INT) AS "One-time", CAST(retired AS INT) AS "Retired",` +
		` age_days AS "Age", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY tier ORDER BY time DESC) AS rn" +
		profileFrom + st + " WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 2 DESC, 1"

	// Graphite has no rows and no dates to list by, so the sponsorships become
	// the price of the tier each was made at, named from the path. A sponsorship
	// whose tier GitHub no longer publishes carries no amount_cents and so has no
	// series here at all.
	spGR, spGRtf := gTbl(rowsOf(fmt.Sprintf("sortByMaxima(scale(%s, 0.01))", gp(sp, "amount_cents")),
		gn(sp, "direction"), gn(sp, "sponsorable")),
		"Direction, sponsorable", []col{{"lastNotNull", "Amount"}})
	// The documents themselves, which is the one shape that returns the tier,
	// the privacy and the link as they are stored. A terms aggregation would
	// need those strings as buckets and a top_metrics over one of them panics
	// the datasource; here nothing is aggregated at all. Every sponsorship is
	// dated the day it began, which never moves, so its document id is stable and
	// a sweep replaces it rather than adding a duplicate row.
	spES, spEStf := b.esRaw(sp, 200, []named{
		{"@timestamp", "Date"},
		{"direction", "Direction"},
		{"sponsorable", "Sponsorable"},
		{"tier", "Tier"},
		{"amount_cents", "Amount (cents)"},
		{"active", "Active"},
		{"one_time", profileOneTime},
		{"url", "Link"},
	}, nil)

	tierGR, tierGRtf := gTbl(rowsOf(fmt.Sprintf("scale(keepLastValue(%s), 0.01)", gp(st, "price_cents")),
		gn(st, "tier")), "Tier", []col{{"lastNotNull", "Price"}})
	// The price as the newest reading of each tier, the two booleans and the
	// age as a max inside the tier's newest document. A top_metrics hands a
	// boolean back as the string "true" and Grafana's Elasticsearch plugin
	// converts a top_metrics value to a float without checking the type, which
	// takes the whole panel with it. A max over the same field is answered as
	// 1 or 0.
	tierES, tierEStf := esTbl(st, []any{b.tm("tier", 50), b.newestDoc(), b.tm("url", 5)},
		[]any{b.mNewest("price_cents"), b.mMax("one_time"), b.mMax("retired"), b.mMax("age_days")},
		[]named{
			{"tier.keyword", "Tier"},
			{panelURLField, "Link"},
			{"price_cents", "Price (cents)"},
			{"one_time", profileOneTime},
			{"retired", "Retired"},
			{"age_days", "Age"},
		}, nil, hideColumns(panelESTime))

	return []Panel{
		statGroup("Sponsorship", box{W: 24, H: 4, X: 0, Y: 0}, []Target{sqlT(
			"SELECT " + strings.Join(moneyCols, ", ") + profileFrom + sl +
				" WHERE $__timeFilter(time) ORDER BY time DESC LIMIT 1",
		)}, money),
		panel("table", "Sponsorships", box{W: 12, H: 10, X: 0, Y: 4}, []Target{sqlT(sponsorships)}, &P{
			// The count of the last sweep, not increase() over the monotonic
			// total. Every sweep re-reads the same finite set of sponsorships,
			// so that total rises once, when the exporter first sees them, and
			// never again: an increase() over it answers zero for every range
			// after the first and the column would read 0 for ever. The count
			// is the smaller, real answer, which is what gh_repo_created does
			// with the same shape of history.
			Prom: []Target{
				promTbl("sum by (direction) (github_sponsorships_count)", "A"),
				promTbl("avg by (direction) (github_sponsorships_amount_cents_mean) / 100", "B"),
				promTbl("avg by (direction) (github_sponsorships_active_mean)", "C"),
				promTbl("avg by (direction) (github_sponsorships_one_time_mean)", "D"),
			},
			PromTF: merged(map[string]string{
				"direction": "Direction", panelValueA: "Sponsorships",
				panelValueB: "Mean amount", panelValueC: "Active share",
				panelValueD: "One-time share",
			}, []string{"user"}),

			// The count where the SQL stores draw the tier each one is at,
			// and the mean where they draw each amount.
			PromAt: map[string]string{"Sponsorships": "Tier", "Mean amount": "Amount"},
			Desc: "Every sponsorship in either direction, dated the day it began and not the day " +
				"of any payment. Amount is the price of the tier it was made at, which is a rate " +
				"per month unless the column beside it says the payment was one-time: a five " +
				"dollar a month sponsorship running since 2021 is one row, dated 2021, reading " +
				"five dollars, and the lifetime tile above is the only figure here that adds " +
				"the money up. Both connections are read with activeOnly off, which is what " +
				"recovers a lapsed one, so a row here can be a sponsorship from years ago that " +
				"no current count still shows. Sponsorable is the literal word private when " +
				"the sponsorship hides the other party, and no link is written then rather " +
				"than one being guessed at. " +
				"There is deliberately no curve of this beside it: on a real account the dated " +
				"record is a handful of rows spread across years, and drawn against any range a " +
				"reader would pick it is an empty axis. It is an inventory, so it names its " +
				"own window rather than taking the dashboard's: a sponsorship made in 2021 " +
				"is listed at the default thirty days, and the Sponsoring tile on the " +
				"Overview and this table count the same thing.",
			PromDesc: "Prometheus counts sponsorships per direction and drops the other party, " +
				"the date and the tier, because a series per sponsorable would never move again. So this is one row " +
				"for money in and one for money out, with the mean amount and the two shares " +
				"beside the count. " + sweepCount + " It is the one store here that is not " +
				"bound by the dashboard range, so a lapsed sponsorship is counted in it " +
				"whatever range is selected.",
			Overrides: []any{
				when("Date"), unitOf("Amount", "currencyUSD", 90),
				profileBool("Active", 80), profileBool(profileOneTime, 90), linkOn("Sponsorable"),
			},
			PromOver: []any{
				unitOf("Mean amount", "currencyUSD", 130),
				unitOf("Active share", "percentunit", 110),
				unitOf("One-time share", "percentunit", 130),
			},
			GR: spGR, GRTF: spGRtf,
			GRDesc: "Graphite has no way to list by date, so this is the price of the tier each " +
				"sponsorship was made at, named direction and sponsorable from the path. " + grRows + " " + grRange,
			ES: spES, ESTF: spEStf,
			ESDesc: "Elasticsearch lists the documents themselves, newest first. " + esCents + " " + esRange,
		}),
		panel("table", "Sponsorship tiers", box{W: 12, H: 10, X: 12, Y: 4}, []Target{sqlT(tiers)}, &P{
			Prom: []Target{
				promTbl("max by (tier) (github_sponsors_tier_price_cents) / 100", "A"),
				promTbl("max by (tier) (github_sponsors_tier_one_time)", "B"),
				promTbl("max by (tier) (github_sponsors_tier_retired)", "C"),
				promTbl("max by (tier) (github_sponsors_tier_age_days)", "D"),
			},
			PromTF: merged(map[string]string{
				"tier": "Tier", panelValueA: "Price", panelValueB: profileOneTime,
				panelValueC: "Retired", panelValueD: "Age",
			}, []string{"user"}),

			Opts: Opts{"sort": "Price"},
			Desc: "Standing inventory, the way an SSH key is. The listing says how many tiers " +
				"there are; this says which and at what price. Dating a tier at its creation " +
				"would put all of them outside every dashboard range, where they would read as " +
				"no tiers at all, so they are anchored to the start of the UTC day and the " +
				"creation date survives as an age.",
			Overrides: []any{
				unitOf("Price", "currencyUSD", 90), profileBool(profileOneTime, 90),
				profileBool("Retired", 85), unitOf("Age", "d", 70), linkOn("Tier"),
			},
			GR: tierGR, GRTF: tierGRtf,
			GRDesc: "Graphite keeps the price, which is the column this table sorts by. " + grRows,
			ES:     tierES, ESTF: tierEStf,
			ESOpts: Opts{"sort": "Price (cents)"},
			ESDesc: esCents,
		}),
	}
}

// profileStanding is the profile page itself: what it shows first, and the
// badges it wears.
func profileStanding(b *builder) []Panel {
	pi, pf := "gh_pinned_item", "gh_profile_flag"

	// `position` is quoted because PostgreSQL knows it as the name of a
	// function as well as a column, and the panel reads it bare in the select
	// list of both dialects.
	pins := `SELECT "position" AS "Position", repo AS "Item", kind AS "Kind",` +
		` stars AS "Stars", days_since_push AS "Idle", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY full_name ORDER BY time DESC) AS rn" +
		profileFrom + pi + " WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 1, full_name"
	// Two columns and the link. The measurement carries a third field, the
	// days since the availability status was set, but on one row of eight,
	// and on live data that row read four years on a flag that was off: it
	// is the age of the status message, not of the flag. Seven empty cells
	// and one that says something else are not a column. GitHub says when
	// none of the badges was granted, and the day the collector first saw a
	// flag true would be the install date dressed as a grant date, so there
	// is no Since column either.
	flags := `SELECT flag AS "Flag", CAST(enabled AS INT) AS "Enabled",` +
		` url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY flag ORDER BY time DESC) AS rn" +
		profileFrom + pf + " WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 1"

	// Sorted like its four siblings rather than left in the order the paths
	// expanded in. A reduced Graphite table comes back one row per series, named
	// by the pin, and the panel's own claim is that it reads in the order the
	// profile arranges them: without this the slot is a column the reader has to
	// sort by hand, on the one store of the five where the rows arrive by name.
	pinGR, pinGRtf := gTbl(rowsOf(profileKeepLast+gp(pi, "position")+")", gn(pi, "repo")),
		"Item", []col{{"lastNotNull", "Position"}})
	pinGRtf = append(pinGRtf, sortAsc("Position"))
	// The kind and the link as buckets and never as metrics: they are strings,
	// and a top_metrics over one panics Grafana's Elasticsearch plugin. A pin
	// has exactly one of each, so bucketing by them costs no rows. `position`
	// is written on every pin, so it can be the newest reading, which is what
	// a rearrangement moves.
	//
	// `stars` and `days_since_push` cannot be, and the reason is the one the
	// deploy keys panel already met: a gist carries neither field, and for a
	// bucket whose top document is missing a top_metrics field the plugin
	// appends nothing at all rather than a null, so the metric column comes
	// back shorter than the bucket columns and the whole panel fails with
	// `frame has different field lengths`. A max appends a null and the gist
	// keeps its row with two empty cells, and taken inside the pin's newest
	// document it is the newest reading: `days_since_push` resets to 0 on a
	// push, and the largest of the range was the staleness from before it.
	pinES, pinEStf := esTbl(pi, append(b.tmRepo(20), b.newestDoc(), b.tm("kind", 5), b.tm("url", 20)),
		[]any{b.mNewest("position"), b.mMax("stars"), b.mMax("days_since_push")},
		[]named{
			{"repo.keyword", "Item"},
			{"kind.keyword", "Kind"},
			{panelURLField, "Link"},
			{"position", "Position"},
			{"stars", "Stars"},
			{"days_since_push", "Idle"},
		}, nil, hideColumns(panelFullNameField, panelESTime), sortAsc("Position"))

	flagGR, flagGRtf := gTbl(rowsOf(profileKeepLast+gp(pf, "enabled")+")", gn(pf, "flag")),
		"Flag", []col{{"lastNotNull", "Enabled"}})
	flagGRtf = append(flagGRtf, sortAsc("Flag"))
	// A max inside each flag's newest document: `enabled` is a boolean, which a
	// top_metrics hands back as text. See newestDoc.
	flagES, flagEStf := esTbl(pf, []any{b.tm("flag", 10, "_key", "asc"), b.newestDoc(), b.tmURL()},
		[]any{b.mMax("enabled")},
		[]named{
			{"flag.keyword", "Flag"},
			{panelURLField, "Link"},
			{"enabled", "Enabled"},
		}, nil, hideColumns(panelESTime))

	return []Panel{
		// No repository filter anywhere in this panel. `repo` is the bare
		// name here like everywhere else now, but a pinned gist is named by
		// its hash and the variable is built from gh_repo, so each of the five
		// filters would drop every gist from a closed list of six rows.
		//
		// Ten rows high, like Achievements: the two tables on this row are
		// closed lists (six pinned items is GitHub's ceiling, eight flags is
		// the whole list) and seven was measured to show five of each, the
		// new sponsors_listing flag reached only by a scroll inside the
		// table. The pair above is the same height because the tier list of
		// the profile this was written against has eight rows, which eight
		// showed six of.
		panel("table", "Pinned items", box{W: 12, H: 10, X: 0, Y: 14}, []Target{sqlT(pins)}, &P{
			Prom: []Target{
				promTbl("max by (full_name, repo) (github_pinned_item_position)", "A"),
				promTbl("max by (full_name, repo) (github_pinned_item_stars)", "B"),
				promTbl("max by (full_name, repo) (github_pinned_item_days_since_push)", "C"),
			},
			PromTF: append(merged(map[string]string{
				"repo": "Item", panelValueA: "Position", panelValueB: "Stars",
				panelValueC: "Idle",
			}, []string{"user"}),

				sortAsc("Position")),
			Desc: "What the profile shows first, in the order it shows it. The row worth seeing " +
				"is a pinned repository nobody has pushed to in two years. Position is a field " +
				"and not a tag on purpose: a repository that moves from slot two to slot three " +
				"is the same pin, and as a tag every rearrangement would fork the series. The " +
				"repository filter at the top of the dashboard does not reach this panel, " +
				"because a pin can be a gist, which is named by its hash and is in no " +
				"repository the filter knows.",
			PromDesc: "Prometheus keeps the numbers; the kind is a string and does not survive " +
				"as a metric.",
			Overrides: []any{
				width("Position", 85), width("Kind", 95), width("Stars", 75),
				unitOf("Idle", "d", 70), linkOn("Item"),
			},
			GR: pinGR, GRTF: pinGRtf,
			GRDesc: "Graphite has no strings either, so this is the slot each pin sits in. " + grRows,
			ES:     pinES, ESTF: pinEStf,
		}),
		panel("table", "Profile flags", box{W: 12, H: 10, X: 12, Y: 14}, []Target{sqlT(flags)}, &P{
			Prom: []Target{promTbl("max by (flag) (github_profile_flag_enabled)")},
			PromTF: []any{organize(map[string]string{
				"flag": "Flag", "Value": "Enabled",
			}, []string{"user"}), sortAsc("Flag")},
			Desc: "A closed list of eight flags the profile advertises, as they read at the " +
				"last sweep. Seven are booleans that change once in years, and the day one " +
				"does is the day worth being able to point at: hireable, developer program, " +
				"campus expert, GitHub star, bounty hunter, employee and whether the account " +
				"has a Sponsors listing. The eighth is the availability status, carried as a " +
				"flag rather than as a measurement of its own. Its message and how long ago " +
				"it was set are collected on that row and shown nowhere here: the message is " +
				"text, and the age belongs to the status message rather than to the flag, on " +
				"one row of eight, so it was a column with seven empty cells and one that " +
				"read the age of a message beside a flag that was off. GitHub says when " +
				"none of the badges was granted, so there is no date for the others to fill.",
			Overrides: []any{
				width("Flag", 150), profileBool("Enabled", 100), linkOn("Flag"),
			},
			GR: flagGR, GRTF: flagGRtf,
			GRDesc: "Graphite carries all eight booleans. " + grRows,
			ES:     flagES, ESTF: flagEStf,
		}),
	}
}

// starLists is how the account organizes the stars it gives: one row per
// list, with how many stars it holds.
//
// A daily snapshot like the tiers and the pins, read the same way: the newest
// row per list, never a sum over the range. A list carries two dates, when it
// was made and when a star last went into it, and both arrive as ages, since
// stamping the row at either would put a list made in 2024 outside every
// dashboard range. The list is named by its slug, which is what its page is
// addressed by; the display name is a field and, being text, is a column in
// none of the stores.
func starLists(b *builder) Panel {
	sl := "gh_star_list"
	lists := `SELECT list AS "List", items AS "Items", CAST(private AS INT) AS "Private",` +
		` age_days AS "Age", days_since_add AS "Last added", url AS "Link" FROM (` +
		"SELECT *, ROW_NUMBER() OVER (PARTITION BY list ORDER BY time DESC) AS rn" +
		profileFrom + sl + " WHERE $__timeFilter(time)) x WHERE rn = 1 ORDER BY 2 DESC, 1"

	listGR, listGRtf := gTbl(rowsOf(profileKeepLast+gp(sl, "items")+")", gn(sl, "list")),
		"List", []col{{"lastNotNull", "Items"}})
	// The item count as the newest reading of each list, the age and the days
	// since the last addition as a max inside the list's newest document:
	// `days_since_add` is written only for a list a star has ever gone into,
	// so a top_metrics over an empty list appends nothing and the metric
	// column comes back shorter than the bucket columns. A max appends a null
	// and the row keeps its shape, and inside the newest document it is the
	// newest reading, where the largest of the range was the staleness from
	// before the last addition.
	//
	// No Private column here. The datasource asks every aggregation of the
	// whole `ghchronicle-*` pattern, and `private` is a tag, so text, on
	// gh_notification and gh_contribution_day_repo: a max over it is refused
	// on those shards and the panel answers with no rows at all, which is how
	// the Social accounts panel was blank before the containerised suite
	// measured it. The boolean is the store's to show the other four ways.
	listES, listEStf := esTbl(sl, []any{b.tm("list", 100), b.newestDoc(), b.tm("url", 5)},
		[]any{b.mNewest("items"), b.mMax("age_days"), b.mMax("days_since_add")},
		[]named{
			{"list.keyword", "List"},
			{panelURLField, "Link"},
			{"items", "Items"},
			{"age_days", "Age"},
			{"days_since_add", profileLastAdded},
		}, nil, hideColumns(panelESTime))

	// No repository filter: a list is the account's, not a repository's.
	return panel("table", "Star lists", box{W: 24, H: 7, X: 0, Y: 24}, []Target{sqlT(lists)}, &P{
		Prom: []Target{
			promTbl("max by (list) (github_star_list_items)", "A"),
			promTbl("max by (list) (github_star_list_private)", "B"),
			promTbl("max by (list) (github_star_list_age_days)", "C"),
			promTbl("max by (list) (github_star_list_days_since_add)", "D"),
		},
		PromTF: merged(map[string]string{
			"list": "List", panelValueA: "Items", panelValueB: "Private",
			panelValueC: "Age", panelValueD: profileLastAdded,
		}, []string{"user"}),

		Opts: Opts{"sort": "Items"},
		Desc: "The lists the account files its stars into, and how many each holds. " +
			"gh_star_given records every star the account gave and the Overview counts " +
			"them; this is how they are organized, which exists nowhere else. It rides in " +
			"the account query that was already being paid for, at no extra cost. The " +
			"row is a daily snapshot: Age is how long ago the list was made and Last " +
			"added how long ago a star last went into it, both as days so the row can " +
			"sit inside any dashboard range.",
		Overrides: []any{
			width("Items", 90), profileBool("Private", 90), unitOf("Age", "d", 80),
			unitOf(profileLastAdded, "d", 110), linkOn("List"),
		},
		GR: listGR, GRTF: listGRtf,
		GRDesc: "Graphite keeps the item count, which is the column this table sorts by. " + grRows,
		ES:     listES, ESTF: listEStf,
		ESDesc: "A list nobody has added to has no Last added at all, in this dashboard " +
			"or any of the others, and its cell is empty. " +
			"Private is not a column here: the datasource aggregates over every " +
			"index at once, and the same name is a text tag on notifications and " +
			"daily contributions, whose shards refuse a max over it.",
	})
}
