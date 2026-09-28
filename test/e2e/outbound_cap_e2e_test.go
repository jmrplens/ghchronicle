package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// capWarning is the start of the line the outbound family writes when a
// search's pages run out before its count does.
const capWarning = "outbound search read fewer items than it counts"

// TestAnOutboundSearchPastGitHubsCapIsSaid is the account of issue #76 in
// small. GitHub serves a thousand results of any search and then answers that
// there is no next page, while issueCount still says how many there are:
// measured on 2026-09-26, 2,860 merged pull requests served ten pages of a
// hundred and stopped. Nothing fails when that happens, so up to 2.5.1 the
// rows past the cap were missing and the family reported success. Here the
// search counts 1,500 and serves its five items, and each of the five
// searches is served the one of its kind and state and counts what the other
// four leave of the 1,500: the rows served reach the store, the family does
// not fail, and each search says it came up short, once, at warning.
func TestAnOutboundSearchPastGitHubsCapIsSaid(t *testing.T) {
	t.Parallel()
	gh := fakegh.New(t, "testdata", cappedSearchOverlay(t))
	dir := t.TempDir()
	points := filepath.Join(dir, "points.jsonl")
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
    outbound: 1m
state_file: %s
`, gh.URL(), login, points, filepath.Join(dir, "state.json")))

	out, err := run(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("ghchronicle -once failed: %v\n%s", err, out)
	}
	log := string(out)
	if strings.Contains(log, "collector failed") {
		t.Errorf("a search past the cap failed the family:\n%s", log)
	}
	var said []string
	for line := range strings.SplitSeq(log, "\n") {
		if strings.Contains(line, capWarning) {
			said = append(said, line)
		}
	}
	if len(said) != 5 {
		t.Fatalf("the cap was said %d times, want once for each of the five searches:\n%s", len(said), log)
	}
	for _, line := range said {
		m := countAndRead.FindStringSubmatch(line)
		if !strings.Contains(line, "level=WARN") || m == nil || m[1] != strconv.Itoa(cappedEachSearch) || m[2] != "1" {
			t.Errorf("the cap was said as %q, want a warning that it counted %d and read 1",
				line, cappedEachSearch)
		}
	}

	contributions := 0
	for _, p := range readPoints(t, points) {
		if p.Measurement == "gh_external_contribution" {
			contributions++
		}
	}
	if contributions != 5 {
		t.Errorf("wrote %d contributions, want the one served to each of the 5 searches", contributions)
	}
}

// cappedEachSearch is what each search of the capped overlay counts: the
// 1,500 the overlay declares, less the four items the fake leaves out of a
// search for one kind in one state.
const cappedEachSearch = 1500 - 4

// countAndRead is what the cap warning says it counted and read.
var countAndRead = regexp.MustCompile(`count=(\d+) read=(\d+)`)

// cappedSearchOverlay is a fixture directory whose outbound search counts
// more items than it serves and says there is no next page, which is what
// GitHub answers on the page after the thousandth result.
func cappedSearchOverlay(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "graphql_search_issues.json"))
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Data struct {
			Search map[string]json.RawMessage `json:"search"`
		} `json:"data"`
	}
	if err = json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("graphql_search_issues.json: %v", err)
	}
	if string(answer.Data.Search["issueCount"]) != "5" {
		t.Fatalf("graphql_search_issues.json counts %s, want the 5 items it serves", answer.Data.Search["issueCount"])
	}
	answer.Data.Search["issueCount"] = json.RawMessage("1500")
	capped, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "graphql_search_issues.json"), capped, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
