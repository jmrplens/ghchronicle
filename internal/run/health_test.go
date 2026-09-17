package run

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// one is the single point of a measurement whose tags match, and a failure
// naming what was there when there is not exactly one.
func (k *kept) one(t *testing.T, measurement string, tags map[string]string) sink.Point {
	t.Helper()
	var found []sink.Point
	for _, p := range k.rows(measurement) {
		matches := true
		for key, want := range tags {
			if p.Tags[key] != want {
				matches = false
			}
		}
		if matches {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d rows of %s with %v, want 1: %v", len(found), measurement, tags, k.rows(measurement))
	}
	return found[0]
}

// TestARepositoryWhoseSecondCallFailsKeepsWhatItsFirstCollected is the defect
// this file is named after, driven end to end: the fake answers the run
// listing and the jobs of the newest run, then 502s the jobs of the next one,
// exactly as GitHub did on five of the author's repositories on 2026-09-16.
// The runs and jobs already rendered have to reach the sink, and the
// repository has to be counted as failed all the same.
func TestARepositoryWhoseSecondCallFailsKeepsWhatItsFirstCollected(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, fake, log := fakeRunner(t, store)
	fake.Fail("/repos/octocat/hello-world/actions/runs/1000163134/jobs", http.StatusBadGateway)

	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}

	if n := len(store.rows("gh_workflow_job")); n == 0 {
		t.Error("the 502 on the second run's jobs threw away the jobs of the first")
	}
	if n := len(store.rows("gh_workflow_run")); n == 0 {
		t.Error("the 502 on one run's jobs threw away the runs collected before it")
	}
	// Counted as failed, which with one repository is every repository, so
	// the family is not marked and the next sweep asks again.
	if when, marked := r.State.LastRun["actions"]; marked {
		t.Errorf("a repository that half failed was marked as collected at %s", when)
	}
	if !strings.Contains(log.String(), `family=actions repo=octocat/hello-world`) {
		t.Errorf("the failure was not reported in the log:\n%s", log)
	}
}

// TestTheSweepWritesDownWhichFamilyFailedOnWhichRepository is the other half:
// the failure above has to be readable from the store, because that is where
// the dashboard reads. One row per family that ran, and one more per
// repository a family could not collect, naming it and why.
func TestTheSweepWritesDownWhichFamilyFailedOnWhichRepository(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, fake, log := fakeRunner(t, store)
	fake.Fail("/repos/octocat/hello-world/actions/runs/1000163134/jobs", http.StatusBadGateway)

	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}

	failure := store.one(t, "gh_collector_family", map[string]string{
		"family": "actions", "scope": "repo",
	})
	if failure.Tags["full_name"] != "octocat/hello-world" || failure.Tags["reason"] != "502" {
		t.Errorf("the failure row names %v, want the repository and the status", failure.Tags)
	}
	if text, _ := failure.Fields["error"].(string); !strings.Contains(text, "/jobs") {
		t.Errorf("the failure row says %q, want the request that failed", text)
	}

	family := store.one(t, "gh_collector_family", map[string]string{
		"family": "actions", "scope": "family",
	})
	if family.Fields["failed"] != 1 || family.Fields["repos"] != 1 {
		t.Errorf("the family row says %v, want one repository of one failed", family.Fields)
	}
	if family.Tags["repo"] != "(none)" {
		t.Errorf("the family row names repository %q, want the sentinel", family.Tags["repo"])
	}
}

// TestEveryFamilyThatRanSaysSoWhetherOrNotItFailed: the rows that always
// arrive are what make the absence of a failure row mean something. Without
// them a family that ran perfectly and a family that never ran are the same
// empty panel, and the measurement itself would not exist on an account where
// nothing has ever failed, which InfluxDB answers with a refusal rather than
// with no rows.
func TestEveryFamilyThatRanSaysSoWhetherOrNotItFailed(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, _, log := fakeRunner(t, store)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}

	rows := store.rows("gh_collector_family")
	if len(rows) == 0 {
		t.Fatal("a sweep where nothing failed wrote nothing about itself")
	}
	for _, p := range rows {
		if p.Tags["scope"] != "family" {
			t.Errorf("nothing failed and %v was written", p.Tags)
		}
		if p.Tags["reason"] != "(none)" {
			t.Errorf("%s failed with %q in a sweep where nothing failed", p.Tags["family"], p.Tags["reason"])
		}
	}
	actions := store.one(t, "gh_collector_family", map[string]string{"family": "actions"})
	if actions.Fields["failed"] != 0 || actions.Fields["repos"] != 1 {
		t.Errorf("the actions row says %v, want one repository and no failure", actions.Fields)
	}
	if points, _ := actions.Fields["points"].(int); points == 0 {
		t.Error("the actions row says it collected nothing, and it collected runs and jobs")
	}
}

// TestAFamilySkippedAsNotDueWritesNoRowAtAll: absence is the only thing that
// can say "this family did not run in this sweep", so a family that was not
// due must not write a row saying it collected nothing.
func TestAFamilySkippedAsNotDueWritesNoRowAtAll(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, _, log := fakeRunner(t, store)
	r.Cfg.Every = everyOnly("traffic")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	for _, p := range store.rows("gh_collector_family") {
		if p.Tags["family"] != "traffic" {
			t.Errorf("%s is switched off and the sweep reported on it anyway", p.Tags["family"])
		}
	}
}

// TestAnAccountFamilyIsMarkedWhenItDeliveredSomeOfIt pins the rule that
// replaced the one the collectors used to carry. Several account-wide families
// read every repository in batches, and a batch lost to a 502 must not cost
// the whole family's pass on every sweep until it comes back; a family that
// delivered nothing at all has not run and is not marked, so the next sweep
// asks again rather than waiting out a twelve hour cadence.
func TestAnAccountFamilyIsMarkedWhenItDeliveredSomeOfIt(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, _, log := fakeRunner(t, store)
	now := time.Now()
	boom := errors.New("graphql: INTERNAL: boom")

	r.family(t.Context(), "totals", now, func() ([]sink.Point, error) {
		return []sink.Point{{
			Measurement: "gh_account_total",
			Tags:        map[string]string{"user": "octocat"},
			Fields:      map[string]any{"commits": 1},
			Time:        now,
		}}, boom
	})
	if _, marked := r.State.LastRun["totals"]; !marked {
		t.Error("a family that delivered most of its rows was not marked, so the next sweep pays for it again")
	}
	if n := len(store.rows("gh_account_total")); n != 1 {
		t.Errorf("%d rows reached the sink, want the one the family did collect", n)
	}
	if !strings.Contains(log.String(), "collector failed") {
		t.Errorf("the failure was not reported at all:\n%s", log)
	}

	r.family(t.Context(), "profile", now, func() ([]sink.Point, error) { return nil, boom })
	if when, marked := r.State.LastRun["profile"]; marked {
		t.Errorf("a family that delivered nothing was marked as run at %s", when)
	}
}

// TestABatchThatLostOneChunkDoesNotCostTheFamilyItsPass is the cost the
// branch's own change nearly introduced. The batched collectors report a chunk
// they lost instead of swallowing it, and collectFamily used to read any batch
// error as every repository having failed, which repoFamilies reads as "this
// family has not run": a daily family re-running every fifteen minutes for as
// long as one repository stayed broken. A batch that still delivered rows
// counts as one thing that failed.
func TestABatchThatLostOneChunkDoesNotCostTheFamilyItsPass(t *testing.T) {
	t.Parallel()
	// One repository of seven answers the gateway's error and the other six
	// answer, one alias per query.
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		// Without the type the gateway's answer reads as an HTML body, which
		// the client calls a query too large and halves rather than a chunk
		// that failed.
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `name: \"broken\"`) {
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"r0":{"nameWithOwner":"o/fine","f0":{"byteSize":12,"text":"version: 2"}},` +
			`"r1":{"nameWithOwner":"o/fine","f0":{"byteSize":12,"text":"version: 2"}}}}`))
	})
	r.Cfg.Every = everyOnly("policyfiles")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.repos = nil
	for i := range 7 {
		name := "fine" + strconv.Itoa(i)
		if i == 3 {
			name = "broken"
		}
		r.repos = append(r.repos, collect.Repo{Owner: "o", Name: name, FullName: "o/" + name})
	}

	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, marked := r.State.LastRun["policyfiles"]; !marked {
		t.Error("one repository of seven lost its chunk and the whole family was left due again, " +
			"which turns a daily family into a quarter-hourly one")
	}
	row := r.health.runs["policyfiles"]
	if row == nil {
		t.Fatal("the family wrote no row about itself")
	}
	if row.Failed != 1 {
		t.Errorf("the family row reports %d repositories failed, want the one thing that did: "+
			"a batched query names no repository when it fails", row.Failed)
	}
	if row.Err == nil {
		t.Error("the batch failed and the family row carries no reason")
	}
}

// TestABatchThatBroughtBackNothingStillLeavesTheFamilyDue is the other side of
// the same rule, and the one the count was protecting: a family whose batch is
// the whole family and which collected nothing has not run, so marking it
// would hide the outage until its next cadence, a whole day for two of them.
func TestABatchThatBroughtBackNothingStillLeavesTheFamilyDue(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
	})
	r.Cfg.Every = everyOnly("policyfiles")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if err := r.repoFamilies(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if when, marked := r.State.LastRun["policyfiles"]; marked {
		t.Errorf("a family whose only query failed was marked as run at %s", when)
	}
}

// TestASweepCutShortStillSaysWhatItDid: the rule the panel is read by is that
// a family with no row did not run. A sweep that ran its account families and
// was then stopped by a discovery that failed used to write no row at all,
// which made that sentence false about every family that had run.
func TestASweepCutShortStillSaysWhatItDid(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, fake, _ := fakeRunner(t, store)
	// The repository listing is the first thing a sweep asks for and the one
	// thing it cannot go on without.
	fake.Fail("/user/repos", http.StatusBadGateway)

	if err := r.Once(t.Context()); err == nil {
		t.Fatal("the repository listing answered 502 and the sweep reported success")
	}
	// The listing is the one collector every family depends on, so it reports
	// itself on the same row: the reason is in the store rather than in a
	// journal, and the sixteen families that never ran are absent because
	// they never ran.
	row := store.one(t, "gh_collector_family", map[string]string{"family": "discover"})
	if row.Fields["failed"] != 1 || row.Tags["reason"] != "502" {
		t.Errorf("the discovery row is %v / %v, want one failure and the status", row.Tags, row.Fields)
	}
	for _, p := range store.rows("gh_collector_family") {
		if p.Tags["scope"] != "family" {
			t.Errorf("a sweep that never reached a repository wrote %v", p.Tags)
		}
	}
}

// TestAListingThatWorkedWritesNoRowOfItsOwn is the other side of the same
// rule: a sweep where the listing succeeded has said so by every other row it
// wrote, since no family could have run without it, so a heartbeat here would
// be one row every fifteen minutes repeating them.
func TestAListingThatWorkedWritesNoRowOfItsOwn(t *testing.T) {
	t.Parallel()
	store := &kept{}
	r, _, log := fakeRunner(t, store)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	for _, p := range store.rows("gh_collector_family") {
		if p.Tags["family"] == "discover" {
			t.Errorf("the listing worked and still wrote %v", p.Fields)
		}
	}
}

// honorsContext is a sink that refuses a write under a canceled context, the
// way a network sink does. The one in this package ignores it, which is why
// nothing here could see that a shutdown took the self report with it.
type honorsContext struct {
	kept
}

func (h *honorsContext) Name() string { return "honors-context" }

func (h *honorsContext) Write(ctx context.Context, points []sink.Point) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return h.kept.Write(ctx, points)
}

// TestAShutdownMidSweepStillSaysWhatRanBeforeIt: a shutdown cancels the
// sweep's context, and the row that says what the sweep managed to do is the
// one write that must survive it. Measured before the detachment: ten
// measurements delivered by the families that ran, and no self report, which
// makes the panel's rule say something false about all ten.
func TestAShutdownMidSweepStillSaysWhatRanBeforeIt(t *testing.T) {
	t.Parallel()
	store := &honorsContext{}
	r, _, _ := fakeRunner(t, store)

	// Canceled once the account families have run and before the sweep is
	// done, which is what a restart landing mid-sweep looks like.
	ctx, cancel := context.WithCancel(t.Context())
	r.Cfg.Every = everyOnly("account", "traffic")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Sinks = []sink.Sink{cancelOnFirstFamily{store: store, cancel: cancel}}

	if err := r.Once(ctx); err == nil {
		t.Fatal("the sweep was canceled and Once reported success")
	}
	if n := len(store.rows("gh_collector_family")); n == 0 {
		t.Errorf("a shutdown mid-sweep delivered %d measurements and no self report",
			len(store.points))
	}
}

// cancelOnFirstFamily delivers to the store and cancels the sweep as soon as
// one family has been written, so the cancellation lands in the middle of the
// sweep rather than before it or after everything.
type cancelOnFirstFamily struct {
	store  *honorsContext
	cancel context.CancelFunc
}

func (c cancelOnFirstFamily) Name() string { return "cancel-on-first-family" }
func (c cancelOnFirstFamily) Close() error { return nil }

func (c cancelOnFirstFamily) Write(ctx context.Context, points []sink.Point) (int, error) {
	n, err := c.store.Write(ctx, points)
	if c.store.measured("gh_account") > 0 {
		c.cancel()
	}
	return n, err
}

// TestAChunkWithNothingToSayStillCountsAsAnAnswer is the corner the first fix
// left: reading rows as the test for "did the batch answer" reads a collector
// that legitimately writes nothing as a collector that failed. deployments is
// the live example, hourly, on an account that has never deployed, so its
// batch produces no rows on any sweep; one chunk failing would then have left
// it due on every tick, which is the cost the rule above exists to prevent.
func TestAChunkWithNothingToSayStillCountsAsAnAnswer(t *testing.T) {
	t.Parallel()
	// Every chunk that answers answers with an empty page, which is what a
	// repository with no deployment looks like, so the family collects
	// nothing at all and one repository still fails.
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `name: \"broken\"`) {
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"r0":{"deployments":{"nodes":[],` +
			`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
	})
	r.Cfg.Every = everyOnly("deployments")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.repos = nil
	for i := range 7 {
		name := "fine" + strconv.Itoa(i)
		if i == 3 {
			name = "broken"
		}
		r.repos = append(r.repos, collect.Repo{Owner: "o", Name: name, FullName: "o/" + name})
	}

	pass, err := r.collectFamily(t.Context(), "deployments", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pass.written != 0 {
		t.Fatalf("the fake answered every chunk with an empty page and %d points came back, "+
			"so this is no longer the shape it was written for", pass.written)
	}
	if pass.failed != 1 {
		t.Errorf("failed = %d, want the one thing that failed: six repositories were asked "+
			"and answered, they simply had nothing to report", pass.failed)
	}
}
