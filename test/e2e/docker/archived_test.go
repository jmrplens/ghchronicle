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
// current, and the documentation audit of 2.6.0 two more, written into the
// same database beside the sweep's own: see staleRows.
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
		features:        archivedPanelSQL(t, doc, all, "Security features", `AS "Enabled"`),
		artifacts:       archivedPanelSQL(t, doc, all, "Artifact storage counted", `AS "Walked"`),
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
	// features and artifacts are the statements of "Security features" and
	// "Artifact storage counted".
	features, artifacts string
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

	holdToTheNewestReading(ctx, t, s, database, stale, panels)
}

// holdToTheNewestReading holds the two tables the documentation audit of
// 2.6.0 found taking MAX() over the range to the newest row of each: the
// reading after the sweep's that found a feature switched off and its alerts
// gone, and the sweep's own artifact total past an older one that walked
// further and held more.
func holdToTheNewestReading(ctx context.Context, t *testing.T, s *Stack, database string, stale staleWrite, panels currentPanels) {
	t.Helper()
	rows, err := influxSQL(ctx, s, database, panels.features)
	if err != nil {
		t.Fatalf("Security features: %v", err)
	}
	i := slices.IndexFunc(rows, func(row map[string]any) bool {
		return row["Repository"] == stale.feature.repo && row["Feature"] == stale.feature.name
	})
	if i < 0 {
		t.Fatalf("Security features lists %v, and not %s of %s", rows, stale.feature.name, stale.feature.repo)
	}
	// The flag and the count as the newest reading has them, not the largest
	// the range held: a feature switched off inside the range read as on.
	wantNumber(t, rows[i], "Enabled", stale.feature.enabled)
	wantNumber(t, rows[i], "Open alerts", stale.feature.open)

	rows, err = influxSQL(ctx, s, database, panels.artifacts)
	if err != nil {
		t.Fatalf("Artifact storage counted: %v", err)
	}
	i = slices.IndexFunc(rows, func(row map[string]any) bool { return row["Repository"] == stale.artifacts.repo })
	if i < 0 {
		t.Fatalf("Artifact storage counted lists %v, and not %s", rows, stale.artifacts.repo)
	}
	wantNumber(t, rows[i], "Walked", stale.artifacts.walked)
	wantNumber(t, rows[i], "Live size", stale.artifacts.live)
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
	// feature is the security feature a reading newer than the sweep's finds
	// switched off, and artifacts the repository an older row says walked
	// further and held more; each carries what its newest row says.
	feature   staleFeature
	artifacts staleArtifacts
}

// staleFeature is one repository's security feature as its newest row has
// it: enabled as 1 or 0, the way the table casts it, and its open alerts.
type staleFeature struct {
	repo, name    string
	enabled, open float64
}

// staleArtifacts is one repository's artifact total as the sweep read it.
type staleArtifacts struct {
	repo         string
	walked, live float64
}

// staleRows are the rows checking 2.6.0 against GitHub panel by panel found
// the dashboards reading as current, made from the sweep's own so that each
// carries exactly the tag set the store keys that table by: the one row of an
// archived repository the collector no longer writes, nine days old, as a
// backfill under an earlier configuration left
// jmrplens/portainer-mcp-enhanced; an older row of the repository set aside
// with more stars than it has now, as a backfill left jmrplens/FFT2octave at
// 4 where GitHub said 3; and a cache on a ref GitHub evicted three days ago,
// as 108 refs of jmrplens/gitlab-mcp-server's golangci-lint were. Then the
// two the documentation audit of 2.6.0 found: a reading of a security feature
// taken after the sweep's, which finds it switched off and its alerts gone,
// so that the sweep's reading is the older one and the one MAX() kept; and an
// older artifact total that walked further and held more than the sweep's.
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
	newestReadingRows(t, points, now, &out)
	return out
}

// newestReadingRows adds the two rows of the documentation audit to what
// staleRows writes: a reading of a security feature taken after the sweep's,
// which finds it switched off and its alerts gone, and an older artifact
// total that walked further and held more than the sweep's.
func newestReadingRows(t *testing.T, points []sqlStoresPoint, now time.Time, out *staleWrite) {
	t.Helper()
	var feature, artifacts *sqlStoresPoint
	for i := range points {
		p := &points[i]
		switch {
		case feature == nil && p.Measurement == "gh_security_feature" && p.Fields["enabled"] == true &&
			p.Fields["open_alerts"] != 0.0:
			// A feature the sweep read as on and with alerts open, so that the
			// newer reading switching it off changes both columns.
			feature = p
		case artifacts == nil && p.Measurement == "gh_artifact_total":
			artifacts = p
		}
	}
	if feature == nil || artifacts == nil {
		t.Fatalf("the sweep wrote no security feature that is on with alerts open (%v) or no "+
			"artifact total (%v)", feature != nil, artifacts != nil)
	}
	out.feature = staleFeature{repo: feature.Tags["repo"], name: feature.Tags["feature"]}
	switchedOff := map[string]any{"enabled": false, "open_alerts": int64(0)}
	if url, ok := feature.Fields["url"].(string); ok {
		// The same url, so that the reading is the same row of the table and
		// not one a grouping on the url would split off.
		switchedOff["url"] = url
	}
	walked, _ := artifacts.Fields["walked"].(float64)
	live, _ := artifacts.Fields["live_bytes"].(float64)
	declared, _ := artifacts.Fields["count"].(float64)
	liveCount, _ := artifacts.Fields["live_count"].(float64)
	out.artifacts = staleArtifacts{repo: artifacts.Tags["repo"], walked: walked, live: live}
	out.rows = append(out.rows,
		sink.Point{
			// After the sweep's own reading, since now is read once the sweep
			// has finished, and inside the range the statement is rendered
			// with, which is open at its end.
			Measurement: "gh_security_feature", Tags: feature.Tags, Fields: switchedOff,
			Time: now,
		},
		sink.Point{
			Measurement: "gh_artifact_total", Tags: artifacts.Tags,
			Fields: map[string]any{
				"walked": int64(walked) + 7, "count": int64(declared) + 7,
				"live_bytes": int64(live) + 1000, "live_count": int64(liveCount) + 3,
			},
			Time: now.Add(-2 * 24 * time.Hour),
		},
	)
}

// influxAwaitStale waits until each row staleRows wrote can be read back, so
// that a panel that reads past them is reading past rows that are there.
func influxAwaitStale(ctx context.Context, t *testing.T, s *Stack, database string, stale staleWrite) {
	t.Helper()
	for _, q := range []string{
		"SELECT stars FROM gh_repo_total WHERE full_name = '" + stale.gone + "'",
		"SELECT stars FROM gh_repo_total WHERE full_name = '" + fakegh.SetAside + "' AND time < now() - INTERVAL '1 day'",
		"SELECT caches FROM gh_actions_cache_entry WHERE ref = 'refs/pull/1/merge'",
		"SELECT open_alerts FROM gh_security_feature WHERE repo = '" + stale.feature.repo +
			"' AND feature = '" + stale.feature.name + "' AND NOT enabled",
		"SELECT walked FROM gh_artifact_total WHERE time < now() - INTERVAL '1 day'",
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
