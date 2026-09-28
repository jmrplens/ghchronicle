package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// Applier brings one store along for one migration: whatever that store's
// way of putting the old rows out of the new ones' way is, from a set-aside
// the store keeps for a day to a command printed for whoever runs it.
type Applier interface {
	// Apply brings the item's store along, and says what it did. An error
	// leaves the item pending, with nothing recorded.
	Apply(ctx context.Context, it Item) (Outcome, error)
}

// Outcome is what an Applier did.
type Outcome struct {
	// Did is what was done, one sentence for the log and the report.
	Did string
	// Aside is the name the old rows are kept under, empty where nothing is
	// kept.
	Aside string
	// Commands is what somebody else still has to run where ghchronicle
	// cannot.
	Commands []string
	// Kept is the record of the copy the old rows are kept in, for the state
	// file, which is what purges it once it has been kept its day. Nil
	// where nothing is kept.
	Kept *run.Aside
}

// Instructions is the Applier of a store ghchronicle cannot change itself,
// a Graphite, whose files are on its own host, and whatever is behind a
// Telegraf: it says what the plan says to do and counts that as applied,
// since whether it was run is something only whoever runs it knows. The plan
// marks every such store as needing somebody's word, so it is only ever
// applied by -migrate -yes, which is that word.
type Instructions struct{}

// Apply says it.
func (Instructions) Apply(_ context.Context, it Item) (Outcome, error) {
	return Outcome{Did: it.Action, Commands: it.Commands}, nil
}

// Chosen is one pending item picked to be applied, in its store.
type Chosen struct {
	Store string
	// Destination is where the store's sink points, which the record kept
	// for it has to match.
	Destination string
	Item        Item
}

// Refiller reads the history of every cleared item again from GitHub and
// writes it to the stores that were cleared, and to no other.
type Refiller func(ctx context.Context, cleared []Chosen) error

// Result is how one chosen item went.
type Result struct {
	Chosen
	Outcome Outcome
	Err     error
}

// Applying is what applying needs besides the items: where to record each
// one, the way each store is brought along, and the refill.
type Applying struct {
	State *run.State
	// Save writes State. Called after every item, so a stop between the
	// set-aside and the refill leaves the set-aside recorded.
	Save     func() error
	Appliers map[string]Applier
	// Refill reads the cleared items again. Nil is a build that cannot,
	// which leaves every cleared item owing its history and says so.
	Refill Refiller
	// Cleared, when set, is told of every item applied, once it is recorded
	// and before the refill, so that what this process remembers of the
	// measurement in that store is forgotten and the refill writes every row
	// again: the write ledger's entries, and the cache file's claims about
	// the families that write it.
	Cleared func(c Chosen)
	Now     func() time.Time
	Log     *slog.Logger
	// Resume is the sentence a failure ends with: how to try again.
	Resume string
}

// ErrNoRefill is a build that cleared a store and has no way to read what
// it held back from GitHub.
var ErrNoRefill = errors.New("this build cannot read the history again: run a backfill of the families named, " +
	"writing to the stores cleared")

// Apply brings every chosen item along, store by store, in the order given,
// recording each one as it goes, and then reads again the history of every
// one that cleared a store. An item that fails is reported and the rest go
// on, the way -uninstall goes on: stopping at the first would leave the rest
// unapplied for no reason of their own.
func (a Applying) Apply(ctx context.Context, chosen []Chosen) ([]Result, error) {
	results := make([]Result, 0, len(chosen))
	var cleared []Chosen
	for _, c := range chosen {
		r := Result{Chosen: c}
		r.Outcome, r.Err = a.one(ctx, c)
		results = append(results, r)
		if r.Err == nil && len(c.Item.Refill) > 0 {
			cleared = append(cleared, c)
		}
	}
	if len(cleared) == 0 {
		return results, nil
	}
	if a.Refill == nil {
		return results, ErrNoRefill
	}
	return results, a.Refill(ctx, cleared)
}

// one applies one item and records it.
func (a Applying) one(ctx context.Context, c Chosen) (Outcome, error) {
	m := c.Item.Migration
	failed := func(err error) (Outcome, error) {
		a.Log.Error("migration failed", "sink", c.Store, "measurement", m.Measurement, "migration", m.ID,
			"err", err, "resume", a.Resume)
		return Outcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}
	applier := a.Appliers[c.Store]
	if applier == nil {
		return failed(fmt.Errorf("this build has no way to bring %s along", c.Store))
	}
	out, err := applier.Apply(ctx, c.Item)
	if err != nil {
		return failed(err)
	}
	rec := a.State.Stores[c.Store]
	if rec == nil || rec.Destination != c.Destination {
		rec = &run.StoreRecord{Destination: c.Destination}
		a.State.Stores[c.Store] = rec
	}
	rec.MarkApplied(m.ID, a.now())
	if out.Kept != nil {
		rec.KeepAside(*out.Kept)
	}
	if a.Cleared != nil {
		a.Cleared(c)
	}
	if err = a.Save(); err != nil {
		// Applied and not recorded: a store that can be asked shows it
		// cleared at the next start, a store that cannot is asked to be
		// cleared again, and a refill cut short is not known to be owed.
		a.Log.Error("migration applied and not recorded: the state file was not saved", "sink", c.Store,
			"migration", m.ID, "err", err)
	}
	a.Log.Info("migration applied", "sink", c.Store, "measurement", m.Measurement, "migration", m.ID,
		"did", out.Did, "aside", out.Aside)
	return out, nil
}

// now is the clock records are dated by.
func (a Applying) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Pending is every pending item of the plan in the order it prints them,
// split into what -migrate -yes applies and what it holds back: an item in a
// store that holds rows of accounts this configuration does not collect, or
// that could not say whose rows it holds, waits for -migrate-others, because
// set aside those rows come back only when whoever collects them reads them
// again.
func (p Plan) Pending(others bool) (apply, held []Chosen) {
	for _, st := range p.Stores {
		for _, it := range st.Items {
			if it.Status != Pending {
				continue
			}
			c := Chosen{Store: st.Name, Destination: st.Destination, Item: it}
			if !others && it.shared() {
				held = append(held, c)
				continue
			}
			apply = append(apply, c)
		}
	}
	return apply, held
}

// shared says whether the item's store may hold rows of somebody else's: it
// does, or it could be asked and was not.
func (it Item) shared() bool {
	return len(it.Others) > 0 || it.Asked && !it.AccountsChecked
}

// Unreached is every store, and every item, the plan could not ask, one
// sentence each.
func (p Plan) Unreached() []string {
	var out []string
	for _, st := range p.Stores {
		if st.Err != nil {
			out = append(out, st.Name+" did not answer: "+oneLine(st.Err.Error()))
			continue
		}
		for _, it := range st.Items {
			if it.Status == Unreachable {
				out = append(out, fmt.Sprintf("%s did not answer about %s: %s", st.Name, it.Migration.ID,
					oneLine(it.Evidence)))
			}
		}
	}
	return out
}

// HeldBack is why -migrate -yes left an item for -migrate-others.
func (c Chosen) HeldBack() string {
	if len(c.Item.Others) > 0 {
		return fmt.Sprintf("it holds rows of %s, which this configuration does not collect", quoted(c.Item.Others))
	}
	return "whose rows it holds could not be read"
}

// joinReasons is a list of reasons as one attribute.
func joinReasons(why []string) string { return strings.Join(why, "; ") }
