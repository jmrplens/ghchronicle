package dashboards

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// aliased matches the column a SQL statement selects under a name.
func aliased(column string) *regexp.Regexp {
	return regexp.MustCompile(`AS\s+"` + regexp.QuoteMeta(column) + `"`)
}

// flat is every panel of a rendered dashboard that is not a row, rows
// descended into, in the order grafana.Panels numbers them.
func flat(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	var walk func(list []map[string]any)
	walk = func(list []map[string]any) {
		for _, p := range list {
			if inner, ok := p["panels"].([]map[string]any); ok {
				walk(inner)
				continue
			}
			out = append(out, p)
		}
	}
	list, ok := doc["panels"].([]map[string]any)
	if !ok {
		t.Fatalf("a dashboard's panels are %T, not a list", doc["panels"])
	}
	walk(list)
	return out
}

// producesColumn reports whether a rendered panel's own query returns the
// column: a SQL alias, or a field its organize transformation renames to it.
func producesColumn(p map[string]any, column string) bool {
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		t, _ := raw.(map[string]any)
		if sql, _ := t["rawSql"].(string); aliased(column).MatchString(sql) {
			return true
		}
	}
	for _, to := range grafana.Renames(p) {
		if to == column {
			return true
		}
	}
	return false
}

// TestLinkOverridesStayWhereTheColumnIs is the arrangement placeLinks makes:
// a link override reaches a store's panel exactly when that store's query
// returns the column it reads, and a store that lost one says so, in the
// description of a query panel or the note of a text panel. Before this the
// shared override list reached all five dashboards, and on three of them it
// was matched byName against a column that was never there: nothing drawn,
// nothing said.
func TestLinkOverridesStayWhereTheColumnIs(t *testing.T) {
	t.Parallel()
	rendered := map[string][]map[string]any{}
	for _, store := range AllStores() {
		rendered[store.Name] = flat(t, store.Build(nil))
	}
	influx := rendered["influxdb"]
	linked := 0
	for i := range influx {
		here := grafana.LinkColumns(influx[i])
		if len(here) > 0 {
			linked++
		}
		for name, panels := range rendered {
			checkLinkPlacement(t, name, panels[i], len(here))
		}
	}
	if linked == 0 {
		t.Fatal("not one InfluxDB panel links from a column, so this checked nothing")
	}
}

// checkLinkPlacement holds one rendered panel of one store to the
// arrangement: every link it carries reads a column its query returns, a Link
// column it returns carries a link, and a link column it has fewer of than
// the InfluxDB twin is said to be missing.
func checkLinkPlacement(t *testing.T, store string, p map[string]any, influxLinks int) {
	t.Helper()
	title, _ := p["title"].(string)
	links := grafana.LinkColumns(p)
	for _, column := range links {
		if !producesColumn(p, column) {
			t.Errorf("%s: %q links from a %s column its query does not return", store, title, column)
		}
	}
	if producesColumn(p, "Link") && len(links) == 0 {
		t.Errorf("%s: %q selects a Link column and carries no override for it", store, title)
	}
	if store == "influxdb" || influxLinks == 0 || len(links) == influxLinks {
		return
	}
	desc, _ := p["description"].(string)
	options, _ := p["options"].(map[string]any)
	content, _ := options["content"].(string)
	if !strings.Contains(desc, noLink[store]) && !strings.Contains(content, noteLink) {
		t.Errorf("%s: %q lost a link column the InfluxDB dashboard has and does not say so", store, title)
	}
}

// TestLinkColumnsAreTheOneShape pins what every link column override says,
// because each part is load-bearing: the pattern decides whether the cell
// reads as one word, the href decides where it goes, and a link not opened in
// a new tab replaces the dashboard.
func TestLinkColumnsAreTheOneShape(t *testing.T) {
	t.Parallel()
	for _, o := range []any{linkColumn(), ownerLink("the settings"), linkColumnAs("Live", "Open"), downloadColumn()} {
		m, _ := o.(map[string]any)
		props, _ := m["properties"].([]any)
		var pattern, href string
		var newTab bool
		for _, raw := range props {
			p, _ := raw.(map[string]any)
			switch p["id"] {
			case "mappings":
				list, _ := p["value"].([]any)
				mapping, _ := list[0].(map[string]any)
				options, _ := mapping["options"].(map[string]any)
				pattern, _ = options["pattern"].(string)
			case "links":
				list, _ := p["value"].([]any)
				link, _ := list[0].(map[string]any)
				href, _ = link["url"].(string)
				newTab, _ = link["targetBlank"].(bool)
			}
		}
		if pattern != "^https?://.+" || href != linkHref || !newTab {
			t.Errorf("%v: pattern %q, href %q, new tab %v", linkedColumn(o), pattern, href, newTab)
		}
	}
	if got := linkedColumn(width("Repository", 100)); got != "" {
		t.Errorf("a width override reads as a link from %q", got)
	}
	if got := linkedColumn(barLink("Downloads", "Page", "Open")[0]); got != "Page" {
		t.Errorf("a bar link reads its url from %q, want the Page column", got)
	}
}

// TestOpenAlertsAreAStateNotARange: an alert item is dated when it was raised
// and rewritten in place while it stays open, so the open ones are a current
// state, and the table that promises the oldest of them cannot be bounded by
// the dashboard range on that date. Measured on the account with 67 open code
// scanning alerts, all raised before the last seven days: the seven day view
// listed none of them under the two counts that said 6 and 67.
func TestOpenAlertsAreAStateNotARange(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		sql := sqlOf(t, mustPanel(t, rendered(t, store), "Oldest open alerts"))
		if strings.Contains(sql, "$__timeFilter") {
			t.Errorf("%s: Oldest open alerts is bounded by the range on the date the alert was raised: %s", store, sql)
		}
		if !strings.Contains(sql, "INTERVAL '30 years'") {
			t.Errorf("%s: Oldest open alerts reads no whole-history window: %s", store, sql)
		}
	}
}

// TestRunTablesJoinOneDeclarationPerWorkflow: the run tables join gh_workflow
// for the file's own page, and a join that can match a run twice doubles its
// run count. The declaration's url carries the default branch, so a renamed
// branch is a second distinct row under the same path; the join has to
// collapse to one row per file before it meets the runs.
func TestRunTablesJoinOneDeclarationPerWorkflow(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for _, title := range []string{"Workflows", "Workflows that keep failing"} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if !strings.Contains(sql, "LEFT JOIN (SELECT repo, path, MAX(url) AS url FROM gh_workflow") ||
			!strings.Contains(sql, "GROUP BY 1, 2) w ON") {
			t.Errorf("%s joins gh_workflow without one row per (repo, path): %s", title, sql)
		}
	}
}

// TestRedBranchCommitsAreNewestFirst: the table is capped at 25 rows, and
// after the columns were reordered its positional ORDER BY pointed at the
// repository name, so the cap kept the commits of the alphabetically last
// repositories rather than the newest ones. The position has to resolve to
// the When column.
func TestRedBranchCommitsAreNewestFirst(t *testing.T) {
	t.Parallel()
	sql := sqlOf(t, mustPanel(t, rendered(t, "influxdb"), "Commits behind a red branch"))
	m := regexp.MustCompile(`ORDER BY (\d+) DESC LIMIT`).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no positional ORDER BY ... DESC LIMIT in %s", sql)
	}
	columns := regexp.MustCompile(`AS "([^"]+)"`).FindAllStringSubmatch(sql[:strings.Index(sql, " FROM ")], -1)
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > len(columns) || columns[n-1][1] != "When" {
		t.Errorf("ORDER BY %s resolves to column %v of %d, want When", m[1], columns[min(max(n, 1), len(columns))-1][1], len(columns))
	}
}

// ownerOnlyURL is every measurement whose `url` is a page GitHub shows the
// owner alone. Measured on 2026-09-12 with an anonymous GET: the settings
// pages, the branch protection page, the traffic graph, the security tabs
// with their alerts, the rulesets under settings, the deployments list and
// the environment activity log, the stargazers list and the billing summary
// answer 404 or redirect to the login. The public twin of a ruleset is
// `/rules/{id}`, which gh_ruleset_version already carries and gh_ruleset
// should; until it does its link says owner only.
var ownerOnlyURL = map[string]bool{
	"gh_repo_policy": true, "gh_key": true, "gh_billing_usage": true,
	"gh_branch_protection": true, "gh_traffic": true,
	"gh_dependabot_alert": true, "gh_dependabot_alert_item": true,
	"gh_code_scanning_alert": true, "gh_code_scanning_alert_item": true,
	"gh_ruleset": true, "gh_deployment": true, "gh_environment": true,
	"gh_star": true, "gh_security_feature": true,
}

// TestOwnerOnlyPagesSaySo holds every link of the InfluxDB dashboard to the
// rule the review asked for: a link to a page only the owner can open says
// so in its title, and a link to a public page does not. The row link reads
// the Link column, so the measurement whose `url` field that is decides; a
// Link taken from another field, a stargazer's `user_url` or a referrer's
// `referrer_url`, is a public page whatever the measurement's own url is. A
// panel link names its page outright.
func TestOwnerOnlyPagesSaySo(t *testing.T) {
	t.Parallel()
	measurement := regexp.MustCompile(`\bgh_[a-z_]+\b`)
	// The url field itself selected as the Link column, bare, qualified or
	// under an aggregate: `url AS "Link"`, `c.url AS "Link"`, `MAX(url) AS "Link"`.
	urlAsLink := regexp.MustCompile(`(?:^|[^_a-z])url\)? AS "Link"`)
	// A qualified one comes from one side of a join, and that side's
	// measurement decides on its own: the community profile links the public
	// community page of gh_repo_community and joins gh_repo_policy, whose
	// url is the settings page, for one count. The side is the subquery
	// closed by the qualifier, `FROM gh_x ... ) c`, with the parentheses of
	// its macros and windows in between.
	qualified := regexp.MustCompile(`\b([a-z])\.url\)? AS "Link"`)
	checked := 0
	for _, p := range flat(t, ByNameOrFatal(t, "influxdb").Build(nil)) {
		title, _ := p["title"].(string)
		var statements []string
		for _, raw := range asAnyList(p["targets"]) {
			target, _ := raw.(map[string]any)
			statement, _ := target["rawSql"].(string)
			statements = append(statements, statement)
		}
		sql := strings.Join(statements, "\n")
		owner := false
		if q := qualified.FindStringSubmatch(sql); q != nil {
			side := regexp.MustCompile(`FROM (gh_[a-z_]+)\b[^()]*(?:\([^()]*\)[^()]*)*\) ` + q[1] + `\b`)
			from := side.FindStringSubmatch(sql)
			if from == nil {
				t.Errorf("%q links %s.url and no subquery of its statement is closed by %s:\n%s", title, q[1], q[1], sql)
				continue
			}
			owner = ownerOnlyURL[from[1]]
		} else {
			for _, m := range measurement.FindAllString(sql, -1) {
				owner = owner || ownerOnlyURL[m]
			}
			owner = owner && urlAsLink.MatchString(sql)
		}
		for _, l := range rowLinkTitles(p) {
			checked++
			if strings.HasSuffix(l, ownerNote) != owner {
				t.Errorf("%q links %q and its url comes from an owner-only page: %v", title, l, owner)
			}
		}
		for _, raw := range asAnyList(p["links"]) {
			link, _ := raw.(map[string]any)
			url, _ := link["url"].(string)
			name, _ := link["title"].(string)
			checked++
			if strings.HasSuffix(name, ownerNote) != strings.Contains(url, "/settings/") {
				t.Errorf("%q carries the panel link %q to %s", title, name, url)
			}
		}
	}
	if checked < 30 {
		t.Fatalf("checked %d links, so the rule covers almost nothing", checked)
	}
}

// rowLinkTitles is the title of every data link that reads the row's Link
// column: the one the reader hovers before opening the item.
func rowLinkTitles(p map[string]any) []string {
	var out []string
	config, _ := p["fieldConfig"].(map[string]any)
	for _, raw := range asAnyList(config["overrides"]) {
		o, _ := raw.(map[string]any)
		if linkedColumn(o) != "Link" {
			continue
		}
		for _, prop := range asAnyList(o["properties"]) {
			m, _ := prop.(map[string]any)
			if m["id"] != "links" {
				continue
			}
			for _, l := range asAnyList(m["value"]) {
				link, _ := l.(map[string]any)
				name, _ := link["title"].(string)
				out = append(out, name)
			}
		}
	}
	return out
}

func asAnyList(v any) []any {
	l, _ := v.([]any)
	return l
}

// ByNameOrFatal is ByName for a test that cannot go on without the store.
func ByNameOrFatal(t *testing.T, name string) *Store {
	t.Helper()
	s, ok := ByName(name)
	if !ok {
		t.Fatalf("there is no %s dashboard", name)
	}
	return s
}
