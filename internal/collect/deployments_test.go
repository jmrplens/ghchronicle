package collect

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// deployRepos are the two repositories the fixture answers for, in alias
// order.
var deployRepos = []Repo{
	{Owner: "jmrplens", Name: "phonometry", FullName: "jmrplens/phonometry"},
	{Owner: "jmrplens", Name: "gitlab-mcp-server", FullName: "jmrplens/gitlab-mcp-server"},
}

// serveDeployments answers every query with the captured batch and records the
// query text, which is how the cursor and the batching are checked.
func serveDeployments(f *fixtureServer, seen *[]string) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if seen != nil {
			*seen = append(*seen, query)
		}
		f.write(w, "graphql_deployments.json")
	})
}

func TestDeploymentsAreDatedWhenTheDeploymentHappened(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	serveDeployments(f, nil)

	points, err := Deployments{Repos: deployRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_deployment")
	if got := len(only(t, points, "gh_deployment")); got != 6 {
		t.Fatalf("got %d deployments, want the fixture's 6", got)
	}

	// The live one: created 12:56:00, its status 33 seconds later.
	live := find(t, points, "gh_deployment", map[string]string{"deployment": "6372116605"})
	if !live.Time.Equal(time.Date(2026, 9, 10, 12, 56, 0, 0, time.UTC)) {
		t.Errorf("stamped %s, want the deployment's own createdAt", live.Time)
	}
	for tag, want := range map[string]string{
		"full_name": "jmrplens/phonometry", "environment": "github-pages",
		"task": "deploy",
	} {
		if live.Tags[tag] != want {
			t.Errorf("tag %s = %q, want %q", tag, live.Tags[tag], want)
		}
	}
	// The outcome moves after the deployment's own date, pending first and
	// success or failure later, so as a tag it opened a second row at the
	// same instant the sweep after the one that saw it pending.
	if _, isTag := live.Tags["state"]; isTag {
		t.Error("the outcome must be a field, not a tag: the row is dated at creation and the status lands later")
	}
	if hasField(live, "state") {
		t.Error("the demoted outcome must not reuse the old tag's column name")
	}
	if live.Fields["outcome"] != "success" {
		t.Errorf("outcome = %v, want success", live.Fields["outcome"])
	}
	if live.Fields["deployment_state"] != "active" {
		t.Errorf("deployment_state = %v, want the deployment's own enum", live.Fields["deployment_state"])
	}
	if got := fieldInt(t, live, "seconds_to_status"); got != 33 {
		t.Errorf("seconds_to_status = %d, want 33", got)
	}
	if live.Fields["success"] != true || live.Fields["superseded"] != false {
		t.Errorf("success/superseded = %v/%v", live.Fields["success"], live.Fields["superseded"])
	}
	if live.Fields["environment_url"] != "https://jmrplens.github.io/phonometry/" {
		t.Errorf("environment_url = %v", live.Fields["environment_url"])
	}
	if live.Fields["ref"] != "main" || live.Fields["creator"] != "jmrplens" {
		t.Errorf("ref/creator = %v/%v", live.Fields["ref"], live.Fields["creator"])
	}
	if live.Fields["commit"] != "5385c9d67e6257c7a39d72af892900fffb61f8ec" {
		t.Errorf("commit = %v", live.Fields["commit"])
	}
	// The run that performed it, so a row joins gh_workflow_run without
	// taking the log URL apart in a panel.
	if got := fieldInt(t, live, "run_id"); got != 34477701377 {
		t.Errorf("run_id = %d, want the run out of the log URL", got)
	}
}

func TestDeploymentsReadInactiveAsSupersededRatherThanFailed(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	serveDeployments(f, nil)

	points, err := Deployments{Repos: deployRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Measured on the live API: this deployment succeeded at 10:44:23 and was
	// marked INACTIVE at 12:56:34, when the next one replaced it. INACTIVE is
	// the outcome of a successful deployment that was superseded.
	old := find(t, points, "gh_deployment", map[string]string{"deployment": "6369856254"})
	if old.Fields["outcome"] != "success" {
		t.Errorf("outcome = %q, want success: INACTIVE is not a failure", old.Fields["outcome"])
	}
	if old.Fields["success"] != true || old.Fields["superseded"] != true {
		t.Errorf("success/superseded = %v/%v, want true/true", old.Fields["success"], old.Fields["superseded"])
	}
	if old.Fields["deployment_state"] != "inactive" {
		t.Errorf("deployment_state = %v, want the raw enum as a field", old.Fields["deployment_state"])
	}
	// The latest status of a superseded deployment is dated when the
	// replacement went live, so it is not how long the deployment took.
	// Recording it as seconds_to_status would overwrite the real duration a
	// sweep taken while it was still ACTIVE had already stored.
	if hasField(old, "seconds_to_status") {
		t.Error("a superseded deployment must not claim a time to status")
	}
	if got := fieldInt(t, old, "seconds_live"); got != 8041 {
		t.Errorf("seconds_live = %d, want 8041", got)
	}

	failed := find(t, points, "gh_deployment", map[string]string{"deployment": "6267407798"})
	if failed.Fields["outcome"] != "failure" || failed.Fields["success"] != false {
		t.Errorf("failed deployment = %v %v", failed.Fields["outcome"], failed.Fields["success"])
	}
	// The branch this release deployment named is gone, and GitHub answers
	// ref: null. An empty tag would be dropped by the sink and an empty field
	// would teach a store the column is sometimes blank.
	if hasField(failed, "ref") {
		t.Errorf("ref = %v, want the field left out when GitHub has no ref", failed.Fields["ref"])
	}
	broken := find(t, points, "gh_deployment", map[string]string{"deployment": "6370198397"})
	if broken.Fields["outcome"] != "error" {
		t.Errorf("outcome = %q, want error", broken.Fields["outcome"])
	}
}

func TestDeploymentsKeepBothDeploymentsOfTheSameSecond(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	serveDeployments(f, nil)

	points, err := Deployments{Repos: deployRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Two release deployments of the same run share createdAt, environment,
	// task and outcome: 14 of the 584 nodes read on 2026-09-10 collide that
	// way. Without the deployment id in the tags the second silently replaces
	// the first.
	same := 0
	for _, p := range only(t, points, "gh_deployment") {
		if p.Time.Equal(time.Date(2026, 9, 4, 15, 20, 56, 0, time.UTC)) {
			same++
		}
	}
	if same != 2 {
		t.Errorf("%d rows for the two deployments of 15:20:56, want 2", same)
	}
	for _, id := range []string{"6267400316", "6267400301"} {
		find(t, points, "gh_deployment", map[string]string{"deployment": id})
	}
}

func TestDeploymentsBatchFiveRepositoriesPerQuery(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	serveDeployments(f, &queries)

	repos := make([]Repo, 7)
	for i := range repos {
		repos[i] = Repo{Owner: "jmrplens", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("jmrplens/r%d", i)}
	}
	if _, err := (Deployments{Repos: repos}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	// Five and two, not ten and not seven. The gateway gives up at about ten
	// seconds and this query asks each repository for a connection.
	if len(queries) != 2 {
		t.Fatalf("%d queries for 7 repositories, want 2 batches of five", len(queries))
	}
	if !strings.Contains(queries[0], "r4: repository") || strings.Contains(queries[0], "r5: repository") {
		t.Error("the first batch must carry exactly five repositories")
	}
	// The end-to-end fake picks a fixture by the first marker found in the
	// query text, and both of these belong to other collectors.
	for _, marker := range []string{"pullRequests(", "history("} {
		if strings.Contains(queries[0], marker) {
			t.Errorf("the query must not contain %q, which is another collector's marker", marker)
		}
	}
}

func TestDeploymentsHalveTheBatchWhenTheGatewayGivesUp(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		if len(queries) == 1 {
			// Measured: an HTML 502 at about ten seconds, whatever the cost.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			return
		}
		f.write(w, "graphql_deployments.json")
	})

	points, err := Deployments{Repos: deployRepos, Batch: 2}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a refused batch must be halved, not lost: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("%d queries, want the refused batch of two split into two of one", len(queries))
	}
	if strings.Contains(queries[1], "r1: repository") {
		t.Error("the retry must ask for one repository at a time")
	}
	checkPoints(t, points)
}

func TestDeploymentsWalkCarryACursorPerRepository(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		if len(queries) == 1 {
			f.write(w, "graphql_deployments.json")
			return
		}
		// The second page ends the walk for the one repository that had more.
		var page map[string]any
		mustUnmarshal(t, fixture(t, "graphql_deployments.json"), &page)
		conn := page["data"].(map[string]any)["r0"].(map[string]any)["deployments"].(map[string]any)
		conn["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": ""}
		_, _ = w.Write(mustMarshal(t, map[string]any{"data": map[string]any{"r0": page["data"].(map[string]any)["r0"]}}))
	})

	_, err := Deployments{Repos: deployRepos, Walk: Unbounded}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("%d queries, want a second one for the repository with more pages", len(queries))
	}
	// Only the repository whose page was not the last, and with its own cursor.
	if !strings.Contains(queries[1], `after: "deploy-cursor-1"`) {
		t.Errorf("the second query carries no cursor: %s", queries[1])
	}
	if strings.Contains(queries[1], "gitlab-mcp-server") {
		t.Error("a repository whose walk is finished must not be asked again")
	}
}

func TestDeploymentsStopTheWalkAtTheBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		f.write(w, "graphql_deployments.json")
	})

	// Everything the fixture holds is older than the bound, so the page that
	// offers a next one is not followed.
	since := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := (Deployments{Repos: deployRepos, Walk: Walk{Pages: -1, Since: since}}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 {
		t.Errorf("%d queries, want the walk to stop at the bound", len(queries))
	}
}

func TestDeploymentsKeepTheBatchWhenOneRepositoryIsGone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		// The measured wire shape, 2026-09-10: a batch with one repository
		// that no longer resolves answers HTTP 200 with the data for every
		// other alias, null for that one, and an errors array naming it. The
		// client reports the error and drops the body with it, so the batch
		// has to be asked for one repository at a time to keep the rest.
		var page map[string]any
		mustUnmarshal(t, fixture(t, "graphql_deployments.json"), &page)
		data := page["data"].(map[string]any)
		if !strings.Contains(query, "phonometry") {
			_, _ = w.Write([]byte(`{"data":{"r0":null},"errors":[{"type":"NOT_FOUND",` +
				`"path":["r0"],"message":"Could not resolve to a Repository"}]}`))
			return
		}
		if strings.Contains(query, "gitlab-mcp-server") {
			data["r1"] = nil
			page["errors"] = []any{map[string]any{
				"type": "NOT_FOUND", "path": []any{"r1"},
				"message": "Could not resolve to a Repository",
			}}
		}
		_, _ = w.Write(mustMarshal(t, page))
	})

	points, err := Deployments{Repos: deployRepos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("one repository that is gone is not a failure: %v", err)
	}
	// The batch, then one query per repository in it.
	if len(queries) != 3 {
		t.Fatalf("%d queries, want the refused batch asked for one at a time", len(queries))
	}
	// The two deployments of the repository that did answer, which the old
	// behavior threw away along with the error.
	if got := len(only(t, points, "gh_deployment")); got != 2 {
		t.Errorf("got %d rows, want the repository that answered to survive", got)
	}
	find(t, points, "gh_deployment", map[string]string{"full_name": "jmrplens/phonometry"})
}

func TestDeploymentsWithoutRepositoriesAskNothing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	serveDeployments(f, nil)
	points, err := Deployments{}.Collect(ctx(t), f.Client, testNow)
	if err != nil || points != nil {
		t.Fatalf("points = %v, err = %v", points, err)
	}
	if n := len(f.calls("/graphql")); n != 0 {
		t.Errorf("%d queries for no repositories", n)
	}
}

func TestRunIDIsReadFromTheLogURL(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]int64{
		"https://github.com/jmrplens/phonometry/actions/runs/34477701377/job/102878591597": 34477701377,
		"https://github.com/jmrplens/phonometry/actions/runs/34477701377":                  34477701377,
		// A status posted by something that is not Actions, which is what
		// Vercel leaves on the two deployments of jmrp.io.
		"https://vercel.com/jmrplens/jmrp-io/7Kd": 0,
		"": 0,
	} {
		if got := runID(url); got != want {
			t.Errorf("runID(%q) = %d, want %d", url, got, want)
		}
	}
}
