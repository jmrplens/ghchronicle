package run

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// walkClock is the instant every walk in this file is dated by.
//
// Fixed and shared, because the question these tests ask is whether two runs
// land on the same rows, and a row that is genuinely a current state is
// stamped with the clock: under two real clocks every one of them would differ
// for a reason that has nothing to do with resuming.
var walkClock = time.Date(2026, 9, 17, 16, 19, 48, 0, time.UTC)

// processFacts are the measurements that describe the collector rather than
// the account, and which two runs of different lengths cannot be expected to
// agree on.
//
// gh_collector_family is each process's report of which of its families ran,
// and a walk done in two halves has two of those by design. gh_rate_limit is
// what the token had left at the moment it was read, and the second half of a
// walk has spent a different amount than a walk that was never stopped. Both
// are true statements about a process; neither is a row of the history a
// resume has to land on.
var processFacts = map[string]bool{"gh_collector_family": true, "gh_rate_limit": true}

// recorded is a sink that keeps every row it is handed, keyed the way a store
// keys one, and can be told to fail or to stop the walk at a chosen write.
type recorded struct {
	// stopAfterRepos is how many single repository writes this sink takes
	// before it cancels the walk, which is how these tests stop one inside a
	// family rather than between two of them. A write carrying exactly one
	// repository is what a family that runs per repository sends; an account
	// wide family sends every repository at once or none at all. Zero never
	// stops.
	stopAfterRepos int
	cancel         context.CancelFunc
	// refuse answers a write with an error, by the repository named on the
	// rows it was given.
	refuse map[string]bool

	mu         sync.Mutex
	writes     int
	repoWrites int
	rows       map[string]bool
}

func (c *recorded) Name() string { return "recorded" }
func (c *recorded) Close() error { return nil }

func (c *recorded) Write(_ context.Context, points []sink.Point) (int, error) {
	c.mu.Lock()
	if c.rows == nil {
		c.rows = map[string]bool{}
	}
	c.writes++
	refused := false
	repos := map[string]bool{}
	for _, p := range points {
		if !processFacts[p.Measurement] {
			c.rows[rowKey(p)] = true
		}
		// full_name and not repo: the repo tag is the bare name, and a
		// checkpoint names a repository the way the walk does.
		if repo := p.Tags["full_name"]; repo != "" {
			repos[repo] = true
			refused = refused || c.refuse[repo]
		}
	}
	if len(repos) == 1 {
		c.repoWrites++
	}
	stop := c.stopAfterRepos > 0 && len(repos) == 1 && c.repoWrites == c.stopAfterRepos
	c.mu.Unlock()
	if refused {
		return 0, fmt.Errorf("this store is refusing %d points", len(points))
	}
	if stop && c.cancel != nil {
		c.cancel()
	}
	return len(points), nil
}

// keys is every row this sink was given, so two walks can be compared.
func (c *recorded) keys() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.rows)
}

// rowKey identifies a point the way a store does: measurement, tag set,
// timestamp and the values themselves, so two runs that collected the same
// thing produce the same key and a run that collected less does not.
func rowKey(p sink.Point) string {
	var b strings.Builder
	b.WriteString(p.Measurement)
	for _, tag := range slices.Sorted(maps.Keys(p.Tags)) {
		fmt.Fprintf(&b, ",%s=%s", tag, p.Tags[tag])
	}
	for _, field := range slices.Sorted(maps.Keys(p.Fields)) {
		fmt.Fprintf(&b, " %s=%v", field, p.Fields[field])
	}
	fmt.Fprintf(&b, " %d", p.Time.UnixNano())
	return b.String()
}

// walkRunner is a backfill over the fake account the gallery fixtures
// describe, which is five repositories and a fork rather than the one the base
// fixtures list, keeping its state and its checkpoint in dir.
//
// The fake is passed in rather than made here, because the two halves of a
// resumed walk read the same account and a test wants to see every request
// both of them made.
func walkRunner(t *testing.T, dir string, fake *fakegh.Server, sinks ...sink.Sink) *Runner {
	t.Helper()
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(fake.URL())
	// Two groups and a bound, for the reason the end to end suite gives where
	// it does the same: an unbounded walk of every family against the fake
	// spends its way down to the reserve and then parks an hour waiting for a
	// window that never turns over. These two carry the account wide families
	// and three that run per repository, which is what a stop has to land in
	// the middle of.
	groups := []string{"account", "audience"}
	cfg := &config.Config{
		GitHub:       config.GitHub{Token: "test-token", BaseURL: fake.URL(), ReserveRate: 1},
		Targets:      config.Targets{User: fakegh.Login, IncludeForks: true},
		Groups:       &groups,
		StateFile:    filepath.Join(dir, "state.json"),
		AllowNoSinks: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	progress, err := OpenProgress(cfg.BackfillProgressFile(), "test-build", ScopeOf(cfg, ""), walkClock)
	if err != nil {
		t.Fatalf("opening the checkpoint: %v", err)
	}
	return &Runner{
		Cfg: cfg, API: api, Sinks: sinks,
		State:         LoadState(cfg.StateFile),
		Log:           slog.New(slog.NewTextHandler(&lockedBuffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Prime:         true,
		Backfill:      true,
		BackfillSince: walkClock.AddDate(0, 0, -30),
		Progress:      progress,
		Now:           func() time.Time { return walkClock },
	}
}

// newFake starts the fake GitHub with the richer account, so a walk has
// several repositories to stop in the middle of.
func newFake(t *testing.T) *fakegh.Server {
	t.Helper()
	return fakegh.New(t, filepath.Join("..", "..", "test", "e2e", "testdata"),
		filepath.Join("..", "..", "test", "e2e", "testdata", "gallery"))
}

// TestAStoppedAndResumedWalkEndsWithWhatAnUninterruptedOneCollects is the
// whole point of the checkpoint, and the one thing that must never be
// approximately true.
//
// The comparison is on the rows themselves and not on how many there are: a
// resume that skipped a repository whose pagination had a tail left would come
// out with fewer rows, and one that re-walked something would come out with
// the same set, which is what idempotence means and is allowed.
func TestAStoppedAndResumedWalkEndsWithWhatAnUninterruptedOneCollects(t *testing.T) {
	t.Parallel()
	// One fake for both walks, because some rows carry a url built from where
	// the API answered, and two fakes listen on two ports: the walks would
	// then differ for a reason that has nothing to do with stopping one.
	fake := newFake(t)
	whole := &recorded{}
	if err := walkRunner(t, t.TempDir(), fake, whole).Once(t.Context()); err != nil {
		t.Fatalf("the uninterrupted walk: %v", err)
	}

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Stopped three repositories into the first family that runs per
	// repository, which is the case this exists for: a family cut in half,
	// with some of its repositories written and the rest not.
	half := &recorded{stopAfterRepos: 3, cancel: cancel}
	if err := walkRunner(t, dir, fake, half).Once(ctx); err == nil {
		t.Fatal("the walk was canceled and ended without saying so")
	}
	checkpoint := readCheckpoint(t, filepath.Join(dir, "state-progress.json"))
	if len(checkpoint.Complete) == 0 {
		t.Fatal("the walk was stopped with nothing recorded, so this test proves nothing")
	}

	resumed := walkRunner(t, dir, fake, half)
	if !resumed.Progress.Resumed() {
		t.Fatal("the second run did not pick the checkpoint up, so this test proves nothing")
	}
	if err := resumed.Once(t.Context()); err != nil {
		t.Fatalf("the resumed walk: %v", err)
	}

	want, got := whole.keys(), half.keys()
	var missing []string
	for row := range want {
		if !got[row] {
			missing = append(missing, row)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("the stopped and resumed walk is missing %d of the %d rows an uninterrupted one wrote, first three:\n%s",
			len(missing), len(want), strings.Join(missing[:min(3, len(missing))], "\n"))
	}
}

// TestAResumedWalkDoesNotAskAgainAboutARepositoryItAlreadyWrote is the saving
// the checkpoint exists for: hours of somebody's rate limit.
func TestAResumedWalkDoesNotAskAgainAboutARepositoryItAlreadyWrote(t *testing.T) {
	t.Parallel()
	dir, fake := t.TempDir(), newFake(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := &recorded{stopAfterRepos: 3, cancel: cancel}
	if err := walkRunner(t, dir, fake, got).Once(ctx); err == nil {
		t.Fatal("the walk was canceled and ended without saying so")
	}

	checkpoint := readCheckpoint(t, filepath.Join(dir, "state-progress.json"))
	families, family, repos := checkpoint.Where()
	if family != "traffic" || repos == 0 {
		t.Fatalf("the walk stopped inside %s with %d repositories written and %d families complete; "+
			"this test reads traffic's own paths and has to be rewritten if it is no longer the family "+
			"a stop this early lands in", family, repos, families)
	}
	written := checkpoint.Written[family]

	before := len(fake.Requests())
	if err := walkRunner(t, dir, fake, got).Once(t.Context()); err != nil {
		t.Fatalf("the resumed walk: %v", err)
	}
	// Traffic's own endpoints, and only those: the resume walks the rest of
	// this family and goes on to the ones after it, and the repositories in
	// the checkpoint are collected by those others perfectly legitimately.
	for _, req := range fake.Requests()[before:] {
		if !strings.Contains(req.Path, "/traffic/") {
			continue
		}
		for _, repo := range written {
			if _, name, _ := strings.Cut(repo, "/"); strings.Contains(req.Path, "/"+name+"/") {
				t.Errorf("the resume asked %s, and the checkpoint said %s was already written for %s",
					req.Path, repo, family)
			}
		}
	}
}

// TestARepositoryTheStoreRefusedIsNotRecordedAsWritten: the checkpoint claims
// that a store holds those rows, and a sink that failed is the one case where
// that claim would be false. Recording it would lose that repository for good,
// because a resume never opens its family again.
func TestARepositoryTheStoreRefusedIsNotRecordedAsWritten(t *testing.T) {
	t.Parallel()
	fake := newFake(t)
	refused := "octocat/pixel-garden"
	got := &recorded{refuse: map[string]bool{refused: true}}
	r := walkRunner(t, t.TempDir(), fake, got)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("the walk: %v", err)
	}
	// traffic is the first family that runs per repository, and every
	// repository of this account has traffic rows, so the refusal lands in it.
	if !slices.Contains(r.Progress.Written["traffic"], "octocat/hello-world") {
		t.Fatalf("no repository at all was recorded for traffic (%v), so this test proves nothing",
			r.Progress.Written["traffic"])
	}
	if slices.Contains(r.Progress.Written["traffic"], refused) {
		t.Errorf("%s was recorded as written although the store refused its rows", refused)
	}
	if r.Progress.FamilyDone("traffic") {
		t.Error("traffic was recorded complete although one of its repositories never reached the store")
	}
}

// TestAHalfWalkedFamilyIsNotRecordedAsComplete: a family recorded complete is
// one a resume never opens again, so recording one that was cut in half would
// lose every repository after the cut.
func TestAHalfWalkedFamilyIsNotRecordedAsComplete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := &recorded{stopAfterRepos: 3, cancel: cancel}
	if err := walkRunner(t, dir, newFake(t), got).Once(ctx); err == nil {
		t.Fatal("the walk was canceled and ended without saying so")
	}
	checkpoint := readCheckpoint(t, filepath.Join(dir, "state-progress.json"))
	_, family, repos := checkpoint.Where()
	if family == "" || repos == 0 {
		t.Skip("the stop landed on a family boundary, which is the case this test cannot make")
	}
	if checkpoint.FamilyDone(family) {
		t.Errorf("%s is recorded complete and %d of its repositories are recorded separately, "+
			"which is a family cut in half being read as one that finished", family, repos)
	}
}

// TestAWalkThatReachedTheEndRemovesItsCheckpoint: the file describes one walk
// and a walk that is over has nothing to resume. Left behind, it would be
// compared against the next walk's settings and refuse a run that has every
// right to start.
func TestAWalkThatReachedTheEndRemovesItsCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := walkRunner(t, dir, newFake(t), &recorded{}).Once(t.Context()); err != nil {
		t.Fatalf("the walk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state-progress.json")); !os.IsNotExist(err) {
		t.Errorf("the checkpoint of a finished walk is still there (%v)", err)
	}
}

// TestAWalkStoppedInsideARateLimitWaitKeepsItsCheckpoint pins the one way a
// stopped walk reaches the end of a sweep with no error to show for it.
//
// A cancellation during the wait for the window to reset makes awaitBudget
// answer no, and every family after it is skipped rather than failed, so the
// sweep returns nil. Deciding by the error alone would delete the checkpoint
// there, which is the six hours the author lost, thrown away by the thing
// written to keep it.
func TestAWalkStoppedInsideARateLimitWaitKeepsItsCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := walkRunner(t, dir, newFake(t), &recorded{})
	if err := r.Progress.FinishFamily("account", walkClock); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.closeProgress(ctx, nil)
	if _, err := os.Stat(r.Progress.Path()); err != nil {
		t.Errorf("the checkpoint of a walk canceled without an error was removed (%v)", err)
	}
}

// TestAnOrdinarySweepKeepsNoCheckpoint: the sweep is a second concern that
// shares a directory and nothing else, and a file written on its schedule
// would be one more thing between it and the store.
func TestAnOrdinarySweepKeepsNoCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, _, log := fakeRunner(t, &recorded{})
	r.Cfg.StateFile = filepath.Join(dir, "state.json")
	r.State = LoadState(r.Cfg.StateFile)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if r.Progress.Active() {
		t.Error("a sweep opened a checkpoint")
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if strings.Contains(e.Name(), "progress") {
			t.Errorf("a sweep wrote %s", e.Name())
		}
	}
}

// TestACheckpointThatCannotBeReadRefusesToStart. A truncated or empty file is
// what a machine that lost power leaves behind, and it is a list of work
// somebody's store may or may not already hold. Refusing with a sentence costs
// a walk; starting over in silence looks like success and is the answer that
// cannot be checked afterwards.
func TestACheckpointThatCannotBeReadRefusesToStart(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"a file the kill left empty":   "",
		"a file cut off mid write":     `{"scope":{"base_url":"","targets":{"user":"octo`,
		"a file that is not this file": "not json at all\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "progress.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := OpenProgress(path, "test-build", Scope{}, walkClock)
			if err == nil {
				t.Fatalf("a checkpoint reading %q was accepted: %+v", body, p)
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "Delete it") {
				t.Errorf("the refusal names neither the file nor what to do about it: %v", err)
			}
		})
	}
}

// TestACheckpointFromAnotherWalkRefusesToStartAndSaysWhich is the refusal that
// protects a history rather than a quota.
//
// The checkpoint is a list of repositories not to walk again, and that list
// means nothing under settings it was not written for. The date bound is the
// one that would go wrong quietly: a walk bounded at a year records its
// repositories as written, and an unbounded resume would skip every one of
// them and leave a store that looks complete and stops a year back.
func TestACheckpointFromAnotherWalkRefusesToStartAndSaysWhich(t *testing.T) {
	t.Parallel()
	began := Scope{
		BaseURL:  "https://api.github.com",
		Targets:  scopeTargets(config.Targets{User: "octocat", Orgs: []string{"acme"}}),
		Families: []string{"actions", "commits", "stars"},
		Since:    "1y",
	}
	for name, tc := range map[string]struct {
		now  func(Scope) Scope
		says string
	}{
		"the date bound was widened": {
			now:  func(s Scope) Scope { s.Since = ""; return s },
			says: `the date bound was "1y" and is now none at all`,
		},
		"the account changed": {
			now: func(s Scope) Scope {
				s.Targets = scopeTargets(config.Targets{User: "someone-else", Orgs: []string{"acme"}})
				return s
			},
			says: `targets.user was "octocat" and is now "someone-else"`,
		},
		"the forks came in": {
			now: func(s Scope) Scope {
				s.Targets = scopeTargets(config.Targets{User: "octocat", Orgs: []string{"acme"}, IncludeForks: true})
				return s
			},
			says: `targets.include_forks was "false" and is now "true"`,
		},
		"a family was switched on": {
			now:  func(s Scope) Scope { s.Families = append(slices.Clone(s.Families), "deps"); return s },
			says: "the family deps is collected now and was not when the walk began",
		},
		"a family was switched off": {
			now:  func(s Scope) Scope { s.Families = []string{"actions", "stars"}; return s },
			says: "the family commits was collected when the walk began and is not now",
		},
		"the API moved": {
			now:  func(s Scope) Scope { s.BaseURL = "https://github.example.com/api/v3"; return s },
			says: `the API base url was "https://api.github.com" and is now "https://github.example.com/api/v3"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeCheckpoint(t, &Progress{Scope: began, Started: walkClock})
			_, err := OpenProgress(path, "test-build", tc.now(began), walkClock)
			if err == nil {
				t.Fatal("a checkpoint from another walk was resumed")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say what changed.\ngot:  %v\nwant it to contain: %s", err, tc.says)
			}
		})
	}
}

// TestACheckpointOfTheSameWalkIsResumedWhateverIsSpelledDifferently: a scope
// that refuses too much is a feature nobody can use. The orgs are a set and
// not a sequence, and a bound with a space around it is the same bound.
func TestACheckpointOfTheSameWalkIsResumedWhateverIsSpelledDifferently(t *testing.T) {
	t.Parallel()
	scope := func(orgs []string, since string) Scope {
		cfg := &config.Config{
			GitHub:       config.GitHub{Token: "x"},
			Targets:      config.Targets{User: "octocat", Orgs: orgs},
			AllowNoSinks: true,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return ScopeOf(cfg, since)
	}
	path := writeCheckpoint(t, &Progress{Scope: scope([]string{"acme", "beta"}, "2y"), Started: walkClock})
	p, err := OpenProgress(path, "test-build", scope([]string{"beta", "acme"}, " 2y "), walkClock)
	if err != nil {
		t.Fatalf("the same walk spelled differently was refused: %v", err)
	}
	if !p.Resumed() {
		t.Error("the checkpoint was not resumed")
	}
}

// TestABuildThatDiffersIsReportedAndNotRefused. The walk this was written for
// was stopped because a binary had to be replaced, so refusing on the version
// would refuse the one case the feature exists for. It is said instead,
// because what the checkpoint names was collected by the build that wrote it.
func TestABuildThatDiffersIsReportedAndNotRefused(t *testing.T) {
	t.Parallel()
	path := writeCheckpoint(t, &Progress{WrittenBy: "1.0.0", Started: walkClock})
	p, err := OpenProgress(path, "1.1.0", Scope{}, walkClock)
	if err != nil {
		t.Fatalf("a checkpoint from an older build was refused: %v", err)
	}
	was, upgraded := p.WrittenByAnother()
	if !upgraded || was != "1.0.0" {
		t.Errorf("the build that wrote it = %q, %t; want 1.0.0 reported", was, upgraded)
	}
	if err = p.FinishFamily("stars", walkClock); err != nil {
		t.Fatal(err)
	}
	if again := readCheckpoint(t, path); again.WrittenBy != "1.1.0" {
		t.Errorf("after a save the checkpoint still says %q wrote it", again.WrittenBy)
	}
}

// TestACheckpointIsPutInPlaceAndNeverWrittenOver: the author stops this with
// systemd and a machine can lose power, so a save that cannot finish must cost
// the save and not the walk.
//
// The temporary file is blocked by a directory of the same name, which is the
// one failure that can be arranged without permissions, since these tests run
// as whatever the CI runs as. A saver that opened the checkpoint itself would
// have truncated it before finding out.
func TestACheckpointIsPutInPlaceAndNeverWrittenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "progress.json")
	p, err := OpenProgress(path, "test-build", Scope{Since: "2y"}, walkClock)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.WroteRepo("commits", "octocat/hello-world", walkClock); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	if err = p.WroteRepo("commits", "octocat/chronicle-cli", walkClock); err == nil {
		t.Fatal("a save that could not be written reported success")
	}
	saved := readCheckpoint(t, path)
	if got := saved.Written["commits"]; !slices.Equal(got, []string{"octocat/hello-world"}) {
		t.Errorf("the checkpoint a failed save left behind = %v, want the one before it, whole", got)
	}
}

// TestEveryTargetSettingIsPartOfTheWalkItShapes is the guard on the refusal.
//
// The scope is written by hand, because a refusal has to name the setting a
// reader would edit. A setting added to config.Targets and forgotten here
// would stop being a reason to refuse, silently, and the checkpoint would then
// be resumed into a repository set it was never written for. That is the one
// mistake in this feature that corrupts a history rather than costing a
// request, so it is the one with a test that fails when somebody makes it.
func TestEveryTargetSettingIsPartOfTheWalkItShapes(t *testing.T) {
	t.Parallel()
	covered := scopeTargets(config.Targets{})
	targets := reflect.TypeFor[config.Targets]()
	for field := range targets.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if _, ok := covered[name]; !ok {
			t.Errorf("targets.%s decides which repositories a walk covers and is not part of its scope, "+
				"so a checkpoint would be resumed after it changed. Add it to scopeTargets", name)
		}
	}
	for name := range covered {
		if !slices.ContainsFunc(slices.Collect(fieldNames(targets)), func(f string) bool { return f == name }) {
			t.Errorf("the walk's scope carries %q and config.Targets has no such setting", name)
		}
	}
}

// fieldNames is the yaml name of every field of a struct type.
func fieldNames(t reflect.Type) func(func(string) bool) {
	return func(yield func(string) bool) {
		for field := range t.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			if !yield(name) {
				return
			}
		}
	}
}

// TestTheCheckpointLivesBesideTheStateFileAndNotInIt: the sweep's state is a
// claim about cadences that the long running service reads on its next tick,
// and a half walked backfill has no business in it.
func TestTheCheckpointLivesBesideTheStateFileAndNotInIt(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{
		"/var/lib/ghchronicle/backfill-state.json": "/var/lib/ghchronicle/backfill-state-progress.json",
		"ghchronicle-state.json":                   "ghchronicle-state-progress.json",
		"":                                         "",
	} {
		cfg := &config.Config{StateFile: state}
		if got := cfg.BackfillProgressFile(); got != want {
			t.Errorf("a state file at %q keeps its checkpoint at %q, want %q", state, got, want)
		}
	}
}

// TestTheMarksOfACompletedFamilyOutliveTheProcessThatMadeThem: a resumed walk
// has to leave the same state file behind as one that was never stopped, and
// the families the first half finished were never marked by the second.
func TestTheMarksOfACompletedFamilyOutliveTheProcessThatMadeThem(t *testing.T) {
	t.Parallel()
	ran := walkClock.Add(-2 * time.Hour)
	p := &Progress{path: "progress.json", Complete: []FamilyDone{{Family: "stars", At: ran}}}
	s := LoadState("")
	p.Restore(s)
	if got := s.LastRun["stars"]; !got.Equal(ran) {
		t.Errorf("stars is marked as run at %v, want the instant the walk finished it, %v", got, ran)
	}
}

// writeCheckpoint puts a checkpoint on disk the way a stopped walk leaves one,
// and answers with its path.
func writeCheckpoint(t *testing.T, p *Progress) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "progress.json")
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readCheckpoint reads one back the way a person with cat does.
func readCheckpoint(t *testing.T, path string) *Progress {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the checkpoint: %v", err)
	}
	p := &Progress{}
	if err = json.Unmarshal(body, p); err != nil {
		t.Fatalf("the checkpoint is not readable as one: %v\n%s", err, body)
	}
	return p
}
