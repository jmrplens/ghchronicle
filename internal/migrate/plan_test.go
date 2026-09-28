package migrate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// fakeStore answers the planner's questions from a table of shapes and keeps
// the questions, so a test can say what was asked as well as what was
// decided.
type fakeStore struct {
	name, server string
	err          error
	shapes       map[string]teardown.Shape
	asked        []string
	described    int
}

func (f *fakeStore) Name() string { return f.name }

func (f *fakeStore) Describe(context.Context) (string, error) {
	f.described++
	return f.server, f.err
}

func (f *fakeStore) Shape(_ context.Context, m string, old, values []string) (teardown.Shape, error) {
	f.asked = append(f.asked, m)
	held, ok := f.shapes[m]
	if !ok {
		return teardown.Shape{}, nil
	}
	out := teardown.Shape{Exists: held.Exists, Rows: held.Rows, Oldest: held.Oldest, Values: map[string][]string{}}
	for _, t := range held.Old {
		if slices.Contains(old, t) {
			out.Old = append(out.Old, t)
		}
	}
	for _, t := range values {
		if v, has := held.Values[t]; has {
			out.Values[t] = v
		}
	}
	return out, nil
}

// oldShape is a store written by 2.6.0 and before: gh_discussion_comment
// with is_answer as a tag, the alert items as 1.0.0 left them, and the cache
// entries of the days before 2.6.0.
func oldShape(users ...string) map[string]teardown.Shape {
	if len(users) == 0 {
		users = []string{"octocat"}
	}
	oldest := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	return map[string]teardown.Shape{
		"gh_discussion_comment": {
			Exists: true, Old: []string{"is_answer"}, Rows: 3, Oldest: oldest,
			Values: map[string][]string{"user": users},
		},
		"gh_dependabot_alert_item": {
			Exists: true, Rows: 12, Oldest: oldest,
			Values: map[string][]string{"owner": {"octocat"}, "full_name": {"octocat/hello-world"}},
		},
		"gh_code_scanning_alert_item": {
			Exists: true, Old: []string{"state", "reason"}, Rows: 40, Oldest: oldest,
			Values: map[string][]string{"owner": {"octocat"}, "full_name": {"octocat/hello-world", "octocat/gone"}},
		},
		"gh_actions_cache_entry": {Exists: true, Rows: 90, Oldest: oldest},
	}
}

// planConfig is a configuration with every sink that has something to plan,
// the SQL file in dir.
func planConfig(t *testing.T, dir string, tweak func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		GitHub:  config.GitHub{Token: "t"},
		Targets: config.Targets{User: "octocat"},
		Sinks: config.Sinks{
			Influx:        &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"},
			Loki:          &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"},
			Telegraf:      &config.TelegrafSink{URL: "http://telegraf:8186/telegraf"},
			Graphite:      &config.GraphiteSink{Addr: "graphite:2003"},
			SQL:           &config.SQLSink{Path: filepath.Join(dir, "points.sql")},
			Postgres:      &config.PostgresSink{DSN: "postgres://gh@db:5432/gh"},
			Elasticsearch: &config.ElasticsearchSink{URL: "http://es:9200"},
		},
	}
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// upgradeInput is the first -migrate after an upgrade from 2.6.1: a state
// file with a history of runs and no record of the stores, a SQL file on
// disk, and three stores that answer with the shapes given.
func upgradeInput(t *testing.T, influx, postgres, elastic map[string]teardown.Shape) Input {
	t.Helper()
	dir := t.TempDir()
	cfg := planConfig(t, dir, nil)
	if err := os.WriteFile(cfg.Sinks.SQL.Path, []byte("-- 2.6.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Input{
		Config: cfg, State: olderState(t, dir), Release: "2.6.2",
		Now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		Inspectors: []teardown.Inspector{
			&fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: influx},
			&fakeStore{name: "postgres", server: "PostgreSQL 18.6", shapes: postgres},
			&fakeStore{name: "elasticsearch", server: "Elasticsearch 9.5.3", shapes: elastic},
		},
		Repos: []string{"octocat/hello-world"}, ReposKnown: true,
	}
}

func itemOf(t *testing.T, p Plan, storeName, id string) Item {
	t.Helper()
	for _, st := range p.Stores {
		if st.Name != storeName {
			continue
		}
		for _, it := range st.Items {
			if it.Migration.ID == id {
				return it
			}
		}
	}
	t.Fatalf("the plan has no %s in %s", id, storeName)
	return Item{}
}

const (
	comments   = "2.6.1/gh_discussion_comment/is_answer"
	dependabot = "1.0.0/gh_dependabot_alert_item/state"
	scanning   = "1.0.0/gh_code_scanning_alert_item/state"
	caches     = "2.6.0/gh_actions_cache_entry/sum"
)

// TestAFreshInstallHasNothingToMigrate is every Action run and every first
// start: stores this release created, or none yet, and a state file with no
// history. Nothing is pending anywhere and the plan says so in one line.
func TestAFreshInstallHasNothingToMigrate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := planConfig(t, dir, nil)
	fresh := map[string]teardown.Shape{
		"gh_discussion_comment": {Exists: true, Rows: 5, Oldest: time.Now()},
	}
	in := Input{
		Config: cfg, State: run.LoadState(filepath.Join(dir, "state.json")), Release: "2.6.2",
		Inspectors: []teardown.Inspector{
			&fakeStore{name: "influxdb", server: "InfluxDB 3 Core 3.11.2", shapes: fresh},
			&fakeStore{name: "postgres", server: "PostgreSQL 18.6"},
			&fakeStore{name: "elasticsearch", server: "Elasticsearch 9.5.3"},
		},
	}
	p := Make(t.Context(), in)
	for _, st := range p.Stores {
		for _, it := range st.Items {
			if it.Status != NotNeeded {
				t.Errorf("%s in %s is %s (%s) on a fresh install", it.Migration.ID, st.Name, it.Status, it.Evidence)
			}
		}
	}
	var out bytes.Buffer
	p.Print(&out)
	if !strings.HasSuffix(out.String(), "Nothing to migrate. Nothing was changed.\n") {
		t.Errorf("a fresh install's plan ends:\n%s", out.String())
	}
}

// TestAStoreThatCanBeAskedDecidesItself: the old tag column in the store is
// pending, whatever the record says, and its absence is not needed; the
// refill is bounded by the oldest row the store held, not by the backfill's
// own bound.
func TestAStoreThatCanBeAskedDecidesItself(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), nil, nil)
	in.Config.Backfill.Since = "90d"
	p := Make(t.Context(), in)
	it := itemOf(t, p, "influxdb", comments)
	if it.Status != Pending || !it.Safe {
		t.Fatalf("the old comments in InfluxDB 3 are %s, safe %v: %v", it.Status, it.Safe, it.Unsafe)
	}
	if want := time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC); !it.Since.Equal(want) {
		t.Errorf("the refill is bounded by %s, want the oldest row's day %s", it.Since, want)
	}
	if !slices.Equal(it.Refill, []string{"discussions", "outbound"}) {
		t.Errorf("the refill reads %v, want both families that write the comments", it.Refill)
	}
	if got := itemOf(t, p, "influxdb", dependabot); got.Status != NotNeeded {
		t.Errorf("an alert table with no state tag is %s", got.Status)
	}
	scan := itemOf(t, p, "influxdb", scanning)
	if scan.Status != Pending || !slices.Contains(scan.Lost, "the rows of 1 repository this configuration no longer covers: octocat/gone") {
		t.Errorf("the pre-1.0 code scanning shape is %s, lost %v", scan.Status, scan.Lost)
	}
	if got := itemOf(t, p, "postgres", comments); got.Status != NotNeeded {
		t.Errorf("a PostgreSQL without the table is %s", got.Status)
	}
	if got := itemOf(t, p, "influxdb", caches); got.Status != Noted || got.Action != "nothing is changed" {
		t.Errorf("cache entries dated before 2.6.0 are %s, %q", got.Status, got.Action)
	}
}

// TestAStoreThatCannotBeAskedFollowsTheRecord: after an upgrade from a
// release that kept no record, a SQL file, a Graphite and a Telegraf may hold
// 2.6.0's shape, and never a pre-release one, which no release wrote.
func TestAStoreThatCannotBeAskedFollowsTheRecord(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, nil, nil, nil)
	in.Config.Backfill.Since = "2024-01-01"
	p := Make(t.Context(), in)
	for _, name := range []string{"sql", "graphite", "telegraf"} {
		if it := itemOf(t, p, name, comments); it.Status != Pending || it.Safe {
			t.Errorf("%s after an upgrade is %s, safe %v", name, it.Status, it.Safe)
		}
		for _, id := range []string{dependabot, scanning} {
			if it := itemOf(t, p, name, id); it.Status != NotNeeded {
				t.Errorf("%s in %s is %s: no release wrote that shape", id, name, it.Status)
			}
		}
	}
	sql := itemOf(t, p, "sql", comments)
	if !strings.Contains(sql.Action, `DROP TABLE IF EXISTS "gh_discussion_comment";`) ||
		!sql.Since.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the SQL file would %q, since %s", sql.Action, sql.Since)
	}
	graphite := itemOf(t, p, "graphite", comments)
	want := "find <storage>/whisper/github/discussion_comment -mindepth 11 -name '*.wsp' -delete"
	if len(graphite.Commands) == 0 || graphite.Commands[0] != want {
		t.Errorf("Graphite's command is %v, want %q", graphite.Commands, want)
	}
	if telegraf := itemOf(t, p, "telegraf", comments); len(telegraf.Refill) > 0 {
		t.Errorf("Telegraf would be refilled with %v, and its output may be a file a refill doubles", telegraf.Refill)
	}
	// Recorded as first written by this release, the same stores are clean.
	Stamp(in.State, in.Config, "2.6.2")
	for name, rec := range in.State.Stores {
		rec.FirstWrittenBy = "2.6.1"
		in.State.Stores[name] = rec
	}
	again := Make(t.Context(), in)
	for _, name := range []string{"sql", "graphite", "telegraf"} {
		if it := itemOf(t, again, name, comments); it.Status != NotNeeded {
			t.Errorf("%s first written by 2.6.1 is %s", name, it.Status)
		}
	}
}

// TestTheRecordSaysAppliedOnlyWhereTheStoreAgrees: applied and clean is
// applied; applied and holding the old shape again is pending, because the
// store is the authority where it can be asked. A record kept for another
// destination says nothing about this one.
func TestTheRecordSaysAppliedOnlyWhereTheStoreAgrees(t *testing.T) {
	t.Parallel()
	clean := map[string]teardown.Shape{"gh_discussion_comment": {Exists: true, Rows: 2}}
	in := upgradeInput(t, clean, oldShape(), nil)
	Stamp(in.State, in.Config, "2.6.2")
	at := time.Date(2026, 9, 28, 5, 35, 0, 0, time.UTC)
	for _, name := range []string{"influxdb", "postgres", "sql"} {
		in.State.Stores[name].Applied = map[string]time.Time{comments: at}
	}
	in.State.Stores["graphite"].Destination = "addr=elsewhere:2003 prefix=github"
	in.State.Stores["graphite"].Applied = map[string]time.Time{comments: at}
	p := Make(t.Context(), in)
	if it := itemOf(t, p, "influxdb", comments); it.Status != Applied {
		t.Errorf("applied and clean is %s", it.Status)
	}
	if it := itemOf(t, p, "postgres", comments); it.Status != Pending ||
		!strings.Contains(it.Evidence, "the old shape is back") {
		t.Errorf("applied and holding the old shape again is %s: %s", it.Status, it.Evidence)
	}
	if it := itemOf(t, p, "sql", comments); it.Status != Applied {
		t.Errorf("a SQL file recorded as applied is %s", it.Status)
	}
	if it := itemOf(t, p, "graphite", comments); it.Status != Pending {
		t.Errorf("a record for another Graphite made this one %s", it.Status)
	}
}

// TestNothingThatRunsWritesItAnyMore: the old rows of a measurement whose
// families are all off, or that the sink is told not to write, are history.
func TestNothingThatRunsWritesItAnyMore(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), oldShape(), nil)
	in.Config.Every = config.Every{Families: map[string]string{"outbound": "0", "discussions": "0", "security": "0"}}
	in.Config.Sinks.Influx.Exclude = nil
	if err := in.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	p := Make(t.Context(), in)
	if it := itemOf(t, p, "postgres", comments); it.Status != Frozen {
		t.Errorf("comments nobody writes are %s", it.Status)
	}
	half := upgradeInput(t, oldShape(), nil, nil)
	half.Config.Every = config.Every{Families: map[string]string{"discussions": "0"}}
	half.Config.Sinks.Influx.Exclude = []string{"gh_code_scanning_alert_item"}
	if err := half.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	p = Make(t.Context(), half)
	it := itemOf(t, p, "influxdb", comments)
	if it.Status != Pending || !slices.Equal(it.Refill, []string{"outbound"}) ||
		!slices.Contains(it.Lost, "the rows discussions wrote: this configuration does not run it") {
		t.Errorf("with discussions off the comments are %s, refilled by %v, lost %v", it.Status, it.Refill, it.Lost)
	}
	if got := itemOf(t, p, "influxdb", scanning); got.Status != Frozen {
		t.Errorf("a measurement the sink excludes is %s", got.Status)
	}
}

// TestAStoreSharedWithAnotherCollectorIsNotSafe: rows of an account this
// configuration does not collect would come back only when the other
// configuration refills them.
func TestAStoreSharedWithAnotherCollectorIsNotSafe(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape("octocat", "hubot"), nil, nil)
	it := itemOf(t, Make(t.Context(), in), "influxdb", comments)
	if it.Status != Pending || it.Safe || !slices.Equal(it.Others, []string{"hubot"}) {
		t.Errorf("a store shared with hubot is %s, safe %v, others %v", it.Status, it.Safe, it.Others)
	}
	in.ReposKnown = false
	scan := itemOf(t, Make(t.Context(), in), "influxdb", scanning)
	if scan.Safe || !strings.Contains(strings.Join(scan.Unsafe, " "), "repository list could not be read") {
		t.Errorf("owners compared with no repository list: safe %v, %v", scan.Safe, scan.Unsafe)
	}
}

// TestInfluxDB2IsNeverSafe: it can only delete, and the delete is final.
func TestInfluxDB2IsNeverSafe(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), nil, nil)
	in.Inspectors[0] = &fakeStore{name: "influxdb", server: "InfluxDB 2 OSS 2.7.12", shapes: oldShape()}
	it := itemOf(t, Make(t.Context(), in), "influxdb", comments)
	if it.Status != Pending || it.Safe || !strings.HasPrefix(it.Action, "delete every row") {
		t.Errorf("InfluxDB 2 is %s, safe %v, would %q", it.Status, it.Safe, it.Action)
	}
}

// TestAStoreThatDoesNotAnswerIsSaidAndAskedNothingMore.
func TestAStoreThatDoesNotAnswerIsSaidAndAskedNothingMore(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, nil, nil, nil)
	down := &fakeStore{name: "postgres", err: errors.New("dial tcp: connection refused")}
	in.Inspectors[1] = down
	p := Make(t.Context(), in)
	for _, st := range p.Stores {
		if st.Name == "postgres" && (st.Err == nil || len(st.Items) > 0) {
			t.Errorf("a store that did not answer planned %v", st.Items)
		}
	}
	if len(down.asked) > 0 {
		t.Errorf("a store that did not answer was asked about %v", down.asked)
	}
	var out bytes.Buffer
	p.Print(&out)
	if !strings.Contains(out.String(), "postgres did not answer") {
		t.Errorf("the summary does not say postgres did not answer:\n%s", out.String())
	}
}

// TestThePlanReadsAsWritten holds the text -migrate prints to a golden file:
// an upgrade from 2.6.0 with every kind of store.
func TestThePlanReadsAsWritten(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), oldShape("octocat", "hubot"), nil)
	in.Inspectors[2] = &fakeStore{name: "elasticsearch", err: errors.New("401 Unauthorized:\n missing authentication")}
	p := Make(t.Context(), in)
	var out bytes.Buffer
	p.Print(&out)
	got := strings.ReplaceAll(out.String(), filepath.Dir(in.Config.Sinks.SQL.Path), "<dir>")
	const golden = "testdata/plan.txt"
	if *updateIdentity {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the plan reads\n%s\nwant\n%s", got, want)
	}
}
