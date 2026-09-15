package grafana

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// recorded is one request a fake Grafana received, decoded.
type recorded struct {
	method, path, contentType, auth string
	body                            map[string]any
}

// fakeGrafana answers every request with status and body, and hands each
// request it received to the channel it returns.
func fakeGrafana(t *testing.T, status int, body string) (Client, <-chan recorded) {
	t.Helper()
	seen := make(chan recorded, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		req := recorded{
			method: r.Method, path: r.URL.Path,
			contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"),
		}
		if err = json.Unmarshal(raw, &req.body); err != nil {
			t.Errorf("the request body is not JSON: %v", err)
		}
		seen <- req
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return Client{URL: srv.URL, Token: "secret"}, seen
}

// TestNewReadsTheEnvironment covers both variables and the development address
// an unset GRAFANA_URL falls back to.
func TestNewReadsTheEnvironment(t *testing.T) {
	t.Setenv("GRAFANA_URL", "")
	t.Setenv("GRAFANA_TOKEN", "")
	if got := New(); got.URL != DefaultURL || got.Token != "" {
		t.Errorf("New() with nothing set = %+v, want the default URL and no token", got)
	}
	t.Setenv("GRAFANA_URL", "http://grafana.test:3000")
	t.Setenv("GRAFANA_TOKEN", "glsa_x")
	if got := New(); got.URL != "http://grafana.test:3000" || got.Token != "glsa_x" {
		t.Errorf("New() = %+v, want what the environment says", got)
	}
}

// TestPostSendsJSONWithTheToken pins the request itself: a POST to the path
// given, a JSON body, and the token as a bearer credential.
func TestPostSendsJSONWithTheToken(t *testing.T) {
	t.Parallel()
	c, seen := fakeGrafana(t, http.StatusOK, `{"status":"success"}`)
	got, err := c.Post(t.Context(), "/api/dashboards/db", map[string]any{"overwrite": true}, 5*time.Second)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got["status"] != "success" {
		t.Errorf("answer = %v, want the decoded body", got)
	}
	req := <-seen
	if req.method != http.MethodPost || req.path != "/api/dashboards/db" {
		t.Errorf("request = %s %s, want POST /api/dashboards/db", req.method, req.path)
	}
	if req.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentType)
	}
	if req.auth != "Bearer secret" {
		t.Errorf("Authorization = %q, want the token as a bearer credential", req.auth)
	}
	if req.body["overwrite"] != true {
		t.Errorf("body = %v, want what was posted", req.body)
	}
}

// TestPostWithoutATokenSendsNoCredential keeps an anonymous request anonymous
// rather than sending an empty bearer header.
func TestPostWithoutATokenSendsNoCredential(t *testing.T) {
	t.Parallel()
	c, seen := fakeGrafana(t, http.StatusOK, `{}`)
	c.Token = ""
	if _, err := c.Post(t.Context(), "/x", map[string]any{}, 5*time.Second); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if req := <-seen; req.auth != "" {
		t.Errorf("Authorization = %q, want none without a token", req.auth)
	}
}

// TestPostReadsARejectionOutOfTheBody is the reason a 4xx is not an error:
// Grafana explains a refused query in the JSON it answers with.
func TestPostReadsARejectionOutOfTheBody(t *testing.T) {
	t.Parallel()
	c, _ := fakeGrafana(t, http.StatusBadRequest, `{"message":"bad query"}`)
	got, err := c.Post(t.Context(), "/x", map[string]any{}, 5*time.Second)
	if err != nil {
		t.Fatalf("Post: %v, want the rejection as an answer", err)
	}
	if got["message"] != "bad query" {
		t.Errorf("answer = %v, want the rejection Grafana wrote", got)
	}
}

// TestPostAnswersAnEmptyBodyWithAnEmptyMap covers a reply with nothing in it,
// which is an answer and not a decoding failure.
func TestPostAnswersAnEmptyBodyWithAnEmptyMap(t *testing.T) {
	t.Parallel()
	c, _ := fakeGrafana(t, http.StatusOK, " \n")
	got, err := c.Post(t.Context(), "/x", map[string]any{}, 5*time.Second)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("Post = %v, %v, want an empty map and no error", got, err)
	}
}

// TestPostReportsABodyThatIsNotJSON names the status and the start of the body,
// which is all a proxy's error page has to say.
func TestPostReportsABodyThatIsNotJSON(t *testing.T) {
	t.Parallel()
	page := "<html>" + strings.Repeat("x", 400) + "</html>"
	c, _ := fakeGrafana(t, http.StatusBadGateway, page)
	_, err := c.Post(t.Context(), "/x", map[string]any{}, 5*time.Second)
	if err == nil {
		t.Fatal("Post accepted an HTML page as an answer")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "502 Bad Gateway: <html>") {
		t.Errorf("error = %q, want the status and the start of the body", msg)
	}
	if len(msg) > len("502 Bad Gateway: ")+200 {
		t.Errorf("error is %d bytes long, want the body cut at 200 characters", len(msg))
	}
}

// TestPostFailsBeforeSendingWhatCannotBeSent covers the three failures that
// happen on this side of the wire: a body JSON cannot encode, a URL a request
// cannot be built from, and a server that is not there.
func TestPostFailsBeforeSendingWhatCannotBeSent(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for name, tc := range map[string]struct {
		c    Client
		body any
	}{
		"a body JSON cannot encode":  {Client{URL: closed.URL}, map[string]any{"f": func() {}}},
		"a URL with a control byte":  {Client{URL: "http://grafana\x7f.test"}, map[string]any{}},
		"a server that is not there": {Client{URL: closed.URL}, map[string]any{}},
	} {
		if got, err := tc.c.Post(t.Context(), "/x", tc.body, time.Second); err == nil {
			t.Errorf("%s: Post = %v, want an error", name, got)
		}
	}
}

// TestPostReportsABodyCutShort covers a reply that announces more than it
// sends, which io.ReadAll reports rather than decoding half a document.
func TestPostReportsABodyCutShort(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hand over its connection")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijacking the connection: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"a\":")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	if _, err := (Client{URL: srv.URL}).Post(t.Context(), "/x", map[string]any{}, 5*time.Second); err == nil {
		t.Error("Post accepted a body cut short")
	}
}

// TestPostGivesUpAtTheTimeout proves the timeout is the request's whole life:
// a server that never answers is abandoned rather than waited on.
func TestPostGivesUpAtTheTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	start := time.Now()
	_, err := (Client{URL: srv.URL}).Post(t.Context(), "/x", map[string]any{}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Post returned an answer from a server that never gave one")
	}
	if netErr, ok := errors.AsType[net.Error](err); !ok || !netErr.Timeout() {
		t.Errorf("error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Post took %v, want it to stop at its 50ms timeout", elapsed)
	}
}

// TestQueryPostsOneTargetTheWayARenderDoes pins the body of /api/ds/query: the
// range, and the target inside a list of one.
func TestQueryPostsOneTargetTheWayARenderDoes(t *testing.T) {
	t.Parallel()
	c, seen := fakeGrafana(t, http.StatusOK, `{"results":{}}`)
	if _, err := c.Query(t.Context(), "now-6h", "now", map[string]any{"refId": "A"}, 5*time.Second); err != nil {
		t.Fatalf("Query: %v", err)
	}
	req := <-seen
	if req.path != "/api/ds/query" {
		t.Errorf("path = %q, want /api/ds/query", req.path)
	}
	queries, _ := req.body["queries"].([]any)
	if req.body["from"] != "now-6h" || req.body["to"] != "now" || len(queries) != 1 {
		t.Errorf("body = %v, want the range and one query", req.body)
	}
}

// TestWalkDescendsIntoRows yields every target of every panel in document
// order, from both shapes a panel list arrives in, and skips what is not an
// object.
func TestWalkDescendsIntoRows(t *testing.T) {
	t.Parallel()
	decoded := []any{
		map[string]any{"title": "Stars", "targets": []any{
			map[string]any{"refId": "A"}, "not a target", map[string]any{"refId": "B"},
		}},
		"not a panel",
		map[string]any{"type": "row", "panels": []map[string]any{
			{"title": "Alerts", "targets": []map[string]any{{"refId": "C"}}},
		}},
		map[string]any{"title": "Text"},
	}
	var got []string
	for _, p := range Walk(decoded) {
		ref, _ := p.Target["refId"].(string)
		got = append(got, p.Title+"/"+ref)
	}
	want := []string{"Stars/A", "Stars/B", "Alerts/C"}
	if !slices.Equal(got, want) {
		t.Errorf("Walk = %v, want %v", got, want)
	}
	if Walk("not a list") != nil {
		t.Error("Walk over something that is not a list yielded panels")
	}
}

// TestFramesCountsRowsAndFindsTheError covers where Grafana puts an error: on
// the query itself, structured or not, or at the top of the reply.
func TestFramesCountsRowsAndFindsTheError(t *testing.T) {
	t.Parallel()
	frame := func(rows ...any) map[string]any {
		return map[string]any{"data": map[string]any{"values": []any{rows, rows}}}
	}
	for _, tc := range []struct {
		name     string
		res      map[string]any
		rows     int
		errorMsg string
	}{
		{
			name: "rows across frames",
			res: map[string]any{"results": map[string]any{"A": map[string]any{
				"frames": []any{frame(1, 2), frame(3), map[string]any{"data": map[string]any{}}},
			}}},
			rows: 3,
		},
		{
			name: "an error on the query",
			res: map[string]any{"results": map[string]any{"A": map[string]any{
				"error": "column not found", "frames": []any{frame(1)},
			}}, "message": "ignored"},
			rows: 1, errorMsg: "column not found",
		},
		{
			name: "a structured error",
			res: map[string]any{"results": map[string]any{"A": map[string]any{
				"error": map[string]any{"code": "x"},
			}}},
			errorMsg: "map[code:x]",
		},
		{
			name:     "an error at the top",
			res:      map[string]any{"message": "datasource not found"},
			errorMsg: "datasource not found",
		},
		{name: "nothing at all", res: map[string]any{}},
	} {
		rows, errText := Frames(tc.res, "A")
		if rows != tc.rows || errText != tc.errorMsg {
			t.Errorf("%s: Frames = %d, %q, want %d, %q", tc.name, rows, errText, tc.rows, tc.errorMsg)
		}
	}
}

// TestColumnReadsTheFirstFrame reads one column as strings and refuses a reply
// that does not have it.
func TestColumnReadsTheFirstFrame(t *testing.T) {
	t.Parallel()
	res := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{
		map[string]any{"data": map[string]any{"values": []any{
			[]any{"octocat/hello-world", "octocat/spoon-knife"},
			[]any{float64(3), true},
		}}},
	}}}}
	got, err := Column(res, "A", 1)
	if err != nil {
		t.Fatalf("Column: %v", err)
	}
	if !slices.Equal(got, []string{"3", "true"}) {
		t.Errorf("Column = %v, want every value as a string", got)
	}
	if _, err = Column(res, "A", 2); err == nil {
		t.Error("Column read a column the answer does not have")
	}
	if _, err = Column(map[string]any{}, "A", 0); err == nil {
		t.Error("Column read a column out of an answer with no frames")
	}
}

// TestTruthyFollowsThePythonTest pins the test the field is put to: absent,
// empty and zero fall through, everything else is an error.
func TestTruthyFollowsThePythonTest(t *testing.T) {
	t.Parallel()
	for _, v := range []any{nil, "", false, float64(0), []any{}, map[string]any{}} {
		if truthy(v) {
			t.Errorf("truthy(%#v) = true, want false", v)
		}
	}
	for _, v := range []any{"x", true, float64(1), []any{1}, map[string]any{"a": 1}, 3} {
		if !truthy(v) {
			t.Errorf("truthy(%#v) = false, want true", v)
		}
	}
}

// TestTrimCountsCharacters cuts where the text reads, never inside a
// character, and leaves a short string alone.
func TestTrimCountsCharacters(t *testing.T) {
	t.Parallel()
	if got := Trim("déjà vu", 4); got != "déjà" {
		t.Errorf("Trim = %q, want the first four characters", got)
	}
	if got := Trim("short", 10); got != "short" {
		t.Errorf("Trim = %q, want a short string unchanged", got)
	}
}

// TestTrimReturnsAMessageThatFitsByteForByte hands back a message exactly as
// long as the limit untouched. Going through runes rewrites a byte that is not
// UTF-8 as the replacement character, so a body that already fits would be
// reported as something Grafana never said.
func TestTrimReturnsAMessageThatFitsByteForByte(t *testing.T) {
	t.Parallel()
	if got := Trim("ok\xff", 3); got != "ok\xff" {
		t.Errorf("Trim = %q, want the three bytes that fit unchanged", got)
	}
}
