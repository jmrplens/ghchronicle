//go:build !windows

// perm_other_test.go says what a file mode reads back as on every platform but
// Windows, for the tests that pin the modes the sinks set and keep: exactly
// what was set, since these platforms store all nine permission bits.

package sink

import "os"

// storedPerm is the permission bits os.Stat reports for a regular file whose
// mode was set to m.
func storedPerm(m os.FileMode) os.FileMode { return m }

// storedDirPerm is the permission bits os.Stat reports for a directory created
// with mode m. The process umask can only take bits away, and the test
// runners' umask of 022 takes none of the ones asked for here.
func storedDirPerm(m os.FileMode) os.FileMode { return m }
