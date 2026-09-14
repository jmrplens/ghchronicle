package config

import (
	"testing"
	"time"
)

// TestPrivateRepositoriesAreCollectedUnlessRefused pins the default the
// documentation has always claimed. A bare `bool` made the absent key mean
// false, so an account whose repositories are mostly private collected a
// fraction of itself and nothing said so: measured with the same token,
// `-list` printed 27 repositories without the key and 35 with it.
func TestPrivateRepositoriesAreCollectedUnlessRefused(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	cases := map[string]bool{
		"":                           true,
		"  include_private: true\n":  true,
		"  include_private: false\n": false,
	}
	for body, want := range cases {
		c, err := Load(write(t, "targets:\n  user: jmrplens\n"+body+"sinks: {stdout: true}\n"))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if got := c.Targets.PrivateIncluded(); got != want {
			t.Errorf("with %q, private repositories collected = %v, want %v", body, got, want)
		}
	}
}

// TestLokiMaxAgeLeavesItsDefaultToTheSink is the other half of the same
// defect: the config layer answered seven days for an absent key, which is
// well outside Loki's out-of-order window and made the sink's own one hour
// unreachable. Zero is the config layer saying "not set", which is the only
// answer that leaves one owner of the default.
func TestLokiMaxAgeLeavesItsDefaultToTheSink(t *testing.T) {
	cases := map[string]time.Duration{
		"":       0,
		"2h":     2 * time.Hour,
		"0s":     0,
		"-1h":    0,
		"sunday": 0,
	}
	for raw, want := range cases {
		if got := (&LokiSink{MaxAge: raw}).Age(); got != want {
			t.Errorf("max_age %q resolves to %v, want %v", raw, got, want)
		}
	}
}
