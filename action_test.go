package ghchronicle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheActionsDefaultConfigLeavesPrivateRepositoriesOut runs the script the
// Action uses when it is given no configuration. That configuration only ever
// feeds a card, which is published, and a card that counts private
// repositories puts their names in a profile README. So private repositories
// stay out unless the caller says true, and anything that is not exactly true
// or false is refused rather than read as one of them. The login is written
// into the same YAML, so anything that is not a GitHub login is refused too:
// a line break in it would add keys of its own to the configuration.
func TestTheActionsDefaultConfigLeavesPrivateRepositoriesOut(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("the Action's steps run in bash, and there is none here")
	}
	script := filepath.Join("scripts", "action-config.sh")
	for _, tc := range []struct {
		name, login, include string
		status               int
		want                 string
	}{
		{"not given", "octocat", "false", 0, "include_private: false"},
		{"asked for", "octocat", "true", 0, "include_private: true"},
		{"a word that is neither", "octocat", "yes", 2, "include-private must be true or false"},
		{"a line break", "octocat", "false\nsinks: {x: 1}", 2, "include-private must be true or false"},
		{"a login with a key after a line break", "octocat\nsinks: {x: 1}", "false", 2, "user must be a GitHub login"},
		{"a login that starts with a dash", "-octocat", "false", 2, "user must be a GitHub login"},
		{"no login", "", "false", 2, "user must be a GitHub login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "ghchronicle.yaml")
			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Env = append(os.Environ(),
				"USER_LOGIN="+tc.login, "INCLUDE_PRIVATE="+tc.include,
				"OUT="+out, "STATE="+filepath.Join(dir, "state.json"))
			output, _ := cmd.CombinedOutput()
			if cmd.ProcessState.ExitCode() != tc.status {
				t.Fatalf("exit %d, want %d:\n%s", cmd.ProcessState.ExitCode(), tc.status, output)
			}
			got := string(output)
			if tc.status == 0 {
				body, readErr := os.ReadFile(out)
				if readErr != nil {
					t.Fatal(readErr)
				}
				got = string(body)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, got)
			}
		})
	}
}
