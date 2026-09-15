package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	// Checked, because a close that fails is a config file that never landed
	// and a test that would then read a truncated one.
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestLoadExpandsEnvAndFillsDefaults(t *testing.T) {
	t.Setenv("TEST_GH_TOKEN", "ghp_secret")
	c, err := Load(write(t, `
github:
  token: ${TEST_GH_TOKEN}
targets:
  user: jmrplens
sinks:
  stdout: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.Token != "ghp_secret" {
		t.Errorf("token not expanded from the environment: %q", c.GitHub.Token)
	}
	if got, ok := c.Interval("traffic"); !ok || got != 6*time.Hour {
		t.Errorf("traffic default = %v, %v", got, ok)
	}
	if c.GitHub.ReserveRate == 0 {
		t.Error("reserve_rate should default to something non-zero")
	}
}

func TestZeroIntervalDisablesACollector(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
every:
  families:
    stats: 0s
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Interval("stats"); ok {
		t.Error("an interval of 0 must disable the collector, not run it constantly")
	}
	if _, ok := c.Interval("traffic"); !ok {
		t.Error("disabling one collector must not disable the others")
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	// A typo that silently does nothing is worse than a startup failure.
	if _, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
every: {families: {nosuchfamily: 1h}}
`)); err == nil {
		t.Fatal("expected a typo in a collector name to fail")
	}
	// The layer names are fields of a struct, so the strict decoder refuses a
	// family written where a layer goes without this package having to look.
	if _, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
every: {default: 15m, families: {traffic: 6h}, nosuchlayer: 1h}
`)); err == nil {
		t.Fatal("expected an unknown layer in every to fail")
	}
}

func TestNoSinkIsAnError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	if _, err := Load(write(t, "targets: {user: jmrplens}\n")); err == nil {
		t.Fatal("a run with nowhere to write should not start")
	}
}

func TestParseSinceAcceptsWhatPeopleType(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":           {},
		"unlimited":  {},
		"all":        {},
		"0":          {},
		"2024-01-01": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		"90d":        now.AddDate(0, 0, -90),
		"2y":         now.AddDate(-2, 0, 0),
		"720h":       now.Add(-720 * time.Hour),
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
	if _, err := parseSince("last tuesday", now); err == nil {
		t.Error("nonsense must be rejected, not silently unbounded")
	}
}

// TestABareNumberIsNotACountOfDaysOrYears keeps the two suffixes Go's parser
// does not know from swallowing input they were never written for. "90" with
// no unit is ambiguous, and reading it as ninety days or ninety years would
// turn a typo into a backfill of a lifetime; a suffix with more after it is a
// mixed duration Go refuses, and so must this.
func TestABareNumberIsNotACountOfDaysOrYears(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{"90", "halfd", "1y2d", "twoy"} {
		if got, err := parseSince(in, now); err == nil {
			t.Errorf("%q resolved to %v, want it refused as not a date, a duration or a count", in, got)
		}
	}
}

// TestAMissingTokenIsRefusedAtStartUp: every request this tool makes needs
// one, so a config that has none must fail before the first sweep rather
// than on it, and the message names both places a token can come from.
func TestAMissingTokenIsRefusedAtStartUp(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	_, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "github.token is empty and GITHUB_TOKEN is unset") {
		t.Fatalf("err = %v, want it to name github.token and GITHUB_TOKEN", err)
	}
}

// TestAnInlineTokenWinsOverTheEnvironment: the environment is the fallback,
// so a token written in the file is the one used.
func TestAnInlineTokenWinsOverTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-env")
	c, err := Load(write(t, "github: {token: inline}\ntargets: {user: jmrplens}\nsinks: {stdout: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.Token != "inline" {
		t.Errorf("token = %q, want the inline one", c.GitHub.Token)
	}
}

// TestAnyOneKindOfTargetIsEnough: an organization or a list of repositories
// is a complete answer to "what to collect" without a user, and naming none
// of the three is the one config with nothing to sweep.
func TestAnyOneKindOfTargetIsEnough(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	for _, targets := range []string{"{user: jmrplens}", "{orgs: [acme]}", "{repos: [acme/tool]}"} {
		if _, err := Load(write(t, "targets: "+targets+"\nsinks: {stdout: true}\n")); err != nil {
			t.Errorf("targets %s: %v", targets, err)
		}
	}
	_, err := Load(write(t, "targets: {}\nsinks: {stdout: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "set at least one of user, orgs or repos") {
		t.Errorf("no target at all: err = %v, want it to say what to set", err)
	}
}

// TestTheLedgerLivesBesideTheStateFile pins where a sweep remembers itself.
// The ledger follows a state file that was moved, and neither default may
// overwrite a path the config chose.
func TestTheLedgerLivesBesideTheStateFile(t *testing.T) {
	cases := []struct{ body, state, dedupe string }{
		{"", "ghchronicle-state.json", "ghchronicle-state-written.bin"},
		{"state_file: /var/lib/g/state.json\n", "/var/lib/g/state.json", "/var/lib/g/state-written.bin"},
		{"state_file: s.json\nsinks: {stdout: true, dedupe_file: \"off\"}\n", "s.json", "off"},
	}
	for _, tc := range cases {
		t.Setenv("GITHUB_TOKEN", "x")
		body := "targets: {user: jmrplens}\n" + tc.body
		if !strings.Contains(tc.body, "sinks:") {
			body += "sinks: {stdout: true}\n"
		}
		c, err := Load(write(t, body))
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if c.StateFile != tc.state || c.Sinks.DedupeFile != tc.dedupe {
			t.Errorf("%q: state_file = %q, dedupe_file = %q, want %q and %q",
				tc.body, c.StateFile, c.Sinks.DedupeFile, tc.state, tc.dedupe)
		}
	}
}

// TestAConfigFileThatCannotBeReadSaysSo: a wrong path is the commonest
// start-up failure there is, and it must not read as an empty config.
func TestAConfigFileThatCannotBeReadSaysSo(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to report the file does not exist", err)
	}
}

// TestMalformedYAMLIsReportedAsItselfWithItsPath: the migration hint answers
// only a file that is valid YAML in the old shape, so a file that is not YAML
// at all gets the parser's own words, prefixed with the file they are about.
func TestMalformedYAMLIsReportedAsItselfWithItsPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	path := write(t, "every: [traffic: 6h\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected malformed YAML to be refused")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, path+": ") || strings.Contains(msg, "three layers") {
		t.Errorf("err = %q, want the path and the parser's error, not the migration hint", msg)
	}
}
