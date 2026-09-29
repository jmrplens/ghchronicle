package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// Start is what a run that writes to the stores does about the registry
// before its first sweep: every item is said at the level it deserves, what
// a later start need not ask again is recorded, and what this start may
// apply on its own is handed back.
//
// The rule for applying on its own is the maintainer's, and all three parts
// of it have to hold: GitHub serves the whole history, so reading it again
// restores every row; the store holds only this configuration's rows; and
// the old rows are set aside for 24 hours rather than destroyed. The planner
// decides those three per item (Item.Safe). Anything else is a warning at
// every start naming the commands that apply it, and there is no setting
// that applies it without them.
type Start struct {
	Plan Plan
	// Auto is migrate: auto. Without it nothing is applied, only said.
	Auto bool
	// Hold is why this start applies nothing on its own although Auto
	// says it may, empty when it may.
	Hold string
	// CanApply says whether this build has a way to bring a store along.
	CanApply func(store string) bool
	// DryRun and Apply are the command lines a warning names, and Others
	// the flag Apply takes for a store holding somebody else's rows.
	DryRun, Apply, Others string
	// Service says this run is the long-running one, which -migrate -yes
	// refuses to run beside, so a warning says to stop it first.
	Service bool
	State   *run.State
	Now     time.Time
	Log     *slog.Logger
}

// Decide says every item and returns the ones to apply now.
func (s Start) Decide(ctx context.Context) []Chosen {
	var apply []Chosen
	for _, st := range s.Plan.Stores {
		if st.Err != nil {
			s.Log.Warn("migration check failed: the store did not answer", "sink", st.Name,
				"err", oneLine(st.Err.Error()), "next", "asked again at the next start")
			continue
		}
		for _, it := range st.Items {
			c := Chosen{Store: st.Name, Destination: st.Destination, Item: it}
			if s.decideOne(ctx, c) {
				apply = append(apply, c)
			}
		}
	}
	return apply
}

// decideOne says one item, and reports whether this start applies it.
func (s Start) decideOne(ctx context.Context, c Chosen) bool {
	it, m := c.Item, c.Item.Migration
	switch it.Status {
	case NotNeeded:
		if !it.Recorded {
			s.record(c).MarkNotNeeded(m.ID, s.Now)
		}
		s.Log.Debug("migration not needed", "sink", c.Store, "migration", m.ID, "found", it.Evidence)
	case Applied:
		s.Log.Debug("migration applied before", "sink", c.Store, "migration", m.ID, "found", it.Evidence)
	case Noted, Frozen:
		s.note(ctx, c)
	case Unreachable:
		s.Log.Warn("migration check failed: the store did not answer", "sink", c.Store, "migration", m.ID,
			"err", oneLine(it.Evidence), "next", "asked again at the next start")
	case Pending:
		if why := s.notOnItsOwn(c); len(why) > 0 {
			s.Pending(c, why)
			return false
		}
		s.Log.Warn("applying a migration before the first sweep", "sink", c.Store,
			"measurement", m.Measurement, "migration", m.ID, "why", m.Why, "found", it.Evidence,
			"action", it.Action, "refill", refillOf(it))
		return true
	}
	return false
}

// note says, once at Info and then at Debug, an item nothing can apply.
func (s Start) note(ctx context.Context, c Chosen) {
	it, m := c.Item, c.Item.Migration
	msg := "migration noted: nothing is changed"
	if it.Status == Frozen {
		msg = "migration frozen: nothing this configuration runs writes the measurement"
	}
	rec := s.record(c)
	level := slog.LevelDebug
	if _, said := rec.Noted[m.ID]; !said {
		level = slog.LevelInfo
		rec.MarkNoted(m.ID, s.Now)
	}
	s.Log.Log(ctx, level, msg, "sink", c.Store, "measurement", m.Measurement,
		"migration", m.ID, "why", m.Why, "found", it.Evidence, "action", it.Action)
}

// notOnItsOwn is every reason this start leaves a pending item alone.
func (s Start) notOnItsOwn(c Chosen) []string {
	var why []string
	if !s.Auto {
		why = append(why, "migrate: warn applies nothing on its own")
	} else if s.Hold != "" {
		why = append(why, s.Hold)
	}
	if !c.Item.Safe {
		why = append(why, c.Item.Unsafe...)
	}
	if s.Auto && s.Hold == "" && c.Item.Safe && s.CanApply != nil && !s.CanApply(c.Store) {
		why = append(why, "this build has no way to bring "+c.Store+" along on its own")
	}
	return why
}

// Pending says an item this start does not apply, with why and with the
// exact commands that do. Every start says it again: a store holding two
// shapes is wrong for as long as it does, and a line said once is a line
// scrolled past.
func (s Start) Pending(c Chosen, why []string) {
	it, m := c.Item, c.Item.Migration
	apply := s.Apply
	if it.shared() {
		apply += " " + s.Others
	}
	args := []any{
		"sink", c.Store, "measurement", m.Measurement, "migration", m.ID, "why", m.Why,
		"found", it.Evidence, "not_applied", joinReasons(why), "plan", s.DryRun, "apply", apply,
	}
	if s.Service {
		args = append(args, "first", "stop this service: -migrate -yes refuses to run beside it")
	}
	s.Log.Warn("migration pending", args...)
}

// Held says the items a start meant to apply and could not, because the
// state file was not its to change.
func (s Start) Held(chosen []Chosen, reason string) {
	for _, c := range chosen {
		s.Pending(c, []string{reason})
	}
}

// record is the state file's record of an item's store, made when the
// start did not stamp one, which only a test does.
func (s Start) record(c Chosen) *run.StoreRecord {
	rec := s.State.Stores[c.Store]
	if rec == nil || rec.Destination != c.Destination {
		rec = &run.StoreRecord{Destination: c.Destination}
		s.State.Stores[c.Store] = rec
	}
	return rec
}

// refillOf is what reading an item again covers, as one attribute.
func refillOf(it Item) string {
	if len(it.Refill) == 0 {
		return "nothing"
	}
	return fmt.Sprintf("%s, %s, writing %s only", strings.Join(it.Refill, ","), bound(it.Since),
		it.Migration.Measurement)
}
