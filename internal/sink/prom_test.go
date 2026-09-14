package sink

import (
	"io"
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
	if err := p.Write(t.Context(), []Point{account("octocat", 12), account("hubot", 3)}); err != nil {
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
	if err := p.Write(t.Context(), []Point{account("say \"hi\"\\\nbye é", 1)}); err != nil {
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
	if err := p.Write(t.Context(), []Point{account("gone", 1)}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	for key, s := range p.samples {
		s.seen = s.seen.Add(-2 * p.Stale)
		p.samples[key] = s
	}
	p.mu.Unlock()
	if err := p.Write(t.Context(), []Point{account("here", 1)}); err != nil {
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
	t.Cleanup(func() {
		if closeErr := p.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})
	if err = p.Write(t.Context(), []Point{account("octocat", 7)}); err != nil {
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
