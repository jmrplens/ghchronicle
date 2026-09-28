package migrate

import (
	"fmt"
	"io"
	"strings"
	"time"
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
	fmt.Fprintf(w, "ghchronicle %s: what this release would change in the stores this configuration writes\n",
		p.Release)
	for _, note := range p.Notes {
		fmt.Fprintf(w, "note: %s\n", note)
	}
	for _, st := range p.Stores {
		fmt.Fprintln(w)
		printStore(w, st)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, p.summary())
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
}

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
