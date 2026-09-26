package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// setAside is the archived repository fakegh.ArchivedOverlay adds.
const setAside = fakegh.SetAside

// TestAnArchivedRepositorySetAsideKeepsItsStarsCounted is issue #78 through
// the binary. A sweep with the default filter used to write an archived
// repository's gh_repo_archived row and nothing else, so its stars and forks
// were whatever the last backfill had said, or nothing on an install that
// never ran one, and they left the account's totals. Here two sweeps, one
// before somebody unstars it and one after, each write its lifetime row
// with the count GitHub answers at that sweep, stamped at the sweep, from
// the one query that names it.
func TestAnArchivedRepositorySetAsideKeepsItsStarsCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	points := filepath.Join(dir, "points.jsonl")

	first := archivedSweep(t, dir, "first", points, fakegh.ArchivedOverlay(t, "testdata", 3))
	second := archivedSweep(t, dir, "second", points, fakegh.ArchivedOverlay(t, "testdata", 2))

	var rows []point
	for _, p := range readPoints(t, points) {
		if p.Tags["full_name"] != setAside {
			continue
		}
		switch p.Measurement {
		case "gh_repo_total":
			rows = append(rows, p)
		case "gh_repo_archived":
			// Dated at the archive, as it was before this row had company.
			if want := fakegh.DaysAgoDate(77) + "T11:41:12Z"; p.Time != want {
				t.Errorf("gh_repo_archived stamped %s, want %s", p.Time, want)
			}
		default:
			t.Errorf("%s was set aside and still got a %s row", setAside, p.Measurement)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("%d lifetime rows for %s over two sweeps, want one each", len(rows), setAside)
	}
	for i, want := range []struct {
		stars  float64
		within [2]time.Time
	}{{3, first}, {2, second}} {
		row := rows[i]
		if row.Tags["archived"] != "true" || row.Tags["visibility"] != "public" || row.Tags["fork"] != "false" {
			t.Errorf("sweep %d tagged %s %v", i+1, setAside, row.Tags)
		}
		if row.Fields["stars"] != want.stars || row.Fields["forks"] != float64(1) || row.Fields["commits"] != float64(16) {
			t.Errorf("sweep %d wrote %v, want %v stars, 1 fork and 16 commits", i+1, row.Fields, want.stars)
		}
		at, err := time.Parse(time.RFC3339Nano, row.Time)
		if err != nil || at.Before(want.within[0]) || at.After(want.within[1]) {
			t.Errorf("sweep %d stamped the row %s, want the sweep's own time, between %s and %s",
				i+1, row.Time, want.within[0].Format(time.RFC3339), want.within[1].Format(time.RFC3339))
		}
	}
}

// archivedSweep runs one -once sweep of the totals family against a fake
// with the overlay, appending to points, and returns when it started and
// finished. It fails the test if anything but the archive query asked about
// the repository set aside.
func archivedSweep(t *testing.T, dir, name, points, overlay string) [2]time.Time {
	t.Helper()
	gh := fakegh.New(t, "testdata", overlay)
	own := filepath.Join(dir, name)
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfigFile(t, own, fmt.Sprintf(`github:
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
    totals: 1m
state_file: %s
`, gh.URL(), login, points, filepath.Join(own, "state.json")))

	started := time.Now().UTC().Truncate(time.Second)
	out, err := run(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("%s sweep failed: %v\n%s", name, err, out)
	}
	finished := time.Now().UTC()
	if !strings.Contains(string(out), "archived_aside=1") {
		t.Errorf("%s sweep did not set %s aside:\n%s", name, setAside, out)
	}

	asked := 0
	for _, r := range gh.Requests() {
		if strings.Contains(r.Path, "spoon-knife") {
			t.Errorf("%s sweep asked %s %s about a repository it sets aside", name, r.Method, r.Path)
		}
		if !strings.Contains(r.GraphQL, `name: "spoon-knife"`) {
			continue
		}
		asked++
		if !strings.Contains(r.GraphQL, "fragment archived on Repository") {
			t.Errorf("%s sweep asked about %s in a query that is not the archive query:\n%s", name, setAside, r.GraphQL)
		}
	}
	if asked != 1 {
		t.Errorf("%s sweep named %s in %d queries, want the one archive query", name, setAside, asked)
	}
	return [2]time.Time{started, finished}
}
