package run

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// captured is a sink that counts the points it receives by measurement and
// answers each write with what fail returns.
type captured struct {
	name string
	fail func() error

	mu       sync.Mutex
	measures map[string]int
}

func (c *captured) Name() string { return c.name }
func (c *captured) Close() error { return nil }

func (c *captured) Write(_ context.Context, points []sink.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.measures == nil {
		c.measures = map[string]int{}
	}
	for _, p := range points {
		c.measures[p.Measurement]++
	}
	if c.fail != nil {
		return c.fail()
	}
	return nil
}

// measured is how many points of measurement the sink received.
func (c *captured) measured(measurement string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.measures[measurement]
}

// lockedBuffer is the log a fake runner writes to. A test that reads the log
// while the runner is still writing to it, which TestServeSweepsUntilCanceled
// does, races on a plain bytes.Buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeRunner is a runner over the fake GitHub the end-to-end suites collect
// from, with every family at its default cadence, its state kept in a
// temporary file, and its log kept for the test to read.
func fakeRunner(t *testing.T, sinks ...sink.Sink) (r *Runner, fake *fakegh.Server, log *lockedBuffer) {
	t.Helper()
	fake = fakegh.New(t, "../../test/e2e/testdata")
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(fake.URL())
	cfg := &config.Config{
		GitHub:       config.GitHub{Token: "test-token"},
		Targets:      config.Targets{User: fakegh.Login},
		AllowNoSinks: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log = &lockedBuffer{}
	r = &Runner{
		Cfg:   cfg,
		API:   api,
		Sinks: sinks,
		State: LoadState(filepath.Join(t.TempDir(), "state", "state.json")),
		Log:   slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Prime: true,
	}
	return r, fake, log
}

// TestAPrimedSweepRunsEveryFamily sweeps the fake account once from a cold
// start: the repositories are discovered, every enabled family runs and is
// marked, the points reach the sinks, and the state is saved for the next
// start.
func TestAPrimedSweepRunsEveryFamily(t *testing.T) {
	t.Parallel()
	got := &captured{name: "captured"}
	r, _, log := fakeRunner(t, got)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	for _, family := range config.Families() {
		_, enabled := r.Cfg.Interval(family)
		_, marked := r.State.LastRun[family]
		if enabled && !marked {
			t.Errorf("%s is enabled and was not marked as run", family)
		}
	}
	for _, measurement := range []string{"gh_repo", "gh_traffic", "gh_star", "gh_account", "gh_workflow_job"} {
		if got.measured(measurement) == 0 {
			t.Errorf("no %s point reached the sink", measurement)
		}
	}
	saved := LoadState(r.State.path)
	if len(saved.LastRun) != len(r.State.LastRun) || len(saved.FirstSaw) == 0 {
		t.Errorf("saved state = %+v, want what the sweep marked and the repositories it first saw", saved)
	}
	for _, line := range []string{"first sweep after start-up", "repositories discovered", "sweep finished"} {
		if !strings.Contains(log.String(), line) {
			t.Errorf("the log does not say %q:\n%s", line, log)
		}
	}
}

// TestASecondSweepOnlyRunsWhatIsDue reuses the repository list and skips every
// family whose cadence has not come round, which on an immediate second sweep
// is all of them.
func TestASecondSweepOnlyRunsWhatIsDue(t *testing.T) {
	t.Parallel()
	r, fake, log := fakeRunner(t)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	before := len(fake.Requests())
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
	if strayed := fake.Requests()[before:]; len(strayed) != 0 {
		t.Errorf("an immediate second sweep asked %d things, first %s; want nothing due", len(strayed), strayed[0].Path)
	}
	if strings.Count(log.String(), "repositories discovered") != 1 {
		t.Error("the repository list was rebuilt before it went stale")
	}
}

// TestABackfillWalksWithItsOwnCollectors runs the backfill variant of every
// family, which asks for more and waits rather than skipping.
//
// The fake answers every page of a GraphQL walk with the same first page, so
// an unbounded walk would never end. Bounding the backfill at the start of
// the test is what stops each walk after one page: everything the fixtures
// date is older than that.
func TestABackfillWalksWithItsOwnCollectors(t *testing.T) {
	t.Parallel()
	r, fake, log := fakeRunner(t)
	r.Backfill = true
	r.BackfillSince = time.Now()
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "running every family, whatever the state file says") {
		t.Errorf("the log does not say the backfill primes every family:\n%s", log)
	}
	sawDeliveries := false
	for _, req := range fake.Requests() {
		if strings.HasSuffix(req.Path, "/deliveries") {
			sawDeliveries = true
		}
	}
	if !sawDeliveries {
		t.Error("a backfill did not read the webhook deliveries")
	}
}

// TestADiscoveryThatFailsStopsTheSweep returns the error without marking
// anything, since there is nothing to sweep without the repository list.
func TestADiscoveryThatFailsStopsTheSweep(t *testing.T) {
	t.Parallel()
	r, _, _ := fakeRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Once(ctx); err == nil {
		t.Fatal("Once swept without a repository list")
	}
	if len(r.State.LastRun) != 0 {
		t.Errorf("marked %v, want nothing", r.State.LastRun)
	}
}

// TestEmitSaysWhatReachedEachSink logs a write that failed, one that was a
// partial success, and how many points a skipping sink left out.
func TestEmitSaysWhatReachedEachSink(t *testing.T) {
	t.Parallel()
	failing := &captured{name: "failing", fail: func() error { return errors.New("store is down") }}
	partial := &captured{name: "partial", fail: func() error { return &sink.RejectedError{N: 2} }}
	dropping := &captured{name: "dropping", fail: func() error { return &sink.DroppedError{N: 1, Older: time.Hour} }}
	ledger := sink.LoadLedger(filepath.Join(t.TempDir(), "ledger.json"), 0, 0)
	skipping := sink.OnlyChanged(&captured{name: "skipping"}, ledger)
	r, _, log := fakeRunner(t, failing, partial, dropping, skipping)
	points := []sink.Point{{
		Measurement: "gh_star", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"starred": 1}, Time: time.Unix(1700000000, 0),
	}}
	r.emit(t.Context(), "stars", points)
	r.emit(t.Context(), "stars", points)
	for _, line := range []string{
		`msg="sink write failed" sink=failing family=stars points=1 err="store is down"`,
		`msg="sink rejected some lines" sink=partial family=stars rejected=2`,
		`msg="sink dropped old entries" sink=dropping family=stars dropped=1 older_than=1h0m0s`,
		`msg=written sink=skipping family=stars points=1 unchanged=0`,
		`msg=written sink=skipping family=stars points=0 unchanged=1`,
	} {
		if !strings.Contains(log.String(), line) {
			t.Errorf("the log does not say\n%s\nin\n%s", line, log)
		}
	}
	r.emit(t.Context(), "stars", nil)
	r.finish()
	if !strings.Contains(log.String(), `msg="points already written and not sent again" sink=skipping skipped=1`) {
		t.Errorf("finish does not report what the skipping sink spared:\n%s", log)
	}
}

// TestASkippingSinkCountsOnlyWhatThisWriteSpared: the sink's counter runs
// for the life of the process, so what one write left out is how far the
// counter moved during that write. A write after others that already spared
// points must not count their points again, which would also take them off
// the figure of what reached the store.
func TestASkippingSinkCountsOnlyWhatThisWriteSpared(t *testing.T) {
	t.Parallel()
	ledger := sink.LoadLedger(filepath.Join(t.TempDir(), "ledger.json"), 0, 0)
	inner := &captured{name: "skipping"}
	log, buf := debugLog()
	r := &Runner{Sinks: []sink.Sink{sink.OnlyChanged(inner, ledger)}, Log: log}
	star := func(repo string) sink.Point {
		return sink.Point{
			Measurement: "gh_star", Tags: map[string]string{"repo": repo},
			Fields: map[string]any{"starred": 1}, Time: time.Unix(1700000000, 0),
		}
	}
	r.emit(t.Context(), "stars", []sink.Point{star("a"), star("b")})
	r.emit(t.Context(), "stars", []sink.Point{star("a"), star("b")})
	r.emit(t.Context(), "stars", []sink.Point{star("a"), star("b"), star("c")})
	for _, line := range []string{
		`msg=written sink=skipping family=stars points=2 unchanged=0`,
		`msg=written sink=skipping family=stars points=0 unchanged=2`,
		`msg=written sink=skipping family=stars points=1 unchanged=2`,
	} {
		if !strings.Contains(buf.String(), line) {
			t.Errorf("the log does not say\n%s\nin\n%s", line, buf)
		}
	}
	if got := inner.measured("gh_star"); got != 3 {
		t.Errorf("the store received %d points, want the two new ones and then c", got)
	}
}

// TestServeSweepsUntilCanceled runs the first sweep at once and another on each
// tick, and returns the cancellation when the context ends.
//
// The loop is given as long as it needs for those two sweeps rather than a
// fixed wall-clock budget. The first sweep walks the whole fake account, and
// on a loaded machine that is slower than any budget short enough to keep the
// test quick: a two second deadline canceled the first sweep mid-walk, and
// the test then failed reporting no sweep at all.
func TestServeSweepsUntilCanceled(t *testing.T) {
	t.Parallel()
	r, _, log := fakeRunner(t)
	r.Cfg.Heartbeat = "20ms"
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()
	giveUp := time.After(2 * time.Minute)
	for strings.Count(log.String(), "sweep finished") < 2 {
		select {
		case err := <-done:
			t.Fatalf("Serve returned %v before the first sweep and one on a tick finished:\n%s", err, log)
		case <-giveUp:
			t.Fatalf("%d sweeps finished, want the first and at least one on a tick:\n%s",
				strings.Count(log.String(), "sweep finished"), log)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Serve = %v, want the context's end", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
}

// excluding is a sink that writes only some of what it is offered and counts
// the rest, which is what the InfluxDB sink does with its `exclude` list.
type excluding struct {
	captured
	skip     string
	filtered uint64
}

func (e *excluding) Write(ctx context.Context, points []sink.Point) error {
	var kept []sink.Point
	for _, p := range points {
		if p.Measurement == e.skip {
			e.filtered++
			continue
		}
		kept = append(kept, p)
	}
	return e.captured.Write(ctx, kept)
}

func (e *excluding) Filtered() uint64 { return e.filtered }

// TestASinkThatDropsPointsIsNotCreditedWithWritingThem is the log line the
// review of 2026-09-17 was misled by: production read
// "sink=influxdb family=joblogs points=440 unchanged=0" for 440 points of
// gh_job_log, which that sink excludes by default and which the database has
// never held a row of. The count is what the store took.
func TestASinkThatDropsPointsIsNotCreditedWithWritingThem(t *testing.T) {
	t.Parallel()
	skipping := &excluding{skip: "gh_job_log"}
	skipping.name = "influxdb"
	ledger := sink.LoadLedger(filepath.Join(t.TempDir(), "ledger.bin"), 0, 0)
	r, _, log := fakeRunner(t, sink.OnlyChanged(skipping, ledger))
	logLine := func(m string) sink.Point {
		return sink.Point{
			Measurement: m, Tags: map[string]string{"repo": "a"},
			Fields: map[string]any{"lines": 1}, Time: time.Unix(1700000000, 0),
		}
	}
	r.emit(t.Context(), "joblogs", []sink.Point{logLine("gh_job_log"), logLine("gh_workflow_job")})
	if got := skipping.measured("gh_job_log"); got != 0 {
		t.Fatalf("the sink wrote %d excluded points, so this test proves nothing", got)
	}
	want := `msg=written sink=influxdb family=joblogs points=1 unchanged=0 filtered=1`
	if !strings.Contains(log.String(), want) {
		t.Errorf("the log does not say\n%s\nin\n%s", want, log)
	}
	// And the sweep's own total, once, at the end.
	r.finish()
	if !strings.Contains(log.String(), `msg="points the sink did not write" sink=influxdb filtered=1`) {
		t.Errorf("finish does not report what the sink dropped:\n%s", log)
	}
}

// TestASinkThatWritesEverythingSaysNothingAboutFiltering keeps the key out of
// every other line: a sweep writing five families to five stores would
// otherwise carry a zero on every one of them.
func TestASinkThatWritesEverythingSaysNothingAboutFiltering(t *testing.T) {
	t.Parallel()
	r, _, log := fakeRunner(t, &captured{name: "plain"})
	r.emit(t.Context(), "stars", []sink.Point{{
		Measurement: "gh_star", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"starred": 1}, Time: time.Unix(1700000000, 0),
	}})
	r.finish()
	if strings.Contains(log.String(), "filtered=") {
		t.Errorf("a sink that wrote everything reported filtering:\n%s", log)
	}
}
