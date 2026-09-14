package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileSinkWritesLineProtocol(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "points.lp")
	cfg := writeSinkConfig(t, dir, gh.URL(), `  file:
    path: `+out+`
    format: influx`)

	sweepOnce(t, cfg)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the file sink wrote nothing: %v", err)
	}
	points := parseLineProtocol(t, string(raw))
	if len(points) < 100 {
		t.Fatalf("only %d lines were written", len(points))
	}
	byName := measurementsOf(points)
	for _, want := range []string{"gh_traffic", "gh_repo", "gh_workflow_run"} {
		if byName[want] == 0 {
			t.Errorf("no %s line; measurements seen: %v", want, sortedNames(byName))
		}
	}
	for _, p := range points {
		for k, v := range p.Tags {
			// An empty tag is not a tag in the line protocol, so it must not
			// be written as one either.
			if v == "" {
				t.Errorf("%s has an empty tag %q", p.Measurement, k)
				break
			}
		}
	}
}

func TestFileSinkWritesJSON(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "points.jsonl")
	cfg := writeSinkConfig(t, dir, gh.URL(), `  file:
    path: `+out+`
    format: json`)

	sweepOnce(t, cfg)

	points := readPoints(t, out)
	if len(points) < 100 {
		t.Fatalf("only %d objects were written", len(points))
	}
	for i, p := range points {
		if _, err := time.Parse(time.RFC3339Nano, p.Time); err != nil {
			t.Fatalf("object %d (%s): time %q does not parse: %v", i, p.Measurement, p.Time, err)
		}
		if len(p.Fields) == 0 {
			t.Errorf("object %d (%s) has no field", i, p.Measurement)
		}
	}
}

// TestFileSinkRotatesAtTheConfiguredSize pins both halves of the retention:
// the file is rotated once it passes max_bytes, and only keep of them survive.
func TestFileSinkRotatesAtTheConfiguredSize(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "points.jsonl")
	const maxBytes = 8192
	cfg := writeSinkConfig(t, dir, gh.URL(), fmt.Sprintf(`  file:
    path: %s
    format: json
    max_bytes: %d
    keep: 2`, out, maxBytes))

	sweepOnce(t, cfg)

	for _, name := range []string{out, out + ".1", out + ".2"} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s is missing: %v", filepath.Base(name), err)
		}
		if name == out {
			continue
		}
		// A rotated file is closed on the line that took it past the limit, so
		// it is at least max_bytes and at most that plus one line.
		if st.Size() < maxBytes {
			t.Errorf("%s is %d bytes, rotated before max_bytes", filepath.Base(name), st.Size())
		}
		if st.Size() > maxBytes+4096 {
			t.Errorf("%s is %d bytes, well past max_bytes", filepath.Base(name), st.Size())
		}
	}
	// keep: 2 means two rotated files, so the third must have been removed.
	if _, err := os.Stat(out + ".3"); err == nil {
		t.Errorf("points.jsonl.3 survived a retention of two")
	}
	// Rotation must not corrupt what it splits.
	for _, name := range []string{out, out + ".1", out + ".2"} {
		for i, line := range strings.Split(strings.TrimSpace(readFile(t, name)), "\n") {
			var p point
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				t.Fatalf("%s line %d is not JSON: %v\n%s", filepath.Base(name), i+1, err, line)
			}
		}
	}
}

func TestStdoutSinkPrintsLineProtocol(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), "  stdout: true")

	stdout, stderr, err := runSplit(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("ghchronicle -once failed: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "sink write failed") {
		t.Errorf("the stdout sink failed:\n%s", stderr)
	}
	points := parseLineProtocol(t, stdout)
	if len(points) < 100 {
		t.Fatalf("only %d lines were printed", len(points))
	}
	if measurementsOf(points)["gh_traffic"] == 0 {
		t.Errorf("the traffic points were not printed")
	}
}

func TestStdoutSinkPrintsJSON(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), "  stdout: true\n  stdout_format: json")

	stdout, stderr, err := runSplit(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("ghchronicle -once failed: %v\n%s", err, stderr)
	}
	printed := 0
	for i, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p point
		if decodeErr := json.Unmarshal([]byte(line), &p); decodeErr != nil {
			t.Fatalf("printed line %d is not JSON: %v\n%s", i+1, decodeErr, line)
		}
		if p.Measurement == "" {
			t.Fatalf("printed line %d has no measurement: %s", i+1, line)
		}
		printed++
	}
	if printed < 100 {
		t.Fatalf("only %d objects were printed", printed)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
