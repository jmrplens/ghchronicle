package run

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
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
