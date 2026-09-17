package sink

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingWriter refuses every write, the way a closed pipe does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestStdoutPrintsLineProtocol writes one line per point that has a field,
// and flushes each batch so a pipe reader sees it at once.
func TestStdoutPrintsLineProtocol(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	s := newStdout(&out)
	if s.Name() != "stdout" {
		t.Errorf("Name = %q, want stdout", s.Name())
	}
	points := []Point{star("a"), {Measurement: "gh_star", Tags: map[string]string{"repo": "empty"}}, star("b")}
	if _, err := s.Write(t.Context(), points); err != nil {
		t.Fatal(err)
	}
	want := "gh_star,repo=a starred=1i 1700000000000000000\ngh_star,repo=b starred=1i 1700000000000000000\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q, flushed before Write returned", out.String(), want)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestStdoutReportsAWriteThatFails returns the error rather than losing the
// batch in a buffer nobody flushes, whether it fails on the flush or on a line
// too long to buffer.
func TestStdoutReportsAWriteThatFails(t *testing.T) {
	t.Parallel()
	if _, err := newStdout(failingWriter{}).Write(t.Context(), []Point{star("a")}); err == nil {
		t.Error("Write reported success through a writer that refused the flush")
	}
	long := star(strings.Repeat("r", 8192))
	if _, err := newStdout(failingWriter{}).Write(t.Context(), []Point{long}); err == nil {
		t.Error("Write reported success through a writer that refused a line too long to buffer")
	}
}

// TestNewStdoutWritesToTheProcessOutput builds both stdout sinks the way the
// binary does, without writing through them.
func TestNewStdoutWritesToTheProcessOutput(t *testing.T) {
	t.Parallel()
	if NewStdout().Name() != "stdout" || NewStdoutJSON().Name() != "stdout" {
		t.Error("the stdout sinks do not call themselves stdout")
	}
}

// TestLogWriterRotatesAtItsLimit moves the file aside as soon as a line takes
// it to its limit, and keeps only the number of old files asked for. Every
// line here is the limit on its own, so every write rotates.
func TestLogWriterRotatesAtItsLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "logs", "ghchronicle.log")
	w := NewLogWriter(path, 16, 2)
	lines := make([]string, 4)
	for i, word := range []string{"first", "second", "third", "fourth"} {
		lines[i] = fmt.Sprintf("%-15s\n", word)
		if n, err := w.Write([]byte(lines[i])); err != nil || n != len(lines[i]) {
			t.Fatalf("Write = %d, %v, want the whole line", n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{".1": lines[3], ".2": lines[2]} {
		got, err := os.ReadFile(path + name)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", name, got, err, want)
		}
	}
	if got, err := os.ReadFile(path); err != nil || len(got) != 0 {
		t.Errorf("the live log = %q, %v, want a fresh empty file once its line was rotated away", got, err)
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a third old file exists (%v), want two kept", err)
	}
}

// TestLogWriterReportsAPlaceItCannotWrite fails the write when the log's
// directory cannot be made, rather than dropping the line in silence.
func TestLogWriterReportsAPlaceItCannotWrite(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := NewLogWriter(filepath.Join(file, "ghchronicle.log"), 0, 0).Write([]byte("x\n")); err == nil || n != 0 {
		t.Errorf("Write = %d, %v, want nothing written and the failure", n, err)
	}
}

// TestTeeWritesToBoth keeps the journal when a log file is configured too.
func TestTeeWritesToBoth(t *testing.T) {
	t.Parallel()
	var a, b strings.Builder
	if _, err := Tee(&a, &b).Write([]byte("line\n")); err != nil || a.String() != "line\n" || b.String() != "line\n" {
		t.Errorf("Tee wrote %q and %q (%v), want the line in both", a.String(), b.String(), err)
	}
}

// TestLogWriterReportsARotationThatFails keeps the line it already wrote and
// reports the rotation that failed, and when the write itself failed first it
// reports that write rather than the rotation it led to: the first error is
// the one that says what went wrong.
func TestLogWriterReportsARotationThatFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ghchronicle.log")
	// A directory with something in it where the rotated file must go.
	if err := os.MkdirAll(filepath.Join(path+".1", "inside"), 0o750); err != nil {
		t.Fatal(err)
	}
	w := NewLogWriter(path, 4, 1)
	t.Cleanup(func() { _ = w.Close() })
	n, err := w.Write([]byte("line\n"))
	if n != 5 || err == nil {
		t.Errorf("Write = %d, %v, want the line counted and the rotation failure", n, err)
	}
	if b, readErr := os.ReadFile(path); readErr != nil || string(b) != "line\n" {
		t.Errorf("log = %q, %v, want the line kept", b, readErr)
	}

	closed := NewLogWriter(filepath.Join(t.TempDir(), "ghchronicle.log"), 1<<20, 1)
	if _, err = closed.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err = closed.f.fh.Close(); err != nil {
		t.Fatal(err)
	}
	closed.f.MaxBytes = 1
	_, err = closed.Write([]byte("second\n"))
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "write" {
		t.Errorf("Write = %v, want the failed write reported, not the rotation after it", err)
	}
}

// TestLogWriterDoesNotRotateBelowItsLimit leaves a log that has room alone.
func TestLogWriterDoesNotRotateBelowItsLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ghchronicle.log")
	w := NewLogWriter(path, 1<<20, 1)
	if _, err := w.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a log with room was rotated (%v)", err)
	}
}
