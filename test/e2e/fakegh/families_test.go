package fakegh

import (
	"slices"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// The lists in this package are the fourth kind of list in this repository to
// go stale by mirroring the runner by hand, and the tests below are what
// closes them. Families is derived, so it cannot drift; NotCollected,
// Measurements and Unanswered are hand-written subsets, and each is asserted
// against internal/config in both directions, so a family added to the runner
// fails here until somebody says which of the three it belongs in.

func TestEveryFamilyIsEitherCollectedOrExcused(t *testing.T) {
	collected := Families()
	for _, f := range config.Families() {
		_, excused := NotCollected[f]
		if !excused && !slices.Contains(collected, f) {
			t.Errorf("family %s is neither collected by the suites nor listed in NotCollected with a reason", f)
		}
	}
}

func TestNothingExcusedOutlivesItsFamily(t *testing.T) {
	for name, why := range NotCollected {
		if _, known := config.GroupOf(name); !known {
			t.Errorf("NotCollected names %s, which is not a family: %s", name, why)
		}
		if why == "" {
			t.Errorf("NotCollected excuses %s without saying why", name)
		}
	}
}

func TestEveryCollectedFamilyNamesAMeasurement(t *testing.T) {
	for _, f := range Families() {
		if Measurements[f] == "" {
			t.Errorf("family %s is collected but names no measurement, so a sweep that "+
				"produced nothing for it would pass unnoticed", f)
		}
	}
}

func TestNoMeasurementOutlivesItsFamily(t *testing.T) {
	collected := Families()
	for name := range Measurements {
		if !slices.Contains(collected, name) {
			t.Errorf("Measurements names %s, which no suite collects", name)
		}
	}
}

func TestNothingUnansweredOutlivesItsFamily(t *testing.T) {
	collected := Families()
	for name, why := range Unanswered {
		if !slices.Contains(collected, name) {
			t.Errorf("Unanswered names %s, which no suite collects: %s", name, why)
		}
		if why == "" {
			t.Errorf("Unanswered excuses %s without saying why", name)
		}
	}
}

// Answering is what the sweep assertions iterate, so it has to be every
// collected family less the quiet ones and nothing else.
func TestAnsweringIsTheCollectedFamiliesLessTheQuietOnes(t *testing.T) {
	got := Answering()
	if len(got)+len(Unanswered) != len(Families()) {
		t.Errorf("Answering has %d families and Unanswered %d, against %d collected",
			len(got), len(Unanswered), len(Families()))
	}
	for _, f := range got {
		if _, quiet := Unanswered[f]; quiet {
			t.Errorf("%s is in Answering and in Unanswered", f)
		}
	}
}
