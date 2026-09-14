package sink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if err := s(srv.URL).Write(context.Background(), points); err != nil {
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
	if err := o.Write(context.Background(), []Point{{
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
