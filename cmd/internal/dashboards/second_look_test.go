package dashboards

import (
	"strings"
	"testing"
)

// The owner's second look at the published dashboard, on 2026-09-12 in the
// evening, found three things: a community profile table saying "no" under
// Issue template for every repository of an account whose repositories carry
// four issue forms each, a text panel telling the reader to query Loki by
// hand where the failed job output could be, and a donut on a phone with a
// legend that ended after eight entries. Each test here pins one of the
// answers to the rendered JSON.

// TestIssueTemplatesAreCountedNotFlagged: GitHub's community profile, like
// its GraphQL issueTemplates, counts Markdown issue templates and not YAML
// issue forms, so its flag said no on every repository of the measured
// account. The count of templates a repository actually has is what the
// totals family writes to gh_repo_policy.issue_templates. The two SQL stores
// join the newest policy row to the newest profile row per repository and
// show the count; Prometheus lines the two families up on the repo label;
// Graphite and Elasticsearch cannot join and keep the flag, under the flag's
// own name, saying whose flag it is.
func TestIssueTemplatesAreCountedNotFlagged(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		sql := sqlOf(t, mustPanel(t, rendered(t, store), "Community profile"))
		for _, want := range []string{
			`p.issue_templates AS "Issue templates"`,
			"FROM gh_repo_policy WHERE $__timeFilter(time) AND repo IN (",
			") p ON p.repo = c.repo AND p.rn = 1 WHERE c.rn = 1",
			"LEFT JOIN",
			`c.url AS "Link"`,
			`CAST(c.has_pull_request_template AS INT) AS "PR template"`,
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s: the profile does not join the newest policy row per repository, missing %q:\n%s", store, want, sql)
			}
		}
		if strings.Contains(sql, "has_issue_template") {
			t.Errorf("%s still reads GitHub's flag, which ignores issue forms:\n%s", store, sql)
		}
	}
	prom := mustPanel(t, rendered(t, "prometheus"), "Community profile")
	exprs := asJSON(t, prom["targets"])
	if !strings.Contains(exprs, "max by (repo) (github_repo_policy_issue_templates{") {
		t.Errorf("Prometheus does not read the policy count: %s", exprs)
	}
	if strings.Contains(exprs, "github_repo_community_has_issue_template") {
		t.Errorf("Prometheus still reads GitHub's flag: %s", exprs)
	}
	if tf := asJSON(t, prom["transformations"]); !strings.Contains(tf, `"Issue templates"`) {
		t.Errorf("the Prometheus count is not named Issue templates: %s", tf)
	}
	for _, store := range []string{"graphite", "elasticsearch"} {
		p := mustPanel(t, rendered(t, store), "Community profile")
		desc, _ := p["description"].(string)
		if !strings.Contains(desc, "legacy single file") {
			t.Errorf("%s does not say that the API flag reports only the legacy file: %q", store, desc)
		}
	}
	es := mustPanel(t, rendered(t, "elasticsearch"), "Community profile")
	if targets := asJSON(t, es["targets"]); !strings.Contains(targets, `"has_issue_template"`) {
		t.Errorf("Elasticsearch cannot join, so it keeps GitHub's flag, and it does not: %s", targets)
	}
	if tf := asJSON(t, es["transformations"]); !strings.Contains(tf, `"Issue template"`) || strings.Contains(tf, `"Issue templates"`) {
		t.Errorf("the Elasticsearch flag is not named as the flag: %s", tf)
	}
	for _, store := range AllStores() {
		p := mustPanel(t, rendered(t, store.Name), "Community profile")
		desc, _ := p["description"].(string)
		// Measured 2026-09-12: the page and health_percentage count a templates
		// directory, and only the API flag misses it (community/community#207706).
		if !strings.Contains(desc, "API's issue_template flag reports only the legacy single file") {
			t.Errorf("%s does not explain why the flag reads false on a repository that scores 100: %q", store.Name, desc)
		}
	}
}

// TestJobLogsAreDrawnWhenALogStoreIsGiven: the exported files keep the text
// panel, since an importer may have no Loki; a dashboard built with a Loki
// datasource draws the failed job output in its place, under the same title
// at the same grid position, from the stream the Loki sink writes, with the
// repository variable applied where the store's variable can be read as a
// regular expression. Nothing else in the document moves.
func TestJobLogsAreDrawnWhenALogStoreIsGiven(t *testing.T) {
	t.Parallel()
	loki := map[string]any{"type": "loki", "uid": "loki-uid"}
	for _, store := range AllStores() {
		plain := store.Build(nil)
		withLogs := store.BuildWith(nil, loki)
		if a, b := layout(plain), layout(withLogs); strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Errorf("%s: the log store changed the layout", store.Name)
		}
		note := panelTitled(t, plain, logTitle)
		if note["type"] != "text" {
			t.Errorf("%s: the exported file draws %q as a %v, and an importer may have no Loki", store.Name, logTitle, note["type"])
		}
		logs := panelTitled(t, withLogs, logTitle)
		if logs["type"] != "logs" {
			t.Fatalf("%s: with a Loki the panel is a %v", store.Name, logs["type"])
		}
		if ds := asJSON(t, logs["datasource"]); ds != `{"type":"loki","uid":"loki-uid"}` {
			t.Errorf("%s: the logs panel reads %s", store.Name, ds)
		}
		checkLogQuery(t, &store, logs)
		options, _ := logs["options"].(map[string]any)
		if options["sortOrder"] != "Descending" || options["wrapLogMessage"] != true {
			t.Errorf("%s: the lines are not newest first and wrapped: %v", store.Name, options)
		}
		// Every other panel is the same document.
		for _, p := range []map[string]any{note, logs} {
			for _, k := range []string{"type", "datasource", "targets", "options", "description", "fieldConfig", "transparent"} {
				delete(p, k)
			}
		}
		if !equal(t, plain, withLogs) {
			t.Errorf("%s: a panel other than %q changed with the log store", store.Name, logTitle)
		}
	}
	if n := ByNameOrFatal(t, "graphite"); n.Variable["allValue"] != "*" {
		t.Error("the Graphite variable no longer names the glob star, so the case above is not exercised")
	}
}

// logTitle is the panel the log store replaces.
const logTitle = "Where failure output went"

// checkLogQuery holds the logs panel's one target to the sink's stream, with
// the repository variable applied where the store's All value is not the
// glob star, and the description saying so where it is.
func checkLogQuery(t *testing.T, store *Store, logs map[string]any) {
	t.Helper()
	targets, _ := logs["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("%s: %d targets on the logs panel", store.Name, len(targets))
	}
	target, _ := targets[0].(map[string]any)
	expr, _ := target["expr"].(string)
	if !strings.HasPrefix(expr, `{job="ghchronicle", kind="job_log"}`) {
		t.Errorf("%s: the logs panel does not read the sink's job_log stream: %s", store.Name, expr)
	}
	desc, _ := logs["description"].(string)
	filtered := strings.Contains(expr, ` | logfmt | repo=~"$repo"`)
	glob := store.Variable["allValue"] == "*"
	switch {
	case glob && filtered:
		t.Errorf("%s: its All value is the glob star, which the regular expression cannot take: %s", store.Name, expr)
	case glob && !strings.Contains(desc, "glob star"):
		t.Errorf("%s: shows every repository and does not say why: %q", store.Name, desc)
	case !glob && !filtered:
		t.Errorf("%s: the repository variable is not applied: %s", store.Name, expr)
	}
}

// panelTitled is the one panel of a document with that title, rows searched.
func panelTitled(t *testing.T, doc map[string]any, title string) map[string]any {
	t.Helper()
	for _, p := range flat(t, doc) {
		if p["title"] == title {
			return p
		}
	}
	t.Fatalf("no panel titled %q", title)
	return nil
}

// TestPieLegendsCarryTheShareAndFitAPhone: a legend of names only with the
// percentages on the slices drew a small donut over a legend that ended
// after eight entries on a phone, and the thin slices never got their label.
// Every entry carries its share and the slices carry nothing. The one pie
// of the spec puts its legend under the donut as a list: under 992 pixels
// Grafana puts any legend there at 35 per cent of the panel whatever the
// placement says, and only a bottom list wraps, so that is the one shape
// whose ten entries are all on the phone; a legend placed right or a table
// is a column there and scrolled after seven. The pie's height is what the
// 35 per cent rests on.
func TestPieLegendsCarryTheShareAndFitAPhone(t *testing.T) {
	t.Parallel()
	pies := 0
	b := &builder{}
	for _, sec := range Sections {
		for _, spec := range sec.Build(b) {
			if spec.Kind != "piechart" {
				continue
			}
			pies++
			if spec.H < 16 {
				t.Errorf("%q is %d units tall, and a phone legend is 35 per cent of that", spec.Title, spec.H)
			}
			for _, store := range AllStores() {
				if p := panelTitled(t, store.Build(nil), spec.Title); p["type"] == "piechart" {
					checkPieLegend(t, store.Name, p)
				}
			}
		}
	}
	if pies == 0 {
		t.Fatal("no piechart in the specification, so this checks nothing")
	}
}

// checkPieLegend is one rendered pie's legend: the share of each slice in it
// and nothing on the slices, and for the events pie a list under the chart,
// the one shape that is complete on a phone.
func checkPieLegend(t *testing.T, store string, p map[string]any) {
	t.Helper()
	title, _ := p["title"].(string)
	options, _ := p["options"].(map[string]any)
	legend, _ := options["legend"].(map[string]any)
	if values := asJSON(t, legend["values"]); values != `["percent"]` {
		t.Errorf("%s: %q lists %s in its legend, and the share is what a slice is", store, title, values)
	}
	if labels := asJSON(t, options["displayLabels"]); labels != "[]" {
		t.Errorf("%s: %q still writes %s on the slices", store, title, labels)
	}
	if title == "Events by type" && (legend["placement"] != "bottom" || legend["displayMode"] != "list") {
		t.Errorf("%s: %q has a %v legend placed %v, and only a bottom list wraps on a phone", store, title, legend["displayMode"], legend["placement"])
	}
}
