package dashboards

import (
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestArchivedRepositoriesSetAsideReachTheAccountTotals is issue #78 on the
// dashboards. A repository the default filter sets aside for being archived
// has a gh_repo_total row from every totals sweep and no gh_repo row at all,
// and the repository picker lists what gh_repo holds. So the stars and forks
// beside the repository count, and the table of every repository, read
// gh_repo_total, and with All selected they let an archived row through in
// every store.
//
// Each query is rendered the way the dashboard checker renders it, with All
// selected over a picker that lists one live repository, which is the picker
// of an account whose archived repositories are all set aside. The SQL stores
// have no allValue, so All is that one name and the archived rows pass only by
// the variable's text; the other three have a wildcard for All, which passes
// them already, provided the query reads the measurement that holds them.
func TestArchivedRepositoriesSetAsideReachTheAccountTotals(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		doc := store.Build(nil)
		allValue, _ := store.Variable["allValue"].(string)
		vars := grafana.Vars{Datasource: store.DS, Repos: []string{"hello-world"}, AllValue: allValue}
		for _, p := range []map[string]any{
			panelOf(t, doc, "Repositories", "stat"),
			panelOf(t, doc, "Every repository, ever", "table"),
		} {
			checkArchivedAdmitted(t, store.Name, vars, p)
		}
	}
}

// checkArchivedAdmitted renders every target of one panel that reads stars or
// commits, and holds each to reading the measurement the archived rows are in
// and to a filter that lets them through.
func checkArchivedAdmitted(t *testing.T, store string, vars grafana.Vars, p map[string]any) {
	t.Helper()
	reads, admits := archivedAdmitted(store)
	checked := 0
	for _, raw := range p["targets"].([]any) {
		target := vars.Apply(raw.(map[string]any))
		text := asJSON(t, target)
		if !strings.Contains(text, "stars") && !strings.Contains(text, "commits") {
			continue // the repository count, which is the account's own row
		}
		checked++
		if left := grafana.Unrendered(target); len(left) > 0 {
			t.Errorf("%s %q leaves %v unrendered", store, p["title"], left)
		}
		for _, want := range []string{reads, admits} {
			if !strings.Contains(text, want) {
				t.Errorf("%s %q, rendered under All, lacks %s, so an archived repository "+
					"set aside is not in it:\n%s", store, p["title"], want, text)
			}
		}
	}
	if checked == 0 {
		t.Errorf("%s %q has no target that reads stars or commits, so this checks nothing", store, p["title"])
	}
}

// archivedAdmitted is, for one store, the measurement a query has to read to
// see the archived repositories set aside, and the repository filter as it
// renders under All, which has to let them through.
func archivedAdmitted(store string) (reads, admits string) {
	switch store {
	case "influxdb", "postgres":
		return "FROM gh_repo_total", "OR (archived = 'true' AND 'All' = 'All')"
	case "prometheus":
		return "github_repo_total_", `repo=~\".*\"`
	case "graphite":
		return "github.repo_total.*.*.*.*.*.", "github.repo_total.*.*.*.*.*."
	default:
		return "_index:ghchronicle-gh_repo_total", "repo.keyword:*"
	}
}

// panelOf is the one panel of the dashboard with this title and type. The
// title alone is not enough here: "Repositories" is the Overview's stat group
// and a table in the Inventory row as well.
func panelOf(t *testing.T, doc map[string]any, title, kind string) map[string]any {
	t.Helper()
	var found []map[string]any
	var walk func(list []map[string]any)
	walk = func(list []map[string]any) {
		for _, p := range list {
			if inner, isRow := p["panels"].([]map[string]any); isRow {
				walk(inner)
			}
			if p["title"] == title && p["type"] == kind {
				found = append(found, p)
			}
		}
	}
	list, _ := doc["panels"].([]map[string]any)
	walk(list)
	if len(found) != 1 {
		t.Fatalf("%d %s panels called %q, want one", len(found), kind, title)
	}
	return found[0]
}
