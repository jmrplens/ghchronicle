package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/test/e2e/racereport"
)

// This file is the shared machinery for the per-sink end-to-end tests: a
// config writer that takes an arbitrary `sinks:` block, ways to run the real
// binary once or in the background, receivers that stand in for each backend,
// and parsers for the wire formats.
//
// The parsers exist because a substring assertion passes for the wrong
// reasons. "the body contains gh_traffic" is true of a body that also contains
// a broken timestamp, an unescaped tag or a field that was silently dropped.

// writeSinkConfig writes a config whose `sinks:` block is exactly what the
// caller passes, already indented by two spaces. Every family runs at a one
// minute cadence, so a fresh state file makes the first sweep collect all of
// them, joblogs and history included.
func writeSinkConfig(t *testing.T, dir, base, sinks string) string {
	t.Helper()
	var every strings.Builder
	for _, f := range families {
		fmt.Fprintf(&every, "    %s: 1m\n", f)
	}
	cfg := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 20s
targets:
  user: %s
sinks:
%s
every:
  families:
%s
state_file: %s
log:
  level: debug
`, base, login, sinks, every.String(), filepath.Join(dir, "state.json"))
	return writeConfigFile(t, dir, cfg)
}

// sweepOnce runs one sweep and fails the test if the binary does, or if any
// sink reported a write failure. A sink that cannot reach its receiver still
// exits zero, so the log has to be read as well as the exit code.
func sweepOnce(t *testing.T, cfg string) string {
	t.Helper()
	out, err := run(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("ghchronicle -once failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("sink write failed")) {
		t.Errorf("a sink failed to write:\n%s", out)
	}
	return string(out)
}

// collectorEnviron is the environment every run against the fake GitHub gets.
// The fake is on localhost, so no proxy from the environment may get in the
// way, and no real token may leak in. Under -race it also carries the
// detector's settings (harness_race_test.go), last so that they win over a
// GORACE of the caller's shell: one naming a log_path would send the report
// to a file that racereport.Check never reads.
func collectorEnviron() []string {
	return slices.Concat(os.Environ(),
		[]string{"HTTPS_PROXY=", "HTTP_PROXY=", "NO_PROXY=*", "GITHUB_TOKEN="},
		raceEnviron())
}

// runSplit runs the binary and keeps standard output apart from the log, which
// goes to standard error. The stdout sink is the only thing that writes to the
// first, so the two have to be separable to assert on it. The race runtime
// writes to standard error too, so that is where a report is looked for.
func runSplit(t *testing.T, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = collectorEnviron()
	var outBuf, logBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &logBuf
	err = cmd.Run()
	racereport.Check(t, logBuf.String())
	return outBuf.String(), logBuf.String(), err
}

// syncBuffer collects the output of a process that is still running, which the
// test goroutine reads while the process writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// bgProcess is the binary running in serve mode. Standard output is kept
// apart from the log because the stdout sink writes to the first.
type bgProcess struct {
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan error
	gone   atomic.Bool
	once   sync.Once
}

// serveInBackground starts the binary without -once, which is the only mode
// that runs the Prometheus exporter and the push sinks' repeat loops. It is
// stopped by the test's cleanup, before the fake GitHub is torn down.
//
// The cleanup is also where a race report is looked for, once the process is
// gone and has printed everything it will. A test that called Stop itself has
// usually made its assertions by then, and the exporter serving while a sweep
// writes is exactly where a race lands after the last of them.
func serveInBackground(t *testing.T, cfg string) *bgProcess {
	t.Helper()
	// The process outlives the call, and Stop is what ends it, so the context
	// here is the background one rather than the test's.
	cmd := exec.CommandContext(context.Background(), binary, "-config", cfg)
	cmd.Env = collectorEnviron()
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	prepareForTermination(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the binary: %v", err)
	}
	p := &bgProcess{cmd: cmd, stdout: stdout, stderr: stderr, done: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		p.gone.Store(true)
		p.done <- err
	}()
	t.Cleanup(func() {
		p.Stop()
		racereport.Check(t, p.stderr.String())
	})
	return p
}

// Output is everything the process has printed, for a failure message.
func (p *bgProcess) Output() string { return p.stderr.String() + p.stdout.String() }

// Stdout is what the stdout sink has printed, without the log.
func (p *bgProcess) Stdout() string { return p.stdout.String() }

// Exited reports that the process is already gone, so a wait can stop there
// rather than burn its whole timeout on a binary that refused its config.
func (p *bgProcess) Exited() bool { return p.gone.Load() }

// awaitSweep waits for the first sweep to finish.
func awaitSweep(t *testing.T, p *bgProcess, timeout time.Duration) {
	t.Helper()
	waitFor(timeout, func() bool {
		return strings.Contains(p.Output(), "sweep finished") || p.Exited()
	})
	if !strings.Contains(p.Output(), "sweep finished") {
		t.Fatalf("the first sweep never finished:\n%s", p.Output())
	}
}

// Stop asks for a clean shutdown, so the sinks are closed and their buffers
// flushed, and kills the process if it does not go. The request is whatever
// the platform delivers as os.Interrupt (see process_*_test.go), and its error
// is not the answer: a process that has already gone cannot be asked, and one
// the request never reached is killed after the wait either way.
func (p *bgProcess) Stop() string {
	p.once.Do(func() {
		_ = signalTermination(p.cmd.Process)
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p.Output()
}

// waitFor polls until cond holds. It returns false on timeout so the caller
// can report what the process had printed by then.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freeAddr reserves a port by binding it and letting it go, for the exporter,
// which is configured by address rather than handed a listener.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return addr
}

// capturedRequest is one request a fake receiver answered.
type capturedRequest struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
	// Accepted records whether the receiver answered with a success status,
	// which is what tells a rejected batch from a written one.
	Accepted bool
}

// capture is an httptest server that records everything it is sent.
type capture struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []capturedRequest
}

// newCapture answers every request with 204, or with whatever reply returns.
// A reply of (0, "") means the default.
func newCapture(t *testing.T, reply func(body []byte) (int, string)) *capture {
	t.Helper()
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(r)
		status, payload := http.StatusNoContent, ""
		if reply != nil {
			if s, p := reply(body); s != 0 {
				status, payload = s, p
			}
		}
		c.mu.Lock()
		c.got = append(c.got, capturedRequest{
			Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: body, Accepted: status < 300,
		})
		c.mu.Unlock()
		w.WriteHeader(status)
		if payload != "" {
			_, _ = w.Write([]byte(payload))
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func readAll(r *http.Request) []byte {
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	return b.Bytes()
}

func (c *capture) URL() string { return c.srv.URL }

// Requests returns a copy of what has arrived so far.
func (c *capture) Requests() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.got...)
}

// Accepted returns only the requests the receiver did not refuse.
func (c *capture) Accepted() []capturedRequest {
	out := []capturedRequest{}
	for _, r := range c.Requests() {
		if r.Accepted {
			out = append(out, r)
		}
	}
	return out
}

// Count is how many requests have arrived, for a waitFor condition.
func (c *capture) Count() int { return len(c.Requests()) }

// Body is every accepted body joined, for the parsers.
func (c *capture) Body() string {
	var b strings.Builder
	for _, r := range c.Accepted() {
		b.Write(r.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// graphiteServer is a real TCP listener speaking Graphite's plaintext
// protocol, because that sink writes to a socket rather than to HTTP and a
// stub HTTP server would prove nothing about it.
type graphiteServer struct {
	ln    net.Listener
	mu    sync.Mutex
	lines []string
}

func newGraphiteServer(t *testing.T) *graphiteServer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &graphiteServer{ln: ln}
	go g.accept()
	t.Cleanup(func() { _ = ln.Close() })
	return g
}

func (g *graphiteServer) accept() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.read(conn)
	}
}

func (g *graphiteServer) read(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSpace(string(pending[:i]))
				pending = pending[i+1:]
				if line == "" {
					continue
				}
				g.mu.Lock()
				g.lines = append(g.lines, line)
				g.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

func (g *graphiteServer) Addr() string { return g.ln.Addr().String() }

func (g *graphiteServer) Lines() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.lines...)
}

// lpPoint is one parsed line of InfluxDB line protocol. Fields keep their wire
// type: an integer written as "12i" comes back as int64 and not as a float, so
// a sink that stopped typing its fields is caught.
type lpPoint struct {
	Measurement string
	Tags        map[string]string
	Fields      map[string]any
	Time        int64
}

func parseLineProtocol(t *testing.T, body string) []lpPoint {
	t.Helper()
	var out []lpPoint
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := parseLPLine(line)
		if err != nil {
			t.Fatalf("line protocol does not parse: %v\n%s", err, line)
		}
		out = append(out, p)
	}
	return out
}

func parseLPLine(line string) (lpPoint, error) {
	head, rest, ok := cutOutside(line, ' ', false)
	if !ok {
		return lpPoint{}, errors.New("no space between the tag set and the fields")
	}
	fieldPart, stampPart, ok := cutOutside(rest, ' ', true)
	if !ok {
		return lpPoint{}, errors.New("no timestamp")
	}
	parts := splitOutside(head, ',', false)
	measurement := unescapeLP(parts[0])
	if measurement == "" {
		return lpPoint{}, errors.New("empty measurement")
	}
	tags, err := parseLPTags(parts[1:])
	if err != nil {
		return lpPoint{}, err
	}
	fields, err := parseLPFields(fieldPart)
	if err != nil {
		return lpPoint{}, err
	}
	stamp, err := strconv.ParseInt(strings.TrimSpace(stampPart), 10, 64)
	if err != nil {
		return lpPoint{}, fmt.Errorf("timestamp %q: %w", stampPart, err)
	}
	return lpPoint{Measurement: measurement, Tags: tags, Fields: fields, Time: stamp}, nil
}

// parseLPTags reads the key=value pairs that follow the measurement.
func parseLPTags(pairs []string) (map[string]string, error) {
	tags := make(map[string]string, len(pairs))
	for _, tag := range pairs {
		k, v, ok := cutOutside(tag, '=', false)
		if !ok {
			return nil, fmt.Errorf("tag %q is not key=value", tag)
		}
		tags[unescapeLP(k)] = unescapeLP(v)
	}
	return tags, nil
}

// parseLPFields reads the field set, keeping each value's wire type. A line
// with no field at all is refused, as InfluxDB refuses it.
func parseLPFields(fieldPart string) (map[string]any, error) {
	fields := map[string]any{}
	for _, f := range splitOutside(fieldPart, ',', true) {
		k, raw, ok := cutOutside(f, '=', true)
		if !ok {
			return nil, fmt.Errorf("field %q is not key=value", f)
		}
		v, err := parseLPValue(raw)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		fields[unescapeLP(k)] = v
	}
	if len(fields) == 0 {
		return nil, errors.New("no fields")
	}
	return fields, nil
}

func parseLPValue(raw string) (any, error) {
	switch {
	case raw == "":
		return nil, errors.New("empty value")
	case raw[0] == '"':
		if len(raw) < 2 || raw[len(raw)-1] != '"' {
			return nil, fmt.Errorf("unterminated string %q", raw)
		}
		return unquoteLP(raw[1 : len(raw)-1]), nil
	case raw == "true" || raw == "false":
		return raw == "true", nil
	case raw[len(raw)-1] == 'i':
		n, err := strconv.ParseInt(raw[:len(raw)-1], 10, 64)
		return n, err
	}
	return strconv.ParseFloat(raw, 64)
}

// cutOutside cuts at the first sep that is neither escaped with a backslash
// nor, when quoted is set, inside a double quoted string.
func cutOutside(s string, sep byte, quoted bool) (before, after string, found bool) {
	inQuotes := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			i++
		case quoted && s[i] == '"':
			inQuotes = !inQuotes
		case s[i] == sep && !inQuotes:
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func splitOutside(s string, sep byte, quoted bool) []string {
	var out []string
	for {
		head, tail, ok := cutOutside(s, sep, quoted)
		out = append(out, head)
		if !ok {
			return out
		}
		s = tail
	}
}

func unescapeLP(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unquoteLP(s string) string {
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`)
	return r.Replace(s)
}

// promSample is one line of the Prometheus exposition format.
type promSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// parseExposition parses the text format and returns the samples plus the
// metrics that were given a TYPE. A malformed line fails the test: a scrape
// that Prometheus would refuse is not a working exporter.
func parseExposition(t *testing.T, body string) (samples []promSample, types map[string]string) {
	t.Helper()
	types = map[string]string{}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(line)
			if len(f) == 4 && f[1] == "TYPE" {
				types[f[2]] = f[3]
			}
			continue
		}
		s, err := parsePromLine(line)
		if err != nil {
			t.Fatalf("exposition line does not parse: %v\n%s", err, line)
		}
		samples = append(samples, s)
	}
	return samples, types
}

func parsePromLine(line string) (promSample, error) {
	s := promSample{Labels: map[string]string{}}
	var name string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return s, errors.New("unbalanced braces")
		}
		name = line[:i]
		labels, err := parsePromLabels(line[i+1 : j])
		if err != nil {
			return s, err
		}
		s.Labels = labels
		line = strings.TrimSpace(line[j+1:])
	} else {
		f := strings.Fields(line)
		if len(f) < 2 {
			return s, errors.New("no value")
		}
		name, line = f[0], f[1]
	}
	s.Name = strings.TrimSpace(name)
	if !validMetricName(s.Name) {
		return s, fmt.Errorf("%q is not a valid metric name", s.Name)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil {
		return s, fmt.Errorf("value %q: %w", line, err)
	}
	s.Value = v
	return s, nil
}

func parsePromLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("label %q is not key=value", s)
		}
		key := strings.TrimSpace(s[:eq])
		rest := s[eq+1:]
		if rest == "" || rest[0] != '"' {
			return nil, fmt.Errorf("label %s is not quoted", key)
		}
		var val strings.Builder
		i := 1
		for ; i < len(rest); i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
				switch rest[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(rest[i])
				}
				continue
			}
			if rest[i] == '"' {
				break
			}
			val.WriteByte(rest[i])
		}
		if i >= len(rest) {
			return nil, fmt.Errorf("label %s is unterminated", key)
		}
		out[key] = val.String()
		s = strings.TrimPrefix(strings.TrimSpace(rest[i+1:]), ",")
	}
	return out, nil
}

func validMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// splitLogfmt divides a Loki line into its sentence and its trailing logfmt
// pairs. The pairs are the longest suffix of space separated tokens that all
// read as key=value, which is how the line is meant to be read: prose first,
// structure after it.
func splitLogfmt(line string) (message string, pairs map[string]string, count int) {
	tokens := tokenizeOutsideQuotes(line)
	first := len(tokens)
	for i, tok := range slices.Backward(tokens) {
		if !isLogfmtPair(tok) {
			break
		}
		first = i
	}
	pairs = map[string]string{}
	for _, tok := range tokens[first:] {
		k, v, _ := strings.Cut(tok, "=")
		pairs[k] = strings.Trim(v, `"`)
	}
	return strings.Join(tokens[:first], " "), pairs, len(tokens) - first
}

func tokenizeOutsideQuotes(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			cur.WriteByte(s[i])
			i++
			cur.WriteByte(s[i])
		case s[i] == '"':
			inQuotes = !inQuotes
			cur.WriteByte(s[i])
		case s[i] == ' ' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	out = append(out, cur.String())
	return out
}

func isLogfmtPair(tok string) bool {
	k, v, ok := strings.Cut(tok, "=")
	if !ok || k == "" || v == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// assertNanosecondStamps checks the precision the line protocol was written
// at. A nanosecond stamp for a date in this decade is eighteen digits or more;
// a sink that had switched to seconds or milliseconds would still parse.
func assertNanosecondStamps(t *testing.T, points []lpPoint) {
	t.Helper()
	for _, p := range points {
		if p.Time < 1e18 {
			t.Errorf("%s is stamped %d, which is not nanoseconds", p.Measurement, p.Time)
			break
		}
	}
}

// measurementsOf counts the parsed points by measurement.
func measurementsOf(points []lpPoint) map[string]int {
	out := map[string]int{}
	for _, p := range points {
		out[p.Measurement]++
	}
	return out
}

// sortedNames is a stable list for a failure message.
func sortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
