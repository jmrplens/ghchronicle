//go:build !windows

// rename_other.go is the rename every platform but Windows needs: rename(2)
// replaces the target whatever its mode says, because the mode of a file
// guards its contents and the name belongs to the directory.

package sink

import "os"

// replaceFile renames from over to.
func replaceFile(from, to string) error {
	return os.Rename(from, to)
}
