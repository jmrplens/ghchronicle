package sink

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// exposition is the page the exporter serves right now.
func exposition(t *testing.T, p *Prom) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.handle(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the text exposition format", ct)
	}
	return rec.Body.String()
}

// account is one gh_account point, which the reduction keeps as the newest
// reading per user.
func account(user string, followers int) Point {
	return Point{
		Measurement: "gh_account",
		Tags:        map[string]string{"user": user, "dropped_by_the_reduction": "x"},
		Fields:      map[string]any{"followers": followers, "gists": 2},
		Time:        time.Now(),
	}
}

// TestPromServesTheCurrentValueOfEachSeries renders one TYPE line per metric
// and one sample per label set, sorted, with the reduction's labels only.
func TestPromServesTheCurrentValueOfEachSeries(t *testing.T) {
	t.Parallel()
	p := NewProm("127.0.0.1:0", "")
	if p.Name() != "prometheus" || p.Path != "/metrics" {
		t.Errorf("NewProm = %q at %q, want the prometheus sink at /metrics", p.Name(), p.Path)
	}
	if _, err := p.Write(t.Context(), []Point{account("octocat", 12), account("hubot", 3)}); err != nil {
		t.Fatal(err)
	}
	want := "# TYPE github_account_followers gauge\n" +
		"github_account_followers{user=\"hubot\"} 3\n" +
		"github_account_followers{user=\"octocat\"} 12\n" +
		"# TYPE github_account_gists gauge\n" +
		"github_account_gists{user=\"hubot\"} 2\n" +
		"github_account_gists{user=\"octocat\"} 2\n"
	if got := exposition(t, p); got != want {
		t.Errorf("exposition =\n%s\nwant\n%s", got, want)
	}
}

// TestPromEscapesALabelOnce writes a label value the way the exposition format
// asks: a backslash, a double quote and a line feed each escaped once. Quoting
// the already escaped value again with %q escaped every one of them twice, so a
// scraper read back a value with the escapes left in it.
func TestPromEscapesALabelOnce(t *testing.T) {
	t.Parallel()
	p := NewProm("127.0.0.1:0", "")
	if _, err := p.Write(t.Context(), []Point{account("say \"hi\"\\\nbye é", 1)}); err != nil {
		t.Fatal(err)
	}
	want := `github_account_followers{user="say \"hi\"\\\nbye é"} 1`
	if got := exposition(t, p); !strings.Contains(got, want+"\n") {
		t.Errorf("exposition =\n%s\nwant a line\n%s", got, want)
	}
}

// TestPromForgetsASeriesNobodyWrites expires a series that has not been
// rewritten within Stale, so a repository that left the sweep stops being
// reported.
func TestPromForgetsASeriesNobodyWrites(t *testing.T) {
	t.Parallel()
	p := NewProm("127.0.0.1:0", "")
	if _, err := p.Write(t.Context(), []Point{account("gone", 1)}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	for key, s := range p.samples {
		s.seen = s.seen.Add(-2 * p.Stale)
		p.samples[key] = s
	}
	p.mu.Unlock()
	if _, err := p.Write(t.Context(), []Point{account("here", 1)}); err != nil {
		t.Fatal(err)
	}
	got := exposition(t, p)
	if strings.Contains(got, `"gone"`) || !strings.Contains(got, `"here"`) {
		t.Errorf("exposition =\n%s\nwant the stale series gone and the fresh one kept", got)
	}
}

// TestPromServesOverHTTP starts the exporter on a real port, serves the page at
// its path and redirects everything else there, and stops.
func TestPromServesOverHTTP(t *testing.T) {
	t.Parallel()
	ln, err := listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	// The port is free again once this closes, and Start binds it at once.
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	p := NewProm(addr, "/scrape")
	if err = p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Without a header timeout a client that opens a connection and never
	// finishes its request holds a goroutine and a descriptor for ever.
	if p.srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", p.srv.ReadHeaderTimeout)
	}
	t.Cleanup(func() {
		if closeErr := p.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})
	if _, err = p.Write(t.Context(), []Point{account("octocat", 7)}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/anything", http.NoBody)
	if err != nil {
		t.Fatal(err)
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
	if resp.Request.URL.Path != "/scrape" || !strings.Contains(string(body), `github_account_followers{user="octocat"} 7`) {
		t.Errorf("GET /anything ended at %s with\n%s\nwant the redirect to /scrape and the page", resp.Request.URL.Path, body)
	}
}

// TestPromReportsABusyPortAtStart fails Start itself rather than a goroutine
// nobody reads, and a Prom that never started closes cleanly.
func TestPromReportsABusyPortAtStart(t *testing.T) {
	t.Parallel()
	ln, err := listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := NewProm(ln.Addr().String(), "")
	if err = p.Start(); err == nil {
		t.Error("Start bound a port that is already taken")
	}
	if err = NewProm("127.0.0.1:0", "").Close(); err != nil {
		t.Errorf("Close before Start = %v, want nil", err)
	}
}

// TestMetricNameIsWhatAPrometheusUserTypes turns the measurement and the
// field into one name of the characters Prometheus allows.
func TestMetricNameIsWhatAPrometheusUserTypes(t *testing.T) {
	t.Parallel()
	if got := metricName("gh_ci-run", "p95.ms"); got != "github_ci_run_p95_ms" {
		t.Errorf("metricName = %q, want github_ci_run_p95_ms", got)
	}
}

// TestNumericReadsEveryShapeAFieldHolds covers the types a reduced field can
// arrive as, a boolean as one or zero, and a string as nothing.
func TestNumericReadsEveryShapeAFieldHolds(t *testing.T) {
	t.Parallel()
	for v, want := range map[any]float64{3: 3, int64(4): 4, 2.5: 2.5, true: 1, false: 0} {
		if got, ok := numeric(v); !ok || got != want {
			t.Errorf("numeric(%#v) = %v, %v, want %v", v, got, ok, want)
		}
	}
	if _, ok := numeric("12"); ok {
		t.Error("numeric read a string as a number")
	}
}

// TestNewPromForgetsASeriesAfterADay keeps a series for the documented day, so
// a sweep that runs a few times a day never blinks a series out between runs.
func TestNewPromForgetsASeriesAfterADay(t *testing.T) {
	t.Parallel()
	if p := NewProm("127.0.0.1:0", "/scrape"); p.Stale != 24*time.Hour || p.Path != "/scrape" {
		t.Errorf("NewProm = stale %v at %q, want a day at /scrape", p.Stale, p.Path)
	}
}

// TestPromNeverExpiresTheSeriesItHasJustWritten keeps what this very write
// produced even with no staleness allowance at all: a series is stale when it
// is older than Stale, and one stamped now is not older than anything.
func TestPromNeverExpiresTheSeriesItHasJustWritten(t *testing.T) {
	t.Parallel()
	p := NewProm("127.0.0.1:0", "")
	p.Stale = 0
	if _, err := p.Write(t.Context(), []Point{account("octocat", 1)}); err != nil {
		t.Fatal(err)
	}
	if got := exposition(t, p); !strings.Contains(got, `github_account_followers{user="octocat"} 1`) {
		t.Errorf("exposition =\n%s\nwant the series just written", got)
	}
}

// TestPromWritesASeriesWithoutLabelsBare renders a sample with no label as the
// bare name rather than with an empty pair of braces, the form the exposition
// format writes it in, and several labels in name order with a comma between
// each. Series of one name are ordered by their labels, so two scrapes of the
// same state are the same page.
func TestPromWritesASeriesWithoutLabelsBare(t *testing.T) {
	t.Parallel()
	p := NewProm("127.0.0.1:0", "")
	p.samples["a"] = sample{name: "github_up", value: 1}
	p.samples["b"] = sample{name: "github_repo_stars", labels: map[string]string{"repo": "a", "owner": "o", "language": "Go"}, value: 3}
	p.samples["c"] = sample{name: "github_repo_stars", labels: map[string]string{"repo": "b", "owner": "o", "language": "Go"}, value: 4}
	p.samples["d"] = sample{name: "github_repo_stars", labels: map[string]string{"repo": "0", "owner": "o", "language": "Go"}, value: 5}
	want := "# TYPE github_repo_stars gauge\n" +
		`github_repo_stars{language="Go",owner="o",repo="0"} 5` + "\n" +
		`github_repo_stars{language="Go",owner="o",repo="a"} 3` + "\n" +
		`github_repo_stars{language="Go",owner="o",repo="b"} 4` + "\n" +
		"# TYPE github_up gauge\n" +
		"github_up 1\n"
	if got := exposition(t, p); got != want {
		t.Errorf("exposition =\n%s\nwant\n%s", got, want)
	}
}

// TestSanitizeKeepsExactlyTheCharactersANameAllows checks every edge of the
// allowed ranges and the character just outside each one.
func TestSanitizeKeepsExactlyTheCharactersANameAllows(t *testing.T) {
	t.Parallel()
	if got := sanitize("azAZ09_"); got != "azAZ09_" {
		t.Errorf("sanitize = %q, want every allowed character kept", got)
	}
	if got := sanitize("`{@[/:.-é"); got != "_________" {
		t.Errorf("sanitize = %q, want every other character replaced", got)
	}
}

// TestPromServesNoSeriesForAText leaves out a field that is not a number, since
// an exposition page has no way to carry one, and keeps the numbers of the
// same point.
func TestPromServesNoSeriesForAText(t *testing.T) {
	t.Parallel()
	p := NewProm("127.0.0.1:0", "")
	pt := account("octocat", 5)
	pt.Fields["company"] = "acme"
	if _, err := p.Write(t.Context(), []Point{pt}); err != nil {
		t.Fatal(err)
	}
	got := exposition(t, p)
	if strings.Contains(got, "company") || !strings.Contains(got, `github_account_followers{user="octocat"} 5`) {
		t.Errorf("exposition =\n%s\nwant the followers and no series for the company", got)
	}
}

// TestPromCloseWaitsForAScraperStillSendingItsRequest gives a connection that
// has not finished its request the grace period Close exists to give, rather
// than reporting a failed shutdown the moment one connection is not idle.
//
// The order is what makes it deterministic. Shutdown closes the idle
// connection on its first pass, and only then does the test let the slow one
// go, so a Close that succeeds is one that kept waiting after that first pass.
func TestPromCloseWaitsForAScraperStillSendingItsRequest(t *testing.T) {
	t.Parallel()
	ln, err := listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	p := NewProm(addr, "")
	if err = p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var d net.Dialer
	slow, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slow.Close() })
	// The kernel hands connections to Accept in the order they were made, so
	// by the time this one has its answer the server has taken the slow one
	// and counts it as a connection still opening.
	idle, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idle.Close() })
	if _, err = io.WriteString(idle, "GET /metrics HTTP/1.1\r\nHost: prom\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(idle)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape = %d, %v, want 200 read to the end", resp.StatusCode, err)
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		// This read ends when Shutdown closes the idle connection, which
		// is the sign it has made its first pass and found the slow one.
		_, _ = io.Copy(io.Discard, br)
		_ = slow.Close()
	}()
	err = p.Close()
	// Unblocks the reader if Close returned without closing anything.
	_ = idle.Close()
	<-gone
	if err != nil {
		t.Errorf("Close = %v, want it to wait for the unfinished request to go away", err)
	}
}
