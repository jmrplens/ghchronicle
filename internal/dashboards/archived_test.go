package dashboards

import (
	"regexp"
	"slices"
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
// renders under All, which has to let them through. The SQL filter goes on to
// ask whether the collector still writes the repository, which the test below
// holds it to.
func archivedAdmitted(store string) (reads string, admits *regexp.Regexp) {
	literal := func(s string) *regexp.Regexp { return regexp.MustCompile(regexp.QuoteMeta(s)) }
	switch store {
	case "influxdb", "postgres":
		return "FROM gh_repo_total", literal("OR (archived = 'true' AND 'All' = 'All' AND ")
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

// TestAnArchivedRepositoryNoLongerCollectedIsLeftOut is the other half of the
// rule above, found by checking 2.6.0 in production against GitHub. Under All
// an archived row was let through however old it was, so the one row of an
// archived repository the collector no longer writes counted for as long as
// the range held it: jmrplens/portainer-mcp-enhanced, an archived fork that
// `include_forks` off leaves out, had one gh_repo_total row, from a backfill
// under an earlier configuration nine days before, and it added 8 stars and 3
// forks to the Overview and a row to "Every repository, ever". An archived
// row now counts only where the collector still writes the repository, which
// is the question the picker asks of a live one.
//
// Graphite is evaluated over overviewAccount and overviewGone. The SQL stores
// are held to asking the picker's own question, in the picker's own window, of
// gh_repo_total, since only a store can evaluate a statement and the
// containerised suite has one run it; Elasticsearch to the window it keeps in
// its place. Prometheus is not asked: an instant query sees what the running
// collector pushed in the last five minutes, and a repository it does not
// collect is not among it, so overviewGone is not a series it could hold.
func TestAnArchivedRepositoryNoLongerCollectedIsLeftOut(t *testing.T) {
	t.Parallel()
	account := graphiteSeriesOf(append(slices.Clone(overviewAccount), overviewGone))
	for _, store := range AllStores() {
		doc := store.Build(nil)
		allValue, _ := store.Variable["allValue"].(string)
		vars := grafana.Vars{Datasource: store.DS, Repos: []string{"hello-world"}, AllValue: allValue}
		collected := setAsideCollectedIn(t, store)
		checked := 0
		for _, p := range []map[string]any{
			panelOf(t, doc, "Repositories", "stat"),
			panelOf(t, doc, "Every repository, ever", "table"),
		} {
			for _, raw := range p["targets"].([]any) {
				target := vars.Apply(raw.(map[string]any))
				if !strings.Contains(asJSON(t, target), "repo_total") {
					continue // the repository count, which is the account's own row
				}
				checked++
				switch store.Name {
				case "prometheus":
				case "graphite":
					checkGoneGraphite(t, p["title"], target["target"].(string), account)
				default:
					checkGoneAsked(t, store.Name, p["title"], target, collected)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no target reads gh_repo_total, so this checks nothing", store.Name)
		}
	}
}

// checkGoneGraphite evaluates one Graphite target over an account holding
// overviewGone: the Overview's sums have to be the account's without it, and
// a table must not list it.
func checkGoneGraphite(t *testing.T, title any, expr string, account []grSeries) {
	t.Helper()
	rows := evalGraphiteSeries(t, expr, account)
	if name := regexp.MustCompile(`, "([^"]+)"\)$`).FindStringSubmatch(expr); name != nil {
		if len(rows) != 1 || rows[0].value != overviewWant[name[1]] {
			t.Errorf("graphite: the Overview's %s are %v over an account with %v, "+
				"counting a repository nothing has written for nine days", name[1], rows, overviewWant[name[1]])
		}
		return
	}
	for _, r := range rows {
		if r.name == overviewGone.name {
			t.Errorf("graphite: %q lists %s, which nothing has written for nine days: %v",
				title, overviewGone.name, rows)
		}
	}
}

// checkGoneAsked holds a SQL or Elasticsearch target to asking, of an
// archived row, what setAsideCollectedIn says that store asks. The statement
// or the Lucene query itself is read, since the JSON of the target escapes the
// comparison the window is made of.
func checkGoneAsked(t *testing.T, store string, title any, target map[string]any, collected string) {
	t.Helper()
	query, _ := target["rawSql"].(string)
	if store == "elasticsearch" {
		query, _ = target["query"].(string)
	}
	if !strings.Contains(query, collected) {
		t.Errorf("%s %q lets an archived row through without asking whether the "+
			"collector still writes its repository, want %s:\n%s", store, title, collected, query)
	}
}

// setAsideCollectedIn is what a query of one store has to say to count an
// archived row only where the collector still writes its repository. For the
// SQL stores that is the picker's own window, read out of the variable's
// query, put to gh_repo_total; Elasticsearch has no such question, so it is
// the window of documents it keeps instead.
func setAsideCollectedIn(t *testing.T, store Store) string {
	t.Helper()
	switch store.Name {
	case "influxdb", "postgres":
		definition, _ := store.Variable["definition"].(string)
		window := regexp.MustCompile(`WHERE (time > now\(\) - INTERVAL '[^']+')`).FindStringSubmatch(definition)
		if window == nil {
			t.Fatalf("%s: the picker's query %q names no window", store.Name, definition)
		}
		return "full_name IN (SELECT full_name FROM gh_repo_total WHERE " + window[1] + " AND archived = 'true')"
	case "elasticsearch":
		return "@timestamp:[now-7d TO *]"
	}
	return ""
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
