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

// buildSinks names every sink here. A sink that stops being wired up, or whose
// YAML key is spelled differently in the config than in the builder, still
// passes its own unit tests and every single-sink test that does not mention
// it. This is the one that notices.
var everySinkName = []string{
	"influxdb", "otlp", "loki", "file", "prometheus",
	"telegraf", "graphite", "sql", "elasticsearch", "stdout",
}

func TestEverySinkReceivesOneSweep(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	influx := newCapture(t, nil)
	otlp := newCapture(t, nil)
	loki := newCapture(t, nil)
	telegraf := newCapture(t, nil)
	elastic := newCapture(t, bulkReply)
	graphite := newGraphiteServer(t)
	promAddr := freeAddr(t)

	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "points.jsonl")
	sqlPath := filepath.Join(dir, "points.sql")
	cfg := writeSinkConfig(t, dir, gh.URL(), fmt.Sprintf(`  influxdb:
    url: %s
    token: influx-token
    org: acme
    bucket: github
  prometheus:
    listen: %s
    path: /metrics
  otlp:
    endpoint: %s/v1/metrics
    service: ghchronicle-e2e
  loki:
    url: %s/loki/api/v1/push
    max_age: 87600h
  file:
    path: %s
    format: json
  stdout: true
  telegraf:
    url: %s
  graphite:
    addr: %s
  sql:
    dialect: postgres
    path: %s
  elasticsearch:
    url: %s`,
		influx.URL(), promAddr, otlp.URL(), loki.URL(), jsonPath,
		telegraf.URL(), graphite.Addr(), sqlPath, elastic.URL()))

	// Serve mode, because the exporter is the one sink a one-shot run skips.
	// Serve still runs exactly one sweep before its first tick, a minute away.
	proc := serveInBackground(t, cfg)
	awaitSweep(t, proc, 90*time.Second)
	var exposition string
	if !waitFor(30*time.Second, func() bool {
		b, up := scrape(t.Context(), promAddr, "/metrics")
		if !up || !strings.Contains(b, "github_") {
			return false
		}
		exposition = b
		return true
	}) {
		t.Fatalf("the exporter served nothing:\n%s", proc.Output())
	}
	out := proc.Stop()
	if strings.Contains(out, "sink write failed") {
		t.Errorf("a sink failed during the sweep:\n%s", out)
	}

	// Every sink reported at least one written batch, which is the builder's
	// half of the wiring.
	for _, name := range everySinkName {
		if !strings.Contains(out, "sink="+name+" ") {
			t.Errorf("no batch was written to the %s sink; the log says:\n%s", name, out)
		}
	}

	// And every receiver holds what it was sent, which is the sink's half.
	if got := measurementsOf(parseLineProtocol(t, influx.Body()))["gh_traffic"]; got == 0 {
		t.Errorf("the InfluxDB receiver got no traffic points")
	}
	if got := measurementsOf(parseLineProtocol(t, telegraf.Body()))["gh_traffic"]; got == 0 {
		t.Errorf("the Telegraf receiver got no traffic points")
	}
	if !strings.Contains(otlp.Body(), `"github.repo.stars"`) {
		t.Errorf("the OTLP receiver got no repository gauges")
	}
	if lokiEntryCount(t, loki) == 0 {
		t.Errorf("the Loki receiver got no entries")
	}
	if len(parseBulk(t, elastic.Body())) == 0 {
		t.Errorf("the Elasticsearch receiver got no documents")
	}
	if !waitFor(30*time.Second, func() bool { return len(graphite.Lines()) > 100 }) {
		t.Errorf("the Graphite listener got %d lines", len(graphite.Lines()))
	}
	if samples, _ := parseExposition(t, exposition); len(samples) < 20 {
		t.Errorf("the exporter served %d samples", len(samples))
	}
	if points := readPoints(t, jsonPath); len(points) < 100 {
		t.Errorf("the file sink wrote %d points", len(points))
	}
	sqlRaw, err := os.ReadFile(sqlPath)
	if err != nil || !strings.Contains(string(sqlRaw), "CREATE TABLE IF NOT EXISTS ") {
		t.Errorf("the SQL sink wrote no schema: %v", err)
	}
	assertStdoutPrintedLineProtocol(t, proc.Stdout())
}

// assertStdoutPrintedLineProtocol checks what the stdout sink printed: a
// sweep's worth of lines, in line protocol, which is its default, rather than
// the file sink's JSON.
func assertStdoutPrintedLineProtocol(t *testing.T, stdout string) {
	t.Helper()
	printed := 0
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) != "" {
			printed++
		}
	}
	if printed < 100 {
		t.Errorf("the stdout sink printed %d lines", printed)
	}
	if json.Valid([]byte(strings.SplitN(strings.TrimSpace(stdout), "\n", 2)[0])) {
		t.Errorf("stdout printed JSON without stdout_format being set to it")
	}
}
