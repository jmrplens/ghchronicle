package e2e

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2"
)

// oldInflux is an InfluxDB 3 that answers the catalog the way 3.11.2 does
// for a store 2.6.0 wrote: gh_discussion_comment with is_answer as a tag.
// Anything it is sent is kept, so a test can hold a dry run to reading.
//
// Once a delete sets the table aside it answers for the copy the way 3.11.2
// does, columns and items, and for the table the writes after it make, in
// the new shape and holding the comments written.
type oldInflux struct {
	mu   sync.Mutex
	sent []string
	// fresh answers as a store with no table at all.
	fresh bool
	// deleted is the name the table was given when a delete set it aside,
	// the way 3.11.2 renames it, and empty before.
	deleted string
	// written is every comment written since the delete.
	written map[string]bool
	// refuse fails the first write that carries it, and failed says it did.
	refuse string
	failed bool
}

// asideComments are the comments the copy holds: one GitHub serves again,
// and one it no longer does.
var asideComments = []string{"18283966", "999999"}

func (s *oldInflux) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sent = append(s.sent, r.Method+" "+r.URL.Path+" "+q+string(body))
		switch {
		case r.URL.Path == "/ping":
			_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
		case r.URL.Path == "/api/v2/write":
			s.write(w, string(body))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/configure/table" && !s.fresh && s.deleted == "":
			s.deleted = r.URL.Query().Get("table") + "-20261001T091004"
		case s.deleted != "":
			s.afterDelete(w, q)
		case strings.Contains(q, "information_schema.tables") && !s.fresh:
			_, _ = io.WriteString(w, `[{"table_name":"gh_repo"},{"table_name":"gh_discussion_comment"}]`)
		case s.fresh || !strings.Contains(q, "'gh_discussion_comment'") && strings.Contains(q, "information_schema"):
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(q, "information_schema.columns"):
			_, _ = io.WriteString(w, oldColumns)
		case strings.HasPrefix(q, "SELECT count(*)"):
			_, _ = io.WriteString(w, `[{"n":3,"oldest":"2023-11-14T22:13:20"}]`)
		case strings.HasPrefix(q, "SELECT DISTINCT"):
			_, _ = io.WriteString(w, `[{"v":"`+login+`"}]`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// oldColumns is the catalog of the table 2.6.0 wrote.
const oldColumns = `[{"column_name":"comment","data_type":"Dictionary(Int32, Utf8)"},` +
	`{"column_name":"is_answer","data_type":"Dictionary(Int32, Utf8)"},` +
	`{"column_name":"user","data_type":"Dictionary(Int32, Utf8)"}]`

// write takes a write, keeping the comments it carries once the table has
// been set aside.
func (s *oldInflux) write(w http.ResponseWriter, body string) {
	if s.refuse != "" && !s.failed && strings.Contains(body, s.refuse) {
		s.failed = true
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if s.deleted != "" {
		for line := range strings.SplitSeq(body, "\n") {
			if !strings.HasPrefix(line, "gh_discussion_comment,") {
				continue
			}
			_, rest, _ := strings.Cut(line, ",comment=")
			id, _, _ := strings.Cut(rest, ",")
			if s.written == nil {
				s.written = map[string]bool{}
			}
			s.written[id] = true
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// afterDelete answers the catalog and the items of the copy and of the table
// written since.
func (s *oldInflux) afterDelete(w http.ResponseWriter, q string) {
	live := len(s.written) > 0
	rows := func(values []string, key string) string {
		out := make([]string, 0, len(values))
		for _, v := range values {
			out = append(out, `{"`+key+`":"`+v+`"}`)
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	switch {
	case strings.Contains(q, "information_schema.tables"):
		tables := []string{"gh_repo", s.deleted}
		if live {
			tables = append(tables, "gh_discussion_comment")
		}
		_, _ = io.WriteString(w, rows(tables, "table_name"))
	case strings.Contains(q, "information_schema.columns") && strings.Contains(q, "'"+s.deleted+"'"):
		_, _ = io.WriteString(w, oldColumns)
	case strings.Contains(q, "information_schema.columns") && strings.Contains(q, "'gh_discussion_comment'") && live:
		_, _ = io.WriteString(w, `[{"column_name":"comment","data_type":"Dictionary(Int32, Utf8)"},`+
			`{"column_name":"user","data_type":"Dictionary(Int32, Utf8)"},{"column_name":"answers","data_type":"Int64"}]`)
	case strings.HasPrefix(q, `SELECT DISTINCT "comment" AS v0 FROM "`+s.deleted+`"`):
		_, _ = io.WriteString(w, rows(asideComments, "v0"))
	case strings.HasPrefix(q, `SELECT DISTINCT "comment" AS v0 FROM "gh_discussion_comment"`):
		_, _ = io.WriteString(w, rows(slices.Sorted(maps.Keys(s.written)), "v0"))
	case strings.HasPrefix(q, "SELECT count(*)") && live:
		_, _ = io.WriteString(w, `[{"n":`+strconv.Itoa(len(s.written))+`,"oldest":"2024-01-01T00:00:00"}]`)
	case strings.HasPrefix(q, "SELECT DISTINCT") && live:
		_, _ = io.WriteString(w, `[{"v":"`+login+`"}]`)
	default:
		_, _ = io.WriteString(w, `[]`)
	}
}

// writes is every line written to the store, in order.
func (s *oldInflux) writes() []string {
	var out []string
	for _, r := range s.requests() {
		if body, ok := strings.CutPrefix(r, "POST /api/v2/write "); ok {
			out = append(out, strings.Split(strings.TrimSpace(body), "\n")...)
		}
	}
	return out
}

// requests is everything the store was sent.
func (s *oldInflux) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// migrateConfig is a configuration with the InfluxDB above, a SQL file and a
// Graphite nobody listens for, in dir.
func migrateConfig(t *testing.T, dir, github, influx string) string {
	t.Helper()
	return writeSinkConfig(t, dir, github, `  influxdb:
    url: `+influx+`
    bucket: github
  sql:
    path: `+filepath.Join(dir, "points.sql")+`
  graphite:
    addr: 127.0.0.1:1`)
}

// snapshot is every file in dir and its contents. The tests keep everything
// they write in dir itself, so one level is all of it.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("%s holds a directory, %s, which a snapshot one level deep would not see", dir, e.Name())
		}
		out[e.Name()] = readFile(t, filepath.Join(dir, e.Name()))
	}
	return out
}

// TestMigrateIsADryRunThatChangesNothing is -migrate after an upgrade from
// 2.6.0: a state file with a history and no record of the stores, a SQL file
// on disk, and an InfluxDB holding the old shape. It prints what is pending
// in each, sends the store nothing but reads, asks GitHub for the repository
// list alone, and leaves every file as it was.
func TestMigrateIsADryRunThatChangesNothing(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	store := &oldInflux{}
	dir := t.TempDir()
	cfg := migrateConfig(t, dir, gh.URL(), store.start(t))
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "points.sql"), []byte("-- written by 2.6.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)

	out, err := run(t, time.Minute, "-config", cfg, "-migrate")
	if err != nil {
		t.Fatalf("-migrate failed: %v\n%s", err, out)
	}
	plan := string(out)
	for _, want := range []string{
		"(InfluxDB 3 Core 3.11.2)",
		"  pending     2.6.1/gh_discussion_comment/is_answer\n",
		"rows of gh_discussion_comment carry is_answer as a tag: 3 rows, the oldest dated 2023-11-14",
		"read again: discussions and outbound, since 2023-11-14, writing gh_discussion_comment only",
		"safe to apply unattended",
		"written before the state file kept a record of which release first wrote it",
		"find <storage>/whisper/github/discussion_comment -mindepth 11 -name '*.wsp' -delete",
		`write DROP TABLE IF EXISTS "gh_discussion_comment"; into the file`,
		"Nothing was changed.",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan does not say %q:\n%s", want, plan)
		}
	}
	for _, r := range store.requests() {
		method, rest, _ := strings.Cut(r, " ")
		_, q, _ := strings.Cut(rest, " ")
		if method != http.MethodGet || q != "" && !strings.HasPrefix(q, "SELECT ") {
			t.Errorf("a dry run sent the store %s", r)
		}
	}
	for _, r := range gh.Requests() {
		if r.Method != http.MethodGet || r.Path != "/user/repos" {
			t.Errorf("a dry run asked GitHub %s %s, want the repository list alone", r.Method, r.Path)
		}
	}
	if after := snapshot(t, dir); !maps.Equal(before, after) {
		t.Errorf("a dry run changed the files beside it:\nbefore %v\nafter  %v", fileNames(before), fileNames(after))
	}
}

// TestMigrateOnAFreshInstallHasNothingToDo: no state file, no SQL file yet
// and a store with no table. Nothing is pending, and nothing is created.
func TestMigrateOnAFreshInstallHasNothingToDo(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	store := &oldInflux{fresh: true}
	dir := t.TempDir()
	cfg := migrateConfig(t, dir, gh.URL(), store.start(t))
	before := snapshot(t, dir)
	out, err := run(t, time.Minute, "-config", cfg, "-migrate")
	if err != nil {
		t.Fatalf("-migrate failed: %v\n%s", err, out)
	}
	if !strings.HasSuffix(string(out), "Nothing to migrate. Nothing was changed.\n") ||
		strings.Contains(string(out), "pending") {
		t.Errorf("a fresh install's plan:\n%s", out)
	}
	if after := snapshot(t, dir); !maps.Equal(before, after) {
		t.Errorf("a dry run created %v", fileNames(after))
	}
}

// TestAStartRecordsWhichReleaseFirstWroteEachStore: the first run on a new
// state file records this release as the first writer of every store that
// cannot be asked, which is what keeps a later -migrate from reading a store
// this release created as one an earlier release may have written. A state
// file 2.6.1 left, with a history and no record, gets a record that says the
// first writer is not known.
func TestAStartRecordsWhichReleaseFirstWroteEachStore(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	for _, c := range []struct {
		name, state, want string
	}{
		{"fresh", "", ghchronicle.Version},
		{"upgraded", `{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			if c.state != "" {
				if err := os.WriteFile(statePath, []byte(c.state), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := writeSinkConfig(t, dir, gh.URL(), "  graphite:\n    addr: 127.0.0.1:1\n  file:\n    path: "+
				filepath.Join(dir, "points.lp"))
			if out, err := run(t, 2*time.Minute, "-config", cfg, "-once"); err != nil {
				t.Fatalf("-once failed: %v\n%s", err, out)
			}
			var saved struct {
				Stores map[string]struct {
					Destination    string `json:"destination"`
					FirstWrittenBy string `json:"first_written_by"`
					WrittenBy      string `json:"written_by"`
				} `json:"stores"`
			}
			if err := json.Unmarshal([]byte(readFile(t, statePath)), &saved); err != nil {
				t.Fatal(err)
			}
			rec, ok := saved.Stores["graphite"]
			if !ok || rec.FirstWrittenBy != c.want || rec.WrittenBy != ghchronicle.Version ||
				rec.Destination != "addr=127.0.0.1:1 prefix=github" {
				t.Errorf("graphite is recorded as %+v, want first written by %q and last by %s",
					rec, c.want, ghchronicle.Version)
			}
			if _, kept := saved.Stores["file"]; kept {
				t.Error("the file sink keeps nothing a release could reshape, yet has a record")
			}
		})
	}
}

// fileNames is the names of the files in a snapshot, for a failure message.
func fileNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, filepath.Base(k))
	}
	slices.Sort(out)
	return out
}

// TestTheServiceKeepsMigrateYesAway runs the two as two processes, which is
// what the lock is for: while the service runs, -migrate -yes names it by
// process and changes nothing; once it has stopped, the same command runs.
func TestTheServiceKeepsMigrateYesAway(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), "  file:\n    path: "+filepath.Join(dir, "points.lp"))
	service := serveInBackground(t, cfg)
	awaitSweep(t, service, 2*time.Minute)

	stdout, stderr, err := runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err == nil || stdout != "" {
		t.Fatalf("-migrate -yes beside the service succeeded:\n%s\n%s", stdout, stderr)
	}
	for _, want := range []string{
		"process " + strconv.Itoa(service.cmd.Process.Pid) + " (ghchronicle " + ghchronicle.Version + ", service, since",
		"nothing was changed",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, stderr)
		}
	}

	service.Stop()
	stdout, stderr, err = runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err != nil || !strings.HasSuffix(stdout, "\nNothing to migrate.\n") {
		t.Errorf("-migrate -yes once the service stopped: %v\n%s\n%s", err, stdout, stderr)
	}
}

// TestUnderWarnAStartOnlyAsks: after an upgrade from 2.6.0, a start under
// migrate: warn says what is pending in each store with the commands that
// apply it, sends the store nothing but reads before its writes, and
// changes nothing.
func TestUnderWarnAStartOnlyAsks(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	store := &oldInflux{}
	dir := t.TempDir()
	cfg := migrateConfig(t, dir, gh.URL(), store.start(t))
	appendToConfig(t, cfg, "migrate: warn\n")
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "points.sql"), []byte("-- written by 2.6.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, log, err := runSplit(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("-once failed: %v\n%s", err, log)
	}
	for _, store := range []string{"influxdb", "sql", "graphite"} {
		want := `msg="migration pending" sink=` + store + ` measurement=gh_discussion_comment migration=2.6.1/gh_discussion_comment/is_answer`
		if !strings.Contains(log, want) {
			t.Errorf("the start does not say %s is pending:\n%s", store, log)
		}
	}
	for _, want := range []string{
		`not_applied="migrate: warn applies nothing on its own"`,
		`plan="ghchronicle -config ` + cfg + ` -migrate" apply="ghchronicle -config ` + cfg + ` -migrate -yes"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the start does not say %q:\n%s", want, log)
		}
	}
	for _, r := range store.requests() {
		method, rest, _ := strings.Cut(r, " ")
		path, q, _ := strings.Cut(rest, " ")
		read := method == http.MethodGet && (q == "" || strings.HasPrefix(q, "SELECT "))
		if !read && (method != http.MethodPost || path != "/api/v2/write") {
			t.Errorf("a start under warn sent the store %.200s", r)
		}
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "points.sql")), "DROP TABLE") {
		t.Error("a start under warn dropped a table in the SQL file")
	}
}

// TestMigrateYesSaysWhatTelegrafNeedsAndRecordsIt: the store behind a
// Telegraf is out of reach, so -migrate -yes applies its change by saying
// what to do there, records it, sends Telegraf nothing, and has nothing left
// to do the next time.
func TestMigrateYesSaysWhatTelegrafNeedsAndRecordsIt(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	telegraf := newCapture(t, nil)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), "  telegraf:\n    url: "+telegraf.URL())
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err != nil {
		t.Fatalf("-migrate -yes failed: %v\n%s\n%s", err, stdout, stderr)
	}
	want := "  applied     2.6.1/gh_discussion_comment/is_answer in telegraf: drop gh_discussion_comment in the " +
		"store behind Telegraf, then read discussions and outbound again through it with -backfill -families discussions,outbound\n"
	if !strings.Contains(stdout, want) || !strings.HasSuffix(stdout, "\n1 applied.\n") {
		t.Errorf("-migrate -yes says:\n%s", stdout)
	}
	if n := telegraf.Count(); n != 0 {
		t.Errorf("Telegraf was sent %d requests", n)
	}
	stdout, stderr, err = runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err != nil || !strings.HasSuffix(stdout, "\nNothing to migrate.\n") ||
		!strings.Contains(stdout, "applied     2.6.1/gh_discussion_comment/is_answer: applied on ") {
		t.Errorf("the second -migrate -yes: %v\n%s\n%s", err, stdout, stderr)
	}
}

// clearedConfig is a configuration with the InfluxDB given, a SQL file and a
// file sink, in dir, beside a state file 2.6.0 left. The file sink keeps
// nothing a release could reshape, and a refill must not write to it.
func clearedConfig(t *testing.T, dir, github, influx string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return writeSinkConfig(t, dir, github, `  influxdb:
    url: `+influx+`
    bucket: github
  sql:
    path: `+filepath.Join(dir, "points.sql")+`
  file:
    path: `+filepath.Join(dir, "points.lp"))
}

// TestMigrateYesClearsOnlyTheMeasurementItNames: after an upgrade from
// 2.6.0, -migrate -yes deletes the one InfluxDB table, which InfluxDB keeps
// aside, and records the copy; writes the drop of that one table into the
// SQL file after everything already in it; reads the comments back from
// both families that write them, writing them and nothing else into those
// two stores and nothing into the file sink; says which comment GitHub no
// longer serves; and sends the store nothing else that changes it. A second
// run finds the store cleared and owing nothing, and does nothing.
func TestMigrateYesClearsOnlyTheMeasurementItNames(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	store := &oldInflux{}
	dir := t.TempDir()
	cfg := clearedConfig(t, dir, gh.URL(), store.start(t))
	statePath, sqlPath := filepath.Join(dir, "state.json"), filepath.Join(dir, "points.sql")
	const earlier = "-- written by 2.6.0\nINSERT INTO \"gh_repo\" VALUES (1);\n"
	if err := os.WriteFile(sqlPath, []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err != nil {
		t.Fatalf("-migrate -yes: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		"  applied     2.6.1/gh_discussion_comment/is_answer in influxdb: InfluxDB set the table gh_discussion_comment aside",
		"the old rows are kept as gh_discussion_comment-20261001T091004",
		"  applied     2.6.1/gh_discussion_comment/is_answer in sql: wrote DROP TABLE IF EXISTS \"gh_discussion_comment\";",
		// The SQL file's bound is backfill.since, unset here, and the walk
		// reads as far back as the furthest any store is owed.
		"  refill      read discussions and outbound again, with no bound, writing gh_discussion_comment to " +
			"influxdb and sql\n",
		"  reconciled  gh_discussion_comment in influxdb: 2 items in gh_discussion_comment-20261001T091004, 6 now; " +
			"1 GitHub no longer serves, whose rows are only in the copy until it is purged: 999999\n",
		"\n2 applied.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("-migrate -yes does not say %q:\n%s\n%s", want, stdout, stderr)
		}
	}
	store.clearedAndRefilled(t)
	refilledAfterTheDrop(t, readFile(t, sqlPath), earlier)
	if _, err = os.Stat(filepath.Join(dir, "points.lp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file sink was written to by a refill: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "state-refill.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refill ended complete and left its checkpoint: %v", err)
	}
	var saved struct {
		Stores map[string]struct {
			Applied  map[string]time.Time `json:"applied"`
			Refill   json.RawMessage      `json:"refill"`
			SetAside []struct {
				Name, Migration string
				ByServer        bool `json:"by_server"`
			} `json:"set_aside"`
		} `json:"stores"`
	}
	if err = json.Unmarshal([]byte(readFile(t, statePath)), &saved); err != nil {
		t.Fatal(err)
	}
	influx := saved.Stores["influxdb"]
	if _, ok := influx.Applied["2.6.1/gh_discussion_comment/is_answer"]; !ok || len(influx.SetAside) != 1 ||
		influx.SetAside[0].Name != "gh_discussion_comment-20261001T091004" || !influx.SetAside[0].ByServer {
		t.Errorf("the record of influxdb is %+v", influx)
	}
	for name, rec := range saved.Stores {
		if rec.Refill != nil {
			t.Errorf("%s is still owed a refill: %s", name, rec.Refill)
		}
	}

	store.mu.Lock()
	store.sent = nil
	store.mu.Unlock()
	stdout, _, _ = runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if strings.Contains(stdout, "  applied     2.6.1/gh_discussion_comment/is_answer in") ||
		strings.Contains(stdout, "  refill") {
		t.Errorf("the second run applied or read again:\n%s", stdout)
	}
	for _, r := range store.requests() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("the second run sent the store %.200s", r)
		}
	}
	if strings.Count(readFile(t, sqlPath), "DROP TABLE") != 1 {
		t.Error("the second run wrote the drop again")
	}
}

// clearedAndRefilled holds the store to the one delete, and to the comments
// both families write and nothing else either collects: the account's own
// anywhere, from outbound, and every comment and reply on the threads of its
// forums, from discussions.
func (s *oldInflux) clearedAndRefilled(t *testing.T) {
	t.Helper()
	var changed []string
	for _, r := range s.requests() {
		method, rest, _ := strings.Cut(r, " ")
		path, q, _ := strings.Cut(rest, " ")
		if path != "/api/v2/write" && (method != http.MethodGet || q != "" && !strings.HasPrefix(q, "SELECT ")) {
			changed = append(changed, r)
		}
	}
	if !slices.Equal(changed, []string{"DELETE /api/v3/configure/table "}) {
		t.Errorf("the store was sent %q, want the one delete", changed)
	}
	for _, line := range s.writes() {
		if !strings.HasPrefix(line, "gh_discussion_comment,") {
			t.Errorf("the refill wrote %.120s", line)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := slices.Sorted(maps.Keys(s.written)); !slices.Equal(got, []string{
		"18283966", "18283967", "18300001", "18300002", "18300003", "7654321",
	}) {
		t.Errorf("the refill wrote the comments %v", got)
	}
}

// refilledAfterTheDrop holds a SQL file to what it held, the drop, and the six
// comments read back, and nothing else.
func refilledAfterTheDrop(t *testing.T, sql, earlier string) {
	t.Helper()
	after, dropped := strings.CutPrefix(sql, earlier+"DROP TABLE IF EXISTS \"gh_discussion_comment\";\n")
	if !dropped {
		t.Errorf("the SQL file does not start with what it held and the drop:\n%.400s", sql)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(after), "\n") {
		if !strings.Contains(line, `"gh_discussion_comment"`) {
			t.Errorf("after the drop the SQL file holds %.120s", line)
		}
	}
	if got := strings.Count(after, "INSERT INTO"); got != 6 {
		t.Errorf("the SQL file got %d comments back, want 6", got)
	}
}

// TestARefillCutShortResumesFromItsOwnCheckpoint: a store that refuses the
// comments of one family leaves the refill owed, with the family that was
// written in the refill's own checkpoint and not in a backfill's. The next
// -migrate -yes, with nothing left to apply, pays what is owed: it walks the
// family that was not written and not the one that was, and then owes
// nothing.
func TestARefillCutShortResumesFromItsOwnCheckpoint(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	store := &oldInflux{refuse: "comment=18300001"}
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), "  influxdb:\n    url: "+store.start(t)+"\n    bucket: github")
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err == nil || !strings.Contains(stdout, "  refill      reading the history again did not finish, and is still owed: "+
		"discussions did not reach the end of what it reads") {
		t.Fatalf("a refill a store refused: %v\n%s\n%s", err, stdout, stderr)
	}
	refillLeftOwed(t, dir, cfg)

	store.mu.Lock()
	store.sent = nil
	store.mu.Unlock()
	stdout, stderr, err = runSplit(t, time.Minute, "-config", cfg, "-migrate", "-yes")
	if err != nil || !strings.Contains(stdout, "  refill      read discussions and outbound again") ||
		!strings.HasSuffix(stdout, "\nNothing to apply, and the refill owed was read.\n") {
		t.Fatalf("the second -migrate -yes: %v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, `msg="family already written by the walk this resumes" run=refill family=outbound`) {
		t.Errorf("the resume walked outbound again:\n%s", stderr)
	}
	for _, line := range store.writes() {
		if !strings.Contains(line, "full_name=octocat/hello-world,") || !strings.Contains(line, "number=12,") {
			t.Errorf("the resume wrote %.120s, which is not a comment of the thread discussions reads", line)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "state-refill.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refill ended and left its checkpoint: %v", err)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "state.json")), `"refill"`) {
		t.Error("the state file still says a refill is owed")
	}
}

// refillLeftOwed holds what a refill cut short leaves: its own checkpoint
// with the family it wrote, no backfill's, and a status and a plan that say
// it is owed.
func refillLeftOwed(t *testing.T, dir, cfg string) {
	t.Helper()
	checkpoint := readFile(t, filepath.Join(dir, "state-refill.json"))
	for _, want := range []string{`"family": "outbound"`, `"influxdb": [`, `"gh_discussion_comment"`} {
		if !strings.Contains(checkpoint, want) {
			t.Errorf("the refill's checkpoint lacks %s:\n%s", want, checkpoint)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state-progress.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refill wrote a backfill's checkpoint: %v", err)
	}
	status, _, err := runSplit(t, time.Minute, "-config", cfg, "-backfill-status")
	if err != nil || !strings.Contains(status, "refill in progress") ||
		!strings.Contains(status, "writing      gh_discussion_comment to influxdb") ||
		!strings.Contains(status, "left         discussions") {
		t.Errorf("-backfill-status: %v\n%s", err, status)
	}
	plan, _, _ := runSplit(t, time.Minute, "-config", cfg, "-migrate")
	if !strings.Contains(plan, "  refill owed discussions and outbound, since 2023-11-14, writing gh_discussion_comment: "+
		"cleared by 2.6.1/gh_discussion_comment/is_answer and not read back yet") ||
		!strings.HasSuffix(plan, "\n1 store owed a refill. Nothing was changed.\n") {
		t.Errorf("the plan does not say the refill is owed:\n%s", plan)
	}
}

// TestTheServiceReadsTheHistoryBackBeforeItsFirstSweep: under migrate:
// auto, the default, a service started after an upgrade from 2.6.0 sets the
// comments aside, which is safe, reads them back before its first sweep, and
// only then sweeps: every comment the refill writes reaches the store before
// the first row of any other measurement.
func TestTheServiceReadsTheHistoryBackBeforeItsFirstSweep(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	store := &oldInflux{}
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), "  influxdb:\n    url: "+store.start(t)+"\n    bucket: github")
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := serveInBackground(t, cfg)
	// The service's own sweep: the refill says "sweep finished" too, as the
	// backfill it is, under run=refill.
	waitFor(2*time.Minute, func() bool {
		return strings.Contains(service.Output(), "msg=\"sweep finished\"\n") || service.Exited()
	})
	log := service.Stop()
	order := []string{
		`msg="applying a migration before the first sweep" sink=influxdb`,
		`msg="refill starting" families=discussions,outbound measurements=gh_discussion_comment sinks=influxdb since=2023-11-14`,
		`msg="refill complete"`,
		`msg=reconciled sink=influxdb measurement=gh_discussion_comment`,
		`msg="ghchronicle running"`,
	}
	at := 0
	for _, want := range order {
		i := strings.Index(log[at:], want)
		if i < 0 {
			t.Fatalf("the log does not say %q after %q:\n%s", want, log[:at], log)
		}
		at += i
	}
	wrote := store.writes()
	first := slices.IndexFunc(wrote, func(line string) bool { return !strings.HasPrefix(line, "gh_discussion_comment,") })
	if first < 6 {
		t.Errorf("the sweep wrote %.80s before the refill had written the six comments", wrote[max(first, 0)])
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "state.json")), `"refill"`) {
		t.Error("the state file still says a refill is owed")
	}
}

// appendToConfig adds top-level settings to a configuration file.
func appendToConfig(t *testing.T, path, lines string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(lines); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
