//go:build dockere2e && !race

// harness_norace_test.go is the ordinary half of the harness build seam, used
// by every run that is not `go test -race`. See harness_race_test.go for the
// other half and for why the seam exists.
package docker

import "time"

// collectorBuildArgs returns the `go build` arguments for the collector under
// test.
func collectorBuildArgs(out string) []string {
	return []string{"build", "-o", out, "github.com/jmrplens/ghchronicle/cmd/ghchronicle"}
}

// collectorBuildTimeout bounds that build.
const collectorBuildTimeout = 5 * time.Minute

// raceEnviron adds nothing to the collector's environment when the detector
// is not in play.
func raceEnviron() []string {
	return nil
}
