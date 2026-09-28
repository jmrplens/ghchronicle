package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// TestExecuteMigratePlansWhatItCannotCompareToo: a GitHub that refuses the
// token, or no token at all, still gives a plan, which says what it could
// not compare; the plan exits 0, since a dry run that found work to do has
// not failed; and the state file is neither created nor written. -migrate
// -yes with no token refuses before it touches anything.
func TestExecuteMigratePlansWhatItCannotCompareToo(t *testing.T) {
	dir := t.TempDir()
	sinks := "sinks:\n  sql:\n    path: " + filepath.Join(dir, "points.sql") + "\n"
	cfg := writeConfig(t, dir, refusingGitHub(t), sinks)
	got := runCommand(t, "-config", cfg, "-migrate")
	if got.status != notExited || !strings.Contains(got.stdout, "the repository list was not read (") ||
		!strings.Contains(got.stdout, "401 Unauthorized") ||
		!strings.HasSuffix(got.stdout, "Nothing to migrate. Nothing was changed.\n") {
		t.Errorf("-migrate against a refusing GitHub = %d:\n%s\n%s", got.status, got.stdout, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Errorf("a dry run left a state file behind: %v", err)
	}

	tokenless := filepath.Join(dir, "tokenless.yaml")
	if err := os.WriteFile(tokenless, []byte("targets:\n  user: octocat\nstate_file: "+
		filepath.Join(dir, "state.json")+"\n"+sinks), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	got = runCommand(t, "-config", tokenless, "-migrate")
	if got.status != notExited || !strings.Contains(got.stdout, "the configuration has no GitHub token") {
		t.Errorf("-migrate with no token = %d:\n%s\n%s", got.status, got.stdout, got.stderr)
	}

	got = runCommand(t, "-config", tokenless, "-migrate", "-yes")
	if got.status != 1 || got.stdout != "" || !strings.Contains(got.stderr, "no GitHub token; nothing was changed") {
		t.Errorf("-migrate -yes with no token = %d, %q, %q, want 1 and nothing applied", got.status, got.stdout, got.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("-migrate -yes with no token left files behind: %v", entries)
	}
}

// TestMigrateOthersGoesWithMigrate: the flag on its own, or on a sweep, is a
// command line that asks for nothing a run does, and exits 2 the way one
// that does not parse does.
func TestMigrateOthersGoesWithMigrate(t *testing.T) {
	for _, args := range [][]string{{"-migrate-others"}, {"-once", "-migrate-others"}, {"-yes", "-migrate-others"}} {
		got := runCommand(t, args...)
		if got.status != 2 || !strings.Contains(got.stderr, "-migrate-others goes with -migrate -yes") {
			t.Errorf("%v = %d, %q, want 2 and the reason", args, got.status, got.stderr)
		}
	}
}

// oldComments is an InfluxDB 3 holding gh_discussion_comment in 2.6.0's
// shape, is_answer a tag, with rows of the accounts named, and nothing else.
// It answers the catalog as 3.11.2 does and takes every write.
type oldComments struct {
	users []string
	mu    sync.Mutex
	asked []string
	// wrote is when the first write arrived.
	wrote time.Time
}

func (s *oldComments) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		s.asked = append(s.asked, r.Method+" "+r.URL.Path+" "+q)
		if r.URL.Path == "/api/v2/write" && s.wrote.IsZero() {
			s.wrote = time.Now()
		}
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/ping":
			_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
		case r.URL.Path == "/api/v2/write":
			w.WriteHeader(http.StatusNoContent)
		case !strings.Contains(q, "gh_discussion_comment"):
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(q, "information_schema.columns"):
			_, _ = io.WriteString(w, `[{"column_name":"comment","data_type":"Dictionary(Int32, Utf8)"},`+
				`{"column_name":"is_answer","data_type":"Dictionary(Int32, Utf8)"},`+
				`{"column_name":"user","data_type":"Dictionary(Int32, Utf8)"}]`)
		case strings.HasPrefix(q, "SELECT count(*)"):
			_, _ = io.WriteString(w, `[{"n":3,"oldest":"2023-11-14T22:13:20"}]`)
		case strings.HasPrefix(q, "SELECT DISTINCT"):
			rows := make([]map[string]string, 0, len(s.users))
			for _, u := range s.users {
				rows = append(rows, map[string]string{"v": u})
			}
			_ = json.NewEncoder(w).Encode(rows)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writes is how many writes the store took.
func (s *oldComments) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.asked {
		if strings.HasPrefix(a, "POST /api/v2/write") {
			n++
		}
	}
	return n
}

// recordingWays stands in for the ways each store is brought along: InfluxDB
// by an applier that notes when it ran, Graphite by the real one, and a
// refill that notes what it was handed.
type recordingWays struct {
	mu       sync.Mutex
	applied  []string
	at       time.Time
	refilled []string
	fail     error
	// noRefill installs no way to read the history again.
	noRefill bool
}

func (r *recordingWays) Apply(_ context.Context, it migrate.Item) (migrate.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return migrate.Outcome{}, r.fail
	}
	r.applied = append(r.applied, it.Migration.ID)
	r.at = time.Now()
	return migrate.Outcome{Did: "set aside", Aside: it.Migration.Measurement + "-20261001T091004"}, nil
}

// install puts r in the place of this build's ways for the length of the
// test.
func (r *recordingWays) install(t *testing.T) {
	t.Helper()
	previous := storeWays
	storeWays = func(migration) (map[string]migrate.Applier, migrate.Refiller) {
		ways := map[string]migrate.Applier{"influxdb": r, "graphite": migrate.Instructions{}}
		if r.noRefill {
			return ways, nil
		}
		return ways,
			func(_ context.Context, cleared []migrate.Chosen) error {
				r.mu.Lock()
				defer r.mu.Unlock()
				for _, c := range cleared {
					r.refilled = append(r.refilled, c.Store+":"+c.Item.Migration.ID)
				}
				return nil
			}
	}
	t.Cleanup(func() { storeWays = previous })
}

// upgradedConfig is a configuration that writes to the InfluxDB given and to
// a Graphite, beside a state file 2.6.1 left, with body after the sinks.
func upgradedConfig(t *testing.T, dir, github, influx, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return writeConfig(t, dir, github, fmt.Sprintf(`sinks:
  influxdb:
    url: %s
    bucket: github
  graphite:
    addr: %s
%s`, influx, graphiteReceiver(t), body))
}

const commentsID = "2.6.1/gh_discussion_comment/is_answer"

// TestAStartAppliesTheSafeOnesBeforeItsFirstSweep: after an upgrade from
// 2.6.1, a sweep's start sets InfluxDB's old comments aside, which is safe,
// before its first write, reads them again, and warns about Graphite's,
// which only its operator can remove, with the commands that apply it; then
// it sweeps, and lets go of the state file.
func TestAStartAppliesTheSafeOnesBeforeItsFirstSweep(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	store := &oldComments{users: []string{fakegh.Login}}
	ways := &recordingWays{}
	ways.install(t)
	dir := t.TempDir()
	cfg := upgradedConfig(t, dir, gh.URL(), store.start(t), "")

	got := runCommand(t, "-config", cfg, "-once")
	if got.status != notExited {
		t.Fatalf("-once = %d:\n%s", got.status, got.stderr)
	}
	if !slices.Equal(ways.applied, []string{commentsID}) || !slices.Equal(ways.refilled, []string{"influxdb:" + commentsID}) {
		t.Errorf("the start applied %v and read again %v, want the comments in InfluxDB", ways.applied, ways.refilled)
	}
	if store.wrote.IsZero() || !ways.at.Before(store.wrote) {
		t.Errorf("the set-aside at %v did not come before the first write at %v", ways.at, store.wrote)
	}
	for _, want := range []string{
		`level=WARN msg="applying a migration before the first sweep" sink=influxdb measurement=gh_discussion_comment migration=` + commentsID,
		`level=WARN msg="migration pending" sink=graphite measurement=gh_discussion_comment migration=` + commentsID,
		`plan="ghchronicle -config ` + cfg + ` -migrate" apply="ghchronicle -config ` + cfg + ` -migrate -yes"`,
		`level=INFO msg="migration applied" sink=influxdb`,
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("the log does not say %q:\n%s", want, got.stderr)
		}
	}
	if strings.Contains(got.stderr, `first="stop this service`) {
		t.Error("a one-shot run is told to stop the service it is not")
	}
	state := run.LoadState(filepath.Join(dir, "state.json"))
	if _, ok := state.Stores["influxdb"].Applied[commentsID]; !ok {
		t.Errorf("the migration applied is not recorded: %+v", state.Stores["influxdb"])
	}
	lock, err := run.TakeLock(filepath.Join(dir, "state-lock"), run.HeldByMigrate, "test", time.Now())
	if err != nil {
		t.Fatalf("the run kept the state file after it ended: %v", err)
	}
	_ = lock.Release()

	// The next start takes the record's word and asks the store nothing
	// about the comments, and applies nothing again.
	ways.applied = nil
	store.asked = nil
	if got = runCommand(t, "-config", cfg, "-once"); got.status != notExited || len(ways.applied) != 0 {
		t.Errorf("a second start = %d and applied %v", got.status, ways.applied)
	}
	for _, a := range store.asked {
		if strings.Contains(a, "gh_discussion_comment") {
			t.Errorf("the second start asked the store %q, which the record settles", a)
		}
	}
}

// TestAStartAppliesNothingItMayNot: under migrate: warn nothing is applied
// and the safe item is warned about with the setting as the reason; a store
// holding another account's rows is never applied by a start, and its
// command carries -migrate-others; and a one-shot run beside a service that
// holds the state file applies nothing and says who holds it.
func TestAStartAppliesNothingItMayNot(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	for _, tc := range []struct {
		name, body string
		users      []string
		hold       bool
		noRefill   bool
		want       string
	}{
		{"under warn", "migrate: warn\n", []string{fakegh.Login}, false, false, `not_applied="migrate: warn applies nothing on its own"`},
		{"shared", "", []string{fakegh.Login, "hubot"}, false, false, "-migrate -yes -migrate-others"},
		{"beside the service", "", []string{fakegh.Login}, true, false, "the state file is not this run's to change: process "},
		// A set-aside is safe only if the history comes back: a build with
		// no way to read it again leaves the store as it is.
		{"no refill", "", []string{fakegh.Login}, false, true, `not_applied="this build has no way to bring influxdb along on its own"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ways := &recordingWays{noRefill: tc.noRefill}
			ways.install(t)
			dir := t.TempDir()
			store := &oldComments{users: tc.users}
			cfg := upgradedConfig(t, dir, gh.URL(), store.start(t), tc.body)
			if tc.hold {
				lock, err := run.TakeLock(filepath.Join(dir, "state-lock"), run.HeldByService, "test", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Release() })
			}
			got := runCommand(t, "-config", cfg, "-once")
			if got.status != notExited || len(ways.applied) != 0 {
				t.Fatalf("-once = %d and applied %v:\n%s", got.status, ways.applied, got.stderr)
			}
			pending := ""
			for line := range strings.SplitSeq(got.stderr, "\n") {
				if strings.Contains(line, `msg="migration pending" sink=influxdb`) {
					pending = line
				}
			}
			if !strings.Contains(pending, tc.want) {
				t.Errorf("the InfluxDB item is said as %q, want it to carry %q", pending, tc.want)
			}
		})
	}
}

// TestMigrateYesAppliesEveryPendingOneAndHoldsBackAnotherAccounts: with the
// word given, the unsafe items are applied too, Graphite's by printing its
// commands, and each is recorded; a store holding another account's rows is
// held back, with exit 1, until -migrate-others is added; and a second run
// has nothing left to do.
func TestMigrateYesAppliesEveryPendingOneAndHoldsBackAnotherAccounts(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	ways := &recordingWays{}
	ways.install(t)
	dir := t.TempDir()
	store := &oldComments{users: []string{fakegh.Login, "hubot"}}
	cfg := upgradedConfig(t, dir, gh.URL(), store.start(t), "")

	got := runCommand(t, "-config", cfg, "-migrate", "-yes")
	if got.status != 1 || len(ways.applied) != 0 {
		t.Fatalf("-migrate -yes on a shared store = %d, applied %v:\n%s\n%s", got.status, ways.applied, got.stdout, got.stderr)
	}
	for _, want := range []string{
		"  held back   " + commentsID + " in influxdb: it holds rows of hubot, which this configuration does not " +
			"collect; -migrate-others applies it anyway\n",
		"  applied     " + commentsID + " in graphite: on the Graphite host, remove the old paths",
		"find <storage>/whisper/github/discussion_comment -mindepth 11 -name '*.wsp' -delete",
		"\n1 applied, 1 held back. Run the same command again",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("-migrate -yes does not say %q:\n%s", want, got.stdout)
		}
	}
	if store.writes() != 0 {
		t.Error("-migrate -yes wrote to the store it held back")
	}

	got = runCommand(t, "-config", cfg, "-migrate", "-yes", "-migrate-others")
	if got.status != notExited || !slices.Equal(ways.applied, []string{commentsID}) {
		t.Fatalf("-migrate -yes -migrate-others = %d, applied %v:\n%s\n%s", got.status, ways.applied, got.stdout, got.stderr)
	}
	if !strings.HasSuffix(got.stdout, "\n1 applied.\n") ||
		!strings.Contains(got.stdout, "the old rows are kept as gh_discussion_comment-20261001T091004") ||
		strings.Contains(got.stdout, "in graphite:") {
		t.Errorf("the second run says:\n%s", got.stdout)
	}
	state := run.LoadState(filepath.Join(dir, "state.json"))
	for _, store := range []string{"influxdb", "graphite"} {
		if _, ok := state.Stores[store].Applied[commentsID]; !ok {
			t.Errorf("%s is not recorded as applied: %+v", store, state.Stores[store])
		}
	}
}

// TestMigrateYesRefusesBesideTheProcessHoldingTheStateFile: with the service
// holding the state file, -migrate -yes names it and changes nothing: the
// store is asked nothing and the state file is as it was.
func TestMigrateYesRefusesBesideTheProcessHoldingTheStateFile(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	ways := &recordingWays{}
	ways.install(t)
	dir := t.TempDir()
	store := &oldComments{users: []string{fakegh.Login}}
	cfg := upgradedConfig(t, dir, gh.URL(), store.start(t), "")
	lock, err := run.TakeLock(filepath.Join(dir, "state-lock"), run.HeldByService, "2.6.2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	before, _ := os.ReadFile(filepath.Join(dir, "state.json"))

	got := runCommand(t, "-config", cfg, "-migrate", "-yes")
	if got.status != 1 || got.stdout != "" {
		t.Fatalf("-migrate -yes beside the service = %d:\n%s\n%s", got.status, got.stdout, got.stderr)
	}
	for _, want := range []string{
		fmt.Sprintf("process %d (ghchronicle 2.6.2, service, since", os.Getpid()),
		"nothing was changed", "stop it, or let it finish",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got.stderr)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if len(store.asked) != 0 || len(ways.applied) != 0 || string(before) != string(after) || len(gh.Requests()) != 0 {
		t.Errorf("a refused run asked the store %v, applied %v, asked GitHub %d times, and the state file changed: %v",
			store.asked, ways.applied, len(gh.Requests()), string(before) != string(after))
	}
}

// TestTheServiceHoldsTheStateFileAndWaitsForAMigration: a service started
// while -migrate -yes holds the state file waits for it, says so once, and
// starts when it lets go; while it runs it holds the file itself; and a
// second service on the same state file is refused.
func TestTheServiceHoldsTheStateFileAndWaitsForAMigration(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "sinks:\n  file:\n    path: "+filepath.Join(dir, "points.lp")+"\n")
	lockPath := filepath.Join(dir, "state-lock")
	migrating, err := run.TakeLock(lockPath, run.HeldByMigrate, "2.6.2", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	signalsFrom(ctx, t)
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		status := notExited
		previous := exitProcess
		exitProcess = func(code int) { status = code }
		defer func() { exitProcess = previous; done <- status }()
		execute([]string{"ghchronicle", "-config", cfg}, io.Discard, &stderr)
	}()
	if !waitUntil(func() bool {
		return strings.Contains(stderr.String(), "waiting for the process holding the state file")
	}) {
		t.Fatalf("the service did not say it was waiting:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "ghchronicle running") {
		t.Fatal("the service ran beside -migrate -yes")
	}
	if err = migrating.Release(); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(func() bool { return strings.Contains(stderr.String(), "sweep finished") }) {
		t.Fatalf("the service did not start once the state file was free:\n%s", stderr.String())
	}
	_, err = run.TakeLock(lockPath, run.HeldByMigrate, "2.6.2", time.Now())
	if held, ok := run.IsHeld(err); !ok || held.Holder.Run != run.HeldByService {
		t.Errorf("while the service runs the lock is %v, want held by the service", err)
	}
	second := runCommand(t, "-config", cfg)
	if second.status != 1 || !strings.Contains(second.stderr, "another ghchronicle service keeps the same state file") {
		t.Errorf("a second service = %d:\n%s", second.status, second.stderr)
	}
	cancel()
	select {
	case status := <-done:
		if status != notExited {
			t.Errorf("the service ended with %d:\n%s", status, stderr.String())
		}
	case <-time.After(time.Minute):
		t.Fatal("the service did not stop")
	}
	if strings.Count(stderr.String(), "waiting for the process holding the state file") != 1 {
		t.Errorf("the wait was said more than once:\n%s", stderr.String())
	}
}

// waitUntil polls cond for up to a minute.
func waitUntil(cond func() bool) bool {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestTheDryRunSaysHowToApplyWhatItFound: the plan ends with the exact
// command that applies it, and with what a start under the configured
// setting does about it.
func TestTheDryRunSaysHowToApplyWhatItFound(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	store := &oldComments{users: []string{fakegh.Login, "hubot"}}
	cfg := upgradedConfig(t, dir, gh.URL(), store.start(t), "migrate: warn\n")
	got := runCommand(t, "-config", cfg, "-migrate")
	if got.status != notExited {
		t.Fatalf("-migrate = %d:\n%s", got.status, got.stderr)
	}
	for _, want := range []string{
		"Nothing was changed.\n\nTo apply every pending one, with the service stopped:\n  ghchronicle -config " + cfg + " -migrate -yes\n",
		"unless -migrate-others is added too",
		"Under migrate: warn, the setting this configuration has, a start applies none of them",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the plan does not end with %q:\n%s", want, got.stdout)
		}
	}
}

// TestACommandLineNamesTheConfigurationSoTheShellReadsItBack: a path with a
// space or a quote in it is quoted, and a plain one is not.
func TestACommandLineNamesTheConfigurationSoTheShellReadsItBack(t *testing.T) {
	for path, want := range map[string]string{
		"/etc/ghchronicle/config.yaml": "ghchronicle -config /etc/ghchronicle/config.yaml -migrate",
		"/srv/my config.yaml":          "ghchronicle -config '/srv/my config.yaml' -migrate",
		"/srv/it's.yaml":               `ghchronicle -config '/srv/it'\''s.yaml' -migrate`,
	} {
		if got := commandLine(path, "-migrate"); got != want {
			t.Errorf("commandLine(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestEveryStoreThatCanBeClearedHasItsWay: this build clears InfluxDB,
// PostgreSQL and Elasticsearch itself, tells the SQL file to write the drop,
// and says what to do on the Graphite host and behind Telegraf; the stores
// that keep nothing a release could reshape have no way at all, so nothing
// is ever applied to them.
func TestEveryStoreThatCanBeClearedHasItsWay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		GitHub: config.GitHub{Token: "t"}, Targets: config.Targets{User: "octocat"},
		Sinks: config.Sinks{
			Influx:        &config.InfluxSink{URL: "http://influx:8181", Bucket: "github"},
			Postgres:      &config.PostgresSink{DSN: "postgres://gh@db:5432/gh"},
			Elasticsearch: &config.ElasticsearchSink{URL: "http://es:9200"},
			SQL:           &config.SQLSink{Path: filepath.Join(dir, "points.sql")},
			Graphite:      &config.GraphiteSink{Addr: "graphite:2003"},
			Telegraf:      &config.TelegrafSink{URL: "http://telegraf:8186/telegraf"},
			Loki:          &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"},
			File:          &config.FileSink{Path: filepath.Join(dir, "points.lp")},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sinks, _, err := buildSinks(cfg, slog.New(slog.DiscardHandler), true)
	if err != nil {
		t.Fatal(err)
	}
	ways, _ := storeWays(migration{cfg: cfg, sinks: sinks})
	names := slices.Sorted(maps.Keys(ways))
	if want := []string{"elasticsearch", "graphite", "influxdb", "postgres", "sql", "telegraf"}; !slices.Equal(names, want) {
		t.Errorf("ways for %v, want %v", names, want)
	}
	if c, ok := ways["postgres"].(migrate.Clearing); !ok || c.Forget == nil {
		t.Errorf("postgres is brought along by %T, without its sink told to forget the table", ways["postgres"])
	}
	if _, ok := ways["sql"].(migrate.Dropping); !ok {
		t.Errorf("the SQL file is brought along by %T", ways["sql"])
	}
}

// TestAMigrationAppliedEarlierIsForgottenByThisRunsLedger: a store cleared by
// -migrate -yes, in a process of its own that opened no ledger, is written
// again by the service that starts after it, for that measurement alone.
func TestAMigrationAppliedEarlierIsForgottenByThisRunsLedger(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	comment := sink.Point{
		Measurement: "gh_discussion_comment", Tags: map[string]string{"comment": "1"},
		Fields: map[string]any{"answers": 1}, Time: at,
	}
	repo := sink.Point{Measurement: "gh_repo", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"stars": 1}, Time: at}
	ledger := sink.LoadLedger("", 0, 0)
	for _, s := range []string{"influxdb", "postgres"} {
		_, commit := ledger.Reserve(s, []sink.Point{comment, repo})
		commit()
	}
	state := run.LoadState("")
	state.Stores["influxdb"] = &run.StoreRecord{Destination: "d"}
	state.Stores["influxdb"].MarkApplied(commentsID, at)
	saltLedger(ledger, state)
	if keep, _ := ledger.Reserve("influxdb", []sink.Point{comment, repo}); len(keep) != 1 || keep[0].Measurement != "gh_discussion_comment" {
		t.Errorf("influxdb is offered %v, want the comment alone", keep)
	}
	if keep, _ := ledger.Reserve("postgres", []sink.Point{comment, repo}); len(keep) != 0 {
		t.Errorf("postgres, never cleared, is offered %v", keep)
	}

	// In the same process: the item applied is forgotten there and then.
	state.Stores["postgres"] = &run.StoreRecord{Destination: "e"}
	state.Stores["postgres"].MarkApplied(commentsID, at)
	cfg := &config.Config{StateFile: filepath.Join(t.TempDir(), "state.json")}
	forgetCleared(migration{cfg: cfg, state: state, ledger: ledger, log: slog.New(slog.DiscardHandler)})(migrate.Chosen{
		Store: "postgres", Item: migrate.Item{Migration: migrate.Registry[slices.IndexFunc(migrate.Registry,
			func(m migrate.Migration) bool { return m.ID == commentsID })], Refill: []string{"outbound"}},
	})
	if keep, _ := ledger.Reserve("postgres", []sink.Point{comment, repo}); len(keep) != 1 || keep[0].Measurement != "gh_discussion_comment" {
		t.Errorf("postgres after its clearing is offered %v, want the comment alone", keep)
	}
}
