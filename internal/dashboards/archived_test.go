package dashboards

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// TestArchivedRepositoriesSetAsideReachTheAccountTotals is issue #78 on the
// dashboards. A repository the default filter sets aside for being archived
// has a gh_repo_total row from every totals sweep and no gh_repo row from a
// sweep, and the repository picker lists what gh_repo holds. So the stars and
// forks beside the repository count read an archived repository's
// gh_repo_total, the table of every repository reads gh_repo_total whole, and
// with All selected both let an archived row through in every store.
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
		if !strings.Contains(text, reads) {
			t.Errorf("%s %q, rendered under All, does not read %s, so an archived repository "+
				"set aside is not in it:\n%s", store, p["title"], reads, text)
		}
		if !admits.MatchString(text) {
			t.Errorf("%s %q, rendered under All, lacks %s, so an archived repository "+
				"set aside is not in it:\n%s", store, p["title"], admits, text)
		}
	}
	if checked == 0 {
		t.Errorf("%s %q has no target that reads stars or commits, so this checks nothing", store, p["title"])
	}
}

// archivedAdmitted is, for one store, the measurement a query has to read to
// see the archived repositories set aside, and the repository filter as it
// renders under All, which has to let them through.
func archivedAdmitted(store string) (reads string, admits *regexp.Regexp) {
	literal := func(s string) *regexp.Regexp { return regexp.MustCompile(regexp.QuoteMeta(s)) }
	switch store {
	case "influxdb", "postgres":
		return "FROM gh_repo_total", literal("OR (archived = 'true' AND 'All' = 'All')")
	case "prometheus":
		return "github_repo_total_", literal(`repo=~\".*\"`)
	case "graphite":
		// The archived flag is a wildcard, or pinned to true where the live
		// repositories come from gh_repo, and every node after it, the
		// repository's among them, is a wildcard.
		return "github.repo_total.", regexp.MustCompile(`github\.repo_total\.(\*|true)(\.\*){5}\.`)
	default:
		return "_index:ghchronicle-gh_repo_total", literal("repo.keyword:*")
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
