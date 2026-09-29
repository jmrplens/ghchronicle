package migrate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// writersOnTheFake is which families write each measurement, found by
// sweeping the fake GitHub one family at a time with every other family off.
// Computed once for the tests that need it.
var writersOnTheFake = struct {
	once    sync.Once
	writers map[string][]string
	// shapes is, per measurement and then per family that writes it, the
	// tag keys that family alone wrote.
	shapes map[string]map[string][]string
}{}

func familyWriters(t *testing.T) map[string][]string {
	t.Helper()
	sweepEachFamily(t)
	return writersOnTheFake.writers
}

// familyShapes is, per measurement and then per family that writes it, the
// tag keys that family alone wrote.
func familyShapes(t *testing.T) map[string]map[string][]string {
	t.Helper()
	sweepEachFamily(t)
	return writersOnTheFake.shapes
}

// sweepEachFamily sweeps the fake one family at a time, once for every test.
func sweepEachFamily(t *testing.T) {
	t.Helper()
	writersOnTheFake.once.Do(func() {
		var mu sync.Mutex
		writers := map[string][]string{}
		byFamily := map[string]map[string][]string{}
		t.Run("sweep", func(t *testing.T) {
			for _, family := range fakegh.Families() {
				t.Run(family, func(t *testing.T) {
					t.Parallel()
					shapes := sweepFake(t, []string{family})
					mu.Lock()
					defer mu.Unlock()
					for m, tags := range shapes {
						writers[m] = append(writers[m], family)
						if byFamily[m] == nil {
							byFamily[m] = map[string][]string{}
						}
						byFamily[m][family] = tags
					}
				})
			}
		})
		for m := range writers {
			slices.Sort(writers[m])
		}
		writersOnTheFake.writers, writersOnTheFake.shapes = writers, byFamily
	})
	if writersOnTheFake.writers == nil {
		t.Fatal("the per-family sweep of the fake did not complete")
	}
}

// TestEveryFamilyWritesAMeasurementInOneShape: identity.json holds each
// measurement's tag keys over the whole sweep, the union of what every family
// writing it wrote, so one of two families dropping a tag leaves the union as
// it was and passes the gate, while the same item reached by both walks is
// two series. gh_discussion_comment is written by discussions and outbound,
// gh_contribution_day by account and history: measured, removing own from
// outbound's comments, or user from history's days, passed every test of this
// package before this one.
func TestEveryFamilyWritesAMeasurementInOneShape(t *testing.T) {
	shapes := familyShapes(t)
	for _, m := range slices.Sorted(maps.Keys(shapes)) {
		first := ""
		for _, family := range slices.Sorted(maps.Keys(shapes[m])) {
			if first == "" {
				first = family
				continue
			}
			if tags := shapes[m][family]; !slices.Equal(tags, shapes[m][first]) {
				t.Errorf("%s is written by %s with the tags %v and by %s with %v: the same item read by both is "+
					"two series", m, first, shapes[m][first], family, tags)
			}
		}
	}
}

// TestEveryMigrationNamesEveryFamilyThatWritesIt holds each entry's Families
// to what the collectors write. A family missing from the list is a refill
// that leaves that family's rows out, which is exactly what the manual
// migration of 2.6.1 did: it backfilled outbound, and every comment the
// discussions family had written on the account's own forums came back only
// for the ten threads a sweep reads.
func TestEveryMigrationNamesEveryFamilyThatWritesIt(t *testing.T) {
	writers := familyWriters(t)
	for _, problem := range familyMismatches(Registry, writers) {
		t.Error(problem)
	}
}

// TestTheFamilyGateCatchesWhatTheManualRefillMissed is the gate turned on
// the refill that went wrong: an entry naming outbound alone.
func TestTheFamilyGateCatchesWhatTheManualRefillMissed(t *testing.T) {
	writers := familyWriters(t)
	if got := writers["gh_discussion_comment"]; !slices.Equal(got, []string{"discussions", "outbound"}) {
		t.Fatalf("gh_discussion_comment is written on the fake by %v, want discussions and outbound: "+
			"without comment nodes on the fake's threads this gate cannot see the discussions family", got)
	}
	short := []Migration{{
		ID: "2.6.1/gh_discussion_comment/is_answer", Measurement: "gh_discussion_comment",
		Kind: Identity, OldTags: []string{"is_answer"}, Families: []string{"outbound"},
	}}
	problems := familyMismatches(short, writers)
	if len(problems) != 1 || !strings.Contains(problems[0], "discussions") {
		t.Errorf("an entry naming outbound alone gives %v, want one complaint naming discussions", problems)
	}
}

// familyMismatches is every entry whose Families is not exactly the families
// the sweep saw write its measurement, worded for the reader who has to fix
// the entry.
func familyMismatches(registry []Migration, writers map[string][]string) []string {
	var out []string
	for _, m := range registry {
		for _, f := range m.Families {
			if why, left := fakegh.NotCollected[f]; left {
				out = append(out, fmt.Sprintf("%s names %s, which the fake cannot answer (%s), so nothing "+
					"here can say whether it writes %s", m.ID, f, why, m.Measurement))
			}
		}
		seen := writers[m.Measurement]
		if missing := difference(seen, m.Families); len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s leaves out %s, which writes %s: a refill without it "+
				"loses that family's rows", m.ID, strings.Join(missing, ", "), m.Measurement))
		}
		if extra := difference(m.Families, seen); len(extra) > 0 {
			out = append(out, fmt.Sprintf("%s names %s, which does not write %s on the fake: a refill "+
				"of it reads GitHub for nothing", m.ID, strings.Join(extra, ", "), m.Measurement))
		}
	}
	return out
}

// TestEachFamilyAloneWritesWhatItWritesInTheWholeSweep is what makes the
// per-family sweep worth trusting: every measurement the whole sweep wrote
// has at least one writer when the families run one at a time, so no family
// writes only in the company of another.
func TestEachFamilyAloneWritesWhatItWritesInTheWholeSweep(t *testing.T) {
	writers := familyWriters(t)
	for m := range identity() {
		if len(writers[m]) == 0 {
			t.Errorf("%s is in identity.json, yet no family alone writes it: %v", m,
				slices.Sorted(maps.Keys(writers)))
		}
	}
}
