package fakegh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SetAside is the archived repository ArchivedOverlay adds to the account.
// The default filter sets it aside, so the totals family is the one thing that
// asks about it, and graphql_repo_archived.json is what it answers.
const SetAside = Login + "/spoon-knife"

// ArchivedOverlay is a fixture directory for New's overlays: the listing in
// fixtures with SetAside added to it, archived, and an archive query that
// answers SetAside with the given number of stars.
//
// The base listing does not carry it. Every suite asserts on the account the
// base fixtures describe, and one repository more there changes what a sweep
// asks and writes everywhere, the backfills most of all, which collect an
// archived repository in full. A suite about archived repositories asks for
// one.
func ArchivedOverlay(tb testing.TB, fixtures string, stars int) string {
	tb.Helper()
	var listing []map[string]any
	readJSON(tb, filepath.Join(fixtures, "user_repos.json"), &listing)
	listing = append(listing, map[string]any{
		"id": 1300192, "name": "spoon-knife", "full_name": SetAside,
		"private": false, "fork": false, "archived": true,
		"owner": map[string]any{"login": Login},
	})

	var answer struct {
		Data map[string]map[string]any `json:"data"`
	}
	readJSON(tb, filepath.Join(fixtures, "graphql_repo_archived.json"), &answer)
	if answer.Data["r0"]["nameWithOwner"] != SetAside {
		tb.Fatalf("graphql_repo_archived.json answers for %v, want %s", answer.Data["r0"]["nameWithOwner"], SetAside)
	}
	answer.Data["r0"]["stargazerCount"] = stars

	dir := tb.TempDir()
	writeJSON(tb, filepath.Join(dir, "user_repos.json"), listing)
	writeJSON(tb, filepath.Join(dir, "graphql_repo_archived.json"), answer)
	return dir
}

func readJSON(tb testing.TB, path string, into any) {
	tb.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	if err = json.Unmarshal(raw, into); err != nil {
		tb.Fatalf("%s: %v", path, err)
	}
}

func writeJSON(tb testing.TB, path string, v any) {
	tb.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		tb.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		tb.Fatal(err)
	}
}
