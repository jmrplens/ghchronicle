package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/dashboards"
	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// grafanaTimeout bounds each call. Publishing a dashboard is one large POST
// and a health check can wait on a store that is not answering, so it is
// generous; nothing here is on the sweep's path.
const grafanaTimeout = 60 * time.Second

// publishDashboards is the whole of -publish-dashboard, and of the same work
// done at start-up. It reconciles one datasource and one dashboard per store
// this collector writes to, and reports each line to out.
//
// It asks GitHub nothing, so it runs without a GitHub token.
func publishDashboards(ctx context.Context, cfg *config.Config, out io.Writer) error {
	settings := cfg.Grafana
	if settings == nil {
		return errors.New("there is no grafana section in the config, so there is nowhere to publish to")
	}
	client := grafana.Client{URL: settings.URL, Token: settings.Token}
	if client.URL == "" {
		client.URL = grafana.DefaultURL
	}
	// This writes to a live server. An anonymous request some Grafanas accept
	// would publish under whoever the server thinks is asking, which is not a
	// thing to do by accident.
	if client.Token == "" {
		return errors.New("grafana.token is empty, refusing to publish")
	}

	stores, err := storesToPublish(cfg)
	if err != nil {
		return err
	}
	folder, err := client.EnsureFolder(ctx, settings.Folder, grafanaTimeout)
	if err != nil {
		return fmt.Errorf("the folder %q: %w", settings.Folder, err)
	}
	logs := lokiUID(cfg, settings)
	written := map[string]bool{}
	for _, store := range stores {
		if failed := publishOne(ctx, client, cfg, store, folder, logs, out); failed != nil {
			return failed
		}
		written[publishedUID(cfg, store)] = true
	}
	reportLeftovers(ctx, client, written, out)
	return nil
}

// publishedUID is where this run's dashboard for a store went.
func publishedUID(cfg *config.Config, store *dashboards.Store) string {
	if override := cfg.Grafana.DashboardUID; override != "" {
		return override
	}
	return store.UID
}

// reportLeftovers names what this made under a uid nothing here writes to any
// more. Changing which store the collector feeds is the case: the dashboard
// and datasource for the old one stay where they are, pointing at something
// nobody fills, and the generated uid of the new store is a different string,
// so nothing overwrites them and nothing says they are there. Setting
// dashboard_uid after a run has already published is the other case, for the
// same reason.
//
// It names them and stops. Deleting somebody's dashboard unasked is not a
// thing a collector should do, even one it made: it may be the copy they are
// still reading, or one they have edited since.
func reportLeftovers(ctx context.Context, client grafana.Client,
	written map[string]bool, out io.Writer,
) {
	for _, store := range dashboards.AllStores() {
		if written[store.UID] {
			continue
		}
		what, err := leftoverAt(ctx, client, store.UID)
		if err != nil {
			// A courtesy check. The publishing it follows has already
			// succeeded, so failing the run over this would undo nothing and
			// report nothing useful; saying it could not look is the honest
			// middle.
			fmt.Fprintf(out, "note: could not check whether %s was left behind: %v\n", store.UID, err)
			continue
		}
		if what == "" {
			continue
		}
		fmt.Fprintf(out, "note: %s is still in Grafana (%s) and no %s sink is configured here. "+
			"Delete it there if an earlier setup left it.\n", store.UID, what, store.Name)
	}
}

// leftoverAt says what answers at a uid, in the words the note prints, and ""
// when nothing does.
func leftoverAt(ctx context.Context, client grafana.Client, uid string) (string, error) {
	dashboard, err := client.Exists(ctx, grafana.DashboardPath(uid), grafanaTimeout)
	if err != nil {
		return "", err
	}
	datasource, err := client.Exists(ctx, grafana.DatasourcePath(uid), grafanaTimeout)
	if err != nil {
		return "", err
	}
	switch {
	case dashboard && datasource:
		return "a dashboard and its datasource", nil
	case dashboard:
		return "a dashboard", nil
	case datasource:
		return "a datasource", nil
	default:
		return "", nil
	}
}

// publishOne does one store: the datasource, the probe that says whether it
// can be reached, and the dashboard.
func publishOne(ctx context.Context, client grafana.Client, cfg *config.Config,
	store *dashboards.Store, folder, logs string, out io.Writer,
) error {
	uid, err := reconcileDatasource(ctx, client, cfg, store, out)
	if err != nil {
		return err
	}
	ok, message, err := client.DatasourceHealth(ctx, uid, grafanaTimeout)
	if err != nil {
		return fmt.Errorf("asking datasource %s whether it answers: %w", uid, err)
	}
	if !ok {
		// Refusing here is the point of the probe. A dashboard published
		// against a datasource Grafana cannot reach draws nothing, and the
		// commonest cause is an address that is right for the collector and
		// wrong for Grafana, which is what grafana.datasource.url is for.
		return fmt.Errorf("datasource %s does not answer: %s\n"+
			"set grafana.datasource.url to the address Grafana reaches the store by, "+
			"which is not always the one this writes to", uid, message)
	}
	fmt.Fprintf(out, "datasource %s answers: %s\n", uid, message)

	doc := dashboards.Publishable(store, uid, logs)
	// The document carries the store's own uid, and publishing overwrites
	// whatever is at it, so two runs update one dashboard rather than leaving
	// two. Somewhere the dashboard already lives under another uid, that is
	// the one to write over: otherwise the reader keeps the dashboard they
	// have open and this quietly updates a second one beside it.
	at := store.UID
	if override := cfg.Grafana.DashboardUID; override != "" {
		at = override
		doc["uid"] = override
	}
	path, err := client.PublishDashboard(ctx, doc, folder,
		"published by ghchronicle, store "+store.Name, grafanaTimeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dashboard %s published at %s\n", at, client.URL+path)
	return nil
}

// reconcileDatasource makes the datasource the dashboard will read from exist
// and say the right things, and gives back its uid.
func reconcileDatasource(ctx context.Context, client grafana.Client,
	cfg *config.Config, store *dashboards.Store, out io.Writer,
) (string, error) {
	want, err := datasourceFor(cfg, store)
	if err != nil {
		return "", err
	}
	// An adopted datasource is somebody else's to describe. Correcting one
	// this did not make would overwrite settings nobody asked it to have.
	if cfg.Grafana.Datasource.UID != "" {
		fmt.Fprintf(out, "datasource %s adopted as configured, left as it is\n", want.UID)
		return want.UID, nil
	}
	outcome, err := client.EnsureDatasource(ctx, want, grafanaTimeout)
	if err != nil {
		return "", fmt.Errorf("the datasource for %s: %w", store.Name, err)
	}
	fmt.Fprintf(out, "datasource %s (%s) %s\n", want.UID, want.Type, outcome)
	return want.UID, nil
}

// lokiUID is the Loki datasource the failed job output panel reads from, or
// "" to leave that panel as the note saying where the lines went. Only an
// adopted uid is used: the Loki sink writes to a push endpoint, and a
// datasource made from it would be a datasource pointed at the wrong half of
// the API.
func lokiUID(cfg *config.Config, settings *config.Grafana) string {
	if cfg.Sinks.Loki == nil {
		return ""
	}
	return settings.Datasource.LokiUID
}

// storesToPublish is one store per metric sink configured, in a fixed order so
// two runs of the same config report the same way.
func storesToPublish(cfg *config.Config) ([]*dashboards.Store, error) {
	var out []*dashboards.Store
	for _, named := range []struct {
		store      string
		configured bool
	}{
		{"influxdb", cfg.Sinks.Influx != nil},
		{"elasticsearch", cfg.Sinks.Elasticsearch != nil},
		{"prometheus", cfg.Sinks.Prometheus != nil},
		{"postgres", cfg.Sinks.SQL != nil},
		{"graphite", cfg.Sinks.Graphite != nil},
	} {
		if !named.configured {
			continue
		}
		store, ok := dashboards.ByName(named.store)
		if !ok {
			return nil, fmt.Errorf("no dashboard is built for %s", named.store)
		}
		out = append(out, store)
	}
	if len(out) == 0 {
		return nil, errors.New("no sink this builds a dashboard for is configured, " +
			"so there is nothing to publish")
	}
	return out, nil
}

// datasourceFor describes the datasource a store's dashboard reads from.
//
// Two of the five sinks know the address Grafana queries, because it is the
// address they write to. The other three do not, and no amount of reading
// their settings would find it: the Prometheus sink is scraped rather than
// written to, the SQL sink writes statements to a file rather than to a
// server, and the Graphite sink speaks the ingest port, which is not the API
// Grafana asks. Those three are adopted by uid or not published at all.
func datasourceFor(cfg *config.Config, store *dashboards.Store) (grafana.Datasource, error) {
	override := cfg.Grafana.Datasource
	want := grafana.Datasource{
		UID:  firstNonEmpty(override.UID, store.UID),
		Name: "ghchronicle-" + store.Name,
		Type: dashboards.PluginID(store),
	}
	switch {
	case store.Name == "influxdb" && cfg.Sinks.Influx != nil:
		sink := cfg.Sinks.Influx
		want.URL = firstNonEmpty(override.URL, sink.URL)
		want.Database = sink.Bucket
		want.JSON = map[string]any{
			"version":         "SQL",
			"httpMode":        "POST",
			"dbName":          sink.Bucket,
			"httpHeaderName1": "Authorization",
		}
		// Both places, because this plugin reads the token from one and the
		// header from the other depending on the call.
		want.Secret = map[string]string{
			"token":            sink.Token,
			"httpHeaderValue1": "Bearer " + sink.Token,
		}
	case store.Name == "elasticsearch" && cfg.Sinks.Elasticsearch != nil:
		sink := cfg.Sinks.Elasticsearch
		want.URL = firstNonEmpty(override.URL, sink.URL)
		want.JSON = map[string]any{
			"index":     sink.Prefix + "*",
			"timeField": "time",
		}
		want.Secret = elasticSecret(sink)
	case store.Name == "prometheus" && override.URL != "":
		// The sink is scraped rather than written to, so it has no idea where
		// the Prometheus server is. Told the address, there is nothing else to
		// know: a Prometheus datasource is a URL.
		want.URL = override.URL
		want.JSON = map[string]any{"httpMethod": "POST"}
	case store.Name == "graphite" && override.URL != "":
		// Also told rather than derived, and for a sharper reason: the sink
		// speaks the ingest port while Grafana queries the web API, which is a
		// different port on the same host.
		want.URL = override.URL
		want.JSON = map[string]any{"graphiteVersion": "1.1"}
	case override.UID != "":
		// Adopted, so nothing here has to describe it.
		return want, nil
	case store.Name == "postgres":
		// The one that cannot be told either. The SQL sink writes statements
		// to a file and never connects, so there is no host, port, user or
		// password anywhere in the config to build a datasource out of.
		return want, errors.New(
			"the sql sink writes statements to a file and never connects, so nothing here " +
				"knows the server Grafana would query: create the datasource in Grafana and " +
				"name it in grafana.datasource.uid",
		)
	default:
		return want, fmt.Errorf(
			"the %s sink does not know the address Grafana would query: set "+
				"grafana.datasource.url to it, or create the datasource in Grafana and name "+
				"it in grafana.datasource.uid", store.Name,
		)
	}
	if want.URL == "" {
		return want, fmt.Errorf("the %s sink names no address, "+
			"so set grafana.datasource.url", store.Name)
	}
	return want, nil
}

// elasticSecret carries whichever credential the sink was given, and none when
// it was given none.
func elasticSecret(sink *config.ElasticsearchSink) map[string]string {
	switch {
	case sink.APIKey != "":
		return map[string]string{"httpHeaderValue1": "ApiKey " + sink.APIKey}
	case sink.Password != "":
		return map[string]string{"basicAuthPassword": sink.Password}
	default:
		return nil
	}
}

// firstNonEmpty is the override, or the thing it overrides.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// publishOnStart reconciles Grafana once before the first sweep, when the
// config asks for it.
//
// A failure here warns and the sweep goes on. The collector's job is to
// collect, and refusing to start because a dashboard server is down would
// trade the thing that cannot be recovered later, the metrics of the hour it
// spent not running, for the thing that can, a dashboard published on the next
// restart.
func publishOnStart(ctx context.Context, cfg *config.Config, o options, logger *slog.Logger) {
	// A one-shot run is somebody's script or a scheduled Action. Publishing
	// from it would rewrite the dashboard on every run of a cron job, which is
	// not what turning this on in a service unit meant.
	if cfg.Grafana == nil || !cfg.Grafana.PublishOnStart || o.once || o.backfill || o.card != "" {
		return
	}
	var said strings.Builder
	if err := publishDashboards(ctx, cfg, &said); err != nil {
		logger.Warn("could not publish the dashboard, carrying on without it",
			"error", err, "said", strings.TrimSpace(said.String()))
		return
	}
	for line := range strings.SplitSeq(strings.TrimSpace(said.String()), "\n") {
		logger.Info("grafana", "did", line)
	}
}
