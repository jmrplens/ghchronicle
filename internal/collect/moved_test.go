package collect

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// movedRepos are the four repositories graphql_moved.json answers for, in
// alias order, read live on 2026-09-27: one that moved in every way, a fork
// with no issue and no pull request, one with pull requests and no issue, and
// one with neither.
var movedRepos = []Repo{
	{Owner: "jmrplens", Name: "ghchronicle", FullName: "jmrplens/ghchronicle"},
	{Owner: "jmrplens", Name: "acme.sh", FullName: "jmrplens/acme.sh"},
	{Owner: "jmrplens", Name: "kleidos", FullName: "jmrplens/kleidos"},
	{Owner: "jmrplens", Name: "homebrew-tap", FullName: "jmrplens/homebrew-tap"},
}

func serveMoved(f *fixtureServer, seen *[]string) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if seen != nil {
			*seen = append(*seen, query)
		}
		f.write(w, "graphql_moved.json")
	})
}

func rfc3339(t *testing.T, stamp string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMovementsSayWhenEachRepositoryLastMoved(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	serveMoved(f, &queries)

	moved, err := Movements{Repos: movedRepos}.Read(ctx(t), f.Client)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 {
		t.Fatalf("%d queries for four repositories, want one", len(queries))
	}
	want := map[string]Movement{
		// The newest of the two lists is the one that counts: here the issue.
		"jmrplens/ghchronicle": {Head: rfc3339(t, "2026-09-25T13:53:26Z"), Items: rfc3339(t, "2026-09-26T21:00:16Z")},
		"jmrplens/acme.sh":     {Head: rfc3339(t, "2026-09-04T08:23:49Z")},
		// No issue at all, so the pull request is the newest item.
		"jmrplens/kleidos":      {Head: rfc3339(t, "2026-09-27T03:17:37Z"), Items: rfc3339(t, "2026-09-22T09:34:50Z")},
		"jmrplens/homebrew-tap": {Head: rfc3339(t, "2026-09-23T18:28:56Z")},
	}
	for name, w := range want {
		got, ok := moved[name]
		if !ok {
			t.Errorf("no answer for %s", name)
			continue
		}
		if !got.Head.Equal(w.Head) || !got.Items.Equal(w.Items) {
			t.Errorf("%s = %+v, want %+v", name, got, w)
		}
	}
	// Each connection is ordered the way the collector it stands in for
	// orders it, so its one node is the one that collector reads first.
	for _, part := range []string{
		"issues(first: 1, orderBy: {field: UPDATED_AT, direction: DESC})",
		"pullRequests(first: 1, orderBy: {field: UPDATED_AT, direction: DESC})",
		"defaultBranchRef { target { ... on Commit { committedDate } } }",
	} {
		if !strings.Contains(queries[0], part) {
			t.Errorf("the query does not ask %s", part)
		}
	}
}

// TestAMovementIsReadAtItsOwnSecond pins the boundary: a head committed at the
// very second the window opens is in history(since:), measured, so it is a
// commit to read; one second before it is not. The items follow the same rule
// as the walks, which stop at an item updated before the bound and not at
// one updated on it.
func TestAMovementIsReadAtItsOwnSecond(t *testing.T) {
	t.Parallel()
	since := rfc3339(t, "2026-09-26T22:27:19Z")
	on := Movement{Head: since, Items: since}
	if !on.CommitsSince(since) || !on.ItemsSince(since) {
		t.Errorf("a movement at exactly since = %+v, want it read", on)
	}
	before := Movement{Head: since.Add(-time.Second), Items: since.Add(-time.Second)}
	if before.CommitsSince(since) || before.ItemsSince(since) {
		t.Errorf("a movement a second before since = %+v, want it left", before)
	}
	// A repository with no default branch, and one with no issue and no pull
	// request, have nothing since any moment; a window with no start holds
	// everything, so nothing is ever skipped against it.
	var never Movement
	if never.CommitsSince(since) || never.ItemsSince(since) {
		t.Error("an empty repository has nothing to read")
	}
	if !never.CommitsSince(time.Time{}) || !never.ItemsSince(time.Time{}) {
		t.Error("a window with no start must be read")
	}
}

func TestAnEmptyRepositoryHasNoHead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// The schema's null for a repository with no commit yet, beside one
	// that has a branch.
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"data":{` +
			`"r0":{"defaultBranchRef":null,"issues":{"nodes":[]},"pullRequests":{"nodes":[]}},` +
			`"r1":{"defaultBranchRef":{"target":{"committedDate":"2026-09-20T16:00:38Z"}},` +
			`"issues":{"nodes":[{"updatedAt":"2026-09-24T17:25:58Z"}]},"pullRequests":{"nodes":[]}}}}`))
	})
	moved, err := Movements{Repos: movedRepos[:2]}.Read(ctx(t), f.Client)
	if err != nil {
		t.Fatal(err)
	}
	if got := moved["jmrplens/ghchronicle"]; !got.Head.IsZero() || !got.Items.IsZero() {
		t.Errorf("an empty repository = %+v, want no head and no item", got)
	}
	if got := moved["jmrplens/acme.sh"]; !got.Items.Equal(rfc3339(t, "2026-09-24T17:25:58Z")) {
		t.Errorf("a repository with an issue and no pull request = %+v, want the issue", got)
	}
}

func TestMovementsBatchTheRepositories(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	serveMoved(f, &queries)

	repos := make([]Repo, 37)
	for i := range repos {
		repos[i] = Repo{Owner: "jmrplens", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("jmrplens/r%d", i)}
	}
	if _, err := (Movements{Repos: repos}).Read(ctx(t), f.Client); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Errorf("%d queries for 37 repositories, want batches of twenty five", len(queries))
	}
	// The end-to-end fake picks a fixture by the first marker in the query
	// text, and pullRequests( is the pull request query's. The fragment name
	// is what tells this one apart.
	if !strings.Contains(queries[0], "fragment moved on Repository") {
		t.Error("the query does not carry the fragment the fake knows it by")
	}
}

func TestMovementsWithoutRepositoriesAskNothing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	serveMoved(f, nil)
	moved, err := Movements{}.Read(ctx(t), f.Client)
	if err != nil || len(moved) != 0 {
		t.Fatalf("moved = %v, err = %v", moved, err)
	}
	if n := len(f.calls("/graphql")); n != 0 {
		t.Errorf("%d queries for no repositories", n)
	}
}

// TestAFailedBatchAnswersForTheRest: the caller reads every repository the
// map leaves out, so a batch that failed has to be left out and the one that
// answered kept.
func TestAFailedBatchAnswersForTheRest(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, `"homebrew-tap"`) {
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
			return
		}
		f.write(w, "graphql_moved.json")
	})
	moved, err := Movements{Repos: movedRepos, Batch: 2}.Read(ctx(t), f.Client)
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMITED") {
		t.Fatalf("err = %v, want the failed batch reported", err)
	}
	if _, ok := moved["jmrplens/ghchronicle"]; !ok {
		t.Error("the batch that answered was dropped with the one that failed")
	}
	for _, gone := range []string{"jmrplens/kleidos", "jmrplens/homebrew-tap"} {
		if _, ok := moved[gone]; ok {
			t.Errorf("%s is answered by a batch that failed", gone)
		}
	}
}
