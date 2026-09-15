package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/jmrplens/ghchronicle/cmd/internal/dashboards"
	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// askedFor is every metric name the Prometheus dashboard asks for, sorted.
func askedFor(t *testing.T) []string {
	t.Helper()
	store, ok := dashboards.ByName("prometheus")
	if !ok {
		t.Fatal("there is no prometheus dashboard")
	}
	seen := map[string]bool{}
	for _, p := range grafana.Walk(store.Build(nil)["panels"]) {
		expr, _ := p.Target["expr"].(string)
		for _, n := range uniqueNames(expr) {
			seen[n] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatal("the dashboard asks for no metric at all")
	}
	return names
}

// writeDump writes a /metrics dump declaring names the way the exporter does:
// a HELP and a TYPE line, then a sample carrying labels.
func writeDump(t *testing.T, names []string) string {
	t.Helper()
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "# HELP %s from ghchronicle\n# TYPE %s gauge\n%s{repo=\"o/r\"} 1\n", n, n, n)
	}
	path := filepath.Join(t.TempDir(), "metrics.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkRun runs the command and returns its status and both streams.
func checkRun(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	status = run(t.Context(), args, &out, &errOut)
	return status, out.String(), errOut.String()
}

// TestNamesOnlyPassesACompleteDump checks every name without a datasource,
// which asks Grafana nothing.
func TestNamesOnlyPassesACompleteDump(t *testing.T) {
	t.Setenv("GRAFANA_URL", "http://127.0.0.1:1")
	status, stdout, stderr := checkRun(t, writeDump(t, askedFor(t)))
	if status != 0 || stderr != "" || stdout != "\n0 unknown metric names, 0 rejected expressions\n" {
		t.Errorf("run = %d, %q, %q, want a clean run and the count", status, stdout, stderr)
	}
}

// TestNamesOnlyReportsAMissingMetric names each panel that asks for a metric
// the exporter does not declare, and fails the run.
func TestNamesOnlyReportsAMissingMetric(t *testing.T) {
	names := askedFor(t)
	gone := names[0]
	status, stdout, _ := checkRun(t, writeDump(t, names[1:]))
	if status != 1 {
		t.Errorf("status %d, want a missing metric to fail the run", status)
	}
	if !strings.Contains(stdout, ": "+gone+" is not exported\n") {
		t.Errorf("stdout does not name %s:\n%s", gone, stdout)
	}
	for line := range strings.Lines(stdout) {
		if strings.HasPrefix(line, "MISSING ") && !strings.Contains(line, gone) {
			t.Errorf("reported %q, want only %s missing", line, gone)
		}
	}
}

// promStandIn is a Grafana that hands each expression to answer and keeps
// every query it was posted.
type promStandIn struct {
	mu      sync.Mutex
	queries []map[string]any
	ranges  []string
}

// servePrometheus starts the stand-in, answering with Grafana's own shape:
// the error on the query's refId when answer returns one.
func servePrometheus(t *testing.T, answer func(expr string) string) *promStandIn {
	t.Helper()
	s := &promStandIn{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
			return
		}
		var body struct {
			From, To string
			Queries  []map[string]any
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the request is not JSON: %v", err)
			return
		}
		results := map[string]any{}
		for _, q := range body.Queries {
			expr, _ := q["expr"].(string)
			ref, _ := q["refId"].(string)
			result := map[string]any{"frames": []any{}}
			if reason := answer(expr); reason != "" {
				result["error"] = reason
			}
			results[ref] = result
			s.mu.Lock()
			s.queries = append(s.queries, q)
			s.ranges = append(s.ranges, body.From+" "+body.To)
			s.mu.Unlock()
		}
		out, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			t.Errorf("encoding the answer: %v", err)
			return
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GRAFANA_URL", srv.URL)
	t.Setenv("GRAFANA_TOKEN", "glsa_test")
	return s
}

// posted is a copy of every query the stand-in was sent, with the range each
// was asked over.
func (s *promStandIn) posted() (queries []map[string]any, ranges []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries), slices.Clone(s.ranges)
}

// TestSyntaxPostsEveryExpression hands each panel's expression to Grafana with
// the repository matcher widened, and reports the ones Prometheus refused.
func TestSyntaxPostsEveryExpression(t *testing.T) {
	names := askedFor(t)
	refusedName := names[len(names)-1]
	s := servePrometheus(t, func(expr string) string {
		if strings.Contains(expr, refusedName) {
			return "1:7: parse error: unexpected identifier"
		}
		return ""
	})
	status, stdout, stderr := checkRun(t, writeDump(t, names), "prom-uid")
	if status != 1 || stderr != "" {
		t.Errorf("status %d, stderr %q, want a refused expression to fail the run", status, stderr)
	}
	if !strings.Contains(stdout, "parse error: unexpected identifier") ||
		!strings.Contains(stdout, " rejected expressions\n") || strings.Contains(stdout, " 0 rejected") {
		t.Errorf("stdout does not report the refusal and count it:\n%s", stdout)
	}
	queries, ranges := s.posted()
	if len(queries) == 0 {
		t.Fatal("no expression was posted")
	}
	for i, q := range queries {
		expr, _ := q["expr"].(string)
		if strings.Contains(expr, "$repo") {
			t.Errorf("posted %q with the variable left in it", expr)
		}
		ds, _ := q["datasource"].(map[string]any)
		if ds["type"] != "prometheus" || ds["uid"] != "prom-uid" {
			t.Errorf("posted to %v, want the Prometheus datasource given", q["datasource"])
		}
		if q["intervalMs"] != float64(300000) || q["maxDataPoints"] != float64(200) || ranges[i] != "now-6h now" {
			t.Errorf("posted %v over %q, want the pacing and range of a six hour graph", q, ranges[i])
		}
	}
	if !slices.ContainsFunc(queries, func(q map[string]any) bool {
		expr, _ := q["expr"].(string)
		return strings.Contains(expr, `repo=~".*"`)
	}) {
		t.Error("no posted expression carries the widened repository matcher")
	}
}

// TestSyntaxReportsAGrafanaItCannotReach counts every expression as rejected
// when the request itself fails, rather than calling silence an acceptance.
func TestSyntaxReportsAGrafanaItCannotReach(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	t.Setenv("GRAFANA_URL", srv.URL)
	t.Setenv("GRAFANA_TOKEN", "glsa_test")
	status, stdout, _ := checkRun(t, writeDump(t, askedFor(t)), "prom-uid")
	if want := refusedText(t, srv.Listener.Addr().String()); status != 1 || !strings.Contains(stdout, want) {
		t.Errorf("status %d, stdout %q, want each expression failed with the connection error, %q", status, stdout, want)
	}
}

// refusedText is how this platform words a refused connection, which the
// command passes on for each expression: "connection refused" on Unix, and on
// Windows Winsock's own sentence for WSAECONNREFUSED. It is read off a dial to
// the same closed address rather than written out in Linux's words.
func refusedText(t *testing.T, addr string) string {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("%s accepted a connection, want it refused", addr)
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		t.Fatalf("dial %s = %v, want the system's refusal", addr, err)
	}
	return errno.Error()
}

// notFoundText is how this platform words the failure to open path, which the
// command passes on as it is: "no such file or directory" on Unix and the
// system's own sentence on Windows. It is read off the same failure rather
// than written out in Linux's words.
func notFoundText(t *testing.T, path string) string {
	t.Helper()
	_, err := os.Stat(path)
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("stat %s = %v, want it missing", path, err)
	}
	return pathErr.Err.Error()
}

// TestRunRefusesWhatItCannotStart covers the usage, a missing dump, a dump
// with a line too long to scan, and a datasource without a token.
func TestRunRefusesWhatItCannotStart(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.txt")
	long := filepath.Join(t.TempDir(), "long.txt")
	if err := os.WriteFile(long, []byte("# TYPE "+strings.Repeat("x", 2<<20)+" gauge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		args           []string
		token          string
		status         int
		stdout, stderr string
	}{
		{"the usage asked for", []string{"-h"}, "t", 0, usage + "\n", ""},
		{"the usage asked for with --help", []string{"--help"}, "t", 0, usage + "\n", ""},
		{"no dump", nil, "t", 1, "", usage + "\n"},
		{"a dump that is not there", []string{absent}, "t", 1, "", notFoundText(t, absent)},
		{"a line too long to scan", []string{long}, "t", 1, "", "token too long"},
		{
			"a datasource without a token",
			[]string{writeDump(t, nil), "uid"},
			"", 1, "",
			"GRAFANA_TOKEN is not set, so no expression can be posted\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GRAFANA_URL", "http://127.0.0.1:1")
			t.Setenv("GRAFANA_TOKEN", tc.token)
			status, stdout, stderr := checkRun(t, tc.args...)
			if status != tc.status || stdout != tc.stdout || !strings.Contains(stderr, tc.stderr) {
				t.Errorf("run = %d, %q, %q, want %d, %q and %q in stderr",
					status, stdout, stderr, tc.status, tc.stdout, tc.stderr)
			}
		})
	}
}

// TestExportedNamesReadsOnlyTypeLines takes a name from its TYPE line and
// never from a sample or a HELP line, which carry names that are not declared.
// A TYPE line cut off right after the keyword declares nothing and is passed
// over rather than read past its end.
func TestExportedNamesReadsOnlyTypeLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.txt")
	dump := "# HELP github_help_only text\n# TYPE github_declared counter\n" +
		"github_sample_only{repo=\"x\"} 3\n# TYPE\n# TYPE \n"
	if err := os.WriteFile(path, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := exportedNames(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["github_declared"] {
		t.Errorf("exportedNames = %v, want only the declared name", got)
	}
}

// TestUniqueNamesKeepsTheFirstMention reports a repeated name once, in the
// order the expression mentions it.
func TestUniqueNamesKeepsTheFirstMention(t *testing.T) {
	t.Parallel()
	got := uniqueNames(`sum(github_b{repo=~"$repo"}) / sum(github_a) + github_b`)
	if !slices.Equal(got, []string{"github_b", "github_a"}) {
		t.Errorf("uniqueNames = %v, want each name once in order", got)
	}
}

// TestExportedNamesReadsPastALongSample keeps reading after a sample line
// longer than the scanner's first buffer. A metric with many label sets can
// write one, and the dump is allowed up to a mebibyte a line so that such a
// line does not end the read with every name declared after it unseen.
func TestExportedNamesReadsPastALongSample(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.txt")
	dump := "github_wide{repo=\"" + strings.Repeat("x", 200<<10) + "\"} 1\n# TYPE github_after gauge\n"
	if err := os.WriteFile(path, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := exportedNames(path)
	if err != nil || len(got) != 1 || !got["github_after"] {
		t.Errorf("exportedNames = %v, %v, want the name declared after the long line", got, err)
	}
}
