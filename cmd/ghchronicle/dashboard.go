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
	for _, store := range stores {
		if failed := publishOne(ctx, client, cfg, store, folder, logs, out); failed != nil {
			return failed
		}
	}
	return nil
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
	case override.UID != "":
		// Adopted, so nothing here has to describe it.
		return want, nil
	default:
		return want, fmt.Errorf(
			"the %s sink does not know the address Grafana would query, so it cannot "+
				"describe a datasource: create one in Grafana and name it in "+
				"grafana.datasource.uid", store.Name,
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
