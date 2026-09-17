package run

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

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
