package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// testRepo is the one repository every fixture describes.
var testRepo = Repo{Owner: "octocat", Name: "hello-world", FullName: "octocat/hello-world"}

// testNow is a Tuesday, so the week anchoring tests have a Sunday to land on
// that is not the same day.
var testNow = time.Date(2026, 9, 8, 15, 4, 5, 0, time.UTC)

// request is what the fixture server remembers about one call.
type request struct {
	Method string
	Path   string
	Query  map[string]string
	Header http.Header
	Body   []byte
}

// fixtureServer stands in for api.github.com. Routes are matched on the
// request path; the handler sees the request so a test can switch on the
// query string, which is how pagination is served.
type fixtureServer struct {
	t      *testing.T
	srv    *httptest.Server
	Client *ghapi.Client

	mu       sync.Mutex
	routes   map[string]http.HandlerFunc
	graphql  func(w http.ResponseWriter, r *http.Request, query string, vars map[string]any)
	requests []request
}

func newFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	f := &fixtureServer{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.Client = ghapi.New("test-token", 0)
	f.Client.SetBaseURL(f.srv.URL)
	return f
}

func (f *fixtureServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := map[string]string{}
	for k, v := range r.URL.Query() {
		q[k] = v[0]
	}
	f.mu.Lock()
	f.requests = append(f.requests, request{
		Method: r.Method, Path: r.URL.Path, Query: q, Header: r.Header.Clone(), Body: body,
	})
	h := f.routes[r.URL.Path]
	gql := f.graphql
	f.mu.Unlock()

	// Every answer carries a rate budget and a JSON content type, as GitHub's
	// do. A handler that wants to imitate the gateway's HTML 502 overrides it.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("x-ratelimit-limit", "5000")
	w.Header().Set("x-ratelimit-remaining", "4999")
	w.Header().Set("x-ratelimit-resource", "core")

	if r.URL.Path == "/graphql" && gql != nil {
		var env struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("x-ratelimit-resource", "graphql")
		gql(w, r, env.Query, env.Variables)
		return
	}
	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`)
		return
	}
	h(w, r)
}

// file serves one fixture on a path, whatever the query string.
func (f *fixtureServer) file(path, name string) {
	f.handle(path, func(w http.ResponseWriter, _ *http.Request) { f.write(w, name) })
}

// handle installs an arbitrary handler on a path.
func (f *fixtureServer) handle(path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = h
}

// status answers a path with a status and a GitHub-shaped message body.
func (f *fixtureServer) status(path string, code int, message string) {
	f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
	})
}

// graphQL routes every POST /graphql through fn, which picks a fixture from
// the query text and can record the variables.
func (f *fixtureServer) graphQL(fn func(w http.ResponseWriter, r *http.Request, query string, vars map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graphql = fn
}

func (f *fixtureServer) write(w http.ResponseWriter, name string) {
	b := fixture(f.t, name)
	if strings.HasSuffix(name, ".json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	_, _ = w.Write(b)
}

// calls returns every request made so far to a path.
func (f *fixtureServer) calls(path string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// repeat clones the first element of a JSON list fixture n times, giving
// each a distinct id, so a page of a hundred can be served without a
// hundred-entry fixture file. key names the list when the fixture is an
// object rather than a bare array.
func repeat(t *testing.T, name, key string, n int, mutate func(i int, row map[string]any)) []byte {
	t.Helper()
	raw := fixture(t, name)
	var rows []map[string]any
	var envelope map[string]any
	if key == "" {
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		list, _ := envelope[key].([]any)
		for _, it := range list {
			rows = append(rows, it.(map[string]any))
		}
	}
	if len(rows) == 0 {
		t.Fatalf("fixture %s has no rows to repeat", name)
	}
	out := make([]map[string]any, 0, n)
	for i := range n {
		row := map[string]any{}
		maps.Copy(row, rows[0])
		if mutate != nil {
			mutate(i, row)
		}
		out = append(out, row)
	}
	var b []byte
	var err error
	if key == "" {
		b, err = json.Marshal(out)
	} else {
		envelope[key] = out
		b, err = json.Marshal(envelope)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkPoints enforces what every collector promises: a measurement name,
// at least one field, no empty tag value, and a timestamp. An empty tag
// value is dropped by the sink, so a collector that emits one has silently
// lost a dimension; a point with no field is not a point at all.
func checkPoints(t *testing.T, points []sink.Point) {
	t.Helper()
	for i, p := range points {
		if p.Measurement == "" {
			t.Errorf("point %d has no measurement: %+v", i, p)
		}
		if !strings.HasPrefix(p.Measurement, "gh_") {
			t.Errorf("point %d: measurement %q is not namespaced", i, p.Measurement)
		}
		if len(p.Fields) == 0 {
			t.Errorf("point %d (%s) has no field", i, p.Measurement)
		}
		for k, v := range p.Tags {
			if k == "" {
				t.Errorf("point %d (%s) has an empty tag key", i, p.Measurement)
			}
			if v == "" {
				t.Errorf("point %d (%s) has an empty value for tag %q", i, p.Measurement, k)
			}
		}
		for k := range p.Fields {
			if _, clash := p.Tags[k]; clash {
				t.Errorf("point %d (%s): %q is both a tag and a field, InfluxDB rejects that", i, p.Measurement, k)
			}
		}
		if p.Time.IsZero() {
			t.Errorf("point %d (%s) has a zero timestamp", i, p.Measurement)
		}
		if sink.LineProtocol(p) == "" {
			t.Errorf("point %d (%s) renders to an empty line", i, p.Measurement)
		}
	}
}

// byMeasurement groups points by name.
func byMeasurement(points []sink.Point) map[string][]sink.Point {
	out := map[string][]sink.Point{}
	for _, p := range points {
		out[p.Measurement] = append(out[p.Measurement], p)
	}
	return out
}

// only returns the points of one measurement, failing when there are none.
func only(t *testing.T, points []sink.Point, measurement string) []sink.Point {
	t.Helper()
	got := byMeasurement(points)[measurement]
	if len(got) == 0 {
		t.Fatalf("no %s points in %v", measurement, measurements(points))
	}
	return got
}

// find returns the first point whose tags include every pair in want.
func find(t *testing.T, points []sink.Point, measurement string, want map[string]string) sink.Point {
	t.Helper()
	for _, p := range points {
		if p.Measurement != measurement {
			continue
		}
		match := true
		for k, v := range want {
			if p.Tags[k] != v {
				match = false
				break
			}
		}
		if match {
			return p
		}
	}
	t.Fatalf("no %s point with tags %v among %d points", measurement, want, len(points))
	return sink.Point{}
}

func measurements(points []sink.Point) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range points {
		if !seen[p.Measurement] {
			seen[p.Measurement] = true
			out = append(out, p.Measurement)
		}
	}
	return out
}

// wantMeasurements fails unless every named measurement is present.
func wantMeasurements(t *testing.T, points []sink.Point, names ...string) {
	t.Helper()
	got := byMeasurement(points)
	for _, n := range names {
		if len(got[n]) == 0 {
			t.Errorf("expected %s points, got measurements %v", n, measurements(points))
		}
	}
}

// fieldInt reads a numeric field whatever integer type the collector used.
func fieldInt(t *testing.T, p sink.Point, name string) int64 {
	t.Helper()
	switch v := p.Fields[name].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	t.Fatalf("%s: field %q is %T (%v), not a number", p.Measurement, name, p.Fields[name], p.Fields[name])
	return 0
}

func hasField(p sink.Point, name string) bool {
	_, ok := p.Fields[name]
	return ok
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// startOfDay is the timestamp an open item is expected to carry.
func startOfDay(now time.Time) time.Time { return now.UTC().Truncate(24 * time.Hour) }
