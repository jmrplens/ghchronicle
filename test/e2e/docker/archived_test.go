//go:build dockere2e

package docker

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// TestAnArchivedRepositorySetAsideReachesTheAccountTotals is issue #78 in the
// store it was reported against. A repository the default filter sets aside
// for being archived gets no gh_repo row from a sweep, so it is not in the
// repository picker, which lists what gh_repo holds, and up to 2.5.1 its
// stars and forks were in the Overview's
// sum and in "Every repository, ever" only as long as a backfill's one row
// stayed in the range. Here a sweep sets one aside, InfluxDB 3 holds what it
// wrote, and the two panels' own SQL, rendered the way a dashboard renders
// it, counts the archived repository under All and leaves it out once the
// reader picks the live repositories by name.
//
// Then the rows checking 2.6.0 against GitHub found three panels reading as
// current, written into the same database beside the sweep's own: see
// staleRows.
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
	// The live repositories' stars as the Overview reads them, out of the
	// gh_repo a sweep writes every hour; the archived one's out of its
	// gh_repo_total, the one row of it there is.
	var liveStars float64
	for _, p := range points {
		if p.Measurement == "gh_repo" && slices.Contains(picker, p.Tags["repo"]) {
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

	everyRepository := func() []map[string]any {
		t.Helper()
		rows, queryErr := influxSQL(ctx, s, sweep.Database,
			archivedPanelSQL(t, doc, all, "Every repository, ever", `AS "Commits"`))
		if queryErr != nil {
			t.Fatalf("Every repository, ever: %v", queryErr)
		}
		return rows
	}
	rows := everyRepository()
	i := slices.IndexFunc(rows, func(row map[string]any) bool { return row["Repository"] == aside })
	if i < 0 {
		t.Fatalf("Every repository, ever lists %v under All, and not %s", rows, aside)
	}
	wantString(t, rows[i], "Archived", "true")
	wantNumber(t, rows[i], "Stars", archivedStars)

	holdToWhatIsCurrent(ctx, t, s, sweep.Database, staleRows(t, points, archivedStars), currentPanels{
		stars:           func() float64 { return stars(all) },
		everyRepository: everyRepository,
		cache:           archivedPanelSQL(t, doc, all, "Cache entries by key", "SUM(caches)"),
		wantStars:       liveStars + archivedStars,
		aside:           aside,
		archivedStars:   archivedStars,
	})
}

// currentPanels is how holdToWhatIsCurrent asks the three panels, each
// rendered under All, and what the first two have to answer.
type currentPanels struct {
	// stars is the Overview's stars, and wantStars what they are.
	stars     func() float64
	wantStars float64
	// everyRepository is the rows of the Lifetime table, where aside, the
	// repository set aside, has to read archivedStars.
	everyRepository func() []map[string]any
	aside           string
	archivedStars   float64
	// cache is the statement of "Cache entries by key".
	cache string
}

// holdToWhatIsCurrent writes the stale rows beside the sweep's own and holds
// each panel to reading past them: the Overview and the Lifetime table to
// leaving out the repository nobody collects, the Lifetime table to the set
// aside one's newest row, and the cache table to the sweep's own day.
func holdToWhatIsCurrent(ctx context.Context, t *testing.T, s *Stack, database string, stale staleWrite, panels currentPanels) {
	t.Helper()
	for _, p := range stale.rows {
		status, body, err := influxWriteLine(ctx, s, database, sink.LineProtocol(p))
		if err != nil || status >= 300 {
			t.Fatalf("writing %s: %d %s %v", sink.LineProtocol(p), status, body, err)
		}
	}
	influxAwaitStale(ctx, t, s, database, stale)

	if got := panels.stars(); got != panels.wantStars {
		t.Errorf("under All the Overview counts %v stars, want %v: %s, which the collector no "+
			"longer writes, is not the account's", got, panels.wantStars, stale.gone)
	}
	rows := panels.everyRepository()
	if slices.ContainsFunc(rows, func(row map[string]any) bool { return row["Repository"] == stale.goneName }) {
		t.Errorf("Every repository, ever lists %s, which the collector no longer writes: %v", stale.gone, rows)
	}
	i := slices.IndexFunc(rows, func(row map[string]any) bool { return row["Repository"] == panels.aside })
	if i < 0 {
		t.Fatalf("Every repository, ever lists %v under All, and not %s", rows, panels.aside)
	}
	// Its newest row, not the most stars any row in the range held.
	wantNumber(t, rows[i], "Stars", panels.archivedStars)

	cacheRows, err := influxSQL(ctx, s, database, panels.cache)
	if err != nil {
		t.Fatalf("Cache entries by key: %v", err)
	}
	i = slices.IndexFunc(cacheRows, func(row map[string]any) bool {
		return row["Cache"] == stale.cache && row["Repository"] == stale.cacheRepo
	})
	if i < 0 {
		t.Fatalf("Cache entries by key lists %v, and not %s of %s", cacheRows, stale.cache, stale.cacheRepo)
	}
	// What the sweep read today, and not the ref GitHub evicted three days ago.
	wantNumber(t, cacheRows[i], "Entries", stale.entries)
	wantNumber(t, cacheRows[i], "Size", stale.size)
}

// staleWrite is what staleRows writes and what the panels have to read past it.
type staleWrite struct {
	rows []sink.Point
	// gone is the archived repository the collector no longer writes, and
	// goneName its short name, which is what the tables show.
	gone, goneName string
	// cache and cacheRepo name the cache whose evicted ref is written, and
	// entries and size are what the sweep read of it today.
	cache, cacheRepo string
	entries, size    float64
}

// staleRows are the rows checking 2.6.0 against GitHub panel by panel found
// the dashboards reading as current, made from the sweep's own so that each
// carries exactly the tag set the store keys that table by: the one row of an
// archived repository the collector no longer writes, nine days old, as a
// backfill under an earlier configuration left
// jmrplens/portainer-mcp-enhanced; an older row of the repository set aside
// with more stars than it has now, as a backfill left jmrplens/FFT2octave at
// 4 where GitHub said 3; and a cache on a ref GitHub evicted three days ago,
// as 108 refs of jmrplens/gitlab-mcp-server's golangci-lint were.
func staleRows(t *testing.T, points []sqlStoresPoint, archivedStars int) staleWrite {
	t.Helper()
	var aside, cache *sqlStoresPoint
	for i := range points {
		p := &points[i]
		switch {
		case aside == nil && p.Measurement == "gh_repo_total" && p.Tags["full_name"] == fakegh.SetAside:
			aside = p
		case cache == nil && p.Measurement == "gh_actions_cache_entry":
			cache = p
		}
	}
	if aside == nil || cache == nil {
		t.Fatalf("the sweep wrote no gh_repo_total row of %s (%v) or no cache row (%v)",
			fakegh.SetAside, aside != nil, cache != nil)
	}
	out := staleWrite{
		gone: fakegh.Login + "/gone", goneName: "gone",
		cache: cache.Tags["cache"], cacheRepo: cache.Tags["repo"],
	}
	for _, p := range points {
		if p.Measurement == "gh_actions_cache_entry" && p.Tags["repo"] == out.cacheRepo &&
			p.Tags["cache"] == out.cache {
			out.entries += p.Fields["caches"].(float64)
			out.size += p.Fields["size_bytes"].(float64)
		}
	}
	retag := func(tags, set map[string]string) map[string]string {
		copied := maps.Clone(tags)
		maps.Copy(copied, set)
		return copied
	}
	now := time.Now().UTC()
	day := now.Truncate(24 * time.Hour)
	out.rows = []sink.Point{
		{
			Measurement: "gh_repo_total",
			Tags:        retag(aside.Tags, map[string]string{"full_name": out.gone, "repo": out.goneName, "fork": "true"}),
			Fields: map[string]any{
				"stars": int64(8), "forks": int64(3), "commits": int64(133),
				"url": "https://github.com/" + out.gone,
			},
			Time: now.Add(-9 * 24 * time.Hour),
		},
		{
			Measurement: "gh_repo_total", Tags: aside.Tags,
			Fields: map[string]any{"stars": int64(archivedStars + 5)},
			Time:   now.Add(-2 * 24 * time.Hour),
		},
		{
			Measurement: "gh_actions_cache_entry",
			Tags:        retag(cache.Tags, map[string]string{"ref": "refs/pull/1/merge"}),
			Fields: map[string]any{
				"size_bytes": int64(1000), "caches": int64(5), "days_since_use": int64(3),
			},
			Time: day.Add(-3 * 24 * time.Hour),
		},
	}
	return out
}

// influxAwaitStale waits until each row staleRows wrote can be read back, so
// that a panel that reads past them is reading past rows that are there.
func influxAwaitStale(ctx context.Context, t *testing.T, s *Stack, database string, stale staleWrite) {
	t.Helper()
	for _, q := range []string{
		"SELECT stars FROM gh_repo_total WHERE full_name = '" + stale.gone + "'",
		"SELECT stars FROM gh_repo_total WHERE full_name = '" + fakegh.SetAside + "' AND time < now() - INTERVAL '1 day'",
		"SELECT caches FROM gh_actions_cache_entry WHERE ref = 'refs/pull/1/merge'",
	} {
		if _, err := influxAwaitRow(ctx, s, database, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
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
