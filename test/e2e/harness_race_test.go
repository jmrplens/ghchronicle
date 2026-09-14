//go:build race

// harness_race_test.go is the race-detector half of the harness build seam. The
// go tool sets the `race` build tag when -race is used, so this file is what is
// compiled by `go test -race ./test/e2e/` and harness_norace_test.go is what is
// compiled otherwise.
package e2e

import "time"

// collectorBuildArgs returns the `go build` arguments for the collector under
// test, with the detector on.
//
// This seam exists because `go test -race` instruments the test binary and
// nothing else. The collector is a separate process built by TestMain, so
// without passing the flag on, a race run would watch the harness's and the
// fake GitHub's goroutines and say nothing about the collector's, which are
// the ones this suite exists to reach: the scheduler, the sinks' repeat loops
// and the exporter serving while a sweep writes can only be driven as a
// process.
func collectorBuildArgs(out string) []string {
	return []string{"build", "-race", "-o", out, "github.com/jmrplens/ghchronicle/cmd/ghchronicle"}
}

// collectorBuildTimeout bounds that build. A race build shares no object cache
// with an ordinary one, so it is a cold build of the whole dependency tree even
// on a machine that has just compiled these tests.
const collectorBuildTimeout = 15 * time.Minute

// raceEnviron is the extra environment an instrumented collector is started
// with.
//
// Without halt_on_error the race runtime prints its report to stderr and lets
// the process continue, and only a clean exit turns into the detector's
// status. A collector in serve mode would go on serving and writing past the
// race until bgProcess.Stop ends it, and Stop reads no exit status at all.
// Halting ends the collector at the first report, so the run stops where the
// race happened with the report as the last thing it printed, and
// racereport.Check fails the test with it.
func raceEnviron() []string {
	return []string{"GORACE=halt_on_error=1"}
}
