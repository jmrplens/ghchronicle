package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/dashboards"
	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
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
	client := grafana.Client{
		URL:      settings.URL,
		Token:    settings.Token,
		User:     settings.User,
		Password: settings.Password,
	}
	if client.URL == "" {
		client.URL = grafana.DefaultURL
	}
	// This writes to a live server. An anonymous request some Grafanas accept
	// would publish under whoever the server thinks is asking, which is not a
	// thing to do by accident.
	if client.Token == "" && client.User == "" {
		return errors.New("grafana has neither a token nor a user, refusing to publish")
	}

	stores, err := storesToPublish(cfg)
	if err != nil {
		return err
	}
	folder, err := client.EnsureFolder(ctx, settings.Folder, grafanaTimeout)
	if err != nil {
		return fmt.Errorf("the folder %q: %w", settings.Folder, err)
	}
	logs, err := lokiDatasource(ctx, client, cfg, out)
	if err != nil {
		return err
	}

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
	switch {
	case ok:
		fmt.Fprintf(out, "datasource %s answers: %s\n", uid, message)
	case emptyStore(message):
		// Reached, and holding nothing yet. That is what a store looks like
		// on the first start of a stack that brought it up alongside this,
		// since a database is made by the first write and the dashboard is
		// published before the first sweep. Refusing here would mean a new
		// deployment never gets its dashboard, which is the opposite of what
		// this is for.
		fmt.Fprintf(out, "note: %s has nothing in it yet (%s), which is what a store "+
			"looks like before the first sweep. Publishing anyway.\n", uid, message)
	default:
		// Refusing here is the point of the probe. A dashboard published
		// against a datasource Grafana cannot reach draws nothing, and the
		// commonest cause is an address that is right for the collector and
		// wrong for Grafana, which is what grafana.datasource.url is for.
		return fmt.Errorf("datasource %s does not answer: %s\n"+
			"set grafana.datasource.url to the address Grafana reaches the store by, "+
			"which is not always the one this writes to", uid, message)
	}
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
	want, err := datasourceFor(cfg, store, out)
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

// lokiPushPath is where a Loki sink writes. A datasource asks the same server
// somewhere else, so the datasource's address is this suffix removed.
const lokiPushPath = "/loki/api/v1/push"

// lokiDatasource is the Loki datasource the failed job output panel reads
// from, or "" to leave that panel as the note saying where the lines went.
//
// A uid in the config is adopted. Otherwise the datasource is made from the
// sink, which is possible for the one reason it looked impossible at first:
// the sink's address is the push endpoint and Grafana queries the base, and
// the difference between them is a fixed, documented suffix rather than
// anything that has to be guessed. A sink writing somewhere else is the case
// that cannot be derived, and only that one still needs the uid.
func lokiDatasource(ctx context.Context, client grafana.Client, cfg *config.Config,
	out io.Writer,
) (string, error) {
	sink := cfg.Sinks.Loki
	if sink == nil {
		return "", nil
	}
	if uid := cfg.Grafana.Datasource.LokiUID; uid != "" {
		return uid, nil
	}
	base, ok := strings.CutSuffix(strings.TrimSuffix(sink.URL, "/"), lokiPushPath)
	if !ok {
		fmt.Fprintf(out, "note: sinks.loki.url does not end in %s, so the address Grafana "+
			"would query cannot be worked out from it. Name a Loki datasource in "+
			"grafana.datasource.loki_uid to draw the failed job output.\n", lokiPushPath)
		return "", nil
	}
	want := grafana.Datasource{
		UID: "ghchronicle-loki", Name: "ghchronicle-loki", Type: "loki", URL: base,
	}
	if sink.TenantID != "" {
		// Loki calls it a tenant and reads it from this header; Grafana calls
		// it an organization and sends it from this setting.
		want.JSON = map[string]any{"httpHeaderName1": "X-Scope-OrgID"}
		want.Secret = map[string]string{"httpHeaderValue1": sink.TenantID}
	}
	outcome, err := client.EnsureDatasource(ctx, want, grafanaTimeout)
	if err != nil {
		return "", fmt.Errorf("the Loki datasource: %w", err)
	}
	fmt.Fprintf(out, "datasource %s (loki) %s\n", want.UID, outcome)
	return want.UID, nil
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
		// Either way into a PostgreSQL: the connecting sink, or the file
		// sink whose statements are loaded into one.
		{"postgres", cfg.Sinks.SQL != nil || cfg.Sinks.Postgres != nil},
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
func datasourceFor(cfg *config.Config, store *dashboards.Store,
	out io.Writer,
) (grafana.Datasource, error) {
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
			"version":  "SQL",
			"httpMode": "POST",
			"dbName":   sink.Bucket,
			// The SQL datasource queries over FlightSQL, which is gRPC and
			// assumes TLS. A server reached over http does not have it, and
			// the handshake fails with "first record does not look like a TLS
			// handshake" rather than with anything about certificates.
			//
			// The key is insecureGrpc and not secureGrpc: the plugin ignores
			// the one that reads like its opposite, so a datasource carrying
			// secureGrpc=false still tried TLS and still failed. Taken from a
			// datasource that demonstrably works rather than from a guess.
			"insecureGrpc": !strings.HasPrefix(want.URL, "https://"),
		}
		if sink.Token != "" {
			want.JSON["httpHeaderName1"] = "Authorization"
		}
		// Both places, because this plugin reads the token from one and the
		// header from the other depending on the call. A store started
		// without auth has neither, and sending an empty header is what the
		// store itself rejects as malformed.
		if sink.Token != "" {
			want.Secret = map[string]string{
				"token":            sink.Token,
				"httpHeaderValue1": "Bearer " + sink.Token,
			}
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
	case store.Name == "postgres" && cfg.Sinks.Postgres != nil:
		// The connecting sink knows the server, because it connects to it.
		// Everything Grafana needs is in the DSN.
		if err := fromDSN(&want, cfg.Sinks.Postgres.DSN, override, out); err != nil {
			return want, err
		}
	case override.UID != "":
		// Adopted, so nothing here has to describe it.
		return want, nil
	case store.Name == "postgres":
		// Only the file sink is configured. It writes statements and never
		// connects, so there is no host, port, user or password anywhere in
		// the config to build a datasource out of. The connecting sink, in the
		// case above, is the one that can.
		return want, errors.New(
			"the sql sink writes statements to a file and never connects, so nothing here " +
				"knows the server Grafana would query: configure sinks.postgres instead, " +
				"which does, or create the datasource in Grafana and name it in " +
				"grafana.datasource.uid",
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

// fromDSN fills a PostgreSQL datasource out of the connection string the sink
// dials with, so the one place the server is written down is the one the
// collector already uses.
//
// pgx's own parser rather than a second one here: it takes the URL form and
// the keyword form, and it reads the password file and the service file the
// way libpq does. A parser of our own would disagree with the sink about what
// the config says on exactly the inputs where being wrong matters.
func fromDSN(want *grafana.Datasource, dsn string, override config.GrafanaDatasource,
	out io.Writer,
) error {
	parsed, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("reading sinks.postgres.dsn: %w", err)
	}
	want.URL = firstNonEmpty(override.URL,
		net.JoinHostPort(parsed.Host, strconv.Itoa(int(parsed.Port))))
	want.Database = parsed.Database
	want.User = parsed.User
	want.JSON = map[string]any{"postgresVersion": 1500}
	if mode, note := sslMode(dsn, override); mode != "" {
		want.JSON["sslmode"] = mode
		if note != "" {
			fmt.Fprintln(out, note)
		}
	}
	// Only a password the DSN itself carries. pgx reads libpq's environment
	// and its password file, the way the sink wants it to, but a datasource is
	// written into a Grafana other people can see: a credential that came from
	// the machine doing the publishing rather than from the configuration is
	// one nobody asked to put there. Measured on a Windows runner, where a DSN
	// with no password produced one.
	switch written := dsnPassword(dsn); {
	case written != "":
		want.Secret = map[string]string{"password": written}
	case parsed.Password != "":
		fmt.Fprintln(out, "note: the dsn carries no password and one was found in this "+
			"machine's environment. It was not copied into the datasource, because that "+
			"is a credential the configuration never named. Put it in the dsn, or set it "+
			"on the datasource in Grafana.")
	}
	return nil
}

// dsnPassword is the password the connection string says, and "" when it says
// none. The URL form carries it in the userinfo and the keyword form in a
// field of its own.
func dsnPassword(dsn string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.User != nil {
		if password, set := parsed.User.Password(); set {
			return password
		}
	}
	if found := dsnPasswordPattern.FindStringSubmatch(dsn); found != nil {
		if found[1] != "" {
			return found[1]
		}
		return found[2]
	}
	return ""
}

// dsnPasswordPattern finds the keyword form's password, which runs to the next
// space or to the end, and may be quoted.
var dsnPasswordPattern = regexp.MustCompile(`(?:^|\s)password=(?:'([^']*)'|(\S+))`)

// grafanaSSLModes are the four the PostgreSQL datasource understands. libpq
// has two more, and that is the whole difficulty below.
var grafanaSSLModes = []string{"disable", "require", "verify-ca", "verify-full"}

// sslMode is the mode the datasource is given, and a line to print when the
// answer had to be chosen rather than read.
//
// libpq defaults to "prefer", which means try TLS and carry on without it, and
// Grafana cannot say that: its datasource either insists or refuses. So a DSN
// that names one of the four is believed, a reader who set the override is
// believed over everything, and a DSN that says nothing, or says "prefer" or
// "allow", is answered with "disable" and a line saying so. Guessing "require"
// instead would be the same guess pointed at the other half of the readers,
// and the ones it breaks would have a datasource that cannot connect at all
// rather than one that connects without TLS on a private network.
func sslMode(dsn string, override config.GrafanaDatasource) (mode, note string) {
	if override.SSLMode != "" {
		return override.SSLMode, ""
	}
	// Read out of the text rather than off the parsed config: pgx spends
	// sslmode building the TLS settings and does not keep the word, and the
	// word is what the datasource is configured with.
	named := dsnSSLMode(dsn)
	if slices.Contains(grafanaSSLModes, named) {
		return named, ""
	}
	if named == "" {
		named = "prefer, which is libpq's default when the dsn does not say"
	}
	return "disable", fmt.Sprintf(
		"note: the dsn asks for sslmode=%s and Grafana's datasource has no such mode, "+
			"so it was given sslmode=disable. Set grafana.datasource.sslmode to one of "+
			"%s to choose.", named, strings.Join(grafanaSSLModes, ", "),
	)
}

// dsnSSLModePattern finds the mode in either shape a DSN takes: a query
// parameter in the URL form, a word in the keyword form.
var dsnSSLModePattern = regexp.MustCompile(`(?:^|[?&\s])sslmode=([A-Za-z-]+)`)

// dsnSSLMode is the mode the connection string names, or "" when it names
// none, in which case libpq's own default applies and Grafana cannot say it.
func dsnSSLMode(dsn string) string {
	if found := dsnSSLModePattern.FindStringSubmatch(dsn); found != nil {
		return strings.ToLower(found[1])
	}
	return ""
}

// emptyStoreSigns are what a store that is there and holds nothing says. They
// are the store's own words rather than a status, because a datasource plugin
// reports both "I could not reach it" and "it has nothing" as one ERROR.
var emptyStoreSigns = []string{
	"database not found",
	"table not found",
	"index_not_found_exception",
	"no such table",
}

// emptyStore says whether a health message is a store with nothing in it yet
// rather than one that cannot be reached.
//
// The difference matters on exactly one day: the first. A store brought up
// beside the collector has no database until the first write, and the
// dashboard is published before the first sweep, so refusing here would mean a
// new deployment never gets the dashboard it was set up to have. A store that
// is genuinely unreachable says something else entirely, and that still stops
// the run.
func emptyStore(message string) bool {
	lower := strings.ToLower(message)
	for _, sign := range emptyStoreSigns {
		if strings.Contains(lower, sign) {
			return true
		}
	}
	return false
}
