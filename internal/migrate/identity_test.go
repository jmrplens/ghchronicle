package migrate

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

var updateIdentity = flag.Bool("update", false,
	"rewrite identity.json from a sweep of the fake GitHub, and pin every registry entry, when the entries not "+
		"pinned yet explain every tag that went away")

// identityPath is the golden file, relative to this package, which is where
// go test runs.
const identityPath = "identity.json"

// TestEveryChangeOfIdentityIsRegistered is the gate that would have caught
// is_answer in review. It sweeps the fake GitHub and compares every
// measurement's tag keys with the ones identity.json recorded: a tag that went
// away is a new identity for every row written after it, and the rows already
// stored keep the old one beside them for ever, so it has to be a registered
// migration before the file can be rewritten to match.
//
// Only an entry not yet pinned in testdata/registry.json explains a tag that
// went away. A pinned entry is one a release may have shipped, and a state
// file that records its ID has settled it for good: a second tag added to its
// OldTags, the quick fix the refusal below seems to invite, would never be
// asked about in a store that recorded the entry as applied or not needed,
// and Graphite's command would name the depth of the first shape only. So a
// pinned entry never changes, and -update pins every entry it accepts.
func TestEveryChangeOfIdentityIsRegistered(t *testing.T) {
	t.Parallel()
	got := sweepFake(t, fakegh.Families())
	want := identity()
	pinned := readPins(t)
	for _, problem := range pinsBroken(Registry, pinned) {
		t.Error(problem)
	}
	fresh := unpinned(Registry, pinned)
	if *updateIdentity {
		if problems := unexplained(fresh, want, got); len(problems) > 0 || t.Failed() {
			t.Fatalf("identity.json not rewritten:\n%s", strings.Join(problems, "\n"))
		}
		writeIdentity(t, got)
		writePins(t, Registry)
		return
	}
	for _, problem := range unexplained(fresh, want, got) {
		t.Error(problem)
	}
	for _, change := range explained(fresh, want, got) {
		t.Errorf("%s; run go test ./internal/migrate -run TestEveryChangeOfIdentityIsRegistered -update", change)
	}
	for _, m := range fresh {
		t.Errorf("%s is not pinned in %s; run go test ./internal/migrate -run TestEveryChangeOfIdentityIsRegistered "+
			"-update once it is what the release will ship", m.ID, pinsPath)
	}
}

// pinsPath is the file that pins every registered entry as it was accepted.
const pinsPath = "testdata/registry.json"

// pin is what may never change about an entry once it is pinned: what a
// state file's record of its ID means.
type pin struct {
	ID          string   `json:"id"`
	Release     string   `json:"release"`
	Measurement string   `json:"measurement"`
	Kind        Kind     `json:"kind"`
	OldTags     []string `json:"old_tags"`
}

func pinOf(m Migration) pin {
	return pin{ID: m.ID, Release: m.Release, Measurement: m.Measurement, Kind: m.Kind, OldTags: m.OldTags}
}

// readPins is the pinned entries, by ID.
func readPins(t *testing.T) map[string]pin {
	t.Helper()
	b, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("%s: %v", pinsPath, err)
	}
	var list []pin
	if err = json.Unmarshal(b, &list); err != nil {
		t.Fatalf("%s: %v", pinsPath, err)
	}
	out := make(map[string]pin, len(list))
	for _, p := range list {
		out[p.ID] = p
	}
	return out
}

// pinsBroken is every pinned entry the registry no longer holds as it was
// pinned, worded for the reader who changed it.
func pinsBroken(registry []Migration, pinned map[string]pin) []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(pinned)) {
		i := slices.IndexFunc(registry, func(m Migration) bool { return m.ID == id })
		switch {
		case i < 0:
			out = append(out, fmt.Sprintf("%s is pinned and no longer registered: an ID a state file may hold "+
				"has to keep meaning what it meant", id))
		case !reflect.DeepEqual(pinOf(registry[i]), pinned[id]):
			out = append(out, fmt.Sprintf("%s changed from %+v to %+v after it was pinned. A state file that "+
				"recorded it settled it for good, so a new change of %s is a new entry, of the release that makes "+
				"it, with its own ID", id, pinned[id], pinOf(registry[i]), registry[i].Measurement))
		}
	}
	return out
}

// unpinned is every entry not pinned yet: the ones that may explain a change
// the sweep sees for the first time.
func unpinned(registry []Migration, pinned map[string]pin) []Migration {
	var out []Migration
	for _, m := range registry {
		if _, done := pinned[m.ID]; !done {
			out = append(out, m)
		}
	}
	return out
}

// writePins pins every entry, one a line.
func writePins(t *testing.T, registry []Migration) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[\n")
	for i, m := range registry {
		line, err := json.Marshal(pinOf(m))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("  ")
		b.Write(line)
		if i < len(registry)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	if err := os.WriteFile(pinsPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAShippedEntryCannotExplainANewChange is the gate turned on the quick
// fix it would otherwise invite: a tag own removed later, added to the
// OldTags of the pinned 2.6.1 entry. The pin refuses the changed entry, and
// the removal is unexplained, since only an entry not yet pinned explains one.
func TestAShippedEntryCannotExplainANewChange(t *testing.T) {
	t.Parallel()
	pinned := readPins(t)
	widened := slices.Clone(Registry)
	for i, m := range widened {
		if m.ID == comments {
			m.OldTags = []string{"is_answer", "own"}
			widened[i] = m
		}
	}
	if broken := pinsBroken(widened, pinned); len(broken) != 1 || !strings.Contains(broken[0], comments) {
		t.Errorf("a pinned entry given another old tag is refused as %v, want one refusal naming it", broken)
	}
	now, _ := Tags("gh_discussion_comment")
	before := map[string][]string{"gh_discussion_comment": now}
	after := map[string][]string{"gh_discussion_comment": slices.DeleteFunc(slices.Clone(now), func(t string) bool { return t == "own" })}
	if problems := unexplained(unpinned(widened, pinned), before, after); len(problems) != 1 ||
		!strings.Contains(problems[0], "own") {
		t.Errorf("own removed and explained by a pinned entry gives %v, want one refusal naming own", problems)
	}
}

// unexplained is every difference between two sweeps that Registry does not
// account for, each worded as what the reader has to do about it.
func unexplained(registry []Migration, want, got map[string][]string) []string {
	var out []string
	for _, m := range slices.Sorted(keysOf(want, got)) {
		before, was := want[m]
		after, is := got[m]
		if !was || !is {
			continue
		}
		gone, added := difference(before, after), difference(after, before)
		if missing := difference(gone, registeredOldTags(registry, m)); len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s no longer carries the tag %s, so every row written from now on "+
				"is a series the rows already stored are not: register it in migrate.Registry, with %s in OldTags "+
				"and every family that writes %s, so a store that holds the old shape can be brought along",
				m, strings.Join(missing, ", "), strings.Join(missing, ", "), m))
		}
		if len(added) > 0 {
			out = append(out, fmt.Sprintf("%s gained the tag %s, so every row written from now on is a series "+
				"the rows already stored are not. A new value is a field, never a tag (CONTRIBUTING.md, "+
				"a new field on an existing measurement); the registry can name a tag that went away and has "+
				"no way yet to find the rows that lack one",
				m, strings.Join(added, ", ")))
		}
	}
	return out
}

// explained is every difference Registry accounts for, or that changes no
// identity: a measurement the sweep writes for the first time, one it no
// longer writes, a tag a registered migration removed. Each still leaves the
// golden file behind the code, which -update puts right.
func explained(registry []Migration, want, got map[string][]string) []string {
	var out []string
	for _, m := range slices.Sorted(keysOf(want, got)) {
		before, was := want[m]
		after, is := got[m]
		switch {
		case !was:
			out = append(out, fmt.Sprintf("%s is new, with the tags %v", m, after))
		case !is:
			out = append(out, fmt.Sprintf("%s is no longer written by a sweep of the fake; a measurement "+
				"nobody writes any more is history and needs no migration", m))
		default:
			gone := difference(before, after)
			if len(gone) > 0 && len(difference(gone, registeredOldTags(registry, m))) == 0 {
				out = append(out, fmt.Sprintf("%s lost the tag %s, which the registry names", m, strings.Join(gone, ", ")))
			}
		}
	}
	return out
}

// registeredOldTags is every tag the registry says a migration of m removed.
func registeredOldTags(registry []Migration, measurement string) []string {
	var out []string
	for _, m := range registry {
		if m.Measurement == measurement && m.Kind == Identity {
			out = append(out, m.OldTags...)
		}
	}
	return out
}

// difference is what a holds and b does not.
func difference(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// keysOf is every measurement either map names.
func keysOf(a, b map[string][]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range a {
			if !yield(k) {
				return
			}
		}
		for k := range b {
			if _, both := a[k]; both {
				continue
			}
			if !yield(k) {
				return
			}
		}
	}
}

// writeIdentity writes one measurement a line, so a change to a measurement's
// identity is a one-line diff a reviewer reads beside its registry entry.
func writeIdentity(t *testing.T, shapes map[string][]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("{\n")
	for i, m := range slices.Sorted(maps.Keys(shapes)) {
		name, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		tags, err := json.Marshal(shapes[m])
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "  %s: %s", name, tags)
		if i < len(shapes)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	if err := os.WriteFile(identityPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("identity.json rewritten: %d measurements", len(shapes))
}

// TestTheGateRefusesATagThatWentAwayUnregistered is the gate turned on
// is_answer itself: the shape 2.6.0 wrote against the one this release does,
// first with the registry as it is and then without the entry that explains
// the difference.
func TestTheGateRefusesATagThatWentAwayUnregistered(t *testing.T) {
	t.Parallel()
	now, ok := Tags("gh_discussion_comment")
	if !ok {
		t.Fatal("identity.json has no gh_discussion_comment")
	}
	before := map[string][]string{"gh_discussion_comment": slices.Sorted(slices.Values(append(now, "is_answer")))}
	after := map[string][]string{"gh_discussion_comment": now}
	if problems := unexplained(Registry, before, after); len(problems) > 0 {
		t.Errorf("the registry names is_answer, yet the gate refuses it: %v", problems)
	}
	without := slices.DeleteFunc(slices.Clone(Registry), func(m Migration) bool {
		return m.Measurement == "gh_discussion_comment"
	})
	problems := unexplained(without, before, after)
	if len(problems) != 1 || !strings.Contains(problems[0], "is_answer") {
		t.Errorf("without its entry the gate says %v, want one refusal naming is_answer", problems)
	}
	gained := map[string][]string{"gh_discussion_comment": append(slices.Clone(now), "zz_new")}
	if refused := unexplained(Registry, after, gained); len(refused) != 1 || !strings.Contains(refused[0], "zz_new") {
		t.Errorf("a tag gained is refused as %v, want one refusal naming it", refused)
	}
}
