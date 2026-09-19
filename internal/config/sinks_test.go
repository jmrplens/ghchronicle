package config

import (
	"slices"
	"strings"
	"testing"
)

func TestPushSinksExpandEnvAndFillDefaults(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("TEST_ES_URL", "http://es:9200")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks:
  telegraf: {url: http://telegraf:8186}
  graphite: {addr: "graphite:2003"}
  sql: {path: "-"}
  elasticsearch: {url: "${TEST_ES_URL}", api_key: k}
  stdout: true
  stdout_format: json
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sinks.Graphite.Prefix != "github" {
		t.Errorf("graphite prefix default = %q", c.Sinks.Graphite.Prefix)
	}
	if c.Sinks.SQL.Dialect != "postgres" {
		t.Errorf("sql dialect default = %q", c.Sinks.SQL.Dialect)
	}
	if c.Sinks.Elasticsearch.URL != "http://es:9200" || c.Sinks.Elasticsearch.Prefix != "ghchronicle" {
		t.Errorf("elasticsearch = %+v", c.Sinks.Elasticsearch)
	}
	if c.Sinks.StdoutFormat != "json" {
		t.Errorf("stdout_format = %q", c.Sinks.StdoutFormat)
	}
}

func TestPushSinksSayWhatIsRequired(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	cases := map[string]string{
		"sinks: {telegraf: {}}":                                      "sinks.telegraf: url is required",
		"sinks: {graphite: {}}":                                      "sinks.graphite: addr is required",
		"sinks: {sql: {}}":                                           "sinks.sql: path is required",
		"sinks: {sql: {path: x, dialect: mysql}}":                    "sinks.sql.dialect",
		"sinks: {elasticsearch: {}}":                                 "sinks.elasticsearch: url is required",
		"sinks: {elasticsearch: {url: u, api_key: k, username: me}}": "not both",
		"sinks: {stdout: true, stdout_format: yaml}":                 "sinks.stdout_format",
	}
	for body, want := range cases {
		_, err := Load(write(t, "targets: {user: jmrplens}\n"+body+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", body, err, want)
		}
	}
}

// TestAnySingleSinkIsEnoughToStart: the "nowhere to write" rule refuses a
// config with no sink, and must never refuse one that has exactly one, of any
// kind. Each row is the minimum that sink needs, so a sink the rule forgot
// to count would read as none.
func TestAnySingleSinkIsEnoughToStart(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	for _, sink := range []string{
		"influxdb: {url: http://influx:8086, bucket: b}",
		"prometheus: {}",
		"otlp: {endpoint: http://collector:4318/v1/metrics}",
		"loki: {url: http://loki:3100/loki/api/v1/push}",
		"file: {path: out.jsonl}",
		"stdout: true",
		"telegraf: {url: http://telegraf:8186}",
		`graphite: {addr: "graphite:2003"}`,
		`sql: {path: "-"}`,
		"elasticsearch: {url: http://es:9200}",
	} {
		if _, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {"+sink+"}\n")); err != nil {
			t.Errorf("sinks: {%s} was refused: %v", sink, err)
		}
	}
}

// TestACallerWithItsOwnDestinationNeedsNoSink is -card-only: it renders an
// SVG and writes nothing to a database, so the rule is waived for it and
// only for it.
func TestACallerWithItsOwnDestinationNeedsNoSink(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	path := write(t, "targets: {user: jmrplens}\n")
	if _, err := LoadWith(path, Relax{NoSinks: true}); err != nil {
		t.Errorf("a caller that brings its own destination was refused: %v", err)
	}
	if _, err := LoadWith(path, Relax{}); err == nil {
		t.Error("without the waiver the same file must still be refused")
	}
}

// TestInfluxNeedsBothItsURLAndItsBucket: either one alone is a write that
// cannot land, and the job log stays out of a metrics database unless the
// config says otherwise.
func TestInfluxNeedsBothItsURLAndItsBucket(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	for _, sink := range []string{"{url: http://influx:8086}", "{bucket: b}"} {
		_, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {influxdb: "+sink+"}\n"))
		if err == nil || !strings.Contains(err.Error(), "sinks.influxdb: url and bucket are required") {
			t.Errorf("influxdb %s: err = %v, want it to say url and bucket are required", sink, err)
		}
	}
	c, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {influxdb: {url: u, bucket: b}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sinks.Influx; got.Org != "default" || !slices.Equal(got.Exclude, []string{"gh_job_log"}) {
		t.Errorf("influxdb defaults: org = %q, exclude = %v, want default and [gh_job_log]", got.Org, got.Exclude)
	}
}

// TestInfluxKeepsWhatTheConfigChose: a default fills a gap and never replaces
// a value, and an explicitly empty exclude list means "send everything",
// which is not the same as leaving the key out.
func TestInfluxKeepsWhatTheConfigChose(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {influxdb: {url: u, bucket: b, org: acme, exclude: [gh_event]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sinks.Influx; got.Org != "acme" || !slices.Equal(got.Exclude, []string{"gh_event"}) {
		t.Errorf("influxdb: org = %q, exclude = %v, want acme and [gh_event]", got.Org, got.Exclude)
	}
	c, err = Load(write(t, "targets: {user: jmrplens}\nsinks: {influxdb: {url: u, bucket: b, exclude: []}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sinks.Influx.Exclude; got == nil || len(got) != 0 {
		t.Errorf("exclude: [] resolved to %#v, want an empty list that excludes nothing", got)
	}
}

// TestPrometheusFillsOnlyWhatIsMissing: the exporter's port and path default
// to the documented ones and never overwrite a chosen one.
func TestPrometheusFillsOnlyWhatIsMissing(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	cases := map[string][2]string{
		"{}":                                   {":9605", "/metrics"},
		`{listen: "127.0.0.1:9000", path: /m}`: {"127.0.0.1:9000", "/m"},
	}
	for sink, want := range cases {
		c, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {prometheus: "+sink+"}\n"))
		if err != nil {
			t.Fatalf("prometheus %s: %v", sink, err)
		}
		if got := c.Sinks.Prometheus; got.Listen != want[0] || got.Path != want[1] {
			t.Errorf("prometheus %s: listen = %q, path = %q, want %q and %q", sink, got.Listen, got.Path, want[0], want[1])
		}
	}
}

// TestTheOtherSinksSayWhatIsRequired is TestPushSinksSayWhatIsRequired for
// the sinks that list leaves out. An endpoint that expands to nothing is as
// missing as one never written.
func TestTheOtherSinksSayWhatIsRequired(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("TEST_EMPTY_ENDPOINT", "")
	cases := map[string]string{
		"sinks: {otlp: {}}":                                   "sinks.otlp: endpoint is required",
		"sinks: {loki: {}}":                                   "sinks.loki: url is required",
		"sinks: {file: {}}":                                   "sinks.file: path is required",
		"sinks: {file: {path: x, format: csv}}":               `sinks.file.format: "csv" is not influx or json`,
		`sinks: {otlp: {endpoint: "${TEST_EMPTY_ENDPOINT}"}}`: "sinks.otlp: endpoint is required",
	}
	for body, want := range cases {
		_, err := Load(write(t, "targets: {user: jmrplens}\n"+body+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", body, err, want)
		}
	}
}

// TestOTLPAndLokiReadTheirSecretsFromTheEnvironment: an endpoint and an
// authorization header are what a config file committed to a repository must
// not carry, so both expand ${VAR} the way the token does.
func TestOTLPAndLokiReadTheirSecretsFromTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("TEST_OTLP_URL", "http://collector:4318/v1/metrics")
	t.Setenv("TEST_OTLP_AUTH", "Bearer s3cret")
	t.Setenv("TEST_LOKI_URL", "http://loki:3100/loki/api/v1/push")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks:
  otlp: {endpoint: "${TEST_OTLP_URL}", headers: {Authorization: "${TEST_OTLP_AUTH}"}}
  loki: {url: "${TEST_LOKI_URL}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sinks.OTLP; got.Endpoint != "http://collector:4318/v1/metrics" || got.Headers["Authorization"] != "Bearer s3cret" {
		t.Errorf("otlp = %+v, want the endpoint and header from the environment", got)
	}
	if got := c.Sinks.Loki.URL; got != "http://loki:3100/loki/api/v1/push" {
		t.Errorf("loki url = %q, want it from the environment", got)
	}
}

// TestEveryDocumentedFormatIsAccepted: both formats the file sink and stdout
// document, and the empty value that means the default, must load.
func TestEveryDocumentedFormatIsAccepted(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	for _, format := range []string{`""`, "influx", "json"} {
		body := "targets: {user: jmrplens}\nsinks: {stdout: true, stdout_format: " + format +
			", file: {path: out, format: " + format + "}}\n"
		if _, err := Load(write(t, body)); err != nil {
			t.Errorf("format %s was refused: %v", format, err)
		}
	}
}

// TestPushSinksKeepWhatTheConfigChose: the defaults that
// TestPushSinksExpandEnvAndFillDefaults pins fill a gap and never replace a
// value, and Elasticsearch accepts a username and password as the
// alternative to an API key.
func TestPushSinksKeepWhatTheConfigChose(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks:
  graphite: {addr: "graphite:2003", prefix: gh}
  sql: {path: "-", dialect: postgres}
  elasticsearch: {url: http://es:9200, username: me, password: pw, prefix: metrics}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sinks.Graphite.Prefix; got != "gh" {
		t.Errorf("graphite prefix = %q, want gh", got)
	}
	if got := c.Sinks.SQL.Dialect; got != "postgres" {
		t.Errorf("sql dialect = %q, want postgres", got)
	}
	if got := c.Sinks.Elasticsearch; got.Prefix != "metrics" || got.Username != "me" || got.APIKey != "" {
		t.Errorf("elasticsearch = %+v, want prefix metrics with username auth", got)
	}
}
