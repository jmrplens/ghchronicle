package sink

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"
)

// StdoutJSON prints one JSON object per line, the same shape the file sink
// writes, for piping into a shipper that reads JSON rather than line
// protocol. It is a second type rather than a mode of Stdout so the two
// renderings stay in the file that owns each of them.
type StdoutJSON struct {
	mu sync.Mutex
	w  *bufio.Writer
	// render borrows the file sink's JSON shape, so a consumer that reads
	// both sees one format, not two that drift apart.
	render *File
}

func NewStdoutJSON() *StdoutJSON { return newStdoutJSON(os.Stdout) }

func newStdoutJSON(w io.Writer) *StdoutJSON {
	return &StdoutJSON{w: bufio.NewWriter(w), render: &File{Format: "json"}}
}

func (s *StdoutJSON) Name() string { return "stdout" }

func (s *StdoutJSON) Write(_ context.Context, points []Point) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range points {
		line, err := s.render.render(p)
		if err != nil {
			return err
		}
		if _, err = s.w.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return s.w.Flush()
}

func (s *StdoutJSON) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Flush()
}
