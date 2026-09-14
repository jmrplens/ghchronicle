//go:build dockere2e

package docker

import (
	"fmt"
	"os"
	"testing"
)

// TestMain tears the stack down after the last test, on every exit path
// including a failing one. It leaves alone a stack that was already running
// when this binary started, because that one belongs to whoever started it.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := Shutdown(); err != nil {
		fmt.Fprintf(os.Stderr, "docker stack teardown: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// TestStackReady is the harness's own test: every store answered, and here is
// where each of them is. It writes nothing to any of them, which is the next
// suites' work; what it proves is that a test can reach all nine.
func TestStackReady(t *testing.T) {
	s := Start(t)

	for _, line := range []struct{ what, where string }{
		{"influxdb", s.InfluxURL},
		{"postgres", s.PostgresHost + ":" + s.PostgresPort},
		{"elasticsearch", s.ElasticsearchURL},
		{"graphite (carbon)", s.GraphiteAddr},
		{"graphite (render)", s.GraphiteURL},
		{"prometheus", s.PrometheusURL},
		{"loki", s.LokiURL},
		{"otlp", s.OTLPEndpoint},
		{"telegraf", s.TelegrafURL},
		{"grafana", s.GrafanaURL},
	} {
		t.Logf("%-18s %s", line.what, line.where)
	}
	if s.GrafanaToken == "" {
		t.Error("no Grafana service account token: /api/ds/query needs one")
	}
}
