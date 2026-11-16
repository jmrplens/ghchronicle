package run

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// reported runs the reported family and keeps what it found in the state
// file.
//
// What is left out is what the advisories family reads as received: every
// repository of the account and of the organizations configured, and every
// repository this run collects, named ones of other owners included.
//
// A sweep reads the inbox and the global database since the last complete
// read of each. With no read on record, which is a first sweep and every run
// of a state file nobody keeps (the Action's, a card's), it reads the last
// reportedWindow of both rather than all of them: 98 inbox pages and 379 of
// the database, 6.5 minutes, measured on 2026-11-16, paid on every such run
// for repositories the search and the contributions mostly name anyway. A
// backfill reads the whole inbox and the database back to its own bound,
// since a backfill is asked to find what the sweeps missed.
func (r *Runner) reported(ctx context.Context, now time.Time) ([]sink.Point, error) {
	user := r.Cfg.Targets.User
	mem := &r.State.Reported
	collected := make([]string, 0, len(r.repos))
	for _, repo := range r.repos {
		collected = append(collected, repo.FullName)
	}
	rep := collect.Reported{
		Login:     user,
		Own:       append([]string{user}, r.Cfg.Targets.Orgs...),
		Collected: collected,
		Known:     mem.Known(),
		Walk:      r.walk(),
		Details:   r.Cfg.Advisories.UnpublishedDetails,
		Refusals:  r.refusalsFor("reported"),
	}
	switch {
	case r.Backfill:
		rep.Scanned = r.BackfillSince
	default:
		rep.Notified, rep.Scanned = mem.Notified, mem.Scanned
		if rep.Notified.IsZero() {
			rep.Notified = now.Add(-reportedWindow)
		}
		if rep.Scanned.IsZero() {
			rep.Scanned = now.Add(-reportedWindow)
		}
	}
	run, err := rep.Collect(ctx, r.API, now)
	mem.Remember(run.Remember, now)
	// Both marks move to a little before this read started, not to the
	// newest item it saw: what changed while the read was under way is read
	// again next time rather than skipped, and the margin covers a clock
	// here that runs ahead of GitHub's. Reading a thread or an advisory twice
	// costs nothing; each only adds a repository to a set.
	if run.InboxRead {
		mem.Notified = now.Add(-reportedMargin)
	}
	if run.Scanned && !r.Backfill {
		mem.Scanned = now.Add(-reportedMargin)
	}
	return run.Points, err
}

// reportedWindow is how far back a sweep with no read on record reads the
// inbox and the global advisory database.
const reportedWindow = 30 * 24 * time.Hour

// reportedMargin is how far before a read's start its mark is set.
const reportedMargin = 10 * time.Minute
