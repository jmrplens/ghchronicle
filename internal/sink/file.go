package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// File appends points to a file, rotating it by size.
//
// It exists for the setups that already have a log shipper. Telegraf tails a
// file of line protocol, Promtail and Vector tail a file of JSON, and neither
// needs this process to know anything about their backend. It is also the
// simplest possible durable buffer: if the database is down, the file still
// has the data.
type File struct {
	Path string
	// Format is "influx" for line protocol or "json" for one object per line.
	Format string
	// MaxBytes rotates the file once it passes this size. Zero means 64 MiB.
	MaxBytes int64
	// Keep is how many rotated files to leave behind. Zero means 5.
	Keep int

	mu sync.Mutex
	fh *os.File
	n  int64
	// perm is the mode the file on disk actually has, so a rotation can give
	// the replacement the same one. Zero until the first open.
	perm os.FileMode
}

// dumpMode is the mode a dump is born with. A sweep of a private account puts
// private repository names, Dependabot severities and whole job log lines in
// this file, so nothing here is created readable by anyone but its owner,
// which is the right default for a file whose reader is unknown when it is
// created.
//
// That is a default rather than the whole answer, because the file exists to
// be tailed. See open and rotate: whatever mode the file ends up with is the
// mode the next one is created with.
//
// It has to be a constant, since the value reaches os.OpenFile through
// createMode and gosec only reads literals. That means gosec is not the thing
// keeping this at 0600 any more: TestFileIsCreatedForTheOwnerAlone is. The
// directory mode below is written at its call site for the opposite reason,
// so that G301 goes on checking it, the way it does in unchanged.go.
//
// On Windows none of this is in the mode, and the sink cannot promise it
// there. A Windows file mode carries the owner's write bit and nothing else,
// stored as the read-only attribute; os.MkdirAll ignores the mode it is given;
// and who may read the dump is decided by the ACL the file inherits from its
// directory, which this sink neither sets nor reads. So on Windows the dump is
// created writable, with the directory's ACL, and keeping it private is the
// operator's ACL to set on that directory. That is also the grant a shipper
// needs there, and a replacement file inherits it with no help from the sink,
// so what open and rotate carry forward on Windows is the read-only attribute
// alone.
const dumpMode os.FileMode = 0o600

func NewFile(path, format string, maxBytes int64, keep int) *File {
	if format == "" {
		format = "influx"
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	if keep <= 0 {
		keep = 5
	}
	return &File{Path: path, Format: format, MaxBytes: maxBytes, Keep: keep}
}

func (f *File) Name() string { return "file" }

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fh == nil {
		return nil
	}
	err := f.fh.Close()
	f.fh = nil
	return err
}

func (f *File) Write(_ context.Context, points []Point) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.open(); err != nil {
		return err
	}
	for _, p := range points {
		line, err := f.render(p)
		if err != nil {
			return err
		}
		if line == "" {
			continue
		}
		if _, err = f.appendLine(line); err != nil {
			return err
		}
	}
	return nil
}

// appendLine writes one line to the open file and rotates once the file has
// reached MaxBytes. It reports whether a rotation happened, because a caller
// that remembers what the current file already declares, as the SQL sink
// does, has to forget it when a new file starts.
//
// A rotation that fails reports false: the new file may not exist, and the
// caller is returning the error anyway.
func (f *File) appendLine(line string) (bool, error) {
	n, err := f.fh.WriteString(line + "\n")
	if err != nil {
		return false, err
	}
	f.n += int64(n)
	if f.n < f.MaxBytes {
		return false, nil
	}
	err = f.rotate()
	return err == nil, err
}

func (f *File) render(p Point) (string, error) {
	if f.Format != "json" {
		return LineProtocol(p), nil
	}
	// A shape a log shipper can parse without a custom decoder: the timestamp
	// in RFC 3339 so a human can read it, and tags and fields kept apart so
	// their difference survives the trip.
	// Empty tag values are dropped, as the line protocol renderer drops
	// them, so the two outputs describe the same point.
	tags := make(map[string]string, len(p.Tags))
	for k, v := range p.Tags {
		if v != "" {
			tags[k] = v
		}
	}
	b, err := json.Marshal(struct {
		Time        string            `json:"time"`
		Measurement string            `json:"measurement"`
		Tags        map[string]string `json:"tags,omitempty"`
		Fields      map[string]any    `json:"fields"`
	}{
		Time:        p.Time.UTC().Format(time.RFC3339Nano),
		Measurement: p.Measurement,
		Tags:        tags,
		Fields:      p.Fields,
	})
	return string(b), err
}

func (f *File) open() error {
	if f.fh != nil {
		return nil
	}
	if dir := filepath.Dir(f.Path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	// A mode is only ever set on a file this creates: an existing one keeps
	// whatever it has, which is the whole point. Telegraf, Promtail, Vector
	// and Fluent Bit all read as their own user, none of them documents a
	// required mode, and all of them document the same remedy, which is a
	// group. So the grant is the operator's to make once, with a chmod or a
	// group-owned directory, and this sink's job is to leave it alone rather
	// than to guess a mode wide enough for a shipper it cannot see.
	fh, created, err := f.openOrCreate()
	if err != nil {
		return err
	}
	if created {
		// open(2) masks the mode it is handed with the process umask, so a
		// grant carried into a replacement file would arrive narrowed under a
		// unit that sets UMask=0077: 0640 would land as 0600 and the shipper
		// would stop reading at the first rotation, which is the failure this
		// whole arrangement exists to avoid. Say the mode outright on the
		// file we just made, where there is no one else's decision to tread
		// on.
		if err = fh.Chmod(f.createMode()); err != nil {
			_ = fh.Close()
			return err
		}
	}
	// Start from the real size, so a restart does not reset the rotation
	// counter and let the file grow without bound, and from the real mode, so
	// a grant made while the process was down is carried into the file that
	// replaces this one.
	if st, statErr := fh.Stat(); statErr == nil {
		f.n = st.Size()
		f.perm = st.Mode().Perm()
	}
	f.fh = fh
	return nil
}

// openOrCreate opens the dump for appending and reports whether it had to
// create it, which is what decides if this process is entitled to set the
// mode.
//
// O_EXCL rather than a stat first, so there is no window in which the answer
// changes between the question and the open. If the file loses that race and
// is gone again by the second call, the O_CREATE there recreates it exactly as
// a single call would have done, so the race costs the explicit chmod and
// nothing else.
func (f *File) openOrCreate() (*os.File, bool, error) {
	fh, err := os.OpenFile(f.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, f.createMode())
	switch {
	case err == nil:
		return fh, true, nil
	case errors.Is(err, fs.ErrExist):
		fh, err = os.OpenFile(f.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, f.createMode())
		return fh, false, err
	default:
		return nil, false, err
	}
}

// createMode is the mode to create the next file with: the one the current
// file has, and the default until there is a file to read it from.
//
// Copying it forward is what makes a grant stick, together with the chmod in
// open that keeps the umask from taking part of it back. lumberjack, which
// most Go daemons rotate with, carries the mode of the file it is replacing
// the same way, and rsyslog sets a mode on creation only and never touches a
// file that already exists.
func (f *File) createMode() os.FileMode {
	if f.perm == 0 {
		return dumpMode
	}
	return f.perm
}

// rotate renames the current file out of the way and starts a new one. Numbered
// suffixes rather than timestamps, so the retention is a simple count and two
// rotations in the same second cannot collide.
func (f *File) rotate() error {
	// Read the mode back before the file is renamed away. An operator who
	// chmod'ed the dump has said who reads it, and without this the
	// replacement would be created at the default: the grant would work until
	// the file first filled up and then stop, with nothing logged at either
	// end, since a shipper that cannot open a file it is tailing is the
	// quietest failure there is.
	if st, err := f.fh.Stat(); err == nil {
		f.perm = st.Mode().Perm()
	}
	if err := f.fh.Close(); err != nil {
		return err
	}
	f.fh = nil
	// Every rename below lands on a name the rotation owns, so replaceFile
	// rather than os.Rename: a rotated file carries the dump's mode, and when
	// that mode is read-only Windows would refuse to replace it, which stops
	// rotation on Windows alone, as soon as every retained name is taken.
	for i := f.Keep - 1; i >= 1; i-- {
		older := fmt.Sprintf("%s.%d", f.Path, i)
		newer := fmt.Sprintf("%s.%d", f.Path, i+1)
		if _, err := os.Stat(older); err != nil {
			continue
		}
		if err := replaceFile(older, newer); err != nil {
			return err
		}
	}
	if err := replaceFile(f.Path, f.Path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	// The file one past the retention count usually does not exist, which is
	// what a first rotation looks like.
	_ = os.Remove(fmt.Sprintf("%s.%d", f.Path, f.Keep+1))
	f.n = 0
	return f.open()
}
