package run

import (
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// slowCadence is where a family stops starting on every sweep it is due and
// takes turns with the other families as slow as it.
//
// State.Mark records the sweep's own clock for every family that ran in it, so
// families that once ran in the same sweep hold the same last_run to the
// nanosecond and are due together again at every cadence, for ever, and the
// state file keeps them together across restarts. In production on 2026-09-26
// the eight daily families ran in the first sweep of the UTC day, as they did
// every day, and the five twelve hour ones together 45 minutes later: the
// first took 294 billable core requests and 24.3 MB against 17.5 requests for
// the median sweep, and the two made its hour eight times the median hour in
// core requests. Nothing about the work put them there; the calendar did.
//
// Six hours because a tick is a small part of a cadence from there up, a
// twenty-fourth of six hours at the quarter hour tick, and a large one below
// it: a quarter of an hourly cadence. The families under it run several times
// an hour between them and are the steady load of every hour, not its peak.
const slowCadence = 6 * time.Hour

// takeTurns decides which of the slow families due this sweep start in it,
// and holds the others back in r.held. A family held back is not marked, so it
// is still due on the next tick, and once two families have run in different
// sweeps their last_run differ and they stay apart without this doing
// anything, restarts included, a primed one too: see markRun. So it only acts
// when a group forms: a fresh install, the service started after a -once or a
// backfill that marked every family at one instant, a family newly switched
// on, or two cadences that meet.
//
// Only in the loop, and only in a sweep that follows the cadences at all. A
// primed sweep, a sweep that draws a card and a backfill run every family by
// definition. -once has no next tick to leave a family for: under a scheduler
// that runs it once a day, the family held back would wait a day.
//
// The families that start are the ones overdue the longest, counted from the
// later of when each last ran and when this process last let it start. The
// second half is what stops a family that keeps failing from taking every
// turn: a failed pass is not marked as run, so by last_run alone it would be
// the most overdue family on every sweep and nothing behind it would start.
// With the order counted this way a family that is due waits for each of the
// others at most once, because a family let start is then due a whole cadence
// later, past every family already waiting. With n slow families and one start
// a sweep the worst wait is therefore n-1 ticks, and it happens only to the
// last of n families due in the same sweep. The built-in table has twelve
// families of six hours or more, eleven ticks at the quarter hour tick they
// give, 2h45m; the production account also names deps and history, fourteen
// and 3h15m; and all three families that ship switched off named at a day or
// more make fifteen, 3h30m.
func (r *Runner) takeTurns(now time.Time) {
	r.held = nil
	if !r.serving || r.prime || r.Backfill {
		return
	}
	var slow, due []string
	var shortest time.Duration
	for _, name := range config.Families() {
		every, enabled := r.Cfg.Interval(name)
		if !enabled || every < slowCadence || !r.runsHere(name) {
			continue
		}
		slow = append(slow, name)
		if shortest == 0 || every < shortest {
			shortest = every
		}
		if r.due(name, every, now) {
			due = append(due, name)
		}
	}
	tick, _ := r.tick()
	slices.SortStableFunc(due, func(a, b string) int { return r.overdueSince(a).Compare(r.overdueSince(b)) })
	turns := min(turnsPerSweep(len(slow), shortest, tick), len(due))
	start, wait := due[:turns], due[turns:]
	if r.started == nil {
		r.started = map[string]time.Time{}
	}
	for _, name := range start {
		r.started[name] = now
	}
	if len(wait) == 0 {
		return
	}
	r.held = make(map[string]bool, len(wait))
	for _, name := range wait {
		r.held[name] = true
	}
	r.Log.Info("slow families due together take turns",
		"starting", strings.Join(start, ","), "waiting", strings.Join(wait, ","))
}

// turnsPerSweep is how many slow families one sweep may start: one, unless one
// a sweep could not start each of them within the shortest of their cadences,
// and then the fewest that can.
//
// The built-in cadences never reach the second case, and a configuration can.
// Its tick is its shortest cadence, held under an hour, so every.default: 6h
// ticks hourly with thirty-one families at six hours, and one a sweep would
// start each of them every thirty-one hours. The count that can is what keeps
// the wait above under one of those cadences: with k starts a sweep a family
// waits at most (n-1)/k ticks, and k = ceil(n / ticks in the shortest cadence)
// holds that under the cadence. A tick longer than the cadence itself leaves
// nothing to spread, and every family due starts.
func turnsPerSweep(slow int, shortest, tick time.Duration) int {
	if tick <= 0 {
		return slow
	}
	ticks := int(shortest / tick)
	if ticks < 1 {
		return slow
	}
	return max(1, (slow+ticks-1)/ticks)
}

// overdueSince is when a slow family came due, counted from the later of its
// last run and the last time this process let it start.
func (r *Runner) overdueSince(name string) time.Time {
	every, _ := r.Cfg.Interval(name)
	last := r.State.LastRun[name]
	if started := r.started[name]; started.After(last) {
		last = started
	}
	return last.Add(every)
}

// runsHere reports whether a family would run at all in this configuration.
// An account-wide family needs a login to ask about, and one that cannot run
// must not take a turn from one that can.
func (r *Runner) runsHere(name string) bool {
	return r.Cfg.Targets.User != "" || slices.Contains(perRepoFamilies, name)
}
