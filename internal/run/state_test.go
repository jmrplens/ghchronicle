package run

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheStateCommentAccountsForEveryFieldItKeeps reads this package's own
// source and fails when State persists something its doc comment does not
// mention.
//
// The comment said "Two things only" while the type had grown to six, and the
// four it left out are the four with consequences: deleting the file loses the
// commit a dependency diff starts from, when each family last read a whole
// page, where the notification window was cut, and the newest event seen. Two
// documents and the site repeated the sentence, because the comment is what
// they were written from.
func TestTheStateCommentAccountsForEveryFieldItKeeps(t *testing.T) {
	doc, fields := stateDocAndFields(t)
	for _, name := range fields {
		if !strings.Contains(doc, name) {
			t.Errorf("State persists %q and its doc comment never names it:\n%s", name, doc)
		}
	}
	if strings.Contains(doc, "Two things only") {
		t.Errorf("State keeps %d fields and its doc comment still says two:\n%s", len(fields), doc)
	}
}

// stateDocAndFields returns State's doc comment and the json name of every
// field it writes to disk, read out of the source rather than by reflection
// because the comment is only in the source.
func stateDocAndFields(t *testing.T) (string, []string) {
	t.Helper()
	doc, st := stateStruct(t)
	var names []string
	for _, field := range st.Fields.List {
		if field.Tag == nil {
			continue // path, which is not persisted
		}
		_, tag, _ := strings.Cut(field.Tag.Value, `json:"`)
		key, _, _ := strings.Cut(tag, `"`)
		if name, _, _ := strings.Cut(key, ","); name != "" {
			names = append(names, name)
		}
	}
	if doc == "" || len(names) == 0 {
		t.Fatal("state.go has no documented State struct, so this test proves nothing")
	}
	return doc, names
}

// stateStruct finds the State declaration in this package's own source.
func stateStruct(t *testing.T) (string, *ast.StructType) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "state.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("cannot read state.go: %v", err)
	}
	for _, decl := range file.Decls {
		gen, isType := decl.(*ast.GenDecl)
		if !isType || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, isSpec := spec.(*ast.TypeSpec)
			if !isSpec || ts.Name.Name != "State" {
				continue
			}
			if st, isStruct := ts.Type.(*ast.StructType); isStruct {
				return gen.Doc.Text(), st
			}
		}
	}
	t.Fatal("state.go declares no State struct, so this test proves nothing")
	return "", nil
}

// TestAStateFileThatNullsItsMapsStillLoadsUsable: a hand-edited or truncated
// state can say null where a map was, and the sweep that loads it writes to
// every one of those maps, which on a nil map is a panic at start-up.
func TestAStateFileThatNullsItsMapsStillLoadsUsable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	nulled := `{"last_run":null,"first_saw":null,"history_read":null,"last_head":null,"last_full":null,"last_event":"42"}`
	if err := os.WriteFile(path, []byte(nulled), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadState(path)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s.Mark("traffic", now)
	s.MarkFull("issues", now)
	s.LastHead["o/n"] = "aaa"
	if !s.FirstSight("o/n") {
		t.Error("a repository nobody walked reads as walked")
	}
	s.MarkSeen("o/n", now)
	if s.FirstSight("o/n") {
		t.Error("a repository recorded as walked still reads as never walked")
	}
	s.MarkHistory("o/n", now)
	if s.LastEvent != "42" {
		t.Errorf("LastEvent = %q, want what the file said beside the nulls", s.LastEvent)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if saved := LoadState(path); !saved.LastRun["traffic"].Equal(now) || saved.LastHead["o/n"] != "aaa" {
		t.Errorf("the state written after the nulls = %+v", saved)
	}
}

// TestAStateWithoutAPathIsNeverWritten: the tests and the one-shot probe
// keep their state in memory, and Save must not drop a file named after
// nothing into whatever directory the process runs in.
func TestAStateWithoutAPathIsNeverWritten(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := LoadState("")
	s.Mark("traffic", time.Now())
	if err := s.Save(); err != nil {
		t.Fatalf("Save of a state with no path = %v", err)
	}
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Errorf("a state with no path wrote %v (%v)", left, err)
	}
}

// TestAStateNamedWithoutADirectoryIsSavedInTheWorkingOne: a bare file name
// has no directory to create, and asking for "." must not stand in the way.
func TestAStateNamedWithoutADirectoryIsSavedInTheWorkingOne(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s := LoadState("state.json")
	s.Mark("traffic", now)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if saved := LoadState(filepath.Join(dir, "state.json")); !saved.LastRun["traffic"].Equal(now) {
		t.Errorf("the state saved beside the process = %+v", saved)
	}
}

// TestAStateThatCannotBeWrittenSaysWhy: each of the three steps of a save
// can fail, and each failure is returned, so the sweep can say the state was
// not kept instead of believing it was.
func TestAStateThatCannotBeWrittenSaysWhy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the temporary file goes, and a non-empty directory
	// where the state itself goes.
	for _, blocked := range []string{filepath.Join(dir, "tmp", "state.json.tmp"), filepath.Join(dir, "over", "state.json", "x")} {
		if err := os.MkdirAll(blocked, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range map[string]string{
		"a parent directory that is a file":      filepath.Join(file, "state.json"),
		"a directory in the way of the new file": filepath.Join(dir, "tmp", "state.json"),
		"a directory in the way of the state":    filepath.Join(dir, "over", "state.json"),
	} {
		if err := LoadState(path).Save(); err == nil {
			t.Errorf("%s: Save reported success", name)
		}
	}
}

// TestAWholeReadIsDueTheMomentItsIntervalHasPassed: the inbox is read whole
// once a day, and a sweep landing exactly a day after the last whole read is
// that day's read. Putting it off to the sweep after would let the daily read
// slip a cadence further every day it lands on the mark.
func TestAWholeReadIsDueTheMomentItsIntervalHasPassed(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s := LoadState("")
	if !s.FullDue("notifs", fullInboxEvery, last) {
		t.Error("a whole read never done is not due")
	}
	s.MarkFull("notifs", last)
	for _, tc := range []struct {
		name string
		at   time.Time
		due  bool
	}{
		{"a moment short of a day", last.Add(fullInboxEvery - time.Nanosecond), false},
		{"exactly a day", last.Add(fullInboxEvery), true},
		{"past a day", last.Add(fullInboxEvery + time.Minute), true},
	} {
		if got := s.FullDue("notifs", fullInboxEvery, tc.at); got != tc.due {
			t.Errorf("%s after the last whole read: due = %t, want %t", tc.name, got, tc.due)
		}
	}
}

// TestAStateFromBeforeTheStarHistoryReadsEveryHistoryOnce: a state file
// written before history_read existed has repositories in first_saw and no
// history_read at all, and that has to read as "never read whole" for every
// one of them, so the first sweep after the upgrade reads each history back
// to its first week. Once recorded, the record survives the file.
func TestAStateFromBeforeTheStarHistoryReadsEveryHistoryOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	older := `{"last_run":{"stars":"2026-09-12T10:00:00Z"},"first_saw":{"o/a":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadState(path)
	if !s.HistoryDue("o/a") || !s.HistoryDue("o/never-seen") {
		t.Fatal("a state without history_read did not read as every history unread")
	}
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s.MarkHistory("o/a", now)
	if s.HistoryDue("o/a") {
		t.Error("a history recorded as read whole is still due")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	saved := LoadState(path)
	if saved.HistoryDue("o/a") || !saved.HistoryRead["o/a"].Equal(now) {
		t.Errorf("history_read after a save = %v, want o/a at %s", saved.HistoryRead, now)
	}
	if !saved.HistoryDue("o/b") {
		t.Error("a repository never read reads as read")
	}
}
