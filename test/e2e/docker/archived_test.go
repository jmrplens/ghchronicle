//go:build dockere2e

package docker

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// TestAnArchivedRepositorySetAsideReachesTheAccountTotals is issue #78 in the
// store it was reported against. A repository the default filter sets aside
// for being archived is never in the repository picker, which lists what
// gh_repo holds, and up to 2.5.1 its stars and forks were in the Overview's
// sum and in "Every repository, ever" only as long as a backfill's one row
// stayed in the range. Here a sweep sets one aside, InfluxDB 3 holds what it
// wrote, and the two panels' own SQL, rendered the way a dashboard renders
// it, counts the archived repository under All and leaves it out once the
// reader picks the live repositories by name.
//
// InfluxDB alone, in a database of its own: the SQL is the same in
// PostgreSQL but for the list formatter, and the shared stores belong to the
// sweep every other test here compares against.
func TestAnArchivedRepositorySetAsideReachesTheAccountTotals(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	const archivedStars = 3
	gh := fakegh.New(t, sqlStoresFixtures, fakegh.ArchivedOverlay(t, sqlStoresFixtures, archivedStars))
	sweep, err := sqlStoresSweepWith(ctx, t, s, "archived", s.InfluxDatabase+"_archived", gh)
	if err != nil {
		t.Fatalf("the sweep with an archived repository set aside failed: %v", err)
	}
	points := sqlStoresPoints(t, sweep)
	aside := fakegh.SetAside[strings.Index(fakegh.SetAside, "/")+1:]
	picker := dashboardRepos(points)
	if len(picker) == 0 || slices.Contains(picker, aside) {
		t.Fatalf("the picker lists %v, want the live repositories and not %s", picker, aside)
	}
	var liveStars float64
	for _, p := range points {
		if p.Measurement == "gh_repo_total" && slices.Contains(picker, p.Tags["repo"]) {
			liveStars += p.Fields["stars"].(float64)
		}
	}

	doc, err := dashboardDocument("influxdb")
	if err != nil {
		t.Fatal(err)
	}
	all := dashboardVars(doc, dashboardStores[0], picker)
	picked := all
	picked.Text = strings.Join(picker, " + ")
	stars := func(vars grafana.Vars) float64 {
		t.Helper()
		rows, queryErr := influxSQL(ctx, s, sweep.Database, archivedPanelSQL(t, doc, vars, "Repositories", "SUM(stars)"))
		if queryErr != nil || len(rows) != 1 {
			t.Fatalf("the Overview's stars answered %v, %v", rows, queryErr)
		}
		return numberOf(t, rows[0], "Stars")
	}
	if got, want := stars(all), liveStars+archivedStars; got != want {
		t.Errorf("under All the Overview counts %v stars, want %v: the live repositories' %v and "+
			"the %d of %s", got, want, liveStars, archivedStars, fakegh.SetAside)
	}
	if got := stars(picked); got != liveStars {
		t.Errorf("with %v picked the Overview counts %v stars, want their %v alone", picker, got, liveStars)
	}

	rows, err := influxSQL(ctx, s, sweep.Database, archivedPanelSQL(t, doc, all, "Every repository, ever", "MAX(commits)"))
	if err != nil {
		t.Fatalf("Every repository, ever: %v", err)
	}
	i := slices.IndexFunc(rows, func(row map[string]any) bool { return row["Repository"] == aside })
	if i < 0 {
		t.Fatalf("Every repository, ever lists %v under All, and not %s", rows, aside)
	}
	wantString(t, rows[i], "Archived", "true")
	wantNumber(t, rows[i], "Stars", archivedStars)
}

// archivedPanelSQL is the statement of the one target of the titled panel that
// carries marker, rendered with vars.
func archivedPanelSQL(t *testing.T, doc map[string]any, vars grafana.Vars, title, marker string) string {
	t.Helper()
	for _, p := range grafana.Panels(doc["panels"]) {
		if p.Title != title {
			continue
		}
		for _, target := range p.Targets {
			if sql, _ := vars.Apply(target)["rawSql"].(string); strings.Contains(sql, marker) {
				return sql
			}
		}
	}
	t.Fatalf("no target of %q carries %s", title, marker)
	return ""
}
