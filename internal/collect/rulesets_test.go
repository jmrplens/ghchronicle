package collect

import (
	"net/http"
	"testing"
	"time"
)

const (
	rulesetListPath    = "/repos/octocat/hello-world/rulesets"
	rulesetHistoryPath = "/repos/octocat/hello-world/rulesets/21/history"
	tagHistoryPath     = "/repos/octocat/hello-world/rulesets/22/history"
)

func rulesetRoutes(f *fixtureServer) {
	f.file(rulesetListPath, "rulesets.json")
	f.file(rulesetHistoryPath, "ruleset_history.json")
	f.file(tagHistoryPath, "ruleset_history_single.json")
}

// TestRulesetVersionsAreDatedWhenTheyWereSaved is the dating rule for the
// changelog: one row per saved version at the moment GitHub saved it, never at
// the sweep, and the actor that saved it on the row.
func TestRulesetVersionsAreDatedWhenTheyWereSaved(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rulesetRoutes(f)

	points, err := RulesetHistory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_ruleset_version")
	versions := only(t, points, "gh_ruleset_version")
	if got := len(versions); got != 4 {
		t.Fatalf("got %d versions, want the three of the branch ruleset and the one of the tag ruleset", got)
	}

	// The newest version of the branch ruleset, saved at 23:50 in a zone two
	// hours ahead of UTC: the row is stamped at that instant, 21:50Z, and not
	// at the day or at the sweep.
	newest := find(t, points, "gh_ruleset_version", map[string]string{
		"ruleset": "protect main", "actor_type": "user",
	})
	if want := time.Date(2026, 9, 7, 21, 50, 57, 568000000, time.UTC); !newest.Time.Equal(want) {
		t.Errorf("newest version is dated %s, want %s", newest.Time, want)
	}
	if newest.Tags["target"] != "branch" || newest.Tags["full_name"] != "octocat/hello-world" {
		t.Errorf("tags = %v", newest.Tags)
	}
	if got := fieldInt(t, newest, "version_id"); got != 48940500 {
		t.Errorf("version_id = %d, want 48940500", got)
	}
	if got := fieldInt(t, newest, "ruleset_id"); got != 21 {
		t.Errorf("ruleset_id = %d, want 21", got)
	}
	if got := fieldInt(t, newest, "actor_id"); got != 583231 {
		t.Errorf("actor_id = %d, want 583231", got)
	}
	if got := newest.Fields["url"]; got != "https://github.com/octocat/hello-world/rules/21" {
		t.Errorf("url = %v, want the ruleset's own page", got)
	}

	// The actor's type is the series and its id is not: an integration that
	// edited the rule is its own row and the person's id rides as a field.
	app := find(t, points, "gh_ruleset_version", map[string]string{
		"ruleset": "protect main", "actor_type": "integration",
	})
	if got := fieldInt(t, app, "actor_id"); got != 29110 {
		t.Errorf("integration actor_id = %d, want 29110", got)
	}
	if want := time.Date(2026, 5, 6, 21, 6, 10, 942000000, time.UTC); !app.Time.Equal(want) {
		t.Errorf("oldest version is dated %s, want %s", app.Time, want)
	}

	tag := find(t, points, "gh_ruleset_version", map[string]string{"ruleset": "tag policy"})
	if tag.Tags["target"] != "tag" || fieldInt(t, tag, "ruleset_id") != 22 {
		t.Errorf("tag ruleset row = %v %v", tag.Tags, tag.Fields)
	}

	// One list and one history per ruleset: the cost the documentation
	// promises.
	if got := len(f.calls(rulesetListPath)); got != 1 {
		t.Errorf("listed the rulesets %d times, want once", got)
	}
	for _, path := range []string{rulesetHistoryPath, tagHistoryPath} {
		if got := len(f.calls(path)); got != 1 {
			t.Errorf("read %s %d times, want once", path, got)
		}
	}
}

// TestRulesetHistoryStopsAtTheWalkBound is what keeps a backfill from walking
// past the window it was given, and a sweep from writing versions older than
// the one it was asked for.
func TestRulesetHistoryStopsAtTheWalkBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	rulesetRoutes(f)

	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	points, err := RulesetHistory{Walk: Walk{Pages: -1, Since: since}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range only(t, points, "gh_ruleset_version") {
		if p.Time.Before(since) {
			t.Errorf("a version dated %s is older than the bound %s", p.Time, since)
		}
	}
	if got := len(points); got != 3 {
		t.Errorf("got %d versions inside the bound, want 3", got)
	}
}

// TestRulesetHistoryTreatsNothingHereAsNothing is the rule every collector
// obeys: a repository whose rulesets this token cannot see, or a ruleset
// deleted between the list and the read, is not a failure of the sweep.
func TestRulesetHistoryTreatsNothingHereAsNothing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status(rulesetListPath, http.StatusNotFound, "Not Found")

	points, err := RulesetHistory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Fatalf("a 404 on the list gave %d points and %v, want nothing and no error", len(points), err)
	}

	// The list answers and one history is gone: the other ruleset's versions
	// are still written.
	g := newFixtureServer(t)
	g.file(rulesetListPath, "rulesets.json")
	g.status(rulesetHistoryPath, http.StatusNotFound, "Not Found")
	g.file(tagHistoryPath, "ruleset_history_single.json")
	points, err = RulesetHistory{}.Collect(ctx(t), g.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(only(t, points, "gh_ruleset_version")); got != 1 {
		t.Errorf("got %d versions with one history gone, want the other ruleset's one", got)
	}
}
