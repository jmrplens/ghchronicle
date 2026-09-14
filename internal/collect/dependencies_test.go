package collect

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// hourlyBudget is any way of writing the budget the SBOM endpoint does not
// have. Measured on 2026-09-12 and pinned by
// TestRateLimitPrefersTheHeadersOfABucketTheEndpointInvents: dependency_sbom
// is a hundred requests in a window of sixty seconds.
var hourlyBudget = regexp.MustCompile(`hundred[ -]an?[ -]hour`)

// TestTheSBOMBudgetIsDescribedByTheMinuteItIs holds the two files that write
// the number down to the measurement.
//
// The comments in dependencies.go had it as "its own hundred-an-hour bucket"
// while the cadence in config.go said a minute, so the same repository's
// dependency graph was sixty times cheaper or sixty times dearer depending on
// which file a reader opened, and three documents were written from the wrong
// one.
func TestTheSBOMBudgetIsDescribedByTheMinuteItIs(t *testing.T) {
	for _, path := range []string{
		"dependencies.go",
		filepath.Join("..", "config", "config.go"),
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		if loc := hourlyBudget.FindString(string(b)); loc != "" {
			t.Errorf("%s calls the dependency_sbom budget %q; its window is a minute", path, loc)
		}
	}
}
