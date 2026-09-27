package dashboards

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// "Every repository, ever" and then "Security features" read a snapshot
// through MAX() over the dashboard range, so a feature switched off inside
// the range read as on, and the audit of 2.6.1 found the same class in a
// dozen more places: "Account keys" put the least days since use of the range
// under a column that means the newest, which on a daily snapshot is the
// reading of the range's first day, while Graphite read the newest; and
// Elasticsearch took the largest reading inside the range in every table
// whose booleans or absent numbers a top_metrics cannot read, saying so in
// its descriptions, when a max inside each series' newest document reads the
// newest all the same.
//
// rangeExtremes is every panel that still takes a maximum or a minimum over
// the range in some store, with why the extreme is what that panel means. A
// panel off this list that takes one fails TestNoPanelReadsACurrentValueAsAnExtreme,
// and has to be made to read each series' newest row or be added here with
// its reason.
var rangeExtremes = map[string]string{
	"Contribution calendar": "MAX(contributions) OVER () is the scale of the grid: " +
		"each day is shaded against the busiest day shown, as GitHub shades it.",
	"Queue wait over time": "Worst queue is the longest wait of each bucket, a peak by design.",
	"Slowest jobs":         "the slowest run of each job, a peak by design.",
	"Slowest steps":        "the slowest run of each step, a peak by design.",
	"Largest merged pull requests": "a merged pull request's size no longer changes, " +
		"and the largest are what the table lists.",
	"Commits behind a red branch": "MAX(run_number) is the latest of one commit's failed " +
		"runs: the rows are runs, events and not readings.",
	"Comments left": "whether any comment of a repository was left elsewhere: the rows are " +
		"comments, events and not readings.",
	"Webhooks configured": "MAX(events) is taken over one row per hook, its newest.",
	"Scan results by tool": "the most rules any analysis of the range ran with, beside the " +
		"results those analyses found and the runs they were: analyses are events.",
	"Usage by repository": "the unit price of the usage summed beside it over the same " +
		"range: usage rows are dated charges, not readings.",
	"Downloads gained": "the newest count less the oldest is what a counter gained " +
		"over the range.",
	"Cache entries by key": "the least days since use over the entries of one key in the " +
		"repository's newest snapshot alone, which is the most recently used of them.",
	"Oldest open alerts": "min(@timestamp) is the first document of an alert, the date " +
		"it was raised.",
	"Discussion answers": "a comment's rows are the shapes it was written in, and the " +
		"largest is the one that recorded the answer, as the SQL twin's ORDER BY answers " +
		"DESC takes it.",
	"Open the longest": "an item still open only grows older, so its largest open time is " +
		"its newest; one that closed inside the range stays listed, which the description " +
		"says, since state is a tag and this store cannot read the newest row before it " +
		"filters on it.",
	"Open issues the longest": "as Open the longest.",
	"Social accounts": "present is written as 1 and never as anything else, so the " +
		"largest is the newest.",
	"Sponsorship": "each day is reduced to its largest reading and the last day that has " +
		"one is taken, because the server-side expressions that make dollars of the " +
		"cents cannot reduce a top_metrics; within one day.",
	"Stars over time": "a point per repository per day, stacked: a date histogram cannot " +
		"take each repository's newest reading before stacking them, and the largest of " +
		"one day differs from its newest only by what that day itself lost.",
	"Forks over time":            "as Stars over time.",
	"Artifact storage over time": "a point per repository per hour, the largest of the hour.",
	"Open alerts over time": "a point per severity per day, which the description says is " +
		"the largest single series of the day in Elasticsearch.",
	"Rate budget used": "the most of each bucket spent inside each point, a peak by design.",
	"Every bucket": "Most used and Lowest remaining are the extremes of the range by " +
		"design, so a bucket spent an hour ago still says so after it has refilled.",
	"Every family": "the most repositories one sweep of the range asked a family about, " +
		"which its description says, beside its failures added up and its sweeps counted " +
		"over the same range.",
}

// sqlNewestPicks are the arguments of a MAX() or MIN() that read no value
// over the range: a string that is one per row it is grouped on, the row
// number a window gave the newest row, the newest time of a partition, and
// the full names repoNameSQL compares to tell whether two share a short name.
var sqlNewestPicks = []string{
	"url", "w.url", "title", "referrer_url", "environment_url", "unit", "rn", "time", "full_name",
}

var sqlExtreme = regexp.MustCompile(`\b(MAX|MIN)\(([^()]*(?:\([^()]*\))?[^()]*)\)`)

// TestNoPanelReadsACurrentValueAsAnExtreme holds every store to the list
// above: an SQL MAX() or MIN() of a value, an Elasticsearch max or min that no
// newestDoc bucket narrows to one document, a Graphite table reduced by its
// maximum or minimum, and a Prometheus max_over_time or min_over_time.
func TestNoPanelReadsACurrentValueAsAnExtreme(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			title, _ := p["title"].(string)
			for _, what := range extremesOf(store.Name, p) {
				used[title] = true
				if _, listed := rangeExtremes[title]; !listed {
					t.Errorf("%s: %q reads %s over the range, which is the largest or least the "+
						"range held and not the value as it stands: read each series' newest "+
						"row, or add the panel to rangeExtremes with why the extreme is what it "+
						"means", store.Name, title, what)
				}
			}
		}
	}
	var stale []string
	for title := range rangeExtremes {
		if !used[title] {
			stale = append(stale, title)
		}
	}
	sort.Strings(stale)
	for _, title := range stale {
		t.Errorf("%q is in rangeExtremes and no store reads an extreme for it any more", title)
	}
}

// extremesOf is what one rendered panel takes a maximum or a minimum of over
// the range, in its own store's terms.
func extremesOf(store string, p map[string]any) []string {
	if store == "graphite" {
		var out []string
		for _, r := range graphiteReducers(p) {
			if r == "max" || r == "min" {
				out = append(out, "the "+r.(string)+" reducer")
			}
		}
		return out
	}
	read := map[string]func(map[string]any) []string{
		"influxdb": sqlExtremes, "postgres": sqlExtremes,
		"elasticsearch": esExtremes, "prometheus": promExtremes,
	}[store]
	var out []string
	for _, raw := range targetList(p) {
		target, _ := raw.(map[string]any)
		out = append(out, read(target)...)
	}
	return out
}

// sqlExtremes is every MAX() and MIN() of a statement that reads a value.
func sqlExtremes(target map[string]any) []string {
	sql, _ := target["rawSql"].(string)
	var out []string
	for _, m := range sqlExtreme.FindAllStringSubmatch(sql, -1) {
		if !slices.Contains(sqlNewestPicks, strings.TrimSpace(m[2])) {
			out = append(out, m[0])
		}
	}
	return out
}

// esExtremes is every max and min metric of a target that no newestDoc bucket
// narrows to one document.
func esExtremes(target map[string]any) []string {
	if narrowedToNewest(target) {
		return nil
	}
	var out []string
	for _, m := range asList(target["metrics"]) {
		if kind, _ := agg(m)["type"].(string); kind == "max" || kind == "min" {
			field, _ := agg(m)["field"].(string)
			out = append(out, kind+"("+field+")")
		}
	}
	return out
}

// promExtremes is every range function of a query that keeps an extreme.
func promExtremes(target map[string]any) []string {
	expr, _ := target["expr"].(string)
	var out []string
	for _, fn := range []string{"max_over_time", "min_over_time"} {
		if strings.Contains(expr, fn) {
			out = append(out, fn)
		}
	}
	return out
}

// narrowedToNewest says whether a target's metrics sit below a newestDoc
// bucket, which leaves them one document of each series to read.
func narrowedToNewest(target map[string]any) bool {
	for _, raw := range asList(target["bucketAggs"]) {
		bucket := agg(raw)
		settings, _ := bucket["settings"].(map[string]any)
		if bucket["type"] == "terms" && bucket["field"] == panelESTime &&
			settings["size"] == "1" && settings["orderBy"] == "_key" && settings["order"] == "desc" {
			return true
		}
	}
	return false
}

// TestTheSnapshotTablesReadTheirNewestRowInSQL: the four SQL tables the audit
// found reading a snapshot as MAX() or MIN() over the range now number each
// series' rows newest first and keep the first, in both SQL stores.
func TestTheSnapshotTablesReadTheirNewestRowInSQL(t *testing.T) {
	t.Parallel()
	for title, partition := range map[string]string{
		"Account keys":              "key, kind",
		"Repository settings":       "full_name",
		"Cache against the ceiling": "full_name",
		"Environments":              "full_name, environment",
	} {
		newest := regexp.MustCompile(`ROW_NUMBER\(\) OVER \(PARTITION BY ` + regexp.QuoteMeta(partition) +
			` ORDER BY time DESC\) AS rn .*\) x WHERE rn = 1\b`)
		for _, store := range []string{"influxdb", "postgres"} {
			sql := strings.ReplaceAll(allSQL(panelOf(t, mustBuild(t, store), title, "table")), `"key"`, "key")
			if !newest.MatchString(sql) {
				t.Errorf("%s: %q does not read each series' newest row, partitioned by %s:\n%s",
					store, title, partition, sql)
			}
		}
	}
}

// TestTheSnapshotTablesReadTheirNewestDocumentInElasticsearch: every table the
// audit found taking the largest reading of the range in Elasticsearch now
// narrows each series to its newest document before its metrics read it,
// with the buckets that say whose series it is above that document and the
// ones that say what it holds below, and hides the timestamp column the
// narrowing adds. Account keys is among them for a second reason: its
// top_metrics over three fields no key carries all of failed the whole
// panel.
func TestTheSnapshotTablesReadTheirNewestDocumentInElasticsearch(t *testing.T) {
	t.Parallel()
	doc := mustBuild(t, "elasticsearch")
	for title, identity := range map[string][]string{
		"Account keys":                {"key.keyword", "kind.keyword"},
		"Branch protection rules":     {"full_name.keyword", "repo.keyword", "pattern.keyword"},
		"Commits by hour of day":      {"full_name.keyword"},
		"Commits by weekday":          {"full_name.keyword"},
		"Community profile":           {"full_name.keyword", "repo.keyword"},
		"Default code scanning setup": {"full_name.keyword", "repo.keyword"},
		"Dependabot ecosystems":       {"full_name.keyword", "repo.keyword"},
		"Deploy keys":                 {"full_name.keyword", "repo.keyword", "key.keyword"},
		"Pinned items":                {"full_name.keyword", "repo.keyword"},
		"Policy files":                {"full_name.keyword", "repo.keyword", "file.keyword"},
		"Profile flags":               {"flag.keyword"},
		"Repository settings":         {"full_name.keyword", "repo.keyword"},
		"Ruleset rules and bypasses":  {"full_name.keyword", "repo.keyword", "ruleset.keyword", "rule.keyword"},
		"Security settings":           {"full_name.keyword", "repo.keyword", "setting.keyword"},
		"Sponsorship tiers":           {"tier.keyword"},
		"Stale branches":              {"full_name.keyword", "repo.keyword", "branch.keyword"},
		"Star lists":                  {"list.keyword"},
		"Workflow token permissions":  {"full_name.keyword", "repo.keyword"},
	} {
		kind := "table"
		if strings.HasPrefix(title, "Commits by") {
			kind = "barchart"
		}
		p := panelOf(t, doc, title, kind)
		target, _ := targetList(p)[0].(map[string]any)
		fields := bucketFieldsOf(asList(target["bucketAggs"]))
		at := slices.Index(fields, any(panelESTime))
		if !narrowedToNewest(target) || at != len(identity) ||
			!slices.Equal(fields[:at], toAny(identity)) {
			t.Errorf("elasticsearch: %q buckets by %v, want %v and then each one's newest "+
				"document", title, fields, identity)
		}
		if kind == "table" && !strings.Contains(asJSON(t, p["transformations"]),
			`"excludeByName":{"@timestamp":true`) {
			t.Errorf("elasticsearch: %q shows the timestamp its newest document is found by",
				title)
		}
	}
}

func mustBuild(t *testing.T, store string) map[string]any {
	t.Helper()
	s, ok := ByName(store)
	if !ok {
		t.Fatalf("there is no %s dashboard", store)
	}
	return s.Build(nil)
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
