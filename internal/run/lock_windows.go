//go:build windows

package run

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f without waiting, and reports whether
// somebody else already has one.
//
// The byte locked is far past anything the file holds, at 4 GiB, because a
// locked range on Windows cannot be read by another process, and the holder's
// name at the start of the file is what that process wants to read.
func lockFile(f *os.File) (held bool, err error) {
	overlapped := windows.Overlapped{OffsetHigh: 1}
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}
