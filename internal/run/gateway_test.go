package run

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// commitPage is one page of a commit history holding a single commit, with a
// cursor to the next page when there is one.
func commitPage(oid, committed, next string) string {
	return fmt.Sprintf(`{"data":{"repository":{"defaultBranchRef":{"name":"main","target":{"history":{`+
		`"totalCount":2,"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[{"oid":%q,`+
		`"url":"https://github.com/o/r/commit/%s","committedDate":%q,"additions":1,"deletions":1,`+
		`"changedFilesIfAvailable":1,"messageHeadline":"fix","author":{"name":"o","user":{"login":"o"}},`+
		`"signature":null,"associatedPullRequests":{"nodes":[]},"statusCheckRollup":null}]}}}}}}`,
		next != "", next, oid, oid, committed)
}

// cutShort answers the commit history of o/walked in one page, and the first
// page of o/cut with a cursor to a second that the gateway gives up on at
// every size.
func cutShort(w http.ResponseWriter, req *http.Request) {
	var env struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if req.URL.Path != "/graphql" || json.NewDecoder(req.Body).Decode(&env) != nil || !strings.Contains(env.Query, "history(") {
		http.NotFound(w, req)
		return
	}
	var body string
	switch {
	case env.Variables["name"] == "walked":
		body = commitPage("1111111111111111", "2026-09-10T00:00:00Z", "")
	case env.Variables["after"] == nil:
		body = commitPage("2222222222222222", "2026-09-10T00:00:00Z", "page-2")
	default:
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// TestABackfillDoesNotRecordAWalkTheGatewayCutShort is the other half of the
// commit walk handing up a timeout it cannot halve past. Up to 2.6.3 the walk
// took GitHub's ten second timeout for the end of the history and reported
// success, and a backfill recorded the repository as walked, so a resume never
// read what lay past that page. Now the rows read before the timeout reach
// the store, the failure is in the family's own rows with its reason, and the
// repository stays out of the checkpoint, where the one beside it that walked
// to the end is in it.
func TestABackfillDoesNotRecordAWalkTheGatewayCutShort(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, cutShort)
	// This server answers at once, and its 502 stands for GitHub's timeout.
	r.API.SetTimeoutWindow(0)
	r.Cfg.Every = everyOnly("commits")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.repos = []collect.Repo{
		{Owner: "o", Name: "walked", FullName: "o/walked"},
		{Owner: "o", Name: "cut", FullName: "o/cut"},
	}
	r.Backfill = true
	path := filepath.Join(t.TempDir(), "progress.json")
	progress, openErr := OpenProgress(path, "test-build", ScopeOf(r.Cfg, ""), time.Now())
	if openErr != nil {
		t.Fatal(openErr)
	}
	r.Progress = progress
	got := &kept{}
	r.Sinks = []sink.Sink{got}

	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Progress.FamilyDone("commits") {
		t.Fatal("commits was recorded complete with a walk the gateway cut short, so a resume would never read past it")
	}
	walked, cut := r.Progress.RepoDone("commits", "o/walked"), r.Progress.RepoDone("commits", "o/cut")
	if !walked || cut {
		t.Errorf("walked recorded %t and cut %t, want only the walk that reached the end", walked, cut)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the checkpoint of a walk with a repository left to read is gone: %v", statErr)
	}
	checkCutShortRows(t, got)
}

// checkCutShortRows holds the store to what the walk cut short owes it: the
// commit of its first page, and a row of the family's saying why the rest is
// missing.
func checkCutShortRows(t *testing.T, got *kept) {
	t.Helper()
	commits := 0
	for _, p := range got.rows("gh_commit") {
		if p.Tags["full_name"] == "o/cut" {
			commits++
		}
	}
	if commits != 1 {
		t.Errorf("%d commits of the walk cut short reached the store, want the first page's one", commits)
	}
	for _, p := range got.rows("gh_collector_family") {
		if p.Tags["scope"] == "repo" && p.Tags["family"] == "commits" &&
			p.Tags["full_name"] == "o/cut" && p.Tags["reason"] == "query too large" {
			return
		}
	}
	t.Errorf("the timeout is not in the family's rows: %v", got.rows("gh_collector_family"))
}
