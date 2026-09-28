// Package migrate brings a store an earlier release wrote into the shape this
// one writes.
//
// A point is keyed by its measurement, its tags and its time, so a release
// that moves a value out of the tags changes what a row is: the rows already
// stored keep the old identity and sit beside the new ones for ever, and every
// count over them drifts upwards. Each time that happened the operator was
// left to drop the table and run a backfill by hand. This package names every
// such change once, in Registry, and works out for each configured store
// whether it still holds the old shape and what bringing it along would take.
package migrate

import (
	"slices"
	"strconv"
	"strings"
)

// Kind is what a release changed about a measurement.
type Kind int

const (
	// Identity: the rows before and after are different series, because a
	// tag was removed or renamed. Stored rows sit beside new ones.
	Identity Kind = iota + 1
	// Value: the identity is unchanged and the rows dated before the release
	// hold a value it no longer writes. Nothing is doubled, and nothing can
	// be read again to put it right.
	Value
)

// Reach is how much of a measurement GitHub can still serve, which decides
// whether its rows can be dropped and read again or only be spoken about.
type Reach int

const (
	// Whole: every item, whatever its age, so a drop is undone by reading
	// the history again.
	Whole Reach = iota + 1
	// Current: today's state only. No past day comes back, so its rows are
	// never dropped.
	Current
)

// PreRelease is the Release of a change made before the first release: the
// shape only builds that were never released wrote. No state file can say a
// store was written by one, so only a store that can be asked finds it.
const PreRelease = "1.0.0"

// Migration is one change to one measurement.
type Migration struct {
	// ID is written into state files. Never renamed, never reused.
	ID string
	// Release is the first release that writes the new shape. PreRelease for
	// a change made before the first release.
	Release     string
	Measurement string
	Kind        Kind
	// OldTags are the tag keys only the old shape carries: what a store is
	// asked about, what makes Graphite's old paths one node deeper, and what
	// the identity gate checks a removed tag against.
	OldTags []string
	// Families is every family that writes the measurement. Held to the
	// collectors by a sweep of the fake GitHub one family at a time, never
	// trusted to memory: 2.6.1's manual refill read one of the two.
	Families []string
	// Account is the tag that says whose rows these are, for telling a store
	// this configuration alone writes from one it shares with another.
	Account string
	Reach   Reach
	// Why is one sentence for the plan and the log.
	Why   string
	Issue string
}

// Registry is every change a store written by a 2.x release can still hold,
// ordered by Release and then by ID. Append-only: an ID in a state file has to
// keep meaning what it meant.
//
// The records are a set of IDs rather than a schema number because entries
// are registered after the fact, as all four below were, and a counter cannot
// take a new entry for an old release.
//
// 2.0.0 is not here. It added owner and full_name to thirteen measurements,
// two of them windows GitHub forgets (gh_event, gh_notification), so a drop
// there loses what no refill returns; a 1.x store is recreated or cleaned by
// hand, as the upgrading page says.
var Registry = []Migration{
	{
		ID: "1.0.0/gh_code_scanning_alert_item/state", Release: PreRelease,
		Measurement: "gh_code_scanning_alert_item", Kind: Identity,
		OldTags: []string{"state", "reason"}, Families: []string{"security"},
		Account: "owner", Reach: Whole,
		// alert_state is in the first public commit, under a new name so a
		// store that already held the tag kept accepting writes: only builds
		// before 1.0.0 wrote these tags.
		Why: "a build before 1.0.0 tagged each code scanning alert with its state and the reason it closed, " +
			"so an alert fixed after it was first read is two rows at one instant",
	},
	{
		ID: "1.0.0/gh_dependabot_alert_item/state", Release: PreRelease,
		Measurement: "gh_dependabot_alert_item", Kind: Identity,
		OldTags: []string{"state"}, Families: []string{"security"},
		Account: "owner", Reach: Whole,
		Why: "a build before 1.0.0 tagged each Dependabot alert with its state, " +
			"so an alert fixed after it was first read is two rows at one instant",
	},
	{
		ID: "2.6.0/gh_actions_cache_entry/sum", Release: "2.6.0",
		Measurement: "gh_actions_cache_entry", Kind: Value,
		Families: []string{"actions"}, Account: "owner", Reach: Current,
		// The identity never changed: the same tags and the same day before
		// and after. A row before 2.6.0 is the last entry of its cache and
		// ref that was written, not a second series, so nothing is doubled.
		Why: "rows dated before 2.6.0 hold one entry of each cache and ref, not their sum; " +
			"no API lists a past day's caches, so they are kept as they are",
		Issue: "https://github.com/jmrplens/ghchronicle/issues/91",
	},
	{
		ID: "2.6.1/gh_discussion_comment/is_answer", Release: "2.6.1",
		Measurement: "gh_discussion_comment", Kind: Identity,
		OldTags:  []string{"is_answer"},
		Families: []string{"discussions", "outbound"},
		Account:  "user", Reach: Whole,
		Why: "is_answer was a tag, so a comment read before and after its " +
			"acceptance is two rows at one instant",
		Issue: "https://github.com/jmrplens/ghchronicle/issues/96",
	},
}

// noteOnly says whether nothing can ever apply the change: rows that cannot
// be told apart, or history GitHub no longer serves, are only spoken about.
func (m Migration) noteOnly() bool { return m.Kind == Value || m.Reach == Current }

// writtenBy says whether a store first written by release was written in the
// new shape. An unknown release, "" or one that does not parse, says no.
func (m Migration) writtenBy(release string) bool {
	return compareRelease(release, m.Release) >= 0
}

// compareRelease orders two release numbers, major.minor.patch, and treats
// anything after the patch (a pre-release, a build) as that patch. A number
// that does not parse sorts before every release, which is what an unknown
// writer has to be read as: older than whatever is asked about.
func compareRelease(a, b string) int {
	pa, okA := parseRelease(a)
	pb, okB := parseRelease(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	return slices.Compare(pa, pb)
}

// parseRelease reads the three numbers of a release, with or without a
// leading v.
func parseRelease(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return nil, false
	}
	// The patch may carry a suffix: 2.6.2-rc.1 or 2.6.2+dirty.
	if i := strings.IndexFunc(parts[2], func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		parts[2] = parts[2][:i]
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
