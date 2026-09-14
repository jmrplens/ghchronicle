package sink

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"
)

// Stdout prints line protocol. It is the sink for seeing what would be written
// before pointing the tool at a database, and for piping into Telegraf.
type Stdout struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func NewStdout() *Stdout { return newStdout(os.Stdout) }

// newStdout is NewStdout writing to w, which is how a test reads it back.
func newStdout(w io.Writer) *Stdout { return &Stdout{w: bufio.NewWriter(w)} }

func (s *Stdout) Name() string { return "stdout" }

func (s *Stdout) Write(_ context.Context, points []Point) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range points {
		if l := LineProtocol(p); l != "" {
			if _, err := s.w.WriteString(l + "\n"); err != nil {
				return err
			}
		}
	}
	return s.w.Flush()
}

func (s *Stdout) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Flush()
}
