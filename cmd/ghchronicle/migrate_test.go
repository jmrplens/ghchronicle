package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecuteMigratePlansWhatItCannotCompareToo: a GitHub that refuses the
// token, or no token at all, still gives a plan, which says what it could
// not compare; the plan exits 0, since a dry run that found work to do has
// not failed; and the state file is neither created nor written.
func TestExecuteMigratePlansWhatItCannotCompareToo(t *testing.T) {
	dir := t.TempDir()
	sinks := "sinks:\n  sql:\n    path: " + filepath.Join(dir, "points.sql") + "\n"
	cfg := writeConfig(t, dir, refusingGitHub(t), sinks)
	got := runCommand(t, "-config", cfg, "-migrate")
	if got.status != notExited || !strings.Contains(got.stdout, "the repository list was not read (") ||
		!strings.Contains(got.stdout, "401 Unauthorized") ||
		!strings.HasSuffix(got.stdout, "Nothing to migrate. Nothing was changed.\n") {
		t.Errorf("-migrate against a refusing GitHub = %d:\n%s\n%s", got.status, got.stdout, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Errorf("a dry run left a state file behind: %v", err)
	}

	tokenless := filepath.Join(dir, "tokenless.yaml")
	if err := os.WriteFile(tokenless, []byte("targets:\n  user: octocat\nstate_file: "+
		filepath.Join(dir, "state.json")+"\n"+sinks), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	got = runCommand(t, "-config", tokenless, "-migrate")
	if got.status != notExited || !strings.Contains(got.stdout, "the configuration has no GitHub token") {
		t.Errorf("-migrate with no token = %d:\n%s\n%s", got.status, got.stdout, got.stderr)
	}

	got = runCommand(t, "-config", tokenless, "-migrate", "-yes")
	if got.status != 1 || got.stdout != "" || !strings.Contains(got.stderr, "-migrate alone prints the plan") {
		t.Errorf("-migrate -yes = %d, %q, %q, want 1 and nothing applied", got.status, got.stdout, got.stderr)
	}
}
