package run

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"time"
)

// A state file has one saver at a time only while somebody holds its lock,
// and a one-shot run holds it only while it applies a migration: a -once from
// cron, or a -backfill that runs for hours, reads the file when it starts and
// saves it when it ends. A -migrate -yes that ran between the two recorded
// what it cleared and the refill it still owes, and the one-shot's save put
// back the copy it had read before, which forgot both: the cleared store then
// looked like one that never held the old shape, and its history was never
// read again. So a save takes, for the stores, what another process recorded
// since this one read the file, and keeps its own changes beside them.

// storesCopy is a copy of what the state records of the stores now.
func (s *State) storesCopy() map[string]*StoreRecord {
	var out map[string]*StoreRecord
	if b, err := json.Marshal(s.Stores); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// takeTheirs merges into the state what the file on disk records of the
// stores that another process wrote since this one read it. A file that is
// not there, or cannot be read, has nothing to add.
func (s *State) takeTheirs() {
	b, err := os.ReadFile(s.path)
	if err != nil || !json.Valid(b) {
		return
	}
	var theirs struct {
		Stores map[string]*StoreRecord `json:"stores"`
	}
	if json.Unmarshal(b, &theirs) != nil {
		return
	}
	for _, name := range namesOf(s.loaded, s.Stores, theirs.Stores) {
		if merged := mergeRecord(s.loaded[name], s.Stores[name], theirs.Stores[name]); merged != nil {
			s.Stores[name] = merged
		} else {
			delete(s.Stores, name)
		}
	}
}

// namesOf is every key of the maps, sorted.
func namesOf[V any](ms ...map[string]V) []string {
	var out []string
	for _, m := range ms {
		out = append(out, slices.Collect(maps.Keys(m))...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// mergeFormer merges three lists of former records by their destination.
func mergeFormer(base, mine, theirs []*StoreRecord) []*StoreRecord {
	byDest := func(list []*StoreRecord) map[string]*StoreRecord {
		out := map[string]*StoreRecord{}
		for _, r := range list {
			if r != nil {
				out[r.Destination] = r
			}
		}
		return out
	}
	b, m, t := byDest(base), byDest(mine), byDest(theirs)
	var out []*StoreRecord
	for _, dest := range namesOf(b, m, t) {
		if merged := mergeRecord(b[dest], m[dest], t[dest]); merged != nil && merged.Owes() {
			out = append(out, merged)
		}
	}
	return out
}

// mergeRecord is one store's record as two processes left it, from the one
// both read. Whichever did not change it takes the other's; where both did,
// what each recorded is kept, and a refill either owes stays owed: a refill
// read twice costs its time, and one forgotten costs the history. A record of
// the other's that this one's pointing the sink elsewhere displaced is kept
// among the former ones while it still owes something there.
func mergeRecord(base, mine, theirs *StoreRecord) *StoreRecord {
	switch {
	case sameRecord(mine, base):
		return theirs
	case sameRecord(theirs, base), theirs == nil:
		return mine
	case mine == nil:
		return theirs
	case mine.Destination != theirs.Destination:
		out := *mine
		if theirs.Owes() {
			out.Former = mergeFormer(nil, mine.Former, append(slices.Clone(theirs.Former), theirs.withoutFormer()))
		}
		return &out
	}
	var b StoreRecord
	if base != nil && base.Destination == mine.Destination {
		b = *base
	}
	out := *mine
	out.FirstWrittenBy = changed(b.FirstWrittenBy, mine.FirstWrittenBy, theirs.FirstWrittenBy)
	out.WrittenBy = changed(b.WrittenBy, mine.WrittenBy, theirs.WrittenBy)
	out.Applied = laterOf(mine.Applied, theirs.Applied)
	out.NotNeeded = laterOf(mine.NotNeeded, theirs.NotNeeded)
	for id := range out.Applied {
		delete(out.NotNeeded, id)
	}
	if len(out.NotNeeded) == 0 {
		out.NotNeeded = nil
	}
	out.Noted = laterOf(mine.Noted, theirs.Noted)
	out.SetAside = mergeAsides(b.SetAside, mine.SetAside, theirs.SetAside)
	switch {
	case sameJSON(mine.Refill, b.Refill):
		out.Refill = theirs.Refill.Clone()
	case sameJSON(theirs.Refill, b.Refill):
	default:
		out.Refill = unionRefill(mine.Refill, theirs.Refill)
	}
	out.Former = mergeFormer(b.Former, mine.Former, theirs.Former)
	return &out
}

// withoutFormer is the record on its own, for keeping among another's former
// ones: a former record's own former ones are kept beside it.
func (r *StoreRecord) withoutFormer() *StoreRecord {
	out := *r
	out.Former = nil
	return &out
}

// changed is whichever of mine and theirs differs from base, mine when both
// do.
func changed(base, mine, theirs string) string {
	if mine == base {
		return theirs
	}
	return mine
}

// laterOf is both maps together, the later instant where both hold an ID.
func laterOf(a, b map[string]time.Time) map[string]time.Time {
	if len(a)+len(b) == 0 {
		return nil
	}
	out := maps.Clone(a)
	if out == nil {
		out = map[string]time.Time{}
	}
	for id, when := range b {
		if when.After(out[id]) {
			out[id] = when
		}
	}
	return out
}

// mergeAsides is the copies both kept and the ones either added; a copy
// either purged since the base is gone.
func mergeAsides(base, mine, theirs []Aside) []Aside {
	has := func(list []Aside, name string) bool {
		return slices.ContainsFunc(list, func(a Aside) bool { return a.Name == name })
	}
	var out []Aside
	for _, a := range mine {
		if has(theirs, a.Name) || !has(base, a.Name) {
			out = append(out, a)
		}
	}
	for _, a := range theirs {
		if !has(base, a.Name) && !has(out, a.Name) {
			out = append(out, a)
		}
	}
	return out
}

// unionRefill is two refills owed as one, nil when neither is.
func unionRefill(a, b *Refill) *Refill {
	switch {
	case a == nil:
		return b.Clone()
	case b == nil:
		return a.Clone()
	}
	out := a.Clone()
	out.Migrations = sortedUnion(out.Migrations, b.Migrations...)
	out.Families = sortedUnion(out.Families, b.Families...)
	out.Measurements = sortedUnion(out.Measurements, b.Measurements...)
	if b.Since.IsZero() || b.Since.Before(out.Since) {
		out.Since = b.Since
	}
	return out
}

// sameRecord says whether two records say the same thing.
func sameRecord(a, b *StoreRecord) bool { return sameJSON(a, b) }

// sameJSON says whether two values encode alike, which for records read from
// one file is whether they say the same thing.
func sameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}
