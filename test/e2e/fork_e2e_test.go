package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// The fork the base listing carries beside hello-world, which the default
// filter drops and a configuration can name.
const (
	namedFork        = login + "/linguist"
	namedForkPath    = "/repos/" + namedFork
	namedForkProfile = namedForkPath + "/community/profile"
)

// TestANamedForkIsNotAskedWhatIsAlreadyKnown is issue #86 through the binary.
// On the account it was measured on, fifteen forks named in targets.repos
// cost two requests an hour each that told the sweep nothing new: discovery
// read every one of them again for four flags the listing had just returned,
// and the repo family asked each for a community profile GitHub never serves
// a fork, a 404 with no ETag and so charged in full every time.
//
// Two sweeps of one process, because the memory of a refusal lives in the
// process, like the ETag cache, and a second -once would be a second empty
// one. The repository itself is still read on both: it is what gh_repo is
// made of.
func TestANamedForkIsNotAskedWhatIsAlreadyKnown(t *testing.T) {
	t.Parallel()
	gh := fakegh.New(t, "testdata", namedForkOverlay(t))
	gh.Fail(namedForkProfile, http.StatusNotFound)
	dir := t.TempDir()
	cfg := writeConfigFile(t, dir, fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 20s
targets:
  user: %s
  repos:
    - %s
sinks:
  file:
    path: %s
    format: json
every:
  default: 0s
  families:
    repo: 1s
heartbeat: 2s
state_file: %s
log:
  level: debug
`, gh.URL(), login, namedFork, filepath.Join(dir, "points.jsonl"), filepath.Join(dir, "state.json")))

	p := serveInBackground(t, cfg)
	if !waitFor(time.Minute, func() bool {
		return strings.Count(p.Output(), "sweep finished") >= 2 || p.Exited()
	}) || p.Exited() {
		t.Fatalf("two sweeps never finished:\n%s", p.Output())
	}
	p.Stop()

	// Discovery runs once, before the family, and the family starts with its
	// batched query: whatever was asked between the listing and that query
	// was discovery asking it. Read by position rather than cut at "sweep
	// finished", which a third sweep can already be past.
	all := gh.Requests()
	listing := slices.IndexFunc(all, func(r recorded) bool { return r.Path == "/user/repos" })
	batch := slices.IndexFunc(all, func(r recorded) bool {
		return strings.Contains(r.GraphQL, "fragment detail on Repository")
	})
	if listing < 0 || batch < listing {
		t.Fatalf("no listing before the repo family's batch (listing at %d, batch at %d)", listing, batch)
	}
	if n := countPath(all[listing:batch], namedForkPath); n != 0 {
		t.Errorf("discovery read %s %d times after the listing had returned it", namedForkPath, n)
	}
	if n := countPath(all, namedForkPath); n < 2 {
		t.Errorf("%s was read %d times over two sweeps, want once a sweep by the repo family", namedForkPath, n)
	}
	if n := countPath(all, namedForkProfile); n != 1 {
		t.Errorf("the sweeps asked %s %d times, want once: the refusal is remembered for a day", namedForkProfile, n)
	}

	var forkRows, profiles int
	for _, pt := range readPoints(t, filepath.Join(dir, "points.jsonl")) {
		if pt.Tags["full_name"] != namedFork {
			continue
		}
		switch pt.Measurement {
		case "gh_repo":
			forkRows++
			if pt.Tags["fork"] != "true" {
				t.Errorf("the named fork's gh_repo is tagged %v", pt.Tags)
			}
		case "gh_repo_community":
			profiles++
		}
	}
	if forkRows < 2 {
		t.Errorf("%d gh_repo rows for %s over two sweeps, want one each: naming it is what collects it", forkRows, namedFork)
	}
	if profiles != 0 {
		t.Errorf("%d community rows for a fork GitHub serves no community profile for", profiles)
	}
}

// namedForkOverlay is a fixture directory that makes the fork in the base
// listing answer for itself: hello-world's repository body, renamed, with
// the fork flag set. Everything else under its path is hello-world's, which
// is what an overlay does for any repository the fake does not know.
func namedForkOverlay(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("repo.json: %v", err)
	}
	body["id"], body["name"], body["full_name"] = 1296270, "linguist", namedFork
	body["html_url"] = "https://github.com/" + namedFork
	body["fork"] = true
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "linguist~repo.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// countPath is how many of the requests asked for one path with GET.
func countPath(requests []recorded, path string) int {
	n := 0
	for _, r := range requests {
		if r.Method == http.MethodGet && r.Path == path {
			n++
		}
	}
	return n
}
