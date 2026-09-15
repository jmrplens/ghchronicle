package render

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Point is the sink's, aliased so the table below reads as data rather than
// as a package path repeated forty times.
type Point = sink.Point

func TestAccumulatorBuildsACardFromPoints(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	err := a.Write(context.Background(), points(now))
	if err != nil {
		t.Fatal(err)
	}
	c := a.Card()

	if c.Login != "someone" {
		t.Errorf("login = %q", c.Login)
	}
	if c.Followers != 41 || c.Repos != 42 || c.Contributions != 6210 {
		t.Errorf("account numbers wrong: %+v", c)
	}
	// Stars and forks are summed from the repositories, because GitHub's
	// account endpoint reports neither total.
	if c.Stars != 30 || c.Forks != 4 {
		t.Errorf("stars = %d, forks = %d, want 30 and 4", c.Stars, c.Forks)
	}
	// The traffic window is only meaningful added up.
	if c.Views != 30 || c.UniqueVisitors != 12 {
		t.Errorf("traffic = %d views, %d visitors", c.Views, c.UniqueVisitors)
	}
	if len(c.TopRepos) != 2 || c.TopRepos[0].Name != "beta" {
		t.Errorf("repos are not ranked by stars: %+v", c.TopRepos)
	}
	if len(c.Sparkline) != 2 {
		t.Errorf("sparkline = %v, want two days", c.Sparkline)
	}
}

func TestAccumulatorKeepsTheNewestRepositorySnapshot(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	_ = a.Write(context.Background(), []Point{
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "language": "Go"},
			Fields: map[string]any{"stars": 1, "forks": 0}, Time: now,
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "language": "Go"},
			Fields: map[string]any{"stars": 9, "forks": 2}, Time: now.Add(time.Hour),
		},
	})
	if c := a.Card(); c.Stars != 9 || c.Forks != 2 {
		t.Errorf("a repository seen twice must count once, at its newest: %+v", c)
	}
}

func TestAccumulatorDoesNotAddACalendarDayTwice(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	// The same day arriving twice is a re-read of the calendar, not two days
	// of work.
	_ = a.Write(context.Background(), []Point{
		{Measurement: "gh_contribution_day", Fields: map[string]any{"contributions": 5}, Time: day},
		{Measurement: "gh_contribution_day", Fields: map[string]any{"contributions": 5}, Time: day},
	})
	c := a.Card()
	if len(c.Sparkline) != 1 || c.Sparkline[0] != 5 {
		t.Errorf("sparkline = %v, want one day of 5", c.Sparkline)
	}
}

func TestAccumulatorKeepsOnlyTheLastYearOfCalendar(t *testing.T) {
	newest := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	var pts []Point
	// Three years of daily rows, which the backfill produces. A sparkline of
	// nine years would be a smear.
	for d := newest.AddDate(-3, 0, 0); !d.After(newest); d = d.AddDate(0, 0, 1) {
		pts = append(pts, Point{
			Measurement: "gh_contribution_day",
			Fields:      map[string]any{"contributions": 1}, Time: d,
		})
	}
	_ = a.Write(context.Background(), pts)
	if n := len(a.Card().Sparkline); n > 370 || n < 360 {
		t.Errorf("sparkline has %d days, want about a year", n)
	}
}

// Languages arrive per repository and the card shows them per account, so the
// sum has to cross repositories and the ranking has to be by size first and
// by name only between equals. A point that names no repository or no
// language, or carries no number, has nothing to add to either.
func TestAccumulatorSumsLanguagesAcrossRepositoriesAndRanksThem(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	lang := func(repo, name string, bytes any) Point {
		return Point{
			Measurement: "gh_repo_language", Tags: map[string]string{"repo": repo, "language": name},
			Fields: map[string]any{"bytes": bytes}, Time: now,
		}
	}
	a := NewAccumulator("someone")
	_ = a.Write(context.Background(), []Point{
		lang("a", "Python", 50),
		lang("a", "Go", 30),
		lang("b", "Go", 20),
		lang("b", "Zig", 120),
		lang("c", "Awk", 1),
		lang("c", "Tcl", 70),
		lang("c", "Elm", 50),
		lang("c", "Nim", 50),
		lang("d", "Hack", 50),
		lang("", "Rust", 900),
		lang("b", "", 900),
		lang("b", "Ruby", "a lot"),
	})
	got := a.Card().Languages
	want := []Language{{Name: "Zig", Bytes: 120}, {Name: "Tcl", Bytes: 70}, {Name: "Elm", Bytes: 50}, {Name: "Go", Bytes: 50}, {Name: "Hack", Bytes: 50}, {Name: "Nim", Bytes: 50}, {Name: "Python", Bytes: 50}, {Name: "Awk", Bytes: 1}}
	if len(got) != len(want) {
		t.Fatalf("languages = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("language %d = %+v, want %+v (all of them: %+v)", i, got[i], want[i], got)
		}
	}
}

// A calendar day is a date, not an instant: two readings of the same day at
// different hours are the same day, and the later one is what it holds.
func TestAccumulatorFoldsTheSameDayReadAtDifferentHours(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	_ = a.Write(context.Background(), []Point{
		{Measurement: "gh_contribution_day", Fields: map[string]any{"contributions": 3}, Time: day.Add(8 * time.Hour)},
		{Measurement: "gh_contribution_day", Fields: map[string]any{"contributions": 5}, Time: day.Add(20 * time.Hour)},
		{Measurement: "gh_contribution_day", Fields: map[string]any{"contributions": "many"}, Time: day.Add(-24 * time.Hour)},
	})
	c := a.Card()
	if len(c.Sparkline) != 1 || c.Sparkline[0] != 5 {
		t.Errorf("sparkline = %v, want the one day holding 5", c.Sparkline)
	}
}

// The account totals follow the newest snapshot whatever order the sweep
// hands them over in: a newer one replaces, an older one arriving late does
// not undo it. A field that is not a number is not a total.
func TestAccumulatorKeepsTheNewestAccountSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	account := func(followers any, at time.Time) Point {
		return Point{
			Measurement: "gh_account",
			Fields:      map[string]any{"followers": followers, "company": "somewhere"},
			Time:        at,
		}
	}
	a := NewAccumulator("someone")
	_ = a.Write(context.Background(), []Point{
		account(10, now),
		account(int64(20), now.Add(time.Hour)),
		account(5.0, now.Add(-time.Hour)),
	})
	if c := a.Card(); c.Followers != 20 {
		t.Errorf("followers = %d, want 20 from the newest snapshot", c.Followers)
	}
	if _, ok := a.account["account.company"]; ok {
		t.Error("a text field was taken for a number")
	}
}

// A repository snapshot older than one already seen is a late arrival and
// changes nothing, and a newer one that lacks a field keeps what the older
// one said rather than zeroing it. A row that names no repository counts for
// nothing.
func TestAccumulatorIgnoresStaleAndNamelessRepositoryRows(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	_ = a.Write(context.Background(), []Point{
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "language": "Go"},
			Fields: map[string]any{"stars": 4, "forks": 2}, Time: now,
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "language": "Go"},
			Fields: map[string]any{"stars": 99, "forks": 99}, Time: now.Add(-time.Hour),
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "language": "Go"},
			Fields: map[string]any{"description": "no numbers here"}, Time: now.Add(time.Hour),
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"language": "Go"},
			Fields: map[string]any{"stars": 1000, "forks": 1000}, Time: now,
		},
	})
	c := a.Card()
	if c.Stars != 4 || c.Forks != 2 {
		t.Errorf("stars = %d, forks = %d, want 4 and 2", c.Stars, c.Forks)
	}
	if len(c.TopRepos) != 1 || c.TopRepos[0].Name != "a" {
		t.Errorf("top repos = %+v, want only a", c.TopRepos)
	}
}

// Repositories with the same stars are ordered by name, so a caller reading
// TopRepos gets the same list on every run however the map iterated. The
// points are fed from a map on purpose: the order they arrive in is random.
func TestAccumulatorBreaksAStarTieByName(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	var pts []Point
	for name, stars := range map[string]int{"delta": 3, "bravo": 3, "top": 8, "charlie": 3, "alpha": 3, "zero": 0} {
		pts = append(pts, Point{
			Measurement: "gh_repo", Tags: map[string]string{"repo": name},
			Fields: map[string]any{"stars": stars}, Time: now,
		})
	}
	_ = a.Write(context.Background(), pts)
	var got []string
	for _, r := range a.Card().TopRepos {
		got = append(got, r.Name)
	}
	if strings.Join(got, " ") != "top alpha bravo charlie delta zero" {
		t.Errorf("tied repositories = %v, want them by name", got)
	}
}

// Traffic is summed per kind, and a row that carries only one of the two
// counters adds to that one alone.
func TestAccumulatorSumsTrafficPerKind(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	a := NewAccumulator("someone")
	_ = a.Write(context.Background(), []Point{
		{Measurement: "gh_traffic", Tags: map[string]string{"kind": "clones"}, Fields: map[string]any{"count": 6}, Time: now},
		{Measurement: "gh_traffic", Tags: map[string]string{"kind": "clones"}, Fields: map[string]any{"count": 1.0, "uniques": 1}, Time: now},
		{Measurement: "gh_traffic", Tags: map[string]string{"kind": "views"}, Fields: map[string]any{"uniques": 9}, Time: now},
		{Measurement: "gh_somewhere_else", Fields: map[string]any{"count": 500}, Time: now},
	})
	c := a.Card()
	if c.Clones != 7 || c.Views != 0 || c.UniqueVisitors != 9 {
		t.Errorf("clones = %d, views = %d, visitors = %d, want 7, 0 and 9", c.Clones, c.Views, c.UniqueVisitors)
	}
}

// numberOf is the one place a field's Go type is read, and a sink receives
// ints, int64s, floats and booleans depending on which collector wrote the
// point.
func TestNumberOfReadsEveryNumericFieldType(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{7, 7, true},
		{int64(1) << 40, 1 << 40, true},
		{2.5, 2.5, true},
		{true, 1, true},
		{false, 0, true},
		{"7", 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		if got, ok := numberOf(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("numberOf(%#v) = %v, %v, want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// The accumulator sits in the sink list like any other, so its name is the
// "sink" the runner logs every write under, and the command closes it with
// the other sinks on the way out. It holds no file or connection, so closing
// has nothing to report and must not throw away what was gathered.
func TestTheAccumulatorIsTheCardSinkAndClosingKeepsTheCard(t *testing.T) {
	a := NewAccumulator("someone")
	if got := a.Name(); got != "card" {
		t.Errorf("name = %q, want card", got)
	}
	if err := a.Write(context.Background(), points(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close = %v, want nil", err)
	}
	if c := a.Card(); c.Followers != 41 || c.Stars != 30 {
		t.Errorf("the card after close lost its numbers: %+v", c)
	}
}

func points(now time.Time) []Point {
	return []Point{
		{
			Measurement: "gh_account", Tags: map[string]string{"user": "someone"},
			Fields: map[string]any{"followers": 41, "public_repos": 42}, Time: now,
		},
		{
			Measurement: "gh_contributions_total", Tags: map[string]string{"user": "someone"},
			Fields: map[string]any{"calendar_total": 6210}, Time: now,
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "alpha", "language": "Go"},
			Fields: map[string]any{"stars": 10, "forks": 1}, Time: now,
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "beta", "language": "Python"},
			Fields: map[string]any{"stars": 20, "forks": 3}, Time: now,
		},
		{
			Measurement: "gh_traffic", Tags: map[string]string{"repo": "alpha", "kind": "views"},
			Fields: map[string]any{"count": 10, "uniques": 4}, Time: now.AddDate(0, 0, -1),
		},
		{
			Measurement: "gh_traffic", Tags: map[string]string{"repo": "alpha", "kind": "views"},
			Fields: map[string]any{"count": 20, "uniques": 8}, Time: now,
		},
		{
			Measurement: "gh_contribution_day", Tags: map[string]string{"user": "someone"},
			Fields: map[string]any{"contributions": 3}, Time: now.AddDate(0, 0, -1),
		},
		{
			Measurement: "gh_contribution_day", Tags: map[string]string{"user": "someone"},
			Fields: map[string]any{"contributions": 7}, Time: now,
		},
	}
}
