//go:build windows

// perm_windows_test.go says what a file mode reads back as on Windows, for the
// tests that pin the modes the sinks set and keep.
//
// A Windows file stores one bit of a mode: os.Chmod sets the read-only
// attribute when the owner's write bit is clear and clears it when it is set,
// os.OpenFile gives a file it creates the attribute on the same terms and
// leaves an existing file's alone, and os.Stat reports 0444 for a read-only
// file and 0666 for any other.
// Directories are created with no mode at all, and report 0777. Who may read
// either is the ACL inherited from the parent directory, which no mode says
// anything about, so the tests hold each file to the one bit it can carry.

package sink

import "os"

// storedPerm is the permission bits os.Stat reports for a regular file whose
// mode was set to m.
func storedPerm(m os.FileMode) os.FileMode {
	if m&0o200 != 0 {
		return 0o666
	}
	return 0o444
}

// storedDirPerm is the permission bits os.Stat reports for a directory created
// with any mode, since os.Mkdir does not pass one to Windows.
func storedDirPerm(os.FileMode) os.FileMode { return 0o777 }
