package migrate

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

var updateIdentity = flag.Bool("update", false,
	"rewrite identity.json from a sweep of the fake GitHub, when Registry explains every tag that went away")

// identityPath is the golden file, relative to this package, which is where
// go test runs.
const identityPath = "identity.json"

// TestEveryChangeOfIdentityIsRegistered is the gate that would have caught
// is_answer in review. It sweeps the fake GitHub and compares every
// measurement's tag keys with the ones identity.json recorded: a tag that went
// away is a new identity for every row written after it, and the rows already
// stored keep the old one beside them for ever, so it has to be a registered
// migration before the file can be rewritten to match.
func TestEveryChangeOfIdentityIsRegistered(t *testing.T) {
	t.Parallel()
	got := sweepFake(t, fakegh.Families())
	want := identity()
	if *updateIdentity {
		if problems := unexplained(Registry, want, got); len(problems) > 0 {
			t.Fatalf("identity.json not rewritten:\n%s", strings.Join(problems, "\n"))
		}
		writeIdentity(t, got)
		return
	}
	for _, problem := range unexplained(Registry, want, got) {
		t.Error(problem)
	}
	for _, change := range explained(Registry, want, got) {
		t.Errorf("%s; run go test ./internal/migrate -run TestEveryChangeOfIdentityIsRegistered -update", change)
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
