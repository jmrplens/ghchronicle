package collect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// TestAPartialFailureKeepsWhatItWrapsReachable.
//
// The reason this wrapper exists is that "the batch brought back nothing" and
// "the batch was cut short" are different answers, and the caller decides what
// to do by looking underneath: a rate limit waits, a cancelation stops, and
// anything else is one family's bad tick. All of that goes through errors.As
// and errors.Is, which only work while Unwrap keeps the failure reachable.
//
// Nothing exercised that before. The type is built in one place and read in
// seven, and a wrapper that swallowed what it wraps would have left every one
// of those reading a generic failure and treating a spent budget as a broken
// collector.
func TestAPartialFailureKeepsWhatItWrapsReachable(t *testing.T) {
	t.Parallel()
	limited := &ghapi.RateLimitedError{Resource: "core", Reset: time.Unix(1, 0)}
	partial := &PartialError{Asked: 14, Err: limited}

	var found *ghapi.RateLimitedError
	if !errors.As(error(partial), &found) {
		t.Fatal("a rate limit wrapped in a partial failure is not found by errors.As, " +
			"so a caller waiting for the window would treat it as a broken collector")
	}
	if found.Resource != "core" {
		t.Errorf("errors.As found a rate limit for %q, want the one that was wrapped", found.Resource)
	}

	// The other half of the same claim: a stopped sweep stays a stopped sweep.
	if !errors.Is(&PartialError{Asked: 3, Err: context.Canceled}, context.Canceled) {
		t.Error("a canceled sweep wrapped in a partial failure is not found by errors.Is")
	}
}

// TestAPartialFailureSaysHowManyAnswered. Asked is how many repositories were
// in the chunks that answered, and it is the number that tells a reader
// whether a family collected almost everything or almost nothing. A message
// that omitted it would leave the log saying only that something failed.
func TestAPartialFailureSaysHowManyAnswered(t *testing.T) {
	t.Parallel()
	got := (&PartialError{Asked: 14, Err: errors.New("the gateway gave up")}).Error()
	for _, want := range []string{"the gateway gave up", "14"} {
		if !strings.Contains(got, want) {
			t.Errorf("the message %q does not carry %q", got, want)
		}
	}
}
