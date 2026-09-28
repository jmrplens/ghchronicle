//go:build !(unix && !aix) && !windows

package run

import "os"

// lockFile takes nothing on a system with no advisory lock this package
// knows how to take, and reports the file free. The binary is released for
// Linux, macOS and Windows; a build for anything else runs as it did before
// the lock existed, with nothing to keep -migrate -yes from running beside
// the service.
func lockFile(*os.File) (held bool, err error) { return false, nil }
