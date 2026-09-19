// Command check_postgres parses every PostgreSQL panel query with PostgreSQL
// itself.
//
// There is no PostgreSQL holding this data to run the queries against, so the
// next best thing: the SQL sink's schema is declared in a scratch database, in
// one transaction that is rolled back at the end, and each panel query is
// EXPLAINed with Grafana's macros replaced by literals. EXPLAIN plans the query
// without executing it, so a column that does not exist, a reserved word left
// unquoted or a bad cast fails here and nothing else does.
//
// The schema is derived from an InfluxDB that has seen every measurement, the
// way the sink types it: a tag becomes a TEXT column defaulting to the empty
// string, an integer BIGINT, a float DOUBLE PRECISION, a boolean BOOLEAN, a
// string TEXT, and the primary key is the time plus the tags in name order.
// The dump is the result of
//
//	SELECT table_name, column_name, data_type FROM information_schema.columns
//	WHERE table_schema = 'iox'
//
// saved as {table: [[column, type], ...]}; `--dump <influxdb-datasource-uid>`
// fetches it through Grafana into the given file first.
//
//	go run ./cmd/check_postgres schema.json
//	GRAFANA_TOKEN=... go run ./cmd/check_postgres --dump <uid> schema.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/dashboards"
	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// db is the scratch database the schema is declared in. It is created and
// dropped by every run, so nothing of it survives.
const db = "ghchronicle_scratch"

// types is the sink's mapping from an InfluxDB column type to a PostgreSQL one.
var types = map[string]string{
	"Int64":   "BIGINT",
	"Float64": "DOUBLE PRECISION",
	"Boolean": "BOOLEAN",
	"Utf8":    "TEXT",
}

// macros are the Grafana macros the panels use, with the literal that stands in
// for each while PostgreSQL plans the query.
//
// The bucketed panels group by $__interval, which Grafana computes from the
// range and the panel's floor at render time; a day stands in for it here,
// since the plan is the same whatever the width. The aliased form goes first
// so the bare one, which the panels use inside a window's partition, does not
// eat its head.
//
// The bucket macros stand in with the epoch arithmetic Grafana's PostgreSQL
// datasource actually substitutes (floor(extract(epoch from time) / n) * n),
// not with a date_trunc of the same width. The two plan the same query and
// read alike, but they are not the same type, and the type is the thing this
// command is here to check: with a date_trunc standing in, the star and fork
// curves unioned a timestamp against the epoch seconds of the anchor macros
// below and PostgreSQL refused both panels with "UNION types timestamp with
// time zone and numeric cannot be matched", which is a failure of the
// stand-in and not of the panel. A stand-in of the wrong type either invents
// a failure like that one or hides a real one.
var macros = [][2]string{
	{"$__timeFilter(time)", "time > now() - interval '30 days'"},
	// The start of the range as a timestamp, which is what the star and fork
	// curves count up to. Grafana substitutes it before the query is sent, so
	// the plan sees a literal; here it stands for the same thirty days as the
	// filter above.
	{"$__timeFrom()", "(now() - interval '30 days')"},
	// The two bounds as epoch seconds, which is how the star and fork curves
	// anchor their first and last bucket: the bucket column is the epoch the
	// PostgreSQL bucket macro returns, so the anchors are epochs too, and
	// both sides of that union are numeric here for the same reason.
	{"$__unixEpochFrom()", "extract(epoch FROM now() - interval '30 days')"},
	{"$__unixEpochTo()", "extract(epoch FROM now())"},
	{"$__timeGroupAlias(time, $__interval)", `floor(extract(epoch FROM time) / 86400) * 86400 AS "time"`},
	{"$__timeGroup(time, $__interval)", `floor(extract(epoch FROM time) / 86400) * 86400`},
	{"$__timeGroupAlias(time, 1d)", `floor(extract(epoch FROM time) / 86400) * 86400 AS "time"`},
	{"$__timeGroupAlias(time, 7d)", `floor(extract(epoch FROM time) / 604800) * 604800 AS "time"`},
	{"$__timeGroupAlias(time, 1h)", `floor(extract(epoch FROM time) / 3600) * 3600 AS "time"`},
	{"$__timeGroupAlias(time, 5m)", `floor(extract(epoch FROM time) / 300) * 300 AS "time"`},
	{"${repo:sqlstring}", "'a','b'"},
}

const usage = `usage: check_postgres [--dump <influxdb-datasource-uid>] <schema.json>`

// Nothing here cancels the run, but psql and Grafana are both reached under
// one context so that a caller that wanted to could.
//
// One line, and that is not a style choice: nothing in a main can be covered,
// so every line of one is a line the new-code coverage gate counts against a
// change that touches it. The decision this used to hold is in exit, beside
// run, where a test reaches it.
func main() { os.Exit(exit(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

// exit is main without the exiting: it turns what run answers into the status
// to leave with, and prints the error that stopped it. Separate so that the
// one decision main used to hold, whether an error outranks the status, is
// somewhere a test can reach.
func exit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	status, err := run(ctx, args, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return status
}

// run is the whole command short of exiting: it answers with the status to exit
// with, or with the error that stopped it before PostgreSQL could say anything.
// The report goes to stdout, which is passed in so a test can read it.
func run(ctx context.Context, args []string, stdout io.Writer) (int, error) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(stdout, usage)
		return 0, nil
	}
	if len(args) > 0 && (args[0] == "--dump" || args[0] == "-dump") {
		if len(args) < 3 {
			return 0, fmt.Errorf("--dump needs a datasource uid and a file to write\n%s", usage)
		}
		if err := dump(ctx, args[1], args[2]); err != nil {
			return 0, err
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return 0, fmt.Errorf("no schema file given\n%s", usage)
	}

	schema, err := readSchema(args[0])
	if err != nil {
		return 0, err
	}
	queries, err := postgresQueries()
	if err != nil {
		return 0, err
	}
	script, literals, err := buildScript(schema, queries)
	if err != nil {
		return 0, err
	}
	out, err := explain(ctx, script)
	if err != nil {
		return 0, err
	}
	return report(stdout, queries, literals, out), nil
}

// readSchema turns the dumped InfluxDB schema into the CREATE TABLE statements
// the SQL sink would have written.
func readSchema(path string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var tables map[string][][]string
	if err = json.Unmarshal(raw, &tables); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ddl(tables)
}

// buildScript is the whole psql script: the schema, then every panel query
// EXPLAINed with Grafana's macros replaced by literals. A failed statement
// aborts the transaction, so each EXPLAIN gets its own savepoint and the next
// one starts from a clean state. The literals come back too, because that is
// what a failure has to be reported against.
func buildScript(schema []string, queries []query) (script, literals []string, err error) {
	script = append([]string{`\set ON_ERROR_STOP off`, "BEGIN;"}, schema...)
	literals = make([]string, len(queries))
	for i, q := range queries {
		if literals[i], err = literal(q.sql); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", q.title, err)
		}
		script = append(script,
			fmt.Sprintf(`\echo @@ %d`, i),
			fmt.Sprintf("SAVEPOINT q%d;", i),
			"EXPLAIN "+literals[i]+";",
			fmt.Sprintf("ROLLBACK TO SAVEPOINT q%d;", i))
	}
	return append(script, "ROLLBACK;"), literals, nil
}

// explain declares the schema and plans every query in a scratch database, and
// throws the database away either way.
//
// The script goes in on psql's standard input rather than through a file. A
// file would have to be readable by the postgres user this runs psql as, which
// on a shared machine means readable by everyone, and a script that exists for
// the length of one command has no business being on disk at all.
//
// psql's standard output and standard error are one pipe, the way `2>&1` would
// make them. The markers are echoed on standard output and a refusal is
// reported on standard error, so read as two streams every refusal lands after
// the last marker, where split blames the last query for the first of them
// and reports the rest, the failing queries among them, as planned. Only one
// stream keeps each error behind the marker of the statement that caused it.
func explain(ctx context.Context, script []string) (string, error) {
	psql := func(database, stdin string, extra ...string) (output string, err error) {
		argv := append([]string{"-u", "postgres", "psql", "-X", "-q", "-d", database}, extra...)
		cmd := exec.CommandContext(ctx, "sudo", argv...)
		var out strings.Builder
		cmd.Stdin = strings.NewReader(stdin)
		// The same writer for both is what makes os/exec hand the child one
		// descriptor for the two, rather than two pipes read side by side.
		cmd.Stdout, cmd.Stderr = &out, &out
		err = cmd.Run()
		return out.String(), err
	}
	drop := func() {
		// Best effort: the run has already said what it had to say.
		_, _ = psql("postgres", "", "-c", "DROP DATABASE IF EXISTS "+db+";")
	}

	drop()
	defer drop()
	// -q keeps a successful CREATE DATABASE silent, so anything it printed is
	// why it failed.
	if said, err := psql("postgres", "", "-c", "CREATE DATABASE "+db+";"); err != nil {
		if said == "" {
			return "", err
		}
		return "", errors.New(strings.TrimRight(said, "\n"))
	}
	out, err := psql(db, strings.Join(script, "\n")+"\n")
	// psql reports a refused statement in its exit status too, and that is the
	// whole point of the run, so only a failure to start it is fatal here.
	if _, cannotStart := errors.AsType[*exec.Error](err); cannotStart {
		return "", err
	}
	return out, nil
}

// report prints what PostgreSQL refused and answers with the status the run
// should exit with.
func report(w io.Writer, queries []query, literals []string, out string) int {
	head, failures := split(out)
	if strings.Contains(head, "ERROR") {
		fmt.Fprintln(w, "schema failed:\n"+head)
		return 1
	}
	ok := 0
	for i, q := range queries {
		if msg, bad := failures[i]; bad {
			fmt.Fprintf(w, "FAIL %s: %s\n     %s\n", q.title, msg, grafana.Trim(literals[i], 300))
		} else {
			ok++
		}
	}
	fmt.Fprintf(w, "\n%d of %d queries planned by PostgreSQL, %d failing\n",
		ok, len(queries), len(failures))
	if len(failures) > 0 {
		return 1
	}
	return 0
}

// query is one panel query, with the panel title to report it under.
type query struct {
	title string
	sql   string
}

// postgresQueries is every raw SQL the postgres dashboard asks for: the panels
// first, then the repository variable.
func postgresQueries() ([]query, error) {
	store, found := dashboards.ByName("postgres")
	if !found {
		return nil, errors.New("there is no postgres dashboard to check")
	}
	var out []query
	for _, p := range grafana.Walk(store.Build(nil)["panels"]) {
		if sql, ok := p.Target["rawSql"].(string); ok {
			out = append(out, query{title: p.Title, sql: sql})
		}
	}
	variable, _ := store.Variable["query"].(map[string]any)
	sql, ok := variable["rawSql"].(string)
	if !ok {
		return nil, errors.New("the repository variable carries no rawSql")
	}
	return append(out, query{title: "variable repo", sql: sql}), nil
}

// ddl is the CREATE TABLE statements the sink would emit, one per measurement.
func ddl(tables map[string][][]string) ([]string, error) {
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []string
	for _, name := range names {
		// A table InfluxDB renamed aside is not a measurement.
		if !strings.HasPrefix(name, "gh_") || strings.Contains(name, "-") {
			continue
		}
		tags, fields, err := columnsOf(name, tables[name])
		if err != nil {
			return nil, err
		}
		out = append(out, createTable(name, tags, fields))
	}
	return out, nil
}

// columnsOf sorts one measurement's dumped columns into the tags and the typed
// fields the sink would declare, each in the order the sink declares them. The
// time column is left out: every table has it, in the same place.
func columnsOf(name string, cols [][]string) (tags []string, fields [][2]string, err error) {
	for _, col := range cols {
		if len(col) != 2 {
			return nil, nil, fmt.Errorf("%s: a column is not a [name, type] pair", name)
		}
		column, kind := col[0], col[1]
		if column == "time" {
			continue
		}
		if strings.HasPrefix(kind, "Dictionary") {
			tags = append(tags, column)
			continue
		}
		sqlType, ok := types[strings.SplitN(kind, "(", 2)[0]]
		if !ok {
			return nil, nil, fmt.Errorf("%s.%s: no PostgreSQL type for %s", name, column, kind)
		}
		fields = append(fields, [2]string{column, sqlType})
	}
	sort.Strings(tags)
	// By name and then by type, so two columns of the same name keep the
	// order the dump would give them rather than an arbitrary one.
	sort.Slice(fields, func(i, j int) bool {
		if fields[i][0] != fields[j][0] {
			return fields[i][0] < fields[j][0]
		}
		return fields[i][1] < fields[j][1]
	})
	return tags, fields, nil
}

// createTable is the statement the sink would write for one measurement: the
// time, then the tags as empty-by-default text, then the fields, keyed on the
// time and the tags.
func createTable(name string, tags []string, fields [][2]string) string {
	parts := []string{`"time" TIMESTAMPTZ NOT NULL`}
	key := []string{`"time"`}
	for _, tag := range tags {
		parts = append(parts, ident(tag)+` TEXT NOT NULL DEFAULT ''`)
		key = append(key, ident(tag))
	}
	for _, f := range fields {
		parts = append(parts, ident(f[0])+" "+f[1])
	}
	return fmt.Sprintf("CREATE TABLE %s (%s, PRIMARY KEY (%s));",
		ident(name), strings.Join(parts, ", "), strings.Join(key, ", "))
}

// ident quotes a PostgreSQL identifier. The names come from whatever the
// dumped InfluxDB schema holds rather than from this repository, so they are
// quoted rather than interpolated. Quoted by hand because %q is Go's quoting
// rather than SQL's: it escapes a quote or a backslash with a backslash, where
// PostgreSQL doubles the quote and reads the backslash as part of the name.
// Neither form makes such a name safe, but only one of them changes it.
func ident(name string) string { return `"` + name + `"` }

// literal replaces every Grafana macro with what it stands for. A macro that
// survives would be planned as a syntax error and blamed on the query, so it is
// reported as what it is instead.
func literal(q string) (string, error) {
	for _, m := range macros {
		q = strings.ReplaceAll(q, m[0], m[1])
	}
	if strings.Contains(q, "$__") || strings.Contains(q, "${") {
		return "", fmt.Errorf("a Grafana macro has no literal to stand in for it: %s", q)
	}
	return q, nil
}

// marker is the line psql echoes before each EXPLAIN, carrying its index.
var marker = regexp.MustCompile(`(?m)^@@ (\d+)$`)

// errorLine is what PostgreSQL says when it refuses to plan a statement.
var errorLine = regexp.MustCompile(`ERROR:.*`)

// split cuts psql's output at the markers and reads the first error out of each
// block. What comes before the first marker is the schema's own output.
func split(out string) (head string, failures map[int]string) {
	failures = map[int]string{}
	found := marker.FindAllStringSubmatchIndex(out, -1)
	if len(found) == 0 {
		return out, failures
	}
	head = out[:found[0][0]]
	for i, m := range found {
		end := len(out)
		if i+1 < len(found) {
			end = found[i+1][0]
		}
		body := out[m[1]:end]
		if msg := errorLine.FindString(body); msg != "" {
			n, err := strconv.Atoi(out[m[2]:m[3]])
			if err == nil {
				failures[n] = msg
			}
		}
	}
	return head, failures
}

// dump fetches the InfluxDB information_schema through Grafana and writes it in
// the shape this command reads back.
func dump(ctx context.Context, uid, path string) error {
	client := grafana.New()
	if client.Token == "" {
		return errors.New("GRAFANA_TOKEN is not set, and --dump has to ask Grafana")
	}
	q := "SELECT table_name, column_name, data_type FROM information_schema.columns" +
		" WHERE table_schema = 'iox' ORDER BY table_name, column_name"
	res, err := client.Query(ctx, "now-1h", "now", map[string]any{
		"datasource": map[string]any{"type": "influxdb", "uid": uid},
		"format":     "table", "rawQuery": true, "rawSql": q, "query": q, "refId": "A",
	}, 60*time.Second)
	if err != nil {
		return err
	}
	if _, errText := grafana.Frames(res, "A"); errText != "" {
		return fmt.Errorf("grafana refused the query: %s", grafana.Trim(errText, 300))
	}
	columns := make([][]string, 3)
	for n := range columns {
		if columns[n], err = grafana.Column(res, "A", n); err != nil {
			return err
		}
	}
	rows := len(columns[0])
	for _, c := range columns[1:] {
		if len(c) < rows {
			rows = len(c)
		}
	}
	tables := map[string][][]string{}
	for i := range rows {
		table := columns[0][i]
		tables[table] = append(tables[table], []string{columns[1][i], columns[2][i]})
	}
	body, err := json.MarshalIndent(tables, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Clean(path), body, 0o600)
}
