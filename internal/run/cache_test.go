package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// restartedRunner is the process after r: a new client against the same
// fake, the state file r saved, and r's cache file, with its first sweep
// primed so that every family runs again, as a service with an exporter
// does after a restart.
func restartedRunner(t *testing.T, r *Runner, fake *fakegh.Server, statePath string) (*Runner, *lockedBuffer) {
	t.Helper()
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(fake.URL())
	api.SetRetryPause(0)
	log := &lockedBuffer{}
	return &Runner{
		Cfg: r.Cfg, API: api, Sinks: r.Sinks,
		State:     LoadState(statePath),
		Log:       slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Prime:     true,
		CacheFile: r.CacheFile,
	}, log
}

// cacheRunner is fakeRunner with its state and its cache file in dir.
func cacheRunner(t *testing.T, dir string) (*Runner, *fakegh.Server, *lockedBuffer, string) {
	t.Helper()
	r, fake, log := fakeRunner(t)
	statePath := filepath.Join(dir, "state.json")
	r.State = LoadState(statePath)
	r.CacheFile = filepath.Join(dir, "state-cache.bin")
	return r, fake, log, statePath
}

// TestAProcessStartsFromWhatThePreviousOneLearned is issue #88 at the
// runner: two processes, one after the other, sharing the state file and the
// cache file beside it. The second asks every URL the first was answered 200
// for with the validator it stored, and is answered 304; it lists no jobs of
// a run the first wrote; and it sizes the pull request page from the first
// one's totals before it has run totals itself. Measured on the production
// service before this, the first 38 minutes after a restart cost 1,092
// charged core requests where the same passes cost about 66 warm.
func TestAProcessStartsFromWhatThePreviousOneLearned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, fake, log, statePath := cacheRunner(t, dir)
	if err := first.Once(t.Context()); err != nil {
		t.Fatalf("first Once: %v\n%s", err, log)
	}
	// The fake's one repository with pull requests has two hundred of
	// them, a page of fifty sized or not, so its count is made that of a
	// repository with three, which is a page of five only if the count
	// crossed the restart.
	if _, counted := first.counts["octocat/hello-world"]; !counted {
		t.Fatalf("the first process's totals counted %v", first.counts)
	}
	first.counts["octocat/hello-world"] = collect.ItemCounts{Pulls: 3}
	if err := first.SaveCache(); err != nil {
		t.Fatal(err)
	}
	firstRequests := fake.Requests()
	if _, listed := runListPages(t, firstRequests); listed == 0 {
		t.Fatal("the first process listed no jobs, so the second has nothing to be spared")
	}

	second, log2 := restartedRunner(t, first, fake, statePath)
	second.loadCache(time.Now())
	// The day's whole page is the read the counts size, and the one the
	// production service paid 328 points for instead of 139.
	delete(second.State.LastFull, "issues")
	if got := second.pulls(collect.Repo{FullName: "octocat/hello-world"}, time.Now()).First; got != 5 {
		t.Errorf("the second process asks for a page of %d, want the 5 the first one's count sizes", got)
	}
	if err := second.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v\n%s", err, log2)
	}
	again := fake.Requests()[len(firstRequests):]
	if _, listed := runListPages(t, again); listed != 0 {
		t.Errorf("the second process listed jobs %d times for runs the first had written", listed)
	}
	answered := map[string]int{}
	for _, req := range firstRequests {
		answered[req.Method+" "+req.Path+"?"+req.Query] = req.Status
	}
	repeats := 0
	for _, req := range again {
		url := req.Method + " " + req.Path + "?" + req.Query
		if req.Method != http.MethodGet || answered[url] != http.StatusOK || fakegh.OffAPI(req.Path) {
			continue
		}
		repeats++
		if req.Status != http.StatusNotModified {
			t.Errorf("%s was answered 200 to the first process and %d to the second: the cache file did not carry its validator", url, req.Status)
		}
	}
	if repeats == 0 {
		t.Fatal("the second process repeated no URL of the first, so this proves nothing")
	}
	if !strings.Contains(log2.String(), "cache file read") {
		t.Errorf("the second process did not say it read the cache file:\n%s", log2)
	}
}

// TestARefusalOutlivesTheProcessThatHeardIt: a feature that is off is paid
// for once a day, and a restart inside the day is no longer a reason to pay
// for it again.
func TestARefusalOutlivesTheProcessThatHeardIt(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := map[string]int{}
	refuse := func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked[req.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Dependabot alerts are disabled for this repository."}`))
	}
	cacheFile := filepath.Join(t.TempDir(), "state-cache.bin")
	process := func() *Runner {
		r := sweepRunner(t, refuse)
		r.Cfg.Every = everyOnly("security")
		if err := r.Cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		r.CacheFile = cacheFile
		r.loadCache(time.Now())
		return r
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return asked["/repos/o/n/dependabot/alerts"]
	}

	first := process()
	if err := first.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveCache(); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("the first process asked %d times, want once", count())
	}
	second := process()
	if err := second.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Errorf("the endpoint was asked %d times across the two processes, want once: the refusal stands for its day", count())
	}
}

// TestTheJobsOfAPassNoSinkTookAreListedAgain: the collector remembers a run
// the moment its jobs are in hand, and a sink that then refuses them used to
// leave the run remembered and its jobs in no store, which a memory that
// crosses a restart would keep that way for good. The pass's runs are
// forgotten instead, the next pass lists and writes them, and only then are
// they what the cache file keeps.
func TestTheJobsOfAPassNoSinkTookAreListedAgain(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	listed := 0
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/actions/runs"):
			_, _ = fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":11,"run_attempt":1,"status":"completed",`+
				`"conclusion":"success","path":".github/workflows/ci.yml","updated_at":%q,"created_at":%q}]}`,
				time.Now().Add(-time.Hour).Format(time.RFC3339), time.Now().Add(-2*time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(req.URL.Path, "/runs/11/jobs"):
			mu.Lock()
			listed++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"total_count":0,"jobs":[]}`))
		default:
			http.NotFound(w, req)
		}
	})
	r.Cfg.Every = everyOnly("actions")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.CacheFile = filepath.Join(t.TempDir(), "state-cache.bin")
	down := errors.New("the store is down")
	store := &captured{name: "store", fail: func() error { return down }}
	r.Sinks = []sink.Sink{store}
	pass := func(at time.Time) int {
		t.Helper()
		if err := r.repoFamilies(context.Background(), at); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return listed
	}

	start := time.Now()
	if n := pass(start); n != 1 {
		t.Fatalf("the first pass listed the jobs %d times, want once", n)
	}
	if err := r.SaveCache(); err != nil {
		t.Fatal(err)
	}
	if head, _, err := readCache(r.CacheFile); err != nil || len(head.Expanded) != 0 {
		t.Errorf("the cache file keeps %v (%v) after a pass no store took, want no run", head, err)
	}

	store.fail = nil
	if n := pass(start.Add(time.Hour)); n != 2 {
		t.Errorf("after a pass the store refused, the jobs were listed %d times in all, want again", n)
	}
	if n := pass(start.Add(2 * time.Hour)); n != 2 {
		t.Errorf("after a pass the store took, the jobs were listed %d times in all, want no more", n)
	}
	if err := r.SaveCache(); err != nil {
		t.Fatal(err)
	}
	head, _, err := readCache(r.CacheFile)
	if err != nil || len(head.Expanded) != 1 || head.Expanded[0].ID != 11 {
		t.Errorf("the cache file keeps %+v (%v), want the run the store took", head, err)
	}
}

// TestTotalsRunFirstWhenNothingSizesThePullRequestPage is the promise the
// comment on counts made and only a primed sweep kept: a first sweep with no
// counts runs totals before the pull requests even when totals is not due,
// and a first sweep that read counts from the cache file does not.
func TestTotalsRunFirstWhenNothingSizesThePullRequestPage(t *testing.T) {
	t.Parallel()
	notDue := func(r *Runner) {
		r.Prime = false
		now := time.Now()
		for _, family := range config.Families() {
			r.State.Mark(family, now)
		}
		delete(r.State.LastRun, "issues")
	}

	r, _, log := fakeRunner(t)
	notDue(r)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if len(r.counts) == 0 {
		t.Errorf("a first sweep with no page sizes ran the pull requests without running totals:\n%s", log)
	}

	remembered, _, log := fakeRunner(t)
	notDue(remembered)
	remembered.CacheFile = filepath.Join(t.TempDir(), "state-cache.bin")
	sized := map[string]collect.ItemCounts{"o/n": {Pulls: 3}}
	if err := writeCache(remembered.CacheFile, &cacheHeader{Written: time.Now(), Counts: sized}, nil); err != nil {
		t.Fatal(err)
	}
	if err := remembered.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if strings.Contains(log.String(), "running totals before") || len(remembered.counts) != 1 {
		t.Errorf("a first sweep with page sizes read from the cache file ran totals anyway: counts %v\n%s", remembered.counts, log)
	}
}

// TestACacheFileThatDoesNotLoadIsNotReadAtAll: a file cut short, damaged,
// with something after it, or of another version starts the process cold,
// with a line in the log and no error, and nothing of it half believed.
func TestACacheFileThatDoesNotLoadIsNotReadAtAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "good.bin")
	head := &cacheHeader{
		Written:  time.Now(),
		Counts:   map[string]collect.ItemCounts{"o/n": {Pulls: 1, Issues: 2}},
		Expanded: []expandedRun{{ID: 7, Attempt: 1, Listed: time.Now().Unix()}},
	}
	answers := []ghapi.Answer{{URL: "https://api.github.com/x", ETag: `"e"`, Body: []byte(`{"a":1}`), Used: time.Now().Unix()}}
	if err := writeCache(good, head, answers); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, read, readErr := readCache(good); readErr != nil || len(read) != 1 || !bytes.Equal(read[0].Body, answers[0].Body) {
		t.Fatalf("the file as written reads back as %+v, %v", read, readErr)
	}
	damaged := bytes.Clone(whole)
	damaged[len(damaged)-12] ^= 0xff
	for name, body := range map[string][]byte{
		"cut short":       whole[:len(whole)-9],
		"damaged":         damaged,
		"another version": append([]byte("GHCACHE2\n"), whole[len(cacheMagic):]...),
		"not one at all":  []byte("{}"),
		"empty":           nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state-cache.bin")
			if writeErr := os.WriteFile(path, body, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			r := pullsRunner(t)
			log := &lockedBuffer{}
			r.Log = slog.New(slog.NewTextHandler(log, nil))
			r.CacheFile = path
			r.loadCache(time.Now())
			if len(r.counts) != 0 || len(r.expanded) != 0 || len(r.API.Answers()) != 0 {
				t.Errorf("read counts %v, runs %v and %d answers out of a file that does not load",
					r.counts, r.expanded, len(r.API.Answers()))
			}
			if !strings.Contains(log.String(), "cache file not read") {
				t.Errorf("a file that does not load was set aside without a word:\n%s", log)
			}
		})
	}
}

// TestTheCacheFileKeepsWhatWasAskedForLately holds the bounds: an answer
// nothing has asked for within the horizon is left out, so is one larger
// than an answer may be, and the rest stop, least recently used first, at the
// total. A run no sweep has listed within the horizon is left out too.
func TestTheCacheFileKeepsWhatWasAskedForLately(t *testing.T) {
	t.Parallel()
	body := func(n int) []byte { return bytes.Repeat([]byte("x"), n) }
	all := []ghapi.Answer{
		{URL: "/newest", Body: body(40), Used: 100},
		{URL: "/huge", Body: body(200), Used: 99},
		{URL: "/middle", Body: body(40), Used: 98},
		{URL: "/over-the-total", Body: body(40), Used: 97},
		{URL: "/stale", Body: body(1), Used: 10},
	}
	var urls []string
	for _, a := range keptAnswers(all, 50, 100, 100) {
		urls = append(urls, a.URL)
	}
	if got, want := strings.Join(urls, " "), "/newest /middle"; got != want {
		t.Errorf("kept %q, want %q", got, want)
	}

	now := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	runs := listedSince(map[collect.RunKey]time.Time{
		{ID: 2, Attempt: 1}: now,
		{ID: 1, Attempt: 2}: now.Add(-time.Hour),
		{ID: 1, Attempt: 1}: now.Add(-72 * time.Hour),
	}, now.Add(-48*time.Hour))
	if len(runs) != 2 || runs[0] != (expandedRun{ID: 1, Attempt: 2, Listed: now.Add(-time.Hour).Unix()}) || runs[1].ID != 2 {
		t.Errorf("kept the runs %+v, want the two listed in the last 48 hours in order", runs)
	}
}

// TestTheHorizonIsTwiceTheLongestCadence: the longest cadence is the one
// every entry has to outlive, twice it leaves room for a pass that was
// skipped, and a configuration of short cadences still keeps a day.
func TestTheHorizonIsTwiceTheLongestCadence(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	if got := r.cacheHorizon(); got != 48*time.Hour {
		t.Errorf("at the default cadences the horizon is %s, want 48h: the daily families", got)
	}
	r.Cfg.Every = config.Every{Families: map[string]string{"history": "168h"}}
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := r.cacheHorizon(); got != 336*time.Hour {
		t.Errorf("with a weekly family the horizon is %s, want two weeks", got)
	}
	r.Cfg.Every = everyOnly("actions")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := r.cacheHorizon(); got != 24*time.Hour {
		t.Errorf("with nothing slower than a minute the horizon is %s, want a day", got)
	}
}

// TestACardOnlyRunWritesNoCacheFile: the runs a card-only sweep expanded are
// in a picture and in no store, so it writes the cache file no more than it
// writes the state file. It reads it all the same.
func TestACardOnlyRunWritesNoCacheFile(t *testing.T) {
	t.Parallel()
	r, _, log, _ := cacheRunner(t, t.TempDir())
	r.Card, r.CardOnly = true, true
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if err := r.SaveCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.CacheFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a card-only run left a cache file behind: %v", err)
	}
}

// TestABackfillReadsTheCacheFileAndLeavesItAsItWas: a walk asks for every
// page of every list, and those pages, all more recently used than anything
// a sweep stored, would crowd the sweeps' own answers out of the file. So a
// backfill starts from the file, and is answered 304 where it asks what a
// sweep already stored, and writes nothing back to it.
func TestABackfillReadsTheCacheFileAndLeavesItAsItWas(t *testing.T) {
	t.Parallel()
	sweep, fake, log, statePath := cacheRunner(t, t.TempDir())
	if err := sweep.Once(t.Context()); err != nil {
		t.Fatalf("the sweep: %v\n%s", err, log)
	}
	if err := sweep.SaveCache(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(sweep.CacheFile)
	if err != nil {
		t.Fatal(err)
	}
	swept := len(fake.Requests())

	walk, log2 := restartedRunner(t, sweep, fake, statePath)
	walk.Backfill, walk.BackfillSince = true, time.Now()
	if err = walk.Once(t.Context()); err != nil {
		t.Fatalf("the backfill: %v\n%s", err, log2)
	}
	if err = walk.SaveCache(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(sweep.CacheFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the backfill wrote the cache file")
	}
	revalidated := 0
	for _, req := range fake.Requests()[swept:] {
		if req.Status == http.StatusNotModified {
			revalidated++
		}
	}
	if revalidated == 0 || !strings.Contains(log2.String(), "cache file read") {
		t.Errorf("the backfill did not start from the cache file: %d answers 304\n%s", revalidated, log2)
	}
}

// TestACacheFileThatCannotBeWrittenIsReportedOncePerInterval: a disk that
// refuses one save refuses the next, and the family after it must not say so
// again.
func TestACacheFileThatCannotBeWrittenIsReportedOncePerInterval(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	log := &lockedBuffer{}
	r.Log = slog.New(slog.NewTextHandler(log, nil))
	// A directory where the file should be, which no rename can replace.
	r.CacheFile = t.TempDir()
	r.cacheDirty, r.cacheSaved = true, time.Now().Add(-time.Hour)
	for range 3 {
		r.cacheDirty = true
		r.saveCacheSoon()
	}
	if n := strings.Count(log.String(), "cache file not saved"); n != 1 {
		t.Errorf("three families after a save that failed said so %d times, want once:\n%s", n, log)
	}
}
