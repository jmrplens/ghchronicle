package run

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestARefusalIsPaidOncePerDayPerFamily pins A4 at the sweep: the security
// family of a repository with Dependabot and code scanning switched off pays
// its two 403s on the first sweep and nothing on the next, the memory is per
// family so the analyses family still asks its own endpoint once, and a
// backfill asks everything again.
func TestARefusalIsPaidOncePerDayPerFamily(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := map[string]int{}
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked[req.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Dependabot alerts are disabled for this repository."}`))
	})
	r.Cfg.Every = everyOnly("security", "analyses")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	count := func(suffix string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for path, c := range asked {
			if strings.HasSuffix(path, suffix) {
				n += c
			}
		}
		return n
	}

	start := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	for _, now := range []time.Time{start, start.Add(time.Minute), start.Add(2 * time.Minute)} {
		if err := r.repoFamilies(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"/dependabot/alerts", "/code-scanning/alerts", "/code-scanning/analyses"} {
		if n := count(suffix); n != 1 {
			t.Errorf("%s asked %d times over three sweeps, want once", suffix, n)
		}
	}
	// A family that answers with a remembered refusal has still run: the
	// points say the features are off, and it is marked so it is not
	// repeated on the next tick.
	if _, marked := r.State.LastRun["security"]; !marked {
		t.Error("security was not marked as run")
	}

	r.Backfill = true
	if err := r.repoFamilies(context.Background(), start.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := count("/dependabot/alerts"); n != 2 {
		t.Errorf("a backfill must ask again, /dependabot/alerts asked %d times in total", n)
	}
}
