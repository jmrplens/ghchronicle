package collect

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// policyRepos are the four repositories the fixture answers for, in alias
// order. Between them they cover every case there is: the file in .github, the
// same file at the root, a file that was deleted, and a repository that never
// had one.
var policyRepos = []Repo{
	{Owner: "jmrplens", Name: "jmrp.io", FullName: "jmrplens/jmrp.io"},
	{Owner: "jmrplens", Name: "Cloudflare-DNS-Updater", FullName: "jmrplens/Cloudflare-DNS-Updater"},
	{Owner: "jmrplens", Name: "mcp.jmrp.io", FullName: "jmrplens/mcp.jmrp.io"},
	{Owner: "jmrplens", Name: "gitlab-mcp-server", FullName: "jmrplens/gitlab-mcp-server"},
}

func servePolicyFiles(f *fixtureServer, seen *[]string) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if seen != nil {
			*seen = append(*seen, query)
		}
		f.write(w, "graphql_policy_files.json")
	})
}

func TestPolicyFilesAreDatedAtTheLastCommitThatTouchedThem(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	servePolicyFiles(f, nil)

	points, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_policy_file", "gh_dependabot_ecosystem")
	// One row per file per repository, whether the file is there or not: the
	// absence is the fact worth querying.
	if got := len(only(t, points, "gh_policy_file")); got != len(policyFileSet)*len(policyRepos) {
		t.Fatalf("got %d policy rows, want one per file per repository", got)
	}

	dependabot := find(t, points, "gh_policy_file", map[string]string{
		"full_name": "jmrplens/jmrp.io", "file": "dependabot",
	})
	if dependabot.Fields["present"] != true {
		t.Error("jmrp.io carries .github/dependabot.yml")
	}
	if got := fieldInt(t, dependabot, "bytes"); got != 917 {
		t.Errorf("bytes = %d, want 917", got)
	}
	// The whole life of the path, on the first sweep and without a backfill.
	if got := fieldInt(t, dependabot, "changes"); got != 2 {
		t.Errorf("changes = %d, want 2", got)
	}
	if !dependabot.Time.Equal(time.Date(2026, 8, 28, 21, 58, 24, 0, time.UTC)) {
		t.Errorf("stamped %s, want the last commit that touched the path", dependabot.Time)
	}
	if dependabot.Fields["path"] != ".github/dependabot.yml" {
		t.Errorf("path = %v", dependabot.Fields["path"])
	}
}

func TestPolicyFilesPickThePathThatIsInForce(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	servePolicyFiles(f, nil)

	points, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// GitHub reads CODEOWNERS from .github first and from the root second, and
	// only one of the two is in force. Both repositories have exactly one, at
	// different paths, and the row must name the one that exists.
	inDotGithub := find(t, points, "gh_policy_file", map[string]string{
		"full_name": "jmrplens/jmrp.io", "file": "codeowners",
	})
	if inDotGithub.Fields["path"] != ".github/CODEOWNERS" || inDotGithub.Fields["present"] != true {
		t.Errorf("codeowners = %v at %v", inDotGithub.Fields["present"], inDotGithub.Fields["path"])
	}
	atRoot := find(t, points, "gh_policy_file", map[string]string{
		"full_name": "jmrplens/gitlab-mcp-server", "file": "codeowners",
	})
	if atRoot.Fields["path"] != "CODEOWNERS" || atRoot.Fields["present"] != true {
		t.Errorf("codeowners = %v at %v", atRoot.Fields["present"], atRoot.Fields["path"])
	}
	if got := fieldInt(t, atRoot, "bytes"); got != 908 {
		t.Errorf("bytes = %d, want the root file's 908", got)
	}
}

func TestPolicyFilesDateADeletedFileAtItsDeletion(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	servePolicyFiles(f, nil)

	points, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Cloudflare-DNS-Updater has no .github/dependabot.yml today and two
	// commits against that path, the last on 2025-12-29. The audit's own table
	// reported no date for it; the API has one, and it is the date the file
	// stopped existing.
	gone := find(t, points, "gh_policy_file", map[string]string{
		"full_name": "jmrplens/Cloudflare-DNS-Updater", "file": "dependabot",
	})
	if gone.Fields["present"] != false {
		t.Error("the file is not there")
	}
	if got := fieldInt(t, gone, "changes"); got != 2 {
		t.Errorf("changes = %d, want the history of a path with no blob", got)
	}
	if !gone.Time.Equal(time.Date(2025, 12, 29, 11, 47, 20, 0, time.UTC)) {
		t.Errorf("stamped %s, want the commit that removed it", gone.Time)
	}
	if got := fieldInt(t, gone, "bytes"); got != 0 {
		t.Errorf("bytes = %d, want 0", got)
	}
}

func TestPolicyFilesStampANeverWrittenFileAtTheStartOfTheDay(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	servePolicyFiles(f, nil)

	points, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// mcp.jmrp.io has no dependabot.yml and never had one, so there is no date
	// of its own to carry. The start of the UTC day keeps a sweep every six
	// hours from writing four copies of "still absent".
	never := find(t, points, "gh_policy_file", map[string]string{
		"full_name": "jmrplens/mcp.jmrp.io", "file": "dependabot",
	})
	if never.Fields["present"] != false || fieldInt(t, never, "changes") != 0 {
		t.Errorf("present/changes = %v/%v", never.Fields["present"], never.Fields["changes"])
	}
	if !never.Time.Equal(startOfDay(testNow)) {
		t.Errorf("stamped %s, want the start of the day", never.Time)
	}
	// This is the crossing the audit was after: no dependabot.yml and no
	// blocks, which no measurement could ask about before.
	if got := fieldInt(t, never, "blocks"); got != 0 {
		t.Errorf("blocks = %d, want 0", got)
	}
	for _, p := range points {
		if p.Measurement == "gh_dependabot_ecosystem" && p.Tags["full_name"] == "jmrplens/mcp.jmrp.io" {
			t.Error("a repository with no dependabot.yml has no ecosystems")
		}
	}
}

func TestDependabotEcosystemsCountTheBlocks(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	servePolicyFiles(f, nil)

	points, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// jmrp.io: two blocks, npm and github-actions, both weekly.
	npm := find(t, points, "gh_dependabot_ecosystem", map[string]string{
		"full_name": "jmrplens/jmrp.io", "ecosystem": "npm",
	})
	if npm.Tags["interval"] != "weekly" || fieldInt(t, npm, "blocks") != 1 {
		t.Errorf("npm = %v every %q", npm.Fields["blocks"], npm.Tags["interval"])
	}
	// Dated with the file, not with the sweep: the configuration is as old as
	// the commit that wrote it.
	if !npm.Time.Equal(time.Date(2026, 8, 28, 21, 58, 24, 0, time.UTC)) {
		t.Errorf("stamped %s, want the date of the file it was parsed from", npm.Time)
	}
	dependabot := find(t, points, "gh_policy_file", map[string]string{
		"full_name": "jmrplens/jmrp.io", "file": "dependabot",
	})
	if got := fieldInt(t, dependabot, "blocks"); got != 2 {
		t.Errorf("blocks = %d, want 2", got)
	}
	if got := fieldInt(t, dependabot, "ecosystems"); got != 2 {
		t.Errorf("ecosystems = %d, want 2", got)
	}

	// gitlab-mcp-server: five ecosystems, all weekly, one block each.
	ecosystems := map[string]bool{}
	for _, p := range only(t, points, "gh_dependabot_ecosystem") {
		if p.Tags["full_name"] == "jmrplens/gitlab-mcp-server" {
			ecosystems[p.Tags["ecosystem"]] = true
		}
	}
	for _, want := range []string{"docker", "docker-compose", "github-actions", "gomod", "npm"} {
		if !ecosystems[want] {
			t.Errorf("no %s block, got %v", want, ecosystems)
		}
	}
}

func TestPolicyFilesAskOneQueryForTheWholeSweep(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	servePolicyFiles(f, &queries)

	if _, err := (PolicyFiles{Repos: policyRepos}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	// Four repositories, five paths each, one query and one point of budget.
	if len(queries) != 1 {
		t.Fatalf("%d queries, want one", len(queries))
	}
	for _, path := range []string{".github/dependabot.yml", ".github/CODEOWNERS", "CODEOWNERS", "SECURITY.md", ".github/FUNDING.yml"} {
		if !strings.Contains(queries[0], fmt.Sprintf("%q", "HEAD:"+path)) {
			t.Errorf("the query does not ask for %s", path)
		}
	}
	// The blob text is only worth carrying where it is parsed, which is
	// dependabot.yml alone: the same expression against
	// copilot-instructions.md returns 25 KB, and CODEOWNERS, asked for and
	// never read, was 3.1 KB of a 29.4 KB answer.
	if strings.Count(queries[0], "byteSize isTruncated text") != 1 {
		t.Error("text belongs to dependabot.yml only")
	}
	// The end-to-end fake picks a fixture by the first marker in the query
	// text, and `history(` is the commit walk's. This one writes the space.
	if strings.Contains(queries[0], "history(") || !strings.Contains(queries[0], "history (first: 1") {
		t.Error("the history field must not spell another collector's marker")
	}
	if strings.Contains(queries[0], "pullRequests(") {
		t.Error("the query must not contain another collector's marker")
	}
}

func TestPolicyFilesBatchTheRepositories(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	servePolicyFiles(f, &queries)

	repos := make([]Repo, 12)
	for i := range repos {
		repos[i] = Repo{Owner: "jmrplens", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("jmrplens/r%d", i)}
	}
	if _, err := (PolicyFiles{Repos: repos}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 3 {
		t.Errorf("%d queries for 12 repositories, want batches of five", len(queries))
	}
}

func TestPolicyFilesWithoutRepositoriesAskNothing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	servePolicyFiles(f, nil)
	points, err := PolicyFiles{}.Collect(ctx(t), f.Client, testNow)
	if err != nil || points != nil {
		t.Fatalf("points = %v, err = %v", points, err)
	}
	if n := len(f.calls("/graphql")); n != 0 {
		t.Errorf("%d queries for no repositories", n)
	}
}

func TestPolicyFilesReportAFailedQuery(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		// A spent budget, which is the refusal that must never be read as
		// "there is nothing here": asking again for less would only spend
		// what is left of it.
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	})
	_, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMITED") {
		t.Fatalf("err = %v, want the GraphQL error", err)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("%d queries, want the batch not retried on an error that is not recoverable", n)
	}
}

func TestPolicyFilesKeepTheBatchWhenOneRepositoryIsGone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	gone := `{"data":{"r0":null},"errors":[{"type":"NOT_FOUND","path":["r0"],` +
		`"message":"Could not resolve to a Repository"}]}`
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		// The measured wire shape, 2026-09-10: a batch carrying one
		// repository that no longer resolves answers HTTP 200 with the data
		// for every other alias, null for that one, and an errors array
		// naming it. The client reports the error and drops the body with it,
		// so the batch has to be asked for one repository at a time or the
		// three that do exist are lost on every sweep.
		if len(queries) == 1 || strings.Contains(query, "Cloudflare-DNS-Updater") {
			_, _ = w.Write([]byte(gone))
			return
		}
		f.write(w, "graphql_policy_files.json")
	})

	points, err := PolicyFiles{Repos: policyRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("one repository that is gone is not a failure: %v", err)
	}
	if len(queries) != 1+len(policyRepos) {
		t.Fatalf("%d queries, want the refused batch asked for one repository at a time", len(queries))
	}
	// Three repositories of four, one row per file each. The old behavior
	// threw all four away along with the error.
	if got := len(only(t, points, "gh_policy_file")); got != 3*len(policyFileSet) {
		t.Errorf("got %d policy rows, want the three repositories that answered", got)
	}
}

func TestDependabotEcosystemsIgnoreAFileThatDoesNotParse(t *testing.T) {
	t.Parallel()
	// A file Dependabot cannot read is a file Dependabot is not acting on, and
	// the policy row already records that it is there.
	if got := dependabotEcosystems("updates: [ this is not yaml"); got != nil {
		t.Errorf("got %v, want nothing", got)
	}
	if got := dependabotEcosystems(""); got != nil {
		t.Errorf("got %v, want nothing", got)
	}
	// A block with no schedule still names an ecosystem, and both tags are
	// written on every point.
	got := dependabotEcosystems("version: 2\nupdates:\n  - package-ecosystem: gomod\n")
	if got[dependabotEcosystem{ecosystem: "gomod", interval: noneTag}] != 1 {
		t.Errorf("got %v, want a gomod block whose interval falls back", got)
	}
}
