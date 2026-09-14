package sink

import (
	"io"
	"os"
	"sync"
)

// LogWriter is a rotating io.Writer for the tool's own log.
//
// It is here rather than in a logging package because it is the same rotation
// the File sink needs, and having two implementations of "append until it is
// too big, then rename" in one binary is one too many.
type LogWriter struct {
	f  *File
	mu sync.Mutex
}

// NewLogWriter returns a writer that appends to path and rotates it.
func NewLogWriter(path string, maxBytes int64, keep int) *LogWriter {
	return &LogWriter{f: NewFile(path, "raw", maxBytes, keep)}
}

func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.open(); err != nil {
		return 0, err
	}
	n, err := w.f.fh.Write(p)
	w.f.n += int64(n)
	if w.f.n >= w.f.MaxBytes {
		// A failed rotation must not lose the line that was already written,
		// so the error is reported but the byte count stands.
		if rerr := w.f.rotate(); rerr != nil && err == nil {
			err = rerr
		}
	}
	return n, err
}

func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// Tee writes to both, so a log file never means losing the journal.
func Tee(a, b io.Writer) io.Writer { return io.MultiWriter(a, b) }

var (
	_ io.Writer = (*LogWriter)(nil)
	_           = os.Stderr
)
