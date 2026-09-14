package grafana

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPanelsNumbersEveryPanelAndKeepsTheQueries pins the ordinal: it counts
// every panel that is not a row, text panels included, because that is the
// index that names one panel across the five dashboards.
func TestPanelsNumbersEveryPanelAndKeepsTheQueries(t *testing.T) {
	t.Parallel()
	doc := []map[string]any{
		{
			"title": "Stars", "type": "timeseries", "description": "per day",
			"targets": []any{map[string]any{"refId": "A"}, "not a target"},
		},
		{"title": "Not answered here", "type": "text"},
		{"type": "row", "panels": []any{
			map[string]any{"title": "Alerts", "type": "stat", "targets": []any{map[string]any{"refId": "B"}}},
			"not a panel",
			map[string]any{"title": "Only junk", "type": "stat", "targets": []any{"x"}},
		}},
		{"title": "Releases", "type": "table", "targets": []map[string]any{{"refId": "C"}}},
	}
	got := Panels(doc)
	type seen struct {
		index             int
		title, kind, desc string
		targets           int
	}
	var summary []seen
	for _, p := range got {
		summary = append(summary, seen{p.Index, p.Title, p.Type, p.Description, len(p.Targets)})
	}
	want := []seen{
		{0, "Stars", "timeseries", "per day", 1},
		{2, "Alerts", "stat", "", 1},
		{4, "Releases", "table", "", 1},
	}
	if !slices.Equal(summary, want) {
		t.Errorf("Panels =\n%v\nwant\n%v", summary, want)
	}
}

// TestApplyRendersATarget substitutes the datasource, the time filter and the
// repository variable in every string, at any depth, and leaves the rest
// alone.
func TestApplyRendersATarget(t *testing.T) {
	t.Parallel()
	ds := map[string]any{"type": "influxdb", "uid": "abc"}
	v := Vars{Datasource: ds, Repos: []string{"o/a", "o/b"}, TimeFilter: "time > now() - INTERVAL '90d'"}
	target := map[string]any{
		"datasource": "${DS_INFLUXDB}",
		"rawSql":     "SELECT * FROM t WHERE $__timeFilter(time) AND repo IN (${repo:singlequote})",
		"nested":     map[string]any{"list": []any{"$repo", 3, true}},
		"limit":      10,
	}
	got := v.Apply(target)
	if rendered, _ := got["datasource"].(map[string]any); rendered["uid"] != "abc" {
		t.Errorf("datasource = %v, want the store it is rendered for", got["datasource"])
	}
	wantSQL := "SELECT * FROM t WHERE time > now() - INTERVAL '90d' AND repo IN ('o/a','o/b')"
	if got["rawSql"] != wantSQL {
		t.Errorf("rawSql = %q, want %q", got["rawSql"], wantSQL)
	}
	nested, _ := got["nested"].(map[string]any)
	list, _ := nested["list"].([]any)
	if len(list) != 3 || list[0] != "{o/a,o/b}" || list[1] != 3 || list[2] != true {
		t.Errorf("nested = %v, want the variable rendered and the rest kept", got["nested"])
	}
	if got["limit"] != 10 {
		t.Errorf("limit = %v, want a number left alone", got["limit"])
	}
	if target["rawSql"] == got["rawSql"] {
		t.Error("Apply rewrote the target it was given instead of a copy")
	}
}

// TestApplyLeavesAnExpressionItsDatasource keeps the reference that makes
// Grafana answer a server-side expression itself.
func TestApplyLeavesAnExpressionItsDatasource(t *testing.T) {
	t.Parallel()
	for _, expr := range []map[string]any{
		{"uid": "__expr__"},
		{"type": "__expr__", "uid": "x"},
	} {
		got := Vars{Datasource: "store"}.Apply(map[string]any{"datasource": expr, "expression": "$A / $B"})
		if kept, _ := got["datasource"].(map[string]any); kept["uid"] != expr["uid"] {
			t.Errorf("datasource = %v, want the expression's own", got["datasource"])
		}
	}
	got := Vars{Datasource: "store"}.Apply(map[string]any{"datasource": "${DS_X}"})
	if got["datasource"] != "store" {
		t.Errorf("datasource = %v, want a query pointed at the store", got["datasource"])
	}
}

// TestApplyWithoutATimeFilterLeavesTheMacro leaves the macro for the
// PostgreSQL plugin, which expands it itself.
func TestApplyWithoutATimeFilterLeavesTheMacro(t *testing.T) {
	t.Parallel()
	got := Vars{}.Apply(map[string]any{"rawSql": "WHERE $__timeFilter(time)"})
	if got["rawSql"] != "WHERE $__timeFilter(time)" {
		t.Errorf("rawSql = %q, want the macro left in place", got["rawSql"])
	}
}

// TestRepoFormatsMatchGrafanas pins each of the formatters the five dashboards
// use, for one repository and for several, and the allValue that replaces all
// of them.
func TestRepoFormatsMatchGrafanas(t *testing.T) {
	t.Parallel()
	one, two := []string{"o/it's"}, []string{"o/a", "o/it's"}
	for _, tc := range []struct {
		token string
		repos []string
		all   string
		want  string
	}{
		{"${repo:singlequote}", two, "", `'o/a','o/it\'s'`},
		{"${repo:sqlstring}", two, "", `'o/a','o/it''s'`},
		{"${repo:sqlstring}", one, "", `'o/it''s'`},
		{"${repo:lucene}", two, "", `("o/a" OR "o/it's")`},
		{"${repo:lucene}", one, "", `"o/it's"`},
		{"${repo}", two, "", "{o/a,o/it's}"},
		{"$repo", one, "", "o/it's"},
		{"$repo", two, "*", "*"},
		{"${repo:lucene}", two, "*", "*"},
	} {
		got := Vars{Repos: tc.repos, AllValue: tc.all}.Apply(map[string]any{"q": "[" + tc.token + "]"})
		if got["q"] != "["+tc.want+"]" {
			t.Errorf("%s over %v (allValue %q) = %v, want [%s]", tc.token, tc.repos, tc.all, got["q"], tc.want)
		}
	}
}

// TestUnrenderedFindsEveryLeftover names each form of the variable once, from
// anywhere in the target.
func TestUnrenderedFindsEveryLeftover(t *testing.T) {
	t.Parallel()
	target := map[string]any{
		"expr":  `sum(x{repo=~"$repo"}) + sum(y{repo=~"$repo"})`,
		"query": []any{map[string]any{"q": "repo:${repo:lucene}"}},
	}
	got := Unrendered(target)
	want := []string{"${repo:lucene}", "$repo"}
	if !slices.Equal(got, want) {
		t.Errorf("Unrendered = %v, want %v", got, want)
	}
	if left := Unrendered(map[string]any{"expr": "up"}); len(left) != 0 {
		t.Errorf("Unrendered = %v for a target with no variable, want none", left)
	}
}

// TestAnswerSumsEveryQueryOfAPanel counts rows across refIds and reports the
// first failure in refId order, so the report does not depend on map order.
func TestAnswerSumsEveryQueryOfAPanel(t *testing.T) {
	t.Parallel()
	frame := map[string]any{"data": map[string]any{"values": []any{[]any{1, 2}}}}
	res := map[string]any{"results": map[string]any{
		"C": map[string]any{"error": "third"},
		"A": map[string]any{"frames": []any{frame}},
		"B": map[string]any{"error": "second", "frames": []any{frame}},
	}}
	if rows, errText := Answer(res); rows != 4 || errText != "B: second" {
		t.Errorf("Answer = %d, %q, want 4 rows and the failure of B", rows, errText)
	}
	if rows, errText := Answer(map[string]any{"message": "unauthorized"}); rows != 0 || errText != "unauthorized" {
		t.Errorf("Answer = %d, %q, want the top-level message", rows, errText)
	}
	if rows, errText := Answer(map[string]any{}); rows != 0 || errText != "" {
		t.Errorf("Answer = %d, %q, want nothing from an empty reply", rows, errText)
	}
}

// queryServer is a fake /api/ds/query that answers each request with one row
// per query it carried, and counts the requests.
func queryServer(t *testing.T, requests *atomic.Int32, lastBody *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
			return
		}
		var body struct {
			Queries []map[string]any `json:"queries"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the request is not JSON: %v", err)
			return
		}
		lastBody.Store(body.Queries)
		results := map[string]any{}
		for _, q := range body.Queries {
			ref, _ := q["refId"].(string)
			results[ref] = map[string]any{"frames": []any{
				map[string]any{"data": map[string]any{"values": []any{[]any{q["expr"]}}}},
			}}
		}
		out, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			t.Errorf("encoding the answer: %v", err)
			return
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckPanelsPostsEachPanelOnce sends every target of a panel in one
// request, rendered and paced the way a browser would, and returns the results
// in the order the panels were given.
func TestCheckPanelsPostsEachPanelOnce(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var lastBody atomic.Value
	srv := queryServer(t, &requests, &lastBody)
	panels := []PanelQuery{
		{Title: "one", Targets: []map[string]any{{"refId": "A", "expr": `x{repo=~"$repo"}`}, {"refId": "B", "expr": "y"}}},
		{Title: "two", Targets: []map[string]any{{"refId": "A", "expr": "z"}}},
		{Title: "three", Targets: []map[string]any{{"refId": "A", "expr": "w"}}},
	}
	results := Client{URL: srv.URL}.CheckPanels(t.Context(), "now-1h", "now", panels,
		Vars{Datasource: "ds", AllValue: ".*"}, Options{Timeout: 5 * time.Second, Workers: 2})
	if n := requests.Load(); n != 3 {
		t.Errorf("%d requests, want one per panel", n)
	}
	var titles []string
	for _, r := range results {
		titles = append(titles, r.Panel.Title)
		if r.Err != "" || r.Answer == nil {
			t.Errorf("%s: Err %q, Answer %v, want a clean answer", r.Panel.Title, r.Err, r.Answer)
		}
	}
	if !slices.Equal(titles, []string{"one", "two", "three"}) {
		t.Errorf("results are in the order %v, want the panels' order", titles)
	}
	if results[0].Rows != 2 {
		t.Errorf("the first panel counted %d rows, want one per query", results[0].Rows)
	}
}

// TestCheckPanelsSendsTheBrowsersPacing sets the interval and the point count
// only when they are given, and with a single worker when none is asked for.
func TestCheckPanelsSendsTheBrowsersPacing(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var lastBody atomic.Value
	srv := queryServer(t, &requests, &lastBody)
	panels := []PanelQuery{{Title: "one", Targets: []map[string]any{{"refId": "A"}}}}
	c := Client{URL: srv.URL}

	c.CheckPanels(t.Context(), "now-1h", "now", panels, Vars{},
		Options{Timeout: 5 * time.Second, IntervalMs: 3600000, MaxDataPoints: 500})
	sent, _ := lastBody.Load().([]map[string]any)
	if len(sent) != 1 || sent[0]["intervalMs"] != float64(3600000) || sent[0]["maxDataPoints"] != float64(500) {
		t.Errorf("sent %v, want the interval and the point count", sent)
	}

	c.CheckPanels(t.Context(), "now-1h", "now", panels, Vars{}, Options{Timeout: 5 * time.Second})
	sent, _ = lastBody.Load().([]map[string]any)
	if _, has := sent[0]["intervalMs"]; has {
		t.Errorf("sent %v, want no interval when none was given", sent)
	}
	if _, has := sent[0]["maxDataPoints"]; has {
		t.Errorf("sent %v, want no point count when none was given", sent)
	}
}

// TestCheckPanelsRefusesALeftoverVariable fails a panel the render left a
// variable in without asking the store, since it would come back empty rather
// than failing. Apply renders every string but a datasource reference, and an
// expression keeps its own, so a variable written into one survives the render.
func TestCheckPanelsRefusesALeftoverVariable(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var lastBody atomic.Value
	srv := queryServer(t, &requests, &lastBody)
	expr := map[string]any{"type": "__expr__", "uid": "${repo}"}
	panels := []PanelQuery{{Title: "one", Targets: []map[string]any{{"refId": "A", "datasource": expr}}}}
	results := Client{URL: srv.URL}.CheckPanels(t.Context(), "now-1h", "now", panels, Vars{},
		Options{Timeout: 5 * time.Second})
	if requests.Load() != 0 {
		t.Error("a panel with a leftover variable was posted anyway")
	}
	if !strings.Contains(results[0].Err, "${repo}") {
		t.Errorf("Err = %q, want the leftover variable named", results[0].Err)
	}
}

// TestCheckPanelsReportsAFailedRequest turns a request that never got an
// answer into that panel's error.
func TestCheckPanelsReportsAFailedRequest(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	panels := []PanelQuery{{Title: "one", Targets: []map[string]any{{"refId": "A"}}}}
	results := Client{URL: closed.URL}.CheckPanels(t.Context(), "now-1h", "now", panels, Vars{},
		Options{Timeout: time.Second})
	if results[0].Err == "" || results[0].Answer != nil {
		t.Errorf("result = %+v, want the request's failure and no answer", results[0])
	}
}

// frameServer answers every query with one frame whose fields and columns
// are given, so a test can hand a panel the exact shape a datasource would.
func frameServer(t *testing.T, fields []string, columns [][]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
			return
		}
		var body struct {
			Queries []map[string]any `json:"queries"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the request is not JSON: %v", err)
			return
		}
		schema := make([]any, len(fields))
		for i, f := range fields {
			schema[i] = map[string]any{"name": f, "type": "string"}
		}
		values := make([]any, len(columns))
		for i, c := range columns {
			values[i] = c
		}
		results := map[string]any{}
		for _, q := range body.Queries {
			ref, _ := q["refId"].(string)
			results[ref] = map[string]any{"frames": []any{map[string]any{
				"schema": map[string]any{"fields": schema},
				"data":   map[string]any{"values": values},
			}}}
		}
		out, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			t.Errorf("encoding the answer: %v", err)
			return
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPanelsReadTheLinkColumnsAndTheRenames pins how a panel's link columns
// are found: a byName override whose data link is the cell's own value or
// another column of the row, and the organize renames that map a frame's
// field to the column the override names.
func TestPanelsReadTheLinkColumnsAndTheRenames(t *testing.T) {
	t.Parallel()
	links := func(url string) any {
		return map[string]any{"id": "links", "value": []any{map[string]any{"url": url}}}
	}
	doc := []map[string]any{{
		"title": "Rows", "type": "table", "targets": []any{map[string]any{"refId": "A"}},
		"fieldConfig": map[string]any{"overrides": []any{
			map[string]any{
				"matcher":    map[string]any{"id": "byName", "options": "Link"},
				"properties": []any{links(LinkHref)},
			},
			map[string]any{
				"matcher":    map[string]any{"id": "byName", "options": "Downloads"},
				"properties": []any{links("${__data.fields.Page}")},
			},
			map[string]any{
				"matcher":    map[string]any{"id": "byName", "options": "User"},
				"properties": []any{links("https://github.com/${__value.raw}")},
			},
			map[string]any{
				"matcher":    map[string]any{"id": "byName", "options": "Stars"},
				"properties": []any{map[string]any{"id": "custom.width", "value": 90}},
			},
		}},
		"transformations": []any{map[string]any{"id": "organize", "options": map[string]any{
			"renameByName": map[string]any{"url.keyword": "Link", "n": "Count"},
		}}},
	}}
	got := Panels(doc)
	if len(got) != 1 {
		t.Fatalf("Panels found %d panels, want 1", len(got))
	}
	if !slices.Equal(got[0].Links, []string{"Link", "Page"}) {
		t.Errorf("Links = %v, want the cell's own column and the row's Page column, and not "+
			"the built url or the width", got[0].Links)
	}
	if got[0].Renames["url.keyword"] != "Link" || got[0].Renames["n"] != "Count" {
		t.Errorf("Renames = %v, want the organize renames", got[0].Renames)
	}
}

// TestCheckPanelsHoldsALinkColumnToItsValues is the check no offline read can
// make: the column a panel links from has to come back, under the name the
// datasource gives it before the panel renames it, and hold nothing but
// absolute urls, nulls and empty strings. An override matched against a column that is not
// there draws nothing and reports nothing, and a value that is not a url is
// a link to the Grafana host.
func TestCheckPanelsHoldsALinkColumnToItsValues(t *testing.T) {
	t.Parallel()
	panel := func(link string, renames map[string]string) PanelQuery {
		return PanelQuery{
			Title: "Rows", Links: []string{link}, Renames: renames,
			Targets: []map[string]any{{"refId": "A"}},
		}
	}
	for _, tc := range []struct {
		name    string
		panel   PanelQuery
		fields  []string
		columns [][]any
		wantErr string
	}{
		{
			"urls and a null pass", panel("Link", nil),
			[]string{"Repository", "Link"},
			[][]any{{"a", "b"}, {"https://github.com/a", nil}},
			"",
		},
		{
			"a renamed field is the column", panel("Link", map[string]string{"url.keyword": "Link"}),
			[]string{"repo.keyword", "url.keyword"},
			[][]any{{"a"}, {"https://github.com/a"}},
			"",
		},
		{
			"the column is missing", panel("Link", nil),
			[]string{"Repository", "Stars"},
			[][]any{{"a"}, {"3"}},
			"the Link column the panel links from is not in the answer",
		},
		{
			"a value that is not a url", panel("Link", nil),
			[]string{"Link"},
			[][]any{{"https://github.com/a", "octocat"}},
			`the Link column holds "octocat", which is not an absolute url`,
		},
		{
			"a relative url", panel("Page", nil),
			[]string{"Page"},
			[][]any{{"/octocat/repo"}},
			`the Page column holds "/octocat/repo", which is not an absolute url`,
		},
		{
			// Elasticsearch has no null: a terms bucket keyed by the empty
			// string is where a document without a url lands, and the
			// table draws it as the same empty cell the SQL stores draw.
			"the empty string is a missing url", panel("Link", map[string]string{"url.keyword": "Link"}),
			[]string{"repo.keyword", "url.keyword"},
			[][]any{{"a", "b"}, {"https://github.com/a", ""}},
			"",
		},
		{
			"an empty answer is not held to it", panel("Link", nil),
			[]string{"Repository"},
			[][]any{{}},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := frameServer(t, tc.fields, tc.columns)
			results := Client{URL: srv.URL}.CheckPanels(t.Context(), "now-1h", "now",
				[]PanelQuery{tc.panel}, Vars{}, Options{Timeout: 5 * time.Second})
			if results[0].Err != tc.wantErr {
				t.Errorf("Err = %q, want %q", results[0].Err, tc.wantErr)
			}
		})
	}
}

// TestAPanelsOwnRangeIsRead: a panel pinned with timeFrom is queried over
// that window and not the dashboard's, which is what Grafana does before the
// request leaves the browser. Without it the checker asked the Code panels
// for two years of gh_commit, which InfluxDB refuses for opening too many
// files, and reported six failures against a dashboard that renders.
func TestAPanelsOwnRangeIsRead(t *testing.T) {
	t.Parallel()
	panels := Panels([]any{
		map[string]any{
			"type": "timeseries", "title": "Pinned", "timeFrom": "90d",
			"targets": []any{map[string]any{"refId": "A"}},
		},
		map[string]any{
			"type": "timeseries", "title": "Free",
			"targets": []any{map[string]any{"refId": "A"}},
		},
	})
	if len(panels) != 2 {
		t.Fatalf("read %d panels, want 2", len(panels))
	}
	if panels[0].From != "90d" {
		t.Errorf("the pinned panel reports %q as its own range, want 90d", panels[0].From)
	}
	if panels[1].From != "" {
		t.Errorf("the free panel reports %q as its own range, want none", panels[1].From)
	}
}
