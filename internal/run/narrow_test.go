package run

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// named is a sink of a given name that keeps the measurements of every point
// it is handed, and the families they came in with.
type named struct {
	name string
	mu   sync.Mutex
	got  map[string]int
}

func (n *named) Name() string { return n.name }
func (n *named) Close() error { return nil }

func (n *named) Write(_ context.Context, points []sink.Point) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.got == nil {
		n.got = map[string]int{}
	}
	for _, p := range points {
		n.got[p.Measurement]++
	}
	return len(points), nil
}

func (n *named) measurements() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.got))
	for m := range n.got {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// TestOnlyWalksTheFamiliesNamedAndKeepHandsEachSinkItsOwn is the refill of a
// migration in miniature: a backfill of the two families that write the
// comments, handing one sink the comments alone and another sink, cleared of
// something else, only that, while a sink Keep does not name gets nothing.
// Without Only every family of the two groups runs; without Keep every sink
// gets everything.
func TestOnlyWalksTheFamiliesNamedAndKeepHandsEachSinkItsOwn(t *testing.T) {
	t.Parallel()
	fake := newFake(t)
	cleared, other, untouched := &named{name: "influxdb"}, &named{name: "postgres"}, &named{name: "loki"}
	r := walkRunner(t, t.TempDir(), fake, cleared, other, untouched)
	// Every family, so that Only is what narrows the walk.
	r.Cfg.Groups = nil
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Progress = nil
	r.Only = map[string]bool{"outbound": true, "discussions": true}
	r.Keep = map[string]map[string]bool{
		"influxdb": {"gh_discussion_comment": true},
		"postgres": {"gh_discussion": true},
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := cleared.measurements(); !slices.Equal(got, []string{"gh_discussion_comment"}) {
		t.Errorf("the sink cleared of the comments was handed %v", got)
	}
	if got := other.measurements(); !slices.Equal(got, []string{"gh_discussion"}) {
		t.Errorf("the sink cleared of the threads was handed %v", got)
	}
	if got := untouched.measurements(); len(got) != 0 {
		t.Errorf("a sink Keep does not name was handed %v", got)
	}
	var ran []string
	for family := range r.health.runs {
		ran = append(ran, family)
	}
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"discussions", "outbound"}) {
		t.Errorf("the walk ran %v, want discussions and outbound alone", ran)
	}
}

// TestANarrowedWalkIsAWalkOfItsOwn: the checkpoint of a walk of some
// families, writing some measurements to some sinks, is refused by a walk of
// anything else and says what differs, and the scope keeps only the sinks it
// writes to, so a store added elsewhere does not refuse it.
func TestANarrowedWalkIsAWalkOfItsOwn(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		GitHub: config.GitHub{Token: "x"}, Targets: config.Targets{User: "octocat"},
		Sinks: config.Sinks{
			Influx: &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"},
			Loki:   &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	keep := map[string][]string{"influxdb": {"gh_discussion_comment"}}
	refill := ScopeOf(cfg, "2023-11-14").Narrowed([]string{"outbound", "discussions", "never-a-family"}, keep)
	if !slices.Equal(refill.Families, []string{"discussions", "outbound"}) {
		t.Errorf("the families are %v: a name no configuration runs is not part of the walk", refill.Families)
	}
	if _, kept := refill.Sinks["loki"]; kept || refill.Sinks["influxdb"] == "" {
		t.Errorf("the sinks are %v, want the one written to", refill.Sinks)
	}
	for name, tc := range map[string]struct {
		now  Scope
		says string
	}{
		"a whole backfill": {
			now:  ScopeOf(cfg, "2023-11-14"),
			says: "is collected now and was not when the walk began",
		},
		"the same families to everything": {
			now:  ScopeOf(cfg, "2023-11-14").Narrowed([]string{"discussions", "outbound"}, nil),
			says: "the walk wrote gh_discussion_comment to influxdb when it began and writes every measurement to every sink now",
		},
		"another measurement": {
			now: ScopeOf(cfg, "2023-11-14").Narrowed([]string{"discussions", "outbound"},
				map[string][]string{"influxdb": {"gh_discussion_comment", "gh_discussion"}}),
			says: "writes gh_discussion,gh_discussion_comment to influxdb now",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeCheckpoint(t, &Progress{Scope: refill, Started: walkClock})
			_, err := OpenProgress(path, "test-build", tc.now, walkClock)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal is %v, want it to say %q", err, tc.says)
			}
		})
	}
	path := writeCheckpoint(t, &Progress{Scope: refill, Started: walkClock})
	cfg.Sinks.OTLP = &config.OTLPSink{Endpoint: "http://otel:4318"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	same := ScopeOf(cfg, "2023-11-14").Narrowed([]string{"discussions", "outbound"}, keep)
	if _, err := OpenProgress(path, "test-build", same, walkClock); err != nil {
		t.Errorf("a sink the walk does not write to refused it: %v", err)
	}
}
