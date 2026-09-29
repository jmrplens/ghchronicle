package migrate

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// TestTheRegistryHoldsTogether is every rule an entry has to keep that the
// code, rather than the fake GitHub, can check.
func TestTheRegistryHoldsTogether(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	previous := Migration{}
	for i, m := range Registry {
		if seen[m.ID] {
			t.Errorf("%s is registered twice; an ID in a state file has to mean one thing", m.ID)
		}
		seen[m.ID] = true
		if want := m.Release + "/" + m.Measurement + "/"; !strings.HasPrefix(m.ID, want) {
			t.Errorf("%s does not start with %s, the release and the measurement it is about", m.ID, want)
		}
		if _, ok := parseRelease(m.Release); !ok {
			t.Errorf("%s: release %q is not a release number", m.ID, m.Release)
		}
		if compareRelease(m.Release, ghchronicle.Version) > 0 {
			t.Errorf("%s: release %s is newer than this build's %s, so a store this build first wrote "+
				"would read as holding the old shape", m.ID, m.Release, ghchronicle.Version)
		}
		if i > 0 && (compareRelease(m.Release, previous.Release) < 0 ||
			compareRelease(m.Release, previous.Release) == 0 && m.ID < previous.ID) {
			t.Errorf("%s comes after %s; the registry is ordered by release, then by ID", m.ID, previous.ID)
		}
		previous = m
		checkEntry(t, m)
	}
}

// checkEntry is the rules that concern one entry alone.
func checkEntry(t *testing.T, m Migration) {
	t.Helper()
	if m.Why == "" || m.Account == "" || m.Kind == 0 || m.Reach == 0 {
		t.Errorf("%s leaves out its why, its account tag, its kind or its reach", m.ID)
	}
	if m.Kind == Identity && len(m.OldTags) == 0 {
		t.Errorf("%s changes identity and names no old tag, so no store can be asked for it", m.ID)
	}
	if m.Kind == Value && len(m.OldTags) > 0 {
		t.Errorf("%s changes a value and names old tags, which only an identity change has", m.ID)
	}
	if len(m.Families) == 0 {
		t.Errorf("%s names no family, so nothing could read it again", m.ID)
	}
	for _, f := range m.Families {
		if !slices.Contains(config.Families(), f) {
			t.Errorf("%s names %q, which is not a family", m.ID, f)
		}
	}
	if !slices.IsSorted(m.Families) {
		t.Errorf("%s: families %v are not sorted, which the refill's checkpoint compares", m.ID, m.Families)
	}
	tags, written := Tags(m.Measurement)
	if !written {
		t.Errorf("%s is about %s, which a sweep of the fake does not write", m.ID, m.Measurement)
		return
	}
	for _, old := range m.OldTags {
		if slices.Contains(tags, old) {
			t.Errorf("%s says %s is a tag of the old shape only, yet this release still writes it", m.ID, old)
		}
	}
	if !slices.Contains(tags, m.Account) {
		t.Errorf("%s: account tag %q is not a tag of %s", m.ID, m.Account, m.Measurement)
	}
	checkAuthor(t, m, tags)
	if m.noteOnly() != (len(m.Item) == 0) {
		t.Errorf("%s: item %v; a change that is read back names the tags of one item, and only such a change",
			m.ID, m.Item)
	}
	for _, tag := range m.Item {
		if !slices.Contains(tags, tag) {
			t.Errorf("%s: item tag %q is not a tag of %s, so the table read back cannot be asked for it",
				m.ID, tag, m.Measurement)
		}
	}
}

// checkAuthor holds an entry's Author to its families: named exactly when an
// account-wide family writes the measurement beside a per-repository one, and
// a tag of the measurement.
func checkAuthor(t *testing.T, m Migration, tags []string) {
	t.Helper()
	mixed := slices.ContainsFunc(m.Families, run.PerRepository) &&
		slices.ContainsFunc(m.Families, func(f string) bool { return !run.PerRepository(f) })
	if mixed != (m.Author != "") {
		t.Errorf("%s: author %q; a measurement an account-wide family writes beside a per-repository one "+
			"names the tag that says who wrote a row, and only such a measurement", m.ID, m.Author)
	}
	if m.Author != "" && !slices.Contains(tags, m.Author) {
		t.Errorf("%s: author tag %q is not a tag of %s", m.ID, m.Author, m.Measurement)
	}
}

func TestReleasesCompareAsNumbers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"2.6.1", "2.6.1", 0},
		{"2.6.10", "2.6.9", 1},
		{"v2.6.2", "2.6.1", 1},
		{"2.6.2-rc.1", "2.6.2", 0},
		{"1.0.0", "2.6.1", -1},
		{"", "1.0.0", -1},
		{"dev", "1.0.0", -1},
		{"2.6", "2.6.0", -1},
	}
	for _, c := range cases {
		if got := compareRelease(c.a, c.b); got != c.want {
			t.Errorf("compareRelease(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
