package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rateLimitEndpoint serves the fifteen buckets, with the graphql one saying
// what it said when this was measured against the live API: nothing spent,
// whatever had really been spent.
func rateLimitEndpoint(f *fixtureServer) {
	f.handle("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
			"core":    map[string]any{"limit": 5000, "used": 1200, "remaining": 3800, "reset": testNow.Unix() + 600},
			"search":  map[string]any{"limit": 30, "used": 5, "remaining": 25, "reset": testNow.Unix() + 60},
			"graphql": map[string]any{"limit": 5000, "used": 0, "remaining": 5000, "reset": testNow.Unix() + 3600},
			// The deprecated alias for core, which would double every total.
			"rate": map[string]any{"limit": 5000, "used": 1200, "remaining": 3800, "reset": testNow.Unix() + 600},
		}})
	})
}

// graphQLBudget answers any query with a rateLimit block, as the live API
// does once the client has added one.
func graphQLBudget(f *fixtureServer, cost, used int) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if !strings.Contains(query, "rateLimit") {
			f.t.Errorf("the client sent a query with no budget block: %s", query)
		}
		fmt.Fprintf(w, `{"data":{"ghcRateLimit":{"limit":5000,"cost":%d,"used":%d,"remaining":%d,"resetAt":%q}}}`,
			cost, used, 5000-used, testNow.Add(30*time.Minute).Format(time.RFC3339))
	})
}

func TestRateLimitMeasuresTheCollectorItself(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rateLimitEndpoint(f)
	graphQLBudget(f, 1, 162)

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_rate_limit")
	if len(points) != 3 {
		t.Fatalf("got %d buckets, want 3: the `rate` alias is core under a second name", len(points))
	}
	core := find(t, points, "gh_rate_limit", map[string]string{"resource": "core"})
	if fieldInt(t, core, "used") != 1200 || fieldInt(t, core, "remaining") != 3800 {
		t.Errorf("core = %v", core.Fields)
	}
	if fieldInt(t, core, "seconds_to_reset") != 600 {
		t.Errorf("seconds_to_reset = %v, want 600", core.Fields["seconds_to_reset"])
	}
	if core.Fields["used_ratio"] != 0.24 {
		t.Errorf("used_ratio = %v, want 0.24", core.Fields["used_ratio"])
	}
}

func TestRateLimitGraphQLRowComesFromGraphQL(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rateLimitEndpoint(f)
	graphQLBudget(f, 1, 162)

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint says the graphql bucket is untouched. It is not: measured
	// against the live API, that bucket reads used=0 for a classic token while
	// GraphQL charges every query. The row must carry GraphQL's numbers.
	gql := find(t, points, "gh_rate_limit", map[string]string{"resource": "graphql"})
	if fieldInt(t, gql, "used") != 162 || fieldInt(t, gql, "remaining") != 4838 {
		t.Errorf("graphql = %v, want the counter GraphQL reports, not the endpoint's zero", gql.Fields)
	}
	if gql.Fields["used_ratio"] == 0.0 {
		t.Error("used_ratio is the flat zero the defect published")
	}
	if got := fieldInt(t, gql, "seconds_to_reset"); got != 1800 {
		t.Errorf("seconds_to_reset = %d, want 1800: the window is GraphQL's own, not the endpoint's", got)
	}
	if len(only(t, points, "gh_rate_limit")) != 3 {
		t.Error("one resource must produce one row: the endpoint's graphql bucket must not be published too")
	}
}

func TestRateLimitCountsWhatThisProcessSpentOnGraphQL(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rateLimitEndpoint(f)
	graphQLBudget(f, 7, 300)

	// Two ordinary queries, as a sweep makes before this family runs.
	var out map[string]any
	for range 2 {
		if err := f.Client.GraphQL(ctx(t), "query { viewer { login } }", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gql := find(t, points, "gh_rate_limit", map[string]string{"resource": "graphql"})
	if got := fieldInt(t, gql, "own_queries"); got != 2 {
		t.Errorf("own_queries = %d, want 2: the budget probe is free and must not count as a query", got)
	}
	if got := fieldInt(t, gql, "own_cost"); got != 14 {
		t.Errorf("own_cost = %d, want 14: two queries GitHub priced at seven points each", got)
	}
}

// sbomHeaders answers an SBOM request the way the live API does: charged to
// its own one-minute bucket, named in the headers, with the reset given.
func sbomHeaders(f *fixtureServer, used int, reset time.Time) {
	f.handle("/repos/octocat/hello-world/dependency-graph/sbom", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-resource", "dependency_sbom")
		w.Header().Set("x-ratelimit-limit", "100")
		w.Header().Set("x-ratelimit-used", strconv.Itoa(used))
		w.Header().Set("x-ratelimit-remaining", strconv.Itoa(100-used))
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(reset.Unix(), 10))
		fmt.Fprint(w, `{"sbom":{}}`)
	})
}

// The endpoint invents more than the graphql bucket. Measured on 2026-09-12:
// an SBOM request's headers said dependency_sbom used 1, remaining 99, and
// GET /rate_limit two seconds later said used 0, remaining 100, its reset
// sliding forward a second per call. The row read zero through the only
// eighteen requests the bucket ever saw. The headers of the last answer are
// the truth while their window is open.
func TestRateLimitPrefersTheHeadersOfABucketTheEndpointInvents(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
			"core":            map[string]any{"limit": 5000, "used": 1200, "remaining": 3800, "reset": testNow.Unix() + 600},
			"dependency_sbom": map[string]any{"limit": 100, "used": 0, "remaining": 100, "reset": testNow.Unix() + 60},
		}})
	})
	sbomHeaders(f, 18, testNow.Add(42*time.Second))
	var out map[string]any
	if _, _, err := f.Client.GetJSON(ctx(t), "/repos/octocat/hello-world/dependency-graph/sbom", &out, ""); err != nil {
		t.Fatal(err)
	}
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "used") != 18 || fieldInt(t, sbom, "remaining") != 82 || fieldInt(t, sbom, "seconds_to_reset") != 42 {
		t.Errorf("dependency_sbom = %v, want the headers' 18 used, 82 remaining, 42 s to reset", sbom.Fields)
	}
	// core is one the endpoint tells the truth about, and the fixture's
	// headers on every answer say 4999 remaining of 5000, an older reading
	// than the endpoint's 1200 used. The endpoint stands.
	core := find(t, points, "gh_rate_limit", map[string]string{"resource": "core"})
	if fieldInt(t, core, "used") != 1200 {
		t.Errorf("core = %v, want the endpoint's 1200: the headers say less and are older", core.Fields)
	}
	if len(only(t, points, "gh_rate_limit")) != 2 {
		t.Error("one resource, one row")
	}
}

// A minute after the last SBOM request the endpoint's zero is right: the
// one-minute window closed and the budget is back. A reading past its reset
// must not be carried forward as spend.
func TestRateLimitDropsHeadersWhoseWindowHasClosed(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
			"dependency_sbom": map[string]any{"limit": 100, "used": 0, "remaining": 100, "reset": testNow.Unix() + 60},
		}})
	})
	sbomHeaders(f, 18, testNow.Add(-5*time.Minute))
	var out map[string]any
	if _, _, err := f.Client.GetJSON(ctx(t), "/repos/octocat/hello-world/dependency-graph/sbom", &out, ""); err != nil {
		t.Fatal(err)
	}
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "used") != 0 || fieldInt(t, sbom, "remaining") != 100 {
		t.Errorf("dependency_sbom = %v, want the endpoint's refilled bucket", sbom.Fields)
	}
}

func TestRateLimitWritesNoGraphQLRowRatherThanAZero(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rateLimitEndpoint(f)
	// No GraphQL handler: the fixture answers 404, as an unreachable gateway
	// would, and nothing has been collected through GraphQL yet.

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	for _, p := range points {
		if p.Tags["resource"] == "graphql" {
			t.Errorf("wrote a graphql row with nothing to read it from: %v", p.Fields)
		}
	}
	if len(points) != 2 {
		t.Errorf("got %d rows, want core and search", len(points))
	}
}

// budgetThenBroken answers the first GraphQL request with a block whose window
// ends at reset, and every later one with a gateway failure, which is what a
// failed probe looks like from here.
func budgetThenBroken(f *fixtureServer, used int, reset time.Time) {
	var n int
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		n++
		if n > 1 {
			// The gateway's HTML 502, which carries no budget headers of its
			// own: nothing overwrites the reading the first answer left.
			for _, h := range []string{"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-resource"} {
				w.Header().Del(h)
			}
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>502</html>")
			return
		}
		fmt.Fprintf(w, `{"data":{"ghcRateLimit":{"limit":5000,"cost":3,"used":%d,"remaining":%d,"resetAt":%q}}}`,
			used, 5000-used, reset.Format(time.RFC3339))
	})
}

func TestRateLimitFallsBackToAReadingOfThisWindow(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rateLimitEndpoint(f)
	// Read forty minutes into an hour-long window: the sweep that ran just
	// before this family left it behind, and it still describes the window
	// the row is being stamped in.
	budgetThenBroken(f, 900, testNow.Add(20*time.Minute))

	var out map[string]any
	if err := f.Client.GraphQL(ctx(t), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatal(err)
	}
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gql := find(t, points, "gh_rate_limit", map[string]string{"resource": "graphql"})
	if fieldInt(t, gql, "used") != 900 {
		t.Errorf("used = %v, want the last reading of this window", gql.Fields["used"])
	}
	if fieldInt(t, gql, "seconds_to_reset") != 1200 {
		t.Errorf("seconds_to_reset = %v, want 1200", gql.Fields["seconds_to_reset"])
	}
	if fieldInt(t, gql, "own_cost") != 3 {
		t.Errorf("own_cost = %v, want 3: the query that seeded the reading was charged", gql.Fields["own_cost"])
	}
}

func TestRateLimitRefusesToStampAClosedWindowWithNow(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rateLimitEndpoint(f)
	// This family runs every fifteen minutes and the batched queries every
	// twelve hours, so the reading left behind can belong to a window that
	// closed hours ago. Publishing it stamped `now` would date a measurement
	// at a moment it was never taken at.
	budgetThenBroken(f, 900, testNow.Add(-11*time.Hour))

	var out map[string]any
	if err := f.Client.GraphQL(ctx(t), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatal(err)
	}
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		if p.Tags["resource"] == "graphql" {
			t.Errorf("published a window that closed at %s as if it were now: %v", testNow.Add(-11*time.Hour), p.Fields)
		}
	}
}
