// The live cache measurement lives here rather than behind a build tag, so
// `go vet` and the linters keep seeing it: a harness nothing compiles is a
// harness that has already rotted by the time anyone needs it again. It is
// skipped unless GHC_LIVE_CONFIG names a config file, which is the same
// bargain test/live makes for the sinks that need a real endpoint.
//
// It exists because the bound in client.go is a number about real traffic,
// and a number like that has to be re-measurable rather than remembered. One
// run spends real rate limit and takes the better part of an hour, so it is
// never part of an ordinary `go test ./...`.
//
// Run it with:
//
//	GHC_LIVE_CONFIG=/path/config.yaml GHC_LIVE_SWEEPS=3 GHC_LIVE_BUDGET=170m \
//	  go test -count=1 -run TestLiveSweepCacheFootprint -v -timeout 175m ./internal/ghapi/
//
// GHC_LIVE_BUDGET has to stay under -timeout, or the test panics on the
// deadline instead of reporting the sweeps it did finish.
package ghapi_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/run"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// discard counts the points a sweep produced and throws them away. The
// footprint being measured is the client's, so no store is involved.
type discard struct{ points int }

func (d *discard) Write(_ context.Context, p []sink.Point) (int, error) {
	d.points += len(p)
	return len(p), nil
}
func (d *discard) Name() string { return "discard" }
func (d *discard) Close() error { return nil }

// sweepSummary is what one sweep did to the cache.
type sweepSummary struct {
	entries int
	charged int
	body    int
}

func TestLiveSweepCacheFootprint(t *testing.T) {
	path := os.Getenv("GHC_LIVE_CONFIG")
	if path == "" {
		t.Skip("set GHC_LIVE_CONFIG to a config file to measure against the live API")
	}
	// The sinks are supplied here, so the config file does not have to name
	// one it will never write to.
	cfg, err := config.LoadWith(path, config.Relax{NoSinks: true})
	if err != nil {
		t.Fatal(err)
	}
	api := ghapi.New(cfg.GitHub.Token, 60*time.Second)
	api.SetReserve(cfg.GitHub.ReserveRate, false)
	// The bound is what is being measured, so it must not bite during the
	// measurement: anything evicted would be missing from the total.
	api.SetCacheLimit(4 << 30)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := &discard{}

	// A full sweep of a real account is sequential and network bound, so the
	// budget here is wall clock rather than anything about the API.
	ctx, cancel := context.WithTimeout(context.Background(), envSetting(t, "GHC_LIVE_BUDGET", 2*time.Hour, time.ParseDuration))
	defer cancel()

	sweeps := envSetting(t, "GHC_LIVE_SWEEPS", 3, strconv.Atoi)

	var prev sweepSummary
	for i := 1; i <= sweeps; i++ {
		// A fresh state and a fresh runner each time, so every family runs on
		// every sweep rather than waiting for its own cadence. That is the
		// worst case the bound has to survive, and it is also the case the
		// owner described: everything polled at one minute.
		state := run.LoadState(filepath.Join(t.TempDir(), "state.json"))
		r := &run.Runner{Cfg: cfg, API: api, Sinks: []sink.Sink{d}, State: state, Log: logger, Prime: true}
		start := time.Now()
		before, seen := api.RateFor("core")
		if err = r.Once(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		after, _ := api.RateFor("core")

		now := summarize(api)
		st := api.CacheStats()
		fmt.Printf("SWEEP %d: %s points=%d core=%s graphql=%+v | entries=%d (+%d) charged=%d (+%d) body=%d (+%d) statsBytes=%d evicted=%d\n",
			i, time.Since(start).Round(time.Second), d.points,
			coreSpend(before, seen, after, now.entries-prev.entries), api.GraphQLSpend(),
			now.entries, now.entries-prev.entries,
			now.charged, now.charged-prev.charged,
			now.body, now.body-prev.body,
			st.Bytes, st.Evicted)
		prev = now
		d.points = 0
	}

	report(t, api)
}

// envSetting reads one of the GHC_LIVE_ settings from the environment, and
// fails the test on a value it cannot parse rather than measuring with a
// default the caller did not ask for.
func envSetting[T any](t *testing.T, name string, fallback T, parse func(string) (T, error)) T {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	parsed, err := parse(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return parsed
}

// coreSpend says what the sweep cost in core requests, and refuses to answer
// with a number the counter cannot support.
//
// x-ratelimit-used is the whole token's spend inside the window running now,
// so subtracting two readings measures this sweep only when one window covered
// both and nothing else spent against the token meanwhile. A cold sweep takes
// ten minutes and readily crosses a reset: that is how the first sweep of the
// measured run reported 445 for work that stored 915 fresh bodies, a number
// that then has to be walked back wherever it was quoted. Where the
// subtraction cannot be trusted the fresh entries are reported instead. Each
// one was stored from a 200 and a 200 is a charged request, so the count is a
// floor that is actually true rather than a delta that looks exact.
func coreSpend(before ghapi.RateState, seen bool, after ghapi.RateState, fresh int) string {
	switch {
	case !seen:
		return fmt.Sprintf(">=%d (nothing read before the sweep, so the fresh entries are the floor)", fresh)
	case !after.Reset.Equal(before.Reset):
		return fmt.Sprintf(">=%d (window reset mid-sweep, so the counter delta of %d covers only the newer window)",
			fresh, after.Used-before.Used)
	default:
		return fmt.Sprintf("+%d", after.Used-before.Used)
	}
}

// summarize totals what the cache holds right now.
func summarize(api *ghapi.Client) sweepSummary {
	var s sweepSummary
	for _, e := range ghapi.CacheEntries(api) {
		s.entries++
		s.charged += e.Charged
		s.body += e.Body
	}
	return s
}

// report prints the size distribution, which is the part that decides between
// a byte bound and an entry bound.
func report(t *testing.T, api *ghapi.Client) {
	t.Helper()
	all := ghapi.CacheEntries(api)
	slices.SortFunc(all, func(a, b ghapi.CacheEntry) int { return b.Body - a.Body })

	// The full list is what the histogram below is a summary of, and it is
	// too long to read inline. It goes to stdout rather than to a file: a path
	// out of the environment is a path this test would then write wherever it
	// was pointed, and the caller is already redirecting the run somewhere.
	if os.Getenv("GHC_LIVE_DUMP") != "" {
		fmt.Println("DUMP body\tcharged\turl")
		for _, e := range all {
			fmt.Printf("DUMP %d\t%d\t%s\n", e.Body, e.Charged, e.URL)
		}
	}

	fmt.Println("TOP 25 BY BODY SIZE")
	for i := 0; i < len(all) && i < 25; i++ {
		fmt.Printf("  %9d  %s\n", all[i].Body, all[i].URL)
	}

	type bucket struct {
		label   string
		max     int
		entries int
		body    int
		charged int
	}
	buckets := []bucket{
		{label: "<1KB", max: 1 << 10},
		{label: "1-8KB", max: 8 << 10},
		{label: "8-64KB", max: 64 << 10},
		{label: "64-256KB", max: 256 << 10},
		{label: "256KB-1MB", max: 1 << 20},
		{label: ">=1MB", max: 1 << 62},
	}
	for _, e := range all {
		for i := range buckets {
			if e.Body < buckets[i].max {
				buckets[i].entries++
				buckets[i].body += e.Body
				buckets[i].charged += e.Charged
				break
			}
		}
	}
	fmt.Println("HISTOGRAM")
	for _, b := range buckets {
		fmt.Printf("  %-10s entries=%4d body=%d charged=%d\n", b.label, b.entries, b.body, b.charged)
	}

	// The median and the mean together are the argument for bounding by bytes:
	// a spread this wide means no entry count describes the memory.
	if len(all) > 0 {
		median := all[len(all)/2].Body
		var total int
		for _, e := range all {
			total += e.Body
		}
		fmt.Printf("SPREAD: entries=%d largest=%d median=%d mean=%d smallest=%d\n",
			len(all), all[0].Body, median, total/len(all), all[len(all)-1].Body)
	}
}
