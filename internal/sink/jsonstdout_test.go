package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestStdoutJSONMatchesTheFileSink(t *testing.T) {
	p := Point{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"stars": 3}, Time: time.Unix(0, 1700000000000000000),
	}
	var buf bytes.Buffer
	s := newStdoutJSON(&buf)
	if err := s.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	fileShape, _ := (&File{Format: "json"}).render(p)
	if got := buf.String(); got != fileShape+"\n" {
		t.Errorf("stdout json = %q\nfile json = %q", got, fileShape)
	}
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("not one JSON object per line: %v", err)
	}
	if m["time"] != "2023-11-14T22:13:20Z" || m["measurement"] != "gh_repo" {
		t.Errorf("object = %v", m)
	}
}

// TestStdoutJSONFlushesEveryBatchAndReportsFailures prints each point of a
// batch before Write returns, without waiting for Close, so a pipe reader sees
// it at once; and it returns a point it cannot encode or a line the writer
// refuses rather than dropping the batch.
func TestStdoutJSONFlushesEveryBatchAndReportsFailures(t *testing.T) {
	var buf bytes.Buffer
	s := newStdoutJSON(&buf)
	at := time.Unix(0, 0)
	if err := s.Write(context.Background(), []Point{
		{Measurement: "a", Fields: map[string]any{"v": 1}, Time: at},
		{Measurement: "b", Fields: map[string]any{"v": 2}, Time: at},
	}); err != nil {
		t.Fatal(err)
	}
	want := `{"time":"1970-01-01T00:00:00Z","measurement":"a","fields":{"v":1}}` + "\n" +
		`{"time":"1970-01-01T00:00:00Z","measurement":"b","fields":{"v":2}}` + "\n"
	if buf.String() != want {
		t.Errorf("stdout before Close = %q, want %q", buf.String(), want)
	}

	if err := newStdoutJSON(&buf).Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": math.NaN()}, Time: at}}); err == nil {
		t.Error("Write accepted a field JSON cannot hold")
	}
	long := Point{Measurement: strings.Repeat("m", 8192), Fields: map[string]any{"v": 1}, Time: at}
	if err := newStdoutJSON(failingWriter{}).Write(context.Background(), []Point{long, long}); err == nil {
		t.Error("Write reported success through a writer that refused a line too long to buffer")
	}
}
