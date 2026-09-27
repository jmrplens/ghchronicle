package run

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// turnsSlow is six of the slow families, per repository and none of them
// batched, so a runner over no repositories runs and marks each of them
// without asking GitHub anything.
var turnsSlow = map[string]string{
	"traffic": "6h", "planning": "6h", "settings": "6h", "stats": "12h", "inventory": "24h", "rulesets": "24h",
}

// turnsRunner is a runner whose sweeps never reach the network, like
// tickRunner, with the six slow families of turnsSlow and actions at every
// tick of a five minute heartbeat.
func turnsRunner(t *testing.T) (*Runner, *lockedBuffer) {
	t.Helper()
	every := config.Every{Default: "0", Families: map[string]string{"actions": "5m"}}
	maps.Copy(every.Families, turnsSlow)
	cfg := &config.Config{
		GitHub: config.GitHub{Token: "token"}, Targets: config.Targets{Repos: []string{"o/n"}},
		AllowNoSinks: true, Every: every, Heartbeat: "5m",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log, buf := debugLog()
	r := &Runner{Cfg: cfg, API: ghapi.New("token", time.Second), State: LoadState(""), Log: log}
	r.repos, r.reposAt = []collect.Repo{}, time.Now()
	return r, buf
}

// serveFor runs the loop in a synctest bubble for the first sweep and the
// ticks that fit in d, each finished before the fake clock moves on. Under
// the hour, so the repository list is never rebuilt and nothing is asked of
// the network.
func serveFor(t *testing.T, r *Runner, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()
	time.Sleep(d)
	synctest.Wait()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Serve = %v, want the context's end", err)
	}
}

// TestTheLoopStartsSlowFamiliesDueTogetherOneASweep is #87 through the loop
// itself: six slow families all due in its first sweep start one a sweep, in
// the order of their cadences, and leave the loop holding six different
// last_run values, while the family at every tick runs at every tick. Before
// the rule all six ran in the first sweep and kept one last_run between them,
// which is what put the daily families of the production account in one
// sweep every day.
func TestTheLoopStartsSlowFamiliesDueTogetherOneASweep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, log := turnsRunner(t)
		first := time.Now()
		serveFor(t, r, 6*5*time.Minute+time.Second)

		// A fresh state has every family overdue since the same instant,
		// so the shorter cadence goes first, and a tie goes by name.
		want := []string{"planning", "settings", "traffic", "stats", "inventory", "rulesets"}
		for i, family := range want {
			if at := r.State.LastRun[family]; !at.Equal(first.Add(time.Duration(i) * 5 * time.Minute)) {
				t.Errorf("%s started at %s, want the sweep %d ticks after the first",
					family, at.Sub(first), i)
			}
		}
		if at := r.State.LastRun["actions"]; !at.Equal(first.Add(30 * time.Minute)) {
			t.Errorf("actions last ran %s after the first sweep, want every tick up to the seventh at 30m", at.Sub(first))
		}
		if got := strings.Count(log.String(), "slow families due together take turns"); got != 5 {
			t.Errorf("%d sweeps said families were waiting their turn, want the five that left one waiting:\n%s", got, log)
		}
	})
}

// TestEveryOtherKindOfSweepStartsEverySlowFamilyDue: the rule is the loop's.
// -once has no next tick to leave a family for, the first sweep of a loop
// that primes runs every family to fill the exporter, and a backfill runs
// every family because that is what it was asked for, so each of them starts
// the six slow families in one sweep.
func TestEveryOtherKindOfSweepStartsEverySlowFamilyDue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(r *Runner)
		serve bool
	}{
		{name: "-once", setup: func(*Runner) {}},
		{name: "a backfill", setup: func(r *Runner) { r.Backfill = true }},
		{name: "the primed first sweep of the loop", setup: func(r *Runner) { r.Prime = true }, serve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				r, log := turnsRunner(t)
				tc.setup(r)
				now := time.Now()
				if tc.serve {
					// The first sweep only: the next tick is five minutes on.
					serveFor(t, r, time.Minute)
				} else if err := r.Once(t.Context()); err != nil {
					t.Fatal(err)
				}
				for family := range turnsSlow {
					if at := r.State.LastRun[family]; !at.Equal(now) {
						t.Errorf("%s last ran at %v, want the one sweep at %v", family, at, now)
					}
				}
				if strings.Contains(log.String(), "take turns") {
					t.Errorf("a sweep that runs every family held one back:\n%s", log)
				}
			})
		})
	}
}

// scheduleRunner is a runner on the built-in cadences, with those extra ones
// set, that sweeps as the loop does and collects nothing.
func scheduleRunner(t *testing.T, every config.Every) (*Runner, time.Duration) {
	t.Helper()
	cfg := &config.Config{GitHub: config.GitHub{Token: "t"}, Targets: config.Targets{User: "u"}, AllowNoSinks: true, Every: every}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	r := &Runner{
		Cfg: cfg, State: LoadState(filepath.Join(t.TempDir(), "state.json")),
		Log: slog.New(slog.DiscardHandler), serving: true,
	}
	tick, _ := r.tick()
	return r, tick
}

// slowFamilies is every family the runner's configuration collects at six
// hours or more.
func slowFamilies(r *Runner) []string {
	var slow []string
	for _, family := range config.Families() {
		if every, on := r.Cfg.Interval(family); on && every >= slowCadence {
			slow = append(slow, family)
		}
	}
	return slow
}

// sweep is one tick of a schedule: its instant, every family it ran and the
// slow ones among them.
type sweep struct {
	at      time.Time
	ran     []string
	started []string
}

// schedule sweeps the ticks of days the way the loop does, from a state where
// every slow family ran together a day before the first tick, so all of them
// are due in it, and marks every family a sweep runs as run, except the ones
// in failing, whose every pass fails.
func schedule(r *Runner, tick time.Duration, days int, failing ...string) []sweep {
	start := time.Date(2026, 9, 27, 0, 3, 0, 0, time.UTC)
	for _, family := range slowFamilies(r) {
		r.State.Mark(family, start.Add(-24*time.Hour))
	}
	var sweeps []sweep
	for i := range int(time.Duration(days) * 24 * time.Hour / tick) {
		// Late by the few milliseconds the loop is late after each tick, as
		// runsInADay is.
		now := start.Add(time.Duration(i)*tick + time.Millisecond*time.Duration(1+2*((i+1)%2)))
		r.takeTurns(now)
		s := sweep{at: now}
		for _, family := range config.Families() {
			every, on := r.Cfg.Interval(family)
			if !on || !r.due(family, every, now) || r.held[family] {
				continue
			}
			s.ran = append(s.ran, family)
			if every >= slowCadence {
				s.started = append(s.started, family)
			}
			if !slices.Contains(failing, family) {
				r.State.Mark(family, now)
			}
		}
		sweeps = append(sweeps, s)
	}
	return sweeps
}

// startsOf is every instant a family started in a schedule.
func startsOf(sweeps []sweep, family string) []time.Time {
	var at []time.Time
	for _, s := range sweeps {
		if slices.Contains(s.started, family) {
			at = append(at, s.at)
		}
	}
	return at
}

// TestAWeekOfTicksSpreadsTheSlowFamiliesOnceAndForAll is the simulation #87
// asks for, on the production account's schedule: the built-in cadences with
// deps and history named at a day, thirteen families of six hours or more, all
// of them due in the same sweep. No sweep starts more than one, so every
// last_run is its own from the sweep the last of them starts in; that last one
// waits twelve quarter hour ticks, 3h, the worst wait this schedule has;
// and from then on every family starts exactly a cadence after it last did,
// because once apart they stay apart. The families at every tick run at every
// tick throughout.
func TestAWeekOfTicksSpreadsTheSlowFamiliesOnceAndForAll(t *testing.T) {
	t.Parallel()
	r, tick := scheduleRunner(t, config.Every{Families: map[string]string{"deps": "24h", "history": "24h"}})
	if tick != 15*time.Minute {
		t.Fatalf("tick = %s, want the quarter hour the built-in cadences give", tick)
	}
	slow := slowFamilies(r)
	if len(slow) != 13 {
		t.Fatalf("%d slow families, want the thirteen of the production schedule: %v", len(slow), slow)
	}
	sweeps := schedule(r, tick, 7)
	for _, s := range sweeps {
		if len(s.started) > 1 {
			t.Errorf("the sweep at %s started %v, want one", s.at.Format(time.DateTime), s.started)
		}
		for _, family := range []string{"actions", "events", "activity", "ratelimit"} {
			if !slices.Contains(s.ran, family) {
				t.Errorf("%s, every tick, did not run in the sweep at %s", family, s.at.Format(time.DateTime))
			}
		}
	}
	var worst time.Duration
	for _, family := range slow {
		worst = max(worst, assertACadenceApart(t, r, sweeps, family, tick))
	}
	if want := time.Duration(len(slow)-1) * tick; worst.Round(time.Minute) != want {
		t.Errorf("the last slow family started %s after the first sweep, want %s: n-1 ticks", worst, want)
	}
	seen := map[time.Time]string{}
	for _, family := range slow {
		at := r.State.LastRun[family]
		if other, clash := seen[at]; clash {
			t.Errorf("%s and %s still share the last_run %s after a week", family, other, at.Format(time.DateTime))
		}
		seen[at] = family
	}
}

// assertACadenceApart holds every start of a family after its first to
// exactly a cadence after the one before, and reports how long after the
// first sweep the first start came.
func assertACadenceApart(t *testing.T, r *Runner, sweeps []sweep, family string, tick time.Duration) time.Duration {
	t.Helper()
	every, _ := r.Cfg.Interval(family)
	at := startsOf(sweeps, family)
	if len(at) == 0 {
		t.Errorf("%s never started", family)
		return 0
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < every-tick/2 || gap > every+tick/2 {
			t.Errorf("%s, every %s, started %s after its previous start at %s",
				family, every, gap, at[i-1].Format(time.DateTime))
		}
	}
	span := sweeps[len(sweeps)-1].at.Sub(sweeps[0].at)
	if want := int(span/every) - 1; len(at) < want {
		t.Errorf("%s, every %s, started %d times in %s, want at least %d", family, every, len(at), span, want)
	}
	return at[0].Sub(sweeps[0].at)
}

// TestAFamilyThatKeepsFailingDoesNotTakeEveryTurn: a pass that fails is not
// marked as run, so by last_run alone the failing family is the most overdue
// on every sweep. Ordered that way, measured with branches failing, every
// daily family behind it and the six and twelve hour ones after their first
// start never ran again in three days. Counting from when this process last
// let a family start puts the failure behind the others, and every one of them
// keeps its cadence.
func TestAFamilyThatKeepsFailingDoesNotTakeEveryTurn(t *testing.T) {
	t.Parallel()
	r, tick := scheduleRunner(t, config.Every{})
	sweeps := schedule(r, tick, 3, "branches")
	for _, family := range slowFamilies(r) {
		if family == "branches" {
			continue
		}
		every, _ := r.Cfg.Interval(family)
		if got, want := len(startsOf(sweeps, family)), int(3*24*time.Hour/every); got != want {
			t.Errorf("%s, every %s, started %d times in three days beside a family that always fails, want %d",
				family, every, got, want)
		}
	}
	if len(startsOf(sweeps, "branches")) < 3 {
		t.Error("the family that fails was not tried again")
	}
}

// TestASweepStartsMoreWhenOneCannotKeepTheCadences: a configuration can have
// more slow families than one a tick can start within their cadence. At
// every.default: 6h the loop ticks hourly and thirty-one families run every
// six hours; one a sweep, measured, started the last of them thirty hours late
// and kept every family near a cadence of thirty-one hours. Six a sweep is the
// fewest that fit, and no family then waits five hours or more.
func TestASweepStartsMoreWhenOneCannotKeepTheCadences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		slow          int
		shortest, tik time.Duration
		want          int
	}{
		{"the built-in table", 11, 6 * time.Hour, 15 * time.Minute, 1},
		{"the production schedule", 13, 6 * time.Hour, 15 * time.Minute, 1},
		{"every.default: 6h", 31, 6 * time.Hour, time.Hour, 6},
		{"nothing faster than the hour", 11, 6 * time.Hour, time.Hour, 2},
		{"a heartbeat longer than the cadence", 5, 6 * time.Hour, 12 * time.Hour, 5},
	} {
		if got := turnsPerSweep(tc.slow, tc.shortest, tc.tik); got != tc.want {
			t.Errorf("%s: %d slow families, the shortest every %s, a tick of %s: %d a sweep, want %d",
				tc.name, tc.slow, tc.shortest, tc.tik, got, tc.want)
		}
	}

	r, tick := scheduleRunner(t, config.Every{Default: "6h"})
	if tick != time.Hour || len(slowFamilies(r)) != 31 {
		t.Fatalf("tick %s over %d slow families, want an hour over thirty-one", tick, len(slowFamilies(r)))
	}
	sweeps := schedule(r, tick, 3)
	for _, family := range slowFamilies(r) {
		at := startsOf(sweeps, family)
		prev := sweeps[0].at.Add(-6 * time.Hour)
		for _, next := range at {
			if late := next.Sub(prev) - 6*time.Hour; late > 5*time.Hour+time.Minute {
				t.Errorf("%s started %s late at %s", family, late.Round(time.Minute), next.Format(time.DateTime))
			}
			prev = next
		}
	}
}

// TestAPrimedRestartKeepsTheTurnsTheFamiliesHad: the first sweep of a loop
// that primes the exporter runs every slow family, and records only the one
// that was due, so the others go on at the times they had. Marked with the
// primed sweep's instant, as they were before, all six would be due together
// again, and the day after every restart they would take turns long enough
// for the exporter to drop the series of the last of them.
func TestAPrimedRestartKeepsTheTurnsTheFamiliesHad(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, log := turnsRunner(t)
		r.Prime = true
		start := time.Now()
		had := map[string]time.Time{
			"traffic": start.Add(-6 * time.Hour), "planning": start.Add(-6*time.Hour + 5*time.Minute),
			"settings": start.Add(-6*time.Hour + 10*time.Minute), "stats": start.Add(-time.Hour),
			"inventory": start.Add(-2 * time.Hour), "rulesets": start.Add(-3 * time.Hour),
		}
		for family, at := range had {
			r.State.Mark(family, at)
		}
		serveFor(t, r, 10*time.Minute+time.Second)

		want := map[string]time.Time{
			"traffic": start, "planning": start.Add(5 * time.Minute), "settings": start.Add(10 * time.Minute),
			"stats": had["stats"], "inventory": had["inventory"], "rulesets": had["rulesets"],
		}
		for family, at := range want {
			if got := r.State.LastRun[family]; !got.Equal(at) {
				t.Errorf("%s last ran %s from the primed sweep, want %s", family, got.Sub(start), at.Sub(start))
			}
		}
		if !strings.Contains(log.String(), "first sweep after start-up") {
			t.Errorf("the first sweep was not the primed one:\n%s", log)
		}
		if strings.Contains(log.String(), "take turns") {
			t.Errorf("families that had their own turns were due together:\n%s", log)
		}
	})
}

// accountTurnsRunner is a loop's runner over the fake GitHub, on a quarter
// hour heartbeat, collecting only the families given, at the cadences given,
// with its clock at *at. Its sweeps are driven one by one with Once, so each
// runs the account families through Runner.family, the way Serve runs them.
func accountTurnsRunner(t *testing.T, families map[string]string, at *time.Time) (*Runner, *lockedBuffer) {
	t.Helper()
	r, fake, log := fakeRunner(t)
	// The profile page the achievements family reads is the fake's too.
	r.Cfg.GitHub.BaseURL = fake.URL()
	r.Cfg.Every = config.Every{Default: "0", Families: families}
	r.Cfg.Heartbeat = "15m"
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Prime, r.serving = false, true
	r.Now = func() time.Time { return *at }
	return r, log
}

// TestAccountFamiliesTakeTurnsToo: three of the built-in families of six hours
// or more ask about the account and not about a repository, achievements,
// keys and profile, and they reach the sweep through Runner.family rather than
// the per-repository loop. All three due at once start one a sweep, the
// shortest cadence first, and are left with three different last_run values.
func TestAccountFamiliesTakeTurnsToo(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Truncate(time.Second)
	first := at
	r, log := accountTurnsRunner(t, map[string]string{"achievements": "24h", "keys": "24h", "profile": "12h"}, &at)
	for range 3 {
		if err := r.Once(t.Context()); err != nil {
			t.Fatalf("Once: %v\n%s", err, log)
		}
		at = at.Add(15 * time.Minute)
	}
	for i, family := range []string{"profile", "achievements", "keys"} {
		if got, want := r.State.LastRun[family], first.Add(time.Duration(i)*15*time.Minute); !got.Equal(want) {
			t.Errorf("%s started %s after the first sweep, want %s", family, got.Sub(first), want.Sub(first))
		}
	}
}

// TestAPrimedRestartLeavesTheAccountFamiliesTheirTurns is
// TestAPrimedRestartKeepsTheTurnsTheFamiliesHad for the families that ask
// about the account: the primed first sweep runs all three and marks only the
// one that was due, so the other two keep the last_run they had instead of
// sharing the restart's.
func TestAPrimedRestartLeavesTheAccountFamiliesTheirTurns(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Truncate(time.Second)
	r, log := accountTurnsRunner(t, map[string]string{"achievements": "24h", "keys": "24h", "profile": "12h"}, &at)
	r.Prime = true
	had := map[string]time.Time{
		"profile": at.Add(-12 * time.Hour), "achievements": at.Add(-2 * time.Hour), "keys": at.Add(-3 * time.Hour),
	}
	for family, last := range had {
		r.State.Mark(family, last)
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "first sweep after start-up") {
		t.Fatalf("the sweep was not the primed one:\n%s", log)
	}
	for family, want := range map[string]time.Time{"profile": at, "achievements": had["achievements"], "keys": had["keys"]} {
		if got := r.State.LastRun[family]; !got.Equal(want) {
			t.Errorf("%s last ran %s from the primed sweep, want %s", family, got.Sub(at), want.Sub(at))
		}
	}
}

// TestTotalsWaitingItsTurnStillSizeThePullRequestPage: a first sweep with no
// page sizes runs totals before the pull requests it sizes, and that holds
// when totals, at a day, is due beside a slow family more overdue than it and
// so would wait its turn. It runs anyway, and the other family starts too.
func TestTotalsWaitingItsTurnStillSizeThePullRequestPage(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Truncate(time.Second)
	r, log := accountTurnsRunner(t, map[string]string{"totals": "24h", "profile": "12h", "issues": "1h"}, &at)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "starting=profile waiting=totals") {
		t.Fatalf("totals was not the family waiting its turn, so this proves nothing:\n%s", log)
	}
	if len(r.counts) == 0 {
		t.Errorf("totals waited its turn and the pull requests ran with no page sizes:\n%s", log)
	}
	if last := r.State.LastRun["profile"]; !last.Equal(at) {
		t.Errorf("profile last ran at %s, want the one sweep", last)
	}
}

// TestAnAccountFamilyWithNoAccountTakesNoTurn: a configuration that names
// repositories alone has no login for the account families to ask about, so
// one of them at six hours never runs, and it must not take the turn a
// per-repository family of a day would have had.
func TestAnAccountFamilyWithNoAccountTakesNoTurn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, log := turnsRunner(t)
		r.Cfg.Every = config.Every{Default: "0", Families: map[string]string{"profile": "6h", "rulesets": "24h"}}
		if err := r.Cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		first := time.Now()
		serveFor(t, r, time.Minute)
		if at := r.State.LastRun["rulesets"]; !at.Equal(first) {
			t.Errorf("rulesets last ran at %v, want the first sweep at %v:\n%s", at, first, log)
		}
		if _, ran := r.State.LastRun["profile"]; ran {
			t.Error("profile ran with no account to ask about")
		}
	})
}
