//go:build dockere2e

package docker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// What a real PostgreSQL does with what the SQL sink emits.
//
// This sink speaks no wire protocol: it writes statements and leaves the
// connection to psql, so the text is the contract and test/e2e already reads
// that text closely. What it cannot do is run it. Everything below is about
// the half that only a server can answer: whether the DDL is accepted, whether
// the table it builds has the columns and the key the sink meant, whether the
// timestamps survive the round trip through TIMESTAMPTZ, and whether a second
// load of the same file rewrites the rows or doubles them.

// pgTable is one CREATE TABLE the sink emitted, as the sink meant it.
type pgTable struct {
	name string
	// cols maps a column to the type the DDL declares for it.
	cols map[string]string
	// key is the primary key in the order the DDL writes it: time first, then
	// the tag columns.
	key []string
}

// pgLoad is the result of piping one sweep's SQL into the database.
type pgLoad struct {
	// tables is what the file declares, parsed from the DDL itself rather
	// than listed here, so the assertions cannot drift from the sink.
	tables map[string]*pgTable
	// inserts counts the INSERT statements per table, which is what the row
	// counts are compared against.
	inserts map[string]int
}

var (
	pgOnce    sync.Once
	pgShared  *pgLoad
	errPGLoad error
)

// postgresLoad pipes the sweep's SQL through psql inside the container, once
// per test binary, and returns what the file declared.
//
// The tables it declares are dropped first. The database outlives a single
// test run when the stack is kept, and a CREATE TABLE IF NOT EXISTS against a
// table that is already there is a statement that proves nothing: dropping
// first is what makes this a test of the DDL rather than of its absence.
func postgresLoad(ctx context.Context, tb testing.TB, s *Stack, sweep *sqlStoresSweep) *pgLoad {
	tb.Helper()
	pgOnce.Do(func() {
		pgShared, errPGLoad = loadSweepSQL(ctx, tb, s, sweep)
	})
	if errPGLoad != nil {
		tb.Fatalf("the emitted SQL did not load: %v", errPGLoad)
	}
	return pgShared
}

func loadSweepSQL(ctx context.Context, tb testing.TB, s *Stack, sweep *sqlStoresSweep) (*pgLoad, error) {
	tb.Helper()
	load, err := parseEmittedSQL(sweep.SQL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	names := make([]string, 0, len(load.tables))
	for name := range load.tables {
		names = append(names, strconv.Quote(name))
	}
	if out, dropErr := s.Psql(ctx, "DROP TABLE IF EXISTS "+strings.Join(names, ", ")+" CASCADE;"); dropErr != nil {
		return nil, fmt.Errorf("dropping what an earlier run left: %w\n%s", dropErr, out)
	}
	if out, replayErr := replaySQL(ctx, s, sweep.SQL); replayErr != nil {
		return nil, fmt.Errorf("%w\n%s", replayErr, out)
	}
	return load, nil
}

// replaySQL pipes the file into psql, which is what the sink's documentation
// tells a user to do with it.
func replaySQL(ctx context.Context, s *Stack, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return s.LoadSQL(ctx, f)
}

func TestPostgresAcceptsTheDDLTheSQLSinkEmits(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := sqlStoresRun(ctx, t, s)
	load := postgresLoad(ctx, t, s, sweep)

	if len(load.tables) < 50 {
		t.Fatalf("the sweep declared only %d tables", len(load.tables))
	}
	columns, err := pgColumns(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := pgPrimaryKeys(ctx, s)
	if err != nil {
		t.Fatal(err)
	}

	for name, table := range load.tables {
		got, ok := columns[name]
		if !ok {
			t.Errorf("the file declares %q and the database has no such table", name)
			continue
		}
		for column, declared := range table.cols {
			want := pgTypeName(declared)
			switch actual := got[column]; {
			case actual == "":
				t.Errorf("%s.%s was declared and does not exist", name, column)
			case actual != want:
				t.Errorf("%s.%s is %s, and the file declared it %s", name, column, actual, declared)
			}
		}
		// The primary key is the whole reason a rewrite converges, so its
		// columns and their order both matter.
		if got := strings.Join(keys[name], ","); got != strings.Join(table.key, ",") {
			t.Errorf("%s has primary key (%s), and the file declared (%s)",
				name, got, strings.Join(table.key, ","))
		}
		if table.key[0] != "time" {
			t.Errorf("%s keys on (%s), which does not start at the time column",
				name, strings.Join(table.key, ","))
		}
	}
	t.Logf("%d tables checked against the DDL that created them", len(load.tables))
}

func TestPostgresKeepsTheDateOfTheEvent(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := sqlStoresRun(ctx, t, s)
	postgresLoad(ctx, t, s, sweep)

	t.Run("a dated event keeps the day it happened", func(t *testing.T) {
		// The tags are read as columns rather than only filtered on, because
		// a tag that arrived empty would still satisfy a WHERE on a different
		// one and the row would look right.
		row := pgRow(ctx, t, s, `SELECT `+pgUTCTime+`, "owner", "repo", "full_name", "url", "user_url", "starred"
			FROM "gh_star" WHERE "user" = 'alice'`)
		wantColumns(t, row, starGivenAt, "octocat", "hello-world", "octocat/hello-world",
			"https://github.com/octocat/hello-world/stargazers", "https://github.com/alice", "1")
	})

	t.Run("a daily snapshot keeps its own day", func(t *testing.T) {
		row := pgRow(ctx, t, s, `SELECT `+pgUTCTime+`, "count", "uniques"
			FROM "gh_traffic" WHERE "kind" = 'views' AND "count" = 120`)
		wantColumns(t, row, trafficDayAt, "120", "18")
	})

	t.Run("a run keeps the moment it finished", func(t *testing.T) {
		row := pgRow(ctx, t, s, `SELECT `+pgUTCTime+`, "duration_seconds", "success"
			FROM "gh_workflow_run" WHERE "run_id" = 1000163135`)
		wantColumns(t, row, runFinishedAt, "220", "t")
	})

	t.Run("a day of the star history keeps its own day", func(t *testing.T) {
		pgWantStarDays(ctx, t, s, sweep)
	})

	t.Run("a current-state gauge is stamped when the sweep looked", func(t *testing.T) {
		row := pgRow(ctx, t, s, `SELECT `+pgUTCTime+`, "stars", "forks" FROM "gh_repo"`)
		if row[1] != "80" || row[2] != "9" {
			t.Errorf("gh_repo holds stars=%s forks=%s, want 80 and 9", row[1], row[2])
		}
		at, err := time.Parse(time.RFC3339, row[0])
		if err != nil {
			t.Fatalf("time %q: %v", row[0], err)
		}
		// The bound is truncated the same way the column is read: pgUTCTime
		// renders whole seconds, so a point written in the same second the
		// sweep started comes back rounded down to before it.
		if at.Before(sweep.Started.Truncate(time.Second)) || at.After(sweep.Finished) {
			t.Errorf("gh_repo is stamped %s, outside the sweep (%s to %s)",
				row[0], sweep.Started.Format(time.RFC3339), sweep.Finished.Format(time.RFC3339))
		}
	})

	t.Run("nothing dated was restamped with the sweep's clock", func(t *testing.T) {
		for _, table := range []string{"gh_star", "gh_star_day", "gh_traffic", "gh_workflow_run"} {
			row := pgRow(ctx, t, s, fmt.Sprintf(`SELECT count(*) FROM %q WHERE "time" >= '%s'::timestamptz`,
				table, sweep.Started.Format(time.RFC3339)))
			if row[0] != "0" {
				t.Errorf("%s holds %s rows stamped after the sweep began", table, row[0])
			}
		}
	})
}

// pgWantStarDays holds the table to the sweep's own days of the star history.
// The tables are dropped before the load, so the store holds exactly the days
// this sweep emitted, the zeros of the newest thirty weeks among them: those
// are what an unstar is written back as, and a sink that skipped them would
// leave a day that lost its star reading one forever.
//
// Exact both ways, and a UTC midnight cannot move it, which is what sets it
// apart from the InfluxDB count: the file of this one sweep is the only one
// the suite loads, the dashboards' replay included, so the store and the
// points it is held to are the same sweep's output whatever day it ran on.
// Loading a second sweep's file here would need the comparison InfluxDB
// makes, of the days before the first sweep's own.
func pgWantStarDays(ctx context.Context, t *testing.T, s *Stack, sweep *sqlStoresSweep) {
	t.Helper()
	points := sqlStoresPoints(t, sweep)
	starDays(t, points)
	want := map[string]string{}
	for _, p := range points {
		if p.Measurement != "gh_star_day" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, p.Time)
		if err != nil {
			t.Fatalf("the sweep's own date %q: %v", p.Time, err)
		}
		key := p.Tags["full_name"] + "|" + p.Tags["owner"] + "|" + p.Tags["repo"] + "|" +
			at.UTC().Format(time.RFC3339)
		want[key] = strconv.FormatFloat(starCount(p), 'f', -1, 64)
	}
	out, err := s.Psql(ctx, `SELECT "full_name", "owner", "repo", `+pgUTCTime+`, "stars"
		FROM "gh_star_day"`)
	if err != nil {
		t.Fatalf("reading the star history back: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, row := range psqlRows(out) {
		if len(row) != 5 {
			t.Fatalf("a row of %d columns, want 5: %v", len(row), row)
		}
		if !strings.HasSuffix(row[3], "T00:00:00Z") {
			t.Errorf("%s holds a day stamped %s, not at the start of the day", row[0], row[3])
		}
		got[strings.Join(row[:4], "|")] = row[4]
	}
	for key, stars := range want {
		if got[key] != stars {
			t.Errorf("%s holds %q stars, and the sweep wrote %s", key, got[key], stars)
		}
	}
	for key := range got {
		if _, emitted := want[key]; !emitted {
			t.Errorf("%s is in the store and the sweep never wrote it", key)
		}
	}
}

// TestPostgresConvergesOnASecondLoad is the property the primary key buys: the
// same file replayed rewrites its rows instead of adding a second copy of
// them. The traffic window is re-read on every sweep, so without it a
// fortnight of views would accumulate into a fortnight of duplicates.
func TestPostgresConvergesOnASecondLoad(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := sqlStoresRun(ctx, t, s)
	load := postgresLoad(ctx, t, s, sweep)

	// The row count after the first load is the file's own INSERT count, which
	// is what proves nothing was silently skipped on the way in.
	for _, table := range []string{"gh_traffic", "gh_star", "gh_star_day", "gh_workflow_run", "gh_commits_week"} {
		row := pgRow(ctx, t, s, fmt.Sprintf(`SELECT count(*) FROM %q`, table))
		if got, want := row[0], strconv.Itoa(load.inserts[table]); got != want {
			t.Errorf("%s holds %s rows and the file inserts %s", table, got, want)
		}
	}

	before := pgRowCounts(ctx, t, s, load)
	if out, err := replaySQL(ctx, s, sweep.SQL); err != nil {
		t.Fatalf("the second load failed: %v\n%s", err, out)
	}
	after := pgRowCounts(ctx, t, s, load)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s held %d rows and holds %d after the same file was replayed",
				table, n, after[table])
		}
	}

	// And the values are the new ones, not the old ones kept: DO UPDATE rather
	// than DO NOTHING is what makes today's traffic row converge upwards as
	// the day fills in.
	row := pgRow(ctx, t, s, `SELECT "count" FROM "gh_traffic" WHERE "kind" = 'views' AND "time" = '`+trafficDayAt+`'::timestamptz`)
	if row[0] != "120" {
		t.Errorf("the traffic day reads %s after the replay, want 120", row[0])
	}
}

// pgRowCounts counts every table the file declared, in one query rather than
// one per table: each one is a docker exec, and there are sixty of them.
func pgRowCounts(ctx context.Context, t *testing.T, s *Stack, load *pgLoad) map[string]int {
	t.Helper()
	var parts []string
	for name := range load.tables {
		parts = append(parts, fmt.Sprintf("SELECT %s AS t, count(*) AS n FROM %q", quoteLiteral(name), name))
	}
	out, err := s.Psql(ctx, strings.Join(parts, " UNION ALL ")+" ORDER BY t;")
	if err != nil {
		t.Fatalf("counting the rows: %v\n%s", err, out)
	}
	counts := map[string]int{}
	for _, line := range psqlRows(out) {
		if len(line) != 2 {
			continue
		}
		n, parseErr := strconv.Atoi(line[1])
		if parseErr != nil {
			t.Fatalf("%q is not a count: %v", line[1], parseErr)
		}
		counts[line[0]] = n
	}
	if len(counts) != len(load.tables) {
		t.Fatalf("counted %d tables of %d", len(counts), len(load.tables))
	}
	return counts
}

// pgColumns reads every column of every table in one query.
func pgColumns(ctx context.Context, s *Stack) (map[string]map[string]string, error) {
	out, err := s.Psql(ctx, `SELECT table_name, column_name, data_type
		FROM information_schema.columns WHERE table_schema = 'public'
		ORDER BY table_name, ordinal_position;`)
	if err != nil {
		return nil, fmt.Errorf("reading the columns back: %w\n%s", err, out)
	}
	columns := map[string]map[string]string{}
	for _, row := range psqlRows(out) {
		if len(row) != 3 {
			continue
		}
		if columns[row[0]] == nil {
			columns[row[0]] = map[string]string{}
		}
		columns[row[0]][row[1]] = row[2]
	}
	return columns, nil
}

// pgPrimaryKeys reads every primary key in the order its columns are indexed,
// because the order is part of what the sink declared.
func pgPrimaryKeys(ctx context.Context, s *Stack) (map[string][]string, error) {
	out, err := s.Psql(ctx, `SELECT c.relname, a.attname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indisprimary
		ORDER BY c.relname, k.ord;`)
	if err != nil {
		return nil, fmt.Errorf("reading the primary keys back: %w\n%s", err, out)
	}
	keys := map[string][]string{}
	for _, row := range psqlRows(out) {
		if len(row) != 2 {
			continue
		}
		keys[row[0]] = append(keys[row[0]], row[1])
	}
	return keys, nil
}

// pgRow runs one query that must answer exactly one row, which is what every
// assertion above wants: a query that matched nothing is a failure with a
// message rather than an empty slice to index into.
func pgRow(ctx context.Context, t *testing.T, s *Stack, query string) []string {
	t.Helper()
	out, err := s.Psql(ctx, query)
	if err != nil {
		t.Fatalf("%s\n%v\n%s", query, err, out)
	}
	rows := psqlRows(out)
	if len(rows) != 1 {
		t.Fatalf("%d rows answered, want exactly one:\n%s\n%s", len(rows), query, out)
	}
	return rows[0]
}

// psqlRows splits the unaligned, header-free output psql was asked for.
func psqlRows(out string) [][]string {
	var rows [][]string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "|"))
	}
	return rows
}

// pgUTCTime renders the time column as the instant it holds, in UTC, so the
// assertion does not depend on the server's time zone.
const pgUTCTime = `to_char("time" AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func wantColumns(t *testing.T, row []string, want ...string) {
	t.Helper()
	if len(row) != len(want) {
		t.Fatalf("the row has %d columns, want %d: %v", len(row), len(want), row)
	}
	for i, w := range want {
		if row[i] != w {
			t.Errorf("column %d = %q, want %q", i+1, row[i], w)
		}
	}
}

// pgTypeName is the name PostgreSQL reports for what the DDL asked for.
func pgTypeName(declared string) string {
	switch declared {
	case "TIMESTAMPTZ":
		return "timestamp with time zone"
	case "BIGINT":
		return "bigint"
	case "DOUBLE PRECISION":
		return "double precision"
	case "BOOLEAN":
		return "boolean"
	default:
		return "text"
	}
}

// quoteLiteral renders a string constant for a query. Every value it is given
// here is a table name the sink itself wrote, but a literal built by
// concatenation deserves the escaping anyway.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseEmittedSQL reads the file the sink wrote and returns what it declares.
// The expectations every assertion above uses come from here rather than from
// a table in this file: the sink is the author of its own schema, and a test
// that repeated it would only prove the copy was faithful.
func parseEmittedSQL(path string) (*pgLoad, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the SQL sink wrote nothing: %w", err)
	}
	load := &pgLoad{tables: map[string]*pgTable{}, inserts: map[string]int{}}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if err = load.record(strings.TrimSpace(line)); err != nil {
			return nil, err
		}
	}
	if len(load.tables) == 0 {
		return nil, fmt.Errorf("%s declares no table at all", path)
	}
	return load, nil
}

// record adds one statement of the file to what the load declares. The sink
// writes one statement per line, so a line that is none of the three it emits
// is a statement this parser has never seen, and it says so rather than skip
// it.
func (load *pgLoad) record(stmt string) error {
	switch {
	case strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS "):
		table, err := parseCreateTable(stmt)
		if err != nil {
			return err
		}
		load.tables[table.name] = table
	case strings.HasPrefix(stmt, "ALTER TABLE "):
		return parseAlterTable(load, stmt)
	case strings.HasPrefix(stmt, "INSERT INTO "):
		name, _, err := quotedName(strings.TrimPrefix(stmt, "INSERT INTO "))
		if err != nil {
			return err
		}
		load.inserts[name]++
	case stmt == "":
	default:
		return fmt.Errorf("unexpected statement: %s", stmt)
	}
	return nil
}

func parseCreateTable(stmt string) (*pgTable, error) {
	name, rest, err := quotedName(strings.TrimPrefix(stmt, "CREATE TABLE IF NOT EXISTS "))
	if err != nil {
		return nil, err
	}
	body, ok := strings.CutSuffix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "(")), ");")
	if !ok {
		return nil, fmt.Errorf("%s: the statement does not close its column list", name)
	}
	table := &pgTable{name: name, cols: map[string]string{}}
	for _, part := range splitTopLevel(body) {
		if err = table.declare(strings.TrimSpace(part)); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(table.key) == 0 {
		return nil, fmt.Errorf("%s: the statement declares no primary key", name)
	}
	return table, nil
}

// declare reads one entry of a CREATE TABLE column list, which is either the
// primary key or one column and its type.
func (table *pgTable) declare(part string) error {
	if after, found := strings.CutPrefix(part, "PRIMARY KEY ("); found {
		table.key = quotedList(strings.TrimSuffix(after, ")"))
		return nil
	}
	column, kind, err := quotedName(part)
	if err != nil {
		return err
	}
	table.cols[column] = columnType(kind)
	return nil
}

func parseAlterTable(load *pgLoad, stmt string) error {
	name, rest, err := quotedName(strings.TrimPrefix(stmt, "ALTER TABLE "))
	if err != nil {
		return err
	}
	table := load.tables[name]
	if table == nil {
		return fmt.Errorf("ALTER TABLE %q before its CREATE TABLE", name)
	}
	column, kind, err := quotedName(strings.TrimPrefix(strings.TrimSpace(rest), "ADD COLUMN IF NOT EXISTS "))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	table.cols[column] = columnType(kind)
	return nil
}

// columnType is the declared type, without the constraints that follow it.
func columnType(rest string) string {
	kind := strings.TrimSuffix(strings.TrimSpace(rest), ";")
	for _, constraint := range []string{" NOT NULL", " DEFAULT"} {
		if i := strings.Index(kind, constraint); i >= 0 {
			kind = kind[:i]
		}
	}
	return strings.TrimSpace(kind)
}

// quotedName reads the double quoted identifier that opens s and returns it
// with whatever follows.
func quotedName(s string) (name, rest string, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("not a quoted identifier: %s", s)
	}
	end := strings.Index(s[1:], `"`)
	if end < 0 {
		return "", "", fmt.Errorf("unterminated identifier: %s", s)
	}
	return s[1 : 1+end], s[end+2:], nil
}

// quotedList reads a comma separated list of quoted identifiers, which is how
// the primary key is written.
func quotedList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if name, _, err := quotedName(part); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// splitTopLevel splits on the commas that are not inside parentheses, so the
// primary key's own list survives as one part.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := range len(s) {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
