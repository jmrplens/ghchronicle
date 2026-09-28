package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The kinds of run that hold the lock, which is what tells a process that
// finds it held whether to wait or to stop.
const (
	// HeldByService is the long-running collector, which holds it for as
	// long as it runs. A second one on the same state file would undo the
	// first one's marks, so it stops rather than waits.
	HeldByService = "service"
	// HeldByMigrate is -migrate -yes, which holds it while it applies and
	// reads the history again, and then lets go.
	HeldByMigrate = "-migrate -yes"
	// HeldByStart is a one-shot run bringing a store along before its sweep,
	// which lets go as soon as that is done.
	HeldByStart = "a run applying migrations before its sweep"
)

// Holder is who holds the lock, as the holder wrote it into the file.
type Holder struct {
	PID     int       `json:"pid"`
	Run     string    `json:"run"`
	Release string    `json:"release"`
	Since   time.Time `json:"since"`
}

// String names the holder the way a reader goes looking for it: by process.
func (h Holder) String() string {
	if h.PID == 0 {
		// Taken a moment ago and not written yet, or written by something
		// that is not this binary: all that is known is that it is held.
		return "another process"
	}
	return fmt.Sprintf("process %d (ghchronicle %s, %s, since %s)", h.PID, h.Release, h.Run,
		h.Since.UTC().Format(time.DateTime+" UTC"))
}

// HeldError is a lock another process holds.
type HeldError struct {
	Path   string
	Holder Holder
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s holds %s", e.Holder, e.Path)
}

// Lock is this process's hold on the file beside a state file that says
// nobody else is changing the stores or the state file.
//
// It is an advisory lock the operating system takes back when the process
// ends, however it ends, so a crash or a kill -9 leaves nothing to clean up
// and nothing that has to be recognized as stale. The file itself stays: it
// only carries the name of whoever last held it, and removing it would let a
// process that is waiting lock a file nobody else can see.
type Lock struct {
	f    *os.File
	path string
}

// TakeLock takes the lock at path without waiting, describing this process
// as run so that another that finds it held can say by whom. A *HeldError is
// a lock somebody else holds; any other error is a file that cannot be
// opened at all.
func TakeLock(path, run, release string, now time.Time) (*Lock, error) {
	if path == "" {
		return &Lock{}, nil
	}
	if dir := filepath.Dir(path); dir != "." {
		// The mode the state file's own directory is made with: see Save.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := lockFile(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if held {
		_ = f.Close()
		return nil, &HeldError{Path: path, Holder: readHolder(path)}
	}
	// Written after the lock is taken, so what the file says is always about
	// a process that held it. A failure here costs the next reader the name
	// and nothing else, so the lock is kept.
	nameHolder(f, Holder{PID: os.Getpid(), Run: run, Release: release, Since: now.UTC()})
	return &Lock{f: f, path: path}, nil
}

// nameHolder writes who holds the lock over whoever held it last.
func nameHolder(f *os.File, h Holder) {
	me, err := json.Marshal(h)
	if err != nil {
		return
	}
	if err = f.Truncate(0); err == nil {
		if _, err = f.WriteAt(me, 0); err == nil {
			_ = f.Sync()
		}
	}
}

// Release lets go of the lock. The operating system does the same when the
// process ends, so a run that exits without calling it loses nothing.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// readHolder is what the holder wrote. A file that cannot be read or does
// not parse names nobody, which Holder.String says.
func readHolder(path string) Holder {
	var h Holder
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &h)
	}
	return h
}

// IsHeld reports whether err is a lock another process holds, and by whom.
func IsHeld(err error) (*HeldError, bool) {
	var held *HeldError
	ok := errors.As(err, &held)
	return held, ok
}
