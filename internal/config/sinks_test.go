package config

import (
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
