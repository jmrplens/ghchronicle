package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a client pointed at a server driven by h.
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("test-token", 5*time.Second)
	c.SetBaseURL(srv.URL)
	return c, srv
}

func rateHeaders(w http.ResponseWriter, resource string, limit, remaining int, reset time.Time) {
	w.Header().Set("x-ratelimit-limit", strconv.Itoa(limit))
	w.Header().Set("x-ratelimit-remaining", strconv.Itoa(remaining))
	w.Header().Set("x-ratelimit-resource", resource)
	if !reset.IsZero() {
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(reset.Unix(), 10))
	}
}

func TestSetBaseURL(t *testing.T) {
	t.Parallel()
	c := New("t", 0)
	if c.base != defaultBase {
		t.Errorf("default base = %q", c.base)
	}
	c.SetBaseURL("https://ghe.example.com/api/v3/")
	if c.base != "https://ghe.example.com/api/v3" {
		t.Errorf("trailing slash must be dropped, got %q", c.base)
	}
	c.SetBaseURL("")
	if c.base != "https://ghe.example.com/api/v3" {
		t.Errorf("an empty base must not reset the client, got %q", c.base)
	}
	if New("t", 0).http.Timeout != 30*time.Second {
		t.Error("a zero timeout means 30s")
	}
}

// foreignTransport is a RoundTripper that is not an *http.Transport, the shape
// an instrumented or mocked default takes.
type foreignTransport struct{ next http.RoundTripper }

func (f foreignTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.next.RoundTrip(r)
}

// TestNewSurvivesAForeignDefaultTransport covers a process that has replaced
// http.DefaultTransport with something that is not an *http.Transport. New
// used to assert the type and panic there; it now builds a plain transport of
// its own, still separate from the default, and the client works.
//
// Not parallel: it swaps a package variable of net/http that every parallel
// test in this package reads through New.
func TestNewSurvivesAForeignDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = foreignTransport{next: original}

	c := New("test-token", 5*time.Second)
	own, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want an *http.Transport of the client's own", c.http.Transport)
	}
	if own.Proxy == nil {
		t.Error("the fallback transport must still honor the proxy variables")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(srv.Close)
	c.SetBaseURL(srv.URL)
	var got struct {
		Login string `json:"login"`
	}
	if _, _, err := c.GetJSON(t.Context(), "/user", &got, ""); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got.Login != "octocat" {
		t.Errorf("login = %q, want octocat", got.Login)
	}
}

// TestCacheTreatsAForeignElementAsEmpty covers the branch pairAt exists for.
// Nothing but put adds to the recency order, so this cannot happen today; if
// it ever does, the cache must answer as if nothing were stored, which costs
// one unconditional request, instead of panicking in the middle of a sweep.
func TestCacheTreatsAForeignElementAsEmpty(t *testing.T) {
	t.Parallel()
	k := newCache(1 << 20)
	odd, gone, stray := cacheKey{url: "/odd"}, cacheKey{url: "/gone"}, cacheKey{url: "/stray"}
	k.entries[odd] = k.order.PushFront("not a pair")

	if etag, body := k.get(odd); etag != "" || body != nil {
		t.Errorf("get = %q, %q, want nothing stored", etag, body)
	}
	k.put(odd, `"v1"`, []byte("{}"))
	if etag, _ := k.get(odd); etag != `"v1"` {
		t.Errorf("a put over the foreign element must store the pair, got etag %q", etag)
	}

	k.entries[gone] = k.order.PushBack(42)
	k.drop(gone)
	if _, ok := k.entries[gone]; ok {
		t.Error("drop must forget the URL whatever its element held")
	}

	k.entries[stray] = k.order.PushBack(struct{}{})
	before := k.order.Len()
	k.evict()
	if k.order.Len() != before-1 {
		t.Error("evict must remove the least recently used element whatever it held")
	}
	if etag, _ := k.get(odd); etag != `"v1"` {
		t.Error("evicting the foreign element must leave the real pair alone")
	}
	// The index must not keep pointing at the element evict removed: a put
	// for that URL would then land outside the recency order, counted in the
	// bytes and out of reach of every later eviction.
	if _, indexed := k.entries[stray]; indexed {
		t.Error("evict must forget the URL of the element it removed whatever it held")
	}
	k.put(stray, `"v2"`, []byte("{}"))
	if k.order.Len() != before {
		t.Errorf("a put after the eviction must join the recency order, order has %d, want %d", k.order.Len(), before)
	}
}

func TestGetJSONSendsTheGitHubHeaders(t *testing.T) {
	t.Parallel()
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	var out struct {
		OK bool `json:"ok"`
	}
	if _, _, err := c.GetJSON(context.Background(), "/user", &out, ""); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Error("body not unmarshaled")
	}
	want := map[string]string{
		"Authorization":        "Bearer test-token",
		"User-Agent":           userAgent,
		"X-Github-Api-Version": "2022-11-28",
		"Accept":               "application/vnd.github+json",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
	// A custom media type replaces the default.
	if _, _, err := c.GetJSON(context.Background(), "/user", &out, "application/vnd.github.star+json"); err != nil {
		t.Fatal(err)
	}
	if got.Get("Accept") != "application/vnd.github.star+json" {
		t.Errorf("Accept = %q", got.Get("Accept"))
	}
}

func TestETagRoundTrip(t *testing.T) {
	t.Parallel()
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("ETag", `W/"abc123"`)
			w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
			rateHeaders(w, "core", 5000, 4999, time.Time{})
			_, _ = w.Write([]byte(`{"stars": 42}`))
			return
		}
		if got := r.Header.Get("If-None-Match"); got != `W/"abc123"` {
			t.Errorf("second request If-None-Match = %q, want the stored ETag", got)
		}
		// A 304 spends no quota, and GitHub still reports the budget on it.
		rateHeaders(w, "core", 5000, 4999, time.Time{})
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
		w.WriteHeader(http.StatusNotModified)
	})

	var first, second struct {
		Stars int `json:"stars"`
	}
	link, cached, err := c.GetJSON(context.Background(), "/repos/o/n", &first, "")
	if err != nil {
		t.Fatal(err)
	}
	if cached || first.Stars != 42 || !strings.Contains(link, `rel="next"`) {
		t.Errorf("first call: cached=%v stars=%d link=%q", cached, first.Stars, link)
	}

	// Change the remaining budget on the 304 so the assertion can tell the
	// two responses apart.
	link, cached, err = c.GetJSON(context.Background(), "/repos/o/n", &second, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cached {
		t.Error("a 304 must be reported as cached")
	}
	if second.Stars != 42 {
		t.Errorf("the stored body must be replayed on a 304, got %+v", second)
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Errorf("the 304's Link header must be returned, got %q", link)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("made %d requests", calls)
	}
	if r, ok := c.RateFor("core"); !ok || r.Remaining != 4999 || r.Limit != 5000 {
		t.Errorf("rate after the 304 = %+v %v, the 304's headers must be read", r, ok)
	}
}

func TestETagRateHeadersOnThe304AreRead(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"v1"`)
			rateHeaders(w, "core", 5000, 4000, time.Time{})
			_, _ = w.Write([]byte(`[1,2,3]`))
			return
		}
		rateHeaders(w, "core", 5000, 3999, time.Unix(1_800_000_000, 0))
		w.WriteHeader(http.StatusNotModified)
	})
	var out []int
	for range 2 {
		if _, _, err := c.GetJSON(context.Background(), "/things", &out, ""); err != nil {
			t.Fatal(err)
		}
	}
	r := c.Rate()
	if r.Remaining != 3999 || !r.Reset.Equal(time.Unix(1_800_000_000, 0)) || r.Resource != "core" {
		t.Errorf("rate = %+v, want the 304's figures", r)
	}
	if len(out) != 3 {
		t.Errorf("replayed body = %v", out)
	}
}

func TestETagIsPerURL(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	var mu sync.Mutex
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.RequestURI()] = r.Header.Get("If-None-Match")
		mu.Unlock()
		w.Header().Set("ETag", `"`+r.URL.Path+`"`)
		_, _ = w.Write([]byte(`{}`))
	})
	var out map[string]any
	for _, p := range []string{"/a", "/b", "/a", "/a?page=2"} {
		if _, _, err := c.GetJSON(context.Background(), p, &out, ""); err != nil {
			t.Fatal(err)
		}
	}
	if seen["/a"] != `"/a"` {
		t.Errorf("/a repeat sent If-None-Match %q", seen["/a"])
	}
	if seen["/b"] != "" || seen["/a?page=2"] != "" {
		t.Errorf("a different URL must not reuse another's ETag: %v", seen)
	}
}

func TestNotModifiedWithoutAStoredBody(t *testing.T) {
	t.Parallel()
	// A 304 the client did not earn (nothing was stored) still reports
	// cached and leaves out untouched rather than failing.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	out := map[string]int{"keep": 1}
	_, cached, err := c.GetJSON(context.Background(), "/x", &out, "")
	if err != nil || !cached || out["keep"] != 1 {
		t.Errorf("err=%v cached=%v out=%v", err, cached, out)
	}
}

func TestRateLimitsAreTrackedPerBucket(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			rateHeaders(w, "search", 30, 29, time.Time{})
		case r.URL.Path == "/graphql":
			w.Header().Set("Content-Type", "application/json")
			rateHeaders(w, "graphql", 5000, 4990, time.Time{})
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		case r.URL.Path == "/nobucket":
			// No resource header: the budget belongs to core.
			w.Header().Set("x-ratelimit-limit", "5000")
			w.Header().Set("x-ratelimit-remaining", "4000")
		default:
			rateHeaders(w, "core", 5000, 4500, time.Time{})
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	var out map[string]any
	if _, _, err := c.GetJSON(ctx, "/repos/o/n", &out, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetJSON(ctx, "/search/issues?q=x", &out, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.GraphQL(ctx, "{}", nil, nil); err != nil {
		t.Fatal(err)
	}

	// One search must not make the sweep believe core has 29 left.
	for _, want := range []RateState{
		{Resource: "core", Limit: 5000, Remaining: 4500},
		{Resource: "search", Limit: 30, Remaining: 29},
		{Resource: "graphql", Limit: 5000, Remaining: 4990},
	} {
		got, ok := c.RateFor(want.Resource)
		if !ok || got.Resource != want.Resource || got.Limit != want.Limit || got.Remaining != want.Remaining {
			t.Errorf("%s = %+v %v, want %+v", want.Resource, got, ok, want)
		}
	}
	if _, ok := c.RateFor("integration_manifest"); ok {
		t.Error("a bucket never charged must not be reported")
	}
	// Rate() is the bucket most recently charged.
	if c.Rate().Resource != "graphql" {
		t.Errorf("Rate() = %+v, want the last bucket", c.Rate())
	}
	all := c.Rates()
	if len(all) != 3 {
		t.Errorf("Rates() has %d buckets, want 3: %v", len(all), all)
	}
	// Rates() is a copy: mutating it must not touch the client.
	all["core"] = RateState{Remaining: 1}
	if r, _ := c.RateFor("core"); r.Remaining != 4500 {
		t.Error("Rates() must return a copy")
	}
	if _, _, err := c.GetJSON(ctx, "/nobucket", &out, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.RateFor("core"); r.Remaining != 4000 {
		t.Errorf("a response with no resource header charges core, got %+v", r)
	}
}

// The three shapes a status can map to. Telling them apart is the whole job
// of the status mapping: a collector treats UnavailableError and NotReadyError as
// nothing to collect and moves on, and must not do that for anything else.
const (
	kindNotReady    = "NotReadyError"
	kindUnavailable = "UnavailableError"
	kindPlain       = "a plain error"
	kindNone        = "no error"
)

// kindOf names which of the three a collector would see.
func kindOf(err error) string {
	if err == nil {
		return kindNone
	}
	if _, ok := errors.AsType[*NotReadyError](err); ok {
		return kindNotReady
	}
	if _, ok := errors.AsType[*UnavailableError](err); ok {
		return kindUnavailable
	}
	return kindPlain
}

func TestStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   string // which of the three kinds the status must map to
		// detail is NotReadyError.Path or UnavailableError.Reason, the one string the
		// kind carries. Empty for a plain error, which carries neither.
		detail string
		// says are substrings Error() must contain, so a message that stops
		// naming the status or the cause is a failure and not a style change.
		says []string
	}{
		{
			name: "202 is NotReadyError", status: http.StatusAccepted,
			want: kindNotReady, detail: "/repos/o/n/stats/participation",
			says: []string{"/repos/o/n/stats/participation", "202"},
		},
		{
			name: "403 is UnavailableError with the message", status: http.StatusForbidden,
			body: `{"message":"Dependabot alerts are disabled for this repository.","documentation_url":"https://docs.github.com"}`,
			want: kindUnavailable, detail: "Dependabot alerts are disabled for this repository.",
			says: []string{"403", "disabled"},
		},
		{
			name: "404 is UnavailableError", status: http.StatusNotFound, body: `{"message":"Not Found"}`,
			want: kindUnavailable, detail: "Not Found",
		},
		{
			name: "403 with a non-JSON body keeps the text", status: http.StatusForbidden,
			body: "forbidden by policy\n",
			want: kindUnavailable, detail: "forbidden by policy",
		},
		{
			// The lowest status that is not a success. Go does not follow a 300,
			// so it arrives here, and read as a 200 its body would be decoded
			// and cached as the answer.
			name: "300 is a plain error", status: http.StatusMultipleChoices, body: `{"message":"pick one"}`,
			want: kindPlain, says: []string{"300", "pick one"},
		},
		{
			name: "422 is a plain error carrying the body", status: http.StatusUnprocessableEntity,
			body: `{"message":"pagination is limited for this resource"}`,
			want: kindPlain, says: []string{"422", "pagination is limited"},
		},
		{
			name: "500 is a plain error", status: http.StatusInternalServerError, body: "boom",
			want: kindPlain, says: []string{"500"},
		},
		{
			name: "502 is a plain error", status: http.StatusBadGateway, body: "<html>bad gateway</html>",
			want: kindPlain,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				rateHeaders(w, "core", 5000, 100, time.Time{})
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			var out map[string]any
			_, _, err := c.GetJSON(context.Background(), "/repos/o/n/stats/participation", &out, "")

			if got := kindOf(err); got != tc.want {
				t.Errorf("%d is %s, want %s (%v)", tc.status, got, tc.want, err)
			}
			if u, ok := errors.AsType[*UnavailableError](err); ok && (u.Status != tc.status || u.Reason != tc.detail) {
				t.Errorf("UnavailableError = %+v, want status %d reason %q", u, tc.status, tc.detail)
			}
			if nr, ok := errors.AsType[*NotReadyError](err); ok && nr.Path != tc.detail {
				t.Errorf("NotReadyError.Path = %q, want %q", nr.Path, tc.detail)
			}
			for _, want := range tc.says {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Error() = %v, must carry %q", err, want)
				}
			}
			// The budget is read whatever the status.
			if r, ok := c.RateFor("core"); !ok || r.Remaining != 100 {
				t.Errorf("rate not read on %d: %+v", tc.status, r)
			}
		})
	}
}

func TestGetJSONMalformedBody(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"broken":`))
	})
	var out map[string]any
	_, _, err := c.GetJSON(context.Background(), "/x", &out, "")
	if err == nil || !strings.Contains(err.Error(), "/x") {
		t.Errorf("err = %v, want a decode error naming the path", err)
	}
}

func TestGetJSONAbsoluteURLIsUsedAsIs(t *testing.T) {
	t.Parallel()
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Encoded rather than spliced into a literal: the path is what this
		// test varies, and a path carrying a quote would otherwise produce a
		// body that is not JSON at all.
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path})
	})
	c.SetBaseURL("http://127.0.0.1:1") // nothing listens here
	var out struct {
		Path string `json:"path"`
	}
	if _, _, err := c.GetJSON(context.Background(), srv.URL+"/absolute", &out, ""); err != nil {
		t.Fatal(err)
	}
	if out.Path != "/absolute" {
		t.Errorf("path = %q", out.Path)
	}
}

func TestGraphQLUnmarshalsDataAndSendsAWellFormedRequest(t *testing.T) {
	t.Parallel()
	var gotReq *http.Request
	var gotBody []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Clone(r.Context())
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		rateHeaders(w, "graphql", 5000, 4999, time.Time{})
		_, _ = w.Write([]byte(`{"data":{"user":{"login":"octocat","followers":{"totalCount":7}}}}`))
	})
	var out struct {
		User struct {
			Login     string `json:"login"`
			Followers struct {
				TotalCount int `json:"totalCount"`
			} `json:"followers"`
		} `json:"user"`
	}
	err := c.GraphQL(context.Background(), `query($login: String!) { user(login: $login) { login } }`,
		map[string]any{"login": "octocat", "first": 50}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.User.Login != "octocat" || out.User.Followers.TotalCount != 7 {
		t.Errorf("out = %+v", out)
	}
	if gotReq.Method != http.MethodPost || gotReq.URL.Path != "/graphql" {
		t.Errorf("request = %s %s", gotReq.Method, gotReq.URL.Path)
	}
	if gotReq.Header.Get("Authorization") != "Bearer test-token" || gotReq.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", gotReq.Header)
	}
	var env struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err = json.Unmarshal(gotBody, &env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Query, "user(login: $login)") || env.Variables["login"] != "octocat" || env.Variables["first"] != float64(50) {
		t.Errorf("envelope = %+v", env)
	}
	if r, ok := c.RateFor("graphql"); !ok || r.Remaining != 4999 {
		t.Errorf("graphql rate = %+v %v", r, ok)
	}
}

func TestGraphQLTreatsA200WithAnErrorsArrayAsAnError(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'o/n'."}]}`))
	})
	var out map[string]any
	err := c.GraphQL(context.Background(), "{}", nil, &out)
	if err == nil {
		t.Fatal("a 200 with an errors array must be an error")
	}
	if !strings.Contains(err.Error(), "NOT_FOUND") || !strings.Contains(err.Error(), "Could not resolve") {
		t.Errorf("err = %v", err)
	}
	if _, ok := errors.AsType[*TooLargeError](err); ok {
		t.Error("a GraphQL error is not a too-large query")
	}
}

func TestGraphQLWithNilOutSkipsDecoding(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"anything":1}}`))
	})
	if err := c.GraphQL(context.Background(), "{}", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGraphQLMapsAnHTMLGatewayFailureToTooLarge(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502</body></html>"))
	})
	err := c.GraphQL(context.Background(), "{}", nil, nil)
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Status != 502 {
		t.Errorf("err = %v, want TooLargeError 502", err)
	}
}

func TestGraphQLMapsANonJSONAnswerToTooLarge(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("nope"))
	})
	var tl *TooLargeError
	if err := c.GraphQL(context.Background(), "{}", nil, nil); !errors.As(err, &tl) {
		t.Errorf("err = %v, want TooLargeError", err)
	}
}

func TestGraphQLMalformedJSONIsADecodeError(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":`))
	})
	var tl *TooLargeError
	err := c.GraphQL(context.Background(), "{}", nil, nil)
	if err == nil || errors.As(err, &tl) || !strings.Contains(err.Error(), "graphql:") {
		t.Errorf("err = %v", err)
	}
}

func TestGetTextDropsAuthorizationOnRedirect(t *testing.T) {
	t.Parallel()
	var storageAuth, apiAuth string
	var storageHits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/n/actions/jobs/1/logs":
			apiAuth = r.Header.Get("Authorization")
			rateHeaders(w, "core", 5000, 4998, time.Time{})
			// Object storage: the signed URL carries its own credentials.
			http.Redirect(w, r, "/storage/blob?sig=signed", http.StatusFound)
		case "/storage/blob":
			atomic.AddInt32(&storageHits, 1)
			storageAuth = r.Header.Get("Authorization")
			if r.URL.Query().Get("sig") != "signed" {
				t.Errorf("signature lost on redirect: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("2026-09-06T14:05:55Z line one\nline two\n"))
		default:
			http.NotFound(w, r)
		}
	})
	text, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
	if err != nil {
		t.Fatal(err)
	}
	if apiAuth != "Bearer test-token" {
		t.Errorf("the API request must carry the token, got %q", apiAuth)
	}
	if atomic.LoadInt32(&storageHits) != 1 {
		t.Fatalf("storage fetched %d times", storageHits)
	}
	if storageAuth != "" {
		t.Errorf("Authorization %q followed the redirect; storage rejects a request with both credentials", storageAuth)
	}
	if !strings.HasPrefix(text, "2026-09-06T14:05:55Z line one") {
		t.Errorf("text = %q", text)
	}
	// The 302 is the API's answer and carries the budget; storage's final
	// response carries none. The client reads the headers on the way through
	// the redirect, so a log fetch still updates the rate state.
	if _, ok := c.RateFor("core"); !ok {
		t.Error("the API's rate headers on the 302 must be read before the redirect is followed")
	}
}

// TestGetTextAsRepeatsAsAFree304: the bare SHA of a commit carries an ETag
// and GitHub answers the repeat 304, so the head read of the dependency
// diff must ask conditionally and replay the text, as a JSON read would,
// rather than paying a request a repository a day for forty bytes that
// rarely change.
func TestGetTextAsRepeatsAsAFree304(t *testing.T) {
	t.Parallel()
	var accepts, conditionals []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/n/commits/HEAD" {
			http.NotFound(w, r)
			return
		}
		accepts = append(accepts, r.Header.Get("Accept"))
		conditionals = append(conditionals, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"9ed1d32274c3b5dd0c15c507403f6d8e9ce1d617"`)
		if r.Header.Get("If-None-Match") == `"9ed1d32274c3b5dd0c15c507403f6d8e9ce1d617"` {
			rateHeaders(w, "core", 5000, 4999, time.Time{})
			w.WriteHeader(http.StatusNotModified)
			return
		}
		rateHeaders(w, "core", 5000, 4999, time.Time{})
		w.Header().Set("Content-Type", "application/vnd.github.sha; charset=utf-8")
		_, _ = w.Write([]byte("9ed1d32274c3b5dd0c15c507403f6d8e9ce1d617"))
	})
	for i := range 2 {
		sha, err := c.GetTextAs(context.Background(), "/repos/o/n/commits/HEAD", "application/vnd.github.sha")
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if sha != "9ed1d32274c3b5dd0c15c507403f6d8e9ce1d617" {
			t.Errorf("read %d = %q", i, sha)
		}
	}
	if len(accepts) != 2 || accepts[0] != "application/vnd.github.sha" || accepts[1] != accepts[0] {
		t.Errorf("Accept headers = %q", accepts)
	}
	if conditionals[0] != "" || conditionals[1] != `"9ed1d32274c3b5dd0c15c507403f6d8e9ce1d617"` {
		t.Errorf("If-None-Match = %q, want none then the ETag", conditionals)
	}
}

// TestGetTextDoesNotCacheWhatARedirectServed: a log blob is read once and
// its storage ETag is not the API's, so the log fetch is never conditional.
func TestGetTextDoesNotCacheWhatARedirectServed(t *testing.T) {
	t.Parallel()
	var conditionals []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/n/actions/jobs/1/logs":
			conditionals = append(conditionals, r.Header.Get("If-None-Match"))
			http.Redirect(w, r, "/storage/blob", http.StatusFound)
		case "/storage/blob":
			w.Header().Set("ETag", `"0x8DD1234"`)
			_, _ = w.Write([]byte("line one\n"))
		default:
			http.NotFound(w, r)
		}
	})
	for range 2 {
		if _, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs"); err != nil {
			t.Fatal(err)
		}
	}
	if len(conditionals) != 2 || conditionals[0] != "" || conditionals[1] != "" {
		t.Errorf("If-None-Match on the log request = %q, want none: storage's ETag is not the API's", conditionals)
	}
}

func TestGetTextStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status      int
		unavailable bool
	}{
		{http.StatusMultipleChoices, false}, // not followed, and not a log either
		{http.StatusGone, true},             // GitHub deletes logs after ninety days
		{http.StatusNotFound, true},
		{http.StatusForbidden, true},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("whatever"))
			})
			_, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
			if err == nil {
				t.Fatal("expected an error")
			}
			var u *UnavailableError
			if got := errors.As(err, &u); got != tc.unavailable {
				t.Errorf("%d: UnavailableError = %v, want %v (%v)", tc.status, got, tc.unavailable, err)
			}
			if tc.unavailable && u.Status != tc.status {
				t.Errorf("UnavailableError.Status = %d", u.Status)
			}
		})
	}
}

func TestGetTextTooManyRedirects(t *testing.T) {
	t.Parallel()
	// Each hop redirects to a fresh path this handler numbers itself, so the
	// chain never repeats a URL and nothing the caller sent decides where the
	// next hop goes.
	var hop atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop/"+strconv.FormatInt(hop.Add(1), 10), http.StatusFound)
	})
	_, err := c.GetText(context.Background(), "/loop")
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("err = %v", err)
	}
	// Five requests and not a sixth: the first plus four hops, the fifth hop
	// refused before it is sent. Every hop of a log fetch is a request, and
	// the ones to the API are charged.
	if n := hop.Load(); n != 5 {
		t.Errorf("the server saw %d requests, want 5", n)
	}
}

func TestGetTextIsBounded(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Well past the 8 MiB cap.
		chunk := strings.Repeat("x", 1<<20)
		for range 10 {
			_, _ = io.WriteString(w, chunk)
		}
	})
	text, err := c.GetText(context.Background(), "/big")
	if err != nil {
		t.Fatal(err)
	}
	if len(text) != 8<<20 {
		t.Errorf("read %d bytes, want the 8 MiB cap", len(text))
	}
}

func TestReasonOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"message":"Not Found","documentation_url":"x"}`: "Not Found",
		`{"message":""}`:  `{"message":""}`,
		"  plain text \n": "plain text",
		"":                "",
	}
	for in, want := range cases {
		if got := reasonOf([]byte(in)); got != want {
			t.Errorf("reasonOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		resource := "core"
		if strings.HasPrefix(r.URL.Path, "/search") {
			resource = "search"
		}
		rateHeaders(w, resource, 5000, 4000, time.Time{})
		w.Header().Set("ETag", `"`+r.URL.Path+`"`)
		if r.Header.Get("If-None-Match") == `"`+r.URL.Path+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`{"n":1}`))
	})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out map[string]int
			path := fmt.Sprintf("/repos/o/n%d", i%4)
			if i%3 == 0 {
				path = "/search/issues"
			}
			for range 5 {
				if _, _, err := c.GetJSON(context.Background(), path, &out, ""); err != nil {
					t.Error(err)
					return
				}
				if out["n"] != 1 {
					t.Errorf("out = %v", out)
				}
				c.Rates()
				c.Rate()
			}
		}(i)
	}
	wg.Wait()
	if len(c.Rates()) != 2 {
		t.Errorf("buckets = %v", c.Rates())
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var out map[string]any
	if _, _, err := c.GetJSON(ctx, "/slow", &out, ""); err == nil {
		t.Error("a canceled context must fail the request")
	}
	if err := c.GraphQL(ctx, "{}", nil, nil); err == nil {
		t.Error("a canceled context must fail the query")
	}
}

func TestRateLimitedIsNotAMissingFeature(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// GitHub answers a spent budget with the same status a switched-off
		// feature does. The headers are what tells them apart.
		w.Header().Set("x-ratelimit-limit", "5000")
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-resource", "core")
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	c := New("token", 0)
	c.SetBaseURL(srv.URL)
	_, _, err := c.GetJSON(context.Background(), "/repos/o/r", &struct{}{}, "")
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want a RateLimitedError", err)
	}
	if limited.Resource != "core" {
		t.Errorf("resource = %q", limited.Resource)
	}
	if _, ok := errors.AsType[*UnavailableError](err); ok {
		t.Error("a spent budget must not also read as a missing feature")
	}
}

func TestBrakeRefusesBeforeSpendingTheReserve(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("x-ratelimit-limit", "5000")
		w.Header().Set("x-ratelimit-remaining", "10")
		w.Header().Set("x-ratelimit-resource", "core")
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New("token", 0)
	c.SetBaseURL(srv.URL)
	c.SetReserve(500, false)
	// The first request has no budget to judge, so it goes out and reports
	// ten left. The second must not: the reserve is five hundred.
	if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, _, err := c.GetJSON(context.Background(), "/b", &struct{}{}, "")
	if _, ok := errors.AsType[*RateLimitedError](err); !ok {
		t.Fatalf("second request: err = %v, want a RateLimitedError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the server saw %d requests; the brake must stop the second before it is sent", got)
	}
}

func TestBrakeScalesTheReserveToTheBucket(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("x-ratelimit-limit", "30")
		w.Header().Set("x-ratelimit-remaining", "20")
		w.Header().Set("x-ratelimit-resource", "search")
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New("token", 0)
	c.SetBaseURL(srv.URL)
	c.SetReserve(500, false)
	// Search allows thirty a minute. A flat reserve of five hundred would make
	// the bucket permanently unusable, so it scales to a fifth of the limit.
	for i := range 2 {
		if _, _, err := c.GetJSON(context.Background(), "/search/issues?q=a", &struct{}{}, ""); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the server saw %d requests, want 2", got)
	}
}

// budgetAnswer writes what GitHub answers a query that carries the injected
// block, with numbers that disagree with the headers so a test can tell which
// source the client believed.
func budgetAnswer(w http.ResponseWriter, cost, used int, reset time.Time) {
	w.Header().Set("Content-Type", "application/json")
	rateHeaders(w, "graphql", 5000, 4999, time.Time{})
	fmt.Fprintf(w, `{"data":{"ghcRateLimit":{"limit":5000,"cost":%d,"used":%d,"remaining":%d,"resetAt":%q}}}`,
		cost, used, 5000-used, reset.Format(time.RFC3339))
}

func TestGraphQLCarriesTheBudgetBlock(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	var queries []string
	var mu sync.Mutex
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Query string `json:"query"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &env)
		mu.Lock()
		queries = append(queries, env.Query)
		mu.Unlock()
		budgetAnswer(w, 17, 240, reset)
	})

	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.GraphQL(context.Background(), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(queries[0], "rateLimit"); n != 1 {
		t.Errorf("the query carried the block %d times, want once: %s", n, queries[0])
	}
	if !strings.HasPrefix(queries[0], "query { "+rateLimitBlock) {
		t.Errorf("the block must open the root selection set, got %s", queries[0])
	}

	// The block is the counter GitHub documents. The headers of the same
	// response were made to disagree here on purpose.
	st, ok := c.RateFor("graphql")
	if !ok || st.Used != 240 || st.Remaining != 4760 || !st.Reset.Equal(reset) {
		t.Errorf("graphql state = %+v %v, want the block's numbers, not the headers'", st, ok)
	}
	if spend := c.GraphQLSpend(); spend.Queries != 1 || spend.Cost != 17 {
		t.Errorf("spend = %+v, want one query costing 17", spend)
	}
	if err := c.GraphQL(context.Background(), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatal(err)
	}
	if spend := c.GraphQLSpend(); spend.Queries != 2 || spend.Cost != 34 {
		t.Errorf("spend = %+v, want what GitHub priced each query at, added up", spend)
	}
}

func TestGraphQLRateIsFreeAndIgnoresTheBrake(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Almost nothing left, so the brake refuses an ordinary query.
		budgetAnswer(w, 1, 4995, reset)
	})
	c.SetReserve(500, false)
	if err := c.GraphQL(context.Background(), "query { viewer { login } }", nil, nil); err != nil {
		t.Fatal(err)
	}
	var limited *RateLimitedError
	if err := c.GraphQL(context.Background(), "query { viewer { login } }", nil, nil); !errors.As(err, &limited) {
		t.Fatalf("an ordinary query past the reserve = %v, want RateLimitedError", err)
	}
	before := c.GraphQLSpend()
	st, err := c.GraphQLRate(context.Background())
	if err != nil {
		t.Fatalf("the budget must be readable when there is no budget left: %v", err)
	}
	if st.Used != 4995 || st.Remaining != 5 {
		t.Errorf("budget = %+v", st)
	}
	// Measured against api.github.com: a rateLimit-only query reports cost 1
	// and does not move `used`, so counting it would invent spending.
	if after := c.GraphQLSpend(); after != before {
		t.Errorf("the budget query was charged: %+v became %+v", before, after)
	}
}

func TestWithRateLimitFindsTheRootOfTheRealQueries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, query, want string
	}{
		{
			"a query with variables, as most collectors write",
			"query($owner: String!, $name: String!) {\n  repository(owner: $owner) { name }\n}",
			"query($owner: String!, $name: String!) { " + rateLimitBlock + "\n  repository(owner: $owner) { name }\n}",
		},
		{
			"the anonymous shorthand",
			"{ viewer { login } }",
			"{ " + rateLimitBlock + " viewer { login } }",
		},
		{
			"a batch of aliases followed by its fragment, which must get one block and not one per alias",
			`query { r0: repository(owner: "o", name: "n") { ...totals } } fragment totals on Repository { name }`,
			`query { ` + rateLimitBlock + ` r0: repository(owner: "o", name: "n") { ...totals } } fragment totals on Repository { name }`,
		},
		{
			"a document whose fragment comes first",
			"fragment totals on Repository { name } query { r0: repository { ...totals } }",
			"fragment totals on Repository { name } query { " + rateLimitBlock + " r0: repository { ...totals } }",
		},
		{
			"a brace inside a string is not structure",
			`query { search(query: "a { b") { total } }`,
			`query { ` + rateLimitBlock + ` search(query: "a { b") { total } }`,
		},
		{
			"a comment before the root",
			"# the account's own numbers {\nquery { viewer { login } }",
			"# the account's own numbers {\nquery { " + rateLimitBlock + " viewer { login } }",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := withRateLimit(tc.query)
			if !ok || got != tc.want {
				t.Errorf("withRateLimit(%q)\n got %q\nwant %q", tc.query, got, tc.want)
			}
			if strings.Count(got, RateLimitAlias) != 1 {
				t.Errorf("the block must appear once, got %q", got)
			}
		})
	}

	t.Run("a document that already carries the block is left alone", func(t *testing.T) {
		t.Parallel()
		got, ok := withRateLimit(BudgetQuery)
		if !ok || got != BudgetQuery {
			t.Errorf("got %q %v", got, ok)
		}
	})

	t.Run("a mutation is never touched", func(t *testing.T) {
		t.Parallel()
		// Nothing here writes, and Mutation has no rateLimit field: injecting
		// would turn a working request into a query error.
		const m = "mutation { addStar(input: {starrableId: \"x\"}) { clientMutationId } }"
		if got, ok := withRateLimit(m); ok || got != m {
			t.Errorf("got %q %v", got, ok)
		}
	})
}

func TestGraphQLCountsAQueryThatOnlyHalfWorked(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// What a batch of ten repositories answers when one of them was
		// renamed away: the data, the block, and a NOT_FOUND for that alias.
		// Measured against api.github.com on 2026-09-08, cost 1 all the same.
		w.Header().Set("Content-Type", "application/json")
		rateHeaders(w, "graphql", 5000, 4404, reset)
		fmt.Fprintf(w, `{"data":{"ghcRateLimit":{"limit":5000,"cost":1,"used":596,"remaining":4404,"resetAt":%q},"r0":{"name":"n"},"r1":null},`+
			`"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`, reset.Format(time.RFC3339))
	})

	var out map[string]any
	if err := c.GraphQL(context.Background(), `query { r0: repository { name } r1: repository { name } }`, nil, &out); err == nil {
		t.Fatal("a partial answer must still be reported as an error to the caller")
	}
	if spend := c.GraphQLSpend(); spend.Queries != 1 || spend.Cost != 1 {
		t.Errorf("spend = %+v, want one query costing 1: GitHub charged for it", spend)
	}
	st, ok := c.RateFor("graphql")
	if !ok || st.Used != 596 {
		t.Errorf("graphql state = %+v %v, want the block of the partial answer", st, ok)
	}
}

func TestWithRateLimitDoesNotReadAnArgumentValueAsStructure(t *testing.T) {
	t.Parallel()
	// The braces of an input object close before the root brace ever opens.
	// Read as structure they make the definition look finished, and the block
	// would then be injected into a mutation, which has no rateLimit field.
	const m = `mutation($i: In = {a: 1}) { addStar(input: $i) { clientMutationId } }`
	if got, ok := withRateLimit(m); ok || got != m {
		t.Errorf("got %q %v, want the mutation untouched", got, ok)
	}
	const q = `query($f: F = {a: 1}) { viewer { login } }`
	got, ok := withRateLimit(q)
	if !ok || !strings.HasPrefix(got, `query($f: F = {a: 1}) { `+rateLimitBlock) {
		t.Errorf("got %q %v, want the block at the root of the query", got, ok)
	}
}

// serveNumbered answers every fresh request with a padded body carrying its
// own sequence number, and revalidates a conditional one the way GitHub does.
//
// The number is what tells a replayed body from a fetched one: a cache hit
// gives back the number the URL was first served, and an entry that was
// evicted comes back with a new one. It also records whether each URL's last
// request carried an If-None-Match, which is the other half of the same
// question asked from the wire rather than from the answer.
//
// The 304 is conditional on the validator still describing what the handler
// would serve now, not merely on the header being present. A test that makes
// size return something different for a path is changing that path's content,
// and a server that answered 304 anyway would be lying about it.
func serveNumbered(size func(path string) int) (handler http.HandlerFunc, wasConditional func(path string) bool) {
	type served struct {
		etag string
		size int
	}
	var mu sync.Mutex
	var seq int
	last := map[string]served{}
	conditional := map[string]bool{}
	handler = func(w http.ResponseWriter, r *http.Request) {
		want := size(r.URL.Path)
		mu.Lock()
		asked := r.Header.Get("If-None-Match")
		conditional[r.URL.Path] = asked != ""
		prev, known := last[r.URL.Path]
		fresh := !known || asked != prev.etag || want != prev.size
		if !fresh {
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		seq++
		n := seq
		// The ETag is the sequence number rather than the path, so a second
		// fetch of the same URL is a different validator.
		tag := `"v` + strconv.Itoa(n) + `"`
		last[r.URL.Path] = served{etag: tag, size: want}
		mu.Unlock()
		w.Header().Set("ETag", tag)
		body := []byte(`{"seq":` + strconv.Itoa(n) + `,"pad":"`)
		for len(body) < want {
			body = append(body, 'a')
		}
		_, _ = w.Write(append(body, '"', '}'))
	}
	wasConditional = func(path string) bool {
		mu.Lock()
		defer mu.Unlock()
		return conditional[path]
	}
	return handler, wasConditional
}

// numbered is what serveNumbered's answers decode into. It keeps the padding
// on purpose: the cache stores the decoded value re-encoded rather than the
// wire body, so a type that dropped the padding would store nine bytes for a
// four kilobyte answer and the tests that fill the bound would fill nothing.
type numbered struct {
	Seq int    `json:"seq"`
	Pad string `json:"pad"`
}

// seqOf fetches path and returns the sequence number of the body it got.
func seqOf(t *testing.T, c *Client, path string) int {
	t.Helper()
	var out numbered
	if _, _, err := c.GetJSON(context.Background(), path, &out, ""); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out.Seq
}

func TestTheCacheIsBoundedByBytes(t *testing.T) {
	t.Parallel()
	h, _ := serveNumbered(func(string) int { return 4 << 10 })
	c, _ := newTestClient(t, h)
	const limit = 64 << 10
	c.SetCacheLimit(limit)

	// Sixteen of these fit. Two hundred are the best part of a megabyte,
	// which is what an unbounded cache would be holding here.
	for i := range 200 {
		seqOf(t, c, "/page/"+strconv.Itoa(i))
	}
	st := c.CacheStats()
	if st.Bytes > limit {
		t.Errorf("the cache holds %d bytes against a limit of %d", st.Bytes, limit)
	}
	if st.Entries >= 200 {
		t.Errorf("the cache kept %d of 200 entries, so nothing was evicted", st.Entries)
	}
	if st.Evicted == 0 {
		t.Error("evictions were not counted")
	}
	if st.Limit != limit {
		t.Errorf("stats report a limit of %d", st.Limit)
	}
	// The running total is what the bound is enforced against, so it has to
	// still equal the entries after two hundred inserts and the evictions they
	// caused. A total that drifts up eventually refuses to store anything; one
	// that drifts down stops bounding.
	var sum int
	for _, el := range c.cache.entries {
		sum += el.Value.(*conditional).size()
	}
	if sum != st.Bytes {
		t.Errorf("the running total is %d but the entries add up to %d", st.Bytes, sum)
	}
	if len(c.cache.entries) != c.cache.order.Len() {
		t.Errorf("%d entries against %d in the eviction order", len(c.cache.entries), c.cache.order.Len())
	}
}

func TestTheLeastRecentlyUsedEntryIsTheOneEvicted(t *testing.T) {
	t.Parallel()
	const size = 10 << 10
	h, wasConditional := serveNumbered(func(string) int { return size })
	c, _ := newTestClient(t, h)
	// Room for three of these and not a fourth.
	c.SetCacheLimit(3 * (size + 512))

	first := map[string]int{}
	for _, p := range []string{"/a", "/b", "/c"} {
		first[p] = seqOf(t, c, p)
	}
	// Touching /a again leaves /b as the oldest, so /d must take /b's place
	// and not /a's.
	if n := seqOf(t, c, "/a"); n != first["/a"] {
		t.Fatalf("/a came back as body %d, want the stored %d replayed", n, first["/a"])
	}
	seqOf(t, c, "/d")

	if n := seqOf(t, c, "/a"); n != first["/a"] {
		t.Errorf("/a came back as body %d: the least recently used entry was /b, not /a", n)
	}
	if !wasConditional("/a") {
		t.Error("/a was still cached, so its repeat had to carry If-None-Match")
	}
	if n := seqOf(t, c, "/b"); n == first["/b"] {
		t.Errorf("/b was still cached as body %d, so the fourth entry evicted nothing", n)
	}
	if wasConditional("/b") {
		t.Error("/b was the entry evicted, and its ETag had to go with its body: a conditional repeat means one of the two was left behind, and its 304 has nothing to replay")
	}
}

func TestABodyTooLargeToCacheEvictsNothing(t *testing.T) {
	t.Parallel()
	const limit = 32 << 10
	h, wasConditional := serveNumbered(func(path string) int {
		if path == "/huge" {
			return 2 * limit
		}
		return 1 << 10
	})
	c, _ := newTestClient(t, h)
	c.SetCacheLimit(limit)

	small := seqOf(t, c, "/small")
	seqOf(t, c, "/huge")
	if st := c.CacheStats(); st.Entries != 1 {
		t.Errorf("the cache holds %d entries and %d bytes: a body that cannot fit must not be stored, and must displace nothing to find that out", st.Entries, st.Bytes)
	}
	seqOf(t, c, "/huge")
	if wasConditional("/huge") {
		t.Error("/huge was never stored, so its repeat must not ask conditionally")
	}
	if n := seqOf(t, c, "/small"); n != small {
		t.Errorf("/small came back as body %d rather than the stored %d: it was evicted for a body that was not kept either", n, small)
	}
}

func TestRefetchingAURLReplacesItsEntryRatherThanAddingOne(t *testing.T) {
	t.Parallel()
	// A URL whose content moved is answered 200 and stored again. It is one
	// URL and it must be counted once: an entry re-counted on every sweep
	// makes the running total climb without the cache holding anything more,
	// and the bound then evicts live entries to make room for arithmetic.
	size := 4 << 10
	var mu sync.Mutex
	h, _ := serveNumbered(func(string) int {
		mu.Lock()
		defer mu.Unlock()
		return size
	})
	c, _ := newTestClient(t, h)

	first := seqOf(t, c, "/moves")
	before := c.CacheStats()
	for i := range 20 {
		// A different size each time is a different body, so the server
		// answers 200 and the client stores the new pair over the old.
		mu.Lock()
		size = (4 << 10) + i + 1
		mu.Unlock()
		if seqOf(t, c, "/moves") == first {
			t.Fatalf("round %d replayed the stored body, so nothing was re-stored", i)
		}
	}
	st := c.CacheStats()
	if st.Entries != 1 {
		t.Errorf("one URL fetched twenty-one times is %d entries", st.Entries)
	}
	if st.Bytes > before.Bytes+64 {
		t.Errorf("the running total went from %d to %d for one URL that grew by twenty bytes", before.Bytes, st.Bytes)
	}
	var sum int
	for _, el := range c.cache.entries {
		sum += el.Value.(*conditional).size()
	}
	if sum != st.Bytes {
		t.Errorf("the running total is %d but the entry is %d", st.Bytes, sum)
	}
}

func TestAnETagWithAnEmptyBodyIsNotStored(t *testing.T) {
	t.Parallel()
	// An endpoint can answer 200 with a validator and nothing in it. Storing
	// that pair would put an ETag in the cache that gets asked with, and the
	// 304 it earns would replay nothing: json.Unmarshal of an empty body is an
	// error, and a caller that got past that would read the emptiness as an
	// endpoint with nothing to report.
	var hits atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"empty"`)
		w.WriteHeader(http.StatusOK)
	})

	out := map[string]int{"keep": 1}
	for range 2 {
		if _, _, err := c.GetJSON(context.Background(), "/nothing", &out, ""); err != nil {
			t.Fatalf("an empty body is not a failure: %v", err)
		}
	}
	if out["keep"] != 1 {
		t.Errorf("out was written from an empty body: %v", out)
	}
	if st := c.CacheStats(); st.Entries != 0 || st.Bytes != 0 {
		t.Errorf("an ETag with no body was stored: %+v", st)
	}
	if hits.Load() != 2 {
		t.Errorf("the server saw %d requests, want both asked unconditionally", hits.Load())
	}
}

func TestAURLWhoseBodyGrowsPastTheLimitGivesUpItsOldEntry(t *testing.T) {
	t.Parallel()
	// The old pair would still be answered correctly: its ETag describes the
	// body stored beside it, so GitHub can only ever answer 200 for it now.
	// That is the point. It can no longer produce a hit, so it is memory held
	// for an answer that will never come.
	const limit = 32 << 10
	var grown atomic.Bool
	h, wasConditional := serveNumbered(func(path string) int {
		if path == "/grows" && grown.Load() {
			return 2 * limit
		}
		return 1 << 10
	})
	c, _ := newTestClient(t, h)
	c.SetCacheLimit(limit)

	seqOf(t, c, "/grows")
	if st := c.CacheStats(); st.Entries != 1 {
		t.Fatalf("the small body should have been cached, stats say %+v", st)
	}
	grown.Store(true)
	// The repeat asks conditionally, the server answers 200 because the
	// content changed, and the body that comes back no longer fits.
	seqOf(t, c, "/grows")
	if !wasConditional("/grows") {
		t.Error("that repeat had a stored body, so it had to carry If-None-Match")
	}
	if st := c.CacheStats(); st.Entries != 0 || st.Bytes != 0 {
		t.Errorf("the cache still holds %d entries and %d bytes for a URL whose body it refused to store", st.Entries, st.Bytes)
	}
	seqOf(t, c, "/grows")
	if wasConditional("/grows") {
		t.Error("nothing is stored for /grows any more, so it must not ask conditionally: a 304 would have nothing to replay")
	}
}

func TestA304IsAnsweredFromTheBodyItsRequestAskedWith(t *testing.T) {
	t.Parallel()
	// The cache can be evicted from while a request is in flight, because the
	// client is used concurrently and holds no lock across the round trip.
	// Asking conditionally is a promise that the answer can be replayed, so
	// the body has to be the one that was read when its ETag was.
	const size = 4 << 10
	var c *Client
	var evicted atomic.Bool
	h, _ := serveNumbered(func(string) int { return size })
	c, _ = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" && !evicted.Swap(true) {
			var out numbered
			for i := range 40 {
				if _, _, err := c.GetJSON(r.Context(), "/filler/"+strconv.Itoa(i), &out, ""); err != nil {
					t.Errorf("filler: %v", err)
				}
			}
		}
		h(w, r)
	})
	c.SetCacheLimit(8 * (size + 512))

	first := seqOf(t, c, "/a")
	if n := seqOf(t, c, "/a"); n != first {
		t.Errorf("the 304 replayed body %d, want %d: a body evicted mid-request must still answer the request that asked with its ETag", n, first)
	}
}

func TestTheDefaultLimitHoldsAMeasuredSweepWithRoomToSpare(t *testing.T) {
	t.Parallel()
	// The most the measured account ever charged against the limit, from
	// TestLiveSweepCacheFootprint and quoted in the comment on
	// DefaultCacheBytes: the high-water mark after three sweeps, of which the
	// first alone charged 114,662,719. This test is here so that shrinking the
	// default has to argue with the measurement rather than quietly undo it.
	const measuredSweep = 114_807_379
	if DefaultCacheBytes < measuredSweep {
		t.Fatalf("the default holds %d bytes and one measured sweep is %d: a limit under one sweep's live set is not a smaller cache but no cache, because every sweep evicts what the next one is about to ask for",
			DefaultCacheBytes, measuredSweep)
	}
	// The account that was measured is not the largest this will ever run
	// against, and the dead entries between evictions need somewhere to sit.
	if DefaultCacheBytes < 2*measuredSweep {
		t.Errorf("the default is %d, only %.1f times the measured sweep of %d: too little headroom for a larger account",
			DefaultCacheBytes, float64(DefaultCacheBytes)/measuredSweep, measuredSweep)
	}
}

func TestSetCacheLimitShrinksWhatIsAlreadyHeld(t *testing.T) {
	t.Parallel()
	h, _ := serveNumbered(func(string) int { return 4 << 10 })
	c, _ := newTestClient(t, h)
	for i := range 20 {
		seqOf(t, c, "/page/"+strconv.Itoa(i))
	}
	const limit = 16 << 10
	c.SetCacheLimit(limit)
	if st := c.CacheStats(); st.Bytes > limit {
		t.Errorf("after shrinking the limit to %d the cache still holds %d bytes in %d entries", limit, st.Bytes, st.Entries)
	}
	c.SetCacheLimit(0)
	if st := c.CacheStats(); st.Limit != DefaultCacheBytes {
		t.Errorf("zero must restore the default, got %d", st.Limit)
	}
}

// wideAnswer is a page shaped like the ones GitHub sends: a handful of fields
// a collector reads and a great deal it never looks at, here the ninety-odd
// per run that a workflow run page carries.
func wideAnswer(rows int) []byte {
	var b strings.Builder
	b.WriteString(`{"total_count":` + strconv.Itoa(rows) + `,"workflow_runs":[`)
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":` + strconv.Itoa(1000+i) + `,"conclusion":"success","run_started_at":"2026-09-08T10:00:00Z","updated_at":"2026-09-08T10:05:00Z"`)
		for f := range 90 {
			b.WriteString(`,"unread_field_` + strconv.Itoa(f) + `":"` + strings.Repeat("x", 40) + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return []byte(b.String())
}

// wideRuns is what a collector keeps of wideAnswer.
type wideRuns struct {
	Total int `json:"total_count"`
	Runs  []struct {
		ID         int64     `json:"id"`
		Conclusion string    `json:"conclusion"`
		Started    time.Time `json:"run_started_at"`
		Updated    time.Time `json:"updated_at"`
	} `json:"workflow_runs"`
}

func TestTheCacheStoresTheDecodedValueNotTheWireBody(t *testing.T) {
	t.Parallel()
	// The measured shape: a page where the collector's struct keeps a few
	// percent of the bytes. What is stored beside the ETag is the struct
	// re-encoded, so the entry costs the bound what the collector keeps
	// rather than what GitHub sent, and the 304 decodes that much less.
	raw := wideAnswer(100)
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 && r.Header.Get("If-None-Match") == `"wide"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"wide"`)
		_, _ = w.Write(raw)
	})

	var first, second wideRuns
	if _, cached, err := c.GetJSON(context.Background(), "/runs", &first, ""); err != nil || cached {
		t.Fatalf("first: err=%v cached=%v", err, cached)
	}
	entries := CacheEntries(c)
	if len(entries) != 1 {
		t.Fatalf("stored %d entries, want the one page", len(entries))
	}
	stored := entries[0].Body
	if stored*10 > len(raw) {
		t.Errorf("stored %d bytes for a %d byte page: the entry must cost what the struct keeps, not what the wire carried", stored, len(raw))
	}
	if st := c.CacheStats(); st.Bytes != entries[0].Charged {
		t.Errorf("the bound is charged %d for an entry of %d: the running total must count the stored body", st.Bytes, entries[0].Charged)
	}

	if _, cached, err := c.GetJSON(context.Background(), "/runs", &second, ""); err != nil || !cached {
		t.Fatalf("second: err=%v cached=%v", err, cached)
	}
	if calls.Load() != 2 {
		t.Fatalf("the server saw %d requests", calls.Load())
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the 304 decoded differently from the 200:\n200: %+v\n304: %+v", first, second)
	}
	if len(second.Runs) != 100 || second.Runs[99].ID != 1099 || !second.Runs[0].Started.Equal(time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("the replay lost data: %+v", second.Runs[len(second.Runs)-1])
	}
}

func TestARawMessageFieldSurvivesTheReplay(t *testing.T) {
	t.Parallel()
	// The event feed keeps each event's payload as json.RawMessage and reads
	// it by type afterwards. RawMessage encodes as the bytes it holds, so the
	// replay carries the payload verbatim, nested objects and all.
	const body = `[{"id":"1","type":"PushEvent","payload":{"size":3,"commits":[{"sha":"a"},{"sha":"b"}]}}]`
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 && r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"events"`)
		_, _ = w.Write([]byte(body))
	})
	type event struct {
		ID      string          `json:"id"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	var first, second []event
	for i, out := range []*[]event{&first, &second} {
		if _, cached, err := c.GetJSON(context.Background(), "/events", out, ""); err != nil || cached != (i == 1) {
			t.Fatalf("call %d: err=%v cached=%v", i+1, err, cached)
		}
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the replay differs:\n200: %s\n304: %s", first[0].Payload, second[0].Payload)
	}
	var push struct {
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(second[0].Payload, &push); err != nil || len(push.Commits) != 2 {
		t.Errorf("the replayed payload does not decode as the 200's did: %v %+v", err, push)
	}
}

// lossy decodes a count it does not encode: the one shape of type the cache
// cannot replay faithfully, pinned here so that a parity failure in the
// collect package reads as a cache property and not a mystery.
type lossy struct {
	Kept    int
	Dropped int
}

func (l *lossy) UnmarshalJSON(b []byte) error {
	var wire struct {
		Kept    int `json:"kept"`
		Dropped int `json:"dropped"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	l.Kept, l.Dropped = wire.Kept, wire.Dropped
	return nil
}

func (l lossy) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int{"kept": l.Kept})
}

func TestATypeThatDecodesMoreThanItEncodesIsReplayedShort(t *testing.T) {
	t.Parallel()
	// A `json:"-"` field is not this case: it is decoded on neither path. The
	// asymmetry is a custom UnmarshalJSON whose MarshalJSON twin writes less
	// than it read. No collector decodes into one, and the parity test in
	// the collect package fails the first that does.
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 && r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"lossy"`)
		_, _ = w.Write([]byte(`{"kept":1,"dropped":2}`))
	})
	var first, second lossy
	for _, out := range []*lossy{&first, &second} {
		if _, _, err := c.GetJSON(context.Background(), "/lossy", out, ""); err != nil {
			t.Fatal(err)
		}
	}
	if first.Kept != 1 || first.Dropped != 2 {
		t.Fatalf("the 200 decoded %+v", first)
	}
	if second.Kept != 1 || second.Dropped != 0 {
		t.Errorf("the 304 decoded %+v: the cache holds what the type encoded, which left dropped out", second)
	}
	if entries := CacheEntries(c); len(entries) != 1 || entries[0].Body != len(`{"kept":1}`) {
		t.Errorf("stored %+v, want the encoded value and nothing the type left out of it", entries)
	}
}

func TestAValueThatCannotBeEncodedKeepsTheWireBody(t *testing.T) {
	t.Parallel()
	// A decoded value encoding/json refuses to encode again is stored as it
	// arrived, so the URL is still asked conditionally and the 304 still has
	// something to replay: the cache is then no better than before for that
	// one answer, and no worse.
	const body = `{"n":1}`
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 && r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"chan"`)
		_, _ = w.Write([]byte(body))
	})
	// A channel field is what json cannot encode; it is skipped on decode.
	type unencodable struct {
		N    int `json:"n"`
		Wake chan struct{}
	}
	out := unencodable{Wake: make(chan struct{})}
	for i := range 2 {
		if _, cached, err := c.GetJSON(context.Background(), "/chan", &out, ""); err != nil || cached != (i == 1) {
			t.Fatalf("call %d: err=%v cached=%v", i+1, err, cached)
		}
	}
	if out.N != 1 {
		t.Errorf("the replay lost the value: %+v", out)
	}
	if entries := CacheEntries(c); len(entries) != 1 || entries[0].Body != len(body) {
		t.Errorf("stored %+v, want the wire body kept for a value that cannot be re-encoded", entries)
	}
}

func TestA304WithNoDestinationReturnsTheLink(t *testing.T) {
	t.Parallel()
	// A call that decodes nothing still stores the wire body, so its repeat
	// is conditional; the 304 then has no value to fill and must not try to,
	// while the Link header a paginating caller reads is still returned.
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
		if calls.Add(1) > 1 && r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"nil"`)
		_, _ = w.Write([]byte(`{"ignored":true}`))
	})
	for i := range 2 {
		link, cached, err := c.GetJSON(context.Background(), "/nil", nil, "")
		if err != nil || cached != (i == 1) || !strings.Contains(link, `rel="next"`) {
			t.Errorf("call %d: err=%v cached=%v link=%q", i+1, err, cached, link)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("the server saw %d requests", calls.Load())
	}
}

func TestOneURLDecodedIntoTwoTypesReplaysEachItsOwnValue(t *testing.T) {
	t.Parallel()
	// GET /repos/{owner}/{repo} is asked twice a sweep when a repository is
	// named in targets.repos: discovery decodes four flags out of it and the
	// repo family decodes forty fields. The cache stores what a caller
	// decoded, so an entry written by the four-field caller holds nothing of
	// the other thirty-six, and a 304 answered from it to the forty-field
	// caller would report zero stars on a repository that has hundreds.
	const body = `{"private":false,"fork":false,"stargazers_count":321,"language":"Go"}`
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `"repo"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"repo"`)
		_, _ = w.Write([]byte(body))
	})
	type flags struct {
		Private bool `json:"private"`
		Fork    bool `json:"fork"`
	}
	type detail struct {
		Private  bool   `json:"private"`
		Stars    int    `json:"stargazers_count"`
		Language string `json:"language"`
	}
	var f flags
	if _, _, err := c.GetJSON(context.Background(), "/repos/o/r", &f, ""); err != nil {
		t.Fatal(err)
	}
	var d detail
	if _, _, err := c.GetJSON(context.Background(), "/repos/o/r", &d, ""); err != nil {
		t.Fatal(err)
	}
	if d.Stars != 321 || d.Language != "Go" {
		t.Fatalf("the second caller decoded %+v: it was answered from what the first caller kept", d)
	}
	// Each caller has its own entry from here on, and each repeat is
	// conditional and answered with the value that caller decoded.
	var f2 flags
	var d2 detail
	if _, cached, err := c.GetJSON(context.Background(), "/repos/o/r", &f2, ""); err != nil || !cached || f2 != f {
		t.Errorf("flags repeat: err=%v cached=%v got %+v", err, cached, f2)
	}
	if _, cached, err := c.GetJSON(context.Background(), "/repos/o/r", &d2, ""); err != nil || !cached || d2 != d {
		t.Errorf("detail repeat: err=%v cached=%v got %+v", err, cached, d2)
	}
	if st := c.CacheStats(); st.Entries != 2 {
		t.Errorf("one URL decoded into two types is %d entries, want one per type", st.Entries)
	}
	if calls.Load() != 4 {
		t.Errorf("the server saw %d requests, want the four the callers made", calls.Load())
	}
}

// TestAnEntryIsChargedWhatItHoldsPlusTheOverhead pins the arithmetic the
// bound is enforced with. The other cache tests compare the running total
// against the sum of the entries' own sizes, which agree with each other
// however size is computed; this one says what that size is.
func TestAnEntryIsChargedWhatItHoldsPlusTheOverhead(t *testing.T) {
	t.Parallel()
	k := newCache(1 << 20)
	k.put(cacheKey{url: "/repos/o/n"}, `"v1"`, []byte(`{"n":1}`))
	want := len("/repos/o/n") + len(`"v1"`) + len(`{"n":1}`) + cacheEntryOverhead
	if k.bytes != want {
		t.Errorf("one entry is charged %d bytes, want %d: the URL, the ETag, the body and the overhead", k.bytes, want)
	}
}

// TestTheLimitIsInclusive: a cache is full when it holds its limit, not when
// it holds one byte less. An entry the size of the whole limit is kept, two
// entries that add up to it exactly are both kept, and only the byte past it
// evicts, exactly once.
func TestTheLimitIsInclusive(t *testing.T) {
	t.Parallel()
	body := []byte(`{"n":1}`)
	a, b, c := cacheKey{url: "/a"}, cacheKey{url: "/b"}, cacheKey{url: "/c"}
	one := (&conditional{key: a, etag: `"1"`, body: body}).size()

	whole := newCache(one)
	whole.put(a, `"1"`, body)
	if etag, _ := whole.get(a); etag != `"1"` {
		t.Errorf("an entry exactly the size of the limit was not stored: %+v", whole)
	}

	k := newCache(2 * one)
	k.put(a, `"1"`, body)
	k.put(b, `"1"`, body)
	if len(k.entries) != 2 || k.bytes != 2*one || k.evicted != 0 {
		t.Errorf("two entries filling the limit exactly left %d entries, %d bytes and %d evictions, want 2, %d and 0",
			len(k.entries), k.bytes, k.evicted, 2*one)
	}
	k.put(c, `"1"`, body)
	if len(k.entries) != 2 || k.evicted != 1 {
		t.Errorf("a third entry left %d entries after %d evictions, want 2 after exactly 1", len(k.entries), k.evicted)
	}
	if etag, _ := k.get(a); etag != "" {
		t.Error("the entry evicted must be the least recently used, /a")
	}
}

// TestSetCacheLimitKeepsWhatStillFits: a new limit evicts only what no longer
// fits under it. A limit the cache already sits exactly at, or one above what
// it holds, costs nothing, and a limit one byte short of it costs exactly the
// least recently used entry. Evicting more would throw away pages the next
// sweep asks for and pay for each of them again.
func TestSetCacheLimitKeepsWhatStillFits(t *testing.T) {
	t.Parallel()
	body := []byte(`{"n":1}`)
	a, b := cacheKey{url: "/a"}, cacheKey{url: "/b"}
	one := (&conditional{key: a, etag: `"1"`, body: body}).size()

	k := newCache(1 << 20)
	k.put(a, `"1"`, body)
	k.put(b, `"1"`, body)
	for _, limit := range []int{4 * one, 2 * one} {
		k.setLimit(limit)
		if len(k.entries) != 2 || k.bytes != 2*one || k.evicted != 0 {
			t.Errorf("a limit of %d over %d bytes held left %d entries, %d bytes and %d evictions, want 2, %d and 0",
				limit, 2*one, len(k.entries), k.bytes, k.evicted, 2*one)
		}
	}
	k.setLimit(2*one - 1)
	if len(k.entries) != 1 || k.bytes != one || k.evicted != 1 {
		t.Errorf("a limit one byte short left %d entries, %d bytes and %d evictions, want 1, %d and exactly 1",
			len(k.entries), k.bytes, k.evicted, one)
	}
	if etag, _ := k.get(a); etag != "" {
		t.Error("the entry evicted must be the least recently used, /a")
	}
	if etag, _ := k.get(b); etag != `"1"` {
		t.Error("the entry that still fits, /b, must be kept")
	}
}

// TestANewCacheHonorsItsLimitAndDefaultsZero: a positive limit is the limit,
// and zero or less is the default, the same rule SetCacheLimit documents.
func TestANewCacheHonorsItsLimitAndDefaultsZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ limit, want int }{
		{4 << 10, 4 << 10},
		{1, 1},
		{0, DefaultCacheBytes},
		{-1, DefaultCacheBytes},
	} {
		if got := newCache(tc.limit).limit; got != tc.want {
			t.Errorf("newCache(%d) has a limit of %d, want %d", tc.limit, got, tc.want)
		}
	}
}

// TestHalfAPairForgetsTheWholeEntry: put refuses an ETag with no body and a
// body with no ETag, and drops what was stored for the key, because the old
// pair describes an answer the URL no longer gives. Every caller in the
// client checks the ETag before it puts, so this is the cache's own promise
// and not one the client happens to keep for it.
func TestHalfAPairForgetsTheWholeEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		etag string
		body []byte
	}{
		{"no ETag", "", []byte(`{"n":2}`)},
		{"no body", `"v2"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newCache(1 << 20)
			key := cacheKey{url: "/a"}
			k.put(key, `"v1"`, []byte(`{"n":1}`))
			k.put(key, tc.etag, tc.body)
			if etag, body := k.get(key); etag != "" || body != nil {
				t.Errorf("get = %q, %q, want the old pair forgotten", etag, body)
			}
			if len(k.entries) != 0 || k.order.Len() != 0 || k.bytes != 0 {
				t.Errorf("the cache still holds %d entries, %d in the order and %d bytes", len(k.entries), k.order.Len(), k.bytes)
			}
		})
	}
}

// TestAnUncachedRequestCarriesNoIfNoneMatchAtAll: with nothing stored the
// header must be absent, not present and empty. An empty validator is still
// a conditional request on the wire, and what a server makes of it is not
// this client's to guess.
func TestAnUncachedRequestCarriesNoIfNoneMatchAtAll(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	present := map[string]bool{}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_, present[r.URL.Path] = r.Header["If-None-Match"]
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	var out map[string]any
	if _, _, err := c.GetJSON(context.Background(), "/json", &out, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetText(context.Background(), "/text"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{"/json", "/text"} {
		if present[p] {
			t.Errorf("%s was asked with an If-None-Match header and nothing stored to answer it", p)
		}
	}
}

// TestGraphQLMapsA500ToTooLargeWhateverItsBody: the gateway's failure is
// recognized by the status as well as by the content type, so a 5xx is a
// query to shrink even when it arrives dressed as JSON. The boundary is the
// first 5xx.
func TestGraphQLMapsA500ToTooLargeWhateverItsBody(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
	})
	var out map[string]any
	err := c.GraphQL(context.Background(), "{ viewer { login } }", nil, &out)
	if tl, ok := errors.AsType[*TooLargeError](err); !ok || tl.Status != http.StatusInternalServerError {
		t.Errorf("err = %v, want TooLargeError 500", err)
	}
	if out != nil {
		t.Errorf("the body of a 500 was decoded: %v", out)
	}
}

// TestRateLimitedReadsTheEvidenceInTheHeaders covers every way GitHub says a
// budget is spent, and what the client makes of each: which bucket to blame
// and when to try again. The reset instant is what a waiting sweep sleeps
// until, so a header it cannot use must fall back to a pause, never to the
// epoch, which reads as a window that turned over half a century ago.
func TestRateLimitedReadsTheEvidenceInTheHeaders(t *testing.T) {
	t.Parallel()
	const resetAt = 1_800_000_000
	cases := []struct {
		name     string
		status   int
		headers  map[string]string
		resource string
		// reset is the exact instant, or the zero time when the client must
		// pick one itself, pause after the moment it was asked.
		reset time.Time
		pause time.Duration
	}{
		{
			name: "a spent bucket names itself and its reset", status: http.StatusForbidden,
			headers:  map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-resource": "search", "x-ratelimit-reset": strconv.Itoa(resetAt)},
			resource: "search", reset: time.Unix(resetAt, 0),
		},
		{
			name: "a spent bucket that names none is core", status: http.StatusForbidden,
			headers:  map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": strconv.Itoa(resetAt)},
			resource: "core", reset: time.Unix(resetAt, 0),
		},
		{
			name: "no reset waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"x-ratelimit-remaining": "0"},
			resource: "core", pause: time.Minute,
		},
		{
			name: "a zero reset waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": "0"},
			resource: "core", pause: time.Minute,
		},
		{
			name: "a negative reset waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": "-5"},
			resource: "core", pause: time.Minute,
		},
		{
			name: "an unreadable reset waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": "soon"},
			resource: "core", pause: time.Minute,
		},
		{
			name: "a secondary limit waits what Retry-After says", status: http.StatusForbidden,
			headers:  map[string]string{"retry-after": "30"},
			resource: "secondary", pause: 30 * time.Second,
		},
		{
			name: "a 429 is a secondary limit too", status: http.StatusTooManyRequests,
			headers:  map[string]string{"retry-after": "30"},
			resource: "secondary", pause: 30 * time.Second,
		},
		{
			name: "a zero Retry-After waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"retry-after": "0"},
			resource: "secondary", pause: time.Minute,
		},
		{
			name: "a negative Retry-After waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"retry-after": "-1"},
			resource: "secondary", pause: time.Minute,
		},
		{
			// Retry-After may also be an HTTP date, which this does not parse.
			name: "an unreadable Retry-After waits a minute", status: http.StatusForbidden,
			headers:  map[string]string{"retry-after": "Wed, 21 Oct 2015 07:28:00 GMT"},
			resource: "secondary", pause: time.Minute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"slow down"}`))
			})
			before := time.Now()
			_, _, err := c.GetJSON(context.Background(), "/repos/o/n", &struct{}{}, "")
			after := time.Now()
			limited, ok := errors.AsType[*RateLimitedError](err)
			if !ok {
				t.Fatalf("err = %v, want a RateLimitedError", err)
			}
			if limited.Resource != tc.resource || limited.Path != "/repos/o/n" {
				t.Errorf("RateLimitedError = %+v, want resource %q on /repos/o/n", limited, tc.resource)
			}
			if tc.pause == 0 {
				if !limited.Reset.Equal(tc.reset) {
					t.Errorf("reset = %v, want %v", limited.Reset, tc.reset)
				}
				return
			}
			if limited.Reset.Before(before.Add(tc.pause)) || limited.Reset.After(after.Add(tc.pause)) {
				t.Errorf("reset = %v, want %v after the request, between %v and %v",
					limited.Reset, tc.pause, before.Add(tc.pause), after.Add(tc.pause))
			}
		})
	}
}

// TestGetTextReportsASpentBudgetAsRateLimited: a log fetch refused for
// budget must not read as a log that is gone, or a sweep records every failed
// job of the window as having no log.
func TestGetTextReportsASpentBudgetAsRateLimited(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				rateHeaders(w, "core", 5000, 0, time.Unix(1_800_000_000, 0))
				w.WriteHeader(status)
			})
			_, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
			if _, ok := errors.AsType[*RateLimitedError](err); !ok {
				t.Errorf("err = %v, want a RateLimitedError", err)
			}
			if _, ok := errors.AsType[*UnavailableError](err); ok {
				t.Error("a spent budget must not also read as a missing log")
			}
		})
	}
}

// spentCore answers every request with a core budget of limit and remaining,
// resetting at reset, and counts the requests that got through the brake.
func spentCore(t *testing.T, limit, remaining int, reset time.Time) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		rateHeaders(w, "core", limit, remaining, reset)
		_, _ = w.Write([]byte(`{}`))
	})
	return c, &calls
}

// TestAZeroReserveNeverBrakes: SetReserve(0, ...) is how a caller turns the
// brake off, so a bucket reported empty still goes out and GitHub is the one
// to refuse it.
func TestAZeroReserveNeverBrakes(t *testing.T) {
	t.Parallel()
	c, calls := spentCore(t, 5000, 0, time.Now().Add(time.Hour))
	c.SetReserve(0, false)
	for i := range 2 {
		if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want both", n)
	}
}

// TestABucketWithNoLimitIsNotBraked: a budget that reports a limit of zero
// is one the client knows nothing about, and a fifth of nothing would refuse
// every request against it.
func TestABucketWithNoLimitIsNotBraked(t *testing.T) {
	t.Parallel()
	c, calls := spentCore(t, 0, 0, time.Now().Add(time.Hour))
	c.SetReserve(500, false)
	for i := range 2 {
		if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want both", n)
	}
}

// TestTheReserveItselfIsNeverSpent: remaining equal to the reserve is the
// reserve, so the brake refuses there and not one request later.
func TestTheReserveItselfIsNeverSpent(t *testing.T) {
	t.Parallel()
	c, calls := spentCore(t, 5000, 100, time.Now().Add(time.Hour))
	c.SetReserve(100, false)
	if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, _, err := c.GetJSON(context.Background(), "/b", &struct{}{}, "")
	if _, ok := errors.AsType[*RateLimitedError](err); !ok {
		t.Errorf("with 100 left and a reserve of 100: err = %v, want a RateLimitedError", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want the brake to stop the second", n)
	}
}

// TestAWindowThatTurnedOverReleasesTheBrake: an empty bucket whose reset has
// passed is full again, whatever the last response said. The client assumes
// so rather than refusing until some response says it, which no refused
// request would ever produce.
func TestAWindowThatTurnedOverReleasesTheBrake(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Only the first answer reports a budget, so what the client holds
		// afterwards is what the brake wrote and not a later reading.
		if calls.Add(1) == 1 {
			rateHeaders(w, "core", 5000, 0, time.Unix(1_000_000_000, 0))
		}
		_, _ = w.Write([]byte(`{}`))
	})
	c.SetReserve(500, false)
	for i := range 2 {
		if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want both", n)
	}
	if st, _ := c.RateFor("core"); st.Remaining != 5000 {
		t.Errorf("core = %+v, want the bucket assumed full again", st)
	}
}

// TestAWaitingBrakeSaysSoAndStopsWithTheContext: with wait on, a spent
// bucket blocks instead of refusing, tells OnWait which bucket and for how
// long first, and gives up when the caller's context does. The context is
// canceled before the call, which makes that deterministic: OnWait comes
// before the wait, and the wait is an hour that is never slept.
func TestAWaitingBrakeSaysSoAndStopsWithTheContext(t *testing.T) {
	t.Parallel()
	c, calls := spentCore(t, 5000, 10, time.Now().Add(time.Hour))
	c.SetReserve(500, true)
	if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
		t.Fatalf("first request: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var told int
	var bucket string
	var waited time.Duration
	c.OnWait = func(b string, d time.Duration) {
		told++
		bucket, waited = b, d
	}
	_, _, err := c.GetJSON(canceled, "/b", &struct{}{}, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context's cancellation", err)
	}
	if told != 1 || bucket != "core" || waited <= 0 || waited > time.Hour {
		t.Errorf("OnWait was called %d times, last with (%q, %v), want once with core and the time left until the reset", told, bucket, waited)
	}

	// With no OnWait the brake still waits, silently.
	c.OnWait = nil
	if _, _, silent := c.GetJSON(canceled, "/c", &struct{}{}, ""); !errors.Is(silent, context.Canceled) {
		t.Errorf("err = %v, want the context's cancellation", silent)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want only the first", n)
	}
}

// TestABudgetWithNoResetReportsNoResetInstant: a response that carries the
// counts and no reset leaves the instant unknown, the zero time, rather than
// the epoch a missing header parses to.
func TestABudgetWithNoResetReportsNoResetInstant(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-limit", "5000")
		w.Header().Set("x-ratelimit-remaining", "4000")
		w.Header().Set("x-ratelimit-reset", "0")
		_, _ = w.Write([]byte(`{}`))
	})
	if _, _, err := c.GetJSON(context.Background(), "/a", &struct{}{}, ""); err != nil {
		t.Fatal(err)
	}
	if st, ok := c.RateFor("core"); !ok || st.Remaining != 4000 || !st.Reset.IsZero() {
		t.Errorf("core = %+v %v, want the counts and no reset instant", st, ok)
	}
}

// TestARequestThatCannotBeBuiltFailsBeforeAnythingIsSent covers the three
// requests the client builds, each from a URL that does not parse.
func TestARequestThatCannotBeBuiltFailsBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, srv := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	const bad = "/repos/o/n\x7f"
	// The parser's own complaint, so a failure that comes from somewhere
	// later, after a request went out, does not pass for this one.
	const unparsable = "invalid control character in URL"
	if _, _, err := c.GetJSON(context.Background(), bad, &struct{}{}, ""); err == nil || !strings.Contains(err.Error(), unparsable) {
		t.Errorf("GetJSON: err = %v, want the URL refused", err)
	}
	if _, err := c.GetText(context.Background(), bad); err == nil || !strings.Contains(err.Error(), unparsable) {
		t.Errorf("GetText: err = %v, want the URL refused", err)
	}
	c.SetBaseURL(srv.URL + "\x7f")
	if err := c.GraphQL(context.Background(), "{ viewer { login } }", nil, nil); err == nil || !strings.Contains(err.Error(), unparsable) {
		t.Errorf("GraphQL: err = %v, want the base URL refused", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the server saw %d requests, want none", n)
	}
}

// TestGraphQLVariablesThatCannotBeEncodedFailBeforeSending: the payload is
// JSON, and a variable json cannot encode is the caller's mistake to hear
// about, not a request to send half built.
func TestGraphQLVariablesThatCannotBeEncodedFailBeforeSending(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	err := c.GraphQL(context.Background(), "{ viewer { login } }", map[string]any{"wake": make(chan int)}, nil)
	if _, ok := errors.AsType[*json.UnsupportedTypeError](err); !ok {
		t.Errorf("err = %v, want the encoder's refusal of the channel", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the server saw %d requests, want none", n)
	}
}

// truncated declares a longer body than it writes, so the client's read
// fails partway, the way a connection dropped mid-answer does.
func truncated(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("ETag", `"cut"`)
	w.Header().Set("Content-Length", "100")
	_, _ = w.Write([]byte(`{"n":1`))
}

// TestABodyCutShortIsAnErrorAndIsNotCached: half an answer is not an empty
// one. Returned as success it would publish nothing, and cached beside its
// ETag it would be replayed on every 304 from then on.
func TestABodyCutShortIsAnErrorAndIsNotCached(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, truncated)
	var out map[string]any
	// The read's own error and not the decoder's: half an object is also bad
	// JSON, and that would fail GetJSON even if the read error were dropped.
	if _, _, err := c.GetJSON(context.Background(), "/json", &out, ""); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("GetJSON: err = %v, want the body reported cut short", err)
	}
	if _, err := c.GetText(context.Background(), "/text"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("GetText: err = %v, want the body reported cut short", err)
	}
	if st := c.CacheStats(); st.Entries != 0 {
		t.Errorf("a truncated body was cached: %+v", st)
	}
}

// TestGraphQLRateReportsAFailureRatherThanAnEmptyBudget: a zero budget is a
// reading, and a sweep that took a failed probe for one would believe the
// bucket spent.
func TestGraphQLRateReportsAFailureRatherThanAnEmptyBudget(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
	})
	st, err := c.GraphQLRate(context.Background())
	if _, ok := errors.AsType[*TooLargeError](err); !ok {
		t.Errorf("err = %v, want the gateway's failure", err)
	}
	if st != (RateState{}) {
		t.Errorf("state = %+v, want nothing alongside the error", st)
	}
}

// TestGraphQLAnswersThatCarryNoBudget covers the answers the block is not
// read from, and what the client records instead: the headers, and no spend.
func TestGraphQLAnswersThatCarryNoBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, query, body string
		// wantErr is a substring of the error, empty for none.
		wantErr string
	}{
		{
			// Mutations are never given the block, so there is none to read.
			name: "a mutation", query: `mutation { addStar(input: {starrableId: "x"}) { clientMutationId } }`,
			body: `{"data":{"addStar":{"clientMutationId":null}}}`,
		},
		{
			// The end-to-end fake GitHub answers every request with a block,
			// asked for or not. A document the client could not give one is
			// not priced by whatever comes back under that key: the headers
			// stay the reading, and nothing is counted as spent.
			name: "a block the document never asked for", query: `mutation { addStar(input: {starrableId: "x"}) { clientMutationId } }`,
			body: `{"data":{"ghcRateLimit":{"limit":5000,"cost":3,"used":10,"remaining":4990,"resetAt":"2026-01-01T00:00:00Z"},"addStar":{"clientMutationId":null}}}`,
		},
		{
			name: "errors and no data", query: "query { viewer { login } }",
			body:    `{"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`,
			wantErr: "Something went wrong",
		},
		{
			// A block of the wrong shape is ignored rather than failing an
			// answer whose data the caller can still use.
			name: "a block that does not decode", query: "query { viewer { login } }",
			body: `{"data":{"ghcRateLimit":"spent","viewer":{"login":"octocat"}}}`,
		},
		{
			name: "neither data nor errors", query: "query { viewer { login } }",
			body:    `{}`,
			wantErr: "no data in the response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				rateHeaders(w, "graphql", 5000, 4321, time.Time{})
				_, _ = w.Write([]byte(tc.body))
			})
			var out map[string]any
			err := c.GraphQL(context.Background(), tc.query, nil, &out)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one saying %q", err, tc.wantErr)
			}
			if st, ok := c.RateFor("graphql"); !ok || st.Remaining != 4321 {
				t.Errorf("graphql = %+v %v, want the headers' reading", st, ok)
			}
			if spend := c.GraphQLSpend(); spend != (GraphQLSpend{}) {
				t.Errorf("spend = %+v, want nothing counted without a block to price it", spend)
			}
		})
	}
}

// TestWithRateLimitReadsNamesStringsAndCommentsAsTheSpecDoes: the root brace
// is found by walking the document, and each case here is a token the walk
// has to step over correctly before it gets there. A parenthesis hidden in a
// string, or a string end missed, leaves the walk inside the variable list,
// where no brace is structure, and the block is never placed.
func TestWithRateLimitReadsNamesStringsAndCommentsAsTheSpecDoes(t *testing.T) {
	t.Parallel()
	const body = ` { viewer { login } }`
	placed := func(head string) string { return head + ` { ` + rateLimitBlock + ` viewer { login } }` }
	cases := []struct{ name, head string }{
		{"a named operation", "query Viewer"},
		{"a named operation with a list type and an underscore", "query Viewer($ids: [ID!]!, $first_page: Int)"},
		{"an empty comment line", "#\nquery"},
		{"a string holding a parenthesis", `query($q: String = "(")`},
		{"a string holding an escaped quote", `query($q: String = "\"(")`},
		{"a block string holding a quote", `query($q: String = """a"b(""")`},
		{"a longer block string", `query($q: String = """((((a"b(""")`},
		{"an empty block string", `query($q: String = """""")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := withRateLimit(tc.head + body)
			if want := placed(tc.head); !ok || got != want {
				t.Errorf("withRateLimit(%q)\n got %q %v\nwant %q", tc.head+body, got, ok, want)
			}
		})
	}
}

// TestWithRateLimitSendsADocumentItCannotFinishReadingUnchanged: a document
// that ends inside a comment, a string or right after a name has no root
// brace to find. It goes out as it came, and the walk must not read past its
// end to learn that.
func TestWithRateLimitSendsADocumentItCannotFinishReadingUnchanged(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"# nothing but a comment",
		"query Viewer",
		`query($q: String = "(`,
		`query($q: String = "\`,
		`query($q: String = """(`,
	} {
		got, ok := withRateLimit(q)
		if ok || got != q {
			t.Errorf("withRateLimit(%q) = %q %v, want it unchanged", q, got, ok)
		}
	}
}

// TestWithRateLimitReadsTheKeywordAsAWholeName: a definition is a query
// when its first name is query, not when its first name has query in it.
// Each document here puts one of the bytes at the edge of a name's ranges
// against the keyword, so a walk that stopped a name one byte early would
// read the rest as the keyword and put the block where no query is.
func TestWithRateLimitReadsTheKeywordAsAWholeName(t *testing.T) {
	t.Parallel()
	for _, word := range []string{"_query", "aquery", "queryz", "Aquery", "queryZ", "query0", "query9"} {
		q := word + " { viewer { login } }"
		if got, ok := withRateLimit(q); ok || got != q {
			t.Errorf("withRateLimit(%q) = %q %v, want it unchanged: %s is not the query keyword", q, got, ok, word)
		}
	}
}

// TestWithRateLimitSkipsADefinitionOnlyAtItsOwnClosingBrace: the braces of a
// fragment or a mutation are counted as they nest, so the brace that closes
// its first inner selection does not end it. Ended early, the rest of its
// selections reads as a new definition, and an inline fragment with no type
// condition, `... { }`, is then exactly the anonymous query shorthand: the
// block would land inside the fragment, or inside a mutation, where
// rateLimit does not exist.
func TestWithRateLimitSkipsADefinitionOnlyAtItsOwnClosingBrace(t *testing.T) {
	t.Parallel()
	const fragment = `fragment F on Repository { owner { login } ... { name } } `
	const query = `query { r0: repository { ...F } }`
	got, ok := withRateLimit(fragment + query)
	if want := fragment + `query { ` + rateLimitBlock + ` r0: repository { ...F } }`; !ok || got != want {
		t.Errorf("withRateLimit(%q)\n got %q %v\nwant %q", fragment+query, got, ok, want)
	}
	const m = `mutation { addStar(input: {starrableId: "x"}) { starrable { id } ... { clientMutationId } } }`
	if kept, touched := withRateLimit(m); touched || kept != m {
		t.Errorf("withRateLimit(%q) = %q %v, want the mutation untouched", m, kept, touched)
	}
}

// TestGetTextAbsoluteURLIsUsedAsIs: the log a job points at can be a full
// URL, and it is fetched where it points rather than under the API base.
func TestGetTextAbsoluteURLIsUsedAsIs(t *testing.T) {
	t.Parallel()
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/absolute" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("the log"))
	})
	c.SetBaseURL("http://127.0.0.1:1") // nothing listens here
	got, err := c.GetText(context.Background(), srv.URL+"/absolute")
	if err != nil {
		t.Fatal(err)
	}
	if got != "the log" {
		t.Errorf("text = %q", got)
	}
}

// TestGetTextIsBrakedLikeGetJSON: a failed job's log is a core request like
// any other, and a sweep with a thousand failed jobs spends the reserve on
// logs as readily as on anything else.
func TestGetTextIsBrakedLikeGetJSON(t *testing.T) {
	t.Parallel()
	c, calls := spentCore(t, 5000, 10, time.Now().Add(time.Hour))
	c.SetReserve(500, false)
	if _, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/2/logs")
	if limited, ok := errors.AsType[*RateLimitedError](err); !ok || limited.Resource != "core" {
		t.Errorf("err = %v, want the core budget refused before sending", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want the brake to stop the second", n)
	}
}
