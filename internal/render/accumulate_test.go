package render

import (
	"context"
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
