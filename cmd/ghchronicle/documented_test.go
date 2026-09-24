package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// docPathInNote finds the documentation file a log attribute sends the reader
// to. The note is the only place the binary cites a document by path, so the
// citation is checked the way a link is checked and not by eye.
var docPathInNote = regexp.MustCompile(`docs/[a-z-]+\.md`)

// TestTheStartUpNoteCitesADocumentThatAnswersIt runs the start-up line an
// operator actually sees and holds its citation to the file that carries the
// answer.
//
// The note says the panels fed by a group that is off will fail and names a
// document. It named docs/running.md, which since docs/ became generated from
// the site is the installation page: it says nothing about groups or panels,
// so the one reader who follows the citation, the one who just switched a
// group off and found broken panels, arrives at the wrong file. The prose
// moved to docs/configuration.md with the cadence page it is generated from.
func TestTheStartUpNoteCitesADocumentThatAnswersIt(t *testing.T) {
	note := startUpNote(t)
	path := docPathInNote.FindString(note)
	if path == "" {
		t.Fatalf("the start-up note cites no document, so a reader cannot follow it:\n%s", note)
	}

	body, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatalf("the start-up note cites %s and it cannot be read: %v", path, err)
	}
	if !strings.Contains(string(body), "Panels for a group you switched off will show an error") {
		t.Errorf("the start-up note sends the reader to %s, which does not explain why the panels fail", path)
	}
}

// startUpNote captures the note attribute of the line logSelection writes when
// a configuration narrows the groups.
func startUpNote(t *testing.T) string {
	t.Helper()
	// The loader refuses a configuration with no token, and the one this
	// writes names none, so without this the test passes on a developer's
	// machine and fails on a runner. Every other test that loads a
	// configuration does the same: see internal/config.
	t.Setenv("GITHUB_TOKEN", "x")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("targets:\n  user: o\ngroups: [audience]\nsinks:\n  stdout: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the test's own configuration does not load, so this test proves nothing: %v", err)
	}

	var out bytes.Buffer
	logSelection(cfg, slog.New(slog.NewTextHandler(&out, nil)))
	line := out.String()
	if !strings.Contains(line, "note=") {
		t.Fatalf("logSelection wrote no note attribute, so this test proves nothing:\n%s", line)
	}
	return line
}
