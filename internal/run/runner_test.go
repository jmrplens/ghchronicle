package run

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

func TestReserveScalesToTheBucket(t *testing.T) {
	r := &Runner{Cfg: &config.Config{GitHub: config.GitHub{ReserveRate: 500}}}
	// Core is large, so the configured reserve applies as written.
	if got := r.reserveFor(ghapi.RateState{Limit: 5000}); got != 500 {
		t.Errorf("core reserve = %d, want 500", got)
	}
	// Search allows thirty a minute. A flat reserve of five hundred would mean
	// the bucket is never usable, which is how one search call stopped a sweep.
	if got := r.reserveFor(ghapi.RateState{Limit: 30}); got != 6 {
		t.Errorf("search reserve = %d, want 6", got)
	}
	_ = time.Now
}

func TestBackfillWaitsForTheWindowRatherThanSkipping(t *testing.T) {
	// A normal sweep protects the reserve and skips. A backfill is run
	// deliberately and the only thing that matters is that it finishes, so it
	// parks until the window turns over.
	r := &Runner{
		Cfg: &config.Config{GitHub: config.GitHub{ReserveRate: 500}},
		API: ghapi.New("token", 0),
	}
	if r.Backfill {
		t.Fatal("the zero value must not backfill")
	}
	r.Backfill = true
	// Nothing has been requested, so no bucket has reported a budget and
	// there is nothing to wait for.
	if d := r.timeToReset(); d != 0 {
		t.Errorf("with no rate seen there is nothing to wait for, got %s", d)
	}
}

// sweepRunner returns a runner pointed at srv with one repository already
// discovered, so a test can drive the sweep loop without a discovery round.
func sweepRunner(t *testing.T, handler http.HandlerFunc) *Runner {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	api := ghapi.New("token", 5*time.Second)
	api.SetBaseURL(srv.URL)

	cfg := &config.Config{
		// BaseURL names the server too: the achievements family derives
		// the profile page's host from it.
		GitHub:       config.GitHub{Token: "token", BaseURL: srv.URL},
		Targets:      config.Targets{User: "o"},
		AllowNoSinks: true,
		// One family only, so the test drives the loop rather than the
		// whole schedule.
		Every: everyOnly("traffic"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Cfg:   cfg,
		API:   api,
		State: LoadState(""),
		Log:   slog.New(slog.DiscardHandler),
	}
	r.repos = []collect.Repo{{Owner: "o", Name: "n", FullName: "o/n"}}
	r.reposAt = time.Now()
	return r
}

// everyOnly disables every family but the ones named, which is what makes a
// sweep in a test one collector wide. A default of zero and then the families
// to keep, which is the shape the three layers exist for: naming thirty-one
// families to switch off is the thing default replaced.
func everyOnly(keep ...string) config.Every {
	every := config.Every{Default: "0", Families: map[string]string{}}
	for _, name := range keep {
		every.Families[name] = "1m"
	}
	return every
}

func TestAFamilyThatFailedEverywhereIsNotMarkedAsRun(t *testing.T) {
	// Marking it would hide the outage until the family's next cadence, which
	// for the slow ones is half a day.
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if when, marked := r.State.LastRun["traffic"]; marked {
		t.Errorf("a family that failed on every repository was marked as run at %s", when)
	}
}

// answerTraffic is the shape the four traffic endpoints answer with: the two
// counters are objects and the two top-ten lists are arrays.
func answerTraffic(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/popular/") {
		_, _ = w.Write([]byte(`[]`))
		return
	}
	_, _ = w.Write([]byte(`{"count":1,"uniques":1,` +
		`"views":[{"timestamp":"2026-09-07T00:00:00Z","count":1,"uniques":1}],` +
		`"clones":[{"timestamp":"2026-09-07T00:00:00Z","count":1,"uniques":1}]}`))
}

func TestASweepThatSucceedsMarksTheFamily(t *testing.T) {
	r := sweepRunner(t, answerTraffic)
	if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, marked := r.State.LastRun["traffic"]; !marked {
		t.Error("a family that collected must be marked, or the next sweep repeats it")
	}
}

func TestACanceledSweepStopsWithoutMarkingAnything(t *testing.T) {
	// Without this every remaining repository logs its own "context canceled"
	// and the family is then marked as done, which is exactly backwards.
	r := sweepRunner(t, answerTraffic)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.repoFamilies(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, marked := r.State.LastRun["traffic"]; marked {
		t.Error("a canceled sweep must leave the family unmarked")
	}
}

// groupRunner is sweepRunner with the schedule narrowed by groups rather than
// by every, so a test can drive the sweep loop the way a user's config does.
func groupRunner(t *testing.T, groups []string, every config.Every, handler http.HandlerFunc) *Runner {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	api := ghapi.New("token", 5*time.Second)
	api.SetBaseURL(srv.URL)

	cfg := &config.Config{
		// BaseURL names the server too: the achievements family derives
		// the profile page's host from it.
		GitHub:       config.GitHub{Token: "token", BaseURL: srv.URL},
		Targets:      config.Targets{User: "o"},
		AllowNoSinks: true,
		Groups:       &groups,
		Every:        every,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Cfg:   cfg,
		API:   api,
		State: LoadState(""),
		Log:   slog.New(slog.DiscardHandler),
	}
	r.repos = []collect.Repo{{Owner: "o", Name: "n", FullName: "o/n"}}
	r.reposAt = time.Now()
	return r
}

// TestASweepSkipsFamiliesOutsideTheSelection proves the half of "neither read
// nor returned" that a config test cannot: not that the family is absent from
// a map, but that no request for it ever leaves the process.
//
// The group is audience and its other two families are then switched off by
// cadence, which leaves traffic as the only thing that may legally be asked
// for. Everything the handler would otherwise see, the repository detail, the
// issues, the workflow runs, is excluded by the group and by nothing else.
func TestASweepSkipsFamiliesOutsideTheSelection(t *testing.T) {
	// Guarded, because the handler runs on the server's goroutine and the
	// assertion below reads the slice on the test's.
	var mu sync.Mutex
	var strayed []string
	r := groupRunner(t, []string{"audience"}, config.Every{Families: map[string]string{"stars": "0", "forks": "0"}},
		func(w http.ResponseWriter, req *http.Request) {
			if !strings.Contains(req.URL.Path, "/traffic/") {
				mu.Lock()
				strayed = append(strayed, req.URL.Path)
				mu.Unlock()
			}
			answerTraffic(w, req)
		})
	if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(strayed) > 0 {
		t.Errorf("a family outside the selected group was requested anyway: %v", strayed)
	}
	if _, marked := r.State.LastRun["traffic"]; !marked {
		t.Error("the selected family must still collect and be marked")
	}
	for _, family := range []string{"repo", "issues", "actions", "deps"} {
		if _, marked := r.State.LastRun[family]; marked {
			t.Errorf("%s is outside the selection and was marked as run", family)
		}
	}
}

// TestTheTickFollowsTheSelection: the sweep tick is the shortest enabled
// cadence, so dropping the two fifteen-minute groups lengthens it. This is a
// consequence of the design rather than a feature, and it is pinned here
// because internal/run computes it from the same narrowed map and was not
// changed to do so.
func TestTheTickFollowsTheSelection(t *testing.T) {
	full := &Runner{Cfg: &config.Config{
		GitHub: config.GitHub{Token: "t"}, Targets: config.Targets{User: "o"}, AllowNoSinks: true,
	}}
	if err := full.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := full.shortestInterval(); got != 15*time.Minute {
		t.Errorf("the default tick = %s, want 15m", got)
	}

	var kept []string
	for _, group := range config.Groups() {
		if group != "ci" && group != "collector" {
			kept = append(kept, group)
		}
	}
	narrowed := &Runner{Cfg: &config.Config{
		GitHub: config.GitHub{Token: "t"}, Targets: config.Targets{User: "o"}, AllowNoSinks: true,
		Groups: &kept,
	}}
	if err := narrowed.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := narrowed.shortestInterval(); got != 30*time.Minute {
		t.Errorf("without ci and collector the tick = %s, want 30m", got)
	}
}

// TestTheHeartbeatForcesTheLoopTick pins the fourth knob. It is not a cadence:
// it sets no family's interval, and it is the one way to make the loop turn
// faster than the one minute floor that guards a mistyped cadence.
func TestTheHeartbeatForcesTheLoopTick(t *testing.T) {
	tickOf := func(heartbeat string) (time.Duration, string) {
		t.Helper()
		cfg := &config.Config{
			GitHub:       config.GitHub{Token: "token"},
			Targets:      config.Targets{User: "o"},
			AllowNoSinks: true,
			Every:        everyOnly("traffic"),
			Heartbeat:    heartbeat,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return (&Runner{Cfg: cfg}).tick()
	}

	// traffic is the only family left and it is at 1m, so the derived tick is
	// one minute and the heartbeat has to be able to beat it downwards.
	if got, from := tickOf(""); got != time.Minute || from != "shortest cadence" {
		t.Errorf("with no heartbeat the tick is %v from %q, want 1m from the shortest cadence", got, from)
	}
	if got, from := tickOf("200ms"); got != 200*time.Millisecond || from != "heartbeat" {
		t.Errorf("heartbeat 200ms gave a tick of %v from %q", got, from)
	}
	// And it wins upwards as well, which is what makes it a knob rather than
	// a floor: the warning about that is the config package's business.
	if got, _ := tickOf("2h"); got != 2*time.Hour {
		t.Errorf("heartbeat 2h gave a tick of %v", got)
	}
}

// bucket is one rate limit bucket the way GitHub reports it, in the
// x-ratelimit-* headers of a response. A zero reset sends no reset header.
type bucket struct {
	resource         string
	limit, remaining int
	reset            time.Time
}

// header writes the bucket onto a response.
func (b bucket) header(h http.Header) {
	h.Set("x-ratelimit-resource", b.resource)
	h.Set("x-ratelimit-limit", strconv.Itoa(b.limit))
	h.Set("x-ratelimit-remaining", strconv.Itoa(b.remaining))
	if !b.reset.IsZero() {
		h.Set("x-ratelimit-reset", strconv.FormatInt(b.reset.Unix(), 10))
	}
}

// debugLog is a logger that keeps every line, debug included, for the test
// to read.
func debugLog() (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// budgetRunner is a runner with the default reserve of five hundred whose
// client has already been answered once per bucket given, so the budget it
// judges is exactly those buckets and nothing else.
func budgetRunner(t *testing.T, buckets ...bucket) (*Runner, *lockedBuffer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		i, err := strconv.Atoi(strings.TrimPrefix(req.URL.Path, "/bucket/"))
		if err != nil || i < 0 || i >= len(buckets) {
			http.NotFound(w, req)
			return
		}
		buckets[i].header(w.Header())
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	api := ghapi.New("token", 5*time.Second)
	api.SetBaseURL(srv.URL)
	for i := range buckets {
		if _, _, err := api.GetJSON(t.Context(), "/bucket/"+strconv.Itoa(i), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	log, buf := debugLog()
	return &Runner{Cfg: &config.Config{GitHub: config.GitHub{ReserveRate: 500}}, API: api, Log: log}, buf
}

// TestTheBudgetIsJudgedByTheBucketsTheSweepSpends: a bucket stops the sweep
// when it is at or under its reserve and its window has not turned over, a
// bucket nothing has charged yet stops nothing, and neither does one that
// reports no limit, because a reserve of a fifth of nothing is no reserve.
func TestTheBudgetIsJudgedByTheBucketsTheSweepSpends(t *testing.T) {
	t.Parallel()
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		buckets []bucket
		left    bool
	}{
		{"a bucket well above its reserve, the other two never charged", []bucket{{"core", 5000, 4000, later}}, true},
		{"a bucket exactly at its reserve", []bucket{{"core", 5000, 500, later}}, false},
		{"a bucket under its reserve until its window resets", []bucket{{"core", 5000, 100, later}}, false},
		{"a bucket under its reserve that says no reset", []bucket{{"core", 5000, 100, time.Time{}}}, false},
		{"a bucket under its reserve whose window already reset", []bucket{{"core", 5000, 100, earlier}}, true},
		{"a bucket that reports no limit", []bucket{{"core", 0, 0, later}}, true},
		{"a healthy core beside a spent search", []bucket{{"core", 5000, 4000, later}, {"search", 30, 2, later}}, false},
	} {
		r, _ := budgetRunner(t, tc.buckets...)
		if got := r.budgetLeft(); got != tc.left {
			t.Errorf("%s: budget left = %v, want %v", tc.name, got, tc.left)
		}
	}
}

// TestABackfillWaitsForTheBucketThatResetsLast: the wait is until the most
// constrained bucket refills, a second past the reset the server named,
// counting a bucket at exactly its reserve and leaving out one above it and
// one that reports no limit.
func TestABackfillWaitsForTheBucketThatResetsLast(t *testing.T) {
	t.Parallel()
	soon, last := time.Now().Add(10*time.Minute), time.Now().Add(40*time.Minute)
	// Core is judged first, so the graphql bucket that resets sooner is the
	// one that must not shorten the wait.
	r, _ := budgetRunner(t,
		bucket{"core", 5000, 500, last},
		bucket{"graphql", 5000, 100, soon},
		bucket{"search", 30, 20, time.Now().Add(3 * time.Hour)},
	)
	before := time.Now()
	got := r.timeToReset()
	after := time.Now()
	refilled := time.Unix(last.Unix(), 0).Add(time.Second)
	if lo, hi := refilled.Sub(after), refilled.Sub(before); got < lo || got > hi {
		t.Errorf("wait = %s, want a second past the core reset, between %s and %s", got, lo, hi)
	}

	r, _ = budgetRunner(t, bucket{"core", 0, 0, soon})
	if wait := r.timeToReset(); wait != 0 {
		t.Errorf("a bucket that reports no limit made a backfill wait %s", wait)
	}
	r, _ = budgetRunner(t, bucket{"core", 5000, 100, time.Time{}})
	if wait := r.timeToReset(); wait != 0 {
		t.Errorf("a bucket that names no reset made a backfill wait %s", wait)
	}
}

// TestTheWaitForAResetIsBoundedBothWays: a reset hours away, which is a
// stale header or a skewed clock, is an hour of sleep at most, and one a
// moment ago is a second rather than a loop that spins.
func TestTheWaitForAResetIsBoundedBothWays(t *testing.T) {
	t.Parallel()
	r, _ := budgetRunner(t, bucket{"core", 5000, 100, time.Now().Add(3 * time.Hour)})
	if got := r.timeToReset(); got != time.Hour {
		t.Errorf("a reset three hours away = a wait of %s, want an hour", got)
	}
	// The header is in whole seconds, so a reset stamped with this second is
	// less than a second ago for as long as the clock stays in it. A call
	// that straddles the next second measures something else and is asked
	// again.
	for attempt := 0; ; attempt++ {
		start := time.Now()
		r, _ = budgetRunner(t, bucket{"core", 5000, 100, start})
		got := r.timeToReset()
		if time.Now().Unix() != start.Unix() {
			if attempt < 5 {
				continue
			}
			t.Fatal("every attempt straddled a second, so the clamp was never measured")
		}
		if got != time.Second {
			t.Errorf("a reset under a second ago = a wait of %s, want one second", got)
		}
		break
	}
}

// TestASweepSkipsAtTheReserveAndABackfillWaitsForTheWindow: a sweep answers
// at once and says the family was skipped; a backfill waits for the window
// and stops waiting when its sweep is canceled; and a backfill with no reset
// to wait for goes on without saying it waited.
func TestASweepSkipsAtTheReserveAndABackfillWaitsForTheWindow(t *testing.T) {
	t.Parallel()
	r, log := budgetRunner(t, bucket{"core", 5000, 100, time.Now().Add(30 * time.Minute)})
	if r.awaitBudget(t.Context(), "traffic") {
		t.Error("a sweep at its reserve went on")
	}
	if !strings.Contains(log.String(), `msg="rate limit reserve reached, family skipped" family=traffic`) {
		t.Errorf("the sweep did not say the family was skipped:\n%s", log)
	}

	r.Backfill = true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r.awaitBudget(ctx, "traffic") {
		t.Error("a backfill whose sweep was canceled went on as if the window had reset")
	}
	if !strings.Contains(log.String(), "waiting for the window to reset") {
		t.Errorf("the backfill did not say it was waiting:\n%s", log)
	}

	r, log = budgetRunner(t, bucket{"core", 5000, 100, time.Time{}})
	r.Backfill = true
	if !r.awaitBudget(t.Context(), "traffic") {
		t.Error("a backfill with no reset to wait for stopped")
	}
	if strings.Contains(log.String(), "waiting") {
		t.Errorf("a backfill with nothing to wait for said it waited:\n%s", log)
	}
}

// TestASpentReserveSkipsBothKindsOfFamilyAndLeavesThemDue: neither an
// account family nor a repository family asks anything once the reserve is
// reached, and neither is marked, so the next sweep takes them.
func TestASpentReserveSkipsBothKindsOfFamilyAndLeavesThemDue(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/bucket" {
			bucket{"core", 5000, 100, time.Now().Add(time.Hour)}.header(w.Header())
			_, _ = w.Write([]byte(`{}`))
			return
		}
		asked.Add(1)
		answerTraffic(w, req)
	})
	r.Cfg.Every = everyOnly("traffic", "events")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.API.GetJSON(t.Context(), "/bucket", nil, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.accountFamilies(t.Context(), now)
	if err := r.repoFamilies(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("%d requests were made past the reserve", n)
	}
	if len(r.State.LastRun) != 0 {
		t.Errorf("families skipped for budget were marked: %v", r.State.LastRun)
	}
}

// TestTheReserveReachedMidFamilyStopsBeforeTheNextRepository: the brake is
// checked between repositories, not only between families, because one
// family over forty repositories can spend a whole window on its own.
func TestTheReserveReachedMidFamilyStopsBeforeTheNextRepository(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := map[string]int{}
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked[strings.Join(strings.SplitN(req.URL.Path, "/", 5)[:4], "/")]++
		mu.Unlock()
		bucket{"core", 5000, 100, time.Now().Add(time.Hour)}.header(w.Header())
		answerTraffic(w, req)
	})
	log, buf := debugLog()
	r.Log = log
	r.repos = []collect.Repo{{Owner: "o", Name: "a", FullName: "o/a"}, {Owner: "o", Name: "b", FullName: "o/b"}}
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["/repos/o/a"] == 0 || asked["/repos/o/b"] != 0 {
		t.Errorf("asked %v, want the first repository and then nothing", asked)
	}
	if !strings.Contains(buf.String(), `msg="stopping this family here" family=traffic at=o/a`) {
		t.Errorf("the log does not say where the family stopped:\n%s", buf)
	}
}

// TestARepositoryThatRunsOutOfBudgetLeavesTheFamilyUnmarked: a spent budget
// answered mid-family is not that repository failing. The family stops
// there, keeps what the repositories before it collected, and is left
// unmarked so the next sweep takes it instead of the next cadence.
func TestARepositoryThatRunsOutOfBudgetLeavesTheFamilyUnmarked(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := map[string]bool{}
	got := &captured{name: "captured"}
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked[strings.Join(strings.SplitN(req.URL.Path, "/", 5)[:4], "/")] = true
		mu.Unlock()
		if strings.HasPrefix(req.URL.Path, "/repos/o/b/") {
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		answerTraffic(w, req)
	})
	r.Sinks = []sink.Sink{got}
	r.repos = []collect.Repo{
		{Owner: "o", Name: "a", FullName: "o/a"},
		{Owner: "o", Name: "b", FullName: "o/b"},
		{Owner: "o", Name: "c", FullName: "o/c"},
	}
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if when, marked := r.State.LastRun["traffic"]; marked {
		t.Errorf("a family that ran out of budget mid-way was marked as run at %s", when)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["/repos/o/c"] {
		t.Error("the repository after the spent budget was asked anyway")
	}
	if got.measured("gh_traffic") == 0 {
		t.Error("what the repository before the spent budget collected was not written")
	}
}

// TestTheRepositoryListIsReusedForExactlyTheDiscoveryInterval: a list an
// hour old is still the list, and one a moment older is rebuilt.
func TestTheRepositoryListIsReusedForExactlyTheDiscoveryInterval(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`[]`))
	})
	now := time.Now()
	r.reposAt = now.Add(-discoverInterval)
	if err := r.discoverRepos(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n != 0 || names(r.repos) != "o/n" {
		t.Errorf("a list exactly an hour old was rebuilt: %d requests, repos %q", n, names(r.repos))
	}
	later := now.Add(time.Nanosecond)
	if err := r.discoverRepos(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if asked.Load() == 0 || !r.reposAt.Equal(later) {
		t.Errorf("a list past the hour was not rebuilt: %d requests, rebuilt at %s", asked.Load(), r.reposAt)
	}
}

// TestAConfigWithoutAUserSkipsTheAccountFamiliesAndSaysSo: a configuration
// that names repositories alone has no login to hand the account families,
// so none of them asks anything or is marked, and the log says why once.
func TestAConfigWithoutAUserSkipsTheAccountFamiliesAndSaysSo(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	r.Cfg.Targets = config.Targets{Repos: []string{"o/n"}}
	r.Cfg.Every = config.Every{}
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, enabled := r.Cfg.Interval("events"); !enabled {
		t.Fatal("events is off by default, so this test proves nothing")
	}
	log, buf := debugLog()
	r.Log = log
	r.accountFamilies(t.Context(), time.Now())
	if n := asked.Load(); n != 0 || len(r.State.LastRun) != 0 {
		t.Errorf("without a user the account families made %d requests and marked %v", n, r.State.LastRun)
	}
	if strings.Count(buf.String(), "no targets.user, account-wide families skipped") != 1 {
		t.Errorf("the log does not say once why the account families were skipped:\n%s", buf)
	}
}

// TestAnAccountFamilyThatFailsIsNotMarkedAsRun is the account-wide twin of
// the per-repository rule: marking a failed family hides the outage until
// its next cadence.
func TestAnAccountFamilyThatFailsIsNotMarkedAsRun(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	r.Cfg.Every = everyOnly("events")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log, buf := debugLog()
	r.Log = log
	r.accountFamilies(t.Context(), time.Now())
	if when, marked := r.State.LastRun["events"]; marked {
		t.Errorf("a failed account family was marked as run at %s", when)
	}
	if !strings.Contains(buf.String(), `msg="collector failed" family=events`) {
		t.Errorf("the failure is not in the log:\n%s", buf)
	}
}

// TestASweepCanceledAmongTheRepositoriesSavesNothing: the cancellation that
// reaches the repository loop ends the sweep there, with the error and
// without the state being saved or the sweep said to be finished.
func TestASweepCanceledAmongTheRepositoriesSavesNothing(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, answerTraffic)
	path := filepath.Join(t.TempDir(), "state.json")
	r.State = LoadState(path)
	log, buf := debugLog()
	r.Log = log
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Once(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Once = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a canceled sweep saved the state: %v", err)
	}
	if strings.Contains(buf.String(), "sweep finished") {
		t.Errorf("a canceled sweep said it finished:\n%s", buf)
	}
}

// TestAStateThatCannotBeSavedIsSaidInTheLog: finish has nobody to return an
// error to, so a state file that could not be written is a warning, or the
// next start re-collects everything with no line saying why.
func TestAStateThatCannotBeSavedIsSaidInTheLog(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := sweepRunner(t, answerTraffic)
	r.State = LoadState(filepath.Join(blocker, "state.json"))
	log, buf := debugLog()
	r.Log = log
	r.finish()
	if !strings.Contains(buf.String(), `msg="state not saved"`) {
		t.Errorf("an unsaved state is not in the log:\n%s", buf)
	}

	r.State = LoadState(filepath.Join(t.TempDir(), "state.json"))
	r.Log, buf = debugLog()
	r.finish()
	if strings.Contains(buf.String(), "state not saved") {
		t.Errorf("a state that was saved was reported as not saved:\n%s", buf)
	}
}

// TestServeLogsAFailedSweepAndReturnsWhenCanceled: a sweep that fails does
// not end the loop, which says so and goes on to wait for the next tick or
// the end of its context.
func TestServeLogsAFailedSweepAndReturnsWhenCanceled(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, answerTraffic)
	// No repository list, so the first sweep has to discover one, which a
	// canceled context cannot.
	r.repos = nil
	log, buf := debugLog()
	r.Log = log
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Serve(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Serve = %v, want the context's end", err)
	}
	if !strings.Contains(buf.String(), `msg="sweep failed"`) {
		t.Errorf("the failed sweep is not in the log:\n%s", buf)
	}
}

// tickRunner is a runner whose sweeps never reach the network: no login, no
// family enabled, and a repository list discovered as long ago as the test
// says. The loop it drives therefore turns on nothing but the clock, which in
// a synctest bubble is a fake one, so a tick is exact and costs no waiting.
func tickRunner(t *testing.T, discoveredAgo time.Duration) (*Runner, *lockedBuffer) {
	t.Helper()
	cfg := &config.Config{
		GitHub: config.GitHub{Token: "token"}, Targets: config.Targets{Repos: []string{"o/n"}},
		AllowNoSinks: true, Every: everyOnly(), Heartbeat: "1m",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log, buf := debugLog()
	r := &Runner{Cfg: cfg, API: ghapi.New("token", time.Second), State: LoadState(""), Log: log}
	r.repos, r.reposAt = []collect.Repo{}, time.Now().Add(-discoveredAgo)
	return r, buf
}

// TestEverySweepOnATickIsJudgedByItsOwnAnswer: the loop reports a sweep a
// tick started as failed exactly when that sweep failed, and goes on either
// way. The first sweep runs before the ticker exists and has its own check,
// so a loop that said "sweep failed" after every good tick, or kept quiet
// about a bad one, would pass every test that only looks at the first.
func TestEverySweepOnATickIsJudgedByItsOwnAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// discoveredAgo puts the next discovery either past the ticks the
		// test waits for or on the first of them.
		discoveredAgo time.Duration
		// named replaces the configured repositories after validation, so
		// a discovery fails without asking GitHub anything.
		named            []string
		finished, failed int
	}{
		{name: "every tick succeeds", finished: 3},
		{
			name: "every tick fails", discoveredAgo: discoverInterval - 30*time.Second,
			named: []string{"not-a-full-name"}, finished: 1, failed: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				r, log := tickRunner(t, tc.discoveredAgo)
				if tc.named != nil {
					r.Cfg.Targets.Repos = tc.named
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- r.Serve(ctx) }()
				// The first sweep at once, then the ticks at one and two
				// minutes, each finished before the fake clock moves on.
				time.Sleep(2*time.Minute + time.Second)
				synctest.Wait()
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Errorf("Serve = %v, want the context's end", err)
				}
				if got := strings.Count(log.String(), `msg="sweep finished"`); got != tc.finished {
					t.Errorf("%d sweeps finished, want %d:\n%s", got, tc.finished, log)
				}
				if got := strings.Count(log.String(), `msg="sweep failed"`); got != tc.failed {
					t.Errorf("%d sweeps reported as failed, want %d:\n%s", got, tc.failed, log)
				}
				if tc.failed > 0 && !strings.Contains(log.String(), "is not owner/name") {
					t.Errorf("the failed sweep does not say why:\n%s", log)
				}
			})
		})
	}
}

// TestACadenceUnderAMinuteDoesNotSpinTheLoop: the loop's floor is a minute,
// whatever the shortest cadence says, unless a heartbeat forces it.
func TestACadenceUnderAMinuteDoesNotSpinTheLoop(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		GitHub: config.GitHub{Token: "token"}, Targets: config.Targets{User: "o"}, AllowNoSinks: true,
		Every: config.Every{Default: "0", Families: map[string]string{"traffic": "10s"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if d, _ := cfg.Interval("traffic"); d != 10*time.Second {
		t.Fatalf("traffic cadence = %s, want the ten seconds asked for", d)
	}
	if got := (&Runner{Cfg: cfg}).shortestInterval(); got != time.Minute {
		t.Errorf("a ten second cadence gave a tick of %s, want the one minute floor", got)
	}
}

// TestTheCommitsWindowIsTwiceTheCadenceAndAMonthOnTheFirstSweep: a sweep
// overlaps the one before it by reading two cadences back, the first sweep
// of a fresh install reads a month, and a backfill walks to its own bound.
func TestTheCommitsWindowIsTwiceTheCadenceAndAMonthOnTheFirstSweep(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	if got := r.commits(now); !got.Since.Equal(now.AddDate(0, 0, -30)) || got.Walk.Pages != 0 {
		t.Errorf("the first sweep = %+v, want a month back", got)
	}
	every, _ := r.Cfg.Interval("commits")
	r.State.Mark("commits", now.Add(-every))
	if got := r.commits(now); !got.Since.Equal(now.Add(-2*every)) || got.Walk.Pages != 0 {
		t.Errorf("a sweep = %+v, want two cadences back, %s", got, now.Add(-2*every))
	}
	r.Backfill, r.BackfillSince = true, now.AddDate(-1, 0, 0)
	if got := r.commits(now); !got.Since.Equal(r.BackfillSince) || got.Walk.Pages != -1 {
		t.Errorf("a backfill = %+v, want every page back to BackfillSince", got)
	}
}

// TestJobLogsReachBackNoFurtherThanGitHubKeepsThem: a sweep reads two
// cadences back; a backfill walks ninety days, which is where GitHub starts
// answering 410, or less when BackfillSince says so, and never more.
func TestJobLogsReachBackNoFurtherThanGitHubKeepsThem(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	r.Cfg.Every = config.Every{Families: map[string]string{"joblogs": "1h"}}
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	if got := r.jobLogs(now); !got.Since.Equal(now.Add(-2*time.Hour)) || got.Walk.Pages != 0 || got.MaxJobs != 0 {
		t.Errorf("a sweep = %+v, want two hours back on the collector's own page", got)
	}
	ninety := now.AddDate(0, 0, -90)
	r.Backfill = true
	for _, tc := range []struct {
		name         string
		since, reach time.Time
	}{
		{"no bound", time.Time{}, ninety},
		{"a bound past ninety days", now.AddDate(-1, 0, 0), ninety},
		{"a bound within ninety days", now.AddDate(0, 0, -10), now.AddDate(0, 0, -10)},
	} {
		r.BackfillSince = tc.since
		got := r.jobLogs(now)
		if !got.Since.Equal(tc.reach) || !got.Walk.Since.Equal(tc.reach) || got.Walk.Pages != -1 || got.MaxJobs != 500 {
			t.Errorf("a backfill with %s = %+v, want every page back to %s, five hundred jobs", tc.name, got, tc.reach)
		}
	}
}

// TestTheJobLogsFamilyAsksForTheFailedRuns: switched on by name, the family
// lists each repository's failed runs and is marked when that answered.
func TestTheJobLogsFamilyAsksForTheFailedRuns(t *testing.T) {
	t.Parallel()
	var failures atomic.Int32
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/repos/o/n/actions/runs" && req.URL.Query().Get("status") == "failure" {
			failures.Add(1)
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	r.Cfg.Every = everyOnly("joblogs")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if failures.Load() == 0 {
		t.Error("the joblogs family never listed the failed runs")
	}
	if _, marked := r.State.LastRun["joblogs"]; !marked {
		t.Error("the joblogs family answered and was not marked")
	}
}

// TestTheDependencyDiffStartsWhereTheLastOneEnded: the first sweep has only
// the photograph and remembers the head it saw, the next diffs from that
// head to the new one, and a sweep that could not read the head keeps the
// one it had rather than forgetting where the next range starts.
func TestTheDependencyDiffStartsWhereTheLastOneEnded(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	head := "aaa"
	var compared []string
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch path := req.URL.Path; {
		case strings.HasSuffix(path, "/commits/HEAD") && head != "":
			_, _ = w.Write([]byte(head))
		case strings.HasSuffix(path, "/dependency-graph/sbom"):
			_, _ = w.Write([]byte(`{"sbom":{"packages":[]}}`))
		case strings.Contains(path, "/dependency-graph/compare/"):
			_, after, _ := strings.Cut(path, "/compare/")
			compared = append(compared, after)
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	r.Cfg.Every = everyOnly("deps")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sweep := func(now time.Time, next string) {
		t.Helper()
		mu.Lock()
		head = next
		mu.Unlock()
		if err := r.repoFamilies(t.Context(), now); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	sweep(start, "aaa")
	if got := r.State.LastHead["o/n"]; got != "aaa" {
		t.Errorf("after the first sweep the remembered head is %q, want aaa", got)
	}
	sweep(start.Add(2*time.Minute), "bbb")
	mu.Lock()
	diffs := strings.Join(compared, ",")
	mu.Unlock()
	if diffs != "aaa...bbb" || r.State.LastHead["o/n"] != "bbb" {
		t.Errorf("the second sweep diffed %q and remembered %q, want aaa...bbb and bbb", diffs, r.State.LastHead["o/n"])
	}
	sweep(start.Add(4*time.Minute), "")
	if got := r.State.LastHead["o/n"]; got != "bbb" {
		t.Errorf("a sweep that could not read the head left %q, want bbb kept", got)
	}
}

// TestAnAccountWithNoRepositoriesStillMarksItsFamilies: no repository failed
// when there were none to fail, so the family has run. Left unmarked it would
// be due on every tick and warn every time that it failed everywhere.
func TestAnAccountWithNoRepositoriesStillMarksItsFamilies(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, answerTraffic)
	r.repos = []collect.Repo{}
	log, buf := debugLog()
	r.Log = log
	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, marked := r.State.LastRun["traffic"]; !marked {
		t.Error("a family over no repositories was left unmarked")
	}
	if strings.Contains(buf.String(), "failed everywhere") {
		t.Errorf("a family over no repositories was said to have failed:\n%s", buf)
	}
}

// TestACardOnlySweepLeavesTheStateFileAsItFoundIt: the sweep that feeds
// nothing but a card writes none of what it learned, and the same sweep
// feeding a store writes it. Every field in the state file is a claim that
// something was delivered somewhere, and a card-only sweep delivers to
// nobody, so a claim it left behind would make the next collection skip a
// family whose data went into a picture and nowhere else.
func TestACardOnlySweepLeavesTheStateFileAsItFoundIt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		cardOnly bool
		saved    bool
	}{
		{name: "a card-only sweep saves nothing", cardOnly: true},
		{name: "a sweep that feeds a store saves what it learned", saved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			log, _ := debugLog()
			r := &Runner{
				Cfg: &config.Config{}, API: ghapi.New("token", time.Second),
				State: LoadState(path), Log: log, CardOnly: tc.cardOnly,
			}
			r.State.Mark("traffic", now)
			r.State.LastEvent = "42"
			r.finish()

			written := LoadState(path)
			if got := written.LastRun["traffic"].Equal(now); got != tc.saved {
				t.Errorf("the state file remembers the family that ran = %v, want %v", got, tc.saved)
			}
			if got := written.LastEvent == "42"; got != tc.saved {
				t.Errorf("the state file remembers the newest event = %v, want %v", got, tc.saved)
			}
		})
	}
}
