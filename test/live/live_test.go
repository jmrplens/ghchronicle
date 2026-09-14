// Package live pushes a handful of points at real endpoints named in the
// environment.
//
// It sits here rather than in internal/sink because every other test in that
// package is hermetic: it stands up an httptest server, asserts on the bytes
// the sink wrote, and needs nothing outside the process. This one needs a
// Loki or an OTLP collector that someone actually runs, so it belongs with
// the other tests that reach outside, next to test/e2e.
package live

import (
	"os"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

// TestLiveSinks is skipped unless GHC_LIVE_OTLP or GHC_LIVE_LOKI names an
// endpoint, so the normal test run stays offline.
func TestLiveSinks(t *testing.T) {
	now := time.Now()
	points := []sink.Point{
		{
			Measurement: "gh_repo", Tags: map[string]string{"owner": "smoke", "repo": "smoke", "full_name": "smoke/smoke"},
			Fields: map[string]any{"stars": 7, "forks": 1}, Time: now,
		},
		{
			Measurement: "gh_star", Tags: map[string]string{"owner": "smoke", "repo": "smoke", "full_name": "smoke/smoke", "user": "someone"},
			Fields: map[string]any{"starred": 1}, Time: now.Add(-time.Minute),
		},
		{
			Measurement: "gh_workflow_run", Tags: map[string]string{"owner": "smoke", "repo": "smoke", "full_name": "smoke/smoke", "workflow": "CI", "conclusion": "failure"},
			Fields: map[string]any{"duration_seconds": 42, "success": false}, Time: now.Add(-2 * time.Minute),
		},
	}
	ctx := t.Context()

	if ep := os.Getenv("GHC_LIVE_OTLP"); ep != "" {
		if err := sink.NewOTLP(ep, "ghchronicle-smoke", nil, false, 0, 0).Write(ctx, points); err != nil {
			t.Errorf("otlp: %v", err)
		}
	}
	if url := os.Getenv("GHC_LIVE_LOKI"); url != "" {
		if err := sink.NewLoki(url, "", map[string]string{"source": "smoke"}, 0, 0, 0).Write(ctx, points); err != nil {
			t.Errorf("loki: %v", err)
		}
	}
}
