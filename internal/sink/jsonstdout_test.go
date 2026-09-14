package sink

import (
	"bytes"
	"context"
	"encoding/json"
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
