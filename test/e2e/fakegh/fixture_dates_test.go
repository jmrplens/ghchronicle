package fakegh

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// isoDate is any calendar date written out in a fixture. No word boundary
// after it: the dates that matter are followed by "T15:04:05Z", and \b between
// a digit and a T is not a boundary at all, which is how the first version of
// this guard passed over every date it was written to catch.
var isoDate = regexp.MustCompile(`\b20\d\d-\d\d-\d\d`)

// recentEnoughToDrift is how close to the present a written-out date may not
// be. Dates only ever get older, so a fixture already further back than this
// can never wander into the window a dashboard asks for; one written today is
// caught the moment it is committed, which is the only moment fixing it is
// cheap.
const recentEnoughToDrift = 90 * 24 * time.Hour

// everyFixture is what the fake would serve, read the same way it reads them.
func everyFixture(tb testing.TB) map[string][]byte {
	tb.Helper()
	all := readFixtures(tb, "../testdata")
	for name, body := range readFixtures(tb, "../testdata/gallery") {
		all["gallery/"+name] = body
	}
	return all
}

// TestNoFixtureWritesOutARecentDate is the guard the release of 2.4.0 did not
// have.
//
// Its fixture merged one pull request on a date written out by hand. The
// dashboards ask for the last thirty days, and the day that date left the
// window fell between a scheduled run at 04:50 and a release at 17:03: four
// panels of every store went blank with nothing in the code having changed,
// which from the outside is what a broken query looks like.
//
// A date far in the past is fine and usually means something (a repository
// created in 2011). A recent one is a countdown nobody set.
func TestNoFixtureWritesOutARecentDate(t *testing.T) {
	t.Parallel()
	cutoff := time.Now().UTC().Add(-recentEnoughToDrift)
	for name, body := range everyFixture(t) {
		for _, written := range isoDate.FindAllString(string(body), -1) {
			at, err := time.Parse(time.DateOnly, written)
			if err != nil || at.Before(cutoff) {
				continue
			}
			t.Errorf("%s writes out %s; spell it %s so it stays where the "+
				"fixture meant it", name, written, offsetFor(at))
		}
	}
}

// offsetFor is the marker a date should have been written as.
func offsetFor(at time.Time) string {
	days := int(time.Since(at).Hours() / 24)
	if days < 0 {
		return fmt.Sprintf("@DAYS_AHEAD_%d@", -days)
	}
	return fmt.Sprintf("@DAYS_AGO_%d@", days)
}

// TestEveryMarkerInTheFixturesIsOneTheFakeResolves. A marker the fake does not
// know is served as itself, and what fails then is a collector three packages
// away reading "@DAYS_AGO_5@" as the zero time.
func TestEveryMarkerInTheFixturesIsOneTheFakeResolves(t *testing.T) {
	t.Parallel()
	known := []string{"@NOW@", "@TODAY@", "@SOON@", "@SOON_EPOCH@"}
	marker := regexp.MustCompile(`@[A-Z0-9_]+@`)
	for name, body := range everyFixture(t) {
		for _, m := range marker.FindAllString(string(body), -1) {
			if daysAgoMarker.MatchString(m) || slices.Contains(known, m) {
				continue
			}
			t.Errorf("%s carries %s, which nothing resolves", name, m)
		}
	}
	// And the ones it does know are still known: a token renamed in resolve
	// without being renamed in the fixtures fails here rather than in a sweep.
	s := &Server{tb: t, now: time.Now().UTC}
	for _, token := range append(known, "@DAYS_AGO_3@") {
		if got := string(s.resolve([]byte(token))); strings.Contains(got, "@") {
			t.Errorf("%s came back as %q", token, got)
		}
	}
}
