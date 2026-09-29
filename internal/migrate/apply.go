package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// Applier brings one store along for one migration: whatever that store's
// way of putting the old rows out of the new ones' way is, from a set-aside
// the store keeps for a day to a command printed for whoever runs it.
type Applier interface {
	// Apply brings the item's store along, and says what it did. An error
	// leaves the item pending. One that wraps teardown.ErrUntouched left the
	// store as it was; any other may have changed it, and the refill
	// recorded as owed before the attempt stays owed. An Outcome with Kept
	// beside an error is a copy made that the state file has to name, to
	// purge it in its turn.
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

// Result is how one chosen item went.
type Result struct {
	Chosen
	Outcome Outcome
	Err     error
}

// Outcomes is what Apply did: every item, and the refill that followed, nil
// when no store was owed one.
type Outcomes struct {
	Results []Result
	Refill  *Refilled
}

// Applying is what applying needs besides the items: where to record each
// one, the way each store is brought along, and the refill.
type Applying struct {
	// Config is the configuration whose stores are brought along, which says
	// which records of the state file are about the stores its sinks write.
	Config *config.Config
	State  *run.State
	// Save writes State. Called after every item, so a stop between the
	// set-aside and the refill leaves the set-aside recorded, and the refill
	// owed with it.
	Save     func() error
	Appliers map[string]Applier
	// Refill reads the cleared history again. Nil is a build that cannot,
	// which leaves every store cleared owing its history and says so.
	Refill Refiller
	// Readers are the stores that can say which items a table holds, by the
	// sink's name, for the comparison with the copy of the old rows once the
	// refill ends. A store with none is not compared.
	Readers map[string]teardown.ItemReader
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
// recording each one as it goes, and then pays every refill the state file
// records as owed: the ones these items left, and any an earlier run was
// stopped before it finished. An item that fails is reported and the rest go
// on, the way -uninstall goes on: stopping at the first would leave the rest
// unapplied for no reason of their own. The error is the refill's.
func (a Applying) Apply(ctx context.Context, chosen []Chosen) (Outcomes, error) {
	done := Outcomes{Results: make([]Result, 0, len(chosen))}
	for _, c := range chosen {
		r := Result{Chosen: c}
		r.Outcome, r.Err = a.one(ctx, c)
		done.Results = append(done.Results, r)
	}
	if done.Refill = a.refill(ctx); done.Refill != nil {
		return done, done.Refill.Err
	}
	return done, nil
}

// one applies one item and records it.
//
// The refill is recorded as owed, and saved, before the store is touched. A
// store cleared looks, to anyone who asks it afterwards, like one that never
// held the old shape, so a record written after the clear is a record a kill
// between the two, or an answer lost on the way back from a store that did
// what it was asked, leaves unwritten, and the history is then never read
// again. Owed first, the worst a clear that did not happen costs is a refill
// read for nothing; and a failure the store says changed nothing takes the
// debt back.
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
	rec := a.State.Stores[c.Store]
	if rec == nil || rec.Destination != c.Destination {
		rec = &run.StoreRecord{Destination: c.Destination}
		a.State.Stores[c.Store] = rec
	}
	owedBefore := rec.Refill.Clone()
	if len(c.Item.Refill) > 0 {
		rec.OweRefill(m.ID, m.Measurement, c.Item.Refill, c.Item.Since)
		if err := a.Save(); err != nil {
			// Nothing has been touched yet, and nothing will be: a clear
			// whose refill the state file cannot hold is a clear nothing
			// may read back.
			rec.Refill = owedBefore
			return failed(fmt.Errorf("the refill it owes could not be recorded before the store was touched: %w", err))
		}
	}
	out, err := applier.Apply(ctx, c.Item)
	if err != nil {
		a.settleFailure(c, rec, owedBefore, out, err)
		return failed(err)
	}
	rec.MarkApplied(m.ID, a.now())
	if out.Kept != nil {
		rec.KeepAside(*out.Kept)
	}
	// Saved before this process forgets what it wrote there: the cache
	// file's rewrite is seconds on a large one, and the record is what a
	// stop inside them must not lose.
	a.save(c, m)
	if a.Cleared != nil {
		a.Cleared(c)
	}
	a.Log.Info("migration applied", "sink", c.Store, "measurement", m.Measurement, "migration", m.ID,
		"did", out.Did, "aside", out.Aside)
	return out, nil
}

// settleFailure records what a failed item leaves: the copy a failed clear
// made, which only the record purges, and the refill, taken back when the
// store says it was left as it was and kept owed when it cannot say, since the
// store may have been cleared behind an answer that did not arrive.
func (a Applying) settleFailure(c Chosen, rec *run.StoreRecord, owedBefore *run.Refill, out Outcome, err error) {
	m := c.Item.Migration
	if out.Kept != nil {
		rec.KeepAside(*out.Kept)
	}
	switch {
	case errors.Is(err, teardown.ErrUntouched):
		rec.Refill = owedBefore
	case len(c.Item.Refill) > 0:
		a.Log.Warn("the store may have been cleared, so the refill stays owed", "sink", c.Store,
			"measurement", m.Measurement, "migration", m.ID, "families", strings.Join(c.Item.Refill, ","))
		if a.Cleared != nil {
			a.Cleared(c)
		}
	}
	a.save(c, m)
}

// save writes the state file after an item, and says when it could not.
func (a Applying) save(c Chosen, m Migration) {
	if err := a.Save(); err != nil {
		// A store that can be asked shows it cleared at the next start, a
		// store that cannot is asked to be cleared again; the refill owed
		// was saved before the store was touched.
		a.Log.Error("migration not recorded: the state file was not saved", "sink", c.Store,
			"migration", m.ID, "err", err)
	}
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
		return fmt.Sprintf("it holds rows of %s, which this configuration does not collect",
			quoted(named(c.Item.Others, c.Item.Migration.Account)))
	}
	return firstOf(c.Item.Unchecked, "whose rows it holds could not be read")
}

// joinReasons is a list of reasons as one attribute.
func joinReasons(why []string) string { return strings.Join(why, "; ") }
