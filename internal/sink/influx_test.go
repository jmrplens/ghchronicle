package sink

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// influxServer is an InfluxDB write endpoint that refuses to parse any batch
// holding a line with "unparseable" in it, the way InfluxDB refuses a whole
// write over one bad line, and keeps every batch it accepted.
type influxServer struct {
	mu       sync.Mutex
	accepted []string
	requests []influxRequest
	status   int
}

// influxRequest is what the endpoint saw of one write.
type influxRequest struct{ path, query, auth, contentType string }

// newInfluxServer starts the endpoint. A non-zero status answers every write
// with it instead.
func newInfluxServer(t *testing.T, status int) (*influxServer, string) {
	t.Helper()
	s := &influxServer{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the write: %v", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, influxRequest{
			r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"),
		})
		switch {
		case s.status != 0:
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, "  internal error  ")
		case strings.Contains(string(body), "unparseable"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"invalid","message":"unable to parse 'x': parse failed"}`)
		default:
			s.accepted = append(s.accepted, string(body))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// written is every line the endpoint accepted, over every request.
func (s *influxServer) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.accepted {
		out = append(out, strings.Split(b, "\n")...)
	}
	return out
}

// sent is every write the endpoint saw.
func (s *influxServer) sent() []influxRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]influxRequest(nil), s.requests...)
}

// star is one point for repo, which the fake refuses when repo says so.
func star(repo string) Point {
	return Point{
		Measurement: "gh_star",
		Tags:        map[string]string{"repo": repo},
		Fields:      map[string]any{"starred": 1},
		Time:        time.Unix(1700000000, 0),
	}
}

// TestInfluxPostsLineProtocolInBatches sends each batch to the v2 write
// endpoint with the token, the org, the bucket and nanosecond precision.
func TestInfluxPostsLineProtocolInBatches(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	i := NewInflux(url+"/", "tok", "acme", "gh", 2, time.Second)
	i.Exclude = map[string]bool{"gh_log": true}
	points := []Point{
		star("a"), star("b"), star("c"),
		{Measurement: "gh_log", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"line": "x"}},
		{Measurement: "gh_star", Tags: map[string]string{"repo": "no-fields"}},
	}
	if _, err := i.Write(t.Context(), points); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := s.written(); len(got) != 3 || !strings.HasPrefix(got[0], "gh_star,repo=a starred=1i 1700000000000000000") {
		t.Errorf("written = %q, want the three stars and nothing excluded or empty", got)
	}
	sent := s.sent()
	if len(sent) != 2 {
		t.Fatalf("%d requests, want two batches of at most two lines", len(sent))
	}
	r := sent[0]
	if r.path != "/api/v2/write" || r.query != "org=acme&bucket=gh&precision=ns" {
		t.Errorf("wrote to %s?%s, want the v2 endpoint for acme/gh at nanosecond precision", r.path, r.query)
	}
	if r.auth != "Token tok" || r.contentType != "text/plain; charset=utf-8" {
		t.Errorf("request = %+v, want the token and plain text", r)
	}
}

// TestInfluxBisectsOutTheLineItCannotParse writes every good line of a batch
// the server refused over one bad one, and reports the bad one by content.
func TestInfluxBisectsOutTheLineItCannotParse(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	i := NewInflux(url, "tok", "acme", "gh", 0, 0)
	var rejected []string
	i.OnReject = func(line string) { rejected = append(rejected, line) }
	long := "unparseable" + strings.Repeat("x", 400)
	points := []Point{star("a"), star("b"), star(long), star("c"), star("d")}
	_, err := i.Write(t.Context(), points)
	var re *RejectedError
	if !errors.As(err, &re) || re.N != 1 {
		t.Fatalf("Write = %v, want one line reported as rejected", err)
	}
	if err.Error() != "1 lines were rejected as unparseable and skipped; everything else was written" {
		t.Errorf("error = %q", err)
	}
	if got := s.written(); len(got) != 4 {
		t.Errorf("written = %q, want the four good lines", got)
	}
	if len(rejected) != 1 || !strings.HasSuffix(rejected[0], "...") || len(rejected[0]) != 303 ||
		!strings.Contains(rejected[0], "unparseable") {
		t.Errorf("rejected = %q, want the bad line, cut at 300 characters", rejected)
	}
}

// TestInfluxWithoutARejectHookStillSkipsTheLine keeps writing when nobody
// asked to hear about a rejected line.
func TestInfluxWithoutARejectHookStillSkipsTheLine(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	_, err := NewInflux(url, "tok", "acme", "gh", 0, 0).Write(t.Context(), []Point{star("unparseable"), star("a")})
	if _, ok := errors.AsType[*RejectedError](err); !ok || len(s.written()) != 1 {
		t.Errorf("Write = %v with %q written, want the good line written and the bad one counted", err, s.written())
	}
}

// TestInfluxFailsOnAnythingButAParseRejection returns a server failure as it
// is, from the first request and from inside a bisection, without retrying
// the rest.
func TestInfluxFailsOnAnythingButAParseRejection(t *testing.T) {
	t.Parallel()
	_, url := newInfluxServer(t, http.StatusInternalServerError)
	_, err := NewInflux(url, "tok", "acme", "gh", 0, 0).Write(t.Context(), []Point{star("a")})
	if err == nil || err.Error() != "influx write: 500 Internal Server Error: internal error" {
		t.Errorf("Write = %v, want the status and the trimmed body", err)
	}

	// A server that refuses to parse the batch and then fails outright.
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "parse failed")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, err = NewInflux(srv.URL, "tok", "acme", "gh", 0, 0).Write(t.Context(), []Point{star("a"), star("b"), star("c")})
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Errorf("Write = %v, want the failure met while bisecting", err)
	}
}

// TestInfluxRefusesAnEndpointItCannotUse names the configured URL rather than
// sending anything, and reports a server that is not there.
func TestInfluxRefusesAnEndpointItCannotUse(t *testing.T) {
	t.Parallel()
	if _, err := NewInflux("ftp://influx.test", "", "o", "b", 0, 0).Write(t.Context(), []Point{star("a")}); err == nil ||
		!strings.HasPrefix(err.Error(), "influx write: endpoint ") {
		t.Errorf("Write = %v, want the endpoint refused", err)
	}
	if _, err := NewInflux("http://influx\x7f.test", "", "o", "b", 0, 0).Write(t.Context(), []Point{star("a")}); err == nil {
		t.Error("Write accepted a URL a request cannot be built from")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := NewInflux(closed.URL, "", "o", "b", 0, 0).Write(t.Context(), []Point{star("a")}); err == nil {
		t.Error("Write reported success to a server that is not there")
	}
}

// TestNewInfluxFillsInTheDefaults gives a zero batch and timeout their
// documented values, and names the sink.
func TestNewInfluxFillsInTheDefaults(t *testing.T) {
	t.Parallel()
	i := NewInflux("http://influx.test/", "", "o", "b", 0, 0)
	if i.Batch != 5000 || i.client.Timeout != 60*time.Second || i.URL != "http://influx.test" {
		t.Errorf("NewInflux = batch %d, timeout %v, URL %q, want 5000, 60s and no trailing slash",
			i.Batch, i.client.Timeout, i.URL)
	}
	if i.Name() != "influxdb" || i.Close() != nil {
		t.Errorf("Name = %q, want influxdb, and a Close with nothing to close", i.Name())
	}
}

// TestInfluxSendsNothingWhenNothingRenders makes no request for a batch that
// renders no line, and none for a batch that fills its last request exactly:
// an empty write is still a request InfluxDB 3 answers with a file.
func TestInfluxSendsNothingWhenNothingRenders(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	i := NewInflux(url, "tok", "acme", "gh", 2, 0)
	if _, err := i.Write(t.Context(), []Point{{Measurement: "gh_star", Tags: map[string]string{"repo": "a"}}}); err != nil {
		t.Fatal(err)
	}
	if n := len(s.sent()); n != 0 {
		t.Errorf("%d requests for a batch with no line, want none", n)
	}
	if _, err := i.Write(t.Context(), []Point{star("a"), star("b")}); err != nil {
		t.Fatal(err)
	}
	if n := len(s.sent()); n != 1 {
		t.Errorf("%d requests for two lines in batches of two, want one", n)
	}
}

// TestInfluxReportsARejectedLineAtItsLimitWhole cuts a rejected line only
// when it is longer than 300 characters.
func TestInfluxReportsARejectedLineAtItsLimitWhole(t *testing.T) {
	t.Parallel()
	var got []string
	i := &Influx{OnReject: func(line string) { got = append(got, line) }}
	exact := strings.Repeat("x", 300)
	i.reject(exact)
	i.reject("short")
	if len(got) != 2 || got[0] != exact || got[1] != "short" {
		t.Errorf("rejected = %q, want both lines as they were", got)
	}
}

// TestInfluxTreatsAnyNonSuccessStatusAsAFailure includes 300, the first status
// that is not a success.
func TestInfluxTreatsAnyNonSuccessStatusAsAFailure(t *testing.T) {
	t.Parallel()
	_, url := newInfluxServer(t, http.StatusMultipleChoices)
	_, err := NewInflux(url, "tok", "acme", "gh", 0, 0).Write(t.Context(), []Point{star("a")})
	if err == nil || !strings.HasPrefix(err.Error(), "influx write: 300") {
		t.Errorf("Write = %v, want the 300 reported", err)
	}
}

// TestInfluxStopsBisectingAtAFailureDeepInside returns a server failure met
// two halvings down, without writing the rest of the batch.
func TestInfluxStopsBisectingAtAFailureDeepInside(t *testing.T) {
	t.Parallel()
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "parse failed")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, err := NewInflux(srv.URL, "tok", "acme", "gh", 0, 0).Write(t.Context(), []Point{star("a"), star("b"), star("c"), star("d")})
	mu.Lock()
	defer mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "503") || calls != 3 {
		t.Errorf("Write = %v after %d requests, want the 503 from the third and nothing after it", err, calls)
	}
}

// TestInfluxCannotBuildAWriteForAnOrgWithAControlCharacter reports the request
// it could not build rather than sending something else.
func TestInfluxCannotBuildAWriteForAnOrgWithAControlCharacter(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	if _, err := NewInflux(url, "tok", "ac\x7fme", "gh", 0, 0).Write(t.Context(), []Point{star("a")}); err == nil {
		t.Error("Write accepted an org no request can carry")
	}
	if n := len(s.sent()); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

// TestIsParseRejectionNeedsAnError answers no for a write that did not fail.
func TestIsParseRejectionNeedsAnError(t *testing.T) {
	t.Parallel()
	if isParseRejection(nil) {
		t.Error("a nil error was read as a parse rejection")
	}
	if isParseRejection(errors.New("503 Service Unavailable")) {
		t.Error("a server failure was read as a parse rejection")
	}
}

// TestInfluxCountsWhatItDidNotWrite: the sink is the only thing that knows
// what its own exclude dropped, and the sweep's log line counts on it to say
// what the database took. Without it production logged 440 points of
// gh_job_log written to a database that excludes the measurement by default
// and has never held a row of it.
func TestInfluxCountsWhatItDidNotWrite(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	i := NewInflux(url, "tok", "acme", "gh", 0, time.Second)
	i.Exclude = map[string]bool{"gh_job_log": true}
	offered := []Point{
		star("a"),
		{Measurement: "gh_job_log", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"line": "x"}},
		{Measurement: "gh_job_log", Tags: map[string]string{"repo": "b"}, Fields: map[string]any{"line": "y"}},
		// No field the line protocol can render, so this one is not written
		// either and is not counted either.
		{Measurement: "gh_star", Tags: map[string]string{"repo": "c"}},
	}
	accepted, err := i.Write(t.Context(), offered)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := len(s.written()); got != 1 {
		t.Fatalf("%d lines reached the database, want the one star", got)
	}
	if accepted != 1 {
		t.Errorf("Write accepted %d of 4 points and wrote 1", accepted)
	}
}

// TestInfluxDoesNotCountALineTheServerRefused: a parse rejection is reported
// on its own warning line and the rest of the batch is written, so the batch
// is a partial success. What it is not is four points written when three
// landed, which is what the count said while it was len(points).
func TestInfluxDoesNotCountALineTheServerRefused(t *testing.T) {
	t.Parallel()
	s, url := newInfluxServer(t, 0)
	i := NewInflux(url, "tok", "acme", "gh", 0, time.Second)
	bad := Point{
		Measurement: "gh_star", Tags: map[string]string{"repo": "unparseable"},
		Fields: map[string]any{"starred": 1}, Time: time.Unix(1700000000, 0),
	}
	accepted, err := i.Write(t.Context(), []Point{star("a"), bad, star("b")})
	var rejected *RejectedError
	if !errors.As(err, &rejected) || rejected.N != 1 {
		t.Fatalf("Write = %v, want one rejected line", err)
	}
	if accepted != 2 {
		t.Errorf("Write accepted %d of 3 points, one of which the server refused", accepted)
	}
	if got := len(s.written()); got != 2 {
		t.Errorf("%d lines reached the database, want the two good ones", got)
	}
}

// TestTheLedgerReportsWhatTheSinkBehindItTook: production wraps the InfluxDB
// sink in the write ledger, so the runner holds the pair. If the wrapper
// answered for itself, the exclude would be invisible again.
func TestTheLedgerReportsWhatTheSinkBehindItTook(t *testing.T) {
	t.Parallel()
	_, url := newInfluxServer(t, 0)
	i := NewInflux(url, "tok", "acme", "gh", 0, time.Second)
	i.Exclude = map[string]bool{"gh_job_log": true}
	wrapped := OnlyChanged(i, LoadLedger(filepath.Join(t.TempDir(), "ledger.bin"), 0, 0))
	accepted, err := wrapped.Write(t.Context(), []Point{
		star("a"),
		{Measurement: "gh_job_log", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"line": "x"}},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if accepted != 1 {
		t.Errorf("the ledger reported %d accepted of two points, one of them excluded", accepted)
	}
	// And a second write of the same points is entirely the ledger's doing:
	// nothing reaches the sink, so nothing is accepted here either, and the
	// caller counts those points as unchanged rather than as written.
	again, err := wrapped.Write(t.Context(), []Point{star("a")})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if again != 0 {
		t.Errorf("a point the ledger held back was counted as accepted: %d", again)
	}
}

// TestASinkThatWritesEverythingAcceptsEverything: the count is not a
// synonym for "dropped something", so a sink with nothing to drop says the
// whole batch.
func TestASinkThatWritesEverythingAcceptsEverything(t *testing.T) {
	t.Parallel()
	wrapped := OnlyChanged(newStdout(io.Discard), LoadLedger(filepath.Join(t.TempDir(), "ledger.bin"), 0, 0))
	accepted, err := wrapped.Write(t.Context(), []Point{star("a"), star("b")})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if accepted != 2 {
		t.Errorf("accepted %d of two points a sink drops nothing from", accepted)
	}
}
