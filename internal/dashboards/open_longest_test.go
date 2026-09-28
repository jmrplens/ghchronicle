package dashboards

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestTheOpenLongestTablesKeepTheLongestOpenInEveryStore is what the audit of
// 2.6.1 left for #97 in "Open the longest" and "Open issues the longest". In
// Elasticsearch each kept twenty-five pull requests or issues per repository
// by document count, not by how long they had been open, so a repository with
// more open items than that showed whichever had the most rows in the range,
// and the table held up to fifty repositories' twenty-five where the SQL
// stores list the twenty-five open longest of all of them. The Prometheus
// table capped its ranking query and not the comments beside it, so the merge
// listed every repository with an open pull request, those past the
// twenty-fifth with an empty Open for.
//
// Every store is held to the same list, the twenty-five open longest, the
// longest first, each in its own terms: the SQL stores order by the open time
// and keep twenty-five; Graphite sorts the series by their largest open time
// and keeps twenty-five; Prometheus ranks by the open time and keeps every
// other column to the rows that ranking kept; and Elasticsearch keeps the
// values of the repository and the number by the open time, whose largest
// twenty-five are then among the rows, and the table sorts them by it and
// keeps twenty-five.
func TestTheOpenLongestTablesKeepTheLongestOpenInEveryStore(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		panels := rendered(t, store.Name)
		for _, title := range []string{"Open the longest", "Open issues the longest"} {
			p := mustPanel(t, panels, title)
			if p["type"] != "table" {
				continue // a store that cannot answer says why in a note
			}
			targets := targetList(p)
			switch store.Name {
			case "influxdb", "postgres":
				sql := allSQL(p)
				if !strings.HasSuffix(sql, fmt.Sprintf(" ORDER BY x.seconds_open DESC LIMIT %d", openLongest)) {
					t.Errorf("%s: %s does not keep the %d open longest, the longest first:\n%s",
						store.Name, title, openLongest, sql)
				}
			case "graphite":
				expr, _ := targets[0].(map[string]any)["target"].(string)
				longest := regexp.MustCompile(fmt.Sprintf(
					`^(removeEmptySeries\()?limit\(sortBy\(aliasByNode\(groupByNodes\([^()]*\.seconds_open, "max", `+
						`\d+, \d+, \d+\), 1, 2\), "max", true\), %d\)\)?$`,
					openLongest,
				))
				if !longest.MatchString(expr) {
					t.Errorf("graphite: %s does not keep the %d series of the largest open time, "+
						"the largest first: %s", title, openLongest, expr)
				}
			case "prometheus":
				checkCappedOnTheOpenTime(t, title, targets)
			case "elasticsearch":
				checkOpenLongestBuckets(t, title, targets[0].(map[string]any))
				checkKeepsTheLargest(t, title, p)
			}
		}
	}
}

// checkCappedOnTheOpenTime holds a Prometheus table of what is still open to
// one ranking by the open time, capped at openLongest, and every other query
// of it to the rows that ranking kept.
func checkCappedOnTheOpenTime(t *testing.T, title string, targets []any) {
	t.Helper()
	first, _ := targets[0].(map[string]any)
	rank, _ := first["expr"].(string)
	top := fmt.Sprintf("topk(%d, ", openLongest)
	if !strings.HasPrefix(rank, top) || !strings.Contains(rank, "seconds_open") {
		t.Errorf("prometheus: %s ranks by %s, want the %d of the largest open time", title, rank, openLongest)
		return
	}
	for _, raw := range targets[1:] {
		target, _ := raw.(map[string]any)
		expr, _ := target["expr"].(string)
		if !strings.HasSuffix(expr, " "+rank) || !strings.Contains(expr, " and on (") {
			t.Errorf("prometheus: %s query %v is not kept to the rows the open time ranks, "+
				"so the merge lists a row for every repository it answers: %s", title, target["refId"], expr)
		}
	}
}

// checkOpenLongestBuckets holds the Elasticsearch query of a table of what is
// still open to keeping the repositories and, in each, the numbers of the
// largest open time, which is the one metric of the query that reads it. A
// repository is its full name, and its short name the one value inside it.
func checkOpenLongestBuckets(t *testing.T, title string, target map[string]any) {
	t.Helper()
	age := ""
	for _, raw := range asList(target["metrics"]) {
		m := agg(raw)
		if m["type"] == "max" && m["field"] == "seconds_open" {
			age, _ = m["id"].(string)
		}
	}
	if age == "" {
		t.Fatalf("elasticsearch: %s reads no largest open time: %v", title, target["metrics"])
	}
	kept := map[string]bool{}
	for _, raw := range asList(target["bucketAggs"]) {
		bucket := agg(raw)
		field, _ := bucket["field"].(string)
		if field != panelFullNameField && field != flowNumberTerm {
			continue
		}
		kept[field] = true
		settings, _ := bucket["settings"].(map[string]any)
		if settings["orderBy"] != age || settings["order"] != "desc" {
			t.Errorf("elasticsearch: %s keeps its %s values by %v %v, want the largest open "+
				"time first, metric %s", title, field, settings["orderBy"], settings["order"], age)
		}
	}
	if !kept[panelFullNameField] || !kept[flowNumberTerm] {
		t.Errorf("elasticsearch: %s buckets by %v, want the repository and the number",
			title, bucketFieldsOf(asList(target["bucketAggs"])))
	}
}

// checkKeepsTheLargest holds an Elasticsearch table to sorting its rows by
// the open time, the largest first, and keeping openLongest of them, after
// the organize that names the column: nested terms buckets keep the top
// values inside each repository, never across the table.
func checkKeepsTheLargest(t *testing.T, title string, p map[string]any) {
	t.Helper()
	var ids []any
	sorted, limited := false, false
	for _, raw := range asList(p["transformations"]) {
		tf, _ := raw.(map[string]any)
		ids = append(ids, tf["id"])
		options, _ := tf["options"].(map[string]any)
		switch tf["id"] {
		case "sortBy":
			for _, s := range asList(options["sort"]) {
				by, _ := s.(map[string]any)
				sorted = sorted || (by["field"] == flowOpenAge && by["desc"] == true)
			}
		case "limit":
			limited = sorted && options["limitField"] == openLongest
		}
	}
	if !limited {
		t.Errorf("elasticsearch: %s does not sort its rows by %s, the largest first, and "+
			"then keep %d: its transformations are %v", title, flowOpenAge, openLongest, ids)
	}
}
