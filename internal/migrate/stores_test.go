package migrate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// everySink is a configuration with every sink the type has switched on, each
// with the settings its validation asks for.
func everySink(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		GitHub:  config.GitHub{Token: "t"},
		Targets: config.Targets{User: "octocat"},
		Sinks: config.Sinks{
			Influx:        &config.InfluxSink{URL: "http://influx:8181", Bucket: "github", Token: "secret-token"},
			Prometheus:    &config.PrometheusSink{},
			OTLP:          &config.OTLPSink{Endpoint: "http://otel:4318/v1/metrics"},
			Loki:          &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"},
			File:          &config.FileSink{Path: filepath.Join(dir, "points.lp")},
			Stdout:        true,
			Telegraf:      &config.TelegrafSink{URL: "http://telegraf:8186/telegraf"},
			Graphite:      &config.GraphiteSink{Addr: "graphite:2003"},
			SQL:           &config.SQLSink{Path: filepath.Join(dir, "points.sql")},
			Postgres:      &config.PostgresSink{DSN: "postgres://gh:hunter2@db:5433/metrics?search_path=gh"},
			Elasticsearch: &config.ElasticsearchSink{URL: "http://es:9200", Password: "hunter2"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestEverySinkIsAStoreThePlannerKnows: a sink added to the configuration and
// left out of storesOf would be a store no migration ever reached, with
// nothing to say so.
func TestEverySinkIsAStoreThePlannerKnows(t *testing.T) {
	t.Parallel()
	cfg := everySink(t, t.TempDir())
	stores := storesOf(cfg)
	want := 0
	for field := range reflect.TypeFor[config.Sinks]().Fields() {
		if field.Type.Kind() == reflect.Pointer || field.Name == "Stdout" {
			want++
		}
	}
	if len(stores) != want {
		t.Fatalf("storesOf knows %d stores of the %d sinks config.Sinks has", len(stores), want)
	}
	for _, s := range stores {
		switch {
		case s.reach == untouched && s.quiet == "":
			t.Errorf("%s is left untouched and says nothing about why", s.name)
		case s.reach != untouched && s.destination == "":
			t.Errorf("%s keeps a record and names no destination to keep it against", s.name)
		case strings.Contains(s.destination, "hunter2") || strings.Contains(s.destination, "secret"):
			t.Errorf("%s names its destination with a credential: %s", s.name, s.destination)
		}
	}
	if got := storeNamed(stores, "postgres").destination; got != "host=db port=5433 database=metrics schema=gh" {
		t.Errorf("the postgres destination is %q", got)
	}
}

func storeNamed(stores []store, name string) store {
	for _, s := range stores {
		if s.name == name {
			return s
		}
	}
	return store{}
}

// TestAFirstStartRecordsThisReleaseAsTheFirstWriter: a state file that has
// never recorded a run is a first start, and every store it writes is then
// this release's, which is what keeps a fresh install from ever being told a
// store it created holds an old shape.
func TestAFirstStartRecordsThisReleaseAsTheFirstWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := everySink(t, dir)
	state := run.LoadState(filepath.Join(dir, "state.json"))
	if warnings := Stamp(state, cfg, "2.6.2"); len(warnings) > 0 {
		t.Errorf("a first start warned: %v", warnings)
	}
	for _, name := range []string{"influxdb", "telegraf", "graphite", "sql", "postgres", "elasticsearch"} {
		rec := state.Stores[name]
		if rec == nil || rec.FirstWrittenBy != "2.6.2" || rec.WrittenBy != "2.6.2" {
			t.Errorf("%s is recorded as %+v, want first and last written by 2.6.2", name, rec)
		}
	}
	for _, name := range []string{"prometheus", "otlp", "loki", "file", "stdout"} {
		if state.Stores[name] != nil {
			t.Errorf("%s keeps nothing a release could reshape, yet has a record", name)
		}
	}
}

// TestAnUpgradeFromAReleaseWithoutTheRecordLeavesTheFirstWriterUnknown is the
// 2.6.1 state file: a history of runs and no stores. A store that cannot be
// asked is then one an earlier release may have written, and only a SQL file
// that is not there yet can be said to be new.
func TestAnUpgradeFromAReleaseWithoutTheRecordLeavesTheFirstWriterUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := everySink(t, dir)
	if err := os.WriteFile(cfg.Sinks.SQL.Path+".1", []byte("-- rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := olderState(t, dir)
	Stamp(state, cfg, "2.6.2")
	for name, rec := range state.Stores {
		if rec.FirstWrittenBy != "" || rec.WrittenBy != "2.6.2" {
			t.Errorf("%s is recorded as %+v, want an unknown first writer and 2.6.2 last", name, rec)
		}
	}
	// Its rotation is gone and the file never was: nothing wrote it.
	if err := os.Remove(cfg.Sinks.SQL.Path + ".1"); err != nil {
		t.Fatal(err)
	}
	again := olderState(t, dir)
	Stamp(again, cfg, "2.6.2")
	if got := again.Stores["sql"].FirstWrittenBy; got != "2.6.2" {
		t.Errorf("a SQL file that is not there is first written by %q, want 2.6.2", got)
	}
}

// olderState is a state file as 2.6.1 left it: runs recorded, no stores.
func olderState(t *testing.T, dir string) *run.State {
	t.Helper()
	path := filepath.Join(dir, "older.json")
	if err := os.WriteFile(path, []byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return run.LoadState(path)
}

// TestASinkAddedLaterOrPointedElsewhere: a state file that already keeps
// records was written by a release that keeps them, so a sink it has no
// record of is new and this release is its first writer; a sink it has a
// record of under another destination is somebody else's store.
func TestASinkAddedLaterOrPointedElsewhere(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := everySink(t, dir)
	state := olderState(t, dir)
	state.Stores["influxdb"] = &run.StoreRecord{
		Destination: "url=http://influx:8181 org=default bucket=github", FirstWrittenBy: "2.6.2", WrittenBy: "2.6.2",
		Applied: map[string]time.Time{"2.6.1/gh_discussion_comment/is_answer": time.Now()},
	}
	state.Stores["graphite"] = &run.StoreRecord{Destination: "addr=old:2003 prefix=github", FirstWrittenBy: "2.6.2"}
	Stamp(state, cfg, "2.6.3")
	if rec := state.Stores["influxdb"]; rec.FirstWrittenBy != "2.6.2" || rec.WrittenBy != "2.6.3" || len(rec.Applied) != 1 {
		t.Errorf("the store it knew is recorded as %+v, want its history kept and 2.6.3 last", rec)
	}
	if rec := state.Stores["telegraf"]; rec.FirstWrittenBy != "2.6.3" {
		t.Errorf("a sink added since is recorded as %+v, want 2.6.3 first", rec)
	}
	if rec := state.Stores["graphite"]; rec.FirstWrittenBy != "" || rec.Destination != "addr=graphite:2003 prefix=github" {
		t.Errorf("a sink pointed elsewhere is recorded as %+v, want a new record with no first writer", rec)
	}
}

// TestADowngradeIsSaidOnce: the record's last writer newer than the binary.
func TestADowngradeIsSaidOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := everySink(t, dir)
	state := run.LoadState(filepath.Join(dir, "state.json"))
	Stamp(state, cfg, "2.7.0")
	warnings := Stamp(state, cfg, "2.6.2")
	if len(warnings) == 0 || !strings.Contains(warnings[0], "2.7.0") {
		t.Errorf("a downgrade warned %v, want the newer release named", warnings)
	}
	if again := Stamp(state, cfg, "2.6.2"); len(again) > 0 {
		t.Errorf("the same downgrade warned twice: %v", again)
	}
}
