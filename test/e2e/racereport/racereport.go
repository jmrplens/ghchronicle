// Package racereport reads the race detector's report out of what a collector
// under test printed, for both end to end suites.
//
// `go test -race` instruments the test binary and nothing else. The suites
// build the collector as a separate process, and pass the detector on to it
// through a build-tag seam (harness_race_test.go in each suite), but the
// collector's report then lands in that process's output rather than in the
// test's. Which tests would notice depends on what each one asserts: a test
// that expects the run to fail, or one that has made its last assertion
// before the process is stopped, would pass with the report sitting in a
// buffer nobody prints. Reading every output for the report is what makes a
// race fail the suite whatever the test was looking at.
//
// It is a package of its own for the reason test/e2e/fakegh is: the two
// suites must not come to disagree about what a report looks like.
package racereport

import (
	"fmt"
	"strings"
	"testing"
)

// marker opens every report the race runtime writes, and nothing else writes
// it: the collector's own log has no reason to.
const marker = "WARNING: DATA RACE"

// rule is the line of equals signs the runtime prints above and below each
// report, which is what delimits one from the log lines around it.
const rule = "=================="

// Find returns the first report in output, from the rule above it to the rule
// below it, and how many reports output holds.
//
// Under GORACE=halt_on_error=1, which the race seam sets, there is only ever
// one, because the process stops at it. A caller who overrode GORACE can see
// more, and the count keeps the others from going unmentioned.
func Find(output string) (report string, count int) {
	at := strings.Index(output, marker)
	if at < 0 {
		return "", 0
	}
	start := strings.LastIndex(output[:at], rule)
	if start < 0 {
		start = at
	}
	end := len(output)
	if closing := strings.Index(output[at:], rule); closing >= 0 {
		end = at + closing + len(rule)
	}
	return output[start:end], strings.Count(output, marker)
}

// Check fails tb when output holds a report, and puts the report in the
// failure, since its stacks are the only record of where the race was.
func Check(tb testing.TB, output string) {
	tb.Helper()
	report, count := Find(output)
	if count == 0 {
		return
	}
	var more string
	if count > 1 {
		more = fmt.Sprintf(" (%d reports; the first follows)", count)
	}
	tb.Errorf("the collector hit a data race%s:\n%s", more, report)
}
