package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheLockIsHeldUntilItIsReleasedAndNamesItsHolder: a second taker is
// refused while the first holds the lock, and told who holds it; once the
// first lets go the second gets it, and the file then names the second.
//
// Two takers in one process are two open files, which is what two processes
// are to flock and to LockFileEx alike.
func TestTheLockIsHeldUntilItIsReleasedAndNamesItsHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state-lock")
	since := time.Date(2026, 9, 28, 21, 0, 0, 0, time.UTC)
	first, err := TakeLock(path, HeldByService, "2.6.2", since)
	if err != nil {
		t.Fatalf("the first taker was refused: %v", err)
	}
	_, err = TakeLock(path, HeldByMigrate, "2.6.2", since.Add(time.Hour))
	held, ok := IsHeld(err)
	if !ok {
		t.Fatalf("the second taker got %v, want the lock refused as held", err)
	}
	h := held.Holder
	if h.PID != os.Getpid() || h.Run != HeldByService || h.Release != "2.6.2" || !h.Since.Equal(since) {
		t.Errorf("the holder is read as %+v, want this process, the service, 2.6.2, since %s", h, since)
	}
	msg := held.Error()
	for _, want := range []string{"process ", "ghchronicle 2.6.2", "service", "2026-09-28 21:00:00 UTC", path} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal %q does not say %q", msg, want)
		}
	}

	if err = first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := TakeLock(path, HeldByMigrate, "2.6.2", since.Add(time.Hour))
	if err != nil {
		t.Fatalf("the lock was not free after its holder let go: %v", err)
	}
	defer func() { _ = second.Release() }()
	if h = readHolder(path); h.Run != HeldByMigrate {
		t.Errorf("the file names %+v after the second taker got it, want -migrate -yes", h)
	}
	if err = first.Release(); err != nil {
		t.Errorf("letting go twice is not a failure: %v", err)
	}
}

// TestALockNobodyNamedSaysAnotherProcess: a holder that has not written its
// name yet, or a file this binary did not write, is still a lock that is
// held, and is said as such rather than as a zero process.
func TestALockNobodyNamedSaysAnotherProcess(t *testing.T) {
	if got := (Holder{}).String(); got != "another process" {
		t.Errorf("an unnamed holder is %q", got)
	}
	path := filepath.Join(t.TempDir(), "state-lock")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := readHolder(path); h.PID != 0 {
		t.Errorf("a file that does not parse names %+v", h)
	}
}

// TestNoStateFileMeansNothingToLock: a configuration with no state file has
// nothing two processes could both write, so the lock is taken at once.
func TestNoStateFileMeansNothingToLock(t *testing.T) {
	l, err := TakeLock("", HeldByService, "2.6.2", time.Now())
	if err != nil || l == nil {
		t.Fatalf("TakeLock with no path = %v, %v", l, err)
	}
	if err = l.Release(); err != nil {
		t.Error(err)
	}
}
