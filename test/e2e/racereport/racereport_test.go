package racereport

import (
	"fmt"
	"strings"
	"testing"
)

// report is the shape the race runtime prints, taken from a real one: a rule,
// the marker, the two accesses with their stacks, the goroutines' origins and
// a closing rule.
const report = `==================
WARNING: DATA RACE
Read at 0x00c0001a4030 by goroutine 42:
  github.com/jmrplens/ghchronicle/v2/internal/sink.(*Prom).handle()
      /src/internal/sink/prom.go:114 +0x7c

Previous write at 0x00c0001a4030 by goroutine 18:
  github.com/jmrplens/ghchronicle/v2/internal/sink.(*Prom).Write()
      /src/internal/sink/prom.go:98 +0x2d4

Goroutine 42 (running) created at:
  net/http.(*Server).Serve()
      /usr/local/go/src/net/http/server.go:3454 +0x8c4
==================`

// logLine stands for the collector's own log, which shares standard error with
// the report and so surrounds it.
const logLine = `time=2026-09-11T10:00:00Z level=INFO msg="sweep finished" families=31`

func TestFindReturnsTheReportBetweenItsRules(t *testing.T) {
	t.Parallel()
	got, count := Find(logLine + "\n" + report + "\n" + logLine + "\n")
	if got != report {
		t.Errorf("Find returned\n%s\nwant\n%s", got, report)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestFindReportsNothingInACleanLog(t *testing.T) {
	t.Parallel()
	// A rule on its own is not a report: something else may print one.
	got, count := Find(logLine + "\n==================\n" + logLine)
	if got != "" || count != 0 {
		t.Errorf("Find(clean log) = %q, %d; want nothing", got, count)
	}
}

func TestFindKeepsATruncatedReport(t *testing.T) {
	t.Parallel()
	// A process killed mid-report leaves no closing rule, and what it did
	// print is still the only record of where the race was.
	cut := report[:strings.LastIndex(report, "Goroutine 42")]
	got, count := Find(logLine + "\n" + cut)
	if got != cut {
		t.Errorf("Find returned\n%s\nwant\n%s", got, cut)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestFindCountsEveryReport(t *testing.T) {
	t.Parallel()
	got, count := Find(report + "\n" + logLine + "\n" + report + "\nFound 2 data race(s)\n")
	if got != report {
		t.Errorf("Find returned\n%s\nwant the first report", got)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

// recorder is a testing.TB that keeps its failures instead of reporting them,
// so that Check can be watched failing without failing this test.
type recorder struct {
	testing.TB

	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestCheckFailsWithTheReport(t *testing.T) {
	t.Parallel()
	rec := &recorder{TB: t}
	Check(rec, logLine+"\n"+report+"\n")
	if len(rec.failures) != 1 {
		t.Fatalf("Check failed %d times, want once: %q", len(rec.failures), rec.failures)
	}
	if !strings.Contains(rec.failures[0], "internal/sink/prom.go:114") {
		t.Errorf("the failure does not say where the race was:\n%s", rec.failures[0])
	}
}

func TestCheckPassesACleanLog(t *testing.T) {
	t.Parallel()
	rec := &recorder{TB: t}
	Check(rec, logLine)
	if len(rec.failures) != 0 {
		t.Errorf("Check failed a clean log: %q", rec.failures)
	}
}
