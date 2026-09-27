package dashboards

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// twoOwnersReading is one reading of a measurement the newest-value sums add
// up: its field, the tags beside the repository's, and what each of the two
// owners' repositories holds.
type twoOwnersReading struct {
	measurement, field string
	tags               map[string]string
	alice, acme        float64
}

// twoOwnersReadings are the five measurements behind "Open alerts", the
// artifact and cache bytes of "Runs in range" and "Downloads", read for
// alice/dotfiles and acme/dotfiles, two owners' repositories of one name, as
// an account and an organization it belongs to each keep a .github.
var twoOwnersReadings = []twoOwnersReading{
	{"gh_dependabot_alert", "open", map[string]string{"severity": "high", "ecosystem": "npm"}, 2, 5},
	{"gh_code_scanning_alert", "open", map[string]string{"severity": "high", "tool": "CodeQL"}, 1, 3},
	{"gh_artifact_total", "live_bytes", nil, 100, 200},
	{"gh_actions_cache", "size_bytes", nil, 10, 20},
	{"gh_release", "downloads", map[string]string{"tag": "v1.0.0", "draft": "false", "prerelease": "false"}, 40, 60},
}

// twoOwnersPoints is each reading as the collectors write it, acme's an hour
// after alice's, so that a store keying the series by the short name keeps
// acme's reading and drops alice's.
func twoOwnersPoints() []sink.Point {
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	var out []sink.Point
	for _, r := range twoOwnersReadings {
		for i, owner := range []string{"alice", "acme"} {
			tags := map[string]string{"owner": owner, "repo": "dotfiles", "full_name": owner + "/dotfiles"}
			maps.Copy(tags, r.tags)
			value := []float64{r.alice, r.acme}[i]
			out = append(out, sink.Point{
				Measurement: r.measurement, Tags: tags,
				Fields: map[string]any{r.field: value}, Time: at.Add(time.Duration(i) * time.Hour),
			})
		}
	}
	return out
}

// twoOwnersPromSeries is what the exporter serves for those points: the
// gauges the reducer makes of them, named the way the exporter names them.
func twoOwnersPromSeries() []promSample {
	var out []promSample
	for _, g := range sink.Summarize(twoOwnersPoints()) {
		for field, raw := range g.Fields {
			v, ok := raw.(float64)
			if !ok {
				continue
			}
			labels := map[string]string{
				"__name__": "github_" + strings.TrimPrefix(g.Measurement, "gh_") + "_" + field,
			}
			maps.Copy(labels, g.Tags)
			out = append(out, promSample{labels, v})
		}
	}
	return out
}

// twoOwnersGraphiteSeries is the same points as the Graphite sink writes
// them: a node per tag in the order of the tag's name, each value made a node
// the way the sink makes one.
func twoOwnersGraphiteSeries() []grSeries {
	node := regexp.MustCompile(`[^A-Za-z0-9_:-]`)
	var out []grSeries
	for _, p := range twoOwnersPoints() {
		parts := []string{"github", strings.TrimPrefix(p.Measurement, "gh_")}
		for _, k := range slices.Sorted(maps.Keys(p.Tags)) {
			parts = append(parts, node.ReplaceAllString(p.Tags[k], "_"))
		}
		for field, raw := range p.Fields {
			v, _ := raw.(float64)
			out = append(out, grSeries{name: strings.Join(append(parts, field), "."), value: v})
		}
	}
	return out
}

// twoOwnersSum is one figure held to the two repositories: the panel and its
// query, and what the figure is with both counted. esOwn is why the
// Elasticsearch query is not held to it, where its description says so.
type twoOwnersSum struct {
	title, ref string
	want       float64
	esOwn      string
}

// twoOwnersSums are the figures of the three tiles #97 names and of the two
// breakdowns beside "Open alerts", whose bars add up to its Dependabot count.
var twoOwnersSums = []twoOwnersSum{
	{securityOpenAlerts, "A", 2 + 5, ""},
	{securityOpenAlerts, "B", 1 + 3, ""},
	{"Runs in range", "F", 100 + 200, ""},
	{"Runs in range", "G", 10 + 20, ""},
	{"Downloads", "A", 40 + 60, ""},
	{"Downloads", "B", 2, "a cardinality of the tag cannot be filtered on the newest value, " +
		"so it counts distinct tags, downloaded or not, as the panel says"},
	{"Alerts by severity", "A", 2 + 5, ""},
	{"Alerts by ecosystem", "A", 2 + 5, ""},
}

// TestTheNewestValueSumsCountTwoOwnersRepositoriesOfOneNameAsTwo is what the
// audit of 2.6.1 left for #97 in the sums of each repository's newest reading.
// esLatestSum and latestSumSQL took the newest reading per short repository
// name, so alice/dotfiles and acme/dotfiles were one series and the newer of
// the two readings stood for both: "Open alerts" read 5 where the two hold 7,
// and so did the artifact and cache bytes of "Runs in range" and the total of
// "Downloads". The exporter kept the same five measurements by `repo` alone,
// so Prometheus had one series for the two before any query ran. The
// Overview and the card key by full_name since 2.6.0 and 2.6.1.
//
// Prometheus and Graphite are evaluated over the two repositories as each
// store holds them: the gauges the exporter's own reducer makes of the
// points, and the paths the Graphite sink writes. Graphite keeps the full name
// in every path, so its sums never merged the two; it is held here all the
// same. The SQL stores and Elasticsearch keep the newest row of each series,
// which only a store can evaluate, so they are held to taking a series by the
// full name, and to the repository filter each query already had.
func TestTheNewestValueSumsCountTwoOwnersRepositoriesOfOneNameAsTwo(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		allValue, _ := store.Variable["allValue"].(string)
		vars := grafana.Vars{Datasource: store.DS, Repos: []string{"dotfiles"}, AllValue: allValue}
		panels := rendered(t, store.Name)
		for _, sum := range twoOwnersSums {
			target := targetOf(t, mustPanel(t, panels, sum.title), sum.ref)
			id := fmt.Sprintf("%s: %q query %s", store.Name, sum.title, sum.ref)
			switch store.Name {
			case "prometheus":
				expr, _ := vars.Apply(target)["expr"].(string)
				checkTwoOwnersSum(t, id, expr, sum.want, sumOf(evalPromSeries(t, expr, twoOwnersPromSeries())))
			case "graphite":
				expr, _ := vars.Apply(target)["target"].(string)
				var got float64
				for _, s := range evalGraphiteSeries(t, expr, twoOwnersGraphiteSeries()) {
					got += s.value
				}
				checkTwoOwnersSum(t, id, expr, sum.want, got)
			case "influxdb", "postgres":
				sql, _ := target["rawSql"].(string)
				checkBySQLFullName(t, id, sql)
			case "elasticsearch":
				if sum.esOwn == "" {
					checkByESFullName(t, id, target)
				}
			}
		}
	}
}

// targetOf is the query of a panel with the given refId.
func targetOf(t *testing.T, p map[string]any, ref string) map[string]any {
	t.Helper()
	for _, raw := range targetList(p) {
		target, _ := raw.(map[string]any)
		if target["refId"] == ref {
			return target
		}
	}
	t.Fatalf("%q has no query %s", p["title"], ref)
	return nil
}

func sumOf(samples []promSample) float64 {
	var out float64
	for _, s := range samples {
		out += s.value
	}
	return out
}

func checkTwoOwnersSum(t *testing.T, id, expr string, want, got float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s reads %v over alice/dotfiles and acme/dotfiles, which hold %v: %s", id, got, want, expr)
	}
}

// bySQLFullName is a window that takes the newest row of each series with the
// full name first among what it partitions by, dated per bucket or not.
var bySQLFullName = regexp.MustCompile(`ROW_NUMBER\(\) OVER \(PARTITION BY (\$__dateBin\(time\), )?full_name[,)\s]`)

func checkBySQLFullName(t *testing.T, id, sql string) {
	t.Helper()
	if !bySQLFullName.MatchString(sql) {
		t.Errorf("%s takes the newest row of each series by something other than the full name, "+
			"and two owners' repositories can share a name:\n%s", id, sql)
	}
	if !strings.Contains(sql, "repo IN (${repo:") {
		t.Errorf("%s no longer narrows to the picked repositories:\n%s", id, sql)
	}
}

func checkByESFullName(t *testing.T, id string, target map[string]any) {
	t.Helper()
	fields := bucketFieldsOf(asList(target["bucketAggs"]))
	if !slices.Contains(fields, any("full_name.keyword")) || slices.Contains(fields, any(inventoryRepoTerm)) {
		t.Errorf("%s buckets a repository by %v, want its full name: two owners' repositories "+
			"can share the short one", id, fields)
	}
	if query, _ := target["query"].(string); !strings.Contains(query, ESF) {
		t.Errorf("%s no longer narrows to the picked repositories: %s", id, query)
	}
}
