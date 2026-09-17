package sink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func captureOTLP(t *testing.T, s func(url string) *OTLP, points []Point) []map[string]any {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q, want application/json", got)
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		bodies = append(bodies, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if _, err := s(srv.URL).Write(context.Background(), points); err != nil {
		t.Fatalf("write: %v", err)
	}
	return bodies
}

func TestOTLPNamesFollowTheConvention(t *testing.T) {
	if got := otlpName("gh_workflow_run", "duration_seconds"); got != "github.workflow.run.duration.seconds" {
		t.Errorf("otlpName = %q", got)
	}
}

func TestOTLPSendsDatedPointsOnlyWhenAsked(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	points := []Point{
		{
			Measurement: "gh_traffic", Tags: map[string]string{"repo": "a", "kind": "views"},
			Fields: map[string]any{"count": 10}, Time: old,
		},
		{
			Measurement: "gh_traffic", Tags: map[string]string{"repo": "a", "kind": "views"},
			Fields: map[string]any{"count": 5}, Time: old.AddDate(0, 0, 1),
		},
	}

	// Reduced by default: the two days of the traffic window become one summed
	// value, because most backends reject a sample dated in January.
	bodies := captureOTLP(t, func(u string) *OTLP { return NewOTLP(u, "test", nil, false, 0, 0) }, points)
	if n := countDataPoints(bodies); n != 1 {
		t.Errorf("reduced send produced %d data points, want 1", n)
	}

	// Raw keeps both, for a backend that accepts them.
	bodies = captureOTLP(t, func(u string) *OTLP { return NewOTLP(u, "test", nil, true, 0, 0) }, points)
	if n := countDataPoints(bodies); n != 2 {
		t.Errorf("raw send produced %d data points, want 2", n)
	}
}

func TestOTLPSkipsStringFields(t *testing.T) {
	bodies := captureOTLP(t, func(u string) *OTLP { return NewOTLP(u, "test", nil, true, 0, 0) },
		[]Point{{
			Measurement: "gh_gist", Tags: map[string]string{"gist": "x"},
			Fields: map[string]any{"description": "a string", "files": 2}, Time: time.Now(),
		}})
	if n := countDataPoints(bodies); n != 1 {
		t.Errorf("got %d data points, want only the numeric one", n)
	}
}

func countDataPoints(bodies []map[string]any) int {
	n := 0
	for _, b := range bodies {
		rms, _ := b["resourceMetrics"].([]any)
		for _, rm := range rms {
			sms, _ := rm.(map[string]any)["scopeMetrics"].([]any)
			for _, sm := range sms {
				ms, _ := sm.(map[string]any)["metrics"].([]any)
				for _, m := range ms {
					g, _ := m.(map[string]any)["gauge"].(map[string]any)
					dps, _ := g["dataPoints"].([]any)
					n += len(dps)
				}
			}
		}
	}
	return n
}

func TestOTLPRepublishesTheCurrentState(t *testing.T) {
	var pushes int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		pushes++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o := NewOTLP(srv.URL, "test", nil, false, 0, 0)
	o.Repeat = 20 * time.Millisecond
	o.Start()
	defer o.Close()
	if _, err := o.Write(context.Background(), []Point{{
		Measurement: "gh_repo",
		Tags:        map[string]string{"repo": "a"}, Fields: map[string]any{"stars": 1}, Time: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	// One push from Write, then the repeat: a Prometheus fed by push only
	// sees a gauge for five minutes after it arrives, so the sink has to
	// keep saying it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := pushes
		mu.Unlock()
		if n >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d pushes, the repeat is not running", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// otlpCollector is a metrics endpoint that counts requests, keeps the service
// name each one reported, and answers every request with status.
type otlpCollector struct {
	mu       sync.Mutex
	requests int
	services []string
	status   int
}

func newOTLPCollector(t *testing.T, status int) (*otlpCollector, string) {
	t.Helper()
	c := &otlpCollector{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body otlpRequest
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		c.mu.Lock()
		c.requests++
		for _, rm := range body.ResourceMetrics {
			for _, a := range rm.Resource.Attributes {
				if a.Key == "service.name" && a.Value.StringValue != nil {
					c.services = append(c.services, *a.Value.StringValue)
				}
			}
		}
		c.mu.Unlock()
		w.WriteHeader(c.status)
		_, _ = io.WriteString(w, " refused ")
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func (c *otlpCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// twoMetrics is one raw point carrying two numeric fields, which is two
// metrics on the wire.
func twoMetrics() []Point {
	return []Point{{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"stars": 1, "forks": 2}, Time: time.Unix(1700000000, 0),
	}}
}

// TestNewOTLPFillsInOnlyWhatWasLeftOut gives a zero batch, timeout and service
// their documented values, keeps what was set, and reports the service name
// the backend files the metrics under.
func TestNewOTLPFillsInOnlyWhatWasLeftOut(t *testing.T) {
	o := NewOTLP("http://collector.test/v1/metrics", "", nil, false, 0, 0)
	if o.Batch != 2000 || o.client.Timeout != 60*time.Second || o.Service != "ghchronicle" || o.Name() != "otlp" {
		t.Errorf("defaults = batch %d, timeout %v, service %q, name %q", o.Batch, o.client.Timeout, o.Service, o.Name())
	}
	o = NewOTLP("http://collector.test/v1/metrics", "mine", nil, false, 3, time.Second)
	if o.Batch != 3 || o.client.Timeout != time.Second || o.Service != "mine" {
		t.Errorf("given = batch %d, timeout %v, service %q, want 3, 1s and mine kept", o.Batch, o.client.Timeout, o.Service)
	}

	c, url := newOTLPCollector(t, http.StatusOK)
	if _, err := NewOTLP(url, "", nil, true, 0, 0).Write(context.Background(), twoMetrics()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	services := c.services
	c.mu.Unlock()
	if len(services) != 1 || services[0] != "ghchronicle" {
		t.Errorf("service names = %q, want ghchronicle", services)
	}
}

// TestOTLPBatchesByDataPoint sends every metric in one request while the batch
// has room, one request per metric when each fills it, and stops at the first
// request that fails.
func TestOTLPBatchesByDataPoint(t *testing.T) {
	c, url := newOTLPCollector(t, http.StatusOK)
	if _, err := NewOTLP(url, "", nil, true, 0, 0).Write(context.Background(), twoMetrics()); err != nil {
		t.Fatal(err)
	}
	if n := c.count(); n != 1 {
		t.Errorf("%d requests for two metrics with room to spare, want one", n)
	}

	c, url = newOTLPCollector(t, http.StatusOK)
	if _, err := NewOTLP(url, "", nil, true, 1, 0).Write(context.Background(), twoMetrics()); err != nil {
		t.Fatal(err)
	}
	if n := c.count(); n != 2 {
		t.Errorf("%d requests for two metrics in batches of one, want two", n)
	}

	c, url = newOTLPCollector(t, http.StatusServiceUnavailable)
	_, err := NewOTLP(url, "", nil, true, 1, 0).Write(context.Background(), twoMetrics())
	if err == nil || err.Error() != "otlp write: 503 Service Unavailable: refused" || c.count() != 1 {
		t.Errorf("Write = %v after %d requests, want the 503 and nothing after it", err, c.count())
	}
}

// TestOTLPSendsNothingWithoutANumber makes no request for a batch whose fields
// are all text.
func TestOTLPSendsNothingWithoutANumber(t *testing.T) {
	c, url := newOTLPCollector(t, http.StatusOK)
	if _, err := NewOTLP(url, "", nil, true, 0, 0).Write(context.Background(), []Point{{
		Measurement: "gh_gist", Fields: map[string]any{"description": "text"}, Time: time.Unix(1, 0),
	}}); err != nil {
		t.Fatal(err)
	}
	if n := c.count(); n != 0 {
		t.Errorf("%d requests for a batch with no number, want none", n)
	}
}

// TestOTLPReportsAFailedWrite treats 300 as a failure like any other status
// that is not a success, and reports an unusable endpoint and a collector that
// is not there.
func TestOTLPReportsAFailedWrite(t *testing.T) {
	_, url := newOTLPCollector(t, http.StatusMultipleChoices)
	if _, err := NewOTLP(url, "", nil, true, 0, 0).Write(context.Background(), twoMetrics()); err == nil ||
		!strings.HasPrefix(err.Error(), "otlp write: 300") {
		t.Errorf("Write = %v, want the 300 reported", err)
	}
	if _, err := NewOTLP("collector:4318", "", nil, true, 0, 0).Write(context.Background(), twoMetrics()); err == nil ||
		!strings.HasPrefix(err.Error(), "otlp write: endpoint ") {
		t.Errorf("Write = %v, want the endpoint refused", err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := NewOTLP(closed.URL, "", nil, true, 0, 0).Write(context.Background(), twoMetrics()); err == nil {
		t.Error("Write reported success to a collector that is not there")
	}
}

// TestOTLPStartsARepeatOnlyWhenOneCanHelp starts no loop without an interval
// or for raw points, which are dated and so cannot be republished as now, and
// starts one loop however often Start is called. Close is safe whether or not
// a loop was ever started.
func TestOTLPStartsARepeatOnlyWhenOneCanHelp(t *testing.T) {
	o := NewOTLP("http://collector.test/v1/metrics", "", nil, false, 0, 0)
	o.Start()
	if o.stop != nil {
		t.Error("a loop started with no interval")
	}
	if err := o.Close(); err != nil {
		t.Errorf("Close with no loop = %v", err)
	}

	raw := NewOTLP("http://collector.test/v1/metrics", "", nil, true, 0, 0)
	raw.Repeat = time.Hour
	raw.Start()
	if raw.stop != nil {
		t.Error("a loop started for raw points")
	}

	o.Repeat = time.Hour
	o.Start()
	first := o.stop
	o.Start()
	if first == nil || o.stop != first {
		t.Error("Start did not start exactly one loop")
	}
	if err := o.Close(); err != nil || o.stop != nil {
		t.Errorf("Close = %v with the loop still recorded, want it stopped", err)
	}
}

// TestOTLPRepublishSendsTheStateItHasAndNothingWithout sends the known series
// again, and makes no request before anything has been written.
func TestOTLPRepublishSendsTheStateItHasAndNothingWithout(t *testing.T) {
	c, url := newOTLPCollector(t, http.StatusOK)
	o := NewOTLP(url, "", nil, false, 0, 0)
	o.republish()
	if n := c.count(); n != 0 {
		t.Errorf("%d requests republishing an empty state, want none", n)
	}
	if _, err := o.Write(context.Background(), []Point{account("octocat", 3)}); err != nil {
		t.Fatal(err)
	}
	o.republish()
	if n := c.count(); n != 2 {
		t.Errorf("%d requests after a write and a republish, want two", n)
	}
}
