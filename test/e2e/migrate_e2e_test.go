package e2e

import (
	"encoding/json"
	"io"
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

	"github.com/jmrplens/ghchronicle/v2"
)

// oldInflux is an InfluxDB 3 that answers the catalog the way 3.11.2 does
// for a store 2.6.0 wrote: gh_discussion_comment with is_answer as a tag.
// Anything it is sent is kept, so a test can hold a dry run to reading.
type oldInflux struct {
	mu   sync.Mutex
	sent []string
	// fresh answers as a store with no table at all.
	fresh bool
}

func (s *oldInflux) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		s.sent = append(s.sent, r.Method+" "+r.URL.Path+" "+q+string(body))
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/ping":
			_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
		case s.fresh || !strings.Contains(q, "'gh_discussion_comment'") && strings.Contains(q, "information_schema"):
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(q, "information_schema.columns"):
			_, _ = io.WriteString(w, `[{"column_name":"comment","data_type":"Dictionary(Int32, Utf8)"},`+
				`{"column_name":"is_answer","data_type":"Dictionary(Int32, Utf8)"},`+
				`{"column_name":"user","data_type":"Dictionary(Int32, Utf8)"}]`)
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
