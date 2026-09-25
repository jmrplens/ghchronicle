//go:build dockere2e

package docker

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a real Graphite does with what the sink writes, and whether the paths
// it holds are the paths the dashboards address.
//
// The metric path is this sink's whole contract:
//
//	<prefix>.<measurement without gh_>.<tag values, in tag key order>.<field>
//
// Every Graphite panel computes a groupByNode or aliasByNode index from that
// shape, and it computes it from one hand-maintained table,
// internal/dashboards/tags.go, which lists the tags each collector sets.
// Nothing has ever checked that table against a collector. A tag added,
// removed or made conditional moves every node after it by one, the panel
// then groups by the wrong node, and nothing says so: the query is valid, the
// series exist, the labels are simply wrong. These tests are what replaces
// that table's maintenance with an assertion.

// graphitePrefix is the sink's default, which the sweep does not override.
const graphitePrefix = "github"

// graphiteUntabled is the one measurement tags.go deliberately omits, with
// the reason it gives: gh_job_log has no panel and therefore no node index to
// get wrong. gh_event used to be the other, addressed from the end of its path
// because `action` and `ref_type` were written only on the event types that
// carry them; the collector writes both on every row now, so it is in the
// table like the rest.
var graphiteUntabled = []string{"gh_job_log"}

func TestGraphiteTagsMatchTheTableTheDashboardsIndexFrom(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	table := graphiteTable(t)
	points := pushPointsByMeasurement(t, sweep)

	t.Run("every measurement the sweep produced is in the table", func(t *testing.T) {
		for measurement := range points {
			if slices.Contains(graphiteUntabled, measurement) {
				continue
			}
			if _, ok := table[measurement]; !ok {
				t.Errorf("%s has no entry in internal/dashboards/tags.go, so no panel can address it", measurement)
			}
		}
	})

	t.Run("the table lists exactly the tags the collector writes", func(t *testing.T) {
		// The table's own rule: only a tag written on every point of a
		// measurement belongs in it, because a conditional tag moves the
		// nodes after it on some points and not others. So the comparison is
		// against the tags every point carries and against the tags any point
		// carries, and both have to be the same list.
		for measurement, list := range points {
			want, ok := table[measurement]
			if !ok {
				continue // reported by the subtest above
			}
			always, ever := graphiteTagKeys(list)
			if !slices.Equal(always, want) || !slices.Equal(ever, want) {
				t.Errorf("%s: the table says %v, the collector writes %v on every point and %v on some",
					measurement, want, always, ever)
			}
		}
	})

	t.Run("the untabled measurements are still untabled", func(t *testing.T) {
		// An entry appearing here means someone gave it a fixed tag list,
		// which is a change of contract: a panel that wanted it would have
		// to be written against the table rather than from the end of the
		// path.
		for _, measurement := range graphiteUntabled {
			if _, ok := table[measurement]; ok {
				t.Errorf("%s is in the table now; the panels that address it from the end of the path have to change with it", measurement)
			}
		}
	})
}

func TestGraphiteStoresEveryMeasurementAtTheDepthTheTableDeclares(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	table := graphiteTable(t)

	// Carbon answers the write immediately and creates the whisper file when
	// it next flushes its cache, so the store is polled rather than asked
	// once.
	stored := graphiteAwaitMeasurements(ctx, t, s, sweep)

	t.Run("the sweep reached the store", func(t *testing.T) {
		// Every measurement carrying a number should have arrived. A
		// measurement whose only fields are strings writes no line at all:
		// a string is not a metric.
		for measurement, list := range pushPointsByMeasurement(t, sweep) {
			if !graphiteHasNumber(list) || slices.Contains(stored, graphiteName(measurement)) {
				continue
			}
			t.Errorf("%s carries numbers and yet no path of it reached carbon", measurement)
		}
	})

	for _, name := range stored {
		measurement := "gh_" + name
		if slices.Contains(graphiteUntabled, measurement) {
			continue
		}
		tags, ok := table[measurement]
		if !ok {
			continue // reported by the test above
		}
		t.Run(measurement, func(t *testing.T) {
			graphiteWantDepth(ctx, t, s, name, len(tags))
		})
	}
}

func TestGraphitePutsEachTagValueOnTheNodeTheTableSays(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	table := graphiteTable(t)
	graphiteAwaitMeasurements(ctx, t, s, sweep)

	// The depth test proves a measurement is as deep as the table says. This
	// one proves the values are in the order the table gives them, which is
	// what makes groupByNode(path, gn(m, "repo")) group by the repository
	// rather than by the license: the whole path is computed from the table
	// and the oracle, and the store is asked for that exact path.
	for measurement, list := range pushPointsByMeasurement(t, sweep) {
		tags, ok := table[measurement]
		if !ok {
			continue
		}
		path, found := graphitePathOf(list, tags)
		if !found {
			continue // no point of it carries a number, so it writes no path
		}
		t.Run(measurement, func(t *testing.T) {
			nodes := graphiteFind(ctx, t, s, path)
			if len(nodes) != 1 || !nodes[0].IsLeaf {
				t.Fatalf("the store holds no leaf at %s, which is where the table puts this point: %v", path, nodes)
			}
		})
	}
}

func TestGraphiteKeepsTheDateOfTheEvent(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	graphiteAwaitMeasurements(ctx, t, s, sweep)

	// Graphite does not keep a timestamp, it keeps a slot: whisper rounds
	// every point down to the start of the interval of whichever archive
	// covers it, and storage-schemas.conf gives these metrics an hour of
	// resolution for a month and then a day for twelve years. So the
	// assertion is that the point landed in the slot its own date belongs to,
	// which is what a dashboard reads, and never in today's.
	t.Run("a dated event keeps the day it happened", func(t *testing.T) {
		at := graphiteWhen(t, starGivenAt)
		graphiteWantPoint(ctx, t, s, "github.star.octocat_hello-world.octocat.hello-world.alice.starred", at, 1)
	})

	t.Run("a daily snapshot keeps its own day", func(t *testing.T) {
		at := graphiteWhen(t, trafficDayAt)
		graphiteWantPoint(ctx, t, s, "github.traffic.octocat_hello-world.views.octocat.hello-world.count", at, 120)
	})

	t.Run("a run keeps the moment it finished", func(t *testing.T) {
		// Two nodes of that path read `_none_`, and both are the fixture
		// rather than a defect: the run fixture names no actor, and its
		// workflow id matches nothing in workflows.json. The collectors write
		// `(none)` where GitHub gives them nothing, and the sink keeps only
		// letters, digits, "_", "-" and ":" in a node, so the parentheses
		// arrive as underscores. They are written all the same, because a tag
		// left out would move every node after it.
		at := graphiteWhen(t, runFinishedAt)
		graphiteWantPoint(ctx, t, s,
			"github.workflow_run._none_.success.push.octocat_hello-world.octocat.hello-world._none_.duration_seconds",
			at, 220)
	})

	t.Run("a day of the star history keeps its own day", func(t *testing.T) {
		// One series per repository rather than one per stargazer, which is
		// what lets the star panels sum a path instead of counting a
		// wildcard of people, and each day in the slot of its own date with
		// the day's count as the value. The path is built from the table the
		// panels index by, so a repository node in the wrong place fails
		// here as well as in the depth tests above.
		table := graphiteTable(t)
		for _, want := range starDays(t, pushPoints(t, sweep)) {
			path, ok := graphitePathOf([]sqlStoresPoint{want}, table["gh_star_day"])
			if !ok {
				t.Fatalf("a day of the star history carries no number: %v", want)
			}
			graphiteWantPoint(ctx, t, s, path, graphiteWhen(t, want.Time), starCount(want))
		}
	})

	// The other half, and the half a positive assertion cannot make. Asking
	// only whether the point is in its own slot passes just as well when the
	// sink also wrote it at the moment of the sweep, and it passes outright
	// when a previous run of this suite left the right slot filled: whisper
	// keeps what earlier sweeps put there, and `make e2e-docker-up` plus a
	// second `go test` is a workflow this repository documents. So the dated
	// paths are also asked what they hold from the moment the sweep began,
	// which for an event that happened years ago has to be nothing.
	//
	// The star history is not asked. Its newest day is today at 00:00 UTC,
	// which is the hourly slot the sweep itself falls in whenever it runs in
	// the first hour of a UTC day, so the question would fail on the clock
	// rather than on the sink; the SQL and Elasticsearch suites, which keep
	// the instant, ask it instead.
	t.Run("nothing dated was restamped with the sweep's clock", func(t *testing.T) {
		for _, path := range []string{
			"github.star.octocat_hello-world.octocat.hello-world.alice.starred",
			"github.traffic.octocat_hello-world.views.octocat.hello-world.count",
			"github.workflow_run._none_.success.push.octocat_hello-world.octocat.hello-world._none_.duration_seconds",
		} {
			graphiteWantNothingSince(ctx, t, s, path, sweep.Started)
		}
	})
}

// graphiteWantNothingSince fails when a path holds any value from at onwards.
//
// The window opens at the start of the sweep rather than at the current slot
// because whisper rounds a point down to the start of its archive interval,
// which is an hour here: a point written now sits at the top of this hour,
// which is earlier than now.
func graphiteWantNothingSince(ctx context.Context, t *testing.T, s *Stack, path string, at time.Time) {
	t.Helper()
	points, err := graphiteRender(ctx, s, path, at.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		// A path the render API does not know holds nothing, which is what
		// this asks. Anything else is a failure to answer the question.
		if strings.Contains(err.Error(), "no such path") {
			return
		}
		t.Fatalf("%s: %v", path, err)
		return
	}
	var found []string
	for _, dp := range points {
		if dp.value == nil || dp.at.Before(at.Truncate(time.Hour)) {
			continue
		}
		found = append(found, fmt.Sprintf("%v at %s", *dp.value, dp.at.Format(time.RFC3339)))
	}
	if len(found) > 0 {
		t.Errorf("%s holds %v, and every point of it belongs before the sweep began at %s",
			path, found, at.Format(time.RFC3339))
	}
}

// ── The table the dashboards compute their node indices from ────────────────

// graphiteTable reads `var tags` out of internal/dashboards/tags.go.
//
// It is parsed rather than imported because the table is unexported and has
// no reader outside its own package, and exporting it to be read once here
// would widen that package's surface for a test; copied rather than parsed
// would be a second hand-maintained table checking the first.
func graphiteTable(t *testing.T) map[string][]string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "internal", "dashboards", "tags.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("the node table: %v", err)
	}
	for _, decl := range file.Decls {
		table, ok := graphiteTableOf(decl)
		if ok {
			return table
		}
	}
	t.Fatalf("%s declares no `tags` map", path)
	return nil
}

// graphiteTableOf returns the `tags` map of one declaration, if it is that
// declaration.
func graphiteTableOf(decl ast.Decl) (map[string][]string, bool) {
	gen, ok := decl.(*ast.GenDecl)
	if !ok || gen.Tok != token.VAR {
		return nil, false
	}
	for _, spec := range gen.Specs {
		if literal := graphiteTagsLiteral(spec); literal != nil {
			return graphiteTableEntries(literal), true
		}
	}
	return nil, false
}

// graphiteTagsLiteral is the composite literal a `tags = ...` spec assigns,
// or nil when the spec declares anything else.
func graphiteTagsLiteral(spec ast.Spec) *ast.CompositeLit {
	value, ok := spec.(*ast.ValueSpec)
	if !ok || len(value.Names) != 1 || value.Names[0].Name != "tags" || len(value.Values) != 1 {
		return nil
	}
	literal, ok := value.Values[0].(*ast.CompositeLit)
	if !ok {
		return nil
	}
	return literal
}

func graphiteTableEntries(literal *ast.CompositeLit) map[string][]string {
	out := map[string][]string{}
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := graphiteString(pair.Key)
		if !ok {
			continue
		}
		list, ok := pair.Value.(*ast.CompositeLit)
		if !ok {
			continue
		}
		out[key] = graphiteStrings(list)
	}
	return out
}

// graphiteStrings is the string literals of one list, in order.
func graphiteStrings(list *ast.CompositeLit) []string {
	var out []string
	for _, item := range list.Elts {
		if s, ok := graphiteString(item); ok {
			out = append(out, s)
		}
	}
	return out
}

func graphiteString(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// ── What the oracle says the paths should be ────────────────────────────────

// graphiteName is the measurement's node: the sink strips the gh_ prefix.
func graphiteName(measurement string) string {
	return graphiteNodeOf(strings.TrimPrefix(measurement, "gh_"))
}

// graphiteNodeOf is the sink's own node sanitizer: a node keeps ASCII letters,
// digits, "_", "-" and ":", an empty value becomes "none", and everything else
// becomes "_", the dot and the slash in "owner/repo" included, because a dot
// would split the node and a slash would nest a directory.
func graphiteNodeOf(s string) string {
	if s == "" {
		return "none"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_', r == '-', r == ':':
			return r
		}
		return '_'
	}, s)
}

// graphiteTagKeys is the tags a measurement's points carry: the ones on every
// point, and the ones on any point.
func graphiteTagKeys(points []sqlStoresPoint) (always, ever []string) {
	counts := map[string]int{}
	for _, p := range points {
		for k := range p.Tags {
			counts[k]++
		}
	}
	for k, n := range counts {
		ever = append(ever, k)
		if n == len(points) {
			always = append(always, k)
		}
	}
	sort.Strings(always)
	sort.Strings(ever)
	return always, ever
}

// graphiteHasNumber reports whether any point of a measurement carries a field
// the sink would write.
func graphiteHasNumber(points []sqlStoresPoint) bool {
	for _, p := range points {
		if _, ok := graphiteNumberOf(p); ok {
			return true
		}
	}
	return false
}

// graphiteNumberOf is the name of the first numeric field of a point, in the
// order the sink writes them: a string is not a metric, and a field whose name
// is also a tag's is dropped, as in the line protocol.
func graphiteNumberOf(p sqlStoresPoint) (field string, ok bool) {
	fields := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	for _, k := range fields {
		if _, clash := p.Tags[k]; clash {
			continue
		}
		switch p.Fields[k].(type) {
		case float64, bool:
			return k, true
		}
	}
	return "", false
}

// graphitePathOf builds the path the table says one point of a measurement
// occupies. The tag values are taken in the table's order, which is the whole
// point: the sink writes them in tag key order, and the table is what the
// dashboards believe that order to be.
func graphitePathOf(points []sqlStoresPoint, tags []string) (string, bool) {
	for _, p := range points {
		field, ok := graphiteNumberOf(p)
		if !ok {
			continue
		}
		parts := make([]string, 0, len(tags)+3)
		parts = append(parts, graphitePrefix, graphiteName(p.Measurement))
		for _, tag := range tags {
			parts = append(parts, graphiteNodeOf(p.Tags[tag]))
		}
		return strings.Join(append(parts, graphiteNodeOf(field)), "."), true
	}
	return "", false
}

// ── Reading carbon back ─────────────────────────────────────────────────────

// graphiteNode is one entry of the metrics/find API.
type graphiteNodeEntry struct {
	Path   string `json:"path"`
	IsLeaf bool   `json:"is_leaf"`
}

// graphiteFind asks the render API's metric browser for one pattern.
func graphiteFind(ctx context.Context, t *testing.T, s *Stack, query string) []graphiteNodeEntry {
	t.Helper()
	nodes, err := graphiteFindErr(ctx, s, query)
	if err != nil {
		t.Fatalf("finding %s: %v", query, err)
	}
	return nodes
}

func graphiteFindErr(ctx context.Context, s *Stack, query string) ([]graphiteNodeEntry, error) {
	var nodes []graphiteNodeEntry
	where := s.GraphiteURL + "/metrics/find?format=json&query=" + url.QueryEscape(query)
	if err := storeJSON(ctx, http.MethodGet, where, nil, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// graphiteAwaitMeasurements waits until carbon has flushed a whisper file for
// every measurement the sweep sent it, and returns the measurement nodes the
// store holds.
func graphiteAwaitMeasurements(ctx context.Context, t *testing.T, s *Stack, sweep *pushSweep) []string {
	t.Helper()
	want := map[string]bool{}
	for measurement, points := range pushPointsByMeasurement(t, sweep) {
		if graphiteHasNumber(points) {
			want[graphiteName(measurement)] = true
		}
	}
	var stored []string
	err := WaitUntil(ctx, "carbon's whisper files", 2*time.Minute, func(ctx context.Context) error {
		nodes, err := graphiteFindErr(ctx, s, graphitePrefix+".*")
		if err != nil {
			return err
		}
		stored = stored[:0]
		missing := 0
		have := map[string]bool{}
		for _, n := range nodes {
			name := n.Path[strings.LastIndexByte(n.Path, '.')+1:]
			stored = append(stored, name)
			have[name] = true
		}
		for name := range want {
			if !have[name] {
				missing++
			}
		}
		if missing > 0 {
			return fmt.Errorf("%d of %d measurements have not been flushed yet", missing, len(want))
		}
		return nil
	})
	if err != nil {
		// Not fatal: which measurements are missing is the test's own
		// finding, and it says which ones. Carbon drops a point older than
		// its longest archive in silence, so this is where that shows up.
		t.Errorf("carbon did not store the whole sweep: %v", err)
	}
	sort.Strings(stored)
	return stored
}

// graphiteWantDepth asserts that every path of one measurement is exactly as
// deep as the table says, in both directions: something at that depth, all of
// it leaves, and nothing one node deeper.
func graphiteWantDepth(ctx context.Context, t *testing.T, s *Stack, name string, tags int) {
	t.Helper()
	at := graphitePrefix + "." + name + strings.Repeat(".*", tags+1)
	nodes := graphiteFind(ctx, t, s, at)
	if len(nodes) == 0 {
		t.Fatalf("nothing at %s, so the table's %d tags are not the depth the sink writes", at, tags)
	}
	for _, n := range nodes {
		if !n.IsLeaf {
			t.Errorf("%s is a branch, so this measurement is deeper than the table's %d tags", n.Path, tags)
		}
	}
	if deeper := graphiteFind(ctx, t, s, at+".*"); len(deeper) != 0 {
		t.Errorf("%s holds %d nodes below the depth the table declares, the first %s",
			name, len(deeper), deeper[0].Path)
	}
}

// graphiteWhen is the slot a date lands in: whisper rounds a point down to the
// start of an interval of the archive that covers it, and the retentions are
// an hour for the first thirty days and a day beyond that.
func graphiteWhen(t *testing.T, stamp string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("the fixture's date %q: %v", stamp, err)
	}
	interval := time.Hour
	if time.Since(at) > 30*24*time.Hour {
		interval = 24 * time.Hour
	}
	return at.Truncate(interval)
}

// graphiteWantPoint renders one path around the moment it should hold a value
// and fails unless that slot holds it.
func graphiteWantPoint(ctx context.Context, t *testing.T, s *Stack, path string, at time.Time, want float64) {
	t.Helper()
	series, err := graphiteRender(ctx, s, path, at.Add(-48*time.Hour), at.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("rendering %s: %v", path, err)
	}
	var dated []string
	for _, dp := range series {
		if dp.value == nil {
			continue
		}
		if dp.at.Equal(at) {
			if *dp.value != want {
				t.Errorf("%s at %s is %v, want %v", path, at.Format(time.RFC3339), *dp.value, want)
			}
			return
		}
		dated = append(dated, dp.at.Format(time.RFC3339))
	}
	t.Errorf("%s holds nothing at %s, the slot its own date belongs to; it holds values at %v",
		path, at.Format(time.RFC3339), dated)
}

// graphitePoint is one datapoint of the render API: a value that may be null,
// and the start of the slot it sits in.
type graphitePoint struct {
	at    time.Time
	value *float64
}

func graphiteRender(ctx context.Context, s *Stack, path string, from, until time.Time) ([]graphitePoint, error) {
	where := s.GraphiteURL + "/render?format=json" +
		"&target=" + url.QueryEscape(path) +
		"&from=" + strconv.FormatInt(from.Unix(), 10) +
		"&until=" + strconv.FormatInt(until.Unix(), 10)
	var series []struct {
		Target     string  `json:"target"`
		Datapoints [][]any `json:"datapoints"`
	}
	if err := storeJSON(ctx, http.MethodGet, where, nil, &series); err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, errors.New("the render API knows no such path")
	}
	out := make([]graphitePoint, 0, len(series[0].Datapoints))
	for _, dp := range series[0].Datapoints {
		if len(dp) != 2 {
			continue
		}
		stamp, ok := dp[1].(float64)
		if !ok {
			continue
		}
		out = append(out, graphitePoint{at: time.Unix(int64(stamp), 0).UTC(), value: graphiteValue(dp[0])})
	}
	return out, nil
}

// graphiteValue is the value half of a datapoint, nil where the render API
// answered null for a slot nothing was written to.
func graphiteValue(raw any) *float64 {
	v, ok := raw.(float64)
	if !ok {
		return nil
	}
	return &v
}
