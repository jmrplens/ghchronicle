package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// A store a migration cleared holds none of that measurement's history until
// it is read again from GitHub: the refill. It is owed from the moment the
// store is cleared, which the state file records before anything is read, and
// it is paid by one backfill of the families that write what was cleared,
// writing that and nothing else, into the stores that were cleared and no
// other. A refill cut short resumes from its own checkpoint.

// Owed is one store's refill, as the state file records it.
type Owed struct {
	Store  string
	Refill run.Refill
}

// OwedIn is every refill the state file records as owed to a store this
// configuration writes, in the order the configuration names its sinks. A
// record kept for a sink since pointed elsewhere, or no longer configured, is
// about a store no run can reach from here, and is left for when it can.
func OwedIn(state *run.State, cfg *config.Config) []Owed {
	var out []Owed
	for _, st := range storesOf(cfg) {
		rec := state.Stores[st.name]
		if rec == nil || rec.Refill == nil || rec.Destination != st.destination {
			continue
		}
		out = append(out, Owed{Store: st.name, Refill: *rec.Refill})
	}
	return out
}

// RefillWalk is what a refill reads again and where it writes it.
type RefillWalk struct {
	// Families is every family the walk runs, sorted.
	Families []string
	// Keep is, per store, the measurements written back to it: what was
	// cleared there, and nothing else.
	Keep map[string][]string
	// Since is how far back the walk reads: the furthest back any store is
	// owed. Zero is no bound.
	Since time.Time
}

// walkOf is the one walk that pays every refill owed. One walk rather than one
// per store, because every store is owed by the same families asking GitHub
// the same things, and a second walk would ask them again for nothing.
func walkOf(owed []Owed) RefillWalk {
	w := RefillWalk{Keep: map[string][]string{}}
	unbounded := false
	for _, o := range owed {
		for _, f := range o.Refill.Families {
			if !slices.Contains(w.Families, f) {
				w.Families = append(w.Families, f)
			}
		}
		w.Keep[o.Store] = slices.Clone(o.Refill.Measurements)
		switch since := o.Refill.Since; {
		case since.IsZero():
			unbounded = true
		case w.Since.IsZero() || since.Before(w.Since):
			w.Since = since
		}
	}
	if unbounded {
		w.Since = time.Time{}
	}
	slices.Sort(w.Families)
	return w
}

// measurements is every measurement the walk writes, sorted, each once.
func (w RefillWalk) measurements() []string {
	var out []string
	for _, ms := range w.Keep {
		out = append(out, ms...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// stores is every store the walk writes to, sorted.
func (w RefillWalk) stores() []string {
	out := make([]string, 0, len(w.Keep))
	for s := range w.Keep {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// Refiller reads again what a walk names and writes it where the walk says.
// An error is a walk that did not reach the end of every family, whose
// checkpoint is kept for the next run to resume.
type Refiller func(ctx context.Context, walk RefillWalk) error

// Refilled is what paying the refills owed came to.
type Refilled struct {
	Walk RefillWalk
	// Err is why the walk did not reach the end. The refills stay owed.
	Err error
	// Reconciled is, per store and measurement whose copy of the old rows
	// could be read, how the copy compares with what was read back.
	Reconciled []Reconciliation
}

// refill pays every refill the state file records as owed, and forgets each
// one only once the walk has reached the end of every family, which is the
// only moment it is known that nothing more is coming.
func (a Applying) refill(ctx context.Context) *Refilled {
	owed := OwedIn(a.State, a.Config)
	if len(owed) == 0 {
		return nil
	}
	w := walkOf(owed)
	out := &Refilled{Walk: w}
	args := []any{
		"families", strings.Join(w.Families, ","), "measurements", strings.Join(w.measurements(), ","),
		"sinks", strings.Join(w.stores(), ","), "since", sinceAttr(w.Since),
	}
	if a.Refill == nil {
		out.Err = ErrNoRefill
		a.Log.Error("refill owed and not read: this build has no way to", append(args, "err", out.Err)...)
		return out
	}
	a.Log.Info("refill starting", args...)
	if out.Err = a.Refill(ctx, w); out.Err != nil {
		a.Log.Error("refill did not finish, and is still owed", append(args, "err", out.Err, "resume", a.Resume)...)
		return out
	}
	for _, o := range owed {
		if rec := a.State.Stores[o.Store]; rec != nil {
			rec.Refill = nil
		}
	}
	if err := a.Save(); err != nil {
		// The next run reads the refill as still owed and walks it again,
		// which rewrites the same rows: time lost, nothing else.
		a.Log.Warn("refill complete and not recorded: the state file was not saved", "err", err)
	}
	a.Log.Info("refill complete", args...)
	out.Reconciled = a.reconcile(ctx, owed)
	return out
}

// Reconciliation is one cleared measurement in one store, compared, once
// the refill ended, with the copy of its old rows the migration kept aside.
type Reconciliation struct {
	Store       string
	Measurement string
	// Aside is the copy compared with.
	Aside string
	// Before is how many items the copy holds, and After how many the table
	// holds now.
	Before, After int
	// Gone is every item the copy holds and the table does not: what GitHub
	// no longer served, whose rows are only in the copy until it is purged.
	Gone []string
	// Err is why the two could not be compared.
	Err error
}

// reconcile compares, where the store keeps a copy and can say which items a
// table holds, each cleared measurement with its copy. A refill cannot bring
// back what GitHub no longer serves: a repository deleted or no longer
// covered, an alert whose feature was switched off. Those rows are in the
// copy for its day, and this is the one moment they can still be named.
func (a Applying) reconcile(ctx context.Context, owed []Owed) []Reconciliation {
	var out []Reconciliation
	for _, o := range owed {
		reader := a.Readers[o.Store]
		rec := a.State.Stores[o.Store]
		if reader == nil || rec == nil {
			continue
		}
		for _, id := range o.Refill.Migrations {
			m, known := registered(id)
			if !known || len(m.Item) == 0 {
				continue
			}
			aside, kept := newestAside(rec, m)
			if !kept {
				continue
			}
			r := Reconciliation{Store: o.Store, Measurement: m.Measurement, Aside: aside.Name}
			if a.now().After(aside.At.Add(teardown.Grace)) {
				r.Err = fmt.Errorf("the copy was due to be purged at %s", aside.At.Add(teardown.Grace).UTC().Format(timeLayout))
			} else {
				r.Before, r.After, r.Gone, r.Err = compareItems(ctx, reader, aside.Name, m)
			}
			a.logReconciled(r)
			out = append(out, r)
		}
	}
	return out
}

// compareItems reads the items of the copy and of the table.
func compareItems(ctx context.Context, reader teardown.ItemReader, aside string, m Migration) (
	before, after int, gone []string, err error,
) {
	was, err := reader.Items(ctx, aside, m.Item)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("reading the copy: %w", err)
	}
	now, err := reader.Items(ctx, m.Measurement, m.Item)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("reading %s: %w", m.Measurement, err)
	}
	for _, item := range was {
		if _, found := slices.BinarySearch(now, item); !found {
			gone = append(gone, item)
		}
	}
	return len(was), len(now), gone, nil
}

// logReconciled says one comparison.
func (a Applying) logReconciled(r Reconciliation) {
	if r.Err != nil {
		a.Log.Warn("not reconciled: the copy and the table could not be compared", "sink", r.Store,
			"measurement", r.Measurement, "aside", r.Aside, "err", r.Err)
		return
	}
	args := []any{
		"sink", r.Store, "measurement", r.Measurement, "aside", r.Aside,
		"items_before", r.Before, "items_after", r.After, "not_served", len(r.Gone),
	}
	if len(r.Gone) == 0 {
		a.Log.Info("reconciled", args...)
		return
	}
	// A warning, since those rows go with the copy: carrying them over by
	// hand is the reader's to decide while it is there.
	a.Log.Warn("reconciled", append(args, "first", sample(r.Gone), "only_in", r.Aside)...)
}

// sinceAttr is a refill's bound as a log attribute.
func sinceAttr(since time.Time) string {
	if since.IsZero() {
		return "none"
	}
	return since.UTC().Format(time.DateOnly)
}

// registered is the migration of an ID.
func registered(id string) (Migration, bool) {
	i := slices.IndexFunc(Registry, func(m Migration) bool { return m.ID == id })
	if i < 0 {
		return Migration{}, false
	}
	return Registry[i], true
}

// newestAside is the latest copy a migration kept in a store.
func newestAside(rec *run.StoreRecord, m Migration) (run.Aside, bool) {
	var found run.Aside
	for _, a := range rec.SetAside {
		if (a.Migration == m.ID || a.Migration == "" && a.Measurement == m.Measurement) && a.At.After(found.At) {
			found = a
		}
	}
	return found, found.Name != ""
}
