package migrate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// startOf is the start after an upgrade from 2.6.1 of every kind of store:
// InfluxDB holding this configuration's rows alone, PostgreSQL holding a
// second account's comments, the three stores that cannot be asked, and the
// log it writes to.
func startOf(t *testing.T, auto bool) (Start, *bytes.Buffer) {
	t.Helper()
	in := upgradeInput(t, oldShape(), oldShape("octocat", "hubot"), nil)
	in.TrustRecord = true
	Stamp(in.State, in.Config, in.Release)
	var log bytes.Buffer
	return Start{
		Plan: Make(t.Context(), in), Auto: auto, CanApply: func(string) bool { return true },
		DryRun: "ghchronicle -config c.yaml -migrate", Apply: "ghchronicle -config c.yaml -migrate -yes",
		Others: "-migrate-others", Service: true, State: in.State, Now: in.Now,
		Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, &log
}

// linesOf is every line of a log that carries all of the words given.
func linesOf(log string, words ...string) []string {
	var out []string
	for line := range strings.SplitSeq(log, "\n") {
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(line, w) }) {
			out = append(out, line)
		}
	}
	return out
}

// TestAStartAppliesOnItsOwnOnlyWhatLosesNothing is the maintainer's rule,
// all three parts of it: of every pending item, a start under auto applies
// the one in a store that sets rows aside, holds this configuration's rows
// alone, and whose history GitHub serves whole, and says every other one at
// WARN with the commands that apply it.
func TestAStartAppliesOnItsOwnOnlyWhatLosesNothing(t *testing.T) {
	t.Parallel()
	s, log := startOf(t, true)
	chosen := s.Decide(t.Context())
	if len(chosen) != 1 || chosen[0].Store != "influxdb" || chosen[0].Item.Migration.ID != comments {
		t.Fatalf("a start applies %v, want the comments in InfluxDB alone", chosenIDs(chosen))
	}
	out := log.String()
	if got := linesOf(out, "level=WARN", `msg="applying a migration before the first sweep"`, "sink=influxdb",
		"migration="+comments, "why=", "action=", "set aside"); len(got) != 1 {
		t.Errorf("the item applied is not announced at WARN with what, why and where:\n%s", out)
	}
	for _, c := range []struct{ store, id, reason string }{
		// Rows of a repository the configuration no longer covers.
		{"influxdb", scanning, "does not bring back every row"},
		// Rows of an account it does not collect.
		{"postgres", comments, "hubot"},
		// Stores that keep nothing aside.
		{"sql", comments, "the DROP reaches whatever the file is replayed into"},
		{"graphite", comments, "only whoever runs the Graphite host"},
		{"telegraf", comments, "cannot reach the store behind Telegraf"},
	} {
		lines := linesOf(out, "level=WARN", `msg="migration pending"`, "sink="+c.store, "migration="+c.id)
		if len(lines) != 1 {
			t.Errorf("%s in %s is not warned about once:\n%s", c.id, c.store, out)
			continue
		}
		for _, want := range []string{
			c.reason, `plan="ghchronicle -config c.yaml -migrate"`,
			"apply=\"ghchronicle -config c.yaml -migrate -yes", "first=\"stop this service",
		} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("the warning for %s in %s does not say %q:\n%s", c.id, c.store, want, lines[0])
			}
		}
	}
	if got := linesOf(out, `msg="migration pending"`, "sink=postgres", "migration="+comments,
		"-migrate -yes -migrate-others"); len(got) != 1 {
		t.Errorf("a store holding hubot's rows is not given -migrate-others:\n%s", out)
	}
	if got := linesOf(out, `msg="migration pending"`, "sink=influxdb", "-migrate-others"); len(got) != 0 {
		t.Errorf("a store holding this configuration's rows alone is told to add -migrate-others:\n%s", got)
	}
}

// TestUnderWarnAStartAppliesNothing: every pending item is said, the safe
// one too, with the setting as the reason.
func TestUnderWarnAStartAppliesNothing(t *testing.T) {
	t.Parallel()
	s, log := startOf(t, false)
	if chosen := s.Decide(t.Context()); len(chosen) != 0 {
		t.Fatalf("a start under warn applies %v", chosenIDs(chosen))
	}
	lines := linesOf(log.String(), "level=WARN", `msg="migration pending"`, "sink=influxdb", "migration="+comments)
	if len(lines) != 1 || !strings.Contains(lines[0], "migrate: warn applies nothing on its own") {
		t.Errorf("the safe item under warn is said as %v", lines)
	}
}

// TestAStartDoesNotApplyWhatThisBuildCannot: a store with no way to bring it
// along is warned about as pending, never announced as being applied.
func TestAStartDoesNotApplyWhatThisBuildCannot(t *testing.T) {
	t.Parallel()
	s, log := startOf(t, true)
	s.CanApply = func(store string) bool { return store != "influxdb" }
	if chosen := s.Decide(t.Context()); len(chosen) != 0 {
		t.Fatalf("a start applies %v with no way to", chosenIDs(chosen))
	}
	if got := linesOf(log.String(), `msg="migration pending"`, "sink=influxdb", "migration="+comments,
		"no way to bring influxdb along"); len(got) != 1 {
		t.Errorf("the item this build cannot apply is not said so:\n%s", log.String())
	}
}

// TestAStartRecordsWhatItNeedNotAskAgain: what a start finds not needed, and
// the notes it has said, are recorded; the next start takes the record's
// word for them and asks a store with nothing else open nothing at all, not
// even which server it is; and a note said once at Info is said at Debug
// after.
func TestAStartRecordsWhatItNeedNotAskAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := planConfig(t, dir, func(c *config.Config) {
		c.Sinks = config.Sinks{Influx: &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"}}
	})
	held := map[string]teardown.Shape{
		"gh_actions_cache_entry": {Exists: true, Rows: 90, Oldest: time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)},
	}
	state := olderState(t, dir)
	Stamp(state, cfg, "2.6.2")
	first := &fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: held}
	var log bytes.Buffer
	start := func(store *fakeStore) Start {
		in := Input{
			Config: cfg, State: state, Release: "2.6.2", TrustRecord: true,
			Inspectors: []teardown.Inspector{store}, Repos: []string{}, ReposKnown: true,
		}
		return Start{
			Plan: Make(t.Context(), in), Auto: true, State: state, Now: time.Now(),
			Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}
	}
	if chosen := start(first).Decide(t.Context()); len(chosen) != 0 {
		t.Fatalf("nothing is pending, and a start applies %v", chosenIDs(chosen))
	}
	rec := state.Stores["influxdb"]
	for _, id := range []string{comments, dependabot, scanning} {
		if _, ok := rec.NotNeeded[id]; !ok {
			t.Errorf("%s was found not needed and is not recorded: %+v", id, rec)
		}
	}
	if _, ok := rec.Noted[caches]; !ok {
		t.Errorf("the note about the cache entries is not recorded: %+v", rec)
	}
	if got := linesOf(log.String(), "level=INFO", `msg="migration noted: nothing is changed"`, caches); len(got) != 1 {
		t.Errorf("the first start does not say the note at Info:\n%s", log.String())
	}

	log.Reset()
	second := &fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: held}
	start(second).Decide(t.Context())
	if second.described != 0 || len(second.asked) != 0 {
		t.Errorf("a start with every migration settled asked the store %d times and about %v",
			second.described, second.asked)
	}
	if got := linesOf(log.String(), "level=DEBUG", `msg="migration noted: nothing is changed"`, caches); len(got) != 1 {
		t.Errorf("the second start does not say the note at Debug alone:\n%s", log.String())
	}
	if strings.Contains(log.String(), "level=INFO") || strings.Contains(log.String(), "level=WARN") {
		t.Errorf("a settled start says more than Debug:\n%s", log.String())
	}

	// The dry run does not take the record's word: the store is asked.
	third := &fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: held}
	Make(t.Context(), Input{
		Config: cfg, State: state, Release: "2.6.2",
		Inspectors: []teardown.Inspector{third}, Repos: []string{}, ReposKnown: true,
	})
	if third.described != 1 || len(third.asked) != len(Registry) {
		t.Errorf("a dry run took the record's word: described %d, asked %v", third.described, third.asked)
	}
}

// TestAFrozenMeasurementIsSaidAndNotTrusted: nothing the configuration runs
// writes the comments, so their old rows are history. That is said once at
// Info, and the record does not settle it, since switching a family back on
// makes it pending again.
func TestAFrozenMeasurementIsSaidAndNotTrusted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := planConfig(t, dir, func(c *config.Config) {
		c.Sinks = config.Sinks{Influx: &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"}}
		c.Every.Families = map[string]string{"discussions": "0", "outbound": "0"}
	})
	state := olderState(t, dir)
	Stamp(state, cfg, "2.6.2")
	var log bytes.Buffer
	in := Input{
		Config: cfg, State: state, Release: "2.6.2", TrustRecord: true, Repos: []string{}, ReposKnown: true,
		Inspectors: []teardown.Inspector{&fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: oldShape()}},
	}
	s := Start{
		Plan: Make(t.Context(), in), Auto: true, State: state, Now: time.Now(),
		Log: slog.New(slog.NewTextHandler(&log, nil)),
	}
	s.Decide(t.Context())
	if got := linesOf(log.String(), "level=INFO", "migration frozen", comments); len(got) != 1 {
		t.Errorf("the frozen comments are not said at Info:\n%s", log.String())
	}
	in.Config = planConfig(t, dir, func(c *config.Config) {
		c.Sinks = config.Sinks{Influx: &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"}}
	})
	in.Inspectors = []teardown.Inspector{&fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: oldShape()}}
	if it := itemOf(t, Make(t.Context(), in), "influxdb", comments); it.Status != Pending {
		t.Errorf("with outbound back on the comments are %s, want pending", it.Status)
	}
}

// TestAStoreThatDoesNotAnswerAtAStartIsWarnedAndNotRecorded: nothing is
// known of it, so nothing is recorded and the next start asks again.
func TestAStoreThatDoesNotAnswerAtAStartIsWarnedAndNotRecorded(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, nil, nil, nil)
	in.TrustRecord = true
	in.Inspectors[0] = &fakeStore{name: "influxdb", err: errors.New("dial tcp: connection refused")}
	Stamp(in.State, in.Config, in.Release)
	var log bytes.Buffer
	s := Start{
		Plan: Make(t.Context(), in), Auto: true, State: in.State, Now: in.Now,
		Log: slog.New(slog.NewTextHandler(&log, nil)),
	}
	s.Decide(t.Context())
	if got := linesOf(log.String(), "level=WARN", "the store did not answer", "sink=influxdb", "connection refused"); len(got) != 1 {
		t.Errorf("the store that did not answer is not warned about:\n%s", log.String())
	}
	if rec := in.State.Stores["influxdb"]; len(rec.NotNeeded)+len(rec.Noted)+len(rec.Applied) > 0 {
		t.Errorf("a store that did not answer has a record: %+v", rec)
	}
}

// TestAStoreThatHangsDelaysAStartByItsTimeoutAlone: the questions to each
// store are bounded, so one that never answers is unreachable after the
// timeout and the others are still asked.
func TestAStoreThatHangsDelaysAStartByItsTimeoutAlone(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, nil, oldShape(), nil)
	in.StoreTimeout = 50 * time.Millisecond
	in.Inspectors[0] = hangingStore{}
	began := time.Now()
	p := Make(t.Context(), in)
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("a store that never answers held the plan for %s", took)
	}
	for _, st := range p.Stores {
		if st.Name == "influxdb" && !errors.Is(st.Err, context.DeadlineExceeded) {
			t.Errorf("the hanging store ended with %v, want its deadline", st.Err)
		}
	}
	if it := itemOf(t, p, "postgres", comments); it.Status != Pending {
		t.Errorf("the store after the hanging one is %s, want still asked and pending", it.Status)
	}
}

// hangingStore answers nothing until it is given up on.
type hangingStore struct{}

func (hangingStore) Name() string { return "influxdb" }

func (hangingStore) Describe(ctx context.Context) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (hangingStore) Shape(ctx context.Context, _ string, _, _ []string) (teardown.Shape, error) {
	<-ctx.Done()
	return teardown.Shape{}, ctx.Err()
}

// TestTheRepositoryListIsReadOnlyWhenAStoreWantsIt: a start whose stores hold
// nothing old asks GitHub nothing for it; one that holds rows keyed by
// repository reads it once for every store.
func TestTheRepositoryListIsReadOnlyWhenAStoreWantsIt(t *testing.T) {
	t.Parallel()
	calls := 0
	load := func(context.Context) ([]string, error) {
		calls++
		return []string{"octocat/hello-world"}, nil
	}
	in := upgradeInput(t, nil, nil, nil)
	in.Repos, in.ReposKnown, in.LoadRepos = nil, false, load
	p := Make(t.Context(), in)
	if calls != 0 || len(p.Notes) != 0 {
		t.Errorf("stores holding nothing old read the list %d times, notes %v", calls, p.Notes)
	}
	in = upgradeInput(t, oldShape(), oldShape(), nil)
	in.Repos, in.ReposKnown, in.LoadRepos = nil, false, load
	p = Make(t.Context(), in)
	if calls != 1 {
		t.Errorf("the list was read %d times for two stores, want once", calls)
	}
	if it := itemOf(t, p, "postgres", scanning); !slices.Contains(it.Lost,
		"the rows of 1 repository this configuration no longer covers: octocat/gone") {
		t.Errorf("the list read lazily was not compared with the store: %v", it.Lost)
	}
	in = upgradeInput(t, oldShape(), nil, nil)
	in.Repos, in.ReposKnown = nil, false
	in.LoadRepos = func(context.Context) ([]string, error) { return nil, errors.New("401 Unauthorized") }
	if p = Make(t.Context(), in); len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "401 Unauthorized") {
		t.Errorf("a list that could not be read is not noted: %v", p.Notes)
	}
}

// chosenIDs names chosen items for a failure message.
func chosenIDs(chosen []Chosen) []string {
	out := make([]string, 0, len(chosen))
	for _, c := range chosen {
		out = append(out, c.Store+":"+c.Item.Migration.ID)
	}
	return out
}

// TestAStartKeptFromTheStateFileSaysWhoKeptIt: the items a one-shot run
// meant to apply and could not, because the service holds the state file,
// are warned about with the holder as the reason.
func TestAStartKeptFromTheStateFileSaysWhoKeptIt(t *testing.T) {
	t.Parallel()
	s, log := startOf(t, true)
	chosen := s.Decide(t.Context())
	log.Reset()
	s.Held(chosen, "the state file is in use by process 42")
	if got := linesOf(log.String(), "level=WARN", `msg="migration pending"`, "sink=influxdb",
		"not_applied=\"the state file is in use by process 42\""); len(got) != 1 {
		t.Errorf("the held item is not said with its holder:\n%s", log.String())
	}
}
