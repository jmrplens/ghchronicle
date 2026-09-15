package config

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// loadGroups writes a minimal config with the given body appended and loads it.
func loadGroups(t *testing.T, body string) *Config {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\n"+body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// loadGroupsErr is loadGroups for the configurations that must not start.
func loadGroupsErr(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "x")
	_, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\n"+body))
	if err == nil {
		t.Fatal("expected this configuration to be refused at start-up")
	}
	return err.Error()
}

// enabled asserts which families a configuration would collect for.
func enabled(t *testing.T, c *Config, on, off []string) {
	t.Helper()
	for _, name := range on {
		if _, ok := c.Interval(name); !ok {
			t.Errorf("%s should be enabled and is not", name)
		}
	}
	for _, name := range off {
		if _, ok := c.Interval(name); ok {
			t.Errorf("%s should be disabled and is not", name)
		}
	}
}

// TestGroupsAbsentCollectsEverything pins the default: no groups key collects
// for every group, and the only families off are the ones whose own cadence is
// zero.
func TestGroupsAbsentCollectsEverything(t *testing.T) {
	c := loadGroups(t, "")
	for _, name := range Families() {
		want := defaultEvery[name].every > 0
		if _, got := c.Interval(name); got != want {
			t.Errorf("%s enabled = %v, want %v", name, got, want)
		}
	}
	// A zero cadence must read as off rather than as "always due", which is
	// what State.Due makes of a zero interval.
	enabled(t, c, []string{"traffic", "repo", "ratelimit"}, []string{"deps", "history", "joblogs"})
	if _, _, narrowed := c.Selection(); narrowed {
		t.Error("a config with no groups key narrows nothing")
	}
}

// TestGroupsNarrowsToTheNamedOnes is the feature itself.
func TestGroupsNarrowsToTheNamedOnes(t *testing.T) {
	c := loadGroups(t, "groups: [audience]\n")
	enabled(t, c,
		[]string{"traffic", "stars", "forks"},
		[]string{"repo", "actions", "issues", "account", "ratelimit"})
	on, off, narrowed := c.Selection()
	if !narrowed {
		t.Error("naming a group narrows the sweep")
	}
	if !slices.Equal(on, []string{"audience"}) {
		t.Errorf("on = %v, want [audience]", on)
	}
	if slices.Contains(off, "audience") || len(off) != len(Groups())-1 {
		t.Errorf("off = %v, want the other seven groups", off)
	}
}

// TestGroupsCannotEnableAFamilyWhoseCadenceIsZero is the rule that lets
// something ship switched off while the default is everything: membership is
// groups, cadence is every, and a family runs only if both say yes.
func TestGroupsCannotEnableAFamilyWhoseCadenceIsZero(t *testing.T) {
	c := loadGroups(t, "groups: [repos, ci]\n")
	enabled(t, c, []string{"repo", "actions"}, []string{"deps", "joblogs"})
}

// TestEveryStillNarrowsInsideASelectedGroup is the other half of the same
// rule: a selected group does not override a cadence of zero.
func TestEveryStillNarrowsInsideASelectedGroup(t *testing.T) {
	c := loadGroups(t, "groups: [audience]\nevery: {families: {forks: 0}}\n")
	enabled(t, c, []string{"traffic", "stars"}, []string{"forks"})
}

func TestUnknownGroupIsFatal(t *testing.T) {
	msg := loadGroupsErr(t, "groups: [audience, sekurity]\n")
	// The index, because a list of eight names with one typo is otherwise a
	// hunt, and the name as typed rather than as normalized.
	for _, want := range []string{"groups[1]", `"sekurity"`, strings.Join(Groups(), ", ")} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// TestAFamilyNameInGroupsIsRefusedWithItsGroup covers the mistake a user will
// actually make: both are lowercase nouns from the same document.
func TestAFamilyNameInGroupsIsRefusedWithItsGroup(t *testing.T) {
	msg := loadGroupsErr(t, "groups: [actions]\n")
	for _, want := range []string{"groups[0]", `"actions"`, "is a family, not a group", `"ci"`, "every.families.actions"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// TestTheHintsNameAKeyTheLoaderAccepts is the guard the two hints were
// missing. Both of them answer a misplaced name by naming the key to write
// instead, and one of them named `every.<family>`, which the loader itself
// refuses: following the advice produced a second error. Rather than pin the
// two strings, this takes the key out of each message and feeds it back in.
func TestTheHintsNameAKeyTheLoaderAccepts(t *testing.T) {
	for _, body := range []string{"groups: [actions]\n", "every: {groups: {actions: 30m}}\n"} {
		msg := loadGroupsErr(t, body)
		// The last match, not the first: a message about every.groups.actions
		// opens by naming where the mistake is and closes by naming the key to
		// write instead, and it is the advice that has to be loadable.
		found := everyKeyRe.FindAllString(msg, -1)
		key := ""
		if len(found) > 0 {
			key = found[len(found)-1]
		}
		if key == "" {
			t.Errorf("%q was refused with %q, which names no every key to write instead", body, msg)
			continue
		}
		t.Setenv("GITHUB_TOKEN", "x")
		if _, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\n"+asYAML(key, "30m"))); err != nil {
			t.Errorf("%q advises %q, which the loader then refuses: %v", body, key, err)
		}
	}
}

// everyKeyRe finds the dotted every key a hint tells the reader to write.
var everyKeyRe = regexp.MustCompile(`every(\.[a-z]+)+`)

// asYAML turns a dotted key into the nested block a reader would type.
func asYAML(key, value string) string {
	parts := strings.Split(key, ".")
	var out strings.Builder
	for i, part := range parts[:len(parts)-1] {
		out.WriteString(strings.Repeat("  ", i) + part + ":\n")
	}
	out.WriteString(strings.Repeat("  ", len(parts)-1) + parts[len(parts)-1] + ": " + value + "\n")
	return out.String()
}

func TestEmptyGroupsListIsFatal(t *testing.T) {
	msg := loadGroupsErr(t, "groups: []\n")
	if !strings.Contains(msg, "omit the key to collect everything") {
		t.Errorf("message %q does not say what to do instead", msg)
	}
	// The distinction the pointer exists for: an empty list and no key at all
	// mean opposite things.
	if c := loadGroups(t, "groups:\n"); c.Groups != nil {
		t.Error("a null groups value must read as no key at all")
	}
}

func TestGroupNamesAreCaseAndSpaceInsensitive(t *testing.T) {
	c := loadGroups(t, `groups: [" Audience "]`+"\n")
	enabled(t, c, []string{"traffic"}, []string{"repo"})
}

// TestEveryOutsideTheSelectionWarnsRatherThanFailing: a user narrowing his
// groups should not also have to prune an every block he tuned last year.
func TestEveryOutsideTheSelectionWarnsRatherThanFailing(t *testing.T) {
	c := loadGroups(t, "groups: [audience, collector]\nevery: {families: {actions: 30m}}\n")
	got := c.Warnings()
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want exactly one", got)
	}
	for _, want := range []string{"every.families.actions", "group ci", "will not run"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not carry %q", got[0], want)
		}
	}
	if _, ok := c.Interval("actions"); ok {
		t.Error("a cadence outside the selection must still not run the family")
	}
}

func TestASelectedGroupWithNothingEnabledWarns(t *testing.T) {
	c := loadGroups(t, "groups: [security, collector]\nevery: {families: {security: 0, analyses: 0}}\n")
	got := c.Warnings()
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "groups names security") || !strings.Contains(got[0], "collects nothing") {
		t.Errorf("warning %q does not name the empty group", got[0])
	}
}

func TestLeavingOutTheCollectorGroupWarns(t *testing.T) {
	c := loadGroups(t, "groups: [audience]\n")
	var found bool
	for _, w := range c.Warnings() {
		if strings.Contains(w, "gh_rate_limit") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one about the rate limit going uncollected", c.Warnings())
	}
	// It is a hint, never a forced inclusion: a switch with a secret exception
	// is worse than a switch.
	if _, ok := c.Interval("ratelimit"); ok {
		t.Error("ratelimit must not be forced into a selection that did not name it")
	}
	if len(loadGroups(t, "groups: [audience, collector]\n").Warnings()) != 0 {
		t.Error("naming collector must silence the hint")
	}
}

// TestEveryFamilyHasAGroup is the forcing function. With the two AST tests in
// families_test.go it means a new collector cannot exist without both a
// cadence and a group.
func TestEveryFamilyHasAGroup(t *testing.T) {
	for _, name := range Families() {
		if defaultEvery[name].group == "" {
			t.Errorf("family %q has no group, so groups can neither select nor exclude it", name)
		}
	}
}

func TestEveryGroupHasADescription(t *testing.T) {
	for _, group := range Groups() {
		if GroupDescription(group) == "" {
			t.Errorf("group %q has no description, so -groups prints a bare name", group)
		}
	}
}

// TestNoDescriptionOutlivesItsGroup is the other direction: prose for a group
// that no family belongs to is a name the documentation offers and nothing
// collects.
func TestNoDescriptionOutlivesItsGroup(t *testing.T) {
	for name := range groupDescriptions {
		if !slices.Contains(Groups(), name) {
			t.Errorf("groupDescriptions carries %q, which no family belongs to", name)
		}
	}
}

// TestAGroupNamedAfterAFamilyContainsIt pins the two deliberate collisions.
// A group named after a family may only ever widen it, never come to mean
// something else.
func TestAGroupNamedAfterAFamilyContainsIt(t *testing.T) {
	for _, group := range Groups() {
		if _, isFamily := GroupOf(group); !isFamily {
			continue
		}
		if !slices.Contains(FamiliesIn(group), group) {
			t.Errorf("group %q shares its name with a family it does not contain, "+
				"so the same word means two unrelated things", group)
		}
	}
}

// TestTheKnownNamesAreSorted keeps a message stable between runs. Ranging a
// map printed the known collectors in a different order every time, which is
// exactly what makes a start-up failure hard to compare against the last one.
func TestTheKnownNamesAreSorted(t *testing.T) {
	if !slices.IsSorted(Families()) {
		t.Errorf("Families() = %v, which is not sorted", Families())
	}
	if !slices.IsSorted(Groups()) {
		t.Errorf("Groups() = %v, which is not sorted", Groups())
	}
	if !slices.IsSorted(FamiliesIn("ci")) {
		t.Errorf(`FamiliesIn("ci") = %v, which is not sorted`, FamiliesIn("ci"))
	}
	msg := loadGroupsErr(t, "every: {families: {nosuchfamily: 1h}}\n")
	_, list, ok := strings.Cut(msg, "(known: ")
	if !ok {
		t.Fatalf("message %q does not list the known collectors", msg)
	}
	names := strings.Split(strings.TrimSuffix(list, ")"), ", ")
	if !slices.IsSorted(names) {
		t.Errorf("the known collectors are listed as %v, which is not sorted", names)
	}
}

// TestAGroupCadenceOutsideTheSelectionWarns is the every.groups half of
// TestEveryOutsideTheSelectionWarnsRatherThanFailing: a cadence for a group
// the selection leaves out sets nothing, and a cadence for a selected group
// is simply in effect and says nothing.
func TestAGroupCadenceOutsideTheSelectionWarns(t *testing.T) {
	outside := loadGroups(t, "groups: [audience, collector]\nevery: {groups: {ci: 30m}}\n").Warnings()
	if len(outside) != 1 || !strings.Contains(outside[0], "every.groups.ci sets a cadence for a group that groups does not select") {
		t.Errorf("warnings = %v, want exactly one about every.groups.ci", outside)
	}
	c := loadGroups(t, "groups: [ci, collector]\nevery: {groups: {ci: 30m}}\n")
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("a cadence for a selected group must not warn, got %v", got)
	}
	if got := mustInterval(t, c, "artifacts"); got != 30*time.Minute {
		t.Errorf("artifacts = %v, want the 30m of every.groups.ci", got)
	}
}
