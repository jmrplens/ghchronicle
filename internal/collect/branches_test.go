package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// branchRepos are the two repositories graphql_branches.json was captured
// from, in the alias order the fixture uses.
var branchRepos = []Repo{
	{Owner: "jmrplens", Name: "jmrp.io", FullName: "jmrplens/jmrp.io"},
	{Owner: "jmrplens", Name: "Cloudflare-DNS-Updater", FullName: "jmrplens/Cloudflare-DNS-Updater"},
}

func TestBranches(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		f.write(w, "graphql_branches.json")
	})

	points, err := Branches{Repos: branchRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_branch")

	// Two repositories in one query, which is the whole point of the alias
	// form: the nested form of the same question was measured at cost 101.
	if len(queries) != 1 {
		t.Fatalf("%d queries for 2 repositories, want 1", len(queries))
	}
	if strings.Contains(queries[0], "associatedPullRequests") {
		t.Error("the pull request connection turns cost 1 into cost 14; the join belongs in the panel")
	}
	if !strings.Contains(queries[0], `refPrefix: "refs/heads/"`) {
		t.Errorf("query does not ask for heads: %s", queries[0])
	}
	if strings.Contains(queries[0], "orderBy") {
		t.Error("refs orderBy does not order heads, measured; the client sorts instead")
	}

	if got := len(only(t, points, "gh_branch")); got != 6 {
		t.Errorf("got %d branch rows, want 4 + 2", got)
	}
	checkBranchIdentity(t, points)
	checkBranchTipAge(t, points)
}

// checkBranchIdentity is the assertion the measurement exists for: every
// branch has to survive as its own row. The name is a tag because it is the
// identity, exactly as gh_pull_request tags its number; with the name in a
// field the four branches of jmrp.io share one series at one timestamp and
// InfluxDB keeps whichever was written last.
func checkBranchIdentity(t *testing.T, points []sink.Point) {
	t.Helper()
	seen := map[string]bool{}
	for _, p := range points {
		if p.Measurement != "gh_branch" {
			continue
		}
		// The series key is everything up to the first space of the line:
		// measurement and tag set, which is what InfluxDB keys a point by
		// together with its timestamp.
		key, _, _ := strings.Cut(sink.LineProtocol(p), " ")
		if seen[key] {
			t.Errorf("two branches share the series %q, so one of them is lost", key)
		}
		seen[key] = true
		if hasField(p, "branch") {
			t.Error("branch is a tag and must not also be a field: InfluxDB rejects the batch")
		}
	}
	def := find(t, points, "gh_branch", map[string]string{"repo": "jmrp.io", "branch": "main"})
	if def.Tags["is_default"] != "true" {
		t.Errorf("main is the default branch of jmrp.io: %v", def.Tags)
	}
	side := find(t, points, "gh_branch", map[string]string{"repo": "jmrp.io", "branch": "chore/deps-update"})
	if side.Tags["is_default"] != "false" {
		t.Errorf("is_default has to be written on every row, not only the default one: %v", side.Tags)
	}
	if side.Fields["oid"] != "94f0e6c2b850736885f8e3f1a66fb5ba35bbee4f" {
		t.Errorf("oid = %v", side.Fields["oid"])
	}
	if fieldInt(t, side, "branches") != 1 {
		t.Errorf("branches = %v", side.Fields)
	}
}

// checkBranchTipAge reads the number the measurement is for: how long a branch
// has been sitting there. Cloudflare-DNS-Updater's fix branch was last
// committed to on 2026-05-09, which is 122 days before testNow.
func checkBranchTipAge(t *testing.T, points []sink.Point) {
	t.Helper()
	stale := find(t, points, "gh_branch", map[string]string{
		"repo": "Cloudflare-DNS-Updater", "branch": "fix/prefer-local-ipv6-fallback",
	})
	if got := fieldInt(t, stale, "days_since_commit"); got != 122 {
		t.Errorf("days_since_commit = %d, want 122", got)
	}
	// An inventory row is stamped at the start of the UTC day, not at the
	// commit it points at: the tip moves and the branch is deleted, so a row
	// dated at the commit would scatter one branch across the year.
	day := startOfDay(testNow)
	for _, p := range only(t, points, "gh_branch") {
		if !p.Time.Equal(day) {
			t.Fatalf("branch %s stamped %s, want the start of the day %s", p.Tags["branch"], p.Time, day)
		}
	}
}

func TestBranchesBatchesAndSurvivesAMissingRepository(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	// r1 of any batch is a repository that was renamed away: GraphQL answers
	// 200 with an errors array, which the client reports as an error for the
	// whole query. The collector has to halve and keep the others.
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		data := map[string]any{}
		aliases := 0
		for strings.Contains(query, fmt.Sprintf(" r%d:", aliases)) {
			aliases++
		}
		body := map[string]any{"data": data}
		for i := range aliases {
			if strings.Contains(query, fmt.Sprintf(`r%d: repository(owner: "octocat", name: "gone")`, i)) {
				data[fmt.Sprintf("r%d", i)] = nil
				body["errors"] = []any{map[string]string{
					"type":    "NOT_FOUND",
					"message": "Could not resolve to a Repository with the name 'octocat/gone'.",
				}}
				continue
			}
			data[fmt.Sprintf("r%d", i)] = map[string]any{
				"defaultBranchRef": map[string]string{"name": "main"},
				"refs": map[string]any{"totalCount": 1, "nodes": []any{
					map[string]any{"name": "main", "target": map[string]string{
						"oid": "abc123", "committedDate": "2026-09-01T00:00:00Z",
					}},
				}},
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	repos := []Repo{
		{Owner: "octocat", Name: "a", FullName: "octocat/a"},
		{Owner: "octocat", Name: "gone", FullName: "octocat/gone"},
		{Owner: "octocat", Name: "b", FullName: "octocat/b"},
		{Owner: "octocat", Name: "c", FullName: "octocat/c"},
	}
	points, err := Branches{Repos: repos, Batch: 4}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	// Three of the four repositories still produce their row.
	if got := len(only(t, points, "gh_branch")); got != 3 {
		t.Errorf("got %d rows, want one for each repository that still exists", got)
	}
	for _, name := range []string{"a", "b", "c"} {
		find(t, points, "gh_branch", map[string]string{"repo": name, "branch": "main"})
	}
	if len(queries) < 2 {
		t.Errorf("%d queries: the failed batch was never halved", len(queries))
	}
}

func TestBranchesWithoutRepositories(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := Branches{}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("empty sweep = %d points, %v", len(points), err)
	}
	if len(f.calls("/graphql")) != 0 {
		t.Error("no repositories must mean no query")
	}
}

func TestBranchesWithoutCommits(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// A repository with no commits has no defaultBranchRef at all. Reading
	// the name off a missing object would mark every branch as the default.
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"r0": map[string]any{
				"defaultBranchRef": nil,
				"refs": map[string]any{"totalCount": 1, "nodes": []any{
					map[string]any{"name": "wip", "target": map[string]any{}},
				}},
			},
		}})
	})
	points, err := Branches{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	p := only(t, points, "gh_branch")[0]
	if p.Tags["is_default"] != "false" {
		t.Errorf("no default branch means no row claims to be it: %v", p.Tags)
	}
	// A ref whose target is not a Commit has no date, and an age computed
	// from a zero time would read as twenty thousand days.
	if hasField(p, "days_since_commit") || hasField(p, "oid") {
		t.Errorf("a ref with no commit must not invent one: %v", p.Fields)
	}
	if !p.Time.Equal(startOfDay(testNow)) {
		t.Errorf("stamped %s", p.Time)
	}
}
