package config

import (
	"os"
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
