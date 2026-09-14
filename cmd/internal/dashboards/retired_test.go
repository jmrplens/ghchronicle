package dashboards

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// retired is every column the collectors stopped writing under that name,
// by the measurement, or the exporter's metric prefix, a query would name it
// on. A tag that moved after the row's own date is a field under a new name
// now, and a tag that identified nothing is gone: InfluxDB 3 plans a query
// naming a column no row has written as an error, so a panel that still says
// the old name is red on every range on a database written by the current
// collector, and check_dashboards is the only thing that would say so,
// after a build and against a live store.
var retired = map[string][]string{
	"gh_artifact":                 {"expired"},
	"gh_commit":                   {"checks"},
	"github_commits":              {"checks"},
	"gh_dependabot_alert_item":    {"state"},
	"github_dependabot_alerts":    {"state"},
	"gh_code_scanning_alert_item": {"state", "reason"},
	"github_code_scanning_alerts": {"state"},
	"gh_issue":                    {"state_reason", "assignee", "milestone", "parent"},
	"github_issues":               {"state_reason"},
	"gh_pull_request":             {"draft", "review_decision"},
	"gh_pull_request_review":      {"state"},
	"github_reviews":              {"state"},
	"gh_discussion":               {"answered"},
	"github_discussions":          {"answered"},
	"gh_repo_activity":            {"ref"},
	"gh_event":                    {"actor", "public"},
	"gh_notification":             {"unread"},
	"gh_deployment":               {"state"},
	"github_deployments":          {"state"},
}

// TestNoPanelNamesARetiredColumn renders every store and reads every target
// as text: a target that names a measurement and, anywhere in it, one of the
// columns that measurement no longer has, is a panel that fails at planning.
//
// The match is on whole words, so `ref_name` is not `ref` and `gh_issue_event`
// is not `gh_issue`; a label in a Prometheus selector, a column in SQL and a
// field in an Elasticsearch bucket all read the same way.
func TestNoPanelNamesARetiredColumn(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, len(retired))
	for name := range retired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, store := range AllStores() {
		panels, ok := store.Build(nil)["panels"].([]map[string]any)
		if !ok {
			t.Fatalf("%s renders no panel list", store.Name)
		}
		for _, target := range targetsOf(panels) {
			raw, err := json.Marshal(target)
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			for _, name := range names {
				if !namesMeasurement(text, name) {
					continue
				}
				for _, column := range retired[name] {
					if regexp.MustCompile(`\b` + regexp.QuoteMeta(column) + `\b`).MatchString(text) {
						t.Errorf("%s: a target on %s still names %s, which the collector no longer writes:\n%s",
							store.Name, name, column, text)
					}
				}
			}
		}
	}
}

// namesMeasurement says whether a target's text names a measurement, or a
// metric of the exporter's family: `github_commits` is the prefix of
// `github_commits_total` and `github_commits_churn_mean`, so the family name
// is matched at its own start only, where a measurement is a whole word.
func namesMeasurement(text, name string) bool {
	if strings.HasPrefix(name, "github_") {
		return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `_`).MatchString(text)
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(text)
}
