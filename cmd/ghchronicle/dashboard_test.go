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
	missing bool // the datasource is not there yet
	unwell  bool // it is there and cannot reach its store
	// present are the uids a GET finds, for the leftover check. Empty means
	// the ordinary case where nothing of an earlier setup is around.
	present map[string]bool
	// brokenLookup makes the leftover check fail without touching the publish
	// that precedes it.
	brokenLookup bool
	writes       []map[string]any
	askedFor     []string
}

// has says whether the stub should answer a GET for this path.
func (g *grafanaStub) has(path string) bool {
	for uid := range g.present {
		if strings.HasSuffix(path, "/"+uid) {
			return true
		}
	}
	return false
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
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/dashboards/uid/"):
			if g.brokenLookup {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"message":"database is locked"}`)
				return
			}
			if !g.has(r.URL.Path) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"dashboard":{"uid":"x"}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/datasources/"):
			if g.missing && !g.has(r.URL.Path) {
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

// TestItOverwritesOneDashboardRatherThanLeavingTwo. The generated document
// carries the store's uid and the publish overwrites it, so running this twice
// updates what is there. A reader whose dashboard already lives under another
// uid says so and that one is written instead, because the alternative is a
// second dashboard beside the one they have open.
func TestItOverwritesOneDashboardRatherThanLeavingTwo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		override string
		wantUID  string
	}{
		{"the one this generates", "", "ghchronicle-influxdb"},
		{"one that already exists elsewhere", "imported-by-hand", "imported-by-hand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &grafanaStub{missing: true}
			cfg := influxConfig(g.serve(t))
			cfg.Grafana.DashboardUID = tc.override
			var said strings.Builder
			// Twice, because "it overwrites" is a claim about the second run.
			for range 2 {
				if err := publishDashboards(t.Context(), cfg, &said); err != nil {
					t.Fatal(err)
				}
			}
			published := publishedUIDs(t, g.writes)
			if len(published) != 2 {
				t.Fatalf("published %d dashboards, want 2 runs", len(published))
			}
			for _, uid := range published {
				if uid != tc.wantUID {
					t.Errorf("published uid %q, want %q every time", uid, tc.wantUID)
				}
			}
		})
	}
}

// publishedUIDs is the uid of every dashboard that was posted, and it fails
// the test for any posted without asking to overwrite, since a publish that
// does not overwrite is refused the second time.
func publishedUIDs(t *testing.T, writes []map[string]any) []string {
	t.Helper()
	var out []string
	for _, body := range writes {
		doc, ok := body["dashboard"].(map[string]any)
		if !ok {
			continue
		}
		uid, _ := doc["uid"].(string)
		out = append(out, uid)
		if body["overwrite"] != true {
			t.Error("it published without asking to overwrite, so a second run would be refused")
		}
	}
	return out
}

// TestItNamesWhatAnEarlierStoreLeftBehind. Changing which store the collector
// feeds leaves the old dashboard and datasource where they are: the new
// store's uid is a different string, so nothing overwrites them and, without
// this, nothing says they are there.
func TestItNamesWhatAnEarlierStoreLeftBehind(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true, present: map[string]bool{"ghchronicle-graphite": true}}
	cfg := influxConfig(g.serve(t))
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said.String(), "ghchronicle-graphite is still in Grafana") {
		t.Errorf("output = %q, want the leftover named", said.String())
	}
	if !strings.Contains(said.String(), "no graphite sink is configured") {
		t.Errorf("output = %q, want it to say why it is a leftover", said.String())
	}
	// It says so and stops there.
	for _, call := range g.askedFor {
		if strings.HasPrefix(call, http.MethodDelete) {
			t.Errorf("it deleted something: %v", g.askedFor)
		}
	}
}

// TestItSaysNothingAboutTheStoreItJustPublished, which is there because this
// put it there and is the opposite of a leftover.
func TestItSaysNothingAboutTheStoreItJustPublished(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true, present: map[string]bool{"ghchronicle-influxdb": true}}
	cfg := influxConfig(g.serve(t))
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(said.String(), "still in Grafana") {
		t.Errorf("output = %q, want nothing said about the dashboard it just wrote", said.String())
	}
}

// TestTheGeneratedUIDIsALeftoverOnceTheDashboardMovedElsewhere: setting
// dashboard_uid after a run has already published leaves the generated one
// behind for the same reason changing store does.
func TestTheGeneratedUIDIsALeftoverOnceTheDashboardMovedElsewhere(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true, present: map[string]bool{"ghchronicle-influxdb": true}}
	cfg := influxConfig(g.serve(t))
	cfg.Grafana.DashboardUID = "somewhere-else"
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said.String(), "ghchronicle-influxdb is still in Grafana") {
		t.Errorf("output = %q, want the dashboard it no longer writes to named", said.String())
	}
}

// TestACleanGrafanaIsSaidNothingAbout.
func TestACleanGrafanaIsSaidNothingAbout(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true}
	cfg := influxConfig(g.serve(t))
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(said.String(), "note:") {
		t.Errorf("output = %q, want no note where there is nothing to note", said.String())
	}
}

// TestTheElasticsearchSinkDescribesItsOwnDatasource, which is the other of the
// two that can: the address it writes to is the address Grafana queries.
func TestTheElasticsearchSinkDescribesItsOwnDatasource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		sink   config.ElasticsearchSink
		secret string
		value  string
	}{
		{"with an api key", config.ElasticsearchSink{
			URL: "http://es:9200", Prefix: "ghc-", APIKey: "key",
		}, "httpHeaderValue1", "ApiKey key"},
		{"with a password", config.ElasticsearchSink{
			URL: "http://es:9200", Prefix: "ghc-", Username: "u", Password: "p",
		}, "basicAuthPassword", "p"},
		{"with neither", config.ElasticsearchSink{
			URL: "http://es:9200", Prefix: "ghc-",
		}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &grafanaStub{missing: true}
			sink := tc.sink
			cfg := &config.Config{
				Sinks:   config.Sinks{Elasticsearch: &sink},
				Grafana: &config.Grafana{URL: g.serve(t), Token: "grafana-token"},
			}
			var said strings.Builder
			if err := publishDashboards(t.Context(), cfg, &said); err != nil {
				t.Fatal(err)
			}
			ds := g.writes[0]
			if ds["url"] != "http://es:9200" {
				t.Errorf("url = %v, want the sink's", ds["url"])
			}
			settings, _ := ds["jsonData"].(map[string]any)
			if settings["index"] != "ghc-*" || settings["timeField"] != "time" {
				t.Errorf("jsonData = %v, want the sink's prefix as the index", settings)
			}
			secret, _ := ds["secureJsonData"].(map[string]any)
			if tc.secret == "" {
				if len(secret) != 0 {
					t.Errorf("secureJsonData = %v, want none where the sink has no credential", secret)
				}
				return
			}
			if secret[tc.secret] != tc.value {
				t.Errorf("secureJsonData[%s] = %v, want %q", tc.secret, secret[tc.secret], tc.value)
			}
		})
	}
}

// TestASinkWithNoAddressIsNotGuessedAt.
func TestASinkWithNoAddressIsNotGuessedAt(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true}
	cfg := &config.Config{
		Sinks:   config.Sinks{Elasticsearch: &config.ElasticsearchSink{Prefix: "ghc-"}},
		Grafana: &config.Grafana{URL: g.serve(t), Token: "grafana-token"},
	}
	var said strings.Builder
	err := publishDashboards(t.Context(), cfg, &said)
	if err == nil || !strings.Contains(err.Error(), "grafana.datasource.url") {
		t.Errorf("err = %v, want it to ask for the address rather than invent one", err)
	}
}

// TestNoStoreToPublishSaysSo rather than reporting a run that did nothing.
func TestNoStoreToPublishSaysSo(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{}
	cfg := &config.Config{
		Sinks:   config.Sinks{Stdout: true},
		Grafana: &config.Grafana{URL: g.serve(t), Token: "grafana-token"},
	}
	var said strings.Builder
	err := publishDashboards(t.Context(), cfg, &said)
	if err == nil || !strings.Contains(err.Error(), "nothing to publish") {
		t.Errorf("err = %v, want it to say no store it builds a dashboard for is configured", err)
	}
}

// TestPublishingNeedsATokenAndAGrafanaSection: both refusals happen before
// anything is sent, since an anonymous request some servers accept would
// publish under whoever the server thinks is asking.
func TestPublishingNeedsATokenAndAGrafanaSection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		says string
	}{
		{"no section at all", &config.Config{}, "no grafana section"},
		{"a section with no token", &config.Config{
			Grafana: &config.Grafana{URL: "http://grafana:3000"},
		}, "refusing to publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var said strings.Builder
			err := publishDashboards(t.Context(), tc.cfg, &said)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want it to carry %q", err, tc.says)
			}
			if said.String() != "" {
				t.Errorf("it said %q before refusing", said.String())
			}
		})
	}
}

// TestALeftoverCheckThatCannotRunSaysSoAndTheRunStillStands. The publishing it
// follows has already succeeded, so failing over the courtesy check would undo
// nothing.
func TestALeftoverCheckThatCannotRunSaysSoAndTheRunStillStands(t *testing.T) {
	t.Parallel()
	g := &grafanaStub{missing: true, brokenLookup: true}
	cfg := influxConfig(g.serve(t))
	var said strings.Builder
	if err := publishDashboards(t.Context(), cfg, &said); err != nil {
		t.Fatalf("the publish failed over a check that only looks: %v", err)
	}
	if !strings.Contains(said.String(), "could not check whether") {
		t.Errorf("output = %q, want it to say it could not look", said.String())
	}
	if !strings.Contains(said.String(), "published at") {
		t.Errorf("output = %q, want the publish reported all the same", said.String())
	}
}
