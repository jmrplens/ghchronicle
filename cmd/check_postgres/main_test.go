package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDDL pins the statements the sink's schema is declared with: tags first
// and keyed, then the fields by name and type, tables in name order, and the
// tables InfluxDB keeps for itself left out.
func TestDDL(t *testing.T) {
	t.Parallel()
	tables := map[string][][]string{
		"gh_repo": {
			{"stars", "Int64"},
			{"time", "Timestamp(Nanosecond, None)"},
			{"repo", "Dictionary(Int32, Utf8)"},
			{"archived", "Boolean"},
			{"owner", "Dictionary(Int32, Utf8)"},
			{"ratio", "Float64"},
			{"license", "Utf8"},
		},
		"gh_commit": {
			{"time", "Timestamp(Nanosecond, None)"},
			{"additions", "Int64"},
		},
		"gh_repo-renamed": {{"stars", "Int64"}},
		"system_tables":   {{"name", "Utf8"}},
	}
	got, err := ddl(tables)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`CREATE TABLE "gh_commit" ("time" TIMESTAMPTZ NOT NULL, "additions" BIGINT, PRIMARY KEY ("time"));`,
		`CREATE TABLE "gh_repo" ("time" TIMESTAMPTZ NOT NULL, "owner" TEXT NOT NULL DEFAULT '', ` +
			`"repo" TEXT NOT NULL DEFAULT '', "archived" BOOLEAN, "license" TEXT, "ratio" DOUBLE PRECISION, ` +
			`"stars" BIGINT, PRIMARY KEY ("time", "owner", "repo"));`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("ddl =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestDDLRefusesWhatItCannotType fails a dump the sink could not have written,
// rather than declaring a schema that differs from the real one.
func TestDDLRefusesWhatItCannotType(t *testing.T) {
	t.Parallel()
	for name, cols := range map[string][][]string{
		"a column that is not a pair":  {{"stars"}},
		"a type the sink never writes": {{"stars", "UInt64"}},
	} {
		if _, err := ddl(map[string][][]string{"gh_repo": cols}); err == nil {
			t.Errorf("%s: ddl accepted it", name)
		}
	}
}

// standin is the stand-in for `sudo -u postgres psql` TestMain builds out of
// testdata/sudo.
var standin string

// standinName is the name the stand-in has to have for PATH to find it as
// sudo: a bare name everywhere but Windows, where exec.LookPath only finds a
// program through an extension PATHEXT lists.
func standinName() string {
	if runtime.GOOS == "windows" {
		return "sudo.exe"
	}
	return "sudo"
}

// TestMain builds the stand-in once for every test that needs one. It is a
// program of its own rather than a shell script because what is under test is
// the order in which psql's two streams reach this command, and a Go program
// writes each line with one system call, exactly when it means to.
func TestMain(m *testing.M) {
	os.Exit(runWithStandin(m))
}

// runWithStandin builds the stand-in, runs the tests and cleans up after them.
func runWithStandin(m *testing.M) int {
	dir, err := os.MkdirTemp("", "check-postgres-standin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create a build directory for the stand-in: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Bounded, because the test timeout cannot stop a build started before
	// m.Run.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	path := filepath.Join(dir, standinName())
	build := exec.CommandContext(ctx, "go", "build", "-o", path, "./testdata/sudo")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "build testdata/sudo: %v\n%s", buildErr, out)
		return 1
	}
	standin = path
	return m.Run()
}

// fakePsql puts the stand-in first and alone on PATH under the name sudo, and
// tells it which EXPLAINs to refuse (marker numbers, and "schema" for the
// tables) and how to answer CREATE DATABASE. Every call is appended to the log
// it returns.
func fakePsql(t *testing.T, refuse, create string) (log string) {
	t.Helper()
	dir := t.TempDir()
	// A name of its own per test, so a stand-in told to vanish takes that
	// name away and not the binary every other test runs. A hard link rather
	// than a symbolic one, because Windows lets an account create a symbolic
	// link only with developer mode on or the process elevated, and a hard
	// link needs neither; it also carries the binary's executable bit with
	// it, where a copy would need a mode written out. Both directories are
	// under the same temporary directory, so they share the one volume a
	// hard link needs.
	if err := os.Link(standin, filepath.Join(dir, standinName())); err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(dir, "calls.log")
	t.Setenv("PATH", dir)
	t.Setenv("FAKEPSQL_LOG", log)
	t.Setenv("FAKEPSQL_REFUSE", refuse)
	t.Setenv("FAKEPSQL_CREATE", create)
	return log
}

// schemaFile writes a dumped schema holding one measurement.
func schemaFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.json")
	body := `{"gh_repo": [["time", "Timestamp(Nanosecond, None)"], ["repo", "Dictionary(Int32, Utf8)"], ["stars", "Int64"]]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkRun runs the command and returns what it printed, its status and its
// error.
func checkRun(t *testing.T, args ...string) (stdout string, status int, err error) {
	t.Helper()
	var out strings.Builder
	status, err = run(t.Context(), args, &out)
	return out.String(), status, err
}

// TestEachRefusalIsBlamedOnItsOwnQuery is why the markers and the errors have
// to be read from one stream. Read as two, every error lands after the last
// marker: the first failing query is reported as passing and the last query
// is blamed instead.
func TestEachRefusalIsBlamedOnItsOwnQuery(t *testing.T) {
	fakePsql(t, "0 2", "")
	queries, err := postgresQueries()
	if err != nil {
		t.Fatal(err)
	}
	stdout, status, err := checkRun(t, schemaFile(t))
	if err != nil || status != 1 {
		t.Fatalf("run = %d, %v, want a failing status and no error", status, err)
	}
	for i, q := range queries {
		failed := strings.Contains(stdout, "FAIL "+q.title+": ERROR:  column \"q"+strconv.Itoa(i)+"\" does not exist")
		if (i == 0 || i == 2) != failed {
			t.Errorf("query %d (%s) reported as failing: %v, want %v", i, q.title, failed, i == 0 || i == 2)
		}
	}
	want := fmt.Sprintf("\n%d of %d queries planned by PostgreSQL, 2 failing\n", len(queries)-2, len(queries))
	if !strings.HasSuffix(stdout, want) {
		t.Errorf("stdout ends %q, want %q", stdout[max(0, len(stdout)-80):], want)
	}
}

// TestAPlannedDashboardPasses runs every query past a PostgreSQL that refuses
// none, inside a scratch database created first and dropped before and after.
func TestAPlannedDashboardPasses(t *testing.T) {
	log := fakePsql(t, "", "")
	stdout, status, err := checkRun(t, schemaFile(t))
	if err != nil || status != 0 {
		t.Fatalf("run = %d, %v, want a pass", status, err)
	}
	if !strings.HasSuffix(stdout, " failing\n") || strings.Contains(stdout, "FAIL ") {
		t.Errorf("stdout = %q, want only the count", stdout)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{
		"-u postgres psql -X -q -d postgres -c DROP DATABASE IF EXISTS " + db + ";",
		"-u postgres psql -X -q -d postgres -c CREATE DATABASE " + db + ";",
		"-u postgres psql -X -q -d " + db,
		"-u postgres psql -X -q -d postgres -c DROP DATABASE IF EXISTS " + db + ";",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("psql was run as\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// TestASchemaThatFailsIsReportedAsSuch fails the run on an error before the
// first marker, which no query can be blamed for.
func TestASchemaThatFailsIsReportedAsSuch(t *testing.T) {
	fakePsql(t, "schema", "")
	stdout, status, err := checkRun(t, schemaFile(t))
	if err != nil || status != 1 || !strings.HasPrefix(stdout, "schema failed:\nERROR:  type \"nope\"") {
		t.Errorf("run = %d, %v, %q, want the schema's own failure", status, err, stdout)
	}
}

// TestPsqlThatCannotStartStopsTheRun covers the three ways the database is
// never reached: no sudo at all, a CREATE DATABASE that is refused, and a psql
// that is gone by the time the script is sent.
func TestPsqlThatCannotStartStopsTheRun(t *testing.T) {
	t.Run("no sudo", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, _, err := checkRun(t, schemaFile(t)); err == nil || !strings.Contains(err.Error(), "executable file not found") {
			t.Errorf("err = %v, want sudo named as missing", err)
		}
	})
	t.Run("the database is refused", func(t *testing.T) {
		fakePsql(t, "", "refuse")
		if _, _, err := checkRun(t, schemaFile(t)); err == nil || err.Error() != "ERROR:  permission denied to create database" {
			t.Errorf("err = %v, want what PostgreSQL said", err)
		}
	})
	t.Run("the database is refused in silence", func(t *testing.T) {
		fakePsql(t, "", "silent")
		if _, _, err := checkRun(t, schemaFile(t)); err == nil || !strings.Contains(err.Error(), "exit status 3") {
			t.Errorf("err = %v, want the exit status when nothing was said", err)
		}
	})
	t.Run("psql is gone before the script", func(t *testing.T) {
		fakePsql(t, "", "vanish")
		if _, _, err := checkRun(t, schemaFile(t)); err == nil || !strings.Contains(err.Error(), "executable file not found") {
			t.Errorf("err = %v, want the missing program named", err)
		}
	})
}

// TestRunReadsItsArguments covers the usage and every argument the command
// refuses before reaching PostgreSQL.
func TestRunReadsItsArguments(t *testing.T) {
	fakePsql(t, "", "")
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-h", "--help", "-help"} {
		stdout, status, err := checkRun(t, flag)
		if stdout != usage+"\n" || status != 0 || err != nil {
			t.Errorf("%s = %q, %d, %v, want the usage and a clean exit", flag, stdout, status, err)
		}
	}
	absent := filepath.Join(t.TempDir(), "absent.json")
	// Each refusal is matched on what it says and not only on its being an
	// error: a -dump the command did not recognize would be read as the name
	// of a schema file, which is absent, and fail all the same.
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no schema file":            {nil, "no schema file given"},
		"--dump without a file":     {[]string{"--dump", "uid"}, "--dump needs a datasource uid and a file to write"},
		"-dump without a file":      {[]string{"-dump", "uid"}, "--dump needs a datasource uid and a file to write"},
		"a schema that is absent":   {[]string{absent}, absent},
		"a schema that is not JSON": {[]string{bad}, bad + ": "},
	} {
		if _, _, err := checkRun(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: run = %v, want an error saying %q", name, err, tc.want)
		}
	}
}

// TestMainExitsWithWhatRunAnswers runs main in a process of its own, because
// main is where an error turns into a message on stderr and an exit status of
// one, and a status into the exit itself: a usage asked for leaves cleanly
// with nothing on stderr, and a missing schema file says why and fails.
//
// The process is this package built as the program it is, found on PATH by its
// own name the way the stand-in for sudo is, and built before PATH is narrowed
// to it because the build needs the go command.
func TestMainExitsWithWhatRunAnswers(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "check_postgres")
	if runtime.GOOS == "windows" {
		program += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", program, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		name           string
		args           []string
		status         int
		stdout, stderr string
	}{
		{"the usage", []string{"--help"}, 0, usage + "\n", ""},
		{"no schema file", nil, 1, "", "no schema file given\n" + usage + "\n"},
	} {
		args := tc.args
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "check_postgres", args...)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			status := 0
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				status = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if status != tc.status || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
				t.Errorf("main = %d, stdout %q, stderr %q, want %d, %q, %q",
					status, stdout.String(), stderr.String(), tc.status, tc.stdout, tc.stderr)
			}
		})
	}
}

// grafanaSchema is a Grafana answering the information_schema query with the
// three columns given, or with the reply given verbatim when it is set.
func grafanaSchema(t *testing.T, columns [][]any, reply string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(raw), "information_schema.columns") ||
			!strings.Contains(string(raw), `"uid":"influx-uid"`) {
			t.Errorf("request = %s, %v, want the schema asked of the datasource given", raw, err)
		}
		if reply == "" {
			values := make([]any, len(columns))
			for i, c := range columns {
				values[i] = c
			}
			body, marshalErr := json.Marshal(map[string]any{"results": map[string]any{"A": map[string]any{
				"frames": []any{map[string]any{"data": map[string]any{"values": values}}},
			}}})
			if marshalErr != nil {
				t.Error(marshalErr)
			}
			reply = string(body)
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GRAFANA_URL", srv.URL)
	t.Setenv("GRAFANA_TOKEN", "glsa_test")
}

// TestDumpWritesTheSchemaItReadsBack fetches the schema through Grafana into
// the file named, and plans the queries against it in the same run.
func TestDumpWritesTheSchemaItReadsBack(t *testing.T) {
	fakePsql(t, "", "")
	grafanaSchema(t, [][]any{
		{"gh_repo", "gh_repo", "gh_repo", "gh_star"},
		{"repo", "stars", "time", "user"},
		{"Dictionary(Int32, Utf8)", "Int64", "Timestamp(Nanosecond, None)"},
	}, "")
	path := filepath.Join(t.TempDir(), "schema.json")
	if _, status, err := checkRun(t, "--dump", "influx-uid", path); err != nil || status != 0 {
		t.Fatalf("run = %d, %v, want the dump written and the queries planned", status, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tables map[string][][]string
	if err = json.Unmarshal(raw, &tables); err != nil {
		t.Fatal(err)
	}
	// The shortest column bounds the rows, so the fourth, which has no type,
	// is dropped rather than read as a pair with a hole in it.
	want := map[string][][]string{"gh_repo": {
		{"repo", "Dictionary(Int32, Utf8)"}, {"stars", "Int64"}, {"time", "Timestamp(Nanosecond, None)"},
	}}
	if fmt.Sprint(tables) != fmt.Sprint(want) {
		t.Errorf("dumped %v, want %v", tables, want)
	}
}

// TestDumpRefusesAnAnswerItCannotUse covers each way the schema never reaches
// the file: no token, a request that fails, a refused query, and an answer
// short of a column.
func TestDumpRefusesAnAnswerItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		columns [][]any
		reply   string
		token   string
		want    string
	}{
		{"no token", nil, "{}", "", "GRAFANA_TOKEN is not set"},
		{"a reply that is not JSON", nil, "<html>", "t", "200 OK"},
		{"a refused query", nil, `{"results":{"A":{"error":"table not found"}}}`, "t", "grafana refused the query: table not found"},
		{"two columns of three", [][]any{{"gh_repo"}, {"stars"}}, "", "t", "column 2 is not in the answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakePsql(t, "", "")
			grafanaSchema(t, tc.columns, tc.reply)
			t.Setenv("GRAFANA_TOKEN", tc.token)
			path := filepath.Join(t.TempDir(), "schema.json")
			if _, _, err := checkRun(t, "--dump", "influx-uid", path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Error("a schema file was written anyway")
			}
		})
	}
}

// TestLiteralRefusesAMacroItCannotReplace reports a surviving macro as what it
// is instead of letting PostgreSQL call it a syntax error in the query.
func TestLiteralRefusesAMacroItCannotReplace(t *testing.T) {
	t.Parallel()
	got, err := literal("SELECT $__timeGroupAlias(time, 1d), x FROM t WHERE $__timeFilter(time) AND repo IN (${repo:sqlstring})")
	if err != nil || got != `SELECT floor(extract(epoch FROM time) / 86400) * 86400 AS "time", x FROM t WHERE time > now() - interval '30 days' AND repo IN ('a','b')` {
		t.Errorf("literal = %q, %v, want every macro replaced", got, err)
	}
	got, err = literal("SELECT $__timeGroupAlias(time, $__interval), ROW_NUMBER() OVER (PARTITION BY $__timeGroup(time, $__interval), repo) FROM t")
	if err != nil || got != `SELECT floor(extract(epoch FROM time) / 86400) * 86400 AS "time", ROW_NUMBER() OVER (PARTITION BY floor(extract(epoch FROM time) / 86400) * 86400, repo) FROM t` {
		t.Errorf("literal = %q, %v, want the interval macros replaced, the aliased one whole", got, err)
	}
	for _, q := range []string{"SELECT $__interval", "SELECT ${other}"} {
		if _, err = literal(q); err == nil {
			t.Errorf("literal(%q) accepted a macro it has no literal for", q)
		}
	}
	if _, _, err = buildScript(nil, []query{{title: "odd", sql: "SELECT $__range"}}); err == nil ||
		!strings.HasPrefix(err.Error(), "odd: ") {
		t.Errorf("buildScript = %v, want the refusal named after its panel", err)
	}
}

// TestTheBucketStandInsAreEpochsLikeGrafanas pins the bucket macros to the
// epoch arithmetic Grafana's PostgreSQL datasource substitutes for them. A
// date_trunc of the same width plans the same shape and reads more clearly,
// but it is a timestamp where Grafana's is numeric, and the star and fork
// curves union that column against $__unixEpochFrom(): with a timestamp on
// one side PostgreSQL refuses two panels the datasource itself accepts, and
// the run blames the panel for the stand-in.
func TestTheBucketStandInsAreEpochsLikeGrafanas(t *testing.T) {
	t.Parallel()
	for _, m := range macros {
		if !strings.HasPrefix(m[0], "$__timeGroup") {
			continue
		}
		if strings.Contains(m[1], "date_trunc") || !strings.Contains(m[1], "extract(epoch") {
			t.Errorf("%s stands in as %q, want the epoch arithmetic Grafana substitutes", m[0], m[1])
		}
	}
}

// TestSplitReadsTheFirstErrorOfEachBlock ignores output that carries no marker
// number it can read, and keeps what precedes the first marker as the head.
func TestSplitReadsTheFirstErrorOfEachBlock(t *testing.T) {
	t.Parallel()
	head, failures := split("NOTICE: schema\n@@ 0\nERROR: one\nERROR: two\n@@ 1\nplan\n@@ 99999999999999999999\nERROR: lost\n")
	if head != "NOTICE: schema\n" || len(failures) != 1 || failures[0] != "ERROR: one" {
		t.Errorf("split = %q, %v, want the head and the first error of block 0", head, failures)
	}
	if head, failures = split("no markers at all"); head != "no markers at all" || len(failures) != 0 {
		t.Errorf("split = %q, %v, want everything as the head", head, failures)
	}
}

// TestColumnsOfOrdersASharedNameByType keeps two columns of one name in the
// order their types give, rather than in whatever order the dump listed them.
// The dump is tried in both orders, because a dump that already lists them the
// way the types sort proves nothing about the sort.
func TestColumnsOfOrdersASharedNameByType(t *testing.T) {
	t.Parallel()
	want := [][2]string{{"age", "BIGINT"}, {"size", "BIGINT"}, {"size", "DOUBLE PRECISION"}}
	for _, cols := range [][][]string{
		{{"size", "Int64"}, {"size", "Float64"}, {"age", "Int64"}},
		{{"size", "Float64"}, {"size", "Int64"}, {"age", "Int64"}},
	} {
		_, fields, err := columnsOf("gh_repo", cols)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fields, want) {
			t.Errorf("columnsOf(%v) = %v, want %v", cols, fields, want)
		}
	}
}
