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
// The owners left out are the ones the advisories family reads as received:
// the account and every organization configured. A sweep reads the inbox and
// the global database since the last complete read of each; a backfill reads
// the whole inbox and the global database back to its own bound, since a
// backfill is asked to find what the sweeps missed.
func (r *Runner) reported(ctx context.Context, now time.Time) ([]sink.Point, error) {
	user := r.Cfg.Targets.User
	mem := &r.State.Reported
	rep := collect.Reported{
		Login:    user,
		Own:      append([]string{user}, r.Cfg.Targets.Orgs...),
		Known:    mem.Known(),
		Walk:     r.walk(),
		Details:  r.Cfg.Advisories.UnpublishedDetails,
		Refusals: r.refusalsFor("reported"),
	}
	if r.Backfill {
		rep.Scanned = r.BackfillSince
	} else {
		rep.Notified, rep.Scanned = mem.Notified, mem.Scanned
	}
	run, err := rep.Collect(ctx, r.API, now)
	mem.Remember(run.Remember, now)
	// Both marks move to when this read started, not to the newest item it
	// saw: what changed while the read was under way is read again next time
	// rather than skipped.
	if run.InboxRead {
		mem.Notified = now
	}
	if run.Scanned && !r.Backfill {
		mem.Scanned = now
	}
	return run.Points, err
}
