package collect

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// reposPagesAsked lists the page numbers a path was asked for, in order, so a
// walk can be checked against the exact pages it should have read.
func reposPagesAsked(f *fixtureServer, path string) []string {
	var out []string
	for _, r := range f.calls(path) {
		out = append(out, r.Query["page"])
	}
	return out
}

// reposFail answers a path with a 500, which is neither a feature switched off
// nor a boundary of the data, and so has to reach the caller.
func reposFail(f *fixtureServer, path string) {
	f.status(path, http.StatusInternalServerError, "boom")
}

// reposPaginationLimit answers a path the way the activity feeds refuse to
// page deeper: a 422 whose message is the boundary, not a failure.
func reposPaginationLimit(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write([]byte(`{"message":"In order to keep the API fast for everyone, pagination is limited for this resource."}`))
}

// reposGraphQLFailure is an error GraphQL answers with that is neither a
// repository gone away nor a request too large, so no batch recovers from it.
const reposGraphQLFailure = `{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`

// TestAWalkResolvesItsPageCountAgainstTheCollectorsDefault pins the three
// readings of Walk.Pages. Unbounded is what a backfill hands every collector,
// and read as a single page it would silently turn a backfill into a sweep.
func TestAWalkResolvesItsPageCountAgainstTheCollectorsDefault(t *testing.T) {
	t.Parallel()
	if got := Unbounded.limit(1); got != 100000 {
		t.Errorf("Unbounded.limit(1) = %d, want the ceiling that stands in for no bound", got)
	}
	if got := (Walk{}).limit(7); got != 7 {
		t.Errorf("a zero walk resolved to %d, want the collector's own default of 7", got)
	}
	if got := (Walk{Pages: 3}).limit(7); got != 3 {
		t.Errorf("a walk of three pages resolved to %d, want 3", got)
	}
}

// TestOnlyNothingHereIsSkippableInGraphQL holds the narrow reading the comment
// on isSkippableGraphQL asks for: a spent budget and an unexplained failure
// must keep failing, or every collector after them reports success with no
// points.
func TestOnlyNothingHereIsSkippableInGraphQL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error at all", nil, false},
		{"a feature switched off", &ghapi.UnavailableError{Path: "/x", Status: 404}, true},
		{"a statistic still being computed", &ghapi.NotReadyError{Path: "/x"}, true},
		{"a query the gateway gave up on", &ghapi.TooLargeError{Status: 502}, true},
		{"a repository renamed away", errors.New("graphql: NOT_FOUND: Could not resolve to a Repository"), true},
		{"a repository no longer visible", errors.New("graphql: FORBIDDEN: Resource not accessible"), true},
		{"a spent budget in the errors array", errors.New("graphql: RATE_LIMITED: API rate limit exceeded"), false},
		{"a spent budget typed by the client", &ghapi.RateLimitedError{Path: "/graphql", Resource: "graphql"}, false},
	} {
		if got := isSkippableGraphQL(tc.err); got != tc.want {
			t.Errorf("%s: isSkippableGraphQL = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPagesStopAtAnEmptyPageAndReportARealFailure covers the loop every REST
// walk of this package shares: an empty page ends it without asking for the
// next, a refusal or the pagination ceiling ends it quietly, and anything else
// is returned.
func TestPagesStopAtAnEmptyPageAndReportARealFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/walk", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(repeat(t, "releases.json", "", 100, nil))
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	visits := 0
	err := pages(ctx(t), f.Client, Walk{Pages: 5}, 1, func(page int) string {
		return "/walk?per_page=100&page=" + strconv.Itoa(page)
	}, func([]releaseRow) bool {
		visits++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := reposPagesAsked(f, "/walk"); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("asked for pages %v, want 1 and the empty 2 that ends the walk", got)
	}
	if visits != 1 {
		t.Errorf("visit ran %d times, want once: an empty page has nothing to visit", visits)
	}

	f.handle("/limited", func(w http.ResponseWriter, _ *http.Request) { reposPaginationLimit(w) })
	reposFail(f, "/broken")
	for path, wantErr := range map[string]bool{"/limited": false, "/missing": false, "/broken": true} {
		got := pages(ctx(t), f.Client, Walk{Pages: 5}, 1, func(int) string { return path },
			func([]releaseRow) bool { return true })
		if (got != nil) != wantErr {
			t.Errorf("%s: err = %v, want an error %v", path, got, wantErr)
		}
	}
}

// TestTheSBOMIsAPhotographOfTheDay pins what the photograph is stamped with
// and how it reads what GitHub could not say. The start of the UTC day is what
// makes two sweeps on one day rewrite one row; stamped at the sweep, every
// sweep that found a moved head would add a second copy of the same packages.
func TestTheSBOMIsAPhotographOfTheDay(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/dependency-graph/sbom", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sbom": map[string]any{"packages": []any{
			// No license at all, which counts with NOASSERTION as a blind spot.
			map[string]any{
				"name": "left-pad", "licenseConcluded": "",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:npm/left-pad@1.3.0"}},
			},
			// A purl with no namespace separator names its ecosystem whole.
			map[string]any{
				"name": "blob", "licenseConcluded": "MIT",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:generic"}},
			},
			// A reference that is not a purl says nothing about an ecosystem.
			map[string]any{
				"name": "vendored", "licenseConcluded": "MIT",
				"externalRefs": []any{map[string]any{"referenceLocator": "https://example.com/vendored.tar.gz"}},
			},
		}}})
	})

	// The head is given, so nothing asks the default branch for it.
	d := &Dependencies{Head: "head1234", SBOM: true}
	points, err := d.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := len(f.calls("/repos/octocat/hello-world/commits/HEAD")); n != 0 {
		t.Errorf("%d reads of the default branch with the head already given", n)
	}
	for _, p := range append(only(t, points, "gh_dependency"), only(t, points, "gh_dependency_license")...) {
		if !p.Time.Equal(startOfDay(testNow)) {
			t.Errorf("%s %v stamped %s, want the start of the UTC day %s", p.Measurement, p.Tags, p.Time, startOfDay(testNow))
		}
	}
	if p := find(t, points, "gh_dependency_license", map[string]string{"license": "undetermined"}); fieldInt(t, p, "packages") != 1 {
		t.Errorf("undetermined = %v, want the one package with no license", p.Fields)
	}
	find(t, points, "gh_dependency", map[string]string{"ecosystem": "npm"})
	find(t, points, "gh_dependency", map[string]string{"ecosystem": "generic"})
	find(t, points, "gh_dependency", map[string]string{"ecosystem": noneTag})
	if d.ResolvedHead != "head1234" {
		t.Errorf("ResolvedHead = %q, want the head that was given", d.ResolvedHead)
	}
}

// TestDependenciesReportWhatBrokeAndSkipWhatIsOff keeps the line between a
// dependency graph that is switched off, which is nothing to report, and one
// of its three reads failing, which the sweep has to hear about.
func TestDependenciesReportWhatBrokeAndSkipWhatIsOff(t *testing.T) {
	t.Parallel()
	const (
		head    = "/repos/octocat/hello-world/commits/HEAD"
		sbom    = "/repos/octocat/hello-world/dependency-graph/sbom"
		compare = "/repos/octocat/hello-world/dependency-graph/compare/base9876...head1234"
	)
	for _, tc := range []struct {
		name    string
		routes  func(f *fixtureServer)
		wantErr bool
	}{
		{"the default branch fails", func(f *fixtureServer) { reposFail(f, head) }, true},
		{"the SBOM fails", func(f *fixtureServer) { headRoute(f); reposFail(f, sbom) }, true},
		{"the compare fails", func(f *fixtureServer) { headRoute(f); reposFail(f, compare) }, true},
		{"the compare is off", func(f *fixtureServer) {
			headRoute(f)
			f.status(sbom, http.StatusNotFound, "Not Found")
			f.status(compare, http.StatusForbidden, "Dependency graph is disabled")
		}, false},
	} {
		f := newFixtureServer(t)
		tc.routes(f)
		d := &Dependencies{Base: "base9876", SBOM: true}
		points, err := d.Collect(ctx(t), f.Client, testRepo, testNow)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want an error %v", tc.name, err, tc.wantErr)
		}
		if !tc.wantErr && len(points) != 0 {
			t.Errorf("%s: %d points, want none from a graph that is off", tc.name, len(points))
		}
	}
}

// TestDependenciesWithABaseAndNoHeadStillPhotograph is the one case where the
// base is known and the head is not: the range cannot be computed, but the
// photograph is still due, and no range row is written against nothing.
func TestDependenciesWithABaseAndNoHeadStillPhotograph(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/commits/HEAD", http.StatusNotFound, "Not Found")
	f.handle("/repos/octocat/hello-world/dependency-graph/sbom", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sbom": map[string]any{"packages": []any{
			map[string]any{
				"name": "left-pad", "licenseConcluded": "MIT",
				"externalRefs": []any{map[string]any{"referenceLocator": "pkg:npm/left-pad@1.3.0"}},
			},
		}}})
	})
	d := &Dependencies{Base: "base9876", SBOM: true}
	points, err := d.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/dependency-graph/sbom")); n != 1 {
		t.Errorf("%d SBOM reads, want the photograph", n)
	}
	if got := byMeasurement(points)["gh_dependency_change"]; len(got) != 0 {
		t.Errorf("got %d range rows without a head, want none", len(got))
	}
	if d.ResolvedHead != "" {
		t.Errorf("ResolvedHead = %q, want empty so the next sweep asks again", d.ResolvedHead)
	}
}

// TestAFilterWantsALiveRepositoryByDefault pins the defaults the other way
// round from the discovery tests: the rules drop forks, archives and private
// repositories, and a repository that is none of the three passes untouched.
func TestAFilterWantsALiveRepositoryByDefault(t *testing.T) {
	t.Parallel()
	var f Filter
	plain := Repo{Owner: "octocat", Name: "hello-world", FullName: "octocat/hello-world"}
	if !f.wants(plain) {
		t.Error("a public, original, live repository was refused by the default filter")
	}
	// A repository that is not archived is never set aside as archived, which
	// is the second list of a Discovery and must hold nothing else.
	if f.archivedAside(plain) {
		t.Error("a live repository was set aside as archived")
	}
	for _, r := range []Repo{
		{FullName: "octocat/fork", Fork: true},
		{FullName: "octocat/archive", Archived: true},
		{FullName: "octocat/private", Private: true},
	} {
		if f.wants(r) {
			t.Errorf("%s passed a filter that includes nothing optional", r.FullName)
		}
	}
}

// TestDiscoverReadsTwentyFullPagesAndNoMore pins the ceiling on a listing. A
// full page means there may be another, and the twentieth is the last one
// asked for: two thousand repositories is the most a sweep will discover.
func TestDiscoverReadsTwentyFullPagesAndNoMore(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var mu sync.Mutex
	served := 0
	f.handle("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		n := served
		mu.Unlock()
		if n > 30 {
			// A walk that does not stop on its own is stopped here, so the
			// failure is a count and not a hung test.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		page := r.URL.Query().Get("page")
		_, _ = w.Write(repeat(t, "user_repos.json", "", 100, func(i int, row map[string]any) {
			row["name"] = fmt.Sprintf("p%s-r%d", page, i)
			row["full_name"] = fmt.Sprintf("octocat/p%s-r%d", page, i)
		}))
	})
	got, err := Discover(ctx(t), f.Client, &Filter{User: "octocat"})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 20)
	for i := range want {
		want[i] = strconv.Itoa(i + 1)
	}
	if asked := reposPagesAsked(f, "/user/repos"); !slices.Equal(asked, want) {
		t.Errorf("asked for pages %v, want 1 to 20", asked)
	}
	if len(got.Repos) != 2000 {
		t.Errorf("discovered %d repositories, want every row of twenty full pages", len(got.Repos))
	}
}

// TestDiscoverReportsABrokenListingWithWhatItRead holds both halves of the
// promise ownedRepos makes: a failed page is returned, and the repositories
// read before it are not lost with it. A listing that is not there is not a
// failure, and neither is a named repository the token cannot see; a named
// one that fails is.
func TestDiscoverReportsABrokenListingWithWhatItRead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(repeat(t, "user_repos.json", "", 100, func(i int, row map[string]any) {
			row["name"] = fmt.Sprintf("r%d", i)
			row["full_name"] = fmt.Sprintf("octocat/r%d", i)
		}))
	})
	got, err := Discover(ctx(t), f.Client, &Filter{User: "octocat"})
	if err == nil {
		t.Fatal("a 500 on the second page of the listing was not reported")
	}
	if len(got.Repos) != 100 {
		t.Errorf("kept %d repositories, want the 100 of the page that was read", len(got.Repos))
	}

	// An organization the token cannot list is skipped, not failed.
	quiet := newFixtureServer(t)
	quiet.status("/orgs/gone/repos", http.StatusNotFound, "Not Found")
	if _, quietErr := Discover(ctx(t), quiet.Client, &Filter{Orgs: []string{"gone"}}); quietErr != nil {
		t.Errorf("an organization that answers 404 failed the discovery: %v", quietErr)
	}

	named := newFixtureServer(t)
	reposFail(named, "/repos/octocat/broken")
	if _, namedErr := Discover(ctx(t), named.Client, &Filter{Repos: []string{"octocat/broken"}}); namedErr == nil {
		t.Error("a named repository that answered 500 was not reported")
	}
}

// TestPolicyResponsesTolerateMissingAndNullParts reads the alias map the way
// the gateway can actually send it: a blob alias missing, null, or of a shape
// that does not decode, and a default branch that is null because the
// repository is empty. None of those may cost the parts that did arrive.
func TestPolicyResponsesTolerateMissingAndNullParts(t *testing.T) {
	t.Parallel()
	raw := map[string]json.RawMessage{
		"f0":               json.RawMessage(`{"byteSize": 12, "isTruncated": false, "text": "version: 2"}`),
		"f2":               json.RawMessage(`null`),
		"f3":               json.RawMessage(`5`),
		"defaultBranchRef": json.RawMessage(`null`),
	}
	res := parsePolicyResponse(raw)
	if res.files[0] == nil || res.files[0].ByteSize != 12 {
		t.Errorf("f0 = %+v, want the blob that arrived", res.files[0])
	}
	for _, i := range []int{1, 2, 3} {
		if res.files[i] != nil {
			t.Errorf("f%d = %+v, want no blob for an alias that is missing, null or undecodable", i, res.files[i])
		}
	}
	if len(res.history) != 0 {
		t.Errorf("history = %v, want none from a null default branch", res.history)
	}

	// A node with no default branch key at all, as a fragment without the
	// history selection would answer, keeps its blobs and has no history.
	bare := parsePolicyResponse(map[string]json.RawMessage{"f0": raw["f0"]})
	if bare.files[0] == nil || len(bare.history) != 0 {
		t.Errorf("without defaultBranchRef: files %v, history %v; want the blob and no history", bare.files, bare.history)
	}

	undecodable := parsePolicyResponse(map[string]json.RawMessage{"defaultBranchRef": json.RawMessage(`"main"`)})
	if len(undecodable.history) != 0 {
		t.Errorf("history = %v, want none from a default branch that does not decode", undecodable.history)
	}

	partial := parsePolicyResponse(map[string]json.RawMessage{
		"defaultBranchRef": json.RawMessage(`{"target": {"h1": {"totalCount": 4, "nodes": []}}}`),
	})
	if len(partial.history) != 1 || partial.history[1] == nil || partial.history[1].TotalCount != 4 {
		t.Errorf("history = %v, want only h1, the one alias the target carried", partial.history)
	}
}

// TestPolicyFilePrecedenceAmongPaths pins choosePolicyBlob case by case. It is
// CODEOWNERS that has two paths, and GitHub reads .github/ first: a file at
// the wrong path does nothing, so which of the two speaks for the file is the
// whole meaning of the row.
func TestPolicyFilePrecedenceAmongPaths(t *testing.T) {
	t.Parallel()
	dotGithub, root := ".github/CODEOWNERS", "CODEOWNERS"
	for _, tc := range []struct {
		name  string
		found []policyBlob
		want  string
	}{
		{name: "none at all"},
		{
			name:  "both present, the second with the shorter history",
			found: []policyBlob{{path: dotGithub, present: true, changes: 5}, {path: root, present: true, changes: 2}},
			want:  dotGithub,
		},
		{
			name:  "a present file beats a deleted one with a longer history",
			found: []policyBlob{{path: dotGithub, present: true, changes: 1}, {path: root, changes: 9}},
			want:  dotGithub,
		},
		{
			name:  "a present second beats an absent first",
			found: []policyBlob{{path: dotGithub, changes: 3}, {path: root, present: true}},
			want:  root,
		},
		{
			name:  "neither ever existed, the first in precedence names the file",
			found: []policyBlob{{path: dotGithub}, {path: root}},
			want:  dotGithub,
		},
		{
			name:  "neither exists, the one with a history was deleted",
			found: []policyBlob{{path: dotGithub}, {path: root, changes: 3}},
			want:  root,
		},
	} {
		if got := choosePolicyBlob(tc.found); got.path != tc.want {
			t.Errorf("%s: chose %q, want %q", tc.name, got.path, tc.want)
		}
	}
}

// TestATruncatedDependabotFileIsPresentButNotRead is the 25 KB case the
// PolicyFiles comment warns about: GitHub cuts the text of a large blob, and a
// cut YAML file is not one to count blocks in. The row still says the file is
// there.
func TestATruncatedDependabotFileIsPresentButNotRead(t *testing.T) {
	t.Parallel()
	text := "version: 2\nupdates:\n  - package-ecosystem: npm\n    schedule:\n      interval: weekly\n"
	for _, truncated := range []bool{false, true} {
		res := policyResponse{
			files:   map[int]*policyBlobNode{0: {ByteSize: len(text), Text: text, IsTruncated: truncated}},
			history: map[int]*policyHistoryNode{},
		}
		points := policyPoints(testRepo, res, testNow)
		row := find(t, points, "gh_policy_file", map[string]string{"file": "dependabot"})
		if row.Fields["present"] != true {
			t.Errorf("truncated %v: present = %v, want true", truncated, row.Fields["present"])
		}
		wantBlocks := 1
		if truncated {
			wantBlocks = 0
		}
		if fieldInt(t, row, "blocks") != int64(wantBlocks) {
			t.Errorf("truncated %v: blocks = %v, want %d", truncated, row.Fields["blocks"], wantBlocks)
		}
		if got := len(byMeasurement(points)["gh_dependabot_ecosystem"]); got != wantBlocks {
			t.Errorf("truncated %v: %d ecosystem rows, want %d", truncated, got, wantBlocks)
		}
	}
}

// TestPolicyFilesKeepWhatAnsweredWhenABatchFails pins that a batch lost to an
// error nothing recovers from costs only its own repositories: the rows of
// the batch that answered are returned, and the sweep does not fail for it.
func TestPolicyFilesKeepWhatAnsweredWhenABatchFails(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, `name: "jmrp.io"`) {
			_, _ = w.Write([]byte(reposGraphQLFailure))
			return
		}
		f.write(w, "graphql_policy_files.json")
	})
	points, err := PolicyFiles{Repos: policyRepos[:2], Batch: 1}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("one failed batch beside one that answered: %v", err)
	}
	rows := only(t, points, "gh_policy_file")
	if len(rows) != len(policyFileSet) {
		t.Errorf("got %d policy rows, want one per file for the repository that answered", len(rows))
	}
	for _, p := range rows {
		if p.Tags["repo"] != "Cloudflare-DNS-Updater" {
			t.Errorf("row for %s, want only the repository whose batch answered", p.Tags["repo"])
		}
	}
}

// reposNullAliases answers every query with the one alias null and no
// errors, which is a batch that asked and heard nothing back.
func reposNullAliases(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"data":{"r0":null}}`))
}

// TestBatchedReadsThatHearNothingAreNotAFailure pins the other side of the
// rule both batched readers end on: no rows and no error is an empty answer,
// returned as nothing and not as a failure.
func TestBatchedReadsThatHearNothingAreNotAFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) { reposNullAliases(w) })
	if points, err := (PolicyFiles{Repos: []Repo{testRepo}}).Collect(ctx(t), f.Client, testNow); err != nil || len(points) != 0 {
		t.Errorf("policy files: %d points, err %v; want nothing and no error", len(points), err)
	}
	if points, err := (RepoDetail{Repos: []Repo{testRepo}}).Collect(ctx(t), f.Client, testNow); err != nil || len(points) != 0 {
		t.Errorf("repo detail: %d points, err %v; want nothing and no error", len(points), err)
	}
}

// TestRepoCoreAgesAreWholeDays reads the two ages the repository snapshot
// computes rather than stores: days since creation on gh_repo, days since
// publication on gh_release. The fixture and testNow fix both numbers.
func TestRepoCoreAgesAreWholeDays(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world", "repo.json")
	f.file("/repos/octocat/hello-world/releases", "releases.json")
	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// created_at 2011-01-26T19:01:12Z, five thousand seven hundred and three
	// whole days before the sweep.
	if got := fieldInt(t, only(t, points, "gh_repo")[0], "age_days"); got != 5703 {
		t.Errorf("gh_repo age_days = %d, want 5703", got)
	}
	// published_at 2026-08-10T09:30:00Z, twenty nine whole days before.
	rel := find(t, points, "gh_release", map[string]string{"tag": "v1.2.0"})
	if got := fieldInt(t, rel, "age_days"); got != 29 {
		t.Errorf("gh_release age_days = %d, want 29", got)
	}
}

// reposReleasePages serves full pages of a hundred releases for every page
// from one on, each dated a day further back than the page before, and an
// empty list for anything else.
func reposReleasePages(t *testing.T, f *fixtureServer) {
	t.Helper()
	f.handle("/repos/octocat/hello-world/releases", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		rows := make([]map[string]any, 100)
		for i := range rows {
			rows[i] = map[string]any{
				"tag_name":     fmt.Sprintf("v%d.%d", page, i),
				"published_at": testNow.AddDate(0, 0, -page).Format(time.RFC3339),
				"assets":       []any{},
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
}

// TestReleaseWalkFollowsFullPagesToItsCap is the backfill of a repository with
// more releases than a page holds: a full page asks for the next one, in
// order, until the walk's own cap.
func TestReleaseWalkFollowsFullPagesToItsCap(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world", "repo.json")
	reposReleasePages(t, f)
	points, err := RepoCore{Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := reposPagesAsked(f, "/repos/octocat/hello-world/releases"); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Errorf("asked for release pages %v, want 1, 2 and 3", got)
	}
	if got := len(only(t, points, "gh_release")); got != 300 {
		t.Errorf("got %d releases, want the 300 of three full pages", got)
	}
}

// TestReleaseWalkStopsOnAShortPageOrPastTheBound pins the two other ways the
// release walk ends before its cap: a page that is not full is the last one,
// and a full page whose oldest release predates the backfill bound is too.
func TestReleaseWalkStopsOnAShortPageOrPastTheBound(t *testing.T) {
	t.Parallel()
	short := newFixtureServer(t)
	short.file("/repos/octocat/hello-world", "repo.json")
	short.file("/repos/octocat/hello-world/releases", "releases.json")
	if _, err := (RepoCore{Walk: Walk{Pages: 3}}).Collect(ctx(t), short.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if n := len(short.calls("/repos/octocat/hello-world/releases")); n != 1 {
		t.Errorf("made %d release calls, a short first page ends the walk", n)
	}

	bounded := newFixtureServer(t)
	bounded.file("/repos/octocat/hello-world", "repo.json")
	reposReleasePages(t, bounded)
	walk := Walk{Pages: 3, Since: testNow.Add(-12 * time.Hour)}
	points, err := RepoCore{Walk: walk}.Collect(ctx(t), bounded.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(bounded.calls("/repos/octocat/hello-world/releases")); n != 1 {
		t.Errorf("made %d release calls, a page older than the bound ends the walk", n)
	}
	if got := len(only(t, points, "gh_release")); got != 100 {
		t.Errorf("got %d releases, want the whole page that was read", got)
	}
}

// TestRepoCoreReportsWhatBrokeAndSkipsWhatIsOff separates the surfaces of the
// snapshot that can fail from the one boundary that is not a failure: the
// release list refusing to page deeper.
func TestRepoCoreReportsWhatBrokeAndSkipsWhatIsOff(t *testing.T) {
	t.Parallel()
	const (
		repo      = "/repos/octocat/hello-world"
		community = "/repos/octocat/hello-world/community/profile"
		releases  = "/repos/octocat/hello-world/releases"
	)
	for _, tc := range []struct {
		name    string
		routes  func(f *fixtureServer)
		wantErr bool
	}{
		{"the repository fails", func(f *fixtureServer) { reposFail(f, repo) }, true},
		{"the community profile fails", func(f *fixtureServer) {
			f.file(repo, "repo.json")
			reposFail(f, community)
		}, true},
		{"the release list fails", func(f *fixtureServer) {
			f.file(repo, "repo.json")
			reposFail(f, releases)
		}, true},
		{"the release list is at its pagination ceiling", func(f *fixtureServer) {
			f.file(repo, "repo.json")
			f.handle(releases, func(w http.ResponseWriter, _ *http.Request) { reposPaginationLimit(w) })
		}, false},
	} {
		f := newFixtureServer(t)
		tc.routes(f)
		points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want an error %v", tc.name, err, tc.wantErr)
		}
		if !tc.wantErr && len(byMeasurement(points)["gh_repo"]) != 1 {
			t.Errorf("%s: measurements %v, want the snapshot kept", tc.name, measurements(points))
		}
	}
}

// TestASecuritySettingWithAnEmptyStatusIsUnavailable holds the fallback for a
// block that names a feature and says nothing about it: that is not
// "disabled", which is a state GitHub states in words.
func TestASecuritySettingWithAnEmptyStatusIsUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"full_name":"octocat/hello-world","security_and_analysis":{` +
			`"secret_scanning":{"status":""},"dependabot_security_updates":{"status":"enabled"}}}`))
	})
	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	empty := find(t, points, "gh_security_setting", map[string]string{"setting": "secret_scanning"})
	if empty.Tags["status"] != "unavailable" || empty.Fields["enabled"] != false {
		t.Errorf("an empty status reads %v %v, want unavailable and not enabled", empty.Tags, empty.Fields)
	}
	on := find(t, points, "gh_security_setting", map[string]string{"setting": "dependabot_security_updates"})
	if on.Tags["status"] != "enabled" || on.Fields["enabled"] != true {
		t.Errorf("an enabled status reads %v %v", on.Tags, on.Fields)
	}
}

// reposDetailRepos names n repositories for a RepoDetail batch.
func reposDetailRepos(n int) []Repo {
	out := make([]Repo, n)
	for i := range out {
		out[i] = Repo{Owner: "octocat", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("octocat/r%d", i)}
	}
	return out
}

// TestRepoDetailAsksTenAtATimeByDefault pins the default the type documents.
// Ten is the page the cost table on RepoDetail was measured at; the package
// default for alias batches is five, and falling through to it would double
// the queries of every sweep.
func TestRepoDetailAsksTenAtATimeByDefault(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	repoDetailServer(f, &queries)
	if _, err := (RepoDetail{Repos: reposDetailRepos(10)}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 {
		t.Errorf("%d queries for ten repositories and no batch size, want one", len(queries))
	}

	none := newFixtureServer(t)
	repoDetailServer(none, nil)
	points, err := RepoDetail{}.Collect(ctx(t), none.Client, testNow)
	if err != nil || points != nil {
		t.Errorf("no repositories: points %v, err %v; want nothing", points, err)
	}
	if n := len(none.calls("/graphql")); n != 0 {
		t.Errorf("%d queries for no repositories", n)
	}
}

// TestRepoDetailReportsAFailureOnlyWhenNothingAnswered pins both sides of the
// rule at the end of Collect: a sweep where every batch failed is a failure,
// and one where some batch answered keeps its rows and is not.
func TestRepoDetailReportsAFailureOnlyWhenNothingAnswered(t *testing.T) {
	t.Parallel()
	failing := newFixtureServer(t)
	failing.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(reposGraphQLFailure))
	})
	points, err := RepoDetail{Repos: reposDetailRepos(2)}.Collect(ctx(t), failing.Client, testNow)
	if err == nil || !strings.Contains(err.Error(), "INTERNAL") {
		t.Errorf("err = %v, want the GraphQL failure when no batch answered", err)
	}
	if points != nil {
		t.Errorf("got %d points from queries that all failed", len(points))
	}

	partial := newFixtureServer(t)
	partial.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, `name: "r0"`) {
			_, _ = w.Write([]byte(reposGraphQLFailure))
			return
		}
		answerRepoDetail(w, query)
	})
	points, err = RepoDetail{Repos: reposDetailRepos(2), Batch: 1}.Collect(ctx(t), partial.Client, testNow)
	if err != nil {
		t.Fatalf("one failed batch beside one that answered: %v", err)
	}
	for _, p := range only(t, points, "gh_repo_language") {
		if p.Tags["repo"] != "r1" {
			t.Errorf("language row for %s, want only r1, whose batch answered", p.Tags["repo"])
		}
	}
}

// TestARulesetWithoutConditionsAppliesEverywhere reads the two shapes a
// ruleset's conditions come in when they say nothing: no conditions object at
// all, and one whose refName is null. Both mean every ref, and both must give
// a row with empty patterns rather than a crash. The same fixture fixes the
// age of the ruleset in whole days.
func TestARulesetWithoutConditionsAppliesEverywhere(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"r0": map[string]any{
			"nameWithOwner": "octocat/hello-world",
			"url":           "https://github.com/octocat/hello-world",
			"rulesets": map[string]any{"nodes": []any{
				map[string]any{
					"name": "No conditions", "target": "BRANCH", "enforcement": "ACTIVE",
					"updatedAt":    "2026-06-01T00:00:00Z",
					"rules":        map[string]any{"nodes": []any{map[string]any{"type": "DELETION"}}},
					"conditions":   nil,
					"bypassActors": map[string]any{"totalCount": 0, "nodes": []any{}},
				},
				map[string]any{
					"name": "Null ref name", "target": "BRANCH", "enforcement": "ACTIVE",
					"updatedAt":    "2026-06-01T00:00:00Z",
					"rules":        map[string]any{"nodes": []any{map[string]any{"type": "DELETION"}}},
					"conditions":   map[string]any{"refName": nil},
					"bypassActors": map[string]any{"totalCount": 0, "nodes": []any{}},
				},
			}},
		}}})
	})
	points, err := RepoDetail{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	for _, name := range []string{"No conditions", "Null ref name"} {
		rule := find(t, points, "gh_ruleset_rule", map[string]string{"ruleset": name})
		if rule.Fields["ref_include"] != "" || rule.Fields["ref_exclude"] != "" {
			t.Errorf("%s: patterns %v / %v, want none", name, rule.Fields["ref_include"], rule.Fields["ref_exclude"])
		}
		// updatedAt 2026-06-01, ninety nine whole days before testNow.
		rs := find(t, points, "gh_ruleset", map[string]string{"ruleset": name})
		if got := fieldInt(t, rs, "days_since_change"); got != 99 {
			t.Errorf("%s: days_since_change = %d, want 99", name, got)
		}
	}
}

// TestRepoInventoryReportsWhatBrokeAndSkipsTheUnnamed covers the failures of
// the inventory's first two reads, a secret row with no name, which is not a
// secret anyone can act on, and a default setup whose date is the zero time,
// which is no date at all.
func TestRepoInventoryReportsWhatBrokeAndSkipsTheUnnamed(t *testing.T) {
	t.Parallel()
	const (
		policy  = "/repos/octocat/hello-world/actions/permissions/workflow"
		actions = "/repos/octocat/hello-world/actions/secrets"
	)
	for name, route := range map[string]string{"the workflow policy": policy, "the actions secrets": actions} {
		f := newFixtureServer(t)
		inventoryRoutes(f)
		reposFail(f, route)
		if _, err := (RepoInventory{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
			t.Errorf("a 500 on %s was not reported", name)
		}
	}

	f := newFixtureServer(t)
	inventoryRoutes(f)
	f.handle(actions, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":2,"secrets":[` +
			`{"name":"","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},` +
			`{"name":"DEPLOY_TOKEN","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`))
	})
	f.handle("/repos/octocat/hello-world/code-scanning/default-setup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"configured","languages":["go"],"updated_at":"0001-01-01T00:00:00Z"}`))
	})
	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	for _, p := range only(t, points, "gh_secret") {
		if p.Tags["kind"] == "actions" && p.Tags["secret"] != "DEPLOY_TOKEN" {
			t.Errorf("actions secret row %v, want only the one with a name", p.Tags)
		}
	}
	find(t, points, "gh_secret", map[string]string{"kind": "actions", "secret": "DEPLOY_TOKEN"})
	setup := only(t, points, "gh_code_scanning_setup")[0]
	if hasField(setup, "days_since_change") {
		t.Errorf("a zero updated_at wrote days_since_change = %v", setup.Fields["days_since_change"])
	}
}

// TestRulesetHistoryReportsWhatBroke keeps the list failing apart from a
// history failing, and the second hands back the versions read before it.
func TestRulesetHistoryReportsWhatBroke(t *testing.T) {
	t.Parallel()
	list := newFixtureServer(t)
	reposFail(list, "/repos/octocat/hello-world/rulesets")
	if _, err := (RulesetHistory{}).Collect(ctx(t), list.Client, testRepo, testNow); err == nil {
		t.Error("a 500 on the ruleset list was not reported")
	}

	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/rulesets", "rulesets.json")
	f.file("/repos/octocat/hello-world/rulesets/21/history", "ruleset_history.json")
	reposFail(f, "/repos/octocat/hello-world/rulesets/22/history")
	points, err := RulesetHistory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil {
		t.Fatal("a 500 on one ruleset's history was not reported")
	}
	if got := len(byMeasurement(points)["gh_ruleset_version"]); got != 3 {
		t.Errorf("kept %d versions, want the 3 of the history read before the failure", got)
	}
}

// reposDependabotPage serves a Dependabot page of n alerts, each older than
// the one before, with a next cursor when cursor is not empty.
func reposDependabotPage(t *testing.T, w http.ResponseWriter, n int, cursor string, created time.Time) {
	t.Helper()
	if cursor != "" {
		w.Header().Set("Link", `<https://api.github.com/repos/octocat/hello-world/dependabot/alerts?per_page=100&after=`+cursor+`>; rel="next"`)
	}
	_, _ = w.Write(repeat(t, "dependabot_alerts.json", "", n, func(i int, row map[string]any) {
		row["number"] = 1000 + i
		row["created_at"] = created.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339)
	}))
}

// TestTheDependabotWalkEndsOnEachOfItsFourSignals pins every way the cursor
// walk stops: a full page with no cursor, a short page with one, a page past
// the backfill bound, and the walk's own cap. Each case is a full walk made
// to look like it should go on, so only the one signal can end it.
func TestTheDependabotWalkEndsOnEachOfItsFourSignals(t *testing.T) {
	t.Parallel()
	old := testNow.AddDate(0, 0, -30)
	for _, tc := range []struct {
		name  string
		walk  Walk
		serve func(w http.ResponseWriter)
		calls int
	}{
		{"a full page without a cursor", Walk{Pages: 3}, func(w http.ResponseWriter) {
			reposDependabotPage(t, w, 100, "", old)
		}, 1},
		{"a short page with a cursor", Walk{Pages: 3}, func(w http.ResponseWriter) {
			reposDependabotPage(t, w, 5, "cursor-2", old)
		}, 1},
		{"a full page past the bound", Walk{Pages: 3, Since: testNow.AddDate(0, 0, -7)}, func(w http.ResponseWriter) {
			reposDependabotPage(t, w, 100, "cursor-2", old)
		}, 1},
		{"the cap", Walk{Pages: 2}, func(w http.ResponseWriter) {
			reposDependabotPage(t, w, 100, "cursor-2", old)
		}, 2},
	} {
		f := newFixtureServer(t)
		f.handle("/repos/octocat/hello-world/dependabot/alerts", func(w http.ResponseWriter, _ *http.Request) { tc.serve(w) })
		points, err := Security{Walk: tc.walk}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n := len(f.calls("/repos/octocat/hello-world/dependabot/alerts")); n != tc.calls {
			t.Errorf("%s: made %d Dependabot calls, want %d", tc.name, n, tc.calls)
		}
		dep := find(t, points, "gh_security_feature", map[string]string{"feature": "dependabot"})
		if dep.Fields["enabled"] != true {
			t.Errorf("%s: dependabot feature = %v, want enabled", tc.name, dep.Fields)
		}
	}
}

// TestAlertWalksKeepWhatWasReadAtThePaginationCeiling holds, for both alert
// lists, that the 422 GitHub answers past its ceiling is the end of the data:
// the pages already read stay and the feature stays enabled.
func TestAlertWalksKeepWhatWasReadAtThePaginationCeiling(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/dependabot/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "" {
			reposPaginationLimit(w)
			return
		}
		reposDependabotPage(t, w, 100, "cursor-2", testNow.AddDate(0, 0, -30))
	})
	f.handle("/repos/octocat/hello-world/code-scanning/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			reposPaginationLimit(w)
			return
		}
		_, _ = w.Write(repeat(t, "code_scanning_alerts.json", "", 100, func(i int, row map[string]any) {
			row["number"] = 1000 + i
		}))
	})
	points, err := Security{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"dependabot", "code_scanning"} {
		p := find(t, points, "gh_security_feature", map[string]string{"feature": feature})
		if p.Fields["enabled"] != true || fieldInt(t, p, "alerts") != 100 {
			t.Errorf("%s = %v, want enabled with the 100 alerts of the page before the ceiling", feature, p.Fields)
		}
	}
}

// TestTheCodeScanningWalkReadsPastAFullPageUpToItsCap pins that a full page of
// a hundred asks for the next, and that the walk's cap ends it however many
// full pages there are.
func TestTheCodeScanningWalkReadsPastAFullPageUpToItsCap(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/code-scanning/alerts"
	serve := func(t *testing.T, sizes map[string]int) *fixtureServer {
		t.Helper()
		f := newFixtureServer(t)
		f.handle(path, func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			n, ok := sizes[page]
			if !ok {
				n = 100
			}
			number, _ := strconv.Atoi(page)
			_, _ = w.Write(repeat(t, "code_scanning_alerts.json", "", n, func(i int, row map[string]any) {
				row["number"] = number*1000 + i
			}))
		})
		return f
	}

	f := serve(t, map[string]int{"2": 5})
	points, err := Security{Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := reposPagesAsked(f, path); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("asked for pages %v, want the full first and the short second", got)
	}
	if n := len(only(t, points, "gh_code_scanning_alert_item")); n != 105 {
		t.Errorf("got %d alert items, want 105", n)
	}

	capped := serve(t, nil)
	if _, cappedErr := (Security{Walk: Walk{Pages: 2}}).Collect(ctx(t), capped.Client, testRepo, testNow); cappedErr != nil {
		t.Fatal(cappedErr)
	}
	if got := reposPagesAsked(capped, path); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("asked for pages %v, want the two the cap allows", got)
	}

	broken := newFixtureServer(t)
	reposFail(broken, path)
	if _, brokenErr := (Security{}).Collect(ctx(t), broken.Client, testRepo, testNow); brokenErr == nil {
		t.Error("a 500 on the code scanning list was not reported")
	}
}

// TestADependabotAlertClosesOneOfThreeWays reads the closing date off each
// field that can carry it, and the precedence when more than one does. A
// wrong pick either writes an open alert as closed or the reverse, and every
// time-to-resolve panel is built on this one number.
func TestADependabotAlertClosesOneOfThreeWays(t *testing.T) {
	t.Parallel()
	created := testNow.AddDate(0, 0, -20)
	at := func(days int) *time.Time { v := created.AddDate(0, 0, days); return &v }
	for _, tc := range []struct {
		name    string
		row     dependabotRow
		resolve int // days to resolve, or -1 for an open alert
	}{
		{"open", dependabotRow{}, -1},
		{"fixed", dependabotRow{FixedAt: at(2)}, 2},
		{"dismissed", dependabotRow{DismissedAt: at(3)}, 3},
		{"auto dismissed", dependabotRow{AutoDismissedAt: at(4)}, 4},
		{"fixed after a dismissal was undone", dependabotRow{FixedAt: at(5), DismissedAt: at(1)}, 5},
	} {
		tc.row.CreatedAt = created
		f := dependabotAlertFields(&tc.row)
		if tc.resolve < 0 {
			if hasField(sink.Point{Fields: f}, "seconds_to_resolve") || hasField(sink.Point{Fields: f}, "seconds_open") {
				t.Errorf("%s: fields %v, want no age on an open alert", tc.name, f)
			}
			continue
		}
		if hasField(sink.Point{Fields: f}, "seconds_open") || f["seconds_to_resolve"] != tc.resolve*86400 {
			t.Errorf("%s: fields %v, want seconds_to_resolve of %d days", tc.name, f, tc.resolve)
		}
	}
}

// TestAZeroEPSSPercentileIsNotAPercentile pins the guard every score on the
// alert row shares: GitHub sends zero for a score it does not have, and a
// written zero reads as "less likely to be exploited than anything known".
func TestAZeroEPSSPercentileIsNotAPercentile(t *testing.T) {
	t.Parallel()
	var none dependabotRow
	if f := dependabotAlertFields(&none); hasField(sink.Point{Fields: f}, "epss_percentile") {
		t.Errorf("a zero percentile was written: %v", f["epss_percentile"])
	}
	var scored dependabotRow
	scored.SecurityAdvisory.EPSS.Percentile = 0.2
	if f := dependabotAlertFields(&scored); f["epss_percentile"] != 0.2 {
		t.Errorf("epss_percentile = %v, want 0.2", f["epss_percentile"])
	}
	if got := alertCWEs([]string{"CWE-79", "", "CWE-89"}); got != "CWE-79,CWE-89" {
		t.Errorf("alertCWEs = %q, want the empty id left out", got)
	}
}

// TestACodeScanningResolutionSaysWhyOrThatItIsOpen pins resolutionOf: the
// reason a person gave wins, a fix is GitHub's silence, and an open alert
// still gets a value so the column exists from the first row.
func TestACodeScanningResolutionSaysWhyOrThatItIsOpen(t *testing.T) {
	t.Parallel()
	fixed := testNow
	for _, tc := range []struct {
		name string
		row  scanRow
		want string
	}{
		{"open", scanRow{}, "open"},
		{"fixed", scanRow{FixedAt: &fixed}, "fixed"},
		{"dismissed", scanRow{DismissedReason: "false positive"}, "false positive"},
		{"dismissed with a fix date as well", scanRow{DismissedReason: "won't fix", FixedAt: &fixed}, "won't fix"},
	} {
		if got := resolutionOf(&tc.row); got != tc.want {
			t.Errorf("%s: resolutionOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// reposAnalysisPages serves full pages of analyses for pages one to full and
// the given body for anything after, each page dated a day further back.
func reposAnalysisPages(t *testing.T, f *fixtureServer, full int, after func(w http.ResponseWriter)) {
	t.Helper()
	f.handle("/repos/octocat/hello-world/code-scanning/analyses", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 || page > full {
			after(w)
			return
		}
		_, _ = w.Write(repeat(t, "code_scanning_analyses.json", "", 100, func(i int, row map[string]any) {
			row["id"] = page*1000 + i
			row["created_at"] = testNow.AddDate(0, 0, -page).Add(-time.Duration(i) * time.Minute).Format(time.RFC3339)
		}))
	})
}

// TestTheAnalysesWalkEndsAtItsCapAnEmptyPageOrTheBound pins how the analyses
// walk stops, and that it reports a failure with the analyses already read.
func TestTheAnalysesWalkEndsAtItsCapAnEmptyPageOrTheBound(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/code-scanning/analyses"
	empty := func(w http.ResponseWriter) { _, _ = w.Write([]byte("[]")) }

	capped := newFixtureServer(t)
	reposAnalysisPages(t, capped, 10, empty)
	points, err := Analyses{Walk: Walk{Pages: 3}}.Collect(ctx(t), capped.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := reposPagesAsked(capped, path); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Errorf("asked for pages %v, want 1, 2 and 3", got)
	}
	if len(points) != 300 {
		t.Errorf("got %d analyses, want the 300 of three full pages", len(points))
	}

	ended := newFixtureServer(t)
	reposAnalysisPages(t, ended, 1, empty)
	points, err = Analyses{Walk: Walk{Pages: 3}}.Collect(ctx(t), ended.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := reposPagesAsked(ended, path); !slices.Equal(got, []string{"1", "2"}) || len(points) != 100 {
		t.Errorf("asked for pages %v and kept %d analyses, want 1 and the empty 2, and 100", got, len(points))
	}

	bounded := newFixtureServer(t)
	reposAnalysisPages(t, bounded, 10, empty)
	walk := Walk{Pages: 3, Since: testNow.Add(-36 * time.Hour)}
	if _, boundedErr := (Analyses{Walk: walk}).Collect(ctx(t), bounded.Client, testRepo, testNow); boundedErr != nil {
		t.Fatal(boundedErr)
	}
	if got := reposPagesAsked(bounded, path); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("asked for pages %v, want the walk to stop at the page that crossed the bound", got)
	}

	limited := newFixtureServer(t)
	reposAnalysisPages(t, limited, 1, reposPaginationLimit)
	points, err = Analyses{Walk: Walk{Pages: 3}}.Collect(ctx(t), limited.Client, testRepo, testNow)
	if err != nil || len(points) != 100 {
		t.Errorf("at the pagination ceiling: %d analyses, err %v; want the 100 read and no error", len(points), err)
	}

	broken := newFixtureServer(t)
	reposAnalysisPages(t, broken, 1, func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })
	points, err = Analyses{Walk: Walk{Pages: 3}}.Collect(ctx(t), broken.Client, testRepo, testNow)
	if err == nil || len(points) != 100 {
		t.Errorf("after a 500: %d analyses, err %v; want the 100 read and the error", len(points), err)
	}
}

// TestAWebhookDeliveryIsOkOnlyInTheTwoHundreds pins the edges of the ok tag.
// A 300 is a redirect the hook's endpoint sent instead of accepting the
// payload, and a 0 is a delivery that never got an answer at all: GitHub
// records a timeout that way.
func TestAWebhookDeliveryIsOkOnlyInTheTwoHundreds(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	settingsRoutes(f)
	f.handle("/repos/octocat/hello-world/hooks/12345678/deliveries", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[` +
			`{"id":1,"delivered_at":"2026-09-07T09:00:00Z","status":"timed out","status_code":0,"event":"push"},` +
			`{"id":2,"delivered_at":"2026-09-07T09:01:00Z","status":"OK","status_code":200,"event":"push"},` +
			`{"id":3,"delivered_at":"2026-09-07T09:02:00Z","status":"OK","status_code":299,"event":"push"},` +
			`{"id":4,"delivered_at":"2026-09-07T09:03:00Z","status":"Moved","status_code":300,"event":"push"}]`))
	})
	points, err := Settings{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for code, ok := range map[string]string{"0": "false", "200": "true", "299": "true", "300": "false"} {
		p := find(t, points, "gh_webhook_delivery", map[string]string{"code": code})
		if p.Tags["ok"] != ok {
			t.Errorf("status %s: ok = %q, want %q", code, p.Tags["ok"], ok)
		}
	}

	// The environment's age and the deploy key's day, from the same sweep:
	// production was created on 2025-01-01, six hundred and fifteen whole
	// days before testNow, and a key is a snapshot of the day.
	production := find(t, points, "gh_environment", map[string]string{"environment": "production"})
	if got := fieldInt(t, production, "age_days"); got != 615 {
		t.Errorf("production age_days = %d, want 615", got)
	}
	for _, key := range only(t, points, "gh_deploy_key") {
		if !key.Time.Equal(startOfDay(testNow)) {
			t.Errorf("deploy key %s stamped %s, want the start of the UTC day", key.Tags["key"], key.Time)
		}
	}
}

// TestSettingsReportABrokenHookListAndSkipAnUnreadableDeliveryList separates
// the hook list failing, which the sweep must hear about, from one hook whose
// deliveries the token cannot read, which costs that hook's deliveries only.
func TestSettingsReportABrokenHookListAndSkipAnUnreadableDeliveryList(t *testing.T) {
	t.Parallel()
	broken := newFixtureServer(t)
	settingsRoutes(broken)
	reposFail(broken, "/repos/octocat/hello-world/hooks")
	if _, err := (Settings{}).Collect(ctx(t), broken.Client, testRepo, testNow); err == nil {
		t.Error("a 500 on the hook list was not reported")
	}

	f := newFixtureServer(t)
	settingsRoutes(f)
	f.status("/repos/octocat/hello-world/hooks/12345678/deliveries", http.StatusNotFound, "Not Found")
	points, err := Settings{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantMeasurements(t, points, "gh_webhook", "gh_environment", "gh_deploy_key")
	if n := len(byMeasurement(points)["gh_webhook_delivery"]); n != 0 {
		t.Errorf("got %d deliveries from a list that answered 404", n)
	}
}

// TestAWebhookHostNeverCarriesThePath pins what hostOf is for: the path of a
// webhook URL usually holds a secret, so a value that is all path, with no
// scheme and no host in front of it, gives nothing rather than the path.
func TestAWebhookHostNeverCarriesThePath(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://hooks.example.com/github/secret": "hooks.example.com",
		"http://ci.example.org":                   "ci.example.org",
		"/github/8f3a2b1c-secret-path":            "",
	} {
		if got := hostOf(raw); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestStargazersReportWhatBrokeAndSkipWhatIsGone covers the stargazer walk's
// failures and absences one read at a time: the first page failing, the last
// page of an increment gone, a page of a full walk gone or failing, and a
// row with no date, which is not a star that can be placed on the curve.
func TestStargazersReportWhatBrokeAndSkipWhatIsGone(t *testing.T) {
	t.Parallel()
	const path = "/repos/octocat/hello-world/stargazers"
	lastIs := func(w http.ResponseWriter, last int) {
		w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/repositories/1296269/stargazers?per_page=100&page=%d>; rel="last"`, last))
	}

	first := newFixtureServer(t)
	reposFail(first, path)
	if _, err := (Stargazers{}).Collect(ctx(t), first.Client, testRepo, testNow); err == nil {
		t.Error("a 500 on the first stargazer page was not reported")
	}

	// An increment on a repository with one page reads that page and no more.
	single := newFixtureServer(t)
	single.file(path, "stargazers_page1.json")
	points, err := Stargazers{}.Collect(ctx(t), single.Client, testRepo, testNow)
	if err != nil || len(points) != 2 || len(single.calls(path)) != 1 {
		t.Errorf("one page: %d stars, %d calls, err %v; want 2 stars from one call", len(points), len(single.calls(path)), err)
	}

	for _, tc := range []struct {
		name    string
		full    bool
		page2   int
		wantErr bool
		calls   int
	}{
		{"an increment whose last page is gone", false, http.StatusNotFound, false, 2},
		{"a full walk whose second page is gone", true, http.StatusNotFound, false, 2},
		{"a full walk whose second page fails", true, http.StatusInternalServerError, true, 2},
	} {
		f := newFixtureServer(t)
		f.handle(path, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") != "" {
				w.WriteHeader(tc.page2)
				return
			}
			lastIs(w, 3)
			f.write(w, "stargazers_page1.json")
		})
		stars, walkErr := Stargazers{Full: tc.full}.Collect(ctx(t), f.Client, testRepo, testNow)
		if (walkErr != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want an error %v", tc.name, walkErr, tc.wantErr)
		}
		if n := len(f.calls(path)); n != tc.calls {
			t.Errorf("%s: made %d calls, want %d", tc.name, n, tc.calls)
		}
		if !tc.wantErr && len(stars) != 2 {
			t.Errorf("%s: got %d stars, want the 2 of the first page", tc.name, len(stars))
		}
	}

	undated := newFixtureServer(t)
	undated.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"user":{"login":"nobody"}},{"starred_at":"2024-03-01T10:00:00Z","user":{"login":"alice"}}]`))
	})
	points, err = Stargazers{}.Collect(ctx(t), undated.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Tags["user"] != "alice" {
		t.Errorf("got %d stars, want only alice's, the one with a date", len(points))
	}
}

// TestAnAppLoginWithNoLoginStaysEmpty keeps the suffix off a login GitHub did
// not give: "[bot]" on its own would be an author that is nobody.
func TestAnAppLoginWithNoLoginStaysEmpty(t *testing.T) {
	t.Parallel()
	if got := appLogin("", true); got != "" {
		t.Errorf("appLogin(\"\", true) = %q, want empty", got)
	}
}

// TestAHostnameIsAtMostTwoHundredAndFiftyThreeCharacters pins the length
// limit of a DNS name at its edge, and the character rule for a label that is
// past 'z'.
func TestAHostnameIsAtMostTwoHundredAndFiftyThreeCharacters(t *testing.T) {
	t.Parallel()
	labels := func(last int) string {
		return strings.Join([]string{
			strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", last),
		}, ".")
	}
	if s := labels(61); len(s) != 253 || !hostLike(s) {
		t.Errorf("a valid name of %d characters was not host-like", len(s))
	}
	if s := labels(62); len(s) != 254 || hostLike(s) {
		t.Errorf("a name of %d characters was host-like", len(s))
	}
	for _, s := range []string{"café.example", "a~b.example", "a{b.example"} {
		if hostLike(s) {
			t.Errorf("hostLike(%q) = true, want false for a character past 'z'", s)
		}
	}
}

// TestTrafficReportsEveryReadThatBreaks covers the three reads after views
// one at a time. A 500 on any of them fails the sweep; a referrer list the
// token cannot see is skipped, and the rest of the traffic stays.
func TestTrafficReportsEveryReadThatBreaks(t *testing.T) {
	t.Parallel()
	routes := func(f *fixtureServer) {
		f.file("/repos/octocat/hello-world/traffic/views", "traffic_views.json")
		f.file("/repos/octocat/hello-world/traffic/clones", "traffic_clones.json")
		f.file("/repos/octocat/hello-world/traffic/popular/referrers", "traffic_referrers.json")
		f.file("/repos/octocat/hello-world/traffic/popular/paths", "traffic_paths.json")
	}
	for _, read := range []string{"clones", "popular/referrers", "popular/paths"} {
		f := newFixtureServer(t)
		routes(f)
		reposFail(f, "/repos/octocat/hello-world/traffic/"+read)
		if _, err := (Traffic{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
			t.Errorf("a 500 on %s was not reported", read)
		}
	}

	f := newFixtureServer(t)
	routes(f)
	f.status("/repos/octocat/hello-world/traffic/popular/referrers", http.StatusForbidden, "Must have push access to repository")
	points, err := Traffic{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantMeasurements(t, points, "gh_traffic", "gh_traffic_path")
	if n := len(byMeasurement(points)["gh_traffic_referrer"]); n != 0 {
		t.Errorf("got %d referrers from a list that answered 403", n)
	}
}
