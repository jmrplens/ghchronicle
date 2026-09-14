//go:build windows

// rename_windows.go is the rename Windows needs to keep the promise the POSIX
// one keeps for free: that the sink can put a file in place over one somebody
// marked read-only.
//
// os.Rename on Windows is MoveFileEx with MOVEFILE_REPLACE_EXISTING, and NTFS
// refuses to replace a read-only file even when told to replace: "the rename
// operation will still fail if a file with the same name already exists and
// is a directory, a read-only file, or a currently executing file"
// (FILE_RENAME_INFORMATION, Microsoft Learn). The read-only attribute is also
// what os.Chmod sets there for any mode without the owner's write bit, so a
// `chmod 0400` on the ledger, or a dump the operator made read-only, would
// otherwise fail every later save or rotation with "Access is denied" while
// the same files rotate and save on Linux and macOS.

package sink

import (
	"errors"
	"io/fs"
	"os"
)

// replaceFile renames from over to, clearing the read-only attribute on to
// when that is the one thing refusing the rename.
//
// The attribute is cleared only after a rename has failed with a permission
// error and only on a regular file that has it, so a rename refused for any
// other reason (the target open in another process, a directory in the way)
// reports that reason and leaves the target as it was. If the retry fails
// after the attribute was cleared, the file stays writable: it was about to be
// replaced, and the error is returned either way.
func replaceFile(from, to string) error {
	err := os.Rename(from, to)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	st, statErr := os.Lstat(to)
	if statErr != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o200 != 0 {
		return err
	}
	// Windows reads only the owner's write bit out of a mode, and clears the
	// read-only attribute when it is set, so this is that and nothing more.
	if chmodErr := os.Chmod(to, 0o600); chmodErr != nil {
		return errors.Join(err, chmodErr)
	}
	return os.Rename(from, to)
}
