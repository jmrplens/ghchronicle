package run

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
)

// What this sweep can say about itself.
//
// The runner already knew all of it and told nobody but the journal: which
// families ran, over how many repositories, which repositories each could not
// collect and why. A reader of the dashboard saw an empty Continuous
// integration row and had no way to tell a quiet account from five
// repositories lost to one 502. So the sweep writes it down, as
// gh_collector_family, and the collector's own row of every generated
// dashboard draws it.
//
// Per sweep, and reset at the start of each one: every row is this sweep's
// answer, and a failure that has stopped happening stops being written rather
// than standing until something clears it.

// health accumulates one sweep's outcomes, in the order the families ran, so
// the rows come out in that order too.
type health struct {
	order []string
	runs  map[string]*collect.FamilyRun
}

// beginHealth starts a fresh record. Called once per sweep.
func (r *Runner) beginHealth() {
	r.health = health{}
}

// familyRun is this sweep's record for one family, created on first mention.
//
// The map is built here rather than only in beginHealth: a caller that drives
// one family directly, which every test of a family does, never begins a
// sweep, and a record that panicked in that case would be a measurement that
// only works one way round.
func (h *health) familyRun(family string) *collect.FamilyRun {
	if h.runs == nil {
		h.runs = map[string]*collect.FamilyRun{}
	}
	if run, seen := h.runs[family]; seen {
		return run
	}
	run := &collect.FamilyRun{Family: family}
	h.runs[family] = run
	h.order = append(h.order, family)
	return run
}

// noteFamily records what one family's pass came to: how many repositories it
// was asked about and how many rows it produced.
//
// A family that was skipped as not due, or for want of budget, is never noted,
// and that is the point: its absence from this sweep's rows is what says it
// did not run, which an empty panel never could.
func (r *Runner) noteFamily(family string, repos, failed, points int) {
	run := r.health.familyRun(family)
	run.Repos, run.Failed, run.Points = repos, failed, points
}

// noteFamilyFailure records a failure that belongs to the family rather than
// to any one repository: an account-wide family's own error, or the batched
// query some per-repository families begin with, which asks about fourteen
// repositories at once and names none of them when it fails.
func (r *Runner) noteFamilyFailure(family string, err error) {
	r.health.familyRun(family).Err = err
}

// noteRepoFailure records one repository one family could not collect.
func (r *Runner) noteRepoFailure(family string, repo collect.Repo, err error) {
	run := r.health.familyRun(family)
	run.Failures = append(run.Failures, collect.RepoFailure{Repo: repo, Err: err})
}

// emitHealth sends what the sweep learned about itself.
//
// Last, after every family, because it is the summary of them; and through the
// same path as everything else, so a sink that filters or counts sees these
// rows the way it sees the rest. It costs no request at all: every number in
// it was already in hand.
func (r *Runner) emitHealth(ctx context.Context, now time.Time) {
	runs := make([]collect.FamilyRun, 0, len(r.health.order))
	for _, family := range r.health.order {
		runs = append(runs, *r.health.runs[family])
	}
	r.emit(ctx, "collector", collect.CollectorPoints(runs, now))
}
