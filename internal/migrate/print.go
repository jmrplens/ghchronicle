package migrate

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// statusWidth is the status column: the longest word, "unreachable", and a
// space.
const statusWidth = 12

// indent is where an item's detail lines start, under its ID.
var indent = strings.Repeat(" ", 2+statusWidth)

// Print writes the plan as -migrate shows it: every configured store, what
// it holds of each registered change and what applying would do, and last a
// line that says nothing was changed, which a dry run never did.
func (p Plan) Print(w io.Writer) {
	p.PrintStores(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, p.summary())
}

// PrintStores is the plan without its last line, which is what -migrate -yes
// prints before it applies: the same findings the dry run showed, so what is
// about to be done is on the screen above what was done.
func (p Plan) PrintStores(w io.Writer) {
	fmt.Fprintf(w, "ghchronicle %s: what this release would change in the stores this configuration writes\n",
		p.Release)
	for _, note := range p.Notes {
		fmt.Fprintf(w, "note: %s\n", note)
	}
	for _, st := range p.Stores {
		fmt.Fprintln(w)
		printStore(w, st)
	}
}

// Report is what -migrate -yes did, after the plan: a line for every item it
// applied, failed to apply or held back, what the refill did, and a last line
// that counts them and says how to go on.
type Report struct {
	Results []Result
	Held    []Chosen
	// Unreached is every store or item the plan could not ask, which
	// nothing was done about.
	Unreached []string
	// Refill is what reading the cleared history again came to, nil when no
	// store was owed it.
	Refill *Refilled
	// Others is the flag that applies what was held back, and Resume how to
	// carry on after a failure.
	Others, Resume string
}

// Failed says whether anything was left undone.
func (r Report) Failed() bool {
	if r.Refill != nil && r.Refill.Err != nil || len(r.Held) > 0 || len(r.Unreached) > 0 {
		return true
	}
	for _, res := range r.Results {
		if res.Err != nil {
			return true
		}
	}
	return false
}

// Print writes the report.
func (r Report) Print(w io.Writer) {
	fmt.Fprintln(w)
	if len(r.Results)+len(r.Held)+len(r.Unreached) == 0 && r.Refill == nil {
		fmt.Fprintln(w, "Nothing to migrate.")
		return
	}
	applied, failed := 0, 0
	for _, res := range r.Results {
		id := res.Item.Migration.ID + " in " + res.Store
		if res.Err != nil {
			failed++
			fmt.Fprintf(w, "  %-*s%s: %s\n", statusWidth, "failed", id, oneLine(res.Err.Error()))
			continue
		}
		applied++
		fmt.Fprintf(w, "  %-*s%s: %s\n", statusWidth, "applied", id, firstOf(res.Outcome.Did, "done"))
		if res.Outcome.Aside != "" {
			fmt.Fprintf(w, "%sthe old rows are kept as %s\n", indent, res.Outcome.Aside)
		}
		for _, c := range res.Outcome.Commands {
			fmt.Fprintf(w, "%s  %s\n", indent, c)
		}
	}
	for _, c := range r.Held {
		fmt.Fprintf(w, "  %-*s%s in %s: %s; %s applies it anyway\n", statusWidth, "held back",
			c.Item.Migration.ID, c.Store, c.HeldBack(), r.Others)
	}
	for _, u := range r.Unreached {
		fmt.Fprintf(w, "  %-*s%s\n", statusWidth, "unreachable", u)
	}
	r.Refill.print(w)
	fmt.Fprintln(w)
	var parts []string
	switch {
	case len(r.Results) > 0 || r.Refill == nil:
		parts = append(parts, fmt.Sprintf("%d applied", applied))
	case r.Refill.Err == nil:
		// A run that only paid what an earlier one left owed.
		parts = append(parts, "Nothing to apply, and the refill owed was read")
	default:
		parts = append(parts, "Nothing to apply, and the refill is still owed")
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if len(r.Held) > 0 {
		parts = append(parts, fmt.Sprintf("%d held back", len(r.Held)))
	}
	if len(r.Unreached) > 0 {
		parts = append(parts, fmt.Sprintf("%d not asked", len(r.Unreached)))
	}
	line := strings.Join(parts, ", ") + "."
	if r.Failed() {
		line += " " + r.Resume
	}
	fmt.Fprintln(w, line)
}

// print is the refill's lines of the report: what was read again, and how
// each copy of the old rows compares with it.
func (r *Refilled) print(w io.Writer) {
	if r == nil {
		return
	}
	if r.Err != nil {
		fmt.Fprintf(w, "  %-*sreading the history again did not finish, and is still owed: %s\n", statusWidth, "refill",
			oneLine(r.Err.Error()))
		return
	}
	fmt.Fprintf(w, "  %-*sread %s again, %s, writing %s\n", statusWidth, "refill", quoted(r.Walk.Families),
		bound(r.Walk.Since), writing(r.Walk.Keep))
	for _, c := range r.Reconciled {
		id := c.Measurement + " in " + c.Store
		switch {
		case c.Err != nil:
			fmt.Fprintf(w, "  %-*s%s: not compared with %s: %s\n", statusWidth, "reconciled", id, firstOf(c.Aside, "its copy"),
				oneLine(c.Err.Error()))
		case len(c.Gone) == 0:
			fmt.Fprintf(w, "  %-*s%s: %d %s in %s, %d now; GitHub served every one again\n", statusWidth,
				"reconciled", id, c.Before, plural(c.Before, "item", "items"), c.Aside, c.After)
		default:
			fmt.Fprintf(w, "  %-*s%s: %d %s in %s, %d now; %d GitHub no longer serves, whose rows are only "+
				"in the copy until it is purged: %s\n", statusWidth, "reconciled", id, c.Before,
				plural(c.Before, "item", "items"), c.Aside, c.After, len(c.Gone), sample(c.Gone))
		}
	}
}

// writing names what a refill writes where, the way a sentence does.
// Stores cleared of the same measurements are named together, which is
// every store of a migration of one measurement.
func writing(keep map[string][]string) string {
	stores := make([]string, 0, len(keep))
	for s := range keep {
		stores = append(stores, s)
	}
	slices.Sort(stores)
	var order []string
	to := map[string][]string{}
	for _, s := range stores {
		what := quoted(keep[s])
		if _, seen := to[what]; !seen {
			order = append(order, what)
		}
		to[what] = append(to[what], s)
	}
	parts := make([]string, 0, len(order))
	for _, what := range order {
		parts = append(parts, what+" to "+quoted(to[what]))
	}
	return strings.Join(parts, "; ")
}

// printStore is one store's heading and its items.
func printStore(w io.Writer, st StorePlan) {
	heading := st.Name
	if st.Destination != "" {
		heading += "  " + st.Destination
	}
	switch {
	case st.Quiet != "":
		fmt.Fprintf(w, "%s\n  nothing to migrate: %s\n", heading, st.Quiet)
		return
	case st.Err != nil:
		fmt.Fprintf(w, "%s\n  %-*s%s\n", heading, statusWidth, Unreachable,
			"it did not answer, so what it holds is not known: "+oneLine(st.Err.Error()))
		return
	case st.Server != "":
		heading += "  (" + st.Server + ")"
	}
	fmt.Fprintln(w, heading)
	for _, it := range st.Items {
		printItem(w, it)
	}
	if o := st.Owed; o != nil {
		fmt.Fprintf(w, "  %-*s%s, %s, writing %s: cleared by %s and not read back yet; -migrate -yes, "+
			"or a start under migrate: auto, reads it\n", statusWidth, "refill owed", quoted(o.Families),
			bound(o.Since), quoted(o.Measurements), quoted(o.Migrations))
	}
	for _, a := range st.Kept {
		purged := "purged by ghchronicle after "
		if a.ByServer {
			purged = "purged by the store itself after "
		}
		fmt.Fprintf(w, "  %-*s%s: %s, set aside %s, %s%s\n", statusWidth, "kept aside", a.Migration, a.Name,
			a.At.UTC().Format(timeLayout), purged, a.At.Add(teardown.Grace).UTC().Format(timeLayout))
	}
}

// timeLayout is an instant as the plan says it.
const timeLayout = "2006-01-02 15:04 UTC"

// printItem is one migration in one store: a line for a settled one, and
// what would be done for one that is not.
func printItem(w io.Writer, it Item) {
	switch it.Status {
	case NotNeeded, Applied, Unreachable:
		fmt.Fprintf(w, "  %-*s%s: %s\n", statusWidth, it.Status, it.Migration.ID, oneLine(it.Evidence))
		return
	}
	fmt.Fprintf(w, "  %-*s%s\n", statusWidth, it.Status, it.Migration.ID)
	detail := func(label, text string) {
		if text != "" {
			fmt.Fprintf(w, "%s%s%s\n", indent, label, text)
		}
	}
	detail("", it.Evidence)
	detail("why: ", it.Migration.Why)
	detail("", it.Action)
	for _, c := range it.Commands {
		detail("  ", c)
	}
	if len(it.Refill) > 0 {
		detail("read again: ", fmt.Sprintf("%s, %s, writing %s only", quoted(it.Refill), bound(it.Since),
			it.Migration.Measurement))
	}
	for _, lost := range it.Lost {
		detail("not coming back: ", lost)
	}
	if it.Status != Pending {
		return
	}
	if it.Safe {
		detail("", "safe to apply unattended: GitHub serves the whole history, the old rows are set aside "+
			"for 24 hours, and every row is this configuration's")
		return
	}
	for _, why := range it.Unsafe {
		detail("needs your word: ", why)
	}
}

// bound says how far back a refill reads.
func bound(since time.Time) string {
	if since.IsZero() {
		return "with no bound"
	}
	return "since " + since.UTC().Format(time.DateOnly)
}

// summary is the plan's last line.
func (p Plan) summary() string {
	var parts []string
	for _, c := range []struct {
		s    Status
		word string
	}{{Pending, "pending"}, {Noted, "noted"}, {Frozen, "frozen"}, {Unreachable, "unreachable"}} {
		if n := p.Count(c.s); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, c.word))
		}
	}
	owed := 0
	for _, st := range p.Stores {
		if st.Owed != nil {
			owed++
		}
	}
	if owed > 0 {
		parts = append(parts, fmt.Sprintf("%d %s owed a refill", owed, plural(owed, "store", "stores")))
	}
	for _, st := range p.Stores {
		if st.Err != nil {
			parts = append(parts, st.Name+" did not answer")
		}
	}
	if len(parts) == 0 {
		return "Nothing to migrate. Nothing was changed."
	}
	return strings.Join(parts, ", ") + ". Nothing was changed."
}

// oneLine keeps a store's complaint on the line it is quoted in.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
