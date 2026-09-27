package dashboards

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestThePunchCardsAddUpEachRepositorysNewestGrid is what the second review
// of 2.6.1 found in both punch cards. gh_commit_punchcard is a grid, a point
// per weekday and hour, and one read stamps every cell of it with the same
// instant. The SQL stores kept the newest row per repository and hour, or per
// repository and weekday, which is one of the cells that tie on that instant,
// and Elasticsearch took the largest of them: on the end-to-end fixture,
// whose Monday holds 3 commits at 09:00 and 5 at 10:00, Monday read 3 in
// InfluxDB and PostgreSQL and 5 in Elasticsearch, where Graphite, which adds
// every cell up, read 8.
//
// Each store is held to every cell of each repository's newest grid, added
// up in its own terms: the SQL stores keep the rows at the repository's
// newest time and sum them by the charted tag; Elasticsearch buckets each
// repository by its newest timestamp, sums the cells of each hour or weekday
// under it and adds the repositories together; Graphite sums every cell's
// series by the node, and says what it cannot drop.
func TestThePunchCardsAddUpEachRepositorysNewestGrid(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ title, node, name string }{
		{"Commits by hour of day", "hour", "Hour"},
		{"Commits by weekday", "weekday", "Weekday"},
	} {
		for _, store := range []string{"influxdb", "postgres", "elasticsearch", "graphite"} {
			p := panelOf(t, mustBuild(t, store), c.title, "barchart")
			switch store {
			case "influxdb", "postgres":
				checkPunchCardSQL(t, store, c.title, c.node, allSQL(p))
			case "elasticsearch":
				checkPunchCardES(t, c.title, c.node, c.name, p)
			case "graphite":
				checkPunchCardGraphite(t, c.title, c.node, p)
			}
		}
	}
}

func checkPunchCardSQL(t *testing.T, store, title, node, sql string) {
	t.Helper()
	for _, want := range []string{
		"SELECT " + node + ` AS "`,
		`SUM(commits) AS "Commits"`,
		"MAX(time) OVER (PARTITION BY full_name) AS newest",
		") x WHERE time = newest GROUP BY 1 ",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s: %q does not add up every cell of each repository's newest grid, "+
				"it lacks %q:\n%s", store, title, want, sql)
		}
	}
	if strings.Contains(sql, "ROW_NUMBER") {
		t.Errorf("%s: %q keeps one row of the cells that share the newest time:\n%s",
			store, title, sql)
	}
}

func checkPunchCardES(t *testing.T, title, node, name string, p map[string]any) {
	t.Helper()
	target, _ := targetList(p)[0].(map[string]any)
	fields := bucketFieldsOf(asList(target["bucketAggs"]))
	if !slices.Equal(fields, []any{"full_name.keyword", panelESTime, node + ".keyword"}) ||
		!narrowedToNewest(target) {
		t.Errorf("elasticsearch: %q buckets by %v, want each repository, its newest "+
			"timestamp and then the %s", title, fields, node)
	}
	metrics := asList(target["metrics"])
	if len(metrics) != 1 || agg(metrics[0])["type"] != "sum" || agg(metrics[0])["field"] != "commits" {
		t.Errorf("elasticsearch: %q reads %v inside the newest timestamp, want the sum of "+
			"its cells' commits", title, metrics)
	}
	tf := asList(p["transformations"])
	grouped, sorted := -1, -1
	for i, raw := range tf {
		step, _ := raw.(map[string]any)
		options, _ := step["options"].(map[string]any)
		switch step["id"] {
		case "groupBy":
			groups, _ := options["fields"].(map[string]any)
			by, _ := groups[name].(map[string]any)
			value, _ := groups["Commits"].(map[string]any)
			if by["operation"] == "groupby" && value["operation"] == "aggregate" &&
				fmt.Sprint(value["aggregations"]) == "[sum]" {
				grouped = i
			}
		case "sortBy":
			if fmt.Sprint(options["sort"]) == fmt.Sprintf("[map[desc:false field:%s]]", name) {
				sorted = i
			}
		}
	}
	if grouped < 0 || sorted < grouped {
		t.Errorf("elasticsearch: %q does not add the repositories up by %s and then order "+
			"them by it: %v", title, name, tf)
	}
}

func checkPunchCardGraphite(t *testing.T, title, node string, p map[string]any) {
	t.Helper()
	target, _ := targetList(p)[0].(map[string]any)
	expr, _ := target["target"].(string)
	want := fmt.Sprintf(`groupByNode(keepLastValue(%s), %d, "sum")`,
		rp("gh_commit_punchcard", "commits"), gn("gh_commit_punchcard", node))
	if !strings.Contains(expr, want) {
		t.Errorf("graphite: %q does not add every cell up by %s, want %s in:\n%s",
			title, node, want, expr)
	}
	if desc, _ := p["description"].(string); !strings.Contains(desc, "a rewritten history emptied") {
		t.Errorf("graphite: %q does not say it keeps a cell the newest grid dropped: %q",
			title, desc)
	}
}
