package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command itself: what it writes, what it refuses, and what it says.

// inRepoRoot runs a test from the directory the generator writes relative to,
// since its destination is fixed rather than a flag.
func inRepoRoot(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

// TestItWritesEveryCombinationAndSaysHowMany.
func TestItWritesEveryCombinationAndSaysHowMany(t *testing.T) {
	inRepoRoot(t)
	var out, errs strings.Builder
	if code := run([]string{"gen_compose"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	written, err := os.ReadDir(deployDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(Combinations()) {
		t.Errorf("wrote %d files, want %d", len(written), len(Combinations()))
	}
	if !strings.Contains(out.String(), "wrote 5 compose file(s)") {
		t.Errorf("output = %q, want it to say what it did", out.String())
	}
}

// TestCheckFailsOnAFileThatDrifted, which is what makes the page's promise
// about them true rather than hopeful.
func TestCheckFailsOnAFileThatDrifted(t *testing.T) {
	inRepoRoot(t)
	var out, errs strings.Builder
	if code := run([]string{"gen_compose"}, &out, &errs); code != 0 {
		t.Fatal(errs.String())
	}
	if code := run([]string{"gen_compose", "-check"}, &out, &errs); code != 0 {
		t.Fatalf("a check of what was just written failed: %s", errs.String())
	}
	edited := filepath.Join(deployDir, Combinations()[0].File)
	if err := os.WriteFile(edited, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := run([]string{"gen_compose", "-check"}, &out, &errs); code == 0 {
		t.Error("a file edited by hand passed the check")
	}
	if !strings.Contains(out.String(), Combinations()[0].File) {
		t.Errorf("output = %q, want it to name the file that drifted", out.String())
	}
	if !strings.Contains(errs.String(), "make compose") {
		t.Errorf("stderr = %q, want it to say how to fix it", errs.String())
	}
}

// TestAnUnknownFlagIsRefused rather than ignored.
func TestAnUnknownFlagIsRefused(t *testing.T) {
	inRepoRoot(t)
	var out, errs strings.Builder
	if code := run([]string{"gen_compose", "-nonsense"}, &out, &errs); code != 2 {
		t.Errorf("exit %d, want 2 for a flag it does not know", code)
	}
}

// TestAskingForHelpIsNotAFailure.
func TestAskingForHelpIsNotAFailure(t *testing.T) {
	inRepoRoot(t)
	var out, errs strings.Builder
	if code := run([]string{"gen_compose", "-h"}, &out, &errs); code != 0 {
		t.Errorf("exit %d, want 0 for a request for the usage", code)
	}
}
