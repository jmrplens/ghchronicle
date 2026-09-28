package migrate

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// clearable is every measurement a migration can clear, which is every one
// whose history GitHub serves whole: the only ones a copy can be of.
func clearable() []string {
	var out []string
	for _, m := range Registry {
		if !m.noteOnly() && !slices.Contains(out, m.Measurement) {
			out = append(out, m.Measurement)
		}
	}
	return out
}

// Purging is what purging the copies a migration set aside takes.
type Purging struct {
	Config *config.Config
	State  *run.State
	// Save writes State, after anything was purged.
	Save func() error
	// Purgers are the stores whose copies ghchronicle purges itself. Nil is
	// the configuration's own.
	Purgers []teardown.Purger
	// Ask says to ask those stores for copies the state file does not name
	// as well. A new state file names none, and every run of the Action
	// starts on one, so without asking a copy set aside there would stay.
	Ask bool
	Now time.Time
	Log *slog.Logger
}

// Owed says whether Run has anything to do: a store to ask, or a copy the
// record names that has been kept its day. A run that owes nothing need not
// take the state file's lock for it.
func (p Purging) Owed() bool {
	if p.Ask && (p.Purgers != nil || len(teardown.Purgers(p.Config)) > 0) {
		return true
	}
	for _, rec := range p.State.Stores {
		for _, a := range rec.SetAside {
			if !p.Now.Before(a.At.Add(teardown.Grace)) {
				return true
			}
		}
	}
	return false
}

// Run purges every copy that has been kept its day, and says when the next
// one falls due, zero when none is waiting. A store is only asked anything
// when a copy it holds has fallen due, or when Ask says to look, so a start or
// a sweep with nothing set aside costs the stores nothing.
//
// Only a copy made the way Clear makes one is ever purged: its name is the
// measurement of a registered migration, a dash and the instant it was made,
// and the instant has to be a day old. A copy InfluxDB 3 keeps is the
// server's to purge, and its record is dropped once it is due.
func (p Purging) Run(ctx context.Context) time.Time {
	purgers := p.Purgers
	if purgers == nil {
		purgers = teardown.Purgers(p.Config)
	}
	byName := map[string]teardown.Purger{}
	for _, s := range purgers {
		byName[s.Name()] = s
	}
	var next time.Time
	changed := false
	for _, st := range storesOf(p.Config) {
		rec := p.State.Stores[st.name]
		if rec != nil && rec.Destination != st.destination {
			// A record of another store: its copies are not in this one.
			rec = nil
		}
		held := p.candidates(ctx, rec, byName[st.name])
		for _, a := range held {
			due := a.At.Add(teardown.Grace)
			if p.Now.Before(due) {
				if next.IsZero() || due.Before(next) {
					next = due
				}
				continue
			}
			if p.purge(ctx, st.name, rec, byName[st.name], a) {
				changed = true
			}
		}
	}
	if changed && p.Save != nil {
		if err := p.Save(); err != nil {
			p.Log.Warn("state not saved", "err", err)
		}
	}
	return next
}

// candidates is every copy one store holds: the ones the record names, and,
// when Ask says so, the ones the store itself lists.
func (p Purging) candidates(ctx context.Context, rec *run.StoreRecord, s teardown.Purger) []run.Aside {
	var out []run.Aside
	if rec != nil {
		out = slices.Clone(rec.SetAside)
	}
	if !p.Ask || s == nil {
		return out
	}
	listed, err := s.Asides(ctx, clearable())
	if err != nil {
		p.Log.Warn("set-aside copies not listed: the store did not answer", "sink", s.Name(), "err", err,
			"next", "asked again at the next start")
		return out
	}
	for _, a := range listed {
		if !slices.ContainsFunc(out, func(r run.Aside) bool { return r.Name == a.Name }) {
			out = append(out, run.Aside{Name: a.Name, Measurement: a.Measurement, At: a.At})
		}
	}
	return out
}

// purge removes one copy that has fallen due, and reports whether the record
// changed.
func (p Purging) purge(ctx context.Context, store string, rec *run.StoreRecord, s teardown.Purger, a run.Aside) bool {
	switch {
	case a.ByServer:
		p.Log.Debug("set-aside copy forgotten: the store purges it itself", "sink", store, "aside", a.Name)
	case s == nil:
		// The record names a copy in a store this build cannot reach any
		// more; -uninstall data, or the store's own tools, remove it.
		return false
	default:
		if err := s.Purge(ctx, a.Name); err != nil {
			p.Log.Warn("set-aside copy not purged", "sink", store, "aside", a.Name, "err", err,
				"next", "tried again after the next sweep or start")
			return false
		}
		p.Log.Info("set-aside copy purged", "sink", store, "aside", a.Name, "set_aside", a.At.Format(time.RFC3339))
	}
	if rec == nil {
		return false
	}
	rec.DropAside(a.Name)
	return true
}
