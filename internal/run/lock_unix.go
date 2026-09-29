//go:build unix && !aix

package run

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive flock on f without waiting, and reports whether
// somebody else already has one. flock rather than a lock on a byte range,
// because a byte-range lock belongs to the process and is dropped when any
// descriptor of the file is closed, which the holder's own read of the file
// would do.
func lockFile(f *os.File) (held bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// unlockFile lets go of the flock before the descriptor is closed. Closing
// would drop it too; saying so keeps the release in one place with the take.
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
