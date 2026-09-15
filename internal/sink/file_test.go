package sink

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFileWritesLineProtocolAndJSON(t *testing.T) {
	dir := t.TempDir()
	p := Point{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"stars": 3}, Time: time.Unix(0, 1700000000000000000),
	}

	lp := filepath.Join(dir, "out.lp")
	f := NewFile(lp, "influx", 0, 0)
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, _ := os.ReadFile(lp)
	if !strings.HasPrefix(string(b), "gh_repo,repo=a stars=3i") {
		t.Errorf("line protocol = %q", b)
	}

	jp := filepath.Join(dir, "out.json")
	f = NewFile(jp, "json", 0, 0)
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, _ = os.ReadFile(jp)
	if !strings.Contains(string(b), `"measurement":"gh_repo"`) || !strings.Contains(string(b), `"time":"2023-11-14`) {
		t.Errorf("json = %q", b)
	}
}

func TestFileRotatesAndKeepsACount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.lp")
	// A tiny cap so every write rotates.
	f := NewFile(path, "influx", 10, 2)
	for i := range 5 {
		if err := f.Write(context.Background(), []Point{{
			Measurement: "m", Tags: map[string]string{"a": "b"},
			Fields: map[string]any{"v": i}, Time: time.Now(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 3 {
		t.Errorf("retention kept %d files, want at most the current plus two", len(entries))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the current file must exist after rotation: %v", err)
	}
}

func TestFileResumesTheSizeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.lp")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFile(path, "influx", 50, 2)
	// The very first write must rotate: the file is already over the cap, and
	// starting the counter at zero would let it grow without bound.
	if err := f.Write(context.Background(), []Point{{
		Measurement: "m", Tags: map[string]string{"a": "b"},
		Fields: map[string]any{"v": 1}, Time: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected a rotated file: %v", err)
	}
}

func TestFileIsCreatedForTheOwnerAlone(t *testing.T) {
	// The dump carries whatever the sweep collected, private repository names
	// and job log lines included, so it must not be created world readable.
	// Granting a shipper access to it is the operator's decision, and
	// TestFileRotationKeepsTheModeAnOperatorSet pins that the sink keeps it.
	//
	// The modes are written out rather than compared against dumpMode, for the
	// reason dumpMode gives. storedPerm turns each into what the platform can
	// hold: all of it everywhere but Windows, and there the one bit a mode
	// carries, which says the dump was created writable. Its privacy on
	// Windows is the directory's ACL, which dumpMode says the sink leaves to
	// the operator.
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "out.lp")
	f := NewFile(path, "influx", 0, 0)
	if err := f.Write(context.Background(), []Point{{
		Measurement: "m",
		Fields:      map[string]any{"v": 1}, Time: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.Mode().Perm(), storedPerm(0o600); got != want {
		t.Errorf("file mode = %04o, want %04o", got, want)
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !dst.IsDir() {
		t.Fatalf("%s is not a directory", filepath.Dir(path))
	}
	if got, want := dst.Mode().Perm(), storedDirPerm(0o750); got != want {
		t.Errorf("directory mode = %04o, want %04o", got, want)
	}
}

func TestFileRotationKeepsTheModeAnOperatorSet(t *testing.T) {
	// The dump exists to be tailed by a shipper running as its own user, and
	// granting that is an operator's chmod on the file or on the directory it
	// is created in. What this sink owes them is that the grant survives:
	// rotation creates the replacement with the mode of the file it is
	// renaming away, not with the default.
	//
	// The grant a shipper actually needs is 0640, which this test cannot
	// write: gosec rejects every file-mode literal above 0600 in this tree,
	// tests included. The mechanism copies whatever mode it finds rather than
	// reasoning about the bits, so any mode that is neither the default nor
	// what the sink would have chosen pins it.
	//
	// 0400 is that on every platform, and it is the only such mode on
	// Windows, which keeps nothing of a mode but the owner's write bit, as
	// the read-only attribute: 0200 would read back there as the default and
	// pin nothing. The dump is still written after the chmod, because the
	// sink holds it open across it and an open descriptor keeps the access it
	// was opened with. The replacement is created by the sink, which may
	// write a file it is creating whatever mode it asks for.
	const granted os.FileMode = 0o400

	dir := t.TempDir()
	path := filepath.Join(dir, "out.lp")
	f := NewFile(path, "influx", 1<<20, 3)
	p := Point{
		Measurement: "m", Tags: map[string]string{"a": "b"},
		Fields: map[string]any{"v": 1}, Time: time.Now(),
	}
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, granted); err != nil {
		t.Fatal(err)
	}

	// A cap of one byte, so the next write rotates.
	f.MaxBytes = 1
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, name := range []string{path, path + ".1"} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := st.Mode().Perm(), storedPerm(granted); got != want {
			t.Errorf("%s is mode %04o after rotation, want %04o", name, got, want)
		}
	}
}

func TestFileLeavesTheModeOfAFileItDidNotCreateAlone(t *testing.T) {
	// The other half of the promise, and the half a rotation cannot make: a
	// mode already on the file when this process arrives is somebody's
	// decision, so opening the file must not touch it, and the mode read
	// there is what a file created later is given. Both are what a restart
	// looks like from the sink's side.
	//
	// 0200 because the sink reopens the file here and so has to be able to
	// write it. On Windows that makes it the default, since the owner's
	// write bit is all a mode holds there, so this pins the part Windows can
	// express: opening the dump and replacing it both leave it writable
	// rather than read-only.
	const granted os.FileMode = 0o200

	path := filepath.Join(t.TempDir(), "out.lp")
	if err := os.WriteFile(path, []byte("m,a=b v=0i\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, granted); err != nil {
		t.Fatal(err)
	}

	// Room to spare, so nothing here rotates: this is the open path alone.
	f := NewFile(path, "influx", 1<<20, 3)
	p := Point{
		Measurement: "m", Tags: map[string]string{"a": "b"},
		Fields: map[string]any{"v": 1}, Time: time.Now(),
	}
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.Mode().Perm(), storedPerm(granted); got != want {
		t.Errorf("opening the dump changed it to mode %04o, want %04o left alone", got, want)
	}

	// And the mode read at open is the mode the next file is created with,
	// which is what happens when the dump is deleted underneath a running
	// process rather than rotated by it.
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if st, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if got, want := st.Mode().Perm(), storedPerm(granted); got != want {
		t.Errorf("the replacement dump is mode %04o, want the %04o it found", got, want)
	}
}

func TestRotationReplacesARotatedFileMarkedReadOnly(t *testing.T) {
	// A rotated file carries the dump's mode, so when an operator has made
	// the dump read-only its rotated copies are read-only too, and rotation
	// has to rename over them once every retained name is taken. rename(2)
	// does that whatever the target's mode; NTFS refuses to replace a
	// read-only file, which stopped rotation on Windows alone. The two cases
	// are the two renames rotate makes: the chain moving up one, which lands
	// on the oldest kept name, and the dump moving to ".1", which lands on an
	// existing file only when a single rotated file is kept.
	t.Run("the dump onto .1", func(t *testing.T) {
		rotateOntoReadOnly(t, 1, ".1")
	})
	t.Run("the chain onto the oldest kept name", func(t *testing.T) {
		rotateOntoReadOnly(t, 2, ".2")
	})
}

// rotateOntoReadOnly leaves a read-only file at the dump's name plus occupied,
// makes a sink that keeps keep rotated files rotate once, and fails unless the
// rotation replaced that file.
func rotateOntoReadOnly(t *testing.T, keep int, occupied string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.lp")
	stale := path + occupied
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o400); err != nil {
		t.Fatal(err)
	}
	if keep > 1 {
		// Something for the chain to move up onto the read-only name.
		if err := os.WriteFile(path+".1", []byte("previous\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A cap of one byte, so the write rotates.
	f := NewFile(path, "influx", 1, keep)
	if err := f.Write(context.Background(), []Point{{
		Measurement: "m", Tags: map[string]string{"a": "b"},
		Fields: map[string]any{"v": 1}, Time: time.Now(),
	}}); err != nil {
		t.Fatalf("rotating over a read-only %s: %v", occupied, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "stale\n" {
		t.Errorf("%s still holds what was there before the rotation", occupied)
	}
}

// TestNewFileFillsInOnlyWhatWasLeftOut gives a zero format, size and count
// their documented values. A keep of zero left as zero would delete the one
// rotated file a rotation had just made, since the name one past the count is
// the one a rotation removes.
func TestNewFileFillsInOnlyWhatWasLeftOut(t *testing.T) {
	f := NewFile("out.lp", "", 0, 0)
	if f.Format != "influx" || f.MaxBytes != 64<<20 || f.Keep != 5 || f.Name() != "file" {
		t.Errorf("defaults = format %q, max %d, keep %d, name %q, want influx, 64 MiB, 5 and file",
			f.Format, f.MaxBytes, f.Keep, f.Name())
	}
	f = NewFile("out.lp", "json", 10, 1)
	if f.Format != "json" || f.MaxBytes != 10 || f.Keep != 1 {
		t.Errorf("given = format %q, max %d, keep %d, want json, 10 and 1 kept", f.Format, f.MaxBytes, f.Keep)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close of a file never opened = %v, want nil", err)
	}
}

// TestFileWritesEveryPointOfABatch appends each point that renders and skips
// the one that does not, rather than stopping after the first line.
func TestFileWritesEveryPointOfABatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.lp")
	f := NewFile(path, "influx", 0, 0)
	at := time.Unix(0, 1700000000000000000)
	if err := f.Write(context.Background(), []Point{
		{Measurement: "m", Tags: map[string]string{"a": "b"}, Fields: map[string]any{"v": 1}, Time: at},
		{Measurement: "m", Fields: map[string]any{"none": nil}, Time: at},
		{Measurement: "m", Tags: map[string]string{"a": "c"}, Fields: map[string]any{"v": 2}, Time: at},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "m,a=b v=1i 1700000000000000000\nm,a=c v=2i 1700000000000000000\n"
	if string(b) != want {
		t.Errorf("file = %q, want %q", b, want)
	}
}

// TestFileRotatesWhenALineReachesTheLimitExactly treats MaxBytes as the size
// a file may reach and no more: a file that is exactly at the limit rotates.
func TestFileRotatesWhenALineReachesTheLimitExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.lp")
	p := Point{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}
	line := LineProtocol(p) + "\n"
	f := NewFile(path, "influx", int64(len(line)), 2)
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path + ".1"); err != nil || string(b) != line {
		t.Errorf("rotated file = %q, %v, want the line that filled the file", b, err)
	}
}

// TestFileJSONLeavesOutAnEmptyTag writes the same tag set the line protocol
// does, so the two dumps of one point describe the same series.
func TestFileJSONLeavesOutAnEmptyTag(t *testing.T) {
	line, err := (&File{Format: "json"}).render(Point{
		Measurement: "m", Tags: map[string]string{"repo": "a", "license": ""},
		Fields: map[string]any{"v": 1}, Time: time.Unix(0, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"1970-01-01T00:00:00Z","measurement":"m","tags":{"repo":"a"},"fields":{"v":1}}`
	if line != want {
		t.Errorf("json = %s\nwant   %s", line, want)
	}
}

// TestFileReportsAPointItCannotRender returns the encoder's refusal of a NaN
// rather than leaving the point out in silence.
func TestFileReportsAPointItCannotRender(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	f := NewFile(path, "json", 0, 0)
	t.Cleanup(func() { _ = f.Close() })
	err := f.Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": math.NaN()}, Time: time.Unix(0, 0)}})
	if err == nil {
		t.Fatal("Write accepted a field JSON cannot hold")
	}
	if b, readErr := os.ReadFile(path); readErr != nil || len(b) != 0 {
		t.Errorf("file = %q, %v, want nothing written", b, readErr)
	}
}

// TestFileCreatesTheDumpInTheWorkingDirectory handles a bare file name, whose
// directory is "." and needs no creating.
func TestFileCreatesTheDumpInTheWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	f := NewFile("out.lp", "influx", 0, 0)
	if err := f.Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile("out.lp"); err != nil || string(b) != "m v=1i 1\n" {
		t.Errorf("out.lp = %q, %v, want the line", b, err)
	}
}

// TestFileReportsADumpItCannotOpen fails the write when the name cannot be
// created, rather than dropping the batch.
func TestFileReportsADumpItCannotOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), strings.Repeat("x", 300))
	err := NewFile(path, "influx", 0, 0).Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}})
	if err == nil {
		t.Error("Write reported success for a file name no file system accepts")
	}
}

// TestFileReportsAWriteToAClosedDump returns the write error, and reports the
// close error of a rotation that finds the file already gone from under it.
func TestFileReportsAWriteToAClosedDump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.lp")
	f := NewFile(path, "influx", 0, 0)
	p := Point{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := f.fh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Write(context.Background(), []Point{p}); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Write to a closed descriptor = %v, want os.ErrClosed", err)
	}
	if err := f.rotate(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("rotate of a closed descriptor = %v, want os.ErrClosed", err)
	}
}

// TestFileReportsARotationThatCannotRename returns the rename that failed,
// both for the chain moving up and for the dump itself, and reports no
// rotation to a caller that would otherwise forget what the new file declares.
func TestFileReportsARotationThatCannotRename(t *testing.T) {
	for _, tc := range []struct {
		name, blocked string
		keep          int
	}{
		{"the chain onto a directory", ".2", 2},
		{"the dump onto a directory", ".1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.lp")
			if err := os.WriteFile(path+".1", []byte("previous\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// A directory with something in it, which no rename replaces.
			if err := os.RemoveAll(path + tc.blocked); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(path+tc.blocked, "inside"), 0o750); err != nil {
				t.Fatal(err)
			}
			f := NewFile(path, "influx", 1, tc.keep)
			if err := f.open(); err != nil {
				t.Fatal(err)
			}
			rotated, err := f.appendLine("m v=1i 1")
			if err == nil || rotated {
				t.Errorf("appendLine = %v, %v, want the failed rename and no rotation", rotated, err)
			}
		})
	}
}

// TestFileRotatesADumpDeletedFromUnderIt carries on when the dump was removed
// while open: there is nothing to rename, and the rotation starts a new file.
//
// Not on Windows, where the situation cannot arise: a file another handle
// holds open cannot be removed unless that handle was opened with
// FILE_SHARE_DELETE, which os.OpenFile does not ask for, so the removal below
// fails with "The process cannot access the file because it is being used by
// another process" instead of setting the test up.
func TestFileRotatesADumpDeletedFromUnderIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a dump cannot be removed while the sink holds it open on Windows")
	}
	path := filepath.Join(t.TempDir(), "out.lp")
	f := NewFile(path, "influx", 1<<20, 2)
	p := Point{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.MaxBytes = 1
	if err := f.Write(context.Background(), []Point{p}); err != nil {
		t.Fatalf("rotating a deleted dump: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("no fresh dump after the rotation: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rotated file appeared for a dump that was gone (%v)", err)
	}
}

// TestFileRotationLeavesNothingPastTheCount removes a rotated file one past the
// count, as happens when Keep is lowered across a restart, rather than moving
// it further up where nothing would ever delete it.
func TestFileRotationLeavesNothingPastTheCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.lp")
	for _, suffix := range []string{".1", ".2", ".3"} {
		if err := os.WriteFile(path+suffix, []byte(suffix+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := NewFile(path, "influx", 1, 2)
	if err := f.Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"out.lp", "out.lp.1", "out.lp.2"}; !slices.Equal(names, want) {
		t.Errorf("directory = %v, want %v", names, want)
	}
	if b, _ := os.ReadFile(path + ".2"); string(b) != ".1\n" {
		t.Errorf(".2 = %q, want what .1 held", b)
	}
}
