//go:build unix

package sink

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFileSetsTheModeTheUmaskWouldHaveTakenAway(t *testing.T) {
	// open(2) masks the mode it is handed with the process umask, so carrying
	// a mode into the file that replaces a rotated one is not by itself
	// enough: under a unit that sets UMask=0077 a 0640 grant would arrive as
	// 0600 and the shipper would stop reading, with nothing logged at either
	// end. The sink therefore sets the mode on a file it creates instead of
	// asking open(2) for it and hoping.
	//
	// The umask here takes an owner bit rather than the group bit a real
	// grant needs, because this tree's lint refuses any file-mode literal
	// above 0600, tests included. It is the same masking either way, and this
	// version can be written down.
	// The temporary directory is made before the umask is narrowed, since a
	// directory this test cannot list afterwards is a cleanup failure rather
	// than a result.
	dir := t.TempDir()
	old := syscall.Umask(0o400)
	t.Cleanup(func() { syscall.Umask(old) })

	path := filepath.Join(dir, "out.lp")
	f := NewFile(path, "influx", 0, 0)
	if err := f.Write(context.Background(), []Point{{
		Measurement: "m", Tags: map[string]string{"a": "b"},
		Fields: map[string]any{"v": 1}, Time: time.Now(),
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
	if got := st.Mode().Perm(); got != dumpMode {
		t.Errorf("the dump is mode %04o under a umask, want %04o", got, dumpMode)
	}
}
