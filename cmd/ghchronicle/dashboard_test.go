package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/internal/config"
)

// grafanaStub answers everything the publisher asks and keeps what it was
// sent, so a test can read the datasource and the dashboard that were written
// rather than only whether the call returned.
type grafanaStub struct {
	missing  bool // the datasource is not there yet
	unwell   bool // it is there and cannot reach its store
	writes   []map[string]any
	askedFor []string
}

func (g *grafanaStub) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.askedFor = append(g.askedFor, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/health"):
			if g.unwell {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"status":"ERROR","message":"dial tcp: no route to host"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"OK","message":"reached it"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/datasources/"):
			if g.missing {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"uid":"ghchronicle-influxdb","type":"influxdb"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `[]`)
		default:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.writes = append(g.writes, body)
			_, _ = io.WriteString(w, `{"status":"success","url":"/d/x/y","uid":"made"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// influxConfig is a collector that writes to InfluxDB and is told about a
// Grafana, which is the ordinary case.
func influxConfig(url string) *config.Config {
	return &config.Config{
		Sinks: config.Sinks{Influx: &config.InfluxSink{
			URL: "http://127.0.0.1:50107", Token: "sink-token", Bucket: "github",
		}},
		Grafana: &config.Grafana{URL: url, Token: "grafana-token"},
	}
}

// dashboardOf finds the dashboard among what was written.
func dashboardOf(t *testing.T, writes []map[string]any) map[string]any {
	t.Helper()
	for _, body := range writes {
		if doc, ok := body["dashboard"].(map[string]any); ok {
			return doc
		}
	}
	t.Fatalf("no dashboard was published; wrote %d thing(s)", len(writes))
	return nil
}

// TestItBuildsTheDatasourceOutOfTheSink, which is the whole proposition: the
// collector already knows where it writes and with which credential, so a
// reader should not have to say it twice.
func TestItBuildsTheDatasourceOutOfTheSink(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true}
	cfg := influxConfig(g.serve(t))
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	ds := g.writes[0]
	if ds["url"] != "http://127.0.0.1:50107" {
		t.Errorf("url = %v, want the address the sink writes to", ds["url"])
	}
	settings, _ := ds["jsonData"].(map[string]any)
	if settings["dbName"] != "github" || settings["version"] != "SQL" {
		t.Errorf("jsonData = %v, want the bucket and SQL", settings)
	}
	secret, _ := ds["secureJsonData"].(map[string]any)
	if secret["httpHeaderValue1"] != "Bearer sink-token" {
		t.Errorf("the sink's token did not reach the datasource: %v", secret)
	}
}

// TestTheAddressGrafanaUsesCanDifferFromTheOneTheSinkWritesTo. Measured on the
// deployment this was written for: the collector writes to a published port
// and Grafana reaches the same store by its name on a container network.
func TestTheAddressGrafanaUsesCanDifferFromTheOneTheSinkWritesTo(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true}
	cfg := influxConfig(g.serve(t))
	cfg.Grafana.Datasource.URL = "http://influxdb3-ghchronicle:8181"
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	if got := g.writes[0]["url"]; got != "http://influxdb3-ghchronicle:8181" {
		t.Errorf("url = %v, want the override rather than the sink's address", got)
	}
}

// TestItRefusesToPublishAgainstADatasourceThatDoesNotAnswer. Without this the
// run reports success and the reader finds a wall of empty panels, which is
// the failure the probe exists for.
func TestItRefusesToPublishAgainstADatasourceThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true, unwell: true}
	cfg := influxConfig(g.serve(t))
	var said strings.Builder
	err := publishDashboards(t.Context(), cfg, &said)
	if err == nil {
		t.Fatal("it published against a datasource that cannot reach its store")
	}
	if !strings.Contains(err.Error(), "no route to host") ||
		!strings.Contains(err.Error(), "grafana.datasource.url") {
		t.Errorf("err = %v, want what the probe said and the setting that fixes it", err)
	}
	for _, body := range g.writes {
		if _, isDashboard := body["dashboard"]; isDashboard {
			t.Error("it published the dashboard anyway")
		}
	}
}

// TestAnAdoptedDatasourceIsLeftAlone. Correcting one this did not make would
// overwrite settings nobody asked it to have.
func TestAnAdoptedDatasourceIsLeftAlone(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{}
	cfg := influxConfig(g.serve(t))
	cfg.Grafana.Datasource.UID = "somebody-elses"
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	for _, body := range g.writes {
		if _, isDashboard := body["dashboard"]; !isDashboard {
			t.Errorf("it wrote to the adopted datasource: %v", body)
		}
	}
	if !strings.Contains(said.String(), "adopted") {
		t.Errorf("output = %q, want it to say the datasource was adopted", said.String())
	}
}

// TestASinkThatCannotDescribeADatasourceSaysSo rather than inventing an
// address. Three of the five write to something that is not the thing Grafana
// queries.
func TestASinkThatCannotDescribeADatasourceSaysSo(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true}
	cfg := &config.Config{
		Sinks:   config.Sinks{Graphite: &config.GraphiteSink{Addr: "graphite:2003"}},
		Grafana: &config.Grafana{URL: g.serve(t), Token: "grafana-token"},
	}
	var said strings.Builder
	err := publishDashboards(t.Context(), cfg, &said)
	if err == nil {
		t.Fatal("it made up a datasource for a sink that cannot describe one")
	}
	if !strings.Contains(err.Error(), "grafana.datasource.uid") {
		t.Errorf("err = %v, want it to name the way out", err)
	}
}

// TestTheLogPanelFollowsTheLokiDatasource: the exported dashboard carries a
// text panel where a failed job's output would be, because a reader may have
// no log store. Naming one swaps that panel for the lines.
func TestTheLogPanelFollowsTheLokiDatasource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		uid   string
		wants string
	}{
		{"no Loki datasource named", "", "text"},
		{"one named", "loki-uid", "logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &grafanaStub{missing: true}
			cfg := influxConfig(g.serve(t))
			cfg.Sinks.Loki = &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"}
			cfg.Grafana.Datasource.LokiUID = tc.uid
			var said strings.Builder
			if err := publishDashboards(t.Context(), cfg, &said); err != nil {
				t.Fatal(err)
			}
			doc := dashboardOf(t, g.writes)
			if got := logPanelKind(doc); got != tc.wants {
				t.Errorf("the failed output panel is a %q, want %q", got, tc.wants)
			}
		})
	}
}

// logPanelTitle is the panel the Loki datasource swaps.
const logPanelTitle = "Where failure output went"

// logPanelKind is the type of the panel that carries a failed job's output.
func logPanelKind(doc map[string]any) string {
	title := logPanelTitle
	var walk func(any) string
	walk = func(panels any) string {
		list, _ := panels.([]any)
		for _, raw := range list {
			panel, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := panel["title"].(string); t == title {
				kind, _ := panel["type"].(string)
				return kind
			}
			if found := walk(panel["panels"]); found != "" {
				return found
			}
		}
		return ""
	}
	return walk(doc["panels"])
}

// TestPublishOnStartStaysOutOfAOneShotRun. Turning it on in a service unit did
// not mean rewriting the dashboard on every run of a scheduled card render.
func TestPublishOnStartStaysOutOfAOneShotRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		o         options
		published bool
	}{
		{"a service", options{}, true},
		{"-once", options{once: true}, false},
		{"-backfill", options{backfill: true}, false},
		{"-card", options{card: "card.svg"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &grafanaStub{missing: true}
			cfg := influxConfig(g.serve(t))
			cfg.Grafana.PublishOnStart = true
			publishOnStart(t.Context(), cfg, tc.o, quietLogger())
			if got := len(g.askedFor) > 0; got != tc.published {
				t.Errorf("talked to Grafana = %v, want %v", got, tc.published)
			}
		})
	}
}

// TestAGrafanaThatIsDownDoesNotStopTheCollector. The metrics of the hour it
// would spend not running cannot be recovered; a dashboard published on the
// next restart can.
func TestAGrafanaThatIsDownDoesNotStopTheCollector(t *testing.T) {
	t.Parallel()
	cfg := influxConfig("http://127.0.0.1:1")
	cfg.Grafana.PublishOnStart = true
	// It returns rather than panicking or exiting, which is the whole claim.
	publishOnStart(t.Context(), cfg, options{}, quietLogger())
}

// quietLogger is a logger for the paths that only have to not blow up.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
