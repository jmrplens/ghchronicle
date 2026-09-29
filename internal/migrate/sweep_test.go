package migrate

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// tagSink keeps the tag keys each measurement's points carried, every key a
// collector set, an empty value included: that is a node of a Graphite path
// all the same, and the golden file is what the planner counts those by.
type tagSink struct {
	mu   sync.Mutex
	tags map[string]map[string]bool
}

func (s *tagSink) Name() string { return "tags" }
func (s *tagSink) Close() error { return nil }

func (s *tagSink) Write(_ context.Context, points []sink.Point) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range points {
		keys := s.tags[p.Measurement]
		if keys == nil {
			keys = map[string]bool{}
			s.tags[p.Measurement] = keys
		}
		for k := range p.Tags {
			keys[k] = true
		}
	}
	return len(points), nil
}

// shapes is what the sink kept, as the golden file spells it: each
// measurement's tag keys, sorted.
func (s *tagSink) shapes() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.tags))
	for m, keys := range s.tags {
		out[m] = slices.Sorted(maps.Keys(keys))
	}
	return out
}

// sweepFake runs one sweep of the fake GitHub the end-to-end suites collect
// from, in this process, with the named families on and every other family
// off, and returns the tag keys of every measurement the sweep wrote.
//
// In process rather than through the binary: the per-family gate is one sweep
// per family, and a sweep here is a fraction of a second where a process is
// a build and a start.
func sweepFake(t *testing.T, families []string) map[string][]string {
	t.Helper()
	fake := fakegh.New(t, "../../test/e2e/testdata")
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(fake.URL())
	api.SetRetryPause(0)
	every := config.Every{Default: "0", Families: map[string]string{}}
	for _, f := range families {
		every.Families[f] = "1m"
	}
	cfg := &config.Config{
		GitHub:       config.GitHub{Token: "test-token"},
		Targets:      config.Targets{User: fakegh.Login},
		Every:        every,
		AllowNoSinks: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	kept := &tagSink{tags: map[string]map[string]bool{}}
	r := &run.Runner{
		Cfg: cfg, API: api, Sinks: []sink.Sink{kept},
		State: run.LoadState(filepath.Join(t.TempDir(), "state.json")),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Prime: true,
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("sweeping the fake with %v: %v", families, err)
	}
	return kept.shapes()
}
