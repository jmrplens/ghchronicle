package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// historyPath is the daily star history of the one repository the fake serves.
const historyPath = "/repos/octocat/hello-world/stargazers/history"

// TestAHiddenStargazerListStillHasItsDailyStars is the account the daily star
// history is read everywhere for. Since July 2026 GitHub gives a repository's
// stargazer list only to its admins and collaborators: anyone else gets a 404
// from the REST list and an empty connection from GraphQL, which is what the
// fake is set up to answer here. The stars have to reach the store all the
// same, from the history, with no person behind them and no day ahead of the
// sweep.
//
// Two sweeps of one process, because the second is the one a hidden list
// lives on for good, and because the client's ETag cache lives in memory: a
// second -once would be a second empty cache. The second reads page one of the
// history again, and GitHub answers that with a 304 it does not charge for.
func TestAHiddenStargazerListStillHasItsDailyStars(t *testing.T) {
	t.Parallel()
	gh := fakegh.New(t, "testdata", hiddenListOverlay(t))
	gh.Fail("/repos/octocat/hello-world/stargazers", http.StatusNotFound)
	dir := t.TempDir()
	cfg := writeConfigFile(t, dir, fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 20s
targets:
  user: %s
sinks:
  file:
    path: %s
    format: json
every:
  default: 0s
  families:
    stars: 1s
heartbeat: 2s
state_file: %s
log:
  level: debug
`, gh.URL(), login, filepath.Join(dir, "points.jsonl"), filepath.Join(dir, "state.json")))

	p := serveInBackground(t, cfg)
	if !waitFor(time.Minute, func() bool {
		return strings.Count(p.Output(), "sweep finished") >= 2 || p.Exited()
	}) || p.Exited() {
		t.Fatalf("two sweeps never finished:\n%s", p.Output())
	}
	p.Stop()
	if strings.Contains(p.Output(), "collector failed") {
		t.Errorf("a hidden list failed the sweep:\n%s", p.Output())
	}

	points := readPoints(t, filepath.Join(dir, "points.jsonl"))
	for _, pt := range points {
		if pt.Measurement == "gh_star" {
			t.Errorf("a gh_star row from a list GitHub hid: %+v", pt)
		}
	}
	assertStarDaysWereWritten(t, points, time.Now())
	assertTheHistoryWasRevalidated(t, gh.Requests())
	assertTheHistoryWasRecordedAsRead(t, filepath.Join(dir, "state.json"))
}

// hiddenListOverlay is a fixture directory whose audience batch answers the
// way GraphQL answers a token the list is hidden from: the repository, with
// its forks, and a stargazer connection with nothing in it.
func hiddenListOverlay(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "graphql_audience.json"))
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Data map[string]map[string]json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("graphql_audience.json: %v", err)
	}
	for alias := range answer.Data {
		answer.Data[alias]["stargazers"] = json.RawMessage(`{"edges":[]}`)
	}
	hidden, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "graphql_audience.json"), hidden, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertTheHistoryWasRevalidated: the first sweep read the history's one page
// and paid for it, and every sweep after it asked for that page again with
// its validator and was answered 304 for nothing. None asked for a page two:
// the fixture is shorter than a page, and the walk reads that as the end
// without a Link header, which the fake never sends.
func assertTheHistoryWasRevalidated(t *testing.T, requests []recorded) {
	t.Helper()
	var asked []recorded
	for _, r := range requests {
		if r.Path == historyPath {
			asked = append(asked, r)
		}
	}
	if len(asked) < 2 {
		t.Fatalf("the history was asked for %d times over two sweeps, want once each", len(asked))
	}
	if first := asked[0]; first.Status != http.StatusOK || first.Cost != 1 {
		t.Errorf("the first sweep's history was answered %d at a cost of %d, want a charged 200", first.Status, first.Cost)
	}
	for _, r := range asked[1:] {
		if r.Status != http.StatusNotModified || r.Cost != 0 {
			t.Errorf("a later sweep's history was answered %d at a cost of %d, want a free 304", r.Status, r.Cost)
		}
	}
	for _, r := range asked {
		if r.Query != "" {
			t.Errorf("the history was asked for with %q, beyond its one page", r.Query)
		}
	}
}

// assertTheHistoryWasRecordedAsRead: the first sweep's walk reached the end,
// so the state file says so, and no later sweep reads the history whole again.
func assertTheHistoryWasRecordedAsRead(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var st struct {
		HistoryRead map[string]time.Time `json:"history_read"`
	}
	if err = json.Unmarshal(b, &st); err != nil {
		t.Fatalf("state file is not JSON: %v\n%s", err, b)
	}
	if _, read := st.HistoryRead["octocat/hello-world"]; !read {
		t.Errorf("the state does not record the history as read: %s", b)
	}
}
