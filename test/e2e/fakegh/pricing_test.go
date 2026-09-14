package fakegh

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
)

// The fake prices what it answers the way api.github.com does, so a suite
// driving the binary against it can say what a sweep cost and hold the
// number: a fixture carries one validator, a request that presents it is
// answered 304 and charged nothing, every other answer charges its bucket
// one, and every GraphQL answer carries the budget block with the running
// total. Before this the fake answered every repeat 200, the client's cache
// never earned a 304, and own_cost read zero on every point the suites wrote.

// fixtures is where this package's own tests read the suites' fixtures from.
const fixtures = "../testdata"

// reply is what one request came back with, read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func get(t *testing.T, s *Server, path, etag string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL()+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
}

func TestARepeatThatPresentsTheValidatorIs304AndFree(t *testing.T) {
	s := New(t, fixtures)
	const path = repoPath + "/traffic/views"

	first := get(t, s, path, "")
	if first.StatusCode != http.StatusOK || first.Header.Get("ETag") == "" {
		t.Fatalf("first answer: %d with ETag %q, want 200 with a validator", first.StatusCode, first.Header.Get("ETag"))
	}
	if first.Header.Get("x-ratelimit-used") != "1" || first.Header.Get("x-ratelimit-remaining") != "4999" {
		t.Errorf("the first answer charged used=%s remaining=%s, want one of the core budget",
			first.Header.Get("x-ratelimit-used"), first.Header.Get("x-ratelimit-remaining"))
	}

	second := get(t, s, path, first.Header.Get("ETag"))
	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("the repeat with the validator answered %d, want 304", second.StatusCode)
	}
	if len(second.Body) != 0 {
		t.Errorf("a 304 carried a body: %s", second.Body)
	}
	if second.Header.Get("x-ratelimit-used") != "1" {
		t.Errorf("the 304 moved used to %s: a 304 costs nothing", second.Header.Get("x-ratelimit-used"))
	}
	if second.Header.Get("ETag") != first.Header.Get("ETag") {
		t.Errorf("the 304 carries ETag %q, the 200 carried %q", second.Header.Get("ETag"), first.Header.Get("ETag"))
	}

	// The validator is the fixture's, so a stale one is a 200 again.
	third := get(t, s, path, `"somebody-else's"`)
	if third.StatusCode != http.StatusOK || third.Header.Get("x-ratelimit-used") != "2" {
		t.Errorf("a repeat with the wrong validator answered %d used=%s, want 200 charged",
			third.StatusCode, third.Header.Get("x-ratelimit-used"))
	}

	reqs := s.Requests()
	if len(reqs) != 3 {
		t.Fatalf("recorded %d requests", len(reqs))
	}
	want := []struct{ status, cost int }{{http.StatusOK, 1}, {http.StatusNotModified, 0}, {http.StatusOK, 1}}
	for i, w := range want {
		if reqs[i].Status != w.status || reqs[i].Cost != w.cost {
			t.Errorf("request %d recorded as %d costing %d, want %d costing %d", i, reqs[i].Status, reqs[i].Cost, w.status, w.cost)
		}
	}
}

func TestTheValidatorIsPerFixtureNotPerRendering(t *testing.T) {
	s := New(t, fixtures)
	// The runs fixture spells its dates "@NOW@", so two renderings differ by
	// the seconds between them. The validator must not: it names the file,
	// or every repeat of a tokenized fixture would be a 200.
	const path = repoPath + "/actions/runs"
	a := get(t, s, path, "")
	b := get(t, s, path, "")
	if a.Header.Get("ETag") == "" || a.Header.Get("ETag") != b.Header.Get("ETag") {
		t.Errorf("two renderings of one fixture carry validators %q and %q", a.Header.Get("ETag"), b.Header.Get("ETag"))
	}
	other := get(t, s, repoPath+"/actions/artifacts", "")
	if other.Header.Get("ETag") == a.Header.Get("ETag") {
		t.Errorf("two fixtures share the validator %q", a.Header.Get("ETag"))
	}
	if missing := get(t, s, "/no/such/route", ""); missing.Header.Get("ETag") != "" {
		t.Errorf("a 404 carries a validator %q; GitHub sends none, and a client that stored one would ask for it", missing.Header.Get("ETag"))
	}
}

func TestObjectStorageIsOffTheAPI(t *testing.T) {
	s := New(t, fixtures)
	// The job log is a redirect to object storage. The redirect is the API's
	// answer and is charged; where it lands is not api.github.com, carries
	// no rate headers and no validator, and costs nothing. A fake that
	// priced it would count a download the real budget never sees, and a
	// client that read a budget off it would read one that is not there.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL()+jobLogPath, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("x-ratelimit-used") != "1" {
		t.Errorf("the redirect answered %d used=%s, want 302 charged one", resp.StatusCode, resp.Header.Get("x-ratelimit-used"))
	}
	stored := get(t, s, resp.Header.Get("Location"), "")
	if stored.StatusCode != http.StatusOK || len(stored.Body) == 0 {
		t.Fatalf("storage answered %d with %d bytes", stored.StatusCode, len(stored.Body))
	}
	for _, h := range []string{"x-ratelimit-limit", "x-ratelimit-used", "x-ratelimit-remaining", "x-ratelimit-resource", "ETag"} {
		if v := stored.Header.Get(h); v != "" {
			t.Errorf("storage answered with %s: %q; it is off the API and carries none", h, v)
		}
	}
	if next := get(t, s, repoPath, ""); next.Header.Get("x-ratelimit-used") != "2" {
		t.Errorf("after the redirect, the download and one more call, core reports used=%s: the download must not have been charged", next.Header.Get("x-ratelimit-used"))
	}
	reqs := s.Requests()
	if len(reqs) != 3 || reqs[1].Cost != 0 || reqs[1].Path != resp.Header.Get("Location") {
		t.Errorf("recorded %+v, want the download recorded at no cost", reqs)
	}
}

func TestAGraphQLAnswerCarriesNoValidator(t *testing.T) {
	s := New(t, fixtures)
	payload, err := json.Marshal(map[string]any{"query": `query { repositoryDiscussionComments { x } }`})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL()+"/graphql", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if etag := resp.Header.Get("ETag"); etag != "" {
		t.Errorf("a GraphQL answer carries ETag %q; the gateway sends none and a POST is never conditional", etag)
	}
}

func TestSearchIsChargedToItsOwnBudget(t *testing.T) {
	s := New(t, fixtures)
	get(t, s, repoPath, "")
	resp := get(t, s, "/search/issues?q=x", "")
	if resp.Header.Get("x-ratelimit-resource") != "search" || resp.Header.Get("x-ratelimit-limit") != "30" ||
		resp.Header.Get("x-ratelimit-used") != "1" || resp.Header.Get("x-ratelimit-remaining") != "29" {
		t.Errorf("search answered resource=%s limit=%s used=%s remaining=%s: it has its own budget of thirty and core's spend is not its own",
			resp.Header.Get("x-ratelimit-resource"), resp.Header.Get("x-ratelimit-limit"),
			resp.Header.Get("x-ratelimit-used"), resp.Header.Get("x-ratelimit-remaining"))
	}
}

// post sends one GraphQL query and decodes the budget block out of its data.
func post(t *testing.T, s *Server, query string) (status int, block map[string]any, data map[string]json.RawMessage) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL()+"/graphql", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if raw, ok := env.Data[ghapi.RateLimitAlias]; ok {
		if err = json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, block, env.Data
}

func TestEveryGraphQLAnswerCarriesThePricedBudgetBlock(t *testing.T) {
	s := New(t, fixtures)
	// What the client sends: the collector's own selection with the block
	// added at the root, under the alias.
	query := "query { " + ghapi.RateLimitAlias + ": rateLimit { limit cost used remaining resetAt } " +
		`repository(owner: "octocat", name: "hello-world") { ...totals } } fragment totals on Repository { name }`
	status, block, data := post(t, s, query)
	if status != http.StatusOK || block == nil {
		t.Fatalf("answered %d with no budget block among %d keys", status, len(data))
	}
	if block["cost"] != float64(1) || block["used"] != float64(1) || block["remaining"] != float64(4999) || block["limit"] != float64(5000) {
		t.Errorf("the first query is priced %v, want cost 1, used 1, remaining 4999 of 5000", block)
	}
	if _, kept := data["r0"]; !kept {
		t.Errorf("the block displaced the fixture's own data: %v", keysOf(data))
	}
	post(t, s, query)
	if _, block, _ = post(t, s, query); block == nil || block["used"] != float64(3) {
		t.Errorf("the third query reports %v, want the block with the running total of 3", block)
	}
}

func TestTheBudgetProbeReportsButIsNotCharged(t *testing.T) {
	s := New(t, fixtures)
	if _, block, _ := post(t, s, ghapi.BudgetQuery); block == nil || block["used"] != float64(0) {
		t.Errorf("the probe answered %v, want the block with nothing spent yet", block)
	}
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Cost != 0 {
		t.Errorf("the probe was recorded as %+v, want a request that cost nothing", reqs)
	}
	// It still reports what the charged queries spent.
	post(t, s, "query { "+ghapi.RateLimitAlias+": rateLimit { cost } repositoryDiscussionComments { x } }")
	if _, block, _ := post(t, s, ghapi.BudgetQuery); block["used"] != float64(1) {
		t.Errorf("after one charged query the probe reports used %v", block["used"])
	}
}

func TestAQueryWithoutTheAliasGetsNoBlock(t *testing.T) {
	s := New(t, fixtures)
	// A query that did not ask for the block must not be answered with one
	// under a key it did not select, which is how the real gateway behaves.
	_, block, data := post(t, s, `query { repositoryDiscussionComments { x } }`)
	if block != nil {
		t.Errorf("a query that did not select the alias was answered with %v", block)
	}
	if len(data) == 0 {
		t.Error("the fixture's data was lost")
	}
	if s.Requests()[0].Cost != 1 {
		t.Error("an ordinary query costs one whether or not it asked for the block")
	}
}

func keysOf(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}
