package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// loadEvery writes a minimal config with the given every block and loads it.
func loadEvery(t *testing.T, body string) *Config {
	t.Helper()
	return loadGroups(t, body)
}

// mustInterval is the resolved cadence of a family that is expected to run.
func mustInterval(t *testing.T, c *Config, name string) time.Duration {
	t.Helper()
	d, ok := c.Interval(name)
	if !ok {
		t.Fatalf("%s should be enabled and is not", name)
	}
	return d
}

// TestPrecedenceIsMostSpecificWins is the whole of the three layers: a
// family's own entry beats its group's, a group's beats default, and default
// beats the built-in table.
func TestPrecedenceIsMostSpecificWins(t *testing.T) {
	c := loadEvery(t, `
every:
  default: 45m
  groups:
    ci: 5m
    feeds: 10m
  families:
    actions: 30s
`)
	cases := map[string]time.Duration{
		"actions":   30 * time.Second, // its own entry, over its group and over default
		"artifacts": 5 * time.Minute,  // its group, ci, over default
		"events":    10 * time.Minute, // its group, feeds
		"traffic":   45 * time.Minute, // default, over a built-in 6h
		"keys":      45 * time.Minute, // default, over a built-in 24h
	}
	for name, want := range cases {
		if got := mustInterval(t, c, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// And with no layer at all, the built-in value survives untouched.
	if got := mustInterval(t, loadEvery(t, ""), "keys"); got != 24*time.Hour {
		t.Errorf("keys with no every block = %v, want 24h", got)
	}
}

// TestTheBroadLayersNeverResurrectAFamilyThatShipsOff is the rule that keeps a
// default written for the fast families from switching on the expensive ones.
// deps alone is 1.8 MB of SBOM per repository.
func TestTheBroadLayersNeverResurrectAFamilyThatShipsOff(t *testing.T) {
	c := loadEvery(t, "every: {default: 15m, groups: {repos: 15m, ci: 15m, account: 15m}}\n")
	for _, name := range []string{"deps", "history", "joblogs"} {
		if d, ok := c.Interval(name); ok {
			t.Errorf("%s was switched on at %v by a layer that never named it", name, d)
		}
	}
	// Naming it is still the way to turn one on, and the only way.
	on := loadEvery(t, "every: {default: 15m, families: {deps: 12h}}\n")
	if got := mustInterval(t, on, "deps"); got != 12*time.Hour {
		t.Errorf("deps named under families = %v, want 12h", got)
	}
}

// TestDefaultZeroCollectsOnlyWhatIsNamed is the idiom the default layer buys:
// switch everything off, then name the families to keep.
func TestDefaultZeroCollectsOnlyWhatIsNamed(t *testing.T) {
	c := loadEvery(t, "every: {default: 0, families: {traffic: 1h}}\n")
	if got := mustInterval(t, c, "traffic"); got != time.Hour {
		t.Errorf("traffic = %v, want 1h", got)
	}
	for _, name := range []string{"repo", "actions", "ratelimit", "keys"} {
		if _, ok := c.Interval(name); ok {
			t.Errorf("%s should be off under a default of zero", name)
		}
	}
}

// TestTheCollidingNamesAreExactlyTwo checks the claim the nesting is justified
// by, against defaultEvery rather than against prose: security and account are
// each both a family and a group, and nothing else is.
//
// A thirty-third family called ci or feeds would be a name that means two
// things in one document, and this is where that gets noticed.
func TestTheCollidingNamesAreExactlyTwo(t *testing.T) {
	var both []string
	for _, name := range Families() {
		if slices.Contains(Groups(), name) {
			both = append(both, name)
		}
	}
	if want := []string{"account", "security"}; !slices.Equal(both, want) {
		t.Errorf("the names that are both a family and a group are %v, the documentation says %v", both, want)
	}
}

// TestACollidingNameIsUnambiguousInEachLayer is the same claim as behavior:
// the one word reads as a family under families and as a group under groups,
// and the two answers differ.
func TestACollidingNameIsUnambiguousInEachLayer(t *testing.T) {
	byFamily := loadEvery(t, "every: {families: {security: 4h}}\n")
	if got := mustInterval(t, byFamily, "security"); got != 4*time.Hour {
		t.Errorf("every.families.security = %v, want 4h", got)
	}
	// analyses is the other member of group security and must be untouched.
	if got := mustInterval(t, byFamily, "analyses"); got != 6*time.Hour {
		t.Errorf("every.families.security moved analyses to %v, want its built-in 6h", got)
	}
	byGroup := loadEvery(t, "every: {groups: {security: 4h}}\n")
	for _, name := range []string{"security", "analyses"} {
		if got := mustInterval(t, byGroup, name); got != 4*time.Hour {
			t.Errorf("every.groups.security left %s at %v, want 4h", name, got)
		}
	}
}

// TestAGroupNameUnderFamiliesPointsAtTheOtherKey covers the mistake the two
// maps make possible and the flat map made unanswerable.
func TestAGroupNameUnderFamiliesPointsAtTheOtherKey(t *testing.T) {
	msg := loadGroupsErr(t, "every: {families: {feeds: 1m}}\n")
	for _, want := range []string{"every.families.feeds", "is a group, not a family", "every.groups.feeds"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

func TestAFamilyNameUnderGroupsPointsAtTheOtherKey(t *testing.T) {
	msg := loadGroupsErr(t, "every: {groups: {actions: 1m}}\n")
	for _, want := range []string{"every.groups.actions", "is a family, not a group", `"ci"`, "every.families.actions"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

func TestAnUnparseableDurationNamesItsLayer(t *testing.T) {
	for body, want := range map[string]string{
		"every: {default: soon}\n":             "every.default",
		"every: {groups: {ci: soon}}\n":        "every.groups.ci",
		"every: {families: {actions: soon}}\n": "every.families.actions",
	} {
		if msg := loadGroupsErr(t, body); !strings.Contains(msg, want) {
			t.Errorf("%q reported %q, which does not name %q", body, msg, want)
		}
	}
}

// TestASubstantiallyFasterCadenceWarns is the warning the whole layer needs:
// a default chosen for the fast families lands on the slow ones too, and the
// reason a cadence is what it is has to come out with it.
func TestASubstantiallyFasterCadenceWarns(t *testing.T) {
	c := loadEvery(t, "every: {default: 15m}\n")
	var keys string
	for _, w := range c.Warnings() {
		if strings.HasPrefix(w, "every.default sets keys to ") {
			keys = w
		}
	}
	if keys == "" {
		t.Fatalf("a 15m default against a 24h built-in must warn about keys; warnings were %v", c.Warnings())
	}
	for _, want := range []string{"every.default", "15m", "24h", "96 times more often", Why("keys")} {
		if !strings.Contains(keys, want) {
			t.Errorf("warning %q does not carry %q", keys, want)
		}
	}
	// Every slow family, not just the first one found: the reason is per
	// family, so the warning has to be too.
	for _, name := range []string{"traffic", "billing", "stats", "branches", "policyfiles"} {
		if !slices.ContainsFunc(c.Warnings(), func(w string) bool { return strings.Contains(w, "sets "+name+" to ") }) {
			t.Errorf("no warning names %s, whose built-in %v is far past a 15m default", name, defaultEvery[name].every)
		}
	}
	// And nothing at or near its built-in value is mentioned.
	for _, name := range []string{"actions", "ratelimit", "events", "notifs", "activity"} {
		if slices.ContainsFunc(c.Warnings(), func(w string) bool { return strings.Contains(w, "sets "+name+" to ") }) {
			t.Errorf("%s runs at or near its built-in cadence and must not warn", name)
		}
	}
}

// TestAGroupCadenceWarnsPerFamilyAndNeverPerGroup: a warning about a group is
// useless, because the reason lives on the family.
func TestAGroupCadenceWarnsPerFamilyAndNeverPerGroup(t *testing.T) {
	// Group work holds families measured at 1h and at 12h, so one number for
	// the group flattens six different values.
	c := loadEvery(t, "every: {groups: {work: 1h}}\n")
	var named []string
	for _, w := range c.Warnings() {
		for _, name := range FamiliesIn("work") {
			if strings.Contains(w, "sets "+name+" to ") {
				named = append(named, name)
			}
		}
		if !strings.HasPrefix(w, "every.groups.work sets ") {
			t.Errorf("warning %q does not point at the key that set the cadence", w)
		}
	}
	// planning is 6h and stats is 12h against the group's 1h. issues and
	// commits are 1h already and discussions is 2h, so all three stay out.
	slices.Sort(named)
	if want := []string{"planning", "stats"}; !slices.Equal(named, want) {
		t.Errorf("warned about %v, want %v", named, want)
	}
}

// TestOneRungDownTheLadderDoesNotWarn defends the factor of four. The built-in
// values step 15m 30m 1h 2h 6h 12h 24h, and the widest single step is three,
// so a deliberate one-rung adjustment stays quiet.
func TestOneRungDownTheLadderDoesNotWarn(t *testing.T) {
	// traffic 6h to 2h is a factor of three, the widest step there is.
	if got := loadEvery(t, "every: {families: {traffic: 2h}}\n").Warnings(); len(got) != 0 {
		t.Errorf("a threefold speed-up must not warn, got %v", got)
	}
	// Four is where it starts, and 6h to 90m is exactly four.
	if got := loadEvery(t, "every: {families: {traffic: 90m}}\n").Warnings(); len(got) != 1 {
		t.Errorf("a fourfold speed-up must warn exactly once, got %v", got)
	}
}

// TestAnExplicitFamilyCadenceStillWarns: being deliberate about keys does not
// make one minute a sensible interval for a value that does not move, and the
// message names the key that set it so the reader knows which knob is his.
func TestAnExplicitFamilyCadenceStillWarns(t *testing.T) {
	got := loadEvery(t, "every: {families: {keys: 1m}}\n").Warnings()
	if len(got) != 1 || !strings.HasPrefix(got[0], "every.families.keys sets keys to 1m against a built-in 24h") {
		t.Fatalf("warnings = %v, want one naming every.families.keys", got)
	}
}

// TestSlowingAFamilyDownNeverWarns: this warns about waste, never about taste.
func TestSlowingAFamilyDownNeverWarns(t *testing.T) {
	if got := loadEvery(t, "every: {default: 24h}\n").Warnings(); len(got) != 0 {
		t.Errorf("lengthening every cadence must not warn, got %v", got)
	}
}

func TestAConfigThatCollectsNothingWarns(t *testing.T) {
	got := loadEvery(t, "every: {default: 0}\n").Warnings()
	if len(got) != 1 || !strings.Contains(got[0], "collects nothing") {
		t.Errorf("warnings = %v, want one saying the run collects nothing", got)
	}
}

// TestTheHeartbeatIsNotACadence: it sets no family's interval and is measured
// against nothing in the built-in table.
func TestTheHeartbeatIsNotACadence(t *testing.T) {
	c := loadEvery(t, "heartbeat: 200ms\n")
	if d, ok := c.HeartbeatEvery(); !ok || d != 200*time.Millisecond {
		t.Errorf("HeartbeatEvery() = %v, %v, want 200ms, true", d, ok)
	}
	for _, name := range Families() {
		want, enabled := BuiltinEvery(name)
		if got, ok := c.Interval(name); ok != (enabled && want > 0) || (ok && got != want) {
			t.Errorf("heartbeat moved %s to %v, want its built-in %v", name, got, want)
		}
	}
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("a heartbeat faster than every cadence has nothing to say, got %v", got)
	}
	if _, ok := loadEvery(t, "").HeartbeatEvery(); ok {
		t.Error("no heartbeat key must leave the runner to derive the tick")
	}
}

// TestAHeartbeatSlowerThanTheFastestCadenceWarns is the quiet half of the same
// mistake: nothing is misconfigured, and yet actions cannot run every 15m.
func TestAHeartbeatSlowerThanTheFastestCadenceWarns(t *testing.T) {
	got := loadEvery(t, "heartbeat: 1h\n").Warnings()
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want exactly one", got)
	}
	for _, want := range []string{"heartbeat is 1h", "shortest cadence is 15m", "actions"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not carry %q", got[0], want)
		}
	}
}

func TestAHeartbeatOfZeroIsRefused(t *testing.T) {
	msg := loadGroupsErr(t, "heartbeat: 0\n")
	if !strings.Contains(msg, "heartbeat") || !strings.Contains(msg, "omit the key") {
		t.Errorf("message %q does not say what to do instead", msg)
	}
	msg = loadGroupsErr(t, "heartbeat: soon\n")
	if !strings.Contains(msg, "heartbeat") {
		t.Errorf("message %q does not name the key", msg)
	}
}

// TestAFlatEveryBlockSaysHowToMigrate: the shape changed under every config
// file that exists, so the reader of an older one gets told what moved rather
// than "field traffic not found in type config.Every".
func TestAFlatEveryBlockSaysHowToMigrate(t *testing.T) {
	msg := loadGroupsErr(t, "every: {traffic: 6h, actions: 15m}\n")
	for _, want := range []string{"three layers", "every.families", "actions, traffic"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// TestEveryFamilyCarriesItsReason is the forcing function for the warning and
// for the documented table alike: a family with no why warns with a dangling
// colon and documents itself as blank.
func TestEveryFamilyCarriesItsReason(t *testing.T) {
	for _, name := range Families() {
		why := Why(name)
		if why == "" {
			t.Errorf("family %q has no why, so its warning ends in a colon and its documented row is blank", name)
			continue
		}
		if strings.Contains(why, "—") {
			t.Errorf("family %q: no em dash characters anywhere in this repository", name)
		}
		if strings.HasSuffix(why, ".") {
			// It is quoted mid-sentence after a colon, so a full stop there
			// reads as the end of the warning when it is not.
			t.Errorf("family %q: why is a clause, not a sentence, so it carries no full stop", name)
		}
	}
}

// TestCompactQuotesADurationInTheUnitsAConfigFileWrites: a warning quotes a
// cadence back at the reader, so 24h reads as 24h and not as 24h0m0s, and a
// value that is not a whole number of either unit keeps Go's own spelling
// rather than being rounded into a number he never typed.
func TestCompactQuotesADurationInTheUnitsAConfigFileWrites(t *testing.T) {
	cases := map[time.Duration]string{
		0:                      "0s",
		-time.Minute:           "-1m0s",
		15 * time.Minute:       "15m",
		90 * time.Minute:       "90m",
		24 * time.Hour:         "24h",
		90 * time.Second:       "1m30s",
		200 * time.Millisecond: "200ms",
	}
	for d, want := range cases {
		if got := compact(d); got != want {
			t.Errorf("compact(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestAnAbsurdlySlowCadenceNeverWarnsAsTooFast is the overflow the order of
// the comparison guards against: a cadence of centuries times four wraps
// int64 to a negative number, and a check that multiplied first would call it
// too fast and report it as zero times more often.
func TestAnAbsurdlySlowCadenceNeverWarnsAsTooFast(t *testing.T) {
	if got := loadEvery(t, "every: {families: {keys: 2500000h}}\n").Warnings(); len(got) != 0 {
		t.Errorf("a cadence of centuries must not warn, got %v", got)
	}
}

// TestAHeartbeatEqualToTheFastestCadenceDoesNotWarn: a tick exactly as long
// as the shortest cadence holds nothing back, so only a slower one is worth a
// line.
func TestAHeartbeatEqualToTheFastestCadenceDoesNotWarn(t *testing.T) {
	if got := loadEvery(t, "heartbeat: 15m\n").Warnings(); len(got) != 0 {
		t.Errorf("a heartbeat equal to the 15m of actions must not warn, got %v", got)
	}
}

// TestAHeartbeatOverAnEmptyScheduleSaysOnlyThatItIsEmpty: with nothing
// collected there is no fastest cadence to compare against, and the one
// warning worth giving is that the run collects nothing.
func TestAHeartbeatOverAnEmptyScheduleSaysOnlyThatItIsEmpty(t *testing.T) {
	got := loadEvery(t, "heartbeat: 1h\nevery: {default: 0}\n").Warnings()
	if len(got) != 1 || !strings.Contains(got[0], "collects nothing") {
		t.Errorf("warnings = %v, want exactly the one saying the run collects nothing", got)
	}
}

// TestAnUnknownNameUnderGroupsListsTheGroups: a name that is neither a group
// nor a family has no other key to point at, so the message lists the groups
// there are.
func TestAnUnknownNameUnderGroupsListsTheGroups(t *testing.T) {
	msg := loadGroupsErr(t, "every: {groups: {sekurity: 1h}}\n")
	for _, want := range []string{"every.groups.sekurity", "unknown group", strings.Join(Groups(), ", ")} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}
