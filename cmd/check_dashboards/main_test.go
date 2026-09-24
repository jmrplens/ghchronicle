package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/dashboards"
	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// repoListSQL is the query the command asks the repository list with.
const repoListSQL = "SELECT DISTINCT repo FROM gh_repo"

// column is one named column of the frame the stand-in returns.
type column struct {
	name   string
	values []any
}

// answerFunc decides what the stand-in says to one query: the columns of the
// one frame it returns, no frame at all for nil, and the error it attaches
// instead of either.
type answerFunc func(query map[string]any) (columns []column, errText string)

// one is a frame of one unnamed column holding the values.
func one(values ...any) []column {
	if values == nil {
		values = []any{}
	}
	return []column{{name: "Value", values: values}}
}

// aliasedAs matches every column a SQL statement names, so a stand-in can
// answer a frame with exactly the columns the panel asked for.
var aliasedAs = regexp.MustCompile(`AS\s+"([^"]+)"`)

// columnsOf is a frame with one column per alias of the query's SQL, each
// holding one url, and a single unnamed column for a query without SQL. A url
// is what every column gets because that is what a link column is held to,
// and no other column minds.
func columnsOf(q map[string]any) []column {
	sql, _ := q["rawSql"].(string)
	var out []column
	for _, m := range aliasedAs.FindAllStringSubmatch(sql, -1) {
		out = append(out, column{name: m[1], values: []any{"https://github.com/octocat"}})
	}
	if len(out) == 0 {
		return one(1)
	}
	return out
}

// standIn is a Grafana /api/ds/query that answers each query of a request
// with what answer decides, and keeps every query it was sent.
type standIn struct {
	mu      sync.Mutex
	queries []map[string]any
}

// serve starts the stand-in and points the command at it with a token.
func serve(t *testing.T, answer answerFunc) *standIn {
	t.Helper()
	s := &standIn{}
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
		results := map[string]any{}
		for _, q := range body.Queries {
			s.mu.Lock()
			s.queries = append(s.queries, q)
			s.mu.Unlock()
			columns, errText := answer(q)
			ref, _ := q["refId"].(string)
			switch {
			case errText != "":
				results[ref] = map[string]any{"error": errText}
			case columns == nil:
				results[ref] = map[string]any{"frames": []any{}}
			default:
				fields := make([]any, len(columns))
				values := make([]any, len(columns))
				for i, c := range columns {
					fields[i] = map[string]any{"name": c.name, "type": "string"}
					values[i] = c.values
				}
				results[ref] = map[string]any{"frames": []any{map[string]any{
					"schema": map[string]any{"fields": fields},
					"data":   map[string]any{"values": values},
				}}}
			}
		}
		out, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			t.Errorf("encoding the answer: %v", err)
			return
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GRAFANA_URL", srv.URL)
	t.Setenv("GRAFANA_TOKEN", "glsa_test")
	return s
}

// sent is every query the stand-in received whose rawSql, expr or target field
// contains substr.
func (s *standIn) sent(substr string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, q := range s.queries {
		for _, field := range []string{"rawSql", "expr", "target"} {
			if v, _ := q[field].(string); strings.Contains(v, substr) {
				out = append(out, q)
				break
			}
		}
	}
	return out
}

// checkRun runs the command and returns its status and both streams.
func checkRun(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	status = run(t.Context(), args, &out, &errOut)
	return status, out.String(), errOut.String()
}

// panelCount is how many panels of a store's dashboard ask a query.
func panelCount(t *testing.T, name string) int {
	t.Helper()
	store, ok := dashboards.ByName(name)
	if !ok {
		t.Fatalf("there is no %s dashboard", name)
	}
	return len(grafana.Panels(store.Build(map[string]any{"uid": "x"})["panels"]))
}

// TestCheckInfluxDBRendersThePanelsFirst runs the InfluxDB dashboard against a
// store that answers everything: the repository list is read first and
// substituted, the time macro is expanded out of the range, and every panel is
// reported as answering.
func TestCheckInfluxDBRendersThePanelsFirst(t *testing.T) {
	s := serve(t, func(q map[string]any) ([]column, string) {
		if q["rawSql"] == repoListSQL {
			return one("octocat/hello-world", "octocat/spoon-knife"), ""
		}
		return columnsOf(q), ""
	})
	status, stdout, stderr := checkRun(t, "influxdb", "influx-uid")
	if status != 0 || stderr != "" {
		t.Fatalf("status %d, stderr %q, want a clean run", status, stderr)
	}
	answering := 0
	for line := range strings.Lines(stdout) {
		if strings.HasPrefix(line, "ok   ") {
			answering++
		}
	}
	if want := panelCount(t, "influxdb"); answering != want {
		t.Errorf("%d panels reported as answering, want every one of the %d", answering, want)
	}
	if !strings.HasSuffix(stdout, "\n0 failing, 0 empty\n") {
		t.Errorf("stdout ends %q, want the count", stdout[max(0, len(stdout)-40):])
	}
	if len(s.sent(repoListSQL)) != 1 {
		t.Error("the repository list was not asked for exactly once")
	}
	if len(s.sent("'octocat/hello-world','octocat/spoon-knife'")) == 0 {
		t.Error("no query carries the repository list in place of the variable")
	}
	if len(s.sent("time >= now() - INTERVAL '90d'")) == 0 {
		t.Error("no query carries the time macro expanded from the default range")
	}
	for _, q := range s.sent("FROM") {
		ds, _ := q["datasource"].(map[string]any)
		if ds["type"] != "influxdb" || ds["uid"] != "influx-uid" {
			t.Fatalf("a query went to %v, want the InfluxDB datasource given", q["datasource"])
		}
	}
}

// TestCheckReportsFailingAndEmptyPanels counts both kinds, and fails the run
// on a failing panel only. Prometheus has an allValue, so nothing is asked
// before the panels.
func TestCheckReportsFailingAndEmptyPanels(t *testing.T) {
	var once sync.Once
	var broken string
	s := serve(t, func(q map[string]any) ([]column, string) {
		expr, _ := q["expr"].(string)
		once.Do(func() { broken = expr })
		if expr == broken {
			return nil, "parse error: unexpected }"
		}
		return one(), ""
	})
	status, stdout, _ := checkRun(t, "prometheus", "prom-uid", "now-7d")
	if status != 1 {
		t.Errorf("status %d, want a failing panel to fail the run", status)
	}
	if !strings.Contains(stdout, "FAIL ") || !strings.Contains(stdout, "parse error: unexpected }") {
		t.Errorf("stdout does not report the failing panel with Grafana's reason:\n%s", stdout)
	}
	if !strings.Contains(stdout, "\nEMPTY ") {
		t.Errorf("stdout does not report the empty panels:\n%s", stdout)
	}
	// The count at the end is the one line a reader of a long run looks at,
	// so it has to agree with the panels reported above it.
	failing, empty := 0, 0
	for line := range strings.Lines(stdout) {
		switch {
		case strings.HasPrefix(line, "FAIL "):
			failing++
		case strings.HasPrefix(line, "EMPTY "):
			empty++
		}
	}
	if want := fmt.Sprintf("\n%d failing, %d empty\n", failing, empty); !strings.HasSuffix(stdout, want) {
		t.Errorf("stdout ends %q, want the count %q", stdout[max(0, len(stdout)-40):], want)
	}
	if len(s.sent(repoListSQL)) != 0 {
		t.Error("the repository list was asked for a dashboard whose variable has an allValue")
	}
	if len(s.sent(`repo=~".*"`)) == 0 {
		t.Error("no expression carries the allValue in place of the variable")
	}
}

// TestCheckHoldsEveryLinkColumnToItsValues is what would have caught a Link
// override with no column behind it before it was published: a panel whose
// link column comes back holding something that is not a url fails the run
// by name, and one whose link column is not in the answer at all fails too.
func TestCheckHoldsEveryLinkColumnToItsValues(t *testing.T) {
	const bad = "Open the longest"
	var missing sync.Once
	var dropped string
	s := serve(t, func(q map[string]any) ([]column, string) {
		if q["rawSql"] == repoListSQL {
			return one("octocat/hello-world"), ""
		}
		columns := columnsOf(q)
		sql, _ := q["rawSql"].(string)
		switch {
		case strings.Contains(sql, "FROM gh_pull_request WHERE") && strings.Contains(sql, "state = 'OPEN'"):
			for i := range columns {
				if columns[i].name == "Link" {
					columns[i].values = []any{"octocat"}
				}
			}
		case strings.Contains(sql, "FROM gh_gist"):
			missing.Do(func() { dropped = sql })
			var kept []column
			for _, c := range columns {
				if c.name != "Link" {
					kept = append(kept, c)
				}
			}
			return kept, ""
		}
		return columns, ""
	})
	status, stdout, _ := checkRun(t, "influxdb", "influx-uid")
	if status != 1 {
		t.Errorf("status %d, want a link column holding a name to fail the run", status)
	}
	if !strings.Contains(stdout, "FAIL "+bad+": ") || !strings.Contains(stdout, `the Link column holds "octocat", which is not an absolute url`) {
		t.Errorf("stdout does not name the panel whose link column holds a name:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAIL Gists: the Link column the panel links from is not in the answer") {
		t.Errorf("stdout does not name the panel whose link column went missing:\n%s", stdout)
	}
	if dropped == "" || len(s.sent("FROM gh_gist")) == 0 {
		t.Error("the Gists panel was never asked, so the missing column was never checked")
	}
}

// TestCheckStopsOnAnUnreadableRepositoryList fails before any panel is posted
// when the list the variable expands to cannot be read.
func TestCheckStopsOnAnUnreadableRepositoryList(t *testing.T) {
	s := serve(t, func(map[string]any) ([]column, string) { return nil, "" })
	status, stdout, stderr := checkRun(t, "postgres", "pg-uid")
	if status != 1 || !strings.Contains(stderr, "the repository list came back unreadable: no frames in the answer") {
		t.Errorf("status %d, stderr %q, want 1 and the list named as unreadable", status, stderr)
	}
	if stdout != "" || len(s.sent("FROM")) != 1 {
		t.Errorf("stdout %q, %d queries, want only the repository list asked", stdout, len(s.sent("FROM")))
	}
}

// TestCheckStopsWhenTheListCannotBeAsked fails the run on a reply that is not
// JSON, which is what a proxy in front of Grafana answers with.
func TestCheckStopsWhenTheListCannotBeAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GRAFANA_URL", srv.URL)
	t.Setenv("GRAFANA_TOKEN", "glsa_test")
	status, _, stderr := checkRun(t, "influxdb", "uid")
	if status != 1 || !strings.Contains(stderr, "502 Bad Gateway") {
		t.Errorf("status %d, stderr %q, want 1 and the proxy's status", status, stderr)
	}
}

// TestUsageNamesTheArgumentsItReads holds the usage line to the arguments the
// command reads. Given exactly the ones the line marks as required it gets past
// them to the next thing it needs, the token; given every one it does the same;
// given one fewer it refuses with the line itself. The line once marked the
// store optional ahead of a required uid, which promised a default that no
// argument list could reach.
func TestUsageNamesTheArgumentsItReads(t *testing.T) {
	sample := map[string]string{"store": "influxdb", "datasource-uid": "uid", "range": "now-7d"}
	var required, all []string
	for _, field := range strings.Fields(usage)[3:] {
		name := strings.Trim(field, "<>[]")
		value, known := sample[name]
		if !known {
			t.Fatalf("the usage names an argument %q this test has no value for", field)
		}
		all = append(all, value)
		if strings.HasPrefix(field, "<") {
			required = append(required, value)
		}
	}
	if len(required) == 0 {
		t.Fatalf("the usage %q marks no argument as required", usage)
	}

	for _, tc := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"only the required arguments", required, "GRAFANA_TOKEN is not set\n"},
		{"every argument", all, "GRAFANA_TOKEN is not set\n"},
		{"one required argument short", required[:len(required)-1], usage + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, func(map[string]any) ([]column, string) { return one(1), "" })
			t.Setenv("GRAFANA_TOKEN", "")
			status, stdout, stderr := checkRun(t, tc.args...)
			if status != 1 || stdout != "" || stderr != tc.stderr {
				t.Errorf("run %q = %d, %q, %q, want 1, \"\", %q",
					tc.args, status, stdout, stderr, tc.stderr)
			}
			if len(s.sent("")) != 0 {
				t.Error("a query was posted without a token")
			}
		})
	}
}

// TestCheckRefusesBeforeAskingAnything covers every way the command stops
// before a request: the usage asked for, too few arguments, a store it does
// not know, and no token.
func TestCheckRefusesBeforeAskingAnything(t *testing.T) {
	for _, tc := range []struct {
		name           string
		args           []string
		token          string
		status         int
		stdout, stderr string
	}{
		{"the usage asked for", []string{"--help"}, "t", 0, usage + "\n", ""},
		{"the usage asked for with -h", []string{"-h"}, "t", 0, usage + "\n", ""},
		{"the usage asked for with -help", []string{"-help"}, "t", 0, usage + "\n", ""},
		{"no arguments at all", nil, "t", 1, "", usage + "\n"},
		{"no uid", []string{"influxdb"}, "t", 1, "", usage + "\n"},
		{
			"a store it does not know",
			[]string{"mysql", "uid"},
			"t", 1, "",
			"mysql is not one of the dashboards: " + strings.Join(dashboards.Names(), ", ") + "\n",
		},
		{"no token", []string{"influxdb", "uid"}, "", 1, "", "GRAFANA_TOKEN is not set\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, func(map[string]any) ([]column, string) { return one(1), "" })
			t.Setenv("GRAFANA_TOKEN", tc.token)
			status, stdout, stderr := checkRun(t, tc.args...)
			if status != tc.status || stdout != tc.stdout || stderr != tc.stderr {
				t.Errorf("run = %d, %q, %q, want %d, %q, %q",
					status, stdout, stderr, tc.status, tc.stdout, tc.stderr)
			}
			if len(s.sent("")) != 0 {
				t.Error("a query was posted anyway")
			}
		})
	}
}

// TestVarsAsksForTheListWhenTheAllValueIsEmpty treats an empty allValue as
// none, the way Grafana does: All then expands to every repository the
// database knows about, so the list is asked for rather than every panel
// being handed an empty string in place of the variable.
func TestVarsAsksForTheListWhenTheAllValueIsEmpty(t *testing.T) {
	s := serve(t, func(q map[string]any) ([]column, string) {
		if q["rawSql"] == repoListSQL {
			return one("octocat/hello-world"), ""
		}
		return nil, "only the repository list is expected"
	})
	store := &dashboards.Store{Variable: map[string]any{"name": "repo", "allValue": ""}}
	ds := map[string]any{"type": "prometheus", "uid": "uid"}
	v, err := vars(t.Context(), grafana.New(), store, ds, "prometheus", "now-7d")
	if err != nil {
		t.Fatalf("vars = %v, want the list read", err)
	}
	if v.AllValue != "" || len(v.Repos) != 1 || v.Repos[0] != "octocat/hello-world" {
		t.Errorf("vars = allValue %q, repos %q, want the repository list in place of the empty allValue", v.AllValue, v.Repos)
	}
	if len(s.sent(repoListSQL)) != 1 {
		t.Error("the repository list was not asked for exactly once")
	}
}

// TestVarsExpandsTheTimeMacroOnlyForInfluxDB fills the macro in for the one
// store whose plugin cannot, and only out of a range that names a duration
// after now-. Any other store's plugin expands its own macros, and a range
// with nothing after now- would become an INTERVAL of an empty string,
// which no database reads.
func TestVarsExpandsTheTimeMacroOnlyForInfluxDB(t *testing.T) {
	store := &dashboards.Store{Variable: map[string]any{"name": "repo", "allValue": ".*"}}
	for _, tc := range []struct {
		name, store, rng     string
		filter, filterFormat string
	}{
		{"influxdb over a week", "influxdb", "now-7d", "time >= now() - INTERVAL '7d'", "time >= now() - INTERVAL '%s'"},
		{"prometheus over a week", "prometheus", "now-7d", "", ""},
		{"influxdb over a range with no duration", "influxdb", "now-", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := vars(t.Context(), grafana.Client{}, store, nil, tc.store, tc.rng)
			if err != nil {
				t.Fatalf("vars = %v, want nothing asked of a variable with an allValue", err)
			}
			if v.AllValue != ".*" || v.TimeFilter != tc.filter || v.TimeFilterFormat != tc.filterFormat {
				t.Errorf("vars = allValue %q, filter %q, format %q, want %q, %q and %q",
					v.AllValue, v.TimeFilter, v.TimeFilterFormat, ".*", tc.filter, tc.filterFormat)
			}
		})
	}
}

// TestARefusedColumnIsReportedApartFromAnAbsentTable: the run against the
// owner's own store on 2026-09-17 reported 34 failing panels under one word,
// 33 of them families the backfill had not reached and one a panel asking for
// a column that account will never have. A releaser reading "34 failing" has
// no way to tell the gap that fills itself from the panel nobody can draw, so
// the two are counted and named apart above the tally.
func TestARefusedColumnIsReportedApartFromAnAbsentTable(t *testing.T) {
	serve(t, func(q map[string]any) ([]column, string) {
		sql, _ := q["rawSql"].(string)
		switch {
		case sql == repoListSQL:
			return one("octocat/hello-world"), ""
		case strings.Contains(sql, "FROM gh_dependabot_alert_item"):
			return nil, "Schema error: No field named dismissed_reason. Valid fields are " +
				"gh_dependabot_alert_item.alert_state, gh_dependabot_alert_item.alerts."
		case strings.Contains(sql, "FROM gh_commit "):
			return nil, "table 'public.iox.gh_commit' not found"
		}
		return columnsOf(q), ""
	})
	status, stdout, _ := checkRun(t, "influxdb", "influx-uid")
	if status != 1 {
		t.Fatalf("status %d, want a refused panel to fail the run", status)
	}
	for _, want := range []string{
		"name a column this store has not created",
		"Time to resolve an alert",
		"waiting on a table this store has not created",
		"WAIT Commits:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the run does not say %q:\n%s", want, stdout)
		}
	}
	// The tally stays the last line, since that is what a long run is read by,
	// and it counts the refused columns and not the absent tables.
	if !strings.HasSuffix(stdout, "\n2 failing, 0 empty\n") {
		t.Errorf("stdout ends %q, want a tally counting the two refused columns alone",
			stdout[max(0, len(stdout)-40):])
	}
}

// TestATableTheStoreHasNotCreatedDoesNotFailTheRun: the release step is a
// checklist item, so its status has to mean something. Against the production
// store on 2026-09-17 thirty-three panels named a family the backfill had not
// reached and one named a column that account will never have, and the command
// exited 1 for both, which is how an item becomes a thing people tick. A store
// that is merely incomplete now passes.
func TestATableTheStoreHasNotCreatedDoesNotFailTheRun(t *testing.T) {
	serve(t, func(q map[string]any) ([]column, string) {
		sql, _ := q["rawSql"].(string)
		switch {
		case sql == repoListSQL:
			return one("octocat/hello-world"), ""
		case strings.Contains(sql, "FROM gh_commit "):
			return nil, "table 'public.iox.gh_commit' not found"
		}
		return columnsOf(q), ""
	})
	status, stdout, _ := checkRun(t, "influxdb", "influx-uid")
	if status != 0 {
		t.Errorf("status %d, want a store whose backfill is still walking to pass:\n%s",
			status, stdout)
	}
	if !strings.Contains(stdout, "they do not fail the run") ||
		!strings.HasSuffix(stdout, "\n0 failing, 0 empty\n") {
		t.Errorf("the run does not report the waiting panels apart from the failures:\n%s", stdout)
	}
}

// TestEveryStoreSpellingOfAMissingColumnIsRecognised: each store words it
// differently, and PostgreSQL words a missing table and a missing column
// almost alike, both ending in "does not exist", so the table spellings are
// tried first.
func TestEveryStoreSpellingOfAMissingColumnIsRecognised(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  string
		want failureKind
	}{
		{"Schema error: No field named dismissed_reason.", missingColumn},
		{`pq: column "dismissed_reason" does not exist`, missingColumn},
		{"table 'public.iox.gh_commit' not found", missingTable},
		{`pq: relation "gh_commit" does not exist`, missingTable},
		{"index_not_found_exception", missingTable},
		{"parse error: unexpected }", otherFailure},
		{"context deadline exceeded", otherFailure},
	} {
		if got := classify(c.err); got != c.want {
			t.Errorf("%q was read as %d, want %d", c.err, got, c.want)
		}
	}
}
