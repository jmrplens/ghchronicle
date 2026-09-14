//go:build dockere2e

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/test/e2e/racereport"
)

// The sweep the Elasticsearch, Graphite, Loki, OTLP, Telegraf and file
// assertions read back, and the second one the Prometheus exporter needs.
//
// One sweep feeds every push store at once, for the same reason the SQL side
// runs one: each store is then asked about the same data, and the file sink's
// JSON is the oracle every other store is compared against rather than a table
// copied by hand into a test. The fake GitHub, the fixtures and the build are
// the ones sweep_sql_stores_test.go already set up, so there is one fake in
// this package and not three.

// The dates the fixtures pin are declared in influxdb_test.go, alongside the
// InfluxDB assertions that named them first. Every store here is asked about
// the same three moments, which is the only way the answers are comparable.

// pushSweepFamilies is every family, on a cadence long enough that no second
// sweep starts while the assertions are reading. The first sweep collects all
// of them regardless: a fresh state file has nothing marked as done, and the
// exporter sweep additionally runs with Prime on.
const pushSweepEvery = "24h"

// pushSweep is one finished sweep and where it left its artifacts.
type pushSweep struct {
	// Dir holds the config, the state file and the artifacts, under the
	// package's git-ignored out/ directory rather than a temporary one, so a
	// failing assertion can be read against the bytes that caused it.
	Dir string
	// Points is the file sink's JSON, one point per line: the oracle.
	Points string
	// Log is everything the binary printed.
	Log string
	// Started and Finished bracket the run. A point carrying the date of the
	// thing that happened falls outside that window; one stamped when the
	// sweep looked falls inside it.
	Started  time.Time
	Finished time.Time
}

var (
	pushOnce   sync.Once
	pushShared *pushSweep
	errPush    error
)

// pushSweepRun runs the one sweep these suites read back, once per test
// binary: Elasticsearch, Graphite, Loki, OTLP, Telegraf and the file sink,
// from the same fixtures, in the same pass.
func pushSweepRun(ctx context.Context, tb testing.TB, s *Stack) *pushSweep {
	tb.Helper()
	pushOnce.Do(func() {
		pushShared, errPush = pushSweepInto(ctx, tb, s, "push")
	})
	if errPush != nil {
		tb.Fatalf("the sweep the assertions read back never finished: %v", errPush)
	}
	return pushShared
}

// pushSweepInto runs one sweep into a named working directory.
func pushSweepInto(ctx context.Context, tb testing.TB, s *Stack, name string) (*pushSweep, error) {
	tb.Helper()
	dir, err := pushWorkDir(name)
	if err != nil {
		return nil, err
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		return nil, err
	}
	sweep := &pushSweep{Dir: dir, Points: filepath.Join(dir, "points.jsonl")}
	gh := newSQLStoresGitHub(tb)
	cfg, err := pushSweepConfig(sweep, s, gh.URL())
	if err != nil {
		return nil, err
	}
	sweep.Started = time.Now().UTC()
	sweep.Log, err = sqlStoresExec(ctx, tb, binary, cfg)
	sweep.Finished = time.Now().UTC()
	if writeErr := os.WriteFile(filepath.Join(dir, "sweep.log"), []byte(sweep.Log), 0o600); writeErr != nil {
		return sweep, writeErr
	}
	if err != nil {
		return sweep, fmt.Errorf("%w\n%s", err, sweep.Log)
	}
	return sweep, nil
}

// pushWorkDir empties and returns one working directory. out/ is git-ignored
// and belongs to one run.
func pushWorkDir(name string) (string, error) {
	dir, err := filepath.Abs(filepath.Join("out", "push-stores", name))
	if err != nil {
		return "", err
	}
	if removeErr := os.RemoveAll(dir); removeErr != nil {
		return "", removeErr
	}
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		return "", mkdirErr
	}
	return dir, nil
}

// pushSweepConfig writes the config for the one-shot sweep.
//
// Three settings are the subject rather than incidental. Deduplication is off,
// so every point is offered to every store on every sweep. The OTLP sink is in
// raw mode, because the reduced mode sends the current value stamped now and
// the question here is whether a data point's own date survives the collector.
// And the Loki horizon is opened wide, because the sink drops an entry older
// than it before Loki ever sees one, and this suite is asking what Loki does
// with an old entry, not what the sink does.
func pushSweepConfig(sweep *pushSweep, s *Stack, githubURL string) (string, error) {
	// Under every.families, the layer that names one family: the flat map this
	// used to write is refused by the loader.
	var every strings.Builder
	every.WriteString("  families:\n")
	for _, f := range sqlStoresFamilies {
		fmt.Fprintf(&every, "    %s: %s\n", f, pushSweepEvery)
	}
	body := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 30s
targets:
  user: %s
sinks:
  elasticsearch:
    url: %s
    prefix: %s
  graphite:
    addr: %s
  loki:
    url: %s/loki/api/v1/push
    max_age: 87600h
  otlp:
    endpoint: %s
    raw: true
  telegraf:
    url: %s
  file:
    path: %s
    format: json
  dedupe_file: "off"
every:
%sstate_file: %s
log:
  level: debug
`, githubURL, sqlStoresLogin, s.ElasticsearchURL, s.ElasticsearchPrefix, s.GraphiteAddr,
		s.LokiURL, s.OTLPEndpoint, s.TelegrafURL, sweep.Points, every.String(),
		filepath.Join(sweep.Dir, "state.json"))

	path := filepath.Join(sweep.Dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// pushPoints reads back what the sweep emitted, which is what every store is
// then compared against.
func pushPoints(tb testing.TB, sweep *pushSweep) []sqlStoresPoint {
	tb.Helper()
	raw, err := os.ReadFile(sweep.Points)
	if err != nil {
		tb.Fatalf("the sweep wrote no points file: %v", err)
	}
	var points []sqlStoresPoint
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p sqlStoresPoint
		if decodeErr := json.Unmarshal([]byte(line), &p); decodeErr != nil {
			tb.Fatalf("point %d is not JSON: %v", len(points)+1, decodeErr)
		}
		points = append(points, p)
	}
	if len(points) == 0 {
		tb.Fatal("the sweep emitted no points at all")
	}
	return points
}

// pushPointsByMeasurement groups the oracle, which is how nearly every
// assertion here reads it: one measurement at a time.
func pushPointsByMeasurement(tb testing.TB, sweep *pushSweep) map[string][]sqlStoresPoint {
	tb.Helper()
	out := map[string][]sqlStoresPoint{}
	for _, p := range pushPoints(tb, sweep) {
		out[p.Measurement] = append(out[p.Measurement], p)
	}
	return out
}

// pushPointAt finds the one point of a measurement stamped at want, and says
// which dates are there when none is: a sink that restamped the point with the
// sweep's clock shows up in that list.
func pushPointAt(tb testing.TB, points []sqlStoresPoint, measurement, want string, tags map[string]string) sqlStoresPoint {
	tb.Helper()
	var seen []string
	for _, p := range points {
		if p.Measurement != measurement || !hasTags(p.Tags, tags) {
			continue
		}
		if p.Time == want {
			return p
		}
		seen = append(seen, p.Time)
	}
	tb.Fatalf("the sweep emitted no %s at %s for %v; it emitted %v", measurement, want, tags, seen)
	return sqlStoresPoint{}
}

func hasTags(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ── The sweep that feeds the exporter ───────────────────────────────────────

// exporterSweep is a running collector: the Prometheus sink is an exporter
// inside the process, so unlike every other sink it has nothing to read back
// once the process has exited.
type exporterSweep struct {
	// URL is the exposition page, on the host.
	URL string
	// Port is what a scraper has to reach, which on a machine that firewalls
	// its docker bridge is the whole difficulty.
	Port int
	Dir  string
	// Log is everything the process printed up to the moment the first sweep
	// finished.
	Log string
}

// startExporterSweep runs the collector as it runs in production, waits for
// its first sweep to finish, and leaves it serving until the test ends.
//
// A one-shot run cannot be used here: cmd/ghchronicle deliberately does not
// build the exporter when -once is given, because a page that stops being
// served the moment the sweep ends serves nobody. So this is the long-running
// mode, with every family on a long cadence and Prime doing the first sweep,
// which is exactly the shape the exporter is meant for.
func startExporterSweep(t *testing.T) *exporterSweep {
	t.Helper()
	ctx := t.Context()
	dir, err := pushWorkDir("exporter")
	if err != nil {
		t.Fatalf("preparing the exporter sweep: %v", err)
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		t.Fatalf("building the collector: %v", err)
	}
	port, err := freePort(ctx)
	if err != nil {
		t.Fatalf("choosing a port for the exporter: %v", err)
	}
	run := &exporterSweep{
		URL:  fmt.Sprintf("http://127.0.0.1:%d/metrics", port),
		Port: port,
		Dir:  dir,
	}
	gh := newSQLStoresGitHub(t)
	cfg, err := exporterConfig(dir, gh.URL(), port)
	if err != nil {
		t.Fatalf("writing the exporter config: %v", err)
	}

	// Its own context, canceled by the cleanup below rather than by whatever
	// the calling test's context does, so the process is killed once and by
	// something that also waits for it.
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(runCtx, binary, "-config", cfg)
	cmd.Env = collectorEnviron()
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if startErr := cmd.Start(); startErr != nil {
		stop()
		t.Fatalf("starting the collector: %v", startErr)
	}
	// The cleanup is also where a race report is looked for, once the process
	// is gone and has printed everything it will: the exporter serving while a
	// sweep writes is where a race lands, and it can land after the scrape the
	// test asserted on.
	t.Cleanup(func() {
		stop()
		_ = cmd.Wait()
		if writeErr := os.WriteFile(filepath.Join(dir, "sweep.log"), []byte(out.String()), 0o600); writeErr != nil {
			t.Errorf("keeping the exporter's log: %v", writeErr)
		}
		racereport.Check(t, out.String())
	})

	// "sweep finished" is the runner's own last line of a pass. Waiting for it
	// rather than for the page to answer matters: the exporter starts serving
	// before it has been written to, so an early scrape returns an empty page
	// that looks like a sink that wrote nothing.
	err = WaitUntil(ctx, "the exporter's first sweep", 5*time.Minute, func(context.Context) error {
		if strings.Contains(out.String(), "sweep finished") {
			return nil
		}
		if cmd.ProcessState != nil {
			return fmt.Errorf("the collector exited: %s", tail(out.String()))
		}
		return errors.New("still sweeping")
	})
	run.Log = out.String()
	if err != nil {
		t.Fatalf("%v\n%s", err, tail(run.Log))
	}
	return run
}

// exporterConfig configures the exporter and the file sink, and nothing else:
// what the other stores hold is the one-shot sweep's business, and a second
// writer into them would only make those assertions harder to read.
func exporterConfig(dir, githubURL string, port int) (string, error) {
	// Under every.families, the layer that names one family: the flat map this
	// used to write is refused by the loader.
	var every strings.Builder
	every.WriteString("  families:\n")
	for _, f := range sqlStoresFamilies {
		fmt.Fprintf(&every, "    %s: %s\n", f, pushSweepEvery)
	}
	body := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 30s
targets:
  user: %s
sinks:
  prometheus:
    listen: 0.0.0.0:%d
  file:
    path: %s
    format: json
  dedupe_file: "off"
every:
%sstate_file: %s
log:
  level: debug
`, githubURL, sqlStoresLogin, port, filepath.Join(dir, "points.jsonl"),
		every.String(), filepath.Join(dir, "state.json"))

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// freePort asks the kernel for a port and hands it back. There is a race
// between closing this listener and the exporter binding the same port, and it
// is the one every test harness that starts a real process runs: the exporter
// itself refuses to start on a busy port, which is a clear failure rather than
// a silent one.
// scrapePortLow and scrapePortHigh bound the range the exporter listens on.
//
// Prometheus runs in a container and reaches the exporter through the bridge
// gateway, so the exporter cannot bind loopback and the host has to accept the
// connection. On a machine whose firewall denies input by default that means a
// rule, and a rule needs a port that does not change every run. This range is
// the one the firewall opens to the container address space and nothing else,
// which is a hundred ports narrower than opening the host to its own bridges.
const (
	scrapePortLow  = 49300
	scrapePortHigh = 49399
)

// freePort returns a port in the scrape range that nothing is listening on.
//
// It binds where the exporter will bind, on every interface rather than on
// loopback, so a port already taken by something that answers only the outside
// world is not offered as free.
func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	for port := scrapePortLow; port <= scrapePortHigh; port++ {
		ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			continue
		}
		if closeErr := ln.Close(); closeErr != nil {
			return 0, closeErr
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free port between %d and %d for the exporter",
		scrapePortLow, scrapePortHigh)
}

// syncBuffer collects a running process's output while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ── Reading the stores back ─────────────────────────────────────────────────

// storeJSON sends one request to a store's query API and decodes the answer.
// Every store here is asked over HTTP and answers JSON, so they share this.
func storeJSON(ctx context.Context, method, url string, body []byte, into any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s: %s", method, url, res.Status, bytes.TrimSpace(raw))
	}
	if into == nil {
		return nil
	}
	if decodeErr := json.Unmarshal(raw, into); decodeErr != nil {
		return fmt.Errorf("%s %s answered something that does not decode: %w\n%s", method, url, decodeErr, raw)
	}
	return nil
}

// storeText fetches a page as text, for the one endpoint here that is not
// JSON: the exporter's own exposition format.
func storeText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return string(raw), nil
}
