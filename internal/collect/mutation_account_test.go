package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// accountInternalError is a GraphQL refusal that is neither a repository gone
// nor a query too large: the kind no collector may read as "nothing here".
const accountInternalError = `{"errors":[{"type":"INTERNAL","message":"something went wrong"}]}`

// accountRateEndpoint serves GET /rate_limit with the buckets given.
func accountRateEndpoint(f *fixtureServer, resources map[string]any) {
	f.handle("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": resources})
	})
}

// accountBucket is one bucket as the endpoint reports it, its reset given in
// seconds from testNow.
func accountBucket(limit, used, remaining int, resetIn int64) map[string]any {
	return map[string]any{"limit": limit, "used": used, "remaining": remaining, "reset": testNow.Unix() + resetIn}
}

// accountSeen makes one REST request whose answer carries the rate headers
// given, which is how the client comes to hold its own reading of a bucket.
func accountSeen(t *testing.T, f *fixtureServer, headers map[string]string) {
	t.Helper()
	const path = "/repos/octocat/hello-world/dependency-graph/sbom"
	f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		fmt.Fprint(w, `{"sbom":{}}`)
	})
	var out map[string]any
	if _, _, err := f.Client.GetJSON(ctx(t), path, &out, ""); err != nil {
		t.Fatal(err)
	}
}

// accountSbomHeaders is a reading of the dependency_sbom bucket, reset in
// 42 s, with the figures given; an empty value leaves that header out.
func accountSbomHeaders(limit, used, remaining string) map[string]string {
	h := map[string]string{
		"x-ratelimit-resource": "dependency_sbom",
		"x-ratelimit-limit":    limit,
		"x-ratelimit-reset":    strconv.FormatInt(testNow.Unix()+42, 10),
	}
	if used != "" {
		h["x-ratelimit-used"] = used
	}
	if remaining != "" {
		h["x-ratelimit-remaining"] = remaining
	}
	return h
}

// The endpoint has buckets that say nothing was used and a remaining below
// the limit. The spend is the difference, and a bucket that does report a
// used figure keeps it even when remaining would say otherwise: GitHub's own
// number is not second-guessed. A bucket with no limit is not a bucket.
func TestRateLimitWorksOutTheSpendOfABucketThatReportsNone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{
		"search":        accountBucket(30, 0, 25, 60),
		"core":          accountBucket(5000, 7, 3800, 600),
		"source_import": accountBucket(0, 0, 0, 60),
		"code_search":   accountBucket(10, 0, 12, 60),
	})
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 3 {
		t.Fatalf("got %d rows, want search, core and code_search: a bucket with no limit has no row", len(points))
	}
	// More remaining than the limit is a bucket GitHub just widened, not a
	// negative spend.
	if cs := find(t, points, "gh_rate_limit", map[string]string{"resource": "code_search"}); fieldInt(t, cs, "used") != 0 {
		t.Errorf("code_search used = %v, want 0 rather than a negative spend", cs.Fields["used"])
	}
	search := find(t, points, "gh_rate_limit", map[string]string{"resource": "search"})
	if fieldInt(t, search, "used") != 5 {
		t.Errorf("search used = %v, want the 5 the limit and remaining imply", search.Fields["used"])
	}
	core := find(t, points, "gh_rate_limit", map[string]string{"resource": "core"})
	if fieldInt(t, core, "used") != 7 || fieldInt(t, core, "remaining") != 3800 {
		t.Errorf("core = %v, want the 7 the bucket reports, not a figure derived over it", core.Fields)
	}
}

// A REST answer can name its bucket and leave x-ratelimit-used out. Its
// reading still says 18 were spent, through the limit and the remaining, and
// that is more than the endpoint admits.
func TestRateLimitReadsTheSpendOffHeadersThatLeaveUsedOut(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"dependency_sbom": accountBucket(100, 0, 100, 60)})
	accountSeen(t, f, accountSbomHeaders("100", "", "82"))

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "used") != 18 || fieldInt(t, sbom, "remaining") != 82 || fieldInt(t, sbom, "seconds_to_reset") != 42 {
		t.Errorf("dependency_sbom = %v, want the headers' 18 used, 82 remaining, 42 s", sbom.Fields)
	}
}

// A header reading with more remaining than its limit and no used header
// implies no spend, which is no more than the endpoint admits.
func TestRateLimitReadsNoSpendOffHeadersWithMoreRemainingThanTheLimit(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"dependency_sbom": accountBucket(100, 0, 100, 60)})
	accountSeen(t, f, accountSbomHeaders("100", "", "120"))

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "used") != 0 || fieldInt(t, sbom, "remaining") != 100 || fieldInt(t, sbom, "seconds_to_reset") != 60 {
		t.Errorf("dependency_sbom = %v, want the endpoint's bucket", sbom.Fields)
	}
}

// A header reading that does carry used keeps it, as the endpoint's buckets
// do, whatever the remaining header would imply.
func TestRateLimitTakesTheUsedHeaderAsItStands(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"dependency_sbom": accountBucket(100, 0, 100, 60)})
	accountSeen(t, f, accountSbomHeaders("100", "18", "50"))

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "used") != 18 || fieldInt(t, sbom, "remaining") != 50 {
		t.Errorf("dependency_sbom = %v, want used 18 as the header says it", sbom.Fields)
	}
}

// Headers that say exactly what the endpoint says are no better a reading,
// so the endpoint's row stands, window and all.
func TestRateLimitKeepsTheEndpointWhenTheHeadersSayNoMore(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"dependency_sbom": accountBucket(100, 18, 82, 60)})
	accountSeen(t, f, accountSbomHeaders("100", "18", "82"))

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "seconds_to_reset") != 60 {
		t.Errorf("seconds_to_reset = %v, want the endpoint's 60: the headers said nothing more", sbom.Fields["seconds_to_reset"])
	}
}

// A reading with a zero limit describes no budget, and taking it would
// publish a used ratio divided by zero in place of the endpoint's bucket.
func TestRateLimitIgnoresAHeaderReadingWithNoLimit(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"dependency_sbom": accountBucket(100, 0, 100, 60)})
	accountSeen(t, f, accountSbomHeaders("0", "5", "95"))

	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	sbom := find(t, points, "gh_rate_limit", map[string]string{"resource": "dependency_sbom"})
	if fieldInt(t, sbom, "limit") != 100 || fieldInt(t, sbom, "used") != 0 || fieldInt(t, sbom, "seconds_to_reset") != 60 {
		t.Errorf("dependency_sbom = %v, want the endpoint's bucket", sbom.Fields)
	}
}

// accountBudgetBlock answers every GraphQL request with a budget block of the
// figures given; a zero reset leaves resetAt out.
func accountBudgetBlock(f *fixtureServer, used, remaining int, reset time.Time) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		block := map[string]any{"limit": 5000, "cost": 1, "used": used, "remaining": remaining}
		if !reset.IsZero() {
			block["resetAt"] = reset.Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{ghapi.RateLimitAlias: block}})
	})
}

// The GraphQL row follows the rule every other row follows: a used of zero
// with a remaining below the limit is spend worked out from the two, and a
// used GraphQL does report is kept as it is.
func TestRateLimitGraphQLRowWorksOutUsedTheWayTheBucketsDo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		used, remaining int
		want            int64
	}{
		{"no used reported", 0, 4838, 162},
		{"used reported", 162, 4000, 162},
		{"more remaining than the limit", 0, 5200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			accountRateEndpoint(f, map[string]any{"core": accountBucket(5000, 1, 4999, 600)})
			accountBudgetBlock(f, tc.used, tc.remaining, testNow.Add(30*time.Minute))
			points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
			if err != nil {
				t.Fatal(err)
			}
			gql := find(t, points, "gh_rate_limit", map[string]string{"resource": "graphql"})
			if got := fieldInt(t, gql, "used"); got != tc.want {
				t.Errorf("used = %d, want %d", got, tc.want)
			}
		})
	}
}

// A budget block without resetAt still says what was spent, and a window
// with no end has no countdown to write rather than one forty years long.
func TestRateLimitGraphQLRowWithNoResetCarriesNoCountdown(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"core": accountBucket(5000, 1, 4999, 600)})
	accountBudgetBlock(f, 162, 4838, time.Time{})
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gql := find(t, points, "gh_rate_limit", map[string]string{"resource": "graphql"})
	if fieldInt(t, gql, "used") != 162 || hasField(gql, "seconds_to_reset") {
		t.Errorf("graphql = %v, want used 162 and no seconds_to_reset", gql.Fields)
	}
}

// When the probe fails, the fallback is the last reading, but only one that
// says which window it belongs to: a reading with no reset cannot be shown
// to be this window's, so no row is written.
func TestRateLimitWritesNoGraphQLRowFromAReadingWithNoWindow(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"core": accountBucket(5000, 1, 4999, 600)})
	var mu sync.Mutex
	var n int
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if !first {
			for _, h := range []string{"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-resource"} {
				w.Header().Del(h)
			}
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>502</html>")
			return
		}
		fmt.Fprint(w, `{"data":{"ghcRateLimit":{"limit":5000,"cost":1,"used":900,"remaining":4100}}}`)
	})
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
			t.Errorf("published a reading with no window as this window's: %v", p.Fields)
		}
	}
}

// A probe that fails before any GraphQL answer was ever read leaves nothing
// to fall back on, and no row is written.
func TestRateLimitWritesNoGraphQLRowWhenTheProbeFailsWithNothingSeen(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountRateEndpoint(f, map[string]any{"core": accountBucket(5000, 1, 4999, 600)})
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		for _, h := range []string{"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-resource"} {
			w.Header().Del(h)
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html>502</html>")
	})
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Tags["resource"] != "core" {
		t.Errorf("got %v, want the core row and no graphql row", points)
	}
}

// A /rate_limit that is broken is a failure; one that is not there is a
// family with only the GraphQL row to write.
func TestRateLimitFailsOnlyWhenTheEndpointIsBroken(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/rate_limit", http.StatusInternalServerError, "boom")
	if _, err := (RateLimit{}).Collect(ctx(t), f.Client, testNow); err == nil {
		t.Error("a 500 from /rate_limit was not reported")
	}

	f = newFixtureServer(t)
	accountBudgetBlock(f, 162, 4838, testNow.Add(30*time.Minute))
	points, err := RateLimit{}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a 404 from /rate_limit is not a failure: %v", err)
	}
	if len(points) != 1 || points[0].Tags["resource"] != "graphql" {
		t.Errorf("got %v, want the graphql row alone", measurements(points))
	}
}

// accountBillingMonths serves the usage report by month: the recorded rows
// for the months named, an empty report for every other.
func accountBillingMonths(f *fixtureServer, withRows ...string) {
	f.handle("/users/octocat/settings/billing/usage", func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(withRows, r.URL.Query().Get("month")) {
			f.write(w, "billing_usage.json")
			return
		}
		_, _ = w.Write([]byte(`{"usageItems":[]}`))
	})
}

// accountMonthsAsked is the month of every usage request, in order.
func accountMonthsAsked(f *fixtureServer) []string {
	var out []string
	for _, c := range f.calls("/users/octocat/settings/billing/usage") {
		out = append(out, c.Query["month"])
	}
	return out
}

// Months left at zero is the current month, not no months at all.
func TestBillingWithNoMonthsSetReadsTheCurrentMonth(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBillingMonths(f, "9")
	points, err := Billing{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := accountMonthsAsked(f); !slices.Equal(got, []string{"9"}) || len(points) != 2 {
		t.Errorf("asked %v and wrote %d rows, want September and its 2 rows", got, len(points))
	}
}

// A walk with a page count of its own decides how many months are read.
func TestBillingLetsAWalkDecideHowManyMonths(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBillingMonths(f, "9", "8", "7")
	if _, err := (Billing{Login: "octocat", Months: 1, Walk: Walk{Pages: 3}}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if got := accountMonthsAsked(f); !slices.Equal(got, []string{"9", "8", "7"}) {
		t.Errorf("asked %v, want the three months the walk allows", got)
	}
}

// A bounded backfill reads back to the month the bound falls in and no
// further: August ends after the 15th and is read, July ends before it.
func TestBillingStopsAtTheMonthThatEndsBeforeTheBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBillingMonths(f, "9", "8", "7", "6")
	since := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if _, err := (Billing{Login: "octocat", Months: 1, Walk: Walk{Pages: -1, Since: since}}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if got := accountMonthsAsked(f); !slices.Equal(got, []string{"9", "8"}) {
		t.Errorf("asked %v, want September and August", got)
	}
}

// Three empty months in a row is how GitHub says it has nothing older, and a
// month with rows starts that count again.
func TestBillingStopsAfterThreeEmptyMonthsInARow(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBillingMonths(f, "7")
	points, err := Billing{Login: "octocat", Months: 7}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := accountMonthsAsked(f); !slices.Equal(got, []string{"9", "8", "7", "6", "5", "4"}) {
		t.Errorf("asked %v, want two empty months, July's rows, then three empty months", got)
	}
	if len(points) != 2 {
		t.Errorf("wrote %d rows, want July's 2", len(points))
	}
}

// A month GitHub no longer has counts as empty too.
func TestBillingStopsAfterThreeMonthsGitHubNoLongerHas(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := Billing{Login: "octocat", Months: 6}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a missing month is not a failure: %v", err)
	}
	if got := accountMonthsAsked(f); !slices.Equal(got, []string{"9", "8", "7"}) || len(points) != 0 {
		t.Errorf("asked %v and wrote %d rows, want three months and nothing", got, len(points))
	}
}

// A month that is broken rather than missing is a failure, handed up with
// the rows the months before it gave.
func TestBillingReportsABrokenMonthWithTheRowsAlreadyRead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/settings/billing/usage", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("month") == "9" {
			f.write(w, "billing_usage.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	points, err := Billing{Login: "octocat", Months: 3}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Fatal("a 500 was not reported")
	}
	if len(points) != 2 {
		t.Errorf("got %d rows beside the error, want September's 2", len(points))
	}
}

// A row whose date is neither of the two shapes GitHub uses has no day to be
// stamped at, so it is dropped and its neighbors are not.
func TestBillingSkipsARowWithADateItCannotRead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/settings/billing/usage", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usageItems":[` +
			`{"date":"yesterday","product":"Actions","sku":"Actions Linux","unitType":"Minutes","quantity":1},` +
			`{"date":"2026-09-03","product":"Actions","sku":"Actions Linux","unitType":"Minutes","quantity":2}]}`))
	})
	points, err := Billing{Login: "octocat", Months: 1}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Fields["quantity"] != 2.0 {
		t.Errorf("got %v, want the one row with a date", points)
	}
}

// accountBrokenFor answers the audience and totals batches like their
// fixtures do, except that any query naming the repository given is refused
// in a way that is not recoverable.
func accountBrokenFor(name string, answer func(t *testing.T, w http.ResponseWriter, query string)) func(t *testing.T, f *fixtureServer) {
	return func(t *testing.T, f *fixtureServer) {
		t.Helper()
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
			if name == "" || strings.Contains(query, fmt.Sprintf("name: %q", name)) {
				_, _ = w.Write([]byte(accountInternalError))
				return
			}
			answer(t, w, query)
		})
	}
}

// accountRepo is a repository of octocat by name.
func accountRepo(name string) Repo {
	return Repo{Owner: "octocat", Name: name, FullName: "octocat/" + name}
}

// A batch that fails beside one that answered costs its own rows and nothing
// else, and the failure travels back with the rows that did arrive.
func TestAudienceKeepsTheRowsOfTheBatchesThatAnsweredAndReportsTheOneThatDidNot(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBrokenFor("broken", answerAudience)(t, f)
	a := Audience{Repos: []Repo{testRepo, accountRepo("broken")}, Stars: true, Batch: 1}
	points, err := a.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Error("one batch failed and the collector reported success")
	}
	if got := len(only(t, points, "gh_star")); got != 4 {
		t.Errorf("got %d stars, want the 4 of the repository that answered", got)
	}
	if n := len(f.calls("/graphql")); n != 2 {
		t.Errorf("%d queries for two repositories in batches of one", n)
	}
}

// Nothing answering is a failure, and it says why.
func TestAudienceReportsAFailureWhenNothingAnswered(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBrokenFor("", answerAudience)(t, f)
	points, err := Audience{Repos: []Repo{testRepo}, Stars: true, Forks: true}.Collect(ctx(t), f.Client, testNow)
	if err == nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want the failure and nothing", len(points), err)
	}
}

// A repository that answered with no stars is a family that ran and found
// nothing, not a failure.
func TestAudienceWithNothingToWriteIsNoFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"r0":{"stargazers":{"edges":[]}}}}`))
	})
	points, err := Audience{Repos: []Repo{testRepo}, Stars: true}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want nothing and no error", len(points), err)
	}
}

// No repository is nothing to ask, whichever connections are due.
func TestAudienceWithNoRepositoriesAsksNothing(t *testing.T) {
	t.Parallel()
	var queries []string
	f := audienceFixture(t, &queries)
	points, err := Audience{Stars: true, Forks: true}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 || len(queries) != 0 {
		t.Errorf("err=%v points=%d queries=%d, want nothing asked", err, len(points), len(queries))
	}
}

// Each alias asks for the count of the type it searches: the issue searches
// for issueCount and the repository search for repositoryCount. The fake in
// totals_test answers by the search type and ignores the selection, so only
// the query text shows which count was asked for.
func TestCountsQueryAsksEachSearchForTheCountOfItsType(t *testing.T) {
	t.Parallel()
	q := countsQuery([]searchCount{
		{field: "pulls_opened", kind: "ISSUE", query: "type:pr author:octocat"},
		{field: "repositories", kind: "REPOSITORY", query: "user:octocat"},
	})
	for _, want := range []string{
		`pulls_opened: search(type: ISSUE, query: "type:pr author:octocat") { issueCount }`,
		`repositories: search(type: REPOSITORY, query: "user:octocat") { repositoryCount }`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query does not carry %s:\n%s", want, q)
		}
	}
}

// A login and no repository is the counts and nothing else.
func TestTotalsWithNoRepositoriesAsksOnlyForTheCounts(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var batches []string
	graphQLTotals(f, &batches)
	points, err := Totals{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 0 {
		t.Errorf("%d repository batches for no repository", len(batches))
	}
	if got := byMeasurement(points); len(got["gh_account_total"]) != 1 || len(got["gh_repo_total"]) != 0 {
		t.Errorf("wrote %v, want the account row alone", measurements(points))
	}
}

// Nothing answering is a failure, and it says why.
func TestTotalsReportsAFailureWhenNoRepositoryAnswered(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBrokenFor("", answerTotals)(t, f)
	points, err := Totals{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err == nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want the failure and nothing", len(points), err)
	}
}

// A repository whose alias came back null is skipped, and a batch with
// nothing left to write is not a failure.
func TestTotalsWithOnlyNullAnswersIsNoFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"r0":null}}`))
	})
	points, err := Totals{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want nothing and no error", len(points), err)
	}
}

// A batch that fails beside one that answered costs its own rows only, and is
// still reported.
func TestTotalsKeepsTheRepositoriesThatAnsweredBesideOneThatFailed(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountBrokenFor("broken", answerTotals)(t, f)
	points, err := Totals{Repos: []Repo{accountRepo("r0"), accountRepo("broken")}, Batch: 1}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Error("one batch failed and the collector reported success")
	}
	if got := len(only(t, points, "gh_repo_total")); got != 1 {
		t.Errorf("got %d repository rows, want the one that answered", got)
	}
	find(t, points, "gh_repo_total", map[string]string{"full_name": "octocat/r0"})
}

// Batch left at zero is ten to a query, the size the gateway was measured to
// finish, and not the smaller default the shared batching falls back on.
func TestTotalsPutsTenRepositoriesInAQueryWhenNoBatchIsSet(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var batches []string
	graphQLTotals(f, &batches)
	repos := make([]Repo, 7)
	for i := range repos {
		repos[i] = accountRepo(fmt.Sprintf("r%d", i))
	}
	if _, err := (Totals{Repos: repos}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Errorf("%d queries for 7 repositories, want one batch of up to ten", len(batches))
	}
}

// The lifetime row carries how long the repository has lived and how long
// since it was pushed, in whole days: created 2017-09-13, pushed 2026-09-01.
func TestTotalsCountsTheDaysARepositoryHasLivedAndSinceItsLastPush(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	graphQLTotals(f, nil)
	points, err := Totals{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	repo := find(t, points, "gh_repo_total", map[string]string{"full_name": "octocat/hello-world"})
	if fieldInt(t, repo, "age_days") != 3282 || fieldInt(t, repo, "days_since_push") != 7 {
		t.Errorf("age_days = %v, days_since_push = %v, want 3282 and 7", repo.Fields["age_days"], repo.Fields["days_since_push"])
	}
}

// accountArchiveAnswer answers the archive query for every alias with a date,
// refusing any query that names a1: as a group, as a query too large, and on
// its own, as a failure that is not recoverable.
func accountArchiveAnswer(t *testing.T, w http.ResponseWriter, query string) {
	t.Helper()
	if strings.Contains(query, `name: "a1"`) {
		if strings.Contains(query, "r1:") {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
			return
		}
		_, _ = w.Write([]byte(accountInternalError))
		return
	}
	data := map[string]any{}
	for i := 0; strings.Contains(query, fmt.Sprintf("r%d:", i)); i++ {
		data[fmt.Sprintf("r%d", i)] = map[string]any{
			"nameWithOwner": "octocat/a0", "url": "https://github.com/octocat/a0",
			"createdAt": "2020-05-14T00:00:00Z", "archivedAt": "2026-08-29T15:22:31Z",
		}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("answer the archive query: %v", err)
	}
}

// The archive rows of the repositories set aside follow the rule of the
// family: one that failed beside one that answered costs its own row, the
// failure is reported either way, and none answering leaves no rows at all.
func TestTotalsKeepsTheArchiveRowsThatAnswered(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		accountArchiveAnswer(t, w, query)
	})
	aside := []Repo{accountRepo("a0"), accountRepo("a1")}
	points, err := Totals{Archived: aside}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Error("one repository refused of two and the collector reported success")
	}
	if got := len(only(t, points, "gh_repo_archived")); got != 1 {
		t.Errorf("got %d archive rows, want the one that answered", got)
	}

	f = newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(accountInternalError))
	})
	points, err = Totals{Archived: aside}.Collect(ctx(t), f.Client, testNow)
	if err == nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want the failure and nothing", len(points), err)
	}
}

// A repository set aside that GraphQL says is not archived, or archived at
// no instant, has no archive row; one with no creation date has its row
// without the age it cannot compute.
func TestArchiveRowNeedsADateToStandOn(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]bool{
		`{"archivedAt":null}`:                   false,
		`{"archivedAt":"0001-01-01T00:00:00Z"}`: false,
		`{"archivedAt":"2026-08-29T15:22:31Z"}`: true,
	} {
		var rt repoTotals
		mustUnmarshal(t, []byte(body), &rt)
		row, ok := rt.archived(testRepo)
		if ok != want {
			t.Errorf("%s: archived = %t, want %t", body, ok, want)
			continue
		}
		if ok && hasField(row, "age_days_at_archive") {
			t.Errorf("%s: an age with no creation date to subtract from: %v", body, row.Fields)
		}
	}

	f := totalsFixture(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"r0":{"nameWithOwner":"octocat/a0","archivedAt":null}}}`))
	})
	points, err := Totals{Archived: []Repo{accountRepo("a0")}}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want no row for a repository that is not archived", len(points), err)
	}
}

// The counts are asked again one by one only while that can help: not once
// the sweep is canceled, and not when the budget is what refused.
func TestRetryOneByOneGivesUpOnACanceledSweepOrASpentBudget(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	refused := errors.New("graphql: SERVICE_UNAVAILABLE: search timed out")
	if retryOneByOne(canceled, refused) {
		t.Error("a canceled sweep retried the counts")
	}
	if retryOneByOne(ctx(t), &ghapi.RateLimitedError{Path: "/graphql", Resource: "graphql"}) {
		t.Error("a spent REST-shaped budget retried the counts")
	}
	if retryOneByOne(ctx(t), errors.New("graphql: RATE_LIMITED: API rate limit exceeded")) {
		t.Error("a spent GraphQL budget retried the counts")
	}
	if !retryOneByOne(ctx(t), refused) {
		t.Error("a search that timed out was not retried")
	}
}

// config.yaml is the chooser's settings under either extension and in any
// case, and a .yaml form is a template like a .yml one.
func TestIssueTemplateFileReadsBothYAMLExtensions(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"CONFIG.YAML": false, "config.yml": false,
		"bug.yaml": true, "bug.yml": true, "bug.md": true, "notes.txt": false,
	} {
		if got := isIssueTemplateFile(name); got != want {
			t.Errorf("isIssueTemplateFile(%q) = %t, want %t", name, got, want)
		}
	}
}

// accountEmptyList answers a listing with no entries.
func accountEmptyList(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }

// accountContainerOnly serves the recorded container package on its listing
// and an empty list for every other registry.
func accountContainerOnly(f *fixtureServer) {
	f.handle("/user/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("package_type") == "container" {
			f.write(w, "packages_container.json")
			return
		}
		accountEmptyList(w, r)
	})
}

// A package and a gist carry their ages in whole days, and those are the only
// place their dates survive, since both rows are stamped now. The package was
// created 2026-06-01 and updated 2026-09-01; the gist 2024-07-28 and
// 2026-05-01.
func TestProfileCountsTheDaysOfAPackageAndAGist(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	accountContainerOnly(f)
	f.file("/user/packages/container/ghchronicle", "package_element.json")
	f.file("/user/packages/container/ghchronicle/versions", "package_versions.json")
	f.file("/gists", "gists.json")
	f.file("/users/octocat/social_accounts", "social_accounts.json")
	f.file("/users/octocat", "user_profile.json")

	points, err := Profile{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	pkg := only(t, points, "gh_package")[0]
	if fieldInt(t, pkg, "age_days") != 99 || fieldInt(t, pkg, "days_since_update") != 7 {
		t.Errorf("package ages = %v, want 99 and 7", pkg.Fields)
	}
	gist := only(t, points, "gh_gist")[0]
	if fieldInt(t, gist, "age_days") != 772 || fieldInt(t, gist, "days_since_update") != 130 {
		t.Errorf("gist ages = %v, want 772 and 130", gist.Fields)
	}
}

// The referrers fallback tag is exactly sha256- and sixty four lower case hex
// digits. Anything else a person could have typed, including a tag that is
// almost that, is a release and is kept.
func TestIsReferrerTagAcceptsOnlyTheExactDigestShape(t *testing.T) {
	t.Parallel()
	hex := strings.Repeat("0f", 32)
	for tag, want := range map[string]bool{
		"sha256-" + hex:                           true,
		"sha256-" + strings.Repeat("9a", 32):      true,
		"sha512-" + hex:                           false,
		"sha256-" + hex[:10]:                      false,
		"sha256-" + strings.ToUpper(hex):          false,
		"sha256-" + hex[:63] + "g":                false,
		"sha256-" + hex[:63] + "+":                false,
		"latest":                                  false,
		"1.2.0-sha256-" + hex[:len(hex)-6]:        false,
		"release-" + strings.Repeat("a", 64)[:63]: false,
	} {
		if got := isReferrerTag(tag); got != want {
			t.Errorf("isReferrerTag(%q) = %t, want %t", tag, got, want)
		}
	}
}

// accountVersionPages serves a package's version list by page: the number of
// versions each named page holds, an empty page otherwise, and no element,
// so the walked count is the one written.
func accountVersionPages(t *testing.T, f *fixtureServer, sizes map[string]int) {
	t.Helper()
	accountContainerOnly(f)
	f.handle("/user/packages/container/ghchronicle/versions", func(w http.ResponseWriter, r *http.Request) {
		n := sizes[r.URL.Query().Get("page")]
		if n == 0 {
			accountEmptyList(w, r)
			return
		}
		_, _ = w.Write(repeat(t, "package_versions.json", "", n, func(_ int, row map[string]any) {
			row["metadata"] = map[string]any{"container": map[string]any{"tags": []string{}}}
		}))
	})
	f.handle("/gists", accountEmptyList)
	f.handle("/users/octocat/social_accounts", accountEmptyList)
}

// accountPagesAsked is the page of every version request, in order.
func accountPagesAsked(f *fixtureServer, path string) []string {
	var out []string
	for _, c := range f.calls(path) {
		out = append(out, c.Query["page"])
	}
	return out
}

// The version walk reads full pages of a hundred until a short or empty one,
// and never past the pages its walk allows.
func TestProfileWalksVersionPagesUntilOneIsShort(t *testing.T) {
	t.Parallel()
	const versions = "/user/packages/container/ghchronicle/versions"
	for _, tc := range []struct {
		name  string
		walk  Walk
		sizes map[string]int
		pages []string
		want  int64
	}{
		{"a full page then a short one", Walk{Pages: 3}, map[string]int{"1": 100, "2": 1, "3": 100}, []string{"1", "2"}, 101},
		{"a full page then an empty one", Walk{}, map[string]int{"1": 100}, []string{"1", "2"}, 100},
		{"a full page and a walk of one", Walk{Pages: 1}, map[string]int{"1": 100, "2": 100}, []string{"1"}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			accountVersionPages(t, f, tc.sizes)
			points, err := Profile{Login: "octocat", Walk: tc.walk}.Collect(ctx(t), f.Client, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := accountPagesAsked(f, versions); !slices.Equal(got, tc.pages) {
				t.Errorf("asked pages %v, want %v", got, tc.pages)
			}
			if got := fieldInt(t, only(t, points, "gh_package")[0], "versions"); got != tc.want {
				t.Errorf("versions = %d, want %d", got, tc.want)
			}
		})
	}
}

// A listing, a version walk, an element, the gists, the social accounts or
// the profile that is broken rather than missing fails the family, with the
// rows read before it.
func TestProfileReportsEveryBrokenSource(t *testing.T) {
	t.Parallel()
	broken := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}
	for name, routes := range map[string]func(f *fixtureServer){
		"the package listing": func(f *fixtureServer) { f.handle("/user/packages", broken) },
		"the version walk": func(f *fixtureServer) {
			accountContainerOnly(f)
			f.handle("/user/packages/container/ghchronicle/versions", broken)
		},
		"the package element": func(f *fixtureServer) {
			accountContainerOnly(f)
			f.file("/user/packages/container/ghchronicle/versions", "package_versions.json")
			f.handle("/user/packages/container/ghchronicle", broken)
		},
		"the gists": func(f *fixtureServer) {
			f.handle("/user/packages", accountEmptyList)
			f.handle("/gists", broken)
		},
		"the social accounts": func(f *fixtureServer) {
			f.handle("/user/packages", accountEmptyList)
			f.handle("/gists", accountEmptyList)
			f.handle("/users/octocat/social_accounts", broken)
		},
		"the profile": func(f *fixtureServer) {
			f.handle("/user/packages", accountEmptyList)
			f.handle("/gists", accountEmptyList)
			f.file("/users/octocat/social_accounts", "social_accounts.json")
			f.handle("/users/octocat", broken)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			routes(f)
			if _, err := (Profile{Login: "octocat"}).Collect(ctx(t), f.Client, testNow); err == nil {
				t.Errorf("a 500 from %s was not reported", name)
			}
		})
	}
}

// A package that belongs to no repository is still a package, and the
// listing goes on past it.
func TestProfileWritesAPackageThatBelongsToNoRepository(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/user/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("package_type") == "npm" {
			_, _ = w.Write([]byte(`[{"name":"loose","package_type":"npm","visibility":"public","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z","repository":null}]`))
			return
		}
		accountEmptyList(w, r)
	})
	f.handle("/gists", accountEmptyList)
	f.handle("/users/octocat/social_accounts", accountEmptyList)
	points, err := Profile{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	pkg := find(t, points, "gh_package", map[string]string{"package": "loose", "type": "npm"})
	if pkg.Tags["repo"] == "octocat/hello-world" || fieldInt(t, pkg, "versions") != 0 {
		t.Errorf("package with no repository = %v %v", pkg.Tags, pkg.Fields)
	}
	if n := len(f.calls("/user/packages")); n != 6 {
		t.Errorf("asked %d registries, want all 6", n)
	}
}

// A scheme with no host after it is not a link.
func TestWebsiteURLRefusesASchemeWithNoHost(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"https://", "http:///path"} {
		if got := websiteURL(in); got != "" {
			t.Errorf("websiteURL(%q) = %q, want nothing", in, got)
		}
	}
}

// A key's age is where its creation date went, in whole days: both recorded
// keys were made on 2026-05-28, the SSH one at 10:00 and the GPG one at 16:02,
// which is a day less by 15:04.
func TestKeysCountTheDaysSinceEachKeyWasMade(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/user/keys", "user_keys.json")
	f.file("/user/gpg_keys", "user_gpg_keys.json")
	points, err := Keys{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	ssh := find(t, points, "gh_key", map[string]string{"key": "nginx"})
	gpg := find(t, points, "gh_key", map[string]string{"kind": "gpg"})
	if fieldInt(t, ssh, "age_days") != 103 || fieldInt(t, gpg, "age_days") != 102 {
		t.Errorf("age_days = %v and %v, want 103 and 102", ssh.Fields["age_days"], gpg.Fields["age_days"])
	}
}

// Either listing missing is a token without that scope and costs only its own
// rows; either listing broken fails the family. A GPG key with no expiry has
// no countdown to write.
func TestKeysSkipAMissingListingAndReportABrokenOne(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/user/gpg_keys", "user_gpg_keys.json")
	points, err := Keys{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 1 || points[0].Tags["kind"] != "gpg" {
		t.Errorf("no SSH listing: got %v and %v, want the GPG key alone", points, err)
	}

	f = newFixtureServer(t)
	f.file("/user/keys", "user_keys.json")
	points, err = Keys{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 2 {
		t.Errorf("no GPG listing: got %d points and %v, want the two SSH keys", len(points), err)
	}

	f = newFixtureServer(t)
	f.status("/user/keys", http.StatusInternalServerError, "boom")
	f.file("/user/gpg_keys", "user_gpg_keys.json")
	if _, err = (Keys{Login: "octocat"}).Collect(ctx(t), f.Client, testNow); err == nil {
		t.Error("a broken SSH listing was not reported")
	}

	f = newFixtureServer(t)
	f.file("/user/keys", "user_keys.json")
	f.status("/user/gpg_keys", http.StatusInternalServerError, "boom")
	points, err = Keys{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err == nil || len(points) != 2 {
		t.Errorf("a broken GPG listing: got %d points and %v, want the SSH keys and the error", len(points), err)
	}

	f = newFixtureServer(t)
	f.handle("/user/gpg_keys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"key_id":"NOEXPIRY","created_at":"2026-05-28T16:02:14Z","expires_at":null,"can_sign":true,"emails":[]}]`))
	})
	points, err = Keys{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gpg := find(t, points, "gh_key", map[string]string{"key": "NOEXPIRY"})
	if hasField(gpg, "days_to_expiry") || fieldInt(t, gpg, "age_days") != 102 {
		t.Errorf("a key that never expires = %v", gpg.Fields)
	}
}

// accountForkPages serves the fork list by page: the number of forks each
// named page holds, an empty page otherwise.
func accountForkPages(t *testing.T, f *fixtureServer, sizes map[string]int) {
	t.Helper()
	f.handle("/repos/octocat/hello-world/forks", func(w http.ResponseWriter, r *http.Request) {
		n := sizes[r.URL.Query().Get("page")]
		if n == 0 {
			accountEmptyList(w, r)
			return
		}
		_, _ = w.Write(repeat(t, "forks.json", "", n, nil))
	})
}

// The fork walk reads full pages of a hundred until a short or empty one,
// and never past the pages its walk allows.
func TestForksWalkPagesUntilOneIsShort(t *testing.T) {
	t.Parallel()
	const forks = "/repos/octocat/hello-world/forks"
	for _, tc := range []struct {
		name  string
		walk  Walk
		sizes map[string]int
		pages []string
		want  int
	}{
		{"a full page then a short one", Walk{Pages: 3}, map[string]int{"1": 100, "2": 1, "3": 100}, []string{"1", "2"}, 101},
		{"a full page then an empty one", Walk{}, map[string]int{"1": 100}, []string{"1", "2"}, 100},
		{"a full page and a walk of one", Walk{Pages: 1}, map[string]int{"1": 100, "2": 100}, []string{"1"}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			accountForkPages(t, f, tc.sizes)
			points, err := Forks{Walk: tc.walk}.Collect(ctx(t), f.Client, testRepo, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := accountPagesAsked(f, forks); !slices.Equal(got, tc.pages) {
				t.Errorf("asked pages %v, want %v", got, tc.pages)
			}
			if len(points) != tc.want {
				t.Errorf("got %d forks, want %d", len(points), tc.want)
			}
		})
	}
}

// A fork list that is not there is nothing to write, a list GitHub will not
// page further keeps the pages it gave, and a broken one fails.
func TestForksTellAMissingListFromABrokenOne(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := Forks{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("a missing list: got %d points and %v, want nothing and no error", len(points), err)
	}

	f = newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/forks", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(repeat(t, "forks.json", "", 100, nil))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"In order to keep the API fast for everyone, pagination is limited for this resource."}`))
	})
	points, err = Forks{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 100 {
		t.Errorf("a pagination limit: got %d points and %v, want the first page and no error", len(points), err)
	}

	f = newFixtureServer(t)
	f.status("/repos/octocat/hello-world/forks", http.StatusInternalServerError, "boom")
	if _, err = (Forks{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
		t.Error("a broken list was not reported")
	}
}

// A fork that was never pushed has no push to measure to: no gap from the
// fork to it and no verdict on whether it advanced.
func TestForkNeverPushedHasNoPushFields(t *testing.T) {
	t.Parallel()
	rows := []forkRow{{FullName: "carol/hello-world", CreatedAt: testNow.Add(-time.Hour), HTMLURL: "https://github.com/carol/hello-world"}}
	rows[0].Owner.Login = "carol"
	p := forkPoints(rows, testRepo)[0]
	if hasField(p, "seconds_to_push") || hasField(p, "advanced") {
		t.Errorf("a fork never pushed = %v", p.Fields)
	}
}

// A label used as often on issues as on pull requests is used, however the
// two compare; a milestone with no due date has no countdown.
func TestPlanningKeepsAnEvenlyUsedLabelAndAnUndatedMilestone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"repository":{` +
			`"labels":{"nodes":[{"name":"even","url":"https://github.com/octocat/hello-world/labels/even","issues":{"totalCount":3},"pullRequests":{"totalCount":3}}]},` +
			`"milestones":{"nodes":[{"title":"someday","state":"OPEN","url":"https://github.com/octocat/hello-world/milestone/9","createdAt":"2026-01-01T00:00:00Z","dueOn":null,"closedAt":null,"progressPercentage":0,"issues":{"totalCount":0},"pullRequests":{"totalCount":0}}]}}}}`))
	})
	points, err := Planning{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	label := find(t, points, "gh_label", map[string]string{"label": "even"})
	if fieldInt(t, label, "used") != 6 {
		t.Errorf("label = %v, want used 6", label.Fields)
	}
	if m := find(t, points, "gh_milestone", map[string]string{"milestone": "someday"}); hasField(m, "days_to_due") {
		t.Errorf("an undated milestone = %v", m.Fields)
	}
}

// A comment reached by either walk names its own author when GraphQL gives
// one, and says whether it answers another comment rather than the
// discussion itself.
func TestDiscussionCommentNamesItsAuthorAndWhetherItIsAReply(t *testing.T) {
	t.Parallel()
	for body, want := range map[string][2]string{
		`{"author":{"login":"bob"},"replyTo":{"databaseId":4}}`: {"bob", "true"},
		`{"author":{"login":""},"replyTo":null}`:                {"octocat", "false"},
		`{"author":null}`:                                       {"octocat", "false"},
	} {
		var n discussionCommentNode
		mustUnmarshal(t, []byte(body), &n)
		p := n.point("octocat", "alice/x")
		if p.Tags["author"] != want[0] || p.Tags["is_reply"] != want[1] {
			t.Errorf("%s: author %q is_reply %q, want %q and %q", body, p.Tags["author"], p.Tags["is_reply"], want[0], want[1])
		}
	}
}

// An accepted answer whose author GraphQL hides names nobody, and a thread
// with no creation date has no durations to measure from it.
func TestDiscussionContextWithoutAnAnswerAuthorOrACreationDate(t *testing.T) {
	t.Parallel()
	var n discussionCommentNode
	mustUnmarshal(t, []byte(`{"discussion":{"isAnswered":true,"answerChosenAt":"2026-09-02T00:00:00Z","closedAt":"2026-09-03T00:00:00Z","answer":{"author":null}}}`), &n)
	fields := map[string]any{}
	n.context().addTo(fields)
	if _, named := fields["answered_by"]; named || fields["discussion_answered"] != true {
		t.Errorf("fields = %v, want answered and nobody named", fields)
	}
	for _, name := range []string{"seconds_to_answer", "seconds_to_close"} {
		if _, ok := fields[name]; ok {
			t.Errorf("%s written with no creation date: %v", name, fields)
		}
	}
}

// accountBackwardServer answers one of the two viewer comment connections
// with one comment a page, saying there is an older page until the fifth
// query, and records the variables of each.
func accountBackwardServer(t *testing.T, connection, node string, emptyPage bool) (*fixtureServer, func() []map[string]any) {
	t.Helper()
	f := newFixtureServer(t)
	var mu sync.Mutex
	var asked []map[string]any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		mu.Lock()
		asked = append(asked, vars)
		n := len(asked)
		mu.Unlock()
		nodes := node
		if emptyPage {
			nodes = ""
		}
		fmt.Fprintf(w, `{"data":{"viewer":{%q:{"totalCount":9,"pageInfo":{"hasPreviousPage":%t,"startCursor":"c%d"},"nodes":[%s]}}}}`,
			connection, n < 5, n, nodes)
	})
	return f, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

const (
	accountIssueComment      = `{"createdAt":"2026-09-06T08:00:00Z","url":"https://github.com/x/y/issues/1#issuecomment-1","issue":{"number":1,"repository":{"nameWithOwner":"x/y"}}}`
	accountDiscussionComment = `{"createdAt":"2026-09-06T08:00:00Z","databaseId":1,"url":"https://github.com/x/y/discussions/1#discussioncomment-1","discussion":{"number":1,"repository":{"nameWithOwner":"x/y"}}}`
)

// Both viewer comment walks go back one cursor at a time as far as the walk
// allows, stop at a page with nothing on it however much it says is older,
// stop once the page is past the bound, and hand up a failed page.
func TestViewerCommentWalksStopWhereTheWalkSays(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	for name, walk := range map[string]struct {
		connection, node string
		run              func(o Outbound, c *ghapi.Client) ([]sink.Point, error)
	}{
		"issue comments": {"issueComments", accountIssueComment, func(o Outbound, c *ghapi.Client) ([]sink.Point, error) {
			return o.issueComments(ctx(t), c, testNow)
		}},
		"discussion comments": {"repositoryDiscussionComments", accountDiscussionComment, func(o Outbound, c *ghapi.Client) ([]sink.Point, error) {
			return o.discussionComments(ctx(t), c, testNow)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, asked := accountBackwardServer(t, walk.connection, walk.node, false)
			points, err := walk.run(Outbound{Login: "octocat", Walk: Walk{Pages: 2}}, f.Client)
			if err != nil {
				t.Fatal(err)
			}
			got := asked()
			if len(got) != 2 || got[0]["before"] != nil || got[1]["before"] != "c1" || len(points) != 2 {
				t.Errorf("a walk of two pages asked %v and wrote %d rows, want two pages, the second before c1", got, len(points))
			}

			f, asked = accountBackwardServer(t, walk.connection, walk.node, true)
			if _, err = walk.run(Outbound{Login: "octocat", Walk: Unbounded}, f.Client); err != nil {
				t.Fatal(err)
			}
			if n := len(asked()); n != 1 {
				t.Errorf("an empty page that says there is an older one: %d queries, want 1", n)
			}

			f, asked = accountBackwardServer(t, walk.connection, walk.node, false)
			if _, err = walk.run(Outbound{Login: "octocat", Walk: Walk{Pages: -1, Since: since}}, f.Client); err != nil {
				t.Fatal(err)
			}
			if n := len(asked()); n != 1 {
				t.Errorf("a page past the bound: %d queries, want 1", n)
			}

			f = newFixtureServer(t)
			f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
				_, _ = w.Write([]byte(accountInternalError))
			})
			if _, err = walk.run(Outbound{Login: "octocat"}, f.Client); err == nil {
				t.Error("a failed page was not reported")
			}
		})
	}
}

// accountStarredServer answers the starred walk with one star a page, saying
// there is a next page until the fifth query.
func accountStarredServer(t *testing.T, emptyPage bool) (*fixtureServer, func() int) {
	t.Helper()
	f := newFixtureServer(t)
	var mu sync.Mutex
	var n int
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		mu.Lock()
		n++
		page := n
		mu.Unlock()
		edges := `{"starredAt":"2026-09-01T07:30:00Z","node":{"nameWithOwner":"golang/go","stargazerCount":1,"primaryLanguage":null}}`
		if emptyPage {
			edges = ""
		}
		fmt.Fprintf(w, `{"data":{"viewer":{"starredRepositories":{"pageInfo":{"hasNextPage":%t,"endCursor":"c%d"},"edges":[%s]}}}}`,
			page < 5, page, edges)
	})
	return f, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// The starred walk reads as many pages as the walk allows and stops at a page
// with nothing on it however much it says follows.
func TestOutboundStarredReadsNoMorePagesThanTheWalkAllows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		walk  Walk
		empty bool
		want  int
	}{
		{Walk{Pages: 1}, false, 1},
		{Walk{Pages: 2}, false, 2},
		{Unbounded, true, 1},
	} {
		f, queries := accountStarredServer(t, tc.empty)
		points, err := Outbound{Login: "octocat", Walk: tc.walk}.starred(ctx(t), f.Client)
		if err != nil {
			t.Fatal(err)
		}
		if queries() != tc.want {
			t.Errorf("walk %+v, empty pages %t: %d queries, want %d", tc.walk, tc.empty, queries(), tc.want)
		}
		if !tc.empty && len(points) != tc.want {
			t.Errorf("walk %+v: %d stars, want one a page", tc.walk, len(points))
		}
	}
}

// Every GraphQL half of Outbound tells "nothing here" from "broken": a
// starred list that is not found ends quietly, and a failure of the stars,
// of a search or of the comments fails the family.
func TestOutboundFailsOnEveryBrokenHalf(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"gone"}]}`))
	})
	points, err := Outbound{Login: "octocat"}.starred(ctx(t), f.Client)
	if err != nil || len(points) != 0 {
		t.Errorf("a starred list not found: got %d points and %v, want nothing and no error", len(points), err)
	}

	for _, broken := range []string{"starredRepositories(first:", "search(type: ISSUE", "repositoryDiscussionComments", "issueComments("} {
		srv := newFixtureServer(t)
		srv.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
			switch {
			case strings.Contains(query, broken):
				_, _ = w.Write([]byte(accountInternalError))
			case strings.Contains(query, "starredRepositories(first:"):
				srv.write(w, "graphql_starred.json")
			case strings.Contains(query, "search(type: ISSUE"):
				srv.write(w, "graphql_search_issues.json")
			case strings.Contains(query, "repositoryDiscussionComments"):
				srv.write(w, "viewer_discussion_comments.json")
			default:
				srv.write(w, "viewer_issue_comments.json")
			}
		})
		if _, collectErr := (Outbound{Login: "octocat"}).Collect(ctx(t), srv.Client, testNow); collectErr == nil {
			t.Errorf("a failure of the query holding %q was not reported", broken)
		}
	}
}

// A name with no owner in it belongs to nobody.
func TestIsOwnNeedsAnOwnerInTheName(t *testing.T) {
	t.Parallel()
	if isOwn("octocat", "octocat") {
		t.Error("a bare name was read as the account's own repository")
	}
}

// The error names the page and says what about it moved.
func TestMarkupErrorSaysWhatMoved(t *testing.T) {
	t.Parallel()
	err := &MarkupError{Reason: "no achievement card"}
	if got := err.Error(); got != "achievements page: markup changed: no achievement card" {
		t.Errorf("Error() = %q", got)
	}
}

// No login is no profile to read, and nothing is asked.
func TestAchievementsWithNoLoginAskNothing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := Achievements{WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 || len(f.calls("/")) != 0 {
		t.Errorf("got %d points and %v, want nothing asked", len(points), err)
	}
}

// accountTransport is an http.RoundTripper made of a function, so a test can
// stand in for github.com without a network.
type accountTransport func(r *http.Request) (*http.Response, error)

func (fn accountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// accountBrokenBody is a response body that fails partway, as a connection
// dropped mid-page does.
type accountBrokenBody struct{}

func (accountBrokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// accountAnswer is a response of the status and body given.
func accountAnswer(r *http.Request, status int, body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:   io.NopCloser(body), Request: r,
	}
}

// The page is fetched with the client handed in when there is one, from
// github.com when no site is configured, and without the API token.
func TestAchievementsFetchWithTheClientGivenFromGitHubByDefault(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	page := recordedAchievements(t)
	var mu sync.Mutex
	var asked []string
	client := &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		asked = append(asked, r.URL.String())
		mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the API token traveled to the web page")
		}
		return accountAnswer(r, http.StatusOK, strings.NewReader(page)), nil
	})}
	points, err := Achievements{Login: "jmrplens", HTTP: client}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(only(t, points, "gh_achievement")) != 8 {
		t.Errorf("got %v, want the 8 badges of the page", measurements(points))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "https://github.com/jmrplens?tab=achievements" {
		t.Errorf("asked %q, want the achievements tab on github.com through the client given", asked)
	}
	p := find(t, points, "gh_achievement", map[string]string{"achievement": "yolo"})
	if p.Fields["url"] != "https://github.com/jmrplens?achievement=yolo&tab=achievements" {
		t.Errorf("url = %v", p.Fields["url"])
	}
}

// A page that cannot be asked for, that fails on the wire, that answers
// neither 200 nor 404, or that breaks off while it is read, is a failure and
// no points: none of them says the account has no badges.
func TestAchievementsFailOnAPageThatCannotBeRead(t *testing.T) {
	t.Parallel()
	for name, a := range map[string]Achievements{
		"a site url that does not parse": {Login: "jmrplens", WebBase: "http://[::1"},
		"a request that fails": {Login: "jmrplens", HTTP: &http.Client{Transport: accountTransport(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}},
		"a server error": {Login: "jmrplens", HTTP: &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
			return accountAnswer(r, http.StatusInternalServerError, strings.NewReader("oops")), nil
		})}},
		"a body that breaks off": {Login: "jmrplens", HTTP: &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
			return accountAnswer(r, http.StatusOK, accountBrokenBody{}), nil
		})}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			points, err := a.Collect(ctx(t), f.Client, testNow)
			if err == nil || isSkippable(err) || IsMarkupError(err) || len(points) != 0 {
				t.Errorf("got %d points and %v, want a failure that is neither skippable nor a markup change", len(points), err)
			}
		})
	}
}

// accountCard returns the recorded page with fn applied to the card of one
// badge only, failing when the card is not there or fn changed nothing.
func accountCard(t *testing.T, slug string, fn func(card string) string) string {
	t.Helper()
	page := recordedAchievements(t)
	for _, span := range achievementCard.FindAllStringIndex(page, -1) {
		card := page[span[0]:span[1]]
		if !strings.Contains(card, `data-achievement-slug="`+slug+`"`) {
			continue
		}
		changed := fn(card)
		if changed == card {
			t.Fatalf("the rewrite of the %s card changed nothing", slug)
		}
		return page[:span[0]] + changed + page[span[1]:]
	}
	t.Fatalf("no %s card on the recorded page", slug)
	return ""
}

// Every part of a card is held to the others, and each refusal names the
// badge and what about it moved, so an update to the parser starts from the
// right place.
func TestAchievementCardRefusalsNameTheBadgeAndThePart(t *testing.T) {
	t.Parallel()
	replace := func(old, repl string) func(string) string {
		return func(card string) string { return strings.ReplaceAll(card, old, repl) }
	}
	cases := map[string]struct {
		slug   string
		change func(string) string
		reason string
	}{
		"an empty slug":                 {"yolo", replace(`data-achievement-slug="yolo"`, `data-achievement-slug=""`), "a card with an empty slug"},
		"the same card twice":           {"yolo", func(card string) string { return card + "\n" + card }, "badge yolo listed twice"},
		"no badge image":                {"yolo", replace(`alt="Achievement: YOLO"`, `alt="YOLO"`), "badge yolo: no badge image with a tier in its name"},
		"an image of another badge":     {"yolo", replace("assets/yolo-default-", "assets/quickdraw-default-"), "badge yolo: image is for quickdraw"},
		"no heading":                    {"yolo", replace(`<h3 class="f4 ws-normal">YOLO</h3>`, `<h4 class="f4 ws-normal">YOLO</h4>`), "badge yolo: no heading, or one that disagrees with the image"},
		"no detail dialog":              {"yolo", replace("<details-dialog", "<details-modal"), "badge yolo: no detail dialog for this login and slug"},
		"a dialog for another badge":    {"yolo", replace("/achievements/yolo/detail", "/achievements/quickdraw/detail"), "badge yolo: no detail dialog for this login and slug"},
		"a tier label of x1":            {"pair-extraordinaire", replace(`ml-2 tmp-ml-2">x3</span>`, `ml-2 tmp-ml-2">x1</span>`), "badge pair-extraordinaire: tier label x1"},
		"a tier label past any integer": {"pair-extraordinaire", replace(`ml-2 tmp-ml-2">x3</span>`, `ml-2 tmp-ml-2">x99999999999999999999</span>`), "badge pair-extraordinaire: tier label x99999999999999999999"},
		"a tiered image with no label":  {"yolo", replace("assets/yolo-default-", "assets/yolo-silver-"), "badge yolo: a silver image with no tier label"},
		"a label that disagrees":        {"pair-extraordinaire", replace("achievement-tier-label--silver", "achievement-tier-label--gold"), "badge pair-extraordinaire: label says gold, image says silver"},
	}
	for name, tc := range cases {
		page := accountCard(t, tc.slug, tc.change)
		earned, err := parseAchievements(page, "jmrplens")
		if !IsMarkupError(err) {
			t.Errorf("%s: got %d badges and %v, want a MarkupError", name, len(earned), err)
			continue
		}
		if me, _ := errors.AsType[*MarkupError](err); me.Reason != tc.reason {
			t.Errorf("%s: reason %q, want %q", name, me.Reason, tc.reason)
		}
	}
}

// A social link has to be an absolute https url with a host: plain http, a
// url that does not parse and a scheme with no host are all a vcard this
// parser no longer recognizes.
func TestVcardRefusesALinkThatIsNotAnAbsoluteHTTPSURL(t *testing.T) {
	t.Parallel()
	page := recordedAchievements(t)
	const orcid = `href="https://orcid.org/0000-0003-1250-6212">`
	for name, link := range map[string]string{
		"plain http":            `href="http://orcid.org/0000-0003-1250-6212">`,
		"an invalid escape":     `href="https://orcid.org/%zz">`,
		"a scheme with no host": `href="https:///0000-0003-1250-6212">`,
	} {
		changed := strings.Replace(page, orcid, link, 1)
		if changed == page {
			t.Fatalf("%s: the ORCID link is not where the test expects it", name)
		}
		if socials, err := parseVcardSocials(changed, "jmrplens"); !IsMarkupError(err) {
			t.Errorf("%s: got %v and %v, want a MarkupError", name, socials, err)
		}
	}
}

// accountProgressServer serves the recorded page, a counts answer for an
// account created on the day of the sweep, so the walk is that one day, and
// the walk page given.
func accountProgressServer(t *testing.T, walkPage string) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return recordedAchievements(t) })
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, "query achievementCounts(") {
			_, _ = w.Write([]byte(`{"data":{"pulls":{"issueCount":1847},"answers":{"discussionCount":6},"user":{"createdAt":"2026-09-08T09:00:00Z","repositories":{"nodes":[{"stargazerCount":113}]}}}}`))
			return
		}
		_, _ = w.Write([]byte(walkPage))
	})
	return f
}

// The count is a floor when either of two things happened, and each alone is
// enough to say so: one day held more than the search will page, or a pull
// request had commits past the page that was read.
func TestAchievementsWarnWhenTheCoauthoredCountIsAFloor(t *testing.T) {
	t.Parallel()
	for name, page := range map[string]string{
		"a day over the search cap": `{"data":{"search":{"issueCount":1500,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
			`{"mergeCommit":{"message":"Solo"},"commits":{"totalCount":1,"nodes":[{"commit":{"message":"work"}}]}}]}}}`,
		"a pull request with unread commits": `{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
			`{"mergeCommit":{"message":"Big"},"commits":{"totalCount":140,"nodes":[{"commit":{"message":"work"}}]}}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := accountProgressServer(t, page)
			var warned []string
			a := Achievements{Login: "jmrplens", WebBase: f.srv.URL, Warn: func(msg string, _ ...any) { warned = append(warned, msg) }}
			points, err := a.Collect(ctx(t), f.Client, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(warned) != 1 || warned[0] != "co-authored pull request count is a floor" {
				t.Errorf("warned %q, want the floor said once", warned)
			}
			if n := len(only(t, points, "gh_achievement_progress")); n != 4 {
				t.Errorf("got %d progress rows, want the 4 a floor still writes", n)
			}
		})
	}
}

// An account with no repository of its own has no most starred one, and its
// Starstruck count is zero rather than a panic.
func TestAchievementCountsWithNoRepositoryReadNoStars(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{"pulls":{"issueCount":3},"answers":{"discussionCount":1},"user":{"createdAt":"2020-01-01T00:00:00Z","repositories":{"nodes":[]}}}}`))
	})
	counts, err := Achievements{Login: "o"}.counts(ctx(t), f.Client)
	if err != nil {
		t.Fatal(err)
	}
	if counts.TopStars != 0 || counts.PullsMerged != 3 || counts.Answers != 1 {
		t.Errorf("counts = %+v", counts)
	}
}

// Only the gateway giving up is answered with a smaller page. Any other
// refusal is handed up on the first query: asking again at half the page
// would be refused the same way.
func TestCoauthoredWalkDoesNotShrinkThePageForAnyOtherRefusal(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	})
	w := coauthoredWalk{login: "o"}
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, day, day.AddDate(0, 1, 0)); err == nil {
		t.Fatal("the refusal was not handed up")
	}
	if n := len(f.calls("/graphql")); n != 1 || w.queries != 1 {
		t.Errorf("%d queries, want the one that was refused", n)
	}
}

// The page is halved down to ten and no further: a gateway that refuses
// every size is a failure after the fifth query, not a loop.
func TestCoauthoredWalkGivesUpBelowAPageOfTen(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var mu sync.Mutex
	var firsts []any
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		mu.Lock()
		firsts = append(firsts, vars["first"])
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502</html>"))
	})
	w := coauthoredWalk{login: "o"}
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	err := w.walk(ctx(t), f.Client, day, day)
	if _, tooLarge := errors.AsType[*ghapi.TooLargeError](err); !tooLarge {
		t.Fatalf("err = %v, want the gateway's refusal handed up", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(firsts) != "[100 50 25 12 6]" {
		t.Errorf("page sizes asked = %v", firsts)
	}
}

// Two days over the cap are still two days, and a range of two splits into
// one day each rather than being taken as a floor.
func TestCoauthoredWalkSplitsARangeOfTwoDays(t *testing.T) {
	t.Parallel()
	f, asked := walkServer(t, map[string]walkPage{
		"2020-01-01..2020-01-02#100": {issueCount: 1500, plain: 1, next: "x"},
		"2020-01-01..2020-01-01#100": {issueCount: 5, coauthored: 1, plain: 4},
		"2020-01-02..2020-01-02#100": {issueCount: 5, coauthored: 2, plain: 3},
	})
	w := coauthoredWalk{login: "o"}
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, day, day.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if w.pulls != 3 || w.capped || len(asked()) != 3 {
		t.Errorf("pulls = %d, capped = %t, asked %q, want 3 from the two days and no floor", w.pulls, w.capped, asked())
	}
}

// A half that fails ends the walk: the other half is not asked, and the
// failure is what comes back.
func TestCoauthoredWalkStopsWhenTheFirstHalfFails(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var mu sync.Mutex
	var ranges []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, vars map[string]any) {
		q, _ := vars["query"].(string)
		key := mergedRange.FindStringSubmatch(q)[1]
		mu.Lock()
		ranges = append(ranges, key)
		mu.Unlock()
		if key == "2020-01-01..2020-01-10" {
			_, _ = w.Write([]byte(walkPage{issueCount: 1500, plain: 1, next: "x"}.body()))
			return
		}
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	})
	w := coauthoredWalk{login: "o"}
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := w.walk(ctx(t), f.Client, from, from.AddDate(0, 0, 9)); err == nil {
		t.Fatal("the failed half was not handed up")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(ranges, []string{"2020-01-01..2020-01-10", "2020-01-01..2020-01-05"}) {
		t.Errorf("asked %q, want the range and its first half only", ranges)
	}
}

// A pull request whose commits were all read, with no trailer among them, is
// not co-authored and is no floor either.
func TestCoauthoredCountIsNoFloorWhenEveryCommitWasRead(t *testing.T) {
	t.Parallel()
	var page coauthoredPage
	mustUnmarshal(t, []byte(`{"search":{"nodes":[{"mergeCommit":null,"commits":{"totalCount":2,"nodes":[{"commit":{"message":"a"}},{"commit":{"message":"b"}}]}}]}}`), &page)
	w := coauthoredWalk{}
	w.count(&page)
	if w.pulls != 0 || w.truncated != 0 {
		t.Errorf("pulls = %d, truncated = %d, want neither", w.pulls, w.truncated)
	}
}

// Each tiered badge is decided by its own count, and a slug with no rule has
// no count.
func TestAchievementRuleReadsItsOwnCount(t *testing.T) {
	t.Parallel()
	counts := achievementCounts{PullsMerged: 1, Answers: 2, TopStars: 3, Coauthored: 4}
	for slug, want := range map[string]int{
		"pull-shark": 1, "galaxy-brain": 2, "starstruck": 3, "pair-extraordinaire": 4, "yolo": 0,
	} {
		if got := (achievementRule{Slug: slug}).count(counts); got != want {
			t.Errorf("%s count = %d, want %d", slug, got, want)
		}
	}
}

// accountPackagePages serves the package listings: for each registry named,
// the sizes of its pages by number, and an empty list everywhere else; a
// negative size answers that page with the status it names.
func accountPackagePages(f *fixtureServer, pages map[string]map[string]int) {
	f.handle("/user/packages", func(w http.ResponseWriter, r *http.Request) {
		n := pages[r.URL.Query().Get("package_type")][r.URL.Query().Get("page")]
		switch n {
		case -http.StatusNotFound:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case -http.StatusInternalServerError:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		case -http.StatusUnprocessableEntity:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"pagination is limited for this resource"}`))
		default:
			rows := make([]map[string]string, n)
			for i := range rows {
				rows[i] = map[string]string{"name": fmt.Sprintf("p%d", i)}
			}
			_ = json.NewEncoder(w).Encode(rows)
		}
	})
}

// The package count pages each registry a hundred at a time up to ten pages,
// goes on to the next registry past one it cannot list, stops at a page
// GitHub will not serve, and on a broken listing keeps the larger of what it
// counted and what GraphQL said.
func TestPackageCountPagesEveryRegistry(t *testing.T) {
	t.Parallel()
	tenFull := map[string]int{}
	for p := 1; p <= 12; p++ {
		tenFull[strconv.Itoa(p)] = 100
	}
	for _, tc := range []struct {
		name    string
		pages   map[string]map[string]int
		graphql int
		want    int
		asked   int
	}{
		{"ten full pages", map[string]map[string]int{"container": tenFull}, 0, 1000, 10 + 5},
		{"a registry it cannot list", map[string]map[string]int{"container": {"1": -404}, "npm": {"1": 3}}, 0, 3, 6},
		{"a page past the limit", map[string]map[string]int{"container": {"1": 100, "2": -422}, "npm": {"1": 2}}, 0, 102, 7},
		{"a broken listing under GraphQL's count", map[string]map[string]int{"container": {"1": 3}, "npm": {"1": -500}}, 5, 5, 2},
		{"a broken listing over GraphQL's count", map[string]map[string]int{"container": {"1": 3}, "npm": {"1": -500}}, 1, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			accountPackagePages(f, tc.pages)
			u := &accountUser{}
			u.Packages.TotalCount = tc.graphql
			if got := packageCount(ctx(t), f.Client, u); got != tc.want {
				t.Errorf("packageCount = %d, want %d", got, tc.want)
			}
			if n := len(f.calls("/user/packages")); n != tc.asked {
				t.Errorf("%d listing requests, want %d", n, tc.asked)
			}
		})
	}
}

// accountUserFrom decodes an account answer the way the query's is decoded.
func accountUserFrom(t *testing.T, body string) *accountUser {
	t.Helper()
	var u accountUser
	mustUnmarshal(t, []byte(body), &u)
	return &u
}

// The headline row carries the account's age in whole days: created
// 2017-05-25 21:07.
func TestAccountRowCountsTheDaysSinceTheAccountWasMade(t *testing.T) {
	t.Parallel()
	u := accountUserFrom(t, `{"login":"octocat","createdAt":"2017-05-25T21:07:25Z"}`)
	p := accountPoint(u, 0, 0, map[string]string{"user": "octocat"}, testNow)
	if got := fieldInt(t, p, "account_age_days"); got != 3392 {
		t.Errorf("account_age_days = %d, want 3392", got)
	}
}

// The availability status is a flag only when the profile sets one, and its
// age only when GitHub says when it was set.
func TestProfileFlagsCarryTheStatusOnlyWhenThereIsOne(t *testing.T) {
	t.Parallel()
	base := map[string]string{"user": "octocat"}
	limited := func(points []sink.Point) (sink.Point, bool) {
		for _, p := range points {
			if p.Tags["flag"] == "limited_availability" {
				return p, true
			}
		}
		return sink.Point{}, false
	}

	points := profileFlagPoints(accountUserFrom(t, `{"login":"octocat"}`), base, testNow)
	if _, ok := limited(points); ok || len(points) != 7 {
		t.Errorf("no status: got %d rows, want the seven flags and no availability row", len(points))
	}

	points = profileFlagPoints(accountUserFrom(t, `{"login":"octocat","status":{"message":"away","createdAt":"2026-08-01T09:00:00Z","indicatesLimitedAvailability":true}}`), base, testNow)
	st, ok := limited(points)
	if !ok || fieldInt(t, st, "age_days") != 38 || st.Fields["enabled"] != true || st.Fields["message"] != "away" {
		t.Errorf("status set on 2026-08-01 = %v, want enabled, the message and 38 days", st.Fields)
	}

	points = profileFlagPoints(accountUserFrom(t, `{"login":"octocat","status":{"message":"away"}}`), base, testNow)
	if st, ok = limited(points); !ok || hasField(st, "age_days") {
		t.Errorf("a status with no date = %v, want the row with no age", st.Fields)
	}
}

// The listing row and its tiers carry ages in whole days where GitHub gives a
// date, and leave out what the listing does not have: no payout date, no goal,
// a tier with no name, a tier with no date and a tier nobody retired.
func TestSponsorsListingWritesOnlyWhatTheListingHas(t *testing.T) {
	t.Parallel()
	base := map[string]string{"user": "octocat"}
	u := accountUserFrom(t, `{"login":"octocat","hasSponsorsListing":true,"sponsorsListing":{
		"name":"sponsors-octocat","isPublic":true,"createdAt":"2021-08-31T12:00:00Z","nextPayoutDate":"","activeGoal":null,
		"tiers":{"totalCount":3,"nodes":[
			{"name":"Coffee","monthlyPriceInCents":500,"createdAt":"2021-08-31T12:00:00Z","adminInfo":null},
			{"name":""},
			{"name":"Lunch","monthlyPriceInCents":1500,"adminInfo":{"isRetired":true}}]}}}`)
	points := sponsorsListingPoints(u, base, testNow)
	listing := only(t, points, "gh_sponsors_listing")[0]
	if fieldInt(t, listing, "listing_age_days") != 1834 {
		t.Errorf("listing_age_days = %v, want 1834", listing.Fields["listing_age_days"])
	}
	for _, absent := range []string{"next_payout_date", "goal_kind", "goal_title"} {
		if hasField(listing, absent) {
			t.Errorf("listing carries %s it does not have: %v", absent, listing.Fields)
		}
	}
	tiers := only(t, points, "gh_sponsors_tier")
	if len(tiers) != 2 {
		t.Fatalf("got %d tiers, want the two with a name", len(tiers))
	}
	coffee := find(t, points, "gh_sponsors_tier", map[string]string{"tier": "Coffee"})
	if fieldInt(t, coffee, "age_days") != 1834 || hasField(coffee, "retired") {
		t.Errorf("Coffee = %v, want 1834 days and no retired field", coffee.Fields)
	}
	lunch := find(t, points, "gh_sponsors_tier", map[string]string{"tier": "Lunch"})
	if hasField(lunch, "age_days") || lunch.Fields["retired"] != true {
		t.Errorf("Lunch = %v, want no age and retired", lunch.Fields)
	}

	points = sponsorsListingPoints(accountUserFrom(t, `{"login":"octocat","sponsorsListing":{"name":"new"}}`), base, testNow)
	if hasField(points[0], "listing_age_days") || points[0].Fields["listing_name"] != "new" {
		t.Errorf("a listing with no date = %v, want its name and no age", points[0].Fields)
	}

	points = sponsorsListingPoints(accountUserFrom(t, `{"login":"octocat"}`), base, testNow)
	if len(points) != 1 || hasField(points[0], "listing_name") || points[0].Fields["has_listing"] != false {
		t.Errorf("no listing: got %v, want the one row saying so", points)
	}
}

// A list without a slug has no identity and is left out; one without dates
// is written without the ages it cannot compute.
func TestStarListsNeedASlugAndWriteOnlyTheAgesTheyHave(t *testing.T) {
	t.Parallel()
	u := accountUserFrom(t, `{"login":"octocat","lists":{"totalCount":2,"nodes":[{"name":"nameless","slug":""},{"name":"Tools","slug":"tools","items":{"totalCount":4}}]}}`)
	points := starListPoints(u, map[string]string{"user": "octocat"}, testNow)
	if len(points) != 1 || points[0].Tags["list"] != "tools" || fieldInt(t, points[0], "items") != 4 {
		t.Fatalf("got %v, want the tools list alone", points)
	}
	if hasField(points[0], "age_days") || hasField(points[0], "days_since_add") {
		t.Errorf("a list with no dates = %v", points[0].Fields)
	}
}

// The other party is the first side with a login, and a sponsorship with none
// is private; one with no creation date has nothing to be stamped at.
func TestSponsorshipsNameTheOtherPartyAndNeedADate(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]string{
		`{"sponsorable":{"login":""},"sponsorEntity":{"login":"bob"}}`: "bob",
		`{"sponsorable":{"login":""},"sponsorEntity":null}`:            "private",
		`{"sponsorable":{"login":"alice"}}`:                            "alice",
	} {
		var s sponsorship
		mustUnmarshal(t, []byte(body), &s)
		if got := s.other(); got != want {
			t.Errorf("%s: other = %q, want %q", body, got, want)
		}
	}
	u := accountUserFrom(t, `{"login":"octocat","sponsorshipsAsSponsor":{"nodes":[{"sponsorable":{"login":"alice"}},{"createdAt":"2021-08-31T12:00:00Z","sponsorable":{"login":"bob"}}]}}`)
	points := sponsorshipPoints(u, map[string]string{"user": "octocat"})
	if len(points) != 1 || points[0].Tags["sponsorable"] != "bob" {
		t.Errorf("got %v, want bob's sponsorship alone", points)
	}
}

// Rows that have nothing to stand on are left out and their neighbors are
// not: a calendar day or a daily commit row with a date that does not read, a
// repository with no name, and a pin with no name. A pinned repository never
// pushed has no age since its push.
func TestAccountRowsLeaveOutWhatHasNothingToStandOn(t *testing.T) {
	t.Parallel()
	base := map[string]string{"user": "octocat"}
	u := accountUserFrom(t, `{"login":"octocat",
		"pinnedItems":{"nodes":[{"__typename":"Repository","nameWithOwner":""},{"__typename":"Repository","nameWithOwner":"octocat/new","stargazerCount":1}]},
		"contributionsCollection":{
			"contributionCalendar":{"weeks":[{"contributionDays":[{"date":"someday","contributionCount":1},{"date":"2026-09-01","contributionCount":2}]}]},
			"commitContributionsByRepository":[
				{"repository":{"nameWithOwner":""},"contributions":{"nodes":[{"occurredAt":"2026-09-01T07:00:00Z","commitCount":1}]}},
				{"repository":{"nameWithOwner":"octocat/new"},"contributions":{"nodes":[{"occurredAt":"yesterday","commitCount":1},{"occurredAt":"2026-09-02T07:00:00Z","commitCount":3}]}}]}}`)

	if days := contributionDayPoints(u, base); len(days) != 1 || fieldInt(t, days[0], "contributions") != 2 {
		t.Errorf("calendar = %v, want the one day that reads", days)
	}
	byRepo := commitDayPoints(u.Contributions.ByRepository, "octocat")
	if len(byRepo) != 1 || byRepo[0].Tags["full_name"] != "octocat/new" || fieldInt(t, byRepo[0], "commits") != 3 {
		t.Errorf("daily commits = %v, want the one named repository's one dated row", byRepo)
	}
	pins := pinnedItemPoints(u, base, testNow)
	if len(pins) != 1 || pins[0].Tags["full_name"] != "octocat/new" || hasField(pins[0], "days_since_push") {
		t.Errorf("pins = %v, want the named pin with no push age", pins)
	}
}

// accountHistoryServer answers the history family: the creation probe with
// the answer given, and each year by its range, 2025 with one day that reads
// and one that does not, 2026 refused.
func accountHistoryServer(t *testing.T, probe string) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		switch {
		case !strings.Contains(query, "contributionsCollection(from:"):
			_, _ = w.Write([]byte(probe))
		case vars["from"] == "2025-01-01T00:00:00Z":
			_, _ = w.Write([]byte(`{"data":{"user":{"contributionsCollection":{"totalCommitContributions":4,` +
				`"contributionCalendar":{"totalContributions":4,"weeks":[{"contributionDays":[{"date":"2025-03-01","contributionCount":4},{"date":"March","contributionCount":1}]}]}}}}}`))
		default:
			_, _ = w.Write([]byte(accountInternalError))
		}
	})
	return f
}

// A backfill walks forward from the year the account was created, hands up a
// year that failed with the years before it, and leaves out a day whose date
// does not read; a probe that fails is a failure before any year is asked.
func TestHistoryKeepsTheYearsReadBeforeOneFailed(t *testing.T) {
	t.Parallel()
	f := accountHistoryServer(t, accountInternalError)
	points, err := History{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err == nil || len(points) != 0 || len(f.calls("/graphql")) != 1 {
		t.Errorf("a failed probe: got %d points and %v after %d queries, want the failure alone", len(points), err, len(f.calls("/graphql")))
	}

	f = accountHistoryServer(t, `{"data":{"user":{"createdAt":"2025-02-01T00:00:00Z"}}}`)
	points, err = History{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Fatal("the failed year was not reported")
	}
	var froms []any
	for _, c := range f.calls("/graphql")[1:] {
		froms = append(froms, graphQLVars(t, c.Body)["from"])
	}
	if fmt.Sprint(froms) != "[2025-01-01T00:00:00Z 2026-01-01T00:00:00Z]" {
		t.Errorf("asked the years from %v, want 2025 and then 2026", froms)
	}
	year := find(t, points, "gh_contribution_year", map[string]string{"year": "2025"})
	if fieldInt(t, year, "commits") != 4 {
		t.Errorf("2025 = %v", year.Fields)
	}
	if days := only(t, points, "gh_contribution_day"); len(days) != 1 {
		t.Errorf("got %d days, want the one whose date reads", len(days))
	}
}
