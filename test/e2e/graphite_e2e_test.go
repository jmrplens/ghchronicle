package e2e

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGraphiteSinkWritesPlaintextOverTCP(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	srv := newGraphiteServer(t)
	dir := t.TempDir()
	// No prefix, so the default "github" is the one under test.
	cfg := writeSinkConfig(t, dir, gh.URL(), `  graphite:
    addr: `+srv.Addr())

	sweepOnce(t, cfg)

	if !waitFor(30*time.Second, func() bool { return len(srv.Lines()) > 100 }) {
		t.Fatalf("only %d lines reached the Graphite listener", len(srv.Lines()))
	}
	lines := srv.Lines()

	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 3 {
			t.Fatalf("%q is not <path> <value> <timestamp>", line)
		}
		if err := graphiteLineProblem(line, parts[0], parts[1], parts[2]); err != nil {
			t.Error(err)
			break
		}
	}

	// The path is the dashboard's contract, so one whole path is pinned:
	// prefix, measurement without gh_, every tag value in tag key order
	// (full_name, kind, owner, repo), then the field.
	day := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC).Unix()
	want := fmt.Sprintf("github.traffic.octocat_hello-world.views.octocat.hello-world.count 120 %d", day)
	if !slices.Contains(lines, want) {
		t.Errorf("the traffic day did not arrive as %q; a sample of what did:\n%s",
			want, strings.Join(sample(lines, 12), "\n"))
	}
	// The slash in owner/repo becomes an underscore rather than nesting a
	// directory, and it is the same node either way.
	for _, line := range lines {
		if strings.Contains(line, "octocat/hello-world") {
			t.Errorf("a slash survived into a metric path: %q", line)
			break
		}
	}
}

// graphiteLineProblem is the first thing wrong with one plaintext line, taken
// apart into its path, value and timestamp, or nil when the line is one a
// Graphite dashboard can use.
func graphiteLineProblem(line, path, value, stamp string) error {
	if !strings.HasPrefix(path, "github.") {
		return fmt.Errorf("path %q does not start with the default prefix", path)
	}
	nodes := strings.Split(path, ".")
	if len(nodes) < 3 {
		return fmt.Errorf("path %q has no room for a measurement and a field", path)
	}
	if strings.HasPrefix(nodes[1], "gh_") {
		return fmt.Errorf("path %q kept the gh_ prefix on the measurement", path)
	}
	for _, n := range nodes {
		if n == "" {
			return fmt.Errorf("path %q has an empty node; an empty tag should be written as none", path)
		}
		if strings.ContainsAny(n, "/ ,") {
			return fmt.Errorf("path %q has a node that would split or nest: %q", path, n)
		}
	}
	if _, err := strconv.ParseFloat(value, 64); err != nil {
		return fmt.Errorf("%q: value does not parse: %w", line, err)
	}
	// Unix seconds, not the nanoseconds the line protocol uses. Anything
	// else lands the point tens of thousands of years away.
	sec, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%q: timestamp does not parse: %w", line, err)
	}
	if sec < 946684800 || sec > 4102444800 {
		return fmt.Errorf("%q: timestamp %d is not Unix seconds in a plausible range", line, sec)
	}
	return nil
}

func sample(lines []string, n int) []string {
	if len(lines) < n {
		return lines
	}
	return lines[:n]
}
