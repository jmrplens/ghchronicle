package run

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// events reads the activity feed down to the page that carries the newest
// event the previous sweep saw, and remembers this sweep's newest for the
// next. A backfill reads the whole feed, which is three pages at most.
func (r *Runner) events(ctx context.Context, login string, now time.Time) ([]sink.Point, error) {
	feed := &collect.Events{Login: login}
	if !r.Backfill {
		feed.After = r.State.LastEvent
	}
	points, err := feed.Collect(ctx, r.API, now)
	if err == nil && feed.Newest != "" {
		r.State.LastEvent = feed.Newest
	}
	return points, err
}

// fullInboxEvery is how often the inbox is read whole, read threads included,
// rather than unread ones from the last thread that moved.
//
// The whole read does two things a windowed sweep cannot. It bounds what the
// `since` window can miss: GitHub applies the filter, on updated_at, and a
// thread it leaves out stays out of every windowed sweep until a read without
// the window lists it again. And it is what closes a thread: a sweep asks for
// unread threads only, so a thread read without a reply leaves that listing
// rather than coming back with the tag changed, and only a read with all=true
// lists it again, with unread false and the same updated_at. Once a day bounds
// both to a day, at twenty pages a day instead of twenty every thirty minutes.
// Without all=true the daily read would rewrite the same unread rows and
// nothing would ever say a thread was read, which is half of the metric.
const fullInboxEvery = 24 * time.Hour

// notifications reads the unread threads from the newest updated_at the
// previous sweep saw, minus two cadences so a late sweep still overlaps the
// one before, and the whole inbox, read threads included, once a day, on a
// backfill, and when the state has no window to cut from, which is what a
// first sweep and an older state file both look like.
func (r *Runner) notifications(ctx context.Context, now time.Time) ([]sink.Point, error) {
	full := r.Backfill || r.State.LastNotified.IsZero() || r.State.FullDue("notifs", fullInboxEvery-r.slack(), now)
	inbox := &collect.Notifications{All: full, Walk: r.walk()}
	if !full {
		every, _ := r.Cfg.Interval("notifs")
		inbox.Since = r.State.LastNotified.Add(-2 * every)
	}
	points, err := inbox.Collect(ctx, r.API, now)
	if err != nil {
		return points, err
	}
	if inbox.Newest.After(r.State.LastNotified) {
		r.State.LastNotified = inbox.Newest
	}
	if full {
		r.State.MarkFull("notifs", now)
	}
	return points, nil
}

// issueEvents is the issue event collector for this run: the timeline of
// the items updated in twice the cadence on a sweep, so a late sweep still
// overlaps the one before, and further back when the family last ran earlier
// than that, the way pulls does, so the hours a stopped process missed are
// read by its first sweep rather than left out of the series; thirty days on
// the first sweep, which is the collector's own default, so a fresh install
// does not chart transitions that start an hour ago; and the per-issue walk
// back to BackfillSince on a backfill.
func (r *Runner) issueEvents(now time.Time) collect.IssueEvents {
	if r.Backfill {
		return collect.IssueEvents{Walk: r.walk()}
	}
	last, ran := r.State.LastRun["issueevents"]
	if !ran {
		return collect.IssueEvents{}
	}
	every, _ := r.Cfg.Interval("issueevents")
	since := now.Add(-2 * every)
	if last.Before(since) {
		since = last.Add(-every)
	}
	return collect.IssueEvents{Since: since}
}
