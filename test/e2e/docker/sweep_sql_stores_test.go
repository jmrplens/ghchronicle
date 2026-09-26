//go:build dockere2e

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/racereport"
)

// The sweep the InfluxDB and PostgreSQL assertions read back, and the fake
// GitHub it collects from.
//
// The fake is test/e2e/fakegh, shared with the suite one directory up rather
// than copied into this one. It used to be a copy, because the original lived
// in a _test.go file of package e2e that no other package can import, and the
// copy went stale: three GraphQL fragments and four REST paths were added to
// the original for branches, deployments, inventory and policy files, this
// copy never learned them, and every table behind those routes was missing
// from the containerized stores while the other suite passed.

// sqlStoresFixtures is the fixture set, which is test/e2e's own.
const sqlStoresFixtures = "../testdata"

const sqlStoresLogin = fakegh.Login

// sqlStoresFamilies is every family the sweep switches on, derived from
// internal/config in fakegh so that a family added to the runner is collected
// here without anybody remembering to add it.
var sqlStoresFamilies = fakegh.Families()

// newSQLStoresGitHub starts the shared fake on the fixture set above.
func newSQLStoresGitHub(tb testing.TB) *fakegh.Server {
	tb.Helper()
	return fakegh.New(tb, sqlStoresFixtures)
}

// sqlStoresPoint is one line of the file sink's JSON format. The sweep writes
// that file as well as to the stores, so an assertion can compare what a store
// holds against what the sweep actually emitted rather than against a guess.
type sqlStoresPoint struct {
	Time        string            `json:"time"`
	Measurement string            `json:"measurement"`
	Tags        map[string]string `json:"tags"`
	Fields      map[string]any    `json:"fields"`
}

// sqlStoresSweep is one finished sweep: where its artifacts are and what it
// logged.
type sqlStoresSweep struct {
	// Dir holds the config, the state file and the artifacts, under the
	// package's git-ignored out/ directory rather than a temporary one, so a
	// failing assertion can be read against the bytes that caused it.
	Dir string
	// SQL is the file the SQL sink wrote, ready to pipe into psql.
	SQL string
	// Points is the file sink's JSON, one point per line.
	Points string
	// Log is everything the binary printed.
	Log string
	// Database is the InfluxDB database the sweep wrote to.
	Database string
	// Started and Finished bracket the run. A point that carries the date of
	// the thing that happened falls outside that window; one that was stamped
	// when the sweep looked falls inside it, and that is the difference this
	// suite exists to measure.
	Started  time.Time
	Finished time.Time
}

var (
	sweepOnce   sync.Once
	sweepShared *sqlStoresSweep
	errSweep    error
)

// sqlStoresRun runs the one sweep this file's suites read back, once per test
// binary: InfluxDB, the SQL sink and the file sink, from the same fixtures, in
// the same pass, so what each store is asked about is the same data.
func sqlStoresRun(ctx context.Context, tb testing.TB, s *Stack) *sqlStoresSweep {
	tb.Helper()
	sweepOnce.Do(func() {
		sweepShared, errSweep = sqlStoresSweepInto(ctx, tb, s, "sweep", s.InfluxDatabase, time.Time{})
	})
	if errSweep != nil {
		tb.Fatalf("the sweep the assertions read back never finished: %v", errSweep)
	}
	return sweepShared
}

// sqlStoresSweepInto runs one sweep into a named working directory and a named
// InfluxDB database. Every caller past the first wants its own of both.
//
// fakeNow, when it is not zero, is the instant the fake's fixtures are dated
// from, instead of the moment each request arrives. A sweep that has to write
// what an earlier one wrote passes that one's start: the fake is new for every
// sweep and resolves its relative dates at request time, so without it a UTC
// midnight between the two moves every traffic day, run, commit and star onto
// a timestamp the first sweep never wrote.
func sqlStoresSweepInto(ctx context.Context, tb testing.TB, s *Stack, name, database string,
	fakeNow time.Time,
) (*sqlStoresSweep, error) {
	tb.Helper()
	gh := newSQLStoresGitHub(tb)
	if !fakeNow.IsZero() {
		gh.FreezeAt(fakeNow)
	}
	return sqlStoresSweepWith(ctx, tb, s, name, database, gh)
}

// sqlStoresSweepWith is the same sweep against a fake the caller started, for
// one that needs an account the base fixtures do not describe.
func sqlStoresSweepWith(ctx context.Context, tb testing.TB, s *Stack, name, database string,
	gh *fakegh.Server,
) (*sqlStoresSweep, error) {
	tb.Helper()
	dir, err := sqlStoresWorkDir(name)
	if err != nil {
		return nil, err
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		return nil, err
	}
	sweep := &sqlStoresSweep{
		Dir:      dir,
		SQL:      filepath.Join(dir, "points.sql"),
		Points:   filepath.Join(dir, "points.jsonl"),
		Database: database,
	}
	cfg, err := sqlStoresConfig(sweep, s, gh.URL())
	if err != nil {
		return nil, err
	}
	sweep.Started = time.Now().UTC()
	sweep.Log, err = sqlStoresExec(ctx, tb, binary, cfg)
	sweep.Finished = time.Now().UTC()
	// Kept beside the artifacts as well as returned, because the answer to
	// "why does the store not hold this" is usually in the sweep's own log and
	// a test failure prints at most the tail of it.
	if writeErr := os.WriteFile(filepath.Join(dir, "sweep.log"), []byte(sweep.Log), 0o600); writeErr != nil {
		return sweep, writeErr
	}
	if err != nil {
		return sweep, fmt.Errorf("%w\n%s", err, sweep.Log)
	}
	return sweep, nil
}

// sqlStoresWorkDir empties and returns one working directory. out/ is
// git-ignored and belongs to one run, and the stack's own three directories
// are elsewhere in it.
func sqlStoresWorkDir(name string) (string, error) {
	dir, err := filepath.Abs(filepath.Join("out", "sql-stores", name))
	if err != nil {
		return "", err
	}
	if removeErr := os.RemoveAll(dir); removeErr != nil {
		return "", removeErr
	}
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		return "", mkdirErr
	}
	return dir, nil
}

// sqlStoresBuild builds the collector. Every sweep builds its own rather than
// sharing one, because the Go build cache makes the second build free and a
// shared binary would need a lifetime nothing here has.
func sqlStoresBuild(ctx context.Context, dir string) (string, error) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "ghchronicle")
	// The arguments and the bound come from the race seam, so a `go test -race`
	// run builds an instrumented collector rather than driving an
	// uninstrumented one (harness_race_test.go).
	ctx, cancel := context.WithTimeout(ctx, collectorBuildTimeout)
	defer cancel()
	build := exec.CommandContext(ctx, goTool, collectorBuildArgs(binary)...)
	build.Dir = filepath.Join("..", "..", "..")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		return "", fmt.Errorf("go build: %w\n%s", buildErr, out)
	}
	return binary, nil
}

// sqlStoresConfig writes the config for one sweep.
//
// Deduplication is off, and that is the whole point of this suite rather than
// an incidental setting: the ledger would skip a point whose fields have not
// changed since the last write, so a second sweep would send InfluxDB almost
// nothing and could not prove that the columns it created the first time are
// still the columns it wants.
func sqlStoresConfig(sweep *sqlStoresSweep, s *Stack, githubURL string) (string, error) {
	var every strings.Builder
	for _, f := range sqlStoresFamilies {
		fmt.Fprintf(&every, "    %s: 1m\n", f)
	}
	body := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 30s
targets:
  user: %s
sinks:
  influxdb:
    url: %s
    token: %s
    org: e2e
    bucket: %s
  sql:
    dialect: postgres
    path: %s
  file:
    path: %s
    format: json
  dedupe_file: "off"
every:
  families:
%sstate_file: %s
log:
  level: debug
`, githubURL, sqlStoresLogin, s.InfluxURL, s.InfluxToken, sweep.Database,
		sweep.SQL, sweep.Points, every.String(), filepath.Join(sweep.Dir, "state.json"))

	path := filepath.Join(sweep.Dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// collectorEnviron is the environment every run of the collector gets: no
// proxy from the environment, no real token anywhere near this, and under
// -race the detector's settings (harness_race_test.go), last so that they win
// over a GORACE of the caller's shell: one naming a log_path would send the
// report to a file that racereport.Check never reads.
func collectorEnviron() []string {
	return slices.Concat(os.Environ(),
		[]string{"HTTPS_PROXY=", "HTTP_PROXY=", "NO_PROXY=*", "GITHUB_TOKEN="},
		raceEnviron())
}

// sqlStoresExec runs one sweep and returns everything it printed. A sink that
// could not write still exits zero, so the log is the result as much as the
// status is, and every caller reads it. A race report in it fails the test
// here, whatever the caller goes on to assert.
func sqlStoresExec(ctx context.Context, tb testing.TB, binary, cfg string) (string, error) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-config", cfg, "-once")
	cmd.Env = collectorEnviron()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	racereport.Check(tb, out.String())
	return out.String(), err
}

// sqlStoresPoints reads back what the sweep emitted.
func sqlStoresPoints(tb testing.TB, sweep *sqlStoresSweep) []sqlStoresPoint {
	tb.Helper()
	raw, err := os.ReadFile(sweep.Points)
	if err != nil {
		tb.Fatalf("the sweep wrote no points file: %v", err)
	}
	var points []sqlStoresPoint
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p sqlStoresPoint
		if decodeErr := json.Unmarshal([]byte(line), &p); decodeErr != nil {
			tb.Fatalf("point %d is not JSON: %v", len(points)+1, decodeErr)
		}
		points = append(points, p)
	}
	return points
}

// influxSQL asks InfluxDB 3 a question over its SQL endpoint and returns the
// rows. It is the read half of every InfluxDB assertion here: a write that the
// server answered 204 to is not yet a row anyone can read.
func influxSQL(ctx context.Context, s *Stack, database, query string) ([]map[string]any, error) {
	body, err := json.Marshal(map[string]string{
		"db": database, "q": query, "format": "json",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.InfluxURL+"/api/v3/query_sql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.InfluxToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query_sql: %s: %s", res.Status, bytes.TrimSpace(raw))
	}
	var rows []map[string]any
	if decodeErr := json.Unmarshal(raw, &rows); decodeErr != nil {
		return nil, fmt.Errorf("query_sql answered something that is not rows: %w\n%s", decodeErr, raw)
	}
	return rows, nil
}

// influxWriteLine writes line protocol straight to the store, bypassing the
// sink, and returns what the server answered. It is how a test asks the store
// itself a question the sink cannot pose.
func influxWriteLine(ctx context.Context, s *Stack, database, line string) (status int, body string, err error) {
	url := s.InfluxURL + "/api/v2/write?org=e2e&bucket=" + database + "&precision=ns"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(line))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Token "+s.InfluxToken)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, "", err
	}
	return res.StatusCode, string(bytes.TrimSpace(raw)), nil
}

// influxAwaitRow polls one query until it answers at least one row. A write
// InfluxDB accepted is readable within milliseconds, but "within milliseconds"
// is not "before the next statement", and a suite that assumes it flakes.
func influxAwaitRow(ctx context.Context, s *Stack, database, query string) ([]map[string]any, error) {
	var rows []map[string]any
	err := WaitUntil(ctx, "influxdb row", time.Minute, func(ctx context.Context) error {
		var err error
		rows, err = influxSQL(ctx, s, database, query)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return errors.New("no row yet")
		}
		return nil
	})
	return rows, err
}

// TestTheSweepCollectedEveryFamily is what the shared fake's route table is
// asserted through, and the reason the fake is shared at all.
//
// A family whose routes the fake does not answer is silent in exactly the same
// way as one that is switched off: the sweep runs it, every request four-oh-
// fours, the collector files that as "this repository has none" and writes no
// point, and the only visible symptom is a table the stores never create,
// three screens later, as eight panels rejected for naming something that does
// not exist. This asserts the sweep directly instead, so a forgotten route is
// one failure that names the family rather than a scattering that names the
// panels.
func TestTheSweepCollectedEveryFamily(t *testing.T) {
	s := Start(t)
	sweep := sqlStoresRun(t.Context(), t, s)
	points := sqlStoresPoints(t, sweep)

	seen := make(map[string]int, len(points))
	for _, p := range points {
		seen[p.Measurement]++
	}
	for _, family := range fakegh.Answering() {
		m := fakegh.Measurements[family]
		if seen[m] == 0 {
			t.Errorf("family %s produced no %s points: either it is not scheduled or the "+
				"fake answers none of the requests it makes", family, m)
		}
	}
	// Two-sided, so a family that starts answering stops being excused rather
	// than staying quietly on the list.
	for family, why := range fakegh.Unanswered {
		if m := fakegh.Measurements[family]; seen[m] > 0 {
			t.Errorf("family %s is listed as answering nothing (%s) and wrote %d %s points",
				family, why, seen[m], m)
		}
	}
	t.Logf("%d families collected, %d measurements", len(fakegh.Answering()), len(seen))
}
