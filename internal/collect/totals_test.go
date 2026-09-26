package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// searchAnswer serves the total_count a search query is for, so a test can
// tell one query from another by what comes back.
func searchAnswer(f *fixtureServer, path string, byQuery map[string]int) {
	f.handle(path, func(w http.ResponseWriter, r *http.Request) {
		n, ok := byQuery[r.URL.Query().Get("q")]
		if !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "unexpected query: " + r.URL.Query().Get("q")})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": n, "items": []any{}})
	})
}

// searchCountsByQuery is what each of the ten counts answers, keyed by the
// query the alias carries, so a count served under the wrong alias is seen.
var searchCountsByQuery = map[string]int{
	"type:pr author:octocat":                         1799,
	"type:pr author:octocat is:merged":               1696,
	"type:pr author:octocat is:open":                 17,
	"type:pr author:octocat is:merged -user:octocat": 19,
	"type:pr reviewed-by:octocat":                    453,
	"type:issue author:octocat":                      167,
	"type:issue author:octocat is:closed":            137,
	"type:issue author:octocat -user:octocat":        27,
	"commenter:octocat -user:octocat":                113,
	"user:octocat":                                   35,
}

// countsAlias matches one alias of the counts query: its name, the search
// type and the query string.
var countsAlias = regexp.MustCompile(`(\w+): search\(type: (ISSUE|REPOSITORY), query: "([^"]*)"\)`)

// answerCounts answers the counts query alias by alias, issueCount for an
// issue search and repositoryCount for a repository one, as GraphQL does.
func answerCounts(t *testing.T, w http.ResponseWriter, query string) {
	t.Helper()
	data := map[string]any{}
	for _, m := range countsAlias.FindAllStringSubmatch(query, -1) {
		n, ok := searchCountsByQuery[m[3]]
		if !ok {
			t.Errorf("unexpected search query %q under alias %s", m[3], m[1])
			continue
		}
		field := "issueCount"
		if m[2] == "REPOSITORY" {
			field = "repositoryCount"
		}
		data[m[1]] = map[string]int{field: n}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("answer the counts query: %v", err)
	}
}

// isCountsQuery tells the ten-count query from the repository batch, which
// is the other query Totals sends.
func isCountsQuery(query string) bool {
	return strings.Contains(query, "issueCount") || strings.Contains(query, "repositoryCount")
}

func totalsFixture(t *testing.T) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	searchAnswer(f, "/search/commits", map[string]int{"author:octocat": 9228})
	return f
}

// archivedAt is null on every repository that is not archived, which is what
// GraphQL answers and what separates "never archived" from "archived at an
// unknown time".
func archivedAt(i int) any {
	if i != 0 {
		return nil
	}
	return "2026-08-29T15:22:31Z"
}

// graphQLTotals answers both queries Totals sends: the ten counts by alias
// and the batched repository query with one node per alias. seen records the
// repository batches only, which is what the batching tests count.
func graphQLTotals(f *fixtureServer, seen *[]string) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if isCountsQuery(query) {
			answerCounts(f.t, w, query)
			return
		}
		if seen != nil {
			*seen = append(*seen, query)
		}
		answerTotals(f.t, w, query)
	})
}

// answerTotals answers every repository alias the query names with the same
// repository, which is what the batched totals query asks for.
func answerTotals(t *testing.T, w http.ResponseWriter, query string) {
	t.Helper()
	data := map[string]any{}
	for i := 0; strings.Contains(query, fmt.Sprintf("r%d:", i)); i++ {
		data[fmt.Sprintf("r%d", i)] = map[string]any{
			"nameWithOwner": "octocat/hello-world", "databaseId": 1296269, "isFork": false,
			// The query asks for `url` and GraphQL answers every field it
			// is asked for, so the fixture does too: gh_repo_policy's link
			// is this with /settings appended.
			"url": "https://github.com/octocat/hello-world",
			// Archived on the first repository of a batch only, so one
			// test sees both an archived repository and a live one. The
			// dates are jmrplens/CATT2Matlab's, measured on 2026-09-10.
			"isArchived": i == 0,
			"archivedAt": archivedAt(i),
			// True on the first repository of a batch only, so a test that
			// asks for several sees both answers.
			"hasVulnerabilityAlertsEnabled": i == 0,
			"createdAt":                     "2017-09-13T00:18:54Z", "pushedAt": "2026-09-01T00:00:00Z",
			"diskUsage": 108, "stargazerCount": 80, "forkCount": 9,
			"watchers":     map[string]int{"totalCount": 4},
			"issuesOpen":   map[string]int{"totalCount": 3},
			"issuesClosed": map[string]int{"totalCount": 40},
			"pullsOpen":    map[string]int{"totalCount": 1},
			"pullsMerged":  map[string]int{"totalCount": 208},
			"pullsClosed":  map[string]int{"totalCount": 12},
			"releases":     map[string]int{"totalCount": 7},
			"discussions":  map[string]int{"totalCount": 2},
			"labels":       map[string]int{"totalCount": 18},
			"milestones":   map[string]int{"totalCount": 1},
			"branches":     map[string]int{"totalCount": 2},
			"tags":         map[string]int{"totalCount": 7},
			"defaultBranchRef": map[string]any{
				"target": map[string]any{"history": map[string]int{"totalCount": 405}},
			},
		}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("answer the totals query: %v", err)
	}
}

func TestTotalsAsksGitHubToDoTheCounting(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	graphQLTotals(f, nil)

	points, err := Totals{Login: "octocat", Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_account_total", "gh_repo_total")

	account := find(t, points, "gh_account_total", map[string]string{"user": "octocat"})
	for field, want := range map[string]int64{
		"pulls_opened": 1799, "pulls_merged": 1696, "pulls_open_now": 17,
		"pulls_merged_elsewhere": 19, "pulls_reviewed": 453,
		"issues_opened": 167, "issues_closed": 137, "issues_elsewhere": 27,
		"commented_elsewhere": 113, "commits": 9228, "repositories": 35,
	} {
		if got := fieldInt(t, account, field); got != want {
			t.Errorf("%s = %d, want %d", field, got, want)
		}
	}
	// One row, stamped now: the whole point is that a tile reads it without
	// scanning anything.
	if !account.Time.Equal(testNow) {
		t.Errorf("account totals stamped %s, want now", account.Time)
	}
	// The row the eleven REST searches wrote for the same answers, recorded
	// before ten of them became one GraphQL query.
	checkGolden(t, "totals", points, "gh_account_total")
	if n := len(f.calls("/search/issues")) + len(f.calls("/search/repositories")); n != 0 {
		t.Errorf("%d REST searches for counts GraphQL answers", n)
	}
	if n := len(f.calls("/search/commits")); n != 1 {
		t.Errorf("%d commit searches, want the one REST search GraphQL cannot replace", n)
	}

	// hasVulnerabilityAlertsEnabled rides free in the same batch and is the
	// second, independent source for whether Dependabot will hand over alerts
	// at all. Verified against GET /repos/{r}/vulnerability-alerts on all 52
	// repositories of the account: 204 on 7, 404 on 45, the same 7.
	policy := find(t, points, "gh_repo_policy", map[string]string{"full_name": "octocat/hello-world"})
	if policy.Fields["vulnerability_alerts"] != true {
		t.Errorf("vulnerability_alerts = %v, want true", policy.Fields["vulnerability_alerts"])
	}
	if !policy.Time.Equal(testNow) {
		t.Errorf("policy stamped %s, want now: it is a current state", policy.Time)
	}
	if policy.Fields["url"] != "https://github.com/octocat/hello-world/settings" {
		t.Errorf("policy url = %v", policy.Fields["url"])
	}

	repo := find(t, points, "gh_repo_total", map[string]string{"full_name": "octocat/hello-world"})
	for field, want := range map[string]int64{
		"commits": 405, "stars": 80, "forks": 9, "watchers": 4,
		"issues_open": 3, "issues_closed": 40, "issues": 43,
		"pulls_open": 1, "pulls_merged": 208, "pulls_closed": 12, "pulls": 221,
		"releases": 7, "discussions": 2, "labels": 18, "milestones": 1,
		"branches": 2, "tags": 7, "size_kb": 108,
	} {
		if got := fieldInt(t, repo, field); got != want {
			t.Errorf("%s = %d, want %d", field, got, want)
		}
	}
}

// Every count that says elsewhere means outside the repositories the account
// owns, which is what -user: leaves out. commented_elsewhere once said
// -author: instead, which kept every thread the owner answered at home and
// counted 125 where the rule of the other two counts 55. The number of them
// is checked too, so renaming a field cannot leave this test with nothing to
// look at.
func TestTotalsCountsEveryElsewhereOutsideTheAccountsRepositories(t *testing.T) {
	t.Parallel()
	const login = "octocat"
	var elsewhere []string
	for _, sc := range (Totals{Login: login}).searchCounts() {
		if !strings.HasSuffix(sc.field, "_elsewhere") {
			continue
		}
		elsewhere = append(elsewhere, sc.field)
		if !slices.Contains(strings.Fields(sc.query), "-user:"+login) {
			t.Errorf("%s searches %q, which does not leave out %s's own repositories with -user:%s",
				sc.field, sc.query, login, login)
		}
	}
	want := []string{"pulls_merged_elsewhere", "issues_elsewhere", "commented_elsewhere"}
	if !slices.Equal(elsewhere, want) {
		t.Errorf("elsewhere counts = %v, want %v", elsewhere, want)
	}
}

func TestTotalsBatchesRepositoriesIntoOneQuery(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var queries []string
	graphQLTotals(f, &queries)

	repos := make([]Repo, 7)
	for i := range repos {
		repos[i] = Repo{Owner: "octocat", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("octocat/r%d", i)}
	}
	points, err := Totals{Login: "", Repos: repos, Batch: 3}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(only(t, points, "gh_repo_total")); got != 7 {
		t.Errorf("got %d repository rows, want 7", got)
	}
	// Seven repositories in batches of three is three queries, not seven.
	if len(queries) != 3 {
		t.Errorf("%d GraphQL queries for 7 repositories in batches of 3", len(queries))
	}
	if !strings.Contains(queries[0], "hasVulnerabilityAlertsEnabled") {
		t.Error("the batch must carry hasVulnerabilityAlertsEnabled, which is what replaced the per-repository probe")
	}
	// The second and third repository of every batch answer false, so a
	// disabled feature is recorded rather than assumed.
	off := 0
	for _, p := range only(t, points, "gh_repo_policy") {
		if p.Fields["vulnerability_alerts"] == false {
			off++
		}
	}
	if off != 4 {
		t.Errorf("%d repositories with alerts off, want 4 of 7", off)
	}
}

// TestTotalsHalvesTheBatchWhenTheGatewayGivesUp pins the halving the Batch
// field promises. The gateway here refuses every query that names two
// repositories, as it does with a query it cannot finish, so asking for the
// same batch again is refused again for as long as the sweep lasts.
func TestTotalsHalvesTheBatchWhenTheGatewayGivesUp(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	sweep, stop := context.WithCancel(ctx(t))
	defer stop()
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		if len(queries) > 10 {
			// A collector that never makes the batch smaller would ask until
			// the sweep is canceled; this ends that sooner than ctx's timeout.
			stop()
		}
		if strings.Contains(query, "r1:") {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			return
		}
		answerTotals(t, w, query)
	})

	repos := []Repo{
		{Owner: "octocat", Name: "r0", FullName: "octocat/r0"},
		{Owner: "octocat", Name: "r1", FullName: "octocat/r1"},
	}
	points, err := Totals{Repos: repos, Batch: 2}.Collect(sweep, f.Client, testNow)
	if err != nil {
		t.Fatalf("a refused batch must be halved, not lost: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("%d queries, want the refused batch of two split into two of one", len(queries))
	}
	if got := len(only(t, points, "gh_repo_total")); got != 2 {
		t.Errorf("got %d repository rows, want both repositories of the refused batch", got)
	}
}

// TestTotalsKeepTheBatchWhenOneRepositoryIsGone pins the other refusal, the
// one a repository renamed away or no longer visible to the token causes. The
// gateway answers such a batch with data for every other alias and an errors
// array naming the missing one, and the client reports the error and drops the
// data with it. Asking for the batch one repository at a time is what keeps
// the rest, where the gone repository used to cost its whole batch on every
// sweep for as long as it stayed in the list.
func TestTotalsKeepTheBatchWhenOneRepositoryIsGone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		if strings.Contains(query, `name: "gone"`) {
			_, _ = w.Write([]byte(`{"data":{"r0":null},"errors":[{"type":"NOT_FOUND",` +
				`"path":["r0"],"message":"Could not resolve to a Repository"}]}`))
			return
		}
		answerTotals(t, w, query)
	})

	repos := []Repo{
		{Owner: "octocat", Name: "r0", FullName: "octocat/r0"},
		{Owner: "octocat", Name: "gone", FullName: "octocat/gone"},
		{Owner: "octocat", Name: "r2", FullName: "octocat/r2"},
	}
	points, err := Totals{Repos: repos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("one repository that is gone is not a failure: %v", err)
	}
	// The batch, then one query per repository in it.
	if len(queries) != 4 {
		t.Fatalf("%d queries, want the refused batch asked for one at a time", len(queries))
	}
	if got := len(only(t, points, "gh_repo_total")); got != 2 {
		t.Errorf("got %d repository rows, want the two that answered", got)
	}
	find(t, points, "gh_repo_total", map[string]string{"full_name": "octocat/r0"})
	find(t, points, "gh_repo_total", map[string]string{"full_name": "octocat/r2"})
}

func TestTotalsKeepsTheNumbersThatAnswered(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// Search is the first bucket to run out, at thirty a minute, and the
	// counts query can be refused on its own. Losing both must not lose the
	// repository totals, which come from another query.
	f.status("/search/commits", http.StatusForbidden, "API rate limit exceeded")
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if isCountsQuery(query) {
			_, _ = w.Write([]byte(`{"errors":[{"type":"FORBIDDEN","message":"search is not available"}]}`))
			return
		}
		answerTotals(t, w, query)
	})

	points, err := Totals{Login: "octocat", Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Error("neither search answered and the family reported success")
	}
	for _, p := range points {
		if p.Measurement == "gh_account_total" {
			t.Error("no search answered, so there is no account row to write")
		}
	}
	if len(only(t, points, "gh_repo_total")) != 1 {
		t.Error("the repository totals were lost with the search budget")
	}
}

// The commit count is the one search GraphQL has no type for. Losing the
// counts query keeps it, and losing it keeps the ten counts: the row carries
// whatever answered.
func TestTotalsWritesTheCountsThatAnsweredWhenOneSourceFails(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if isCountsQuery(query) {
			_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
			return
		}
		answerTotals(t, w, query)
	})
	points, err := Totals{Login: "octocat", Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Error("the counts query was refused and the family reported success")
	}
	account := find(t, points, "gh_account_total", map[string]string{"user": "octocat"})
	if fieldInt(t, account, "commits") != 9228 || hasField(account, "pulls_opened") {
		t.Errorf("account row = %v, want the commit count alone", account.Fields)
	}

	f2 := newFixtureServer(t)
	f2.status("/search/commits", http.StatusForbidden, "API rate limit exceeded")
	graphQLTotals(f2, nil)
	points, err = Totals{Login: "octocat", Repos: []Repo{testRepo}}.Collect(ctx(t), f2.Client, testNow)
	if err == nil {
		t.Error("the commit search was refused and the family reported success")
	}
	account = find(t, points, "gh_account_total", map[string]string{"user": "octocat"})
	if fieldInt(t, account, "pulls_opened") != 1799 || hasField(account, "commits") {
		t.Errorf("account row = %v, want the ten counts without the commits", account.Fields)
	}
}

// The date a repository was archived exists only in GraphQL. Verified on
// 2026-09-10: the keys of GET /repos/{r} carry `archived` and no
// `archived_at`, on an archived repository and on a live one.
func TestTotalsDatesTheArchiveAtTheMomentItHappened(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var queries []string
	graphQLTotals(f, &queries)

	repos := make([]Repo, 3)
	for i := range repos {
		repos[i] = Repo{Owner: "octocat", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("octocat/r%d", i)}
	}
	points, err := Totals{Repos: repos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if !strings.Contains(queries[0], "archivedAt") {
		t.Fatal("the batch must ask for archivedAt: REST does not carry it at all")
	}

	// One row for the archived repository, none for the two that are not.
	archived := only(t, points, "gh_repo_archived")
	if len(archived) != 1 {
		t.Fatalf("got %d archive rows for 3 repositories, want 1", len(archived))
	}
	row := find(t, points, "gh_repo_archived", map[string]string{"full_name": "octocat/r0"})
	if got := fieldInt(t, row, "archived"); got != 1 {
		t.Errorf("archived = %d, want 1", got)
	}
	// Created 2017-09-13, archived 2026-08-29.
	if got := fieldInt(t, row, "age_days_at_archive"); got != 3272 {
		t.Errorf("age_days_at_archive = %d, want 3272", got)
	}
	// The whole point: dated when the repository was archived, not when the
	// sweep ran. gh_repo_total's `archived` tag is stamped now and cannot tell
	// eighteen repositories archived in three batches from eighteen archived
	// on the same afternoon.
	want := time.Date(2026, 8, 29, 15, 22, 31, 0, time.UTC)
	if !row.Time.Equal(want) {
		t.Errorf("archive row stamped %s, want %s", row.Time, want)
	}
	if total := find(t, points, "gh_repo_total", map[string]string{"full_name": "octocat/r0"}); !total.Time.Equal(testNow) {
		t.Errorf("the lifetime row is still a snapshot stamped now, got %s", total.Time)
	}
}

// TestTotalsDatesTheArchiveOfRepositoriesSetAside is the sweep's half of
// the archive: the repositories the filter set aside get their
// gh_repo_archived row and their lifetime row from one query, and nothing
// else. The listing cannot supply the date: REST carries no archived_at, and
// its updated_at was measured two seconds to eight minutes after archivedAt.
func TestTotalsDatesTheArchiveOfRepositoriesSetAside(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var totalsQueries, archivedQueries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		switch {
		case isCountsQuery(query):
			answerCounts(f.t, w, query)
		case strings.Contains(query, "fragment archived on Repository"):
			archivedQueries = append(archivedQueries, query)
			answerArchived(t, w, query)
		default:
			totalsQueries = append(totalsQueries, query)
			answerTotals(f.t, w, query)
		}
	})

	collected := []Repo{{Owner: "octocat", Name: "r0", FullName: "octocat/r0"}}
	aside := []Repo{
		{Owner: "octocat", Name: "a0", FullName: "octocat/a0", Archived: true},
		{Owner: "octocat", Name: "a1", FullName: "octocat/a1", Archived: true},
	}
	points, err := Totals{Repos: collected, Archived: aside}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	// One query for both, carrying the lifetime row and none of the settings
	// gh_repo_policy is made of.
	if len(archivedQueries) != 1 {
		t.Fatalf("%d archive queries for 2 repositories set aside, want 1", len(archivedQueries))
	}
	q := archivedQueries[0]
	if !strings.Contains(q, "r1:") || !strings.Contains(q, lifetimeFields) {
		t.Errorf("the archive query must alias every repository and ask for its lifetime row:\n%s", q)
	}
	for _, setting := range []string{"isSecurityPolicyEnabled", "codeowners", "issueTemplates", "branchProtectionRules"} {
		if strings.Contains(q, setting) {
			t.Errorf("the archive query asks for %s, which only gh_repo_policy reads:\n%s", setting, q)
		}
	}
	if len(totalsQueries) != 1 || strings.Contains(totalsQueries[0], `name: "a0"`) {
		t.Errorf("the lifetime batch must carry the collected repository only, got %d: %v", len(totalsQueries), totalsQueries)
	}

	checkSetAsideRows(t, points, aside)
	// The collected repository keeps its own, from its own batch.
	if rows := only(t, points, "gh_repo_archived"); len(rows) != 3 {
		t.Errorf("got %d archive rows, want the two set aside and the collected one", len(rows))
	}

	// With nothing set aside there is nothing to ask.
	archivedQueries = nil
	if _, err = (Totals{Repos: collected}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(archivedQueries) != 0 {
		t.Errorf("an empty set-aside list still cost %d queries", len(archivedQueries))
	}
}

// answerArchived answers an archive query for however many repositories it
// aliases, each archived a second after the one before.
func answerArchived(t *testing.T, w http.ResponseWriter, query string) {
	t.Helper()
	data := map[string]any{}
	for i := 0; strings.Contains(query, fmt.Sprintf("r%d:", i)); i++ {
		data[fmt.Sprintf("r%d", i)] = map[string]any{
			"nameWithOwner": fmt.Sprintf("octocat/a%d", i),
			"url":           fmt.Sprintf("https://github.com/octocat/a%d", i),
			"createdAt":     "2020-05-14T00:00:00Z",
			"archivedAt":    fmt.Sprintf("2026-08-29T15:22:%02dZ", 31+i),
			"isArchived":    true, "stargazerCount": 5 + i,
		}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("answer the archive query: %v", err)
	}
}

// TestTotalsAsksAboutTwentyFiveArchivedRepositoriesAtATime: twenty five to a
// query and no more. The counts are what the gateway's ten seconds go on, and
// measured on 2026-09-26 a query of fifty was refused two times in three,
// which aliasBatch survives by halving, at ten seconds lost each time.
func TestTotalsAsksAboutTwentyFiveArchivedRepositoriesAtATime(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var mu sync.Mutex
	var archivedQueries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		switch {
		case isCountsQuery(query):
			answerCounts(f.t, w, query)
		case strings.Contains(query, "fragment archived on Repository"):
			mu.Lock()
			archivedQueries = append(archivedQueries, query)
			mu.Unlock()
			answerArchived(t, w, query)
		default:
			answerTotals(f.t, w, query)
		}
	})
	many := make([]Repo, 26)
	for i := range many {
		name := fmt.Sprintf("a%d", i)
		many[i] = Repo{Owner: "octocat", Name: name, FullName: "octocat/" + name, Archived: true}
	}
	if _, err := (Totals{Archived: many}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(archivedQueries) != 2 || !strings.Contains(archivedQueries[0], "r24:") ||
		strings.Contains(archivedQueries[0], "r25:") {
		t.Errorf("26 repositories set aside took %d archive queries, want 2, of 25 and 1", len(archivedQueries))
	}
}

// checkSetAsideRows is the half of the test above about the rows: each
// repository set aside gets the archive row, dated as GraphQL dates it, and
// its lifetime row, stamped at the sweep, and no row of any other kind,
// because nothing else about it is collected.
func checkSetAsideRows(t *testing.T, points []sink.Point, aside []Repo) {
	t.Helper()
	for i, repo := range aside {
		row := find(t, points, "gh_repo_archived", map[string]string{"full_name": repo.FullName})
		if want := time.Date(2026, 8, 29, 15, 22, 31+i, 0, time.UTC); !row.Time.Equal(want) {
			t.Errorf("%s archived at %s, want %s", repo.FullName, row.Time, want)
		}
		if got := fieldInt(t, row, "age_days_at_archive"); got != 2298 {
			t.Errorf("%s age_days_at_archive = %d, want 2298", repo.FullName, got)
		}
		total := find(t, points, "gh_repo_total", map[string]string{"full_name": repo.FullName, "archived": "true"})
		if !total.Time.Equal(testNow) {
			t.Errorf("%s lifetime row stamped %s, want the sweep's %s", repo.FullName, total.Time, testNow)
		}
		if got := fieldInt(t, total, "stars"); got != int64(5+i) {
			t.Errorf("%s stars = %d, want the %d GraphQL answered", repo.FullName, got, 5+i)
		}
		for _, p := range points {
			if p.Tags["full_name"] != repo.FullName {
				continue
			}
			if p.Measurement != "gh_repo_archived" && p.Measurement != "gh_repo_total" {
				t.Errorf("%s was set aside and still got a %s row", repo.FullName, p.Measurement)
			}
		}
	}
}

// TestTotalsCountsTheStarsOfRepositoriesSetAside is issue #78. A repository
// set aside for being archived is still starred, unstarred and forked, and
// up to 2.5.1 a sweep wrote its gh_repo_archived row and nothing else, so its
// stars and forks were whatever the last backfill had said: on 2026-09-26
// jmrplens/FFT2octave's only row, from the backfill of 2026-09-18, said 4
// stars where GitHub said 3. gh_repo_total is the table the account's totals
// are read from. graphql_archived_totals.json is the archive query's answer
// for two of that account's archived repositories, recorded on 2026-09-26,
// and graphql_issue_templates.json answers the live one.
func TestTotalsCountsTheStarsOfRepositoriesSetAside(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var archivedQueries, totalsQueries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if strings.Contains(query, "fragment archived on Repository") {
			archivedQueries = append(archivedQueries, query)
			f.writeSelected(w, "graphql_archived_totals.json", query)
			return
		}
		totalsQueries = append(totalsQueries, query)
		f.write(w, "graphql_issue_templates.json")
	})
	live := Repo{Owner: "jmrplens", Name: "gitlab-mcp-server", FullName: "jmrplens/gitlab-mcp-server"}
	aside := []Repo{
		{Owner: "jmrplens", Name: "FFT2octave", FullName: "jmrplens/FFT2octave", Archived: true},
		{Owner: "jmrplens", Name: "SetFigPaper", FullName: "jmrplens/SetFigPaper", Archived: true},
	}
	points, err := Totals{Repos: []Repo{live}, Archived: aside}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	liveRow := find(t, points, "gh_repo_total", map[string]string{"full_name": live.FullName})
	for full, want := range map[string]map[string]int64{
		"jmrplens/FFT2octave":  {"stars": 3, "forks": 1, "watchers": 1, "commits": 16, "pulls_merged": 3},
		"jmrplens/SetFigPaper": {"stars": 11, "forks": 4, "watchers": 1, "commits": 95, "pulls_merged": 1},
	} {
		checkSetAsideLifetimeRow(t, points, full, want, liveRow)
	}

	// Nothing else is asked about them: the one query that names them is
	// the archive query, the lifetime batch of the collected repositories
	// does not, and no REST request was made at all.
	if len(archivedQueries) != 1 || len(totalsQueries) != 1 {
		t.Fatalf("%d archive and %d lifetime queries, want one of each", len(archivedQueries), len(totalsQueries))
	}
	for _, repo := range aside {
		if strings.Contains(totalsQueries[0], `name: "`+repo.Name+`"`) {
			t.Errorf("the lifetime batch of the collected repositories asks about %s", repo.FullName)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Path != "/graphql" {
			t.Errorf("%s %s asked for a family the sweep leaves archived repositories out of", r.Method, r.Path)
		}
	}
}

// checkSetAsideLifetimeRow is the row half of the test above: the archived
// repository's gh_repo_total carries the counts GitHub answered, is stamped
// at the sweep like the live row beside it, since a lifetime row is a current
// state and the account's totals read the newest one, and has the tag and
// field names a collected repository's row has, so every panel that reads
// the live rows reads these without knowing they are different. Its settings
// were not asked for, so it has no gh_repo_policy.
func checkSetAsideLifetimeRow(t *testing.T, points []sink.Point, full string, want map[string]int64, liveRow sink.Point) {
	t.Helper()
	row := find(t, points, "gh_repo_total", map[string]string{
		"full_name": full, "archived": "true", "fork": "false", "visibility": "public",
	})
	for field, n := range want {
		if got := fieldInt(t, row, field); got != n {
			t.Errorf("%s %s = %d, want %d", full, field, got, n)
		}
	}
	if !row.Time.Equal(testNow) {
		t.Errorf("%s lifetime row stamped %s, want the sweep's %s", full, row.Time, testNow)
	}
	if got, live := slices.Sorted(maps.Keys(row.Tags)), slices.Sorted(maps.Keys(liveRow.Tags)); !slices.Equal(got, live) {
		t.Errorf("%s is tagged %v, a collected repository %v", full, got, live)
	}
	if got, live := slices.Sorted(maps.Keys(row.Fields)), slices.Sorted(maps.Keys(liveRow.Fields)); !slices.Equal(got, live) {
		t.Errorf("%s carries %v, a collected repository %v", full, got, live)
	}
	for _, p := range points {
		if p.Tags["full_name"] == full && p.Measurement == "gh_repo_policy" {
			t.Errorf("%s was set aside and still got its settings asked for", full)
		}
	}
}

// TestTotalsWritesTheSameLifetimeRowForARepositorySetAside: set aside or
// collected, an archived repository's gh_repo_total is the same line, byte
// for byte, from the same recording. The two paths decode through the same
// method, so what can make them differ is what each query asks for, and the
// server here answers each with the fields it selects and no others, as
// GitHub does. An archive query cut down to the scalars and the watchers, the
// cheaper query #78 weighed, would write every archived repository with no
// commits, merged pull requests, issues, releases, branches or tags, since a
// count GraphQL was not asked for decodes as zero, and nothing else would
// say so.
func TestTotalsWritesTheSameLifetimeRowForARepositorySetAside(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		f.writeSelected(w, "graphql_archived_totals.json", query)
	})
	repos := []Repo{
		{Owner: "jmrplens", Name: "FFT2octave", FullName: "jmrplens/FFT2octave", Archived: true},
		{Owner: "jmrplens", Name: "SetFigPaper", FullName: "jmrplens/SetFigPaper", Archived: true},
	}
	collected, err := Totals{Repos: repos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	setAside, err := Totals{Archived: repos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		want := sink.LineProtocol(find(t, collected, "gh_repo_total", map[string]string{"full_name": repo.FullName}))
		got := sink.LineProtocol(find(t, setAside, "gh_repo_total", map[string]string{"full_name": repo.FullName}))
		if got != want {
			t.Errorf("%s set aside:\n got %s\nwant %s", repo.FullName, got, want)
		}
	}
}

// writeSelected answers a GraphQL query with a recorded answer cut down to
// what the query selects: each repository keeps a field only where the query
// names it, as a field or as an alias. A recording holds every field the
// lifetime row is made of, and a query that stopped asking for one must not
// be handed it anyway.
func (f *fixtureServer) writeSelected(w http.ResponseWriter, name, query string) {
	var answer struct {
		Data map[string]map[string]any `json:"data"`
	}
	if err := json.Unmarshal(fixture(f.t, name), &answer); err != nil {
		f.t.Errorf("read %s: %v", name, err)
		return
	}
	for _, repo := range answer.Data {
		for key := range repo {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `\b`).MatchString(query) {
				delete(repo, key)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		f.t.Errorf("answer from %s: %v", name, err)
	}
}

// TestTotalsWritesNoSettingsLinkWithoutARepositoryURL is the same guard
// TestRepoCoreWritesNoCommunityLinkWithoutAnHTMLURL holds on the other half of
// the pair: gh_repo_policy's url is the repository's own url with /settings
// appended, and appending to nothing used to produce "/settings", a value the
// dashboards draw as itself and link to a path under the Grafana host.
func TestTotalsWritesNoSettingsLinkWithoutARepositoryURL(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"r0": map[string]any{"nameWithOwner": "octocat/hello-world", "databaseId": 1296269},
		}})
	})

	points, err := Totals{Login: "octocat", Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	policy := find(t, points, "gh_repo_policy", map[string]string{"full_name": "octocat/hello-world"})
	if hasField(policy, "url") {
		t.Errorf("url = %v, want no url field at all: a suffix on its own is a "+
			"relative path, not a link to GitHub", policy.Fields["url"])
	}
}

// GraphQL refuses the whole counts query when one of its ten aliases fails,
// and the client drops the data with the errors. The nine that would have
// answered are then asked on their own, so the row loses one number rather
// than ten, which is what it lost when each count was a REST search.
func TestTotalsAsksTheCountsOneByOneWhenTheQueryIsRefused(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var single int
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if !isCountsQuery(query) {
			answerTotals(t, w, query)
			return
		}
		aliases := len(countsAlias.FindAllStringSubmatch(query, -1))
		if aliases == 1 {
			single++
		}
		// The reviewed count is the one search that fails, alone or in the
		// batch of ten.
		if strings.Contains(query, "reviewed-by:") {
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"SERVICE_UNAVAILABLE","message":"search timed out"}]}`))
			return
		}
		answerCounts(t, w, query)
	})
	points, err := Totals{Login: "octocat", Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err == nil {
		t.Error("one of the ten searches failed and the family reported success")
	}
	account := find(t, points, "gh_account_total", map[string]string{"user": "octocat"})
	if hasField(account, "pulls_reviewed") {
		t.Errorf("the count whose search failed was written: %v", account.Fields)
	}
	for field, want := range map[string]int64{"pulls_opened": 1799, "repositories": 35, "commented_elsewhere": 113, "commits": 9228} {
		if fieldInt(t, account, field) != want {
			t.Errorf("%s = %v, want %d", field, account.Fields[field], want)
		}
	}
	if single != 10 {
		t.Errorf("%d single-count queries after the refusal, want one per count", single)
	}
}

// A spent GraphQL budget refuses the ten-alias query too, and ten more
// queries would only be refused ten more times.
func TestTotalsDoesNotRetryTheCountsOnASpentBudget(t *testing.T) {
	t.Parallel()
	f := totalsFixture(t)
	var counts int
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if isCountsQuery(query) {
			counts++
			_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
			return
		}
		answerTotals(t, w, query)
	})
	if _, err := (Totals{Login: "octocat", Repos: []Repo{testRepo}}).Collect(ctx(t), f.Client, testNow); err == nil {
		t.Error("the budget was spent and the family reported success")
	}
	if counts != 1 {
		t.Errorf("%d counts queries on a spent budget, want the one that was refused", counts)
	}
}

// TestTotalsCountsIssueFormsAsTemplates pins the reading of issue templates
// against the real API. graphql_issue_templates.json is a batch of three
// repositories recorded on 2026-09-12: jmrplens/gitlab-mcp-server keeps four
// YAML issue forms and a config.yml, which GraphQL's issueTemplates reports
// as none; jmrplens/jmrp.io keeps two Markdown templates, which it reports;
// jmrplens/jmrplens has no directory at all.
func TestTotalsCountsIssueFormsAsTemplates(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		f.write(w, "graphql_issue_templates.json")
	})
	repos := []Repo{
		{Owner: "jmrplens", Name: "gitlab-mcp-server", FullName: "jmrplens/gitlab-mcp-server"},
		{Owner: "jmrplens", Name: "jmrp.io", FullName: "jmrplens/jmrp.io"},
		{Owner: "jmrplens", Name: "jmrplens", FullName: "jmrplens/jmrplens"},
	}
	points, err := Totals{Repos: repos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(queries) != 1 || !strings.Contains(queries[0], `object(expression: "HEAD:.github/ISSUE_TEMPLATE")`) {
		t.Fatalf("the forms must ride in the one totals batch, got %d queries: %q", len(queries), queries)
	}
	for full, want := range map[string]int64{
		"jmrplens/gitlab-mcp-server": 4, // forms, config.yml left out
		"jmrplens/jmrp.io":           2, // Markdown, listed by both
		"jmrplens/jmrplens":          0, // no directory, no list
	} {
		policy := find(t, points, "gh_repo_policy", map[string]string{"full_name": full})
		if got := fieldInt(t, policy, "issue_templates"); got != want {
			t.Errorf("%s issue_templates = %d, want %d", full, got, want)
		}
	}
}

// A Markdown template kept outside the directory, .github/ISSUE_TEMPLATE.md,
// is listed by GraphQL and invisible to the tree, so the list is the floor.
// And a directory holding only config.yml is not a template.
func TestIssueTemplateCountFallsBackToTheMarkdownList(t *testing.T) {
	t.Parallel()
	var rt repoTotals
	mustUnmarshal(t, []byte(`{"issueTemplates":[{"name":"Bug"}],"issueForms":null}`), &rt)
	if got := rt.issueTemplateCount(); got != 1 {
		t.Errorf("no directory: %d, want the one Markdown template GraphQL lists", got)
	}
	mustUnmarshal(t, []byte(`{"issueTemplates":[],"issueForms":{"entries":[{"name":"config.yml"},{"name":"notes.txt"}]}}`), &rt)
	if got := rt.issueTemplateCount(); got != 0 {
		t.Errorf("config.yml and a text file: %d, want none", got)
	}
}
