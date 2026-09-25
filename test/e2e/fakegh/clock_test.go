package fakegh

import (
	"strconv"
	"testing"
	"time"
)

// What a relative date in a fixture resolves to, and what freezing the clock
// is for.

// TestAFixtureCountsBackFromTheFakesToday. The pull request the fixtures merge
// is what four dashboard panels of every store are drawn from, and it has to
// stay inside the window those panels ask for. Spelling the day it merged as
// an offset is what keeps it there; spelling a date put it outside between one
// scheduled run and the next, with nothing in the code having changed.
func TestAFixtureCountsBackFromTheFakesToday(t *testing.T) {
	t.Parallel()
	s := &Server{now: func() time.Time { return time.Now().UTC() }}
	start := time.Now().UTC().Truncate(24 * time.Hour)
	if got, want := s.agoDate("@DAYS_AGO_16@"),
		start.AddDate(0, 0, -16).Format(time.DateOnly); got != want {
		t.Errorf("agoDate(@DAYS_AGO_16@) = %q, want %q", got, want)
	}
	if s.agoDate("@DAYS_AGO_16@") == start.Format(time.DateOnly) {
		t.Error("it resolved to today, so the offset was not read at all")
	}
	// Whole days, not the moment this ran: a point carries its own date, and
	// two sweeps of one test have to write the same one.
	if s.agoDate("@DAYS_AGO_0@") != start.Format(time.DateOnly) {
		t.Error("zero days ago is not the start of today")
	}
}

// TestAFrozenClockGivesTheSameDayForever, which is what lets the committed
// cards be compared byte for byte against fixtures whose dates move.
func TestAFrozenClockGivesTheSameDayForever(t *testing.T) {
	t.Parallel()
	s := &Server{now: func() time.Time { return time.Now().UTC() }}
	s.FreezeAt(time.Date(2026, 9, 11, 13, 45, 0, 0, time.UTC))
	if got := s.agoDate("@DAYS_AGO_12@"); got != "2026-08-30" {
		t.Errorf("twelve days before the frozen day = %q, want 2026-08-30", got)
	}
	// The hour it was frozen at does not reach the date, so a suite can freeze
	// at any moment of the day it means.
	if got := s.agoDate("@DAYS_AGO_0@"); got != "2026-09-11" {
		t.Errorf("the frozen day itself = %q, want 2026-09-11", got)
	}
}

// TestDaysAgoIsWhatATestAssertsWith: the exported helper and the marker have
// to agree, or an assertion drifts from the fixture it is checking.
func TestDaysAgoIsWhatATestAssertsWith(t *testing.T) {
	t.Parallel()
	s := &Server{now: func() time.Time { return time.Now().UTC() }}
	for _, n := range []int{0, 1, 12, 37} {
		if got, want := DaysAgoDate(n), s.agoDate("@DAYS_AGO_"+strconv.Itoa(n)+"@"); got != want {
			t.Errorf("DaysAgoDate(%d) = %q, but the fixture would read %q", n, got, want)
		}
		if h := DaysAgo(n).Hour(); h != 0 {
			t.Errorf("DaysAgo(%d) is at %02d:00, want the start of the day", n, h)
		}
	}
}

// TestAWeekEpochIsTheSundayOfTheFakesWeek: the star history labels each week
// with the Unix second its Sunday starts at in UTC, and the fixture's weeks
// have to be the weeks of the fake's own present, counted back whole.
func TestAWeekEpochIsTheSundayOfTheFakesWeek(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		at     time.Time
		marker string
		want   time.Time
	}{
		{
			name: "a Friday afternoon", marker: "@WEEK_EPOCH_0@",
			at:   time.Date(2026, 9, 11, 13, 45, 0, 0, time.UTC),
			want: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "two weeks back", marker: "@WEEK_EPOCH_2@",
			at:   time.Date(2026, 9, 11, 13, 45, 0, 0, time.UTC),
			want: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC),
		},
		// The Sunday itself is its own week's start, not the week before's.
		{
			name: "just past a Sunday's midnight", marker: "@WEEK_EPOCH_0@",
			at:   time.Date(2026, 9, 13, 0, 30, 0, 0, time.UTC),
			want: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "the last moment of a Saturday", marker: "@WEEK_EPOCH_1@",
			at:   time.Date(2026, 9, 12, 23, 59, 59, 0, time.UTC),
			want: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		},
	} {
		s := &Server{tb: t}
		s.FreezeAt(tc.at)
		if got, want := s.weekEpoch(tc.marker), strconv.FormatInt(tc.want.Unix(), 10); got != want {
			t.Errorf("%s: %s = %s, want %s (%s)", tc.name, tc.marker, got, want, tc.want.Format(time.DateOnly))
		}
	}
}
