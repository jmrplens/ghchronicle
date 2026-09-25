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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/httpx"
)

// The project name is fixed, and every compose command below carries it. The
// machine this is developed on runs unrelated containers, so a command that
// addressed the default project could reach them.
const (
	project     = "ghchronicle-e2e"
	composeFile = "docker-compose.yml"
)

// What the compose file configures the stores with. A test that needs to name
// a database or a token reads it from the Stack rather than repeating it.
const (
	influxDatabase      = "ghchronicle"
	influxToken         = "e2e"
	postgresUser        = "ghchronicle"
	postgresPassword    = "ghchronicle"
	postgresDatabase    = "ghchronicle"
	elasticsearchPrefix = "ghchronicle"
	grafanaUser         = "admin"
	grafanaPassword     = "admin"
)

// The datasource uids config/grafana-datasources.yaml provisions.
// /api/ds/query addresses a datasource by uid, so these are the strings a
// dashboard test posts.
const (
	DatasourceInflux        = "e2e-influxdb"
	DatasourcePostgres      = "e2e-postgres"
	DatasourceElasticsearch = "e2e-elasticsearch"
	DatasourceGraphite      = "e2e-graphite"
	DatasourcePrometheus    = "e2e-prometheus"
	DatasourceLoki          = "e2e-loki"
)

// How long any one service is given to answer its readiness probe. Elastic-
// search and InfluxDB are the slow ones and a suite that flakes on a busy
// machine is worse than no suite, so this is generous rather than tight; the
// number that matters is the one Start logs, not this bound.
const readyTimeout = 5 * time.Minute

// stackClient carries the harness's own requests to the stores and Grafana,
// with a pool of its own rather than http.DefaultClient's, for the reason
// internal/httpx gives: the suite's tests run in parallel, and one of them
// closing an httptest server empties the process-wide pool under a request
// the harness has in flight.
var stackClient = &http.Client{Transport: httpx.OwnTransport()}

// dockerBin is a variable rather than a constant so a machine with docker
// somewhere unusual can point at it without editing the file.
var dockerBin = "docker"

// ErrExporterUnreachable is what SetPrometheusTarget returns when Prometheus
// discovered the exporter and could not connect to it.
//
// The exporter is the one sink that is scraped, and it listens on the host, so
// the scrape has to cross from the container back out to the host. Two things
// have to be true for that, and both were false when this was written. The
// exporter has to listen on more than loopback, since the container arrives at
// the bridge gateway address; it now binds every interface. And the host has to
// accept the connection, which a default-deny INPUT policy does not: the
// machine this was written on runs ufw, and no container on any bridge could
// reach any host port until a rule allowed the container address space to the
// scrape port range, and nothing else.
//
// A CI runner needs no rule and neither does an ordinary workstation. Where the
// rule is absent the scrape still fails, and a test that needs it should treat
// this error as "not answerable here" rather than as a failure of the exporter,
// which answers on loopback either way. The range and the reason for it are in
// sweep_push_stores_test.go, beside the port it picks.
var ErrExporterUnreachable = errors.New("prometheus cannot reach the exporter on the host")

// portRef is one published port of one service.
type portRef struct {
	service string
	port    int
}

// Every port the stack publishes. The host side of each is ephemeral, chosen
// by Docker, and read back with `docker compose port`: a fixed host port would
// collide with whatever else the machine is already running.
var published = []portRef{
	{"influxdb", 8181},
	{"postgres", 5432},
	{"elasticsearch", 9200},
	{"graphite", 2003},
	{"graphite", 80},
	{"prometheus", 9090},
	{"loki", 3100},
	{"otelcol", 4318},
	{"otelcol", 13133},
	{"telegraf", 8186},
	{"grafana", 3000},
}

// Stack is the running stack: where each store is, and what it was configured
// with. Every address is on loopback, and every port in it was chosen by
// Docker at start-up.
type Stack struct {
	InfluxURL      string
	InfluxDatabase string
	InfluxToken    string

	PostgresHost     string
	PostgresPort     string
	PostgresUser     string
	PostgresPassword string
	PostgresDatabase string

	ElasticsearchURL    string
	ElasticsearchPrefix string

	// GraphiteAddr is carbon's plaintext receiver, which the sink writes to.
	// GraphiteURL is the render API, which the dashboards read from.
	GraphiteAddr string
	GraphiteURL  string

	PrometheusURL string
	LokiURL       string

	// OTLPEndpoint is the full endpoint the otlp sink posts to, path included.
	// OTelOutput is the file the collector's pipeline writes what it received
	// to, on the host, so a test reads back the data points it accepted.
	OTLPEndpoint string
	OTelOutput   string

	// TelegrafURL is the http_listener_v2 path, and TelegrafOutput is the file
	// its output writes, on the host.
	TelegrafURL    string
	TelegrafOutput string

	GrafanaURL string
	// GrafanaToken is a fresh service account token with the Admin role, which
	// is what /api/ds/query wants and what cmd/check_dashboards reads out of
	// GRAFANA_TOKEN.
	GrafanaToken string

	// Boot is how long each service took to answer its readiness probe,
	// measured from before the containers were created. It is the number that
	// decides whether CI can afford to run this on every push.
	Boot map[string]time.Duration

	dir string
}

var (
	startOnce   sync.Once
	shared      *Stack
	errStart    error
	startedHere bool
)

// Start brings the stack up once per test binary and returns it. A stack that
// is already running, because `make e2e-docker-up` or `make test-e2e-docker`
// started it, is reused as it is and is left running afterwards.
func Start(tb testing.TB) *Stack {
	tb.Helper()
	startOnce.Do(func() {
		// The logging goes to whichever test asked first, which is where a
		// reader of the output would look for it anyway.
		shared, errStart = bringUp(context.Background(), tb)
	})
	if errStart != nil {
		tb.Fatalf("docker stack: %v", errStart)
	}
	return shared
}

// Shutdown tears the stack down, unless it was already running before this
// process started or GHCHRONICLE_E2E_KEEP is set. TestMain calls it.
func Shutdown() error {
	if !startedHere || os.Getenv("GHCHRONICLE_E2E_KEEP") != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return Down(ctx)
}

// Down removes every container, network and volume of the project. It names
// the project explicitly, as everything here does.
func Down(ctx context.Context) error {
	_, err := compose(ctx, "down", "--volumes", "--remove-orphans", "--timeout", "10")
	return err
}

func bringUp(ctx context.Context, tb testing.TB) (*Stack, error) {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	up, err := running(ctx)
	if err != nil {
		return nil, err
	}
	if !up {
		// The bind-mounted output directories belong to one run, and they are
		// only emptied when this process is the one creating the containers:
		// removing a directory a running container has mounted would leave
		// that container writing to an inode nothing on the host can see.
		if prepareErr := prepareOutputs(dir); prepareErr != nil {
			return nil, prepareErr
		}
	}

	// Timed from before the containers are created rather than after, so the
	// number includes everything a cold CI run would pay for. `up -d` without
	// --wait deliberately: the readiness gate is the probe below, which asks
	// each service the question the tests care about.
	// Recorded before the containers are created rather than after, so that a
	// creation that fails half way is still torn down by Shutdown.
	startedHere = !up
	started := time.Now()
	if out, upErr := compose(ctx, "up", "-d"); upErr != nil {
		return nil, fmt.Errorf("compose up: %w\n%s", upErr, out)
	}

	addrs, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	boot, err := waitAll(ctx, addrs, started)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedByDuration(boot) {
		tb.Logf("docker stack: %s ready after %s", name, boot[name].Round(100*time.Millisecond))
	}

	s := newStack(dir, addrs, boot)
	if tokenErr := s.mintGrafanaToken(ctx); tokenErr != nil {
		return nil, tokenErr
	}
	return s, nil
}

// newStack turns the discovered addresses into the addresses a test uses.
func newStack(dir string, addrs map[portRef]string, boot map[string]time.Duration) *Stack {
	host, port, _ := net.SplitHostPort(addrs[portRef{"postgres", 5432}])
	return &Stack{
		InfluxURL:      "http://" + addrs[portRef{"influxdb", 8181}],
		InfluxDatabase: influxDatabase,
		InfluxToken:    influxToken,

		PostgresHost:     host,
		PostgresPort:     port,
		PostgresUser:     postgresUser,
		PostgresPassword: postgresPassword,
		PostgresDatabase: postgresDatabase,

		ElasticsearchURL:    "http://" + addrs[portRef{"elasticsearch", 9200}],
		ElasticsearchPrefix: elasticsearchPrefix,

		GraphiteAddr: addrs[portRef{"graphite", 2003}],
		GraphiteURL:  "http://" + addrs[portRef{"graphite", 80}],

		PrometheusURL: "http://" + addrs[portRef{"prometheus", 9090}],
		LokiURL:       "http://" + addrs[portRef{"loki", 3100}],

		OTLPEndpoint: "http://" + addrs[portRef{"otelcol", 4318}] + "/v1/metrics",
		OTelOutput:   filepath.Join(dir, "out", "otel", "metrics.json"),

		TelegrafURL:    "http://" + addrs[portRef{"telegraf", 8186}] + "/telegraf",
		TelegrafOutput: filepath.Join(dir, "out", "telegraf", "telegraf.influx"),

		GrafanaURL: "http://" + addrs[portRef{"grafana", 3000}],

		Boot: boot,
		dir:  dir,
	}
}

// outputDirs are the three host directories the stack writes into. They are
// bind mounts rather than named volumes because a test reads them directly.
func outputDirs(dir string) []string {
	return []string{
		filepath.Join(dir, "out", "prometheus-targets"),
		filepath.Join(dir, "out", "otel"),
		filepath.Join(dir, "out", "telegraf"),
	}
}

func prepareOutputs(dir string) error {
	for _, d := range outputDirs(dir) {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// running reports whether the project already has containers running, which is
// what decides both whether the outputs are emptied and whether this process
// tears the stack down when it is done.
func running(ctx context.Context) (bool, error) {
	out, err := compose(ctx, "ps", "--status", "running", "--quiet")
	if err != nil {
		return false, fmt.Errorf("compose ps: %w\n%s", err, out)
	}
	return strings.TrimSpace(out) != "", nil
}

// discover reads the host side of every published port back from compose,
// because Docker chose all of them.
func discover(ctx context.Context) (map[portRef]string, error) {
	addrs := make(map[portRef]string, len(published))
	for _, p := range published {
		out, err := compose(ctx, "port", p.service, strconv.Itoa(p.port))
		if err != nil {
			return nil, fmt.Errorf("compose port %s %d: %w\n%s", p.service, p.port, err, out)
		}
		addr := strings.TrimSpace(out)
		if addr == "" {
			return nil, fmt.Errorf("compose published no host port for %s:%d", p.service, p.port)
		}
		// A published port on some Docker versions comes back as 0.0.0.0 even
		// when it was bound to loopback, and a test dialing 0.0.0.0 is a
		// coin toss on a multi-homed host.
		if host, port, splitErr := net.SplitHostPort(addr); splitErr == nil && (host == "0.0.0.0" || host == "::" || host == "") {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		addrs[p] = addr
	}
	return addrs, nil
}

// check is the question that proves one service is ready to be used.
type check struct {
	name  string
	where portRef
	probe func(ctx context.Context, addr string) error
}

// checks asks each service the thing the tests need from it, which is not
// always the thing its container healthcheck answers. Two of them have no
// container healthcheck at all: the Loki and OpenTelemetry collector images
// ship one binary and no shell, so nothing inside them can run a probe.
var checks = []check{
	{"influxdb", portRef{"influxdb", 8181}, httpGet("/health")},
	{"postgres", portRef{"postgres", 5432}, dialTCP},
	{"elasticsearch", portRef{"elasticsearch", 9200}, httpGet("/_cluster/health?wait_for_status=yellow&timeout=1s")},
	{"graphite-carbon", portRef{"graphite", 2003}, dialTCP},
	{"graphite-render", portRef{"graphite", 80}, httpGet("/render?target=carbon.agents.*.metricsReceived&format=json")},
	{"prometheus", portRef{"prometheus", 9090}, httpGet("/-/ready")},
	{"loki", portRef{"loki", 3100}, httpGet("/ready")},
	{"otelcol", portRef{"otelcol", 13133}, httpGet("/")},
	{"otelcol-otlp", portRef{"otelcol", 4318}, dialTCP},
	{"telegraf", portRef{"telegraf", 8186}, telegrafReady},
	{"grafana", portRef{"grafana", 3000}, httpGet("/api/health")},
}

// waitAll asks every service at once, so that one slow store does not hide
// behind another and the times reported are each service's own.
func waitAll(ctx context.Context, addrs map[portRef]string, started time.Time) (map[string]time.Duration, error) {
	type result struct {
		name string
		took time.Duration
		err  error
	}
	out := make(chan result, len(checks))
	var wg sync.WaitGroup
	for _, c := range checks {
		wg.Go(func() {
			err := WaitUntil(ctx, c.name, readyTimeout, func(ctx context.Context) error {
				return c.probe(ctx, addrs[c.where])
			})
			out <- result{c.name, time.Since(started), err}
		})
	}
	wg.Wait()
	close(out)

	boot := make(map[string]time.Duration, len(checks))
	var errs []error
	for r := range out {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		boot[r.name] = r.took
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return boot, nil
}

// WaitUntil polls probe until it stops returning an error, the timeout runs
// out or the context ends. Nothing in this package sleeps for a fixed time and
// hopes: readiness is always something a service was asked.
func WaitUntil(ctx context.Context, what string, timeout time.Duration, probe func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := probe(attempt)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s: %w", what, timeout, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready: %w", what, ctx.Err())
		case <-tick.C:
		}
	}
}

// httpGet returns a probe that wants 200 from one path.
func httpGet(path string) func(context.Context, string) error {
	return func(ctx context.Context, addr string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, http.NoBody)
		if err != nil {
			return err
		}
		res, err := stackClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s", path, res.Status)
		}
		return nil
	}
}

// telegrafReady asks the http_listener_v2 input the same question the sink
// does, minus the points: the input has no health endpoint of its own, and an
// answer of any kind proves the listener is bound. Only a 5xx means it is not.
func telegrafReady(ctx context.Context, addr string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/telegraf", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	res, err := stackClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("POST /telegraf: %s", res.Status)
	}
	return nil
}

// dialTCP is the probe for a port that speaks no HTTP.
func dialTCP(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// sortedByDuration orders the boot report slowest last, so the last line of it
// is the number that decides how often CI can run this.
func sortedByDuration(boot map[string]time.Duration) []string {
	names := make([]string, 0, len(boot))
	for name := range boot {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		switch {
		case boot[a] < boot[b]:
			return -1
		case boot[a] > boot[b]:
			return 1
		default:
			return strings.Compare(a, b)
		}
	})
	return names
}

// Compose runs one compose command against this project and returns its
// combined output.
func (s *Stack) Compose(ctx context.Context, args ...string) (string, error) {
	return compose(ctx, args...)
}

// Exec runs a command inside one of the containers, with no TTY, and returns
// its combined output.
func (s *Stack) Exec(ctx context.Context, service string, args ...string) (string, error) {
	return compose(ctx, append([]string{"exec", "-T", service}, args...)...)
}

// Logs returns one service's container log, which is the first thing to read
// when a store refused something and the test only saw a status code.
func (s *Stack) Logs(ctx context.Context, service string) (string, error) {
	return compose(ctx, "logs", "--no-color", service)
}

// Psql runs one statement and returns the rows, unaligned and without
// headers, which is the shape an assertion wants.
func (s *Stack) Psql(ctx context.Context, statement string) (string, error) {
	return s.Exec(ctx, "postgres",
		"psql", "--username", postgresUser, "--dbname", postgresDatabase,
		"--no-align", "--tuples-only", "--quiet", "--command", statement)
}

// LoadSQL pipes statements through psql, which is exactly what the SQL sink's
// documentation tells a user to do with its output: the sink writes text and
// leaves the connection to psql.
//
// ON_ERROR_STOP is set because psql otherwise reports success having skipped
// every statement that failed, which would make this suite blind to the one
// thing it exists to check.
func (s *Stack) LoadSQL(ctx context.Context, statements io.Reader) (string, error) {
	args := []string{
		"compose", "-p", project, "-f", composeFile, "exec", "-T", "postgres",
		"psql", "--username", postgresUser, "--dbname", postgresDatabase,
		"--quiet", "--variable", "ON_ERROR_STOP=1", "--file", "-",
	}
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Stdin = statements
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("psql: %w", err)
	}
	return string(out), nil
}

// SetPrometheusTarget points Prometheus at an exporter listening on the host,
// and waits until it has actually scraped it.
//
// The exporter is the one sink that is scraped rather than pushed to, and it
// runs inside the collector process on a port the test picks, so the target
// cannot be in the compose file. It arrives through a file_sd file in a
// directory Prometheus has mounted, and host.docker.internal is how the
// container reaches back out to the host.
func (s *Stack) SetPrometheusTarget(ctx context.Context, port int) error {
	target := []map[string]any{{
		"targets": []string{net.JoinHostPort("host.docker.internal", strconv.Itoa(port))},
		"labels":  map[string]string{"job": "ghchronicle"},
	}}
	body, err := json.Marshal(target)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, "out", "prometheus-targets")
	tmp := filepath.Join(dir, ".ghchronicle.json")
	if writeErr := os.WriteFile(tmp, body, 0o600); writeErr != nil {
		return writeErr
	}
	// Renamed into place rather than written in place: Prometheus re-reads the
	// file the moment it changes, and a half-written one is a parse error it
	// would report rather than retry.
	if renameErr := os.Rename(tmp, filepath.Join(dir, "ghchronicle.json")); renameErr != nil {
		return renameErr
	}
	address := net.JoinHostPort("host.docker.internal", strconv.Itoa(port))
	// The caller's exporter has finished its first sweep and is serving by
	// now, and the previous test's exporter was stopped in that test's
	// cleanup, so a successful scrape of this address after this instant can
	// only have reached the caller's.
	configured := time.Now()
	if waitErr := WaitUntil(ctx, "prometheus target", time.Minute, func(ctx context.Context) error {
		return s.targetUp(ctx, address, configured)
	}); waitErr != nil {
		return fmt.Errorf("%w: %w", ErrExporterUnreachable, waitErr)
	}
	return nil
}

// targetUp asks Prometheus whether the exporter at address answered a scrape
// made after since.
//
// Both halves matter, because the previous exporter can look exactly like
// this one. freePort takes the first free port of its range, so consecutive
// tests put their exporters on the same port, the target file comes out byte
// for byte the same, and Prometheus keeps one target whose health is whatever
// its last scrape found. Asking only "is a target up" answered with the
// previous test's last scrape of an exporter that no longer existed: the
// caller then queried before its own exporter had been scraped, and passed
// only while the previous exporter's series had not yet gone stale. The
// race-built collector, slower to start, moved that timing far enough to show
// it.
func (s *Stack) targetUp(ctx context.Context, address string, since time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.PrometheusURL+"/api/v1/targets?state=active", http.NoBody)
	if err != nil {
		return err
	}
	res, err := stackClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var answer struct {
		Data struct {
			ActiveTargets []struct {
				Health     string    `json:"health"`
				LastError  string    `json:"lastError"`
				LastScrape time.Time `json:"lastScrape"`
				ScrapeURL  string    `json:"scrapeUrl"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if decodeErr := json.NewDecoder(res.Body).Decode(&answer); decodeErr != nil {
		return decodeErr
	}
	for _, t := range answer.Data.ActiveTargets {
		scrape, parseErr := url.Parse(t.ScrapeURL)
		if parseErr != nil || scrape.Host != address {
			continue
		}
		if !t.LastScrape.After(since) {
			return fmt.Errorf("prometheus has not scraped %s since the target was set", address)
		}
		if t.Health == "up" {
			return nil
		}
		return fmt.Errorf("prometheus target %s is %s: %s", address, t.Health, t.LastError)
	}
	return fmt.Errorf("prometheus has not picked up the target %s yet", address)
}

// mintGrafanaToken creates a service account and a token for it, which is what
// /api/ds/query authenticates with. The name carries the start time because a
// reused stack already has the earlier run's account and Grafana refuses a
// second one by the same name.
func (s *Stack) mintGrafanaToken(ctx context.Context) error {
	name := "e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var account struct {
		ID int `json:"id"`
	}
	err := s.grafanaPost(ctx, "/api/serviceaccounts",
		map[string]any{"name": name, "role": "Admin", "isDisabled": false}, &account)
	if err != nil {
		return err
	}
	var token struct {
		Key string `json:"key"`
	}
	err = s.grafanaPost(ctx, "/api/serviceaccounts/"+strconv.Itoa(account.ID)+"/tokens",
		map[string]any{"name": name}, &token)
	if err != nil {
		return err
	}
	if token.Key == "" {
		return errors.New("grafana returned an empty service account token")
	}
	s.GrafanaToken = token.Key
	return nil
}

func (s *Stack) grafanaPost(ctx context.Context, path string, body, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.GrafanaURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(grafanaUser, grafanaPassword)
	res, err := stackClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("POST %s: %s", path, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(into)
}

// compose runs one docker compose command in the package directory, always
// naming the project and the file.
func compose(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"compose", "-p", project, "-f", composeFile}, args...)
	cmd := exec.CommandContext(ctx, dockerBin, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
	}
	return string(out), nil
}
