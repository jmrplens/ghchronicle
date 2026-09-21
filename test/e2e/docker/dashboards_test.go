//go:build dockere2e

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// The five dashboards, against the five stores one sweep loaded.
//
// Every other suite here reads a store with the store's own API, which proves
// the sink's bytes arrived. It does not prove a panel can ask for them. A
// dashboard reaches a store through a Grafana datasource plugin, and the
// plugin rejects what the raw API accepts, aggregates on fields the mapping may
// not allow, and expands macros the query cannot do without. Until this suite
// the only thing that ever ran these queries was a person pointing
// cmd/check_dashboards at their own Grafana.
//
// So this posts every panel of every committed dashboard to /api/ds/query, the
// way a rendered panel does, and asks three questions in order of value:
//
//  1. did the datasource reject it,
//  2. did a panel the sweep has something to show in answer with something,
//  3. do the stores agree where they claim to hold the same fact.
//
// The run itself is cmd/check_dashboards' own, lifted into internal/grafana so
// that the command and this suite cannot drift into asking different
// questions.

// The committed dashboards are the subject rather than the builder in
// internal/dashboards: they are the files a user imports, nothing outside
// cmd/ may import that package, and `make check-dashboards` already fails when
// the two disagree.
const dashboardsDir = "../../../dashboards"

// dashboardRange is the range the committed dashboards open at. It is asserted
// to be this rather than read blindly, because the whole suite is framed by it:
// an empty panel is judged against what the sweep wrote inside this window.
//
// Deliberately not a wider one. Measured here at now-3y, two stores answer
// differently for reasons that belong to a store that has seen one sweep and
// not to any dashboard:
//
//   - Graphite keeps two archives, 1h:120d and 1d:12y. A query wider than the
//     hourly archive reads the daily one, which whisper fills only by
//     propagating complete hourly slots, so after a single sweep it is empty
//     and a counting panel answers 0 where the other stores answer 1. The
//     hourly archive was thirty days until the Code panels pinned themselves
//     to ninety, which put two of them the wrong side of that edge although
//     the dashboard range never moved; storage-schemas.conf says the rest.
//   - Elasticsearch refuses a date_histogram of one hour across years:
//     "Trying to create too many buckets ... [65536]". Four panels fail at
//     now-3y and none of them at thirty days.
//
// Both are worth knowing and neither is a defect this suite should report as
// one, so the range under test is the dashboard's own.
const (
	dashboardRange  = "now-30d"
	dashboardWindow = 30 * 24 * time.Hour
)

// What a rendered panel sends alongside the query. Grafana derives both from
// the width of the panel in the browser, and several queries interpolate
// $__interval and $__rate_interval out of them.
const (
	dashboardInterval      = 3600000
	dashboardMaxDataPoints = 500
)

// dashboardStore is one committed dashboard and the datasource that answers
// it. The plugin id is named because /api/ds/query carries one, and a dashboard
// posted with the wrong one is not the request a render makes.
type dashboardStore struct {
	name   string
	uid    string
	plugin string
}

var dashboardStores = []dashboardStore{
	{"influxdb", DatasourceInflux, "influxdb"},
	{"postgres", DatasourcePostgres, "grafana-postgresql-datasource"},
	{"graphite", DatasourceGraphite, "graphite"},
	{"elasticsearch", DatasourceElasticsearch, "elasticsearch"},
	{"prometheus", DatasourcePrometheus, "prometheus"},
}

// dashboardOutcome is what one panel did.
type dashboardOutcome struct {
	panel grafana.PanelQuery
	err   string
	// values is every non-null value the panel drew, out of every column that
	// is not the time; numbers is those of them that are numbers.
	values  int
	numbers []float64
	// named is the subset of those numbers that a column name identifies: a
	// column holding exactly one number, which is the shape of a stat. It is
	// what lets the cross-store comparison read a group of named values one
	// value at a time, rather than skipping any panel that draws more than
	// one number.
	named map[string]float64
}

// answered reports whether the panel put anything on the screen.
//
// Counting rows would not do, and Graphite is why: a Graphite query over a
// thirty day window answers with 720 hourly slots whether or not any of them
// holds anything, so a panel with no data comes back as 720 rows of null. The
// trap in the other direction is SQL, where COUNT(*) over no rows answers one
// row holding zero, which is a real value and is counted. A stat or a gauge is
// stricter still: it displays a number, and a terms bucket's own key is not
// one.
func (o *dashboardOutcome) answered() bool {
	if o.panel.Type == "stat" || o.panel.Type == "gauge" {
		return len(o.numbers) > 0
	}
	return o.values > 0
}

// dashboardRun is one pass of all five dashboards over the loaded stores.
type dashboardRun struct {
	// outcomes is keyed by store name and then by the panel's index, which is
	// the same panel in all five dashboards.
	outcomes map[string]map[int]dashboardOutcome
	// subject is the measurements each panel reads, taken from the InfluxDB
	// dashboard's SQL because it is the dialect the others are translated from
	// and the same panel reads the same thing in every store.
	subject map[int][]string
	// inWindow counts the sweep's points per measurement inside the dashboard
	// range, and written holds every field or tag name a measurement was
	// written with a non-empty value for. Together they are what turns "this
	// panel is empty" into either "there was nothing to show" or "the query is
	// wrong".
	inWindow map[string]int
	written  map[string]map[string]bool
	// promSkip says why Prometheus holds nothing, and is empty when it holds
	// the sweep. The exporter is scraped rather than pushed to, and a machine
	// that firewalls its docker bridge will not let the container reach it.
	promSkip string
}

var (
	dashboardsOnce   sync.Once
	dashboardsShared *dashboardRun
	errDashboards    error
)

// dashboardsRun loads every store from one sweep and runs all five dashboards
// against them, once per test binary. Every query is issued here rather than in
// the tests themselves, because the Prometheus half depends on an exporter
// process that lives only as long as the test that started it.
func dashboardsRun(t *testing.T, s *Stack) *dashboardRun {
	t.Helper()
	dashboardsOnce.Do(func() {
		dashboardsShared, errDashboards = dashboardsRunInto(t, s)
	})
	if errDashboards != nil {
		t.Fatalf("the dashboards never ran: %v", errDashboards)
	}
	return dashboardsShared
}

func dashboardsRunInto(t *testing.T, s *Stack) (*dashboardRun, error) {
	t.Helper()
	// The context is the first test's, which is the one the whole run happens
	// under: everything below is issued inside the sync.Once above.
	ctx := t.Context()
	run := &dashboardRun{outcomes: map[string]map[int]dashboardOutcome{}}
	sweep := sqlStoresRun(ctx, t, s)
	oracle := sqlStoresPoints(t, sweep)
	run.inWindow, run.written = dashboardOracle(oracle, time.Now().Add(-dashboardWindow))

	// InfluxDB, Elasticsearch and Graphite were written by the sweeps
	// themselves. PostgreSQL is a file the SQL sink wrote, and psql is what
	// puts it in, exactly as the sink's documentation says.
	if out, err := replaySQL(ctx, s, sweep.SQL); err != nil {
		return nil, fmt.Errorf("loading the SQL sink's file: %w\n%s", err, out)
	}

	push := pushSweepRun(ctx, t, s)
	esRefresh(ctx, t, s)
	graphiteAwaitMeasurements(ctx, t, s, push)
	if err := dashboardAwaitGraphite(ctx, s); err != nil {
		return nil, err
	}
	run.promSkip = dashboardsLoadPrometheus(t, s)

	client := grafana.Client{URL: s.GrafanaURL, Token: s.GrafanaToken}
	repos := dashboardRepos(oracle)
	for _, store := range dashboardStores {
		doc, err := dashboardDocument(store.name)
		if err != nil {
			return nil, err
		}
		if from := dashboardDefaultRange(doc); from != dashboardRange {
			return nil, fmt.Errorf("the %s dashboard opens at %q, and this suite judges every "+
				"empty panel against what the sweep wrote inside %q", store.name, from, dashboardRange)
		}
		panels := grafana.Panels(doc["panels"])
		if len(panels) == 0 {
			return nil, fmt.Errorf("the %s dashboard has no panel with a query", store.name)
		}
		if store.name == "influxdb" {
			run.subject = dashboardSubjects(panels)
		}
		started := time.Now()
		results := client.CheckPanels(ctx, dashboardRange, "now", panels,
			dashboardVars(doc, store, repos), grafana.Options{
				Timeout:       120 * time.Second,
				Workers:       dashboardWorkers(store.name),
				IntervalMs:    dashboardInterval,
				MaxDataPoints: dashboardMaxDataPoints,
			})
		run.outcomes[store.name] = dashboardOutcomes(results)
		dashboardRetryEmpty(ctx, t, client, store.name, panels, dashboardVars(doc, store, repos), run.outcomes[store.name])
		t.Logf("%-14s %d panels in %s", store.name, len(panels), time.Since(started).Round(time.Millisecond))
		if reportErr := dashboardReport(store.name, results, run.outcomes[store.name]); reportErr != nil {
			return nil, reportErr
		}
	}
	return run, nil
}

func dashboardOutcomes(results []grafana.Result) map[int]dashboardOutcome {
	out := make(map[int]dashboardOutcome, len(results))
	for _, r := range results {
		values, numbers, named := dashboardAnswer(r.Answer)
		out[r.Panel.Index] = dashboardOutcome{
			panel: r.Panel, err: r.Err, values: values, numbers: numbers, named: named,
		}
	}
	return out
}

// dashboardRetryEmpty asks again, one panel at a time, for every panel that
// answered nothing, and keeps the better of the two answers.
//
// This is here for Graphite and it is deliberately visible rather than hidden
// in the runner. Across cold runs a handful of Graphite panels answered nothing
// and answered correctly when asked again, and the ones that did were a
// different, contiguous group every time: panels 8 to 10 in one run, 36 to 38
// in the next, which is the shape of a moment rather than of a query. Carbon is
// not the cause: by the time a dashboard is asked, the sweep has been on disk
// for over a minute (measured, one point is readable a second after it is
// sent), and a serial re-ask sees it. Eight renders at once against
// graphite-web is the only thing left, so the second attempt is serial.
//
// The log line is the point of it. A store that needs this every run is a
// finding, and the assertions still fail if the retry does not answer either.
// dashboardWorkers is how many panels of one store are asked at once.
//
// Eight keeps a dashboard's 148 panels to seconds rather than minutes, and no
// store here is asked for enough data to make the concurrency the subject of
// the measurement. Graphite is the exception, and it is not a tuning
// preference: graphite-web served under eight concurrent renders returns an
// empty series for a path that holds data, a different and contiguous group of
// panels each run. That would be survivable if it showed up as an empty panel,
// which the retry below recovers, but many of these targets wrap the series in
// isNonNull and summarize, and those turn no data into the number 0 rather than
// into nothing. The panel then answers, plausibly and wrongly, the retry never
// fires because nothing looks empty, and the third assertion reports a
// disagreement between stores that does not exist. One at a time is what makes
// Graphite's answers reproducible; it costs about half a minute.
func dashboardWorkers(store string) int {
	if store == "graphite" {
		return 1
	}
	return 8
}

func dashboardRetryEmpty(ctx context.Context, t *testing.T, client grafana.Client, store string,
	panels []grafana.PanelQuery, vars grafana.Vars, outcomes map[int]dashboardOutcome,
) {
	t.Helper()
	// Twice, and kept although dashboardWorkers now asks Graphite one panel at
	// a time and the recoveries went from one to six a run to none in either of
	// two cold runs. It stays as the net for a store that answers late for some
	// other reason, and it reports what it recovered, so a number climbing back
	// off zero is the signal that something has started answering slowly again
	// rather than a thing quietly papered over.
	for range 2 {
		var again []grafana.PanelQuery
		for _, p := range panels {
			o := outcomes[p.Index]
			if o.err == "" && !o.answered() {
				again = append(again, p)
			}
		}
		if len(again) == 0 {
			return
		}
		results := client.CheckPanels(ctx, dashboardRange, "now", again, vars, grafana.Options{
			Timeout: 120 * time.Second, Workers: 1,
			IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
		})
		changed := 0
		retried := dashboardOutcomes(results)
		for _, index := range slices.Sorted(maps.Keys(retried)) {
			o := retried[index]
			if o.err == "" && o.answered() {
				outcomes[index] = o
				changed++
			}
		}
		if changed == 0 {
			return
		}
		t.Logf("%-14s %d of %d empty panels answered when asked again, one at a time",
			store, changed, len(again))
	}
}

// dashboardsLoadPrometheus starts an exporter and points Prometheus at it, and
// says why it could not when it could not. The scrape is the one thing on this
// machine a firewall can refuse, and a suite that failed for it would be
// reporting the firewall rather than the dashboard.
func dashboardsLoadPrometheus(t *testing.T, s *Stack) string {
	t.Helper()
	run := startExporterSweep(t)
	if err := s.SetPrometheusTarget(t.Context(), run.Port); err != nil {
		if errors.Is(err, ErrExporterUnreachable) {
			return err.Error()
		}
		t.Fatalf("pointing Prometheus at the exporter: %v", err)
	}
	// SetPrometheusTarget returns after one successful scrape, and one sample
	// is not a dashboard. Every "per day" and "over the range" panel of the
	// Prometheus dashboard is an increase() over a counter, and increase() of a
	// series with a single sample is nothing at all. The scrape interval is
	// five seconds, so this is about fifteen.
	if err := WaitUntil(t.Context(), "prometheus to scrape the exporter three times",
		2*time.Minute, func(ctx context.Context) error {
			n, err := dashboardPromScalar(ctx, s, `count_over_time(up{job="ghchronicle"}[10m])`)
			if err != nil {
				return err
			}
			if n < 3 {
				return fmt.Errorf("prometheus has scraped the exporter %v times", n)
			}
			return nil
		}); err != nil {
		return err.Error()
	}
	return ""
}

// dashboardPromScalar is the first value of an instant query.
func dashboardPromScalar(ctx context.Context, s *Stack, expr string) (float64, error) {
	var out struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	where := s.PrometheusURL + "/api/v1/query?query=" + url.QueryEscape(expr)
	if err := storeJSON(ctx, http.MethodGet, where, nil, &out); err != nil {
		return 0, err
	}
	if len(out.Data.Result) == 0 {
		return 0, fmt.Errorf("%s answered no series", expr)
	}
	text, _ := out.Data.Result[0].Value[1].(string)
	return strconv.ParseFloat(text, 64)
}

// dashboardAwaitGraphite waits until carbon has finished writing its cache out
// to whisper files.
//
// graphiteAwaitMeasurements, which runs first, waits for one leaf per
// measurement, and that is the right gate for the suite it belongs to. It is
// not enough here. A panel asks for one named field, carbon drains its cache a
// metric at a time, and a measurement whose first leaf exists can still be
// missing the leaf this panel reads. Nothing bridges the gap on the read side
// either: graphite-web can merge carbon's unflushed cache into a render over
// CarbonLink, and in this image it does not, so a point carbon has accepted is
// not yet a point a panel can draw.
//
// Measured across cold runs, the panels this hit were different every time,
// which is a suite reporting a dashboard defect that is not there. Carbon's own
// carbon.agents.*.cache.size says what it is: twenty points still queued when
// the sweep returned, and zero a minute later. That metric is reported once a
// minute, too coarse to wait on, so the gate is the number of leaves the render
// API can see, which is live, has to stop growing, and costs one small request.
//
// A quiet period rather than an expected count, because how many leaves a sweep
// creates is the sink's own business (one per numeric field per tag set,
// booleans included) and restating it here would be a second copy of that rule,
// wrong the first time either changed.
func dashboardAwaitGraphite(ctx context.Context, s *Stack) error {
	const quiet = 32 // WaitUntil polls every 250ms, so eight seconds without a new leaf
	last, stable := -1, 0
	return WaitUntil(ctx, "carbon to finish writing the sweep to whisper", 3*time.Minute,
		func(ctx context.Context) error {
			now := time.Now()
			points, err := graphiteRender(ctx, s, "countSeries(github.**)", now.Add(-time.Hour), now)
			if err != nil {
				return err
			}
			leaves := 0
			for _, p := range points {
				if p.value != nil {
					leaves = int(*p.value)
				}
			}
			if leaves > 0 && leaves == last {
				stable++
			} else {
				stable = 0
			}
			last = leaves
			if stable < quiet {
				return fmt.Errorf("the render API sees %d leaves and carbon is still writing", leaves)
			}
			return nil
		})
}

// dashboardDocument reads one committed dashboard.
func dashboardDocument(store string) (map[string]any, error) {
	path := filepath.Join(dashboardsDir, "ghchronicle-"+store+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if decodeErr := json.Unmarshal(raw, &doc); decodeErr != nil {
		return nil, fmt.Errorf("%s: %w", path, decodeErr)
	}
	return doc, nil
}

// dashboardVars is the substitution a render performs and /api/ds/query does
// not. The allValue comes out of the dashboard's own variable rather than a
// list kept here, so a dashboard that changed how it expands All changes this
// with it.
func dashboardVars(doc map[string]any, store dashboardStore, repos []string) grafana.Vars {
	v := grafana.Vars{
		Datasource: map[string]any{"type": store.plugin, "uid": store.uid},
		AllValue:   dashboardAllValue(doc),
	}
	if v.AllValue == "" {
		v.Repos = repos
	}
	// InfluxDB 3 in SQL mode has no macro of its own, so the dashboard's
	// $__timeFilter(time) is expanded here, the way cmd/check_dashboards
	// expands it. The PostgreSQL plugin expands its own, $__timeGroupAlias
	// included, so those queries are posted with the macros intact and the
	// plugin is part of what is under test.
	if store.name == "influxdb" {
		// The format as well as the value: a panel pinned to its own range
		// gets the macro substituted from that range instead, the way a
		// render does, and the checker in cmd/check_dashboards must not be
		// asking a different question from this suite.
		v.TimeFilterFormat = "time >= now() - INTERVAL '%s'"
		v.TimeFilter = fmt.Sprintf(v.TimeFilterFormat,
			strings.TrimPrefix(dashboardRange, "now-"))
	}
	return v
}

// dashboardDefaultRange is the range the dashboard opens at.
func dashboardDefaultRange(doc map[string]any) string {
	window, _ := doc["time"].(map[string]any)
	from, _ := window["from"].(string)
	return from
}

// dashboardAllValue reads the repository variable's allValue, which Grafana
// substitutes verbatim when All is selected.
func dashboardAllValue(doc map[string]any) string {
	templating, _ := doc["templating"].(map[string]any)
	list, _ := templating["list"].([]any)
	for _, raw := range list {
		v, _ := raw.(map[string]any)
		if v["name"] != "repo" {
			continue
		}
		all, _ := v["allValue"].(string)
		return all
	}
	return ""
}

// dashboardRepos is what the repository variable's own query would answer:
// every repository gh_repo carries.
func dashboardRepos(points []sqlStoresPoint) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range points {
		if p.Measurement != "gh_repo" {
			continue
		}
		if name := p.Tags["repo"]; name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

var dashboardMeasurement = regexp.MustCompile(`\bgh_[a-z_]+`)

// dashboardSubjects is the measurements each panel reads, read out of the SQL
// the InfluxDB dashboard carries.
func dashboardSubjects(panels []grafana.PanelQuery) map[int][]string {
	out := map[int][]string{}
	for _, p := range panels {
		var sql strings.Builder
		for _, t := range p.Targets {
			if s, ok := t["rawSql"].(string); ok {
				sql.WriteString(s)
				sql.WriteByte(' ')
			}
		}
		for _, m := range dashboardMeasurement.FindAllString(sql.String(), -1) {
			if !slices.Contains(out[p.Index], m) {
				out[p.Index] = append(out[p.Index], m)
			}
		}
	}
	return out
}

// dashboardOracle reduces the sweep's own points to the two questions the
// assertions ask of them: how much of a measurement is inside the dashboard
// range, and which of its fields were ever written with a value.
//
// A field written as an empty string counts as not written, because that is
// what the stores do with it: the InfluxDB line protocol drops it, so the
// column is never created and a query naming it fails to plan, while the SQL
// sink declares the column in its DDL and leaves it null.
func dashboardOracle(points []sqlStoresPoint, since time.Time) (inWindow map[string]int, written map[string]map[string]bool) {
	inWindow = map[string]int{}
	written = map[string]map[string]bool{}
	for _, p := range points {
		if written[p.Measurement] == nil {
			written[p.Measurement] = map[string]bool{}
		}
		if at, err := time.Parse(time.RFC3339Nano, p.Time); err == nil && at.After(since) {
			inWindow[p.Measurement]++
		}
		for k, v := range p.Tags {
			if v != "" {
				written[p.Measurement][k] = true
			}
		}
		for k, v := range p.Fields {
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			written[p.Measurement][k] = true
		}
	}
	return inWindow, written
}

// dashboardReport keeps one line per panel beside the other artifacts, because
// a failure names a panel and the next question is always what the other 147
// did.
func dashboardReport(store string, results []grafana.Result, outcomes map[int]dashboardOutcome) error {
	dir := filepath.Join("out", "dashboards")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	var b strings.Builder
	dump := make([]map[string]any, 0, len(results))
	for _, r := range results {
		o := outcomes[r.Panel.Index]
		switch {
		case o.err != "":
			fmt.Fprintf(&b, "FAIL  %3d %-48s %s\n", r.Panel.Index, r.Panel.Title, grafana.Trim(o.err, 400))
		case !o.answered():
			fmt.Fprintf(&b, "EMPTY %3d %s\n", r.Panel.Index, r.Panel.Title)
		default:
			fmt.Fprintf(&b, "ok    %3d %-48s %d values in %d rows\n",
				r.Panel.Index, r.Panel.Title, o.values, r.Rows)
		}
		dump = append(dump, map[string]any{
			"index": r.Panel.Index, "title": r.Panel.Title, "type": r.Panel.Type,
			"desc": r.Panel.Description, "rows": r.Rows, "err": o.err,
			"values": o.values, "numbers": o.numbers,
		})
	}
	if err := os.WriteFile(filepath.Join(dir, store+".txt"), []byte(b.String()), 0o600); err != nil {
		return err
	}
	raw, err := json.Marshal(dump)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, store+".json"), raw, 0o600)
}

// dashboardAnswer is what a panel put on the screen: how many values it drew,
// and those of them that are numbers. The time column is skipped: a timestamp
// is the axis, not an answer.
func dashboardAnswer(res map[string]any) (values int, numbers []float64, named map[string]float64) {
	results, _ := res["results"].(map[string]any)
	named = map[string]float64{}
	for _, ref := range slices.Sorted(maps.Keys(results)) {
		answer, _ := results[ref].(map[string]any)
		frames, _ := answer["frames"].([]any)
		for _, raw := range frames {
			frame, _ := raw.(map[string]any)
			n, nums, cols := dashboardFrameAnswer(frame)
			values += n
			numbers = append(numbers, nums...)
			for name, v := range cols {
				// A name two frames both answer is not one value, so it is
				// not comparable and neither copy is kept.
				if _, clash := named[name]; clash {
					named[name] = math.NaN()
					continue
				}
				named[name] = v
			}
		}
	}
	return values, numbers, named
}

// dashboardFrameAnswer also returns every column that holds exactly one
// number, by its own name. That is a stat's shape, one tile or one value of a
// group, and it is the only shape a number can be compared across stores by
// name at all: a column of many rows has no single reading, and a column of
// none has nothing to read.
func dashboardFrameAnswer(frame map[string]any) (values int, numbers []float64, named map[string]float64) {
	schema, _ := frame["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	data, _ := frame["data"].(map[string]any)
	columns, _ := data["values"].([]any)
	named = map[string]float64{}
	for i, raw := range fields {
		if i >= len(columns) {
			break
		}
		field, _ := raw.(map[string]any)
		if field["type"] == "time" {
			continue
		}
		column, _ := columns[i].([]any)
		var only []float64
		for _, v := range column {
			if v == nil {
				continue
			}
			values++
			if n, ok := v.(float64); ok {
				numbers = append(numbers, n)
				only = append(only, n)
			}
		}
		name, _ := field["name"].(string)
		if name != "" && len(only) == 1 && len(column) == 1 {
			named[name] = only[0]
		}
	}
	return values, numbers, named
}

// ── 1. No panel query errors ────────────────────────────────────────────────

// dashboardMissing is every panel a store refuses because the query names
// something this sweep never wrote: the value is the identifier the store
// reports missing, either a bare measurement or measurement.field, and the test
// checks against the sweep's own points that it really is absent before
// accepting the excuse.
//
// The class matters more than the list. InfluxDB 3 creates a column the first
// time it sees a value and DataFusion refuses to plan a query naming a column
// that does not exist, so a panel selecting a field this account has never had
// a value for does not come back empty: it comes back broken. PostgreSQL fails
// the same way for a missing table, and for a field the sink never saw, but not
// for the eight `url` entries below, because the SQL sink declares the column
// in its DDL and leaves it null while the InfluxDB line protocol drops an empty
// string and never creates it.
//
// Which of these a real account would meet is worth separating:
//
//   - `url` is absent from those fixtures and never absent from GitHub, so the
//     eight are thin fixtures rather than a defect a user would see. Six more
//     stood here until the fixtures gained the url GitHub always sends.
//     gh_repo.url on panel 157 and gh_repo_total.url on panel 23 went first:
//     both reached InfluxDB as an empty string, which the line protocol drops,
//     so the column was never created, and the same emptiness was what made
//     gh_repo_community and gh_repo_policy write "/community" and "/settings"
//     into their Link columns. gh_pull_request.url on panel 58,
//     gh_external_contribution.url on panel 152 and gh_package_version.url on
//     panel 161 followed, because the emptiness cost something in
//     Elasticsearch too: a document is written without a key rather than with
//     an empty one, so the three panels selected a Link column and the
//     datasource returned no such field, and the table drew a row naming an
//     item with no way to open it. The GraphQL query asks every pull request
//     for `url` and the two REST walks read `html_url`, so all three were the
//     fixture answering without a field GitHub always sends. gh_fork.url on
//     panel 102 was the sixth, closed when the newest hundred
//     forks moved into the audience batch: its fixture answers `url`, and the
//     REST fixture gained `html_url` so the two roads could be held to the
//     same rows;
//   - gh_account_total.commits comes from the commit search, which this fake
//     GitHub does not answer;
//   - seconds_to_resolve exists only on a resolved alert, and every code
//     scanning alert here is open, so any account whose alerts are all open
//     gets the broken panel;
//   - the four missing tables (issue events, cache entries, account keys,
//     dependency licenses) are families this account has nothing in, which is
//     the ordinary case for most accounts;
//   - gh_contribution_day_repo is the fifth missing table and the one case
//     where the fixture rather than the account is thin. graphql_account.json
//     carries commitContributionsByRepository with a totalCount per repository
//     and no `contributions.nodes`, so the collector has no day to date a row
//     at and writes none, while the per-repository totals it does write reach
//     gh_contribution_repo and answer the panels above. Nothing but the two
//     panels here reads the daily split, so a real account has both and this
//     sweep has neither.
//   - gh_repo_created is the sixth, and the fixture is thin in the same way:
//     graphql_account.json carries totalRepositoryContributions as a number
//     and no `repositoryContributions.nodes`, so the collector has neither a
//     repository to name nor a date to stamp it at, and writes no row at all.
//     Any account that created a repository inside GitHub's trailing year has
//     them.
//   - gh_repo_archived is the seventh and is not the fixture at all. The
//     measurement is written from the repository list the sweep discovers, so
//     it needs `targets.include_archived`, and this sweep's config leaves it
//     off. The fixture does hold an archived repository, octocat/linguist,
//     archived a fortnight back, and with that setting off it is never asked
//     about. An account running the default filter is in exactly the same
//     position, which is why the panel's own description names the setting.
//   - gh_dependency and gh_dependency_change are the eighth and ninth, beside
//     gh_dependency_license above and for one reason shared by all three:
//     `deps` is the one family fakegh.NotCollected leaves out, because there is
//     no SBOM or dependency-diff fixture to switch it on for, so no sweep here
//     creates the tables. gh_dependency_change could not be written by this suite even
//     with one: the diff runs from the previous sweep's head, which a fresh
//     state file does not have, so a first sweep resolves a head and writes
//     nothing. Closing this gap is an SBOM fixture and a two-sweep fixture,
//     not a change to the panels, and any account that switches the family on
//     has both tables.
//
// The last two are real: on InfluxDB and PostgreSQL a panel about something you
// do not have is an error message, not an empty panel. Graphite and
// Elasticsearch answer the same panels with nothing at all, which is the
// difference the three assertions here are for.
var dashboardMissing = map[string]map[int]string{
	"influxdb": {
		7:   "gh_repo_created",
		8:   "gh_repo_archived",
		30:  "gh_contribution_day_repo",
		31:  "gh_contribution_day_repo",
		51:  "gh_workflow.url",
		55:  "gh_workflow.url",
		57:  "gh_workflow.url",
		68:  "gh_commit.url",
		69:  "gh_label.url",
		70:  "gh_milestone.url",
		75:  "gh_issue_event",
		86:  "gh_environment.url",
		94:  "gh_release.url",
		95:  "gh_release_asset.url",
		96:  "gh_release_asset.url",
		103: "gh_code_scanning_alert_item.seconds_to_resolve",
		115: "gh_actions_cache_entry",
		130: "gh_package.url",
		131: "gh_gist.url",
		134: "gh_key",
		135: "gh_dependency_license",
		140: "gh_dependency",
		141: "gh_dependency_change",
	},
	"postgres": {
		7:   "gh_repo_created",
		8:   "gh_repo_archived",
		30:  "gh_contribution_day_repo",
		31:  "gh_contribution_day_repo",
		75:  "gh_issue_event",
		103: "gh_code_scanning_alert_item.seconds_to_resolve",
		115: "gh_actions_cache_entry",
		134: "gh_key",
		135: "gh_dependency_license",
		140: "gh_dependency",
		141: "gh_dependency_change",
	},
}

// dashboardPluginBugs is the other kind: a panel the store holds the data for
// and the datasource plugin cannot render. These are defects a user meets, and
// the assertion is two-sided, so an entry that stops describing anything fails
// this test and has to be taken out deliberately.
//
// Empty, and what emptied it is worth keeping. Three Elasticsearch panels drew
// nothing but "An error occurred within the plugin", and they were two distinct
// faults in grafana-elasticsearch-datasource 12.8.2, both in how it reads a
// `top_metrics`:
//
//   - Community profile and Repository settings asked top_metrics for a
//     boolean. The sink writes a Go bool as a JSON boolean, dynamic mapping
//     makes it `boolean`, and Elasticsearch hands a boolean back out of a
//     top_metrics as the string "true". The plugin converts a top_metrics value
//     to a float without checking the type, so the whole panel came back as
//     `panic triggered: interface conversion: interface {} is string, not
//     float64`, in addTopMetricsToFields (metrics_response_processor.go:674).
//   - Deploy keys asked top_metrics for `days_since_use`, which is a number,
//     but one the collector writes only for a key GitHub has seen used. The
//     plugin appends nothing at all for a null, so the metric column came back
//     one row shorter than the bucket columns: `frame has different field
//     lengths, field 0 is len 2 but field 3 is len 1`.
//
// All three now take their reading with a `max`, which is answered as a number
// over a boolean and appends a null for a missing value. Community profile and
// Deploy keys say in their own descriptions that Elasticsearch reads the
// largest value inside the range where the other dashboards read the newest
// one; Repository settings says nothing because there is nothing to say, its
// SQL twin being a MAX() over the same range. Panel 155, "Social accounts",
// was the same family, `url` as a top_metrics, and was moved to a terms bucket
// earlier.
var dashboardPluginBugs = map[string]map[int]string{}

// TestDashboardPanelsAreAcceptedByTheirStore is the first question and the one
// no offline check can answer: does the datasource reject the query. A rejected
// query is a panel that renders an error and nothing else, and until this suite
// the only thing that would have noticed was somebody looking at their own
// Grafana.
func TestDashboardPanelsAreAcceptedByTheirStore(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)

	for _, store := range dashboardStores {
		t.Run(store.name, func(t *testing.T) {
			for _, index := range slices.Sorted(maps.Keys(run.outcomes[store.name])) {
				o := run.outcomes[store.name][index]
				dashboardCheckError(t, run, store.name, &o)
			}
			dashboardCheckStale(t, run, store.name, dashboardMissing[store.name], "refuses")
			dashboardCheckStale(t, run, store.name, dashboardPluginBugs[store.name], "cannot render")
		})
	}
}

func dashboardCheckError(t *testing.T, run *dashboardRun, store string, o *dashboardOutcome) {
	t.Helper()
	if o.err == "" {
		return // an entry that no longer fails is reported by dashboardCheckStale
	}
	if _, known := dashboardPluginBugs[store][o.panel.Index]; known {
		return
	}
	missing, listed := dashboardMissing[store][o.panel.Index]
	if !listed {
		t.Errorf("panel %d %q was rejected by %s: %s",
			o.panel.Index, o.panel.Title, store, grafana.Trim(o.err, 400))
		return
	}
	dashboardCheckMissing(t, run, store, o, missing)
}

// dashboardCheckMissing is what keeps the list above from being an excuse: the
// identifier it names has to be one the sweep really never wrote, and the store
// has to be complaining about that identifier and not about something else.
func dashboardCheckMissing(t *testing.T, run *dashboardRun, store string, o *dashboardOutcome, missing string) {
	t.Helper()
	measurement, field, hasField := strings.Cut(missing, ".")
	named := field
	if !hasField {
		named = measurement
	}
	switch {
	case !hasField:
		if run.written[measurement] != nil {
			t.Errorf("panel %d %q is excused on %s as %q having no table, but the sweep wrote %d "+
				"of them", o.panel.Index, o.panel.Title, store, measurement, run.inWindow[measurement])
		}
	case run.written[measurement] == nil:
		t.Errorf("panel %d %q is excused on %s as %q, but the sweep wrote no %s at all",
			o.panel.Index, o.panel.Title, store, missing, measurement)
	case run.written[measurement][field]:
		t.Errorf("panel %d %q is excused on %s as %q, but the sweep did write that field, so the "+
			"store refusing it is a defect and not a thin fixture: %s",
			o.panel.Index, o.panel.Title, store, missing, grafana.Trim(o.err, 200))
	}
	if !strings.Contains(o.err, named) {
		t.Errorf("panel %d %q is excused on %s as %q, but that is not what the store complained "+
			"about: %s", o.panel.Index, o.panel.Title, store, missing, grafana.Trim(o.err, 200))
	}
}

// dashboardCheckStale fails on an entry that no longer describes anything, so
// that a dashboard fixed elsewhere cannot leave a pinned defect behind.
func dashboardCheckStale(t *testing.T, run *dashboardRun, store string, list map[int]string, verb string) {
	t.Helper()
	for _, index := range slices.Sorted(maps.Keys(list)) {
		o, ok := run.outcomes[store][index]
		if !ok {
			t.Errorf("panel %d is listed as one %s %s, and that dashboard has no such panel",
				index, store, verb)
			continue
		}
		if o.err == "" {
			t.Errorf("panel %d %q is listed as one %s %s and it no longer does: take the entry out",
				index, o.panel.Title, store, verb)
		}
	}
}

// ── 2. Panels that should have rows do ──────────────────────────────────────

// dashboardKnownEmpty is every panel that answers nothing although the sweep
// wrote something inside the dashboard range for the measurement it reads. Each
// one is a claim about why, checked by hand against the sweep's own points and
// against the four other stores' answers to the same panel.
//
// The panels the window explains are not here and are not listed anywhere: the
// test derives them from the sweep, so a fixture that grows a star inside the
// last thirty days makes the star panels answer and nothing has to be edited.
//
// The list used to carry a fourth kind, and what settled it is worth keeping.
// "Actions minutes" and "Actions minutes over time" filter `unit = 'Minutes'`
// and answered nothing in InfluxDB, PostgreSQL and Graphite, which read like a
// defect in the panels. It was the fixture: GitHub answers unitType with a
// capital, and the live store proves it ("Minutes" 1489 rows, "GigabyteHours"
// 2215, "AICredits" 28), while billing_usage.json said "minutes". Elasticsearch
// answered all along and only by accident, because `unit` is mapped text and
// the standard analyzer lower cases both sides of the term. The fixture and
// internal/collect/billing_test.go now say what the API says, and the three
// entries went with them.
//
// Folding the stat tiles into groups would have hidden this rather than fixed
// it: "Actions minutes" is now one named value of "Spend in range", and a group
// answers as long as any of its values does, so the empty column would have
// drawn as a blank beside three numbers and no test would have spoken.
var dashboardKnownEmpty = map[string]map[int]string{
	"influxdb": {
		55:  "every workflow run in the fixture succeeded, and the panel wants the ones that fail",
		104: "the Dependabot alert inside the range is open, so it has no seconds_to_resolve",
		137: "one sweep is one snapshot, and the panel counts the values that changed between two",
		// The good case, and the one the panel is built to draw rather than
		// be refused for: the family rows of gh_collector_family are written
		// every sweep whether or not anything failed, so the measurement and
		// every column of it exist, and a sweep that lost no repository draws
		// an empty list instead of a planner error.
		153: "nothing failed in the sweep, and this panel lists the repositories a collector could not collect",
	},
	"postgres": {
		55:  "every workflow run in the fixture succeeded, and the panel wants the ones that fail",
		68:  "no commit in the fixture shares a sha with a failed run",
		96:  "one sweep is one snapshot, and the panel is the difference between two download counts",
		104: "the Dependabot alert inside the range is open, so it has no seconds_to_resolve",
		137: "one sweep is one snapshot, and the panel counts the values that changed between two",
		153: "nothing failed in the sweep, and this panel lists the repositories a collector could not collect",
	},
	"graphite": {
		96:  "one sweep is one snapshot, and the panel is the difference between two download counts",
		103: "every code scanning alert here is open, so no seconds_to_resolve was ever written",
		104: "the Dependabot alert inside the range is open, so it has no seconds_to_resolve",
		153: "nothing failed in the sweep, and this panel lists the repositories a collector could not collect",
	},
	// Prometheus lists only the panels that ask for something the exporter
	// publishes right now and still get nothing. Everything else this dashboard
	// leaves empty is derived by promNeedsHistory below, because the reason is
	// the same for all of them and it is a property of the harness.
	"prometheus": {
		153: "nothing failed in the sweep, and this panel lists the repositories a collector could not collect",
	},
	"elasticsearch": {
		103: "every code scanning alert here is open, so no seconds_to_resolve was ever written",
		104: "the Dependabot alert inside the range is open, so it has no seconds_to_resolve",
		153: "nothing failed in the sweep, and this panel lists the repositories a collector could not collect",
	},
}

// TestDashboardPanelsAnswerWhenThereIsSomethingToShow is the second question.
// The sweep is deterministic, so a panel over a measurement the sweep wrote
// inside the dashboard's range has to put something on the screen, and one that
// does not is either a broken query or a gap in the fixture. The difference is
// what this decides, out of the sweep's own points rather than out of a list.
func TestDashboardPanelsAnswerWhenThereIsSomethingToShow(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)

	for _, store := range dashboardStores {
		t.Run(store.name, func(t *testing.T) {
			if store.name == "prometheus" && run.promSkip != "" {
				t.Skipf("Prometheus holds nothing on this machine, so no panel of it can answer: %s",
					run.promSkip)
			}
			answered := 0
			for _, index := range slices.Sorted(maps.Keys(run.outcomes[store.name])) {
				o := run.outcomes[store.name][index]
				if dashboardCheckAnswer(t, run, store.name, &o) {
					answered++
				}
			}
			t.Logf("%d of %d panels answered", answered, len(run.outcomes[store.name]))
			dashboardCheckStaleEmpty(t, run, store.name)
		})
	}
}

// dashboardCheckAnswer reports whether the panel answered, and fails when it
// did not and the sweep says it should have.
func dashboardCheckAnswer(t *testing.T, run *dashboardRun, store string, o *dashboardOutcome) bool {
	t.Helper()
	if o.err != "" {
		return false // the first test's business
	}
	if o.answered() {
		return true
	}
	if _, listed := dashboardKnownEmpty[store][o.panel.Index]; listed {
		return false
	}
	if store == "prometheus" && promNeedsHistory(&o.panel) {
		return false
	}
	subject := run.subject[o.panel.Index]
	var have []string
	for _, m := range subject {
		if run.inWindow[m] > 0 {
			have = append(have, fmt.Sprintf("%s (%d points)", m, run.inWindow[m]))
		}
	}
	if len(have) == 0 {
		// Nothing to show, so nothing to report: the sweep wrote none of this
		// panel's measurements inside the range.
		return false
	}
	t.Errorf("panel %d %q of %s answered nothing, and the sweep wrote %s inside %s: "+
		"either the query is wrong or the fixture needs a reason listing in dashboardKnownEmpty",
		o.panel.Index, o.panel.Title, store, strings.Join(have, ", "), dashboardRange)
	return false
}

// promRangeFunction matches the PromQL functions that read a range vector.
// Anything built on one answers out of a series' history rather than out of its
// present value.
var promRangeFunction = regexp.MustCompile(`\b(?:increase|rate|irate|delta|idelta|deriv|predict_linear|holt_winters|changes|resets)\s*\(`)

// promNeedsHistory reports whether a Prometheus panel asks for something no run
// of this suite can produce, whatever the dashboard says.
//
// The exporter is not a store. It is served from inside the collector process,
// which this suite starts, lets Prometheus scrape a few times, and stops; when
// it stops Prometheus writes stale markers and the series end. So the whole
// history a panel can draw on is the half minute the sweep was running, against
// a dashboard range of thirty days. Two shapes of panel cannot answer out of
// that, and both are the panel being right rather than wrong:
//
//   - a timeseries, which Grafana evaluates at a step of range/maxDataPoints,
//     around forty minutes here. At most one of those evaluation points falls
//     inside the half minute of samples, and usually none does.
//   - anything over a range vector. One sweep writes each counter once, so the
//     counter is the same at every scrape and increase() over it is zero; the
//     gauges that divide two of them (Success rate, Signed commits, Webhook
//     failure rate) are then 0/0, which is NaN and draws nothing.
//
// This is derived rather than listed for the reason the rest of this file is:
// thirty-one identical sentences in dashboardKnownEmpty would read like
// thirty-one separate findings and would have to be re-checked whenever a panel
// moved. The cost is real and worth stating: a timeseries panel that is
// genuinely broken is excused here too, and only the instant panels, which are
// most of the dashboard, are held to the assertion. Proving the rest needs a
// Prometheus that scraped for thirty days, which is not a thing a test can have.
func promNeedsHistory(p *grafana.PanelQuery) bool {
	if p.Type == "timeseries" {
		return true
	}
	for _, t := range p.Targets {
		if expr, _ := t["expr"].(string); promRangeFunction.MatchString(expr) {
			return true
		}
	}
	return false
}

func dashboardCheckStaleEmpty(t *testing.T, run *dashboardRun, store string) {
	t.Helper()
	for _, index := range slices.Sorted(maps.Keys(dashboardKnownEmpty[store])) {
		o, ok := run.outcomes[store][index]
		if !ok {
			t.Errorf("panel %d is listed as one the %s dashboard leaves empty, and it has no such panel",
				index, store)
			continue
		}
		if o.err == "" && o.answered() {
			t.Errorf("panel %d %q is listed as one the %s dashboard leaves empty and it now answers: "+
				"take the entry out", index, o.panel.Title, store)
		}
	}
}

// ── 3. The dashboards agree ─────────────────────────────────────────────────

// dashboardDisagrees is every panel where two stores answer the same question
// with different numbers although the panel says nothing about differing. It is
// meant to stay this short.
//
// The one entry is honest rather than a rounding tolerance: InfluxDB's SQL has
// no exact median. The panel is written as approx_percentile_cont(x, 0.5),
// which is a t-digest estimate and answers 52 for the two jobs here, while
// PostgreSQL's percentile_cont and Elasticsearch's percentiles both answer the
// exact 47.5. Nothing in the panel's description says so, and a reader
// comparing the two dashboards would be right to think one of them is wrong.
//
// Keyed by the name a reader sees rather than by ordinal: a panel's title, or
// a value's own name where the reading is one value of a grouped stat, which
// is what "Queue wait" now is. An ordinal moves every time a panel is added
// above it, and the one entry here had drifted from the stat it describes to
// a timeseries three sections away without anything noticing.
var dashboardDisagrees = map[string]string{
	"Queue wait": "InfluxDB has only an approximate median (approx_percentile_cont, a t-digest); " +
		"PostgreSQL and Elasticsearch answer the exact one",
}

// TestTheDashboardsAgreeOnTheSameNumber is the third question. Two stores
// asked the same panel should not answer two different numbers, and where they
// honestly differ the specification already says so in the panel's own
// description: the Prometheus sentences about scrape time, the Graphite ones
// about storage slots, the Elasticsearch one about the range. So the
// description is the allowance, and a panel whose description is identical in
// two dashboards is a panel claiming the same fact in both.
//
// Only panels that reduce to a single number are compared. A table or a
// timeseries is not comparable across these stores even when both are right:
// each returns its own columns and its own bucketing, so summing them measures
// the shape of the answer rather than the fact.
func TestTheDashboardsAgreeOnTheSameNumber(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)

	compared, agreed := 0, 0
	for _, index := range slices.Sorted(maps.Keys(run.subject)) {
		answers := dashboardComparable(run, index)
		if len(answers) < 2 {
			continue
		}
		compared++
		title := dashboardTitle(run, index)
		if dashboardSameNumber(answers) {
			agreed++
			if _, listed := dashboardDisagrees[title]; listed {
				t.Errorf("panel %d %q is listed as one the stores disagree on and they now agree (%v): "+
					"take the entry out", index, title, answers)
			}
			continue
		}
		if _, listed := dashboardDisagrees[title]; listed {
			continue
		}
		t.Errorf("panel %d %q answers differently in stores whose description of it is identical, "+
			"so nothing warns a reader that it would: %v",
			index, title, answers)
	}
	// A grouped stat draws several numbers, so the loop above skips it whole.
	// Its values are still one number each and still comparable, by the name
	// the group gives them, and this is where the tiles that were folded into
	// groups are asked the same question they were asked as tiles.
	for _, index := range slices.Sorted(maps.Keys(run.subject)) {
		c, a := dashboardCompareValues(t, run, index)
		compared += c
		agreed += a
	}
	t.Logf("%d readings reduce to one number in two or more stores and describe it identically; "+
		"%d of them agree", compared, agreed)
	// The floor is a floor, not a measurement: it is here so that a change
	// that quietly stops the comparison reducing anything fails instead of
	// passing with nothing asked. Both shapes count towards it, a panel that
	// is one number and a named value of a group, which is what kept it
	// standing when twenty-two tiles became seven groups.
	if compared < 40 {
		t.Errorf("only %d readings were comparable, which is too few for this to have asked "+
			"anything: the sweep or the descriptions have changed shape", compared)
	}
}

// dashboardCompareValues asks the question of every named value of one panel
// and reports how many were comparable and how many agreed.
func dashboardCompareValues(t *testing.T, run *dashboardRun, index int) (compared, agreed int) {
	t.Helper()
	values := dashboardComparableValues(run, index)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		answers := values[name]
		if len(answers) < 2 {
			continue
		}
		compared++
		_, listed := dashboardDisagrees[name]
		switch {
		case dashboardSameNumber(answers):
			agreed++
			if listed {
				t.Errorf("value %q of panel %d %q is listed as one the stores disagree on and "+
					"they now agree (%v): take the entry out",
					name, index, dashboardTitle(run, index), answers)
			}
		case !listed:
			t.Errorf("value %q of panel %d %q answers differently in stores whose description of "+
				"it is identical, so nothing warns a reader that it would: %v",
				name, index, dashboardTitle(run, index), answers)
		}
	}
	return compared, agreed
}

// dashboardComparableValues is dashboardComparable for a panel that draws more
// than one number: every value InfluxDB names, against the stores that name it
// too and describe the panel identically.
func dashboardComparableValues(run *dashboardRun, index int) map[string]map[string]float64 {
	reference, ok := run.outcomes["influxdb"][index]
	if !ok || reference.err != "" || len(reference.numbers) < 2 {
		return nil
	}
	out := map[string]map[string]float64{}
	for name, v := range reference.named {
		if math.IsNaN(v) {
			continue
		}
		answers := map[string]float64{"influxdb": v}
		for _, store := range dashboardStores {
			if store.name == "influxdb" {
				continue
			}
			o, found := run.outcomes[store.name][index]
			if !found || o.err != "" || o.panel.Description != reference.panel.Description {
				continue
			}
			if n, has := o.named[name]; has && !math.IsNaN(n) {
				answers[store.name] = n
			}
		}
		out[name] = answers
	}
	return out
}

// dashboardComparable is the stores that answered this panel with exactly one
// number and describe it in the same words as the InfluxDB dashboard does.
func dashboardComparable(run *dashboardRun, index int) map[string]float64 {
	reference, found := dashboardOneNumber(run, "influxdb", index)
	if !found {
		return nil
	}
	out := map[string]float64{}
	for _, store := range dashboardStores {
		o, ok := dashboardOneNumber(run, store.name, index)
		if !ok || o.panel.Description != reference.panel.Description {
			continue
		}
		out[store.name] = o.numbers[0]
	}
	return out
}

// dashboardOneNumber is one store's outcome for the panel, reported only when
// the panel ran without an error and reduced to exactly one number.
func dashboardOneNumber(run *dashboardRun, store string, index int) (dashboardOutcome, bool) {
	o, ok := run.outcomes[store][index]
	return o, ok && o.err == "" && len(o.numbers) == 1
}

// dashboardSameNumber compares to a millionth of the value rather than exactly,
// because Elasticsearch keeps a float as a float32 and hands it back widened:
// the same 11.712 that the other three answer arrives from it as
// 11.712000012397766. That is the format of the store, not a different answer,
// and the one disagreement this suite does report is off by ten per cent.
func dashboardSameNumber(answers map[string]float64) bool {
	var first float64
	seen := false
	for _, v := range answers {
		if !seen {
			first, seen = v, true
			continue
		}
		if math.Abs(v-first) > 1e-6*max(math.Abs(first), 1) {
			return false
		}
	}
	return true
}

func dashboardTitle(run *dashboardRun, index int) string {
	return run.outcomes["influxdb"][index].panel.Title
}
