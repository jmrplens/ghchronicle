package sink

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// What the connecting sink decides, which is everything worth reading and
// needs no server to read it. What it does with a server is
// test/e2e/docker/postgres_sink_test.go, behind the dockere2e tag, because
// only a server can answer whether PostgreSQL accepts this.

// probePoints is one batch with the shapes that matter: two series of one
// measurement, four field types, an empty string, and a field with no value.
func probePoints(at time.Time) []Point {
	return []Point{
		{
			Measurement: "gh_repo", Time: at,
			Tags:   map[string]string{"full_name": "a/b", "owner": "a"},
			Fields: map[string]any{"stars": 3, "size_kb": 1.5, "archived": false, "language": "Go"},
		},
		{
			Measurement: "gh_repo", Time: at,
			Tags:   map[string]string{"full_name": "c/d", "owner": "c"},
			Fields: map[string]any{"stars": 1, "language": ""},
		},
		// Nothing but tags: not a row.
		{
			Measurement: "gh_repo", Time: at,
			Tags: map[string]string{"full_name": "e/f", "owner": "e"},
		},
	}
}

// TestTheUpsertNamesOnlyIdentifiersAndBindsEverythingElse. The file sink has to
// render its values because its output is text somebody else runs; here there
// is a connection, so a value that reached the statement would be a value that
// could change it.
func TestTheUpsertNamesOnlyIdentifiersAndBindsEverythingElse(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	points := probePoints(at)
	shapes := sqlShapes(points)
	p := NewPostgres("postgres://ignored", 100)
	rows := p.rowsFor(points, shapes)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: the point carrying no field is not one", len(rows))
	}
	first := rows[0]
	for _, want := range []string{
		`INSERT INTO "gh_repo" (`,
		`VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		`ON CONFLICT ("time", "full_name", "owner") DO UPDATE SET`,
		`"stars" = EXCLUDED."stars"`,
	} {
		if !strings.Contains(first.statement, want) {
			t.Errorf("statement does not carry %q:\n%s", want, first.statement)
		}
	}
	// A tag is part of the key and so is never in the SET list: updating it
	// would be updating the thing the row was found by.
	if strings.Contains(first.statement, `"full_name" = EXCLUDED`) {
		t.Errorf("a key column is in the SET list:\n%s", first.statement)
	}
	if len(first.values) != 7 {
		t.Errorf("%d values for 7 placeholders: %v", len(first.values), first.values)
	}
	if first.values[0] != at {
		t.Errorf("the first value is %v, want the point's time", first.values[0])
	}
	// The empty string is left out of the second row rather than stored.
	if strings.Contains(rows[1].statement, `"language"`) {
		t.Errorf("an empty string became a column:\n%s", rows[1].statement)
	}
}

// fakeSchema answers declareAll the way a server would, without one: it
// records every statement sent, reports the columns a table already has, and
// can refuse a statement a number of times, the way a lock_timeout does.
type fakeSchema struct {
	sent []string
	// has is the columns each table holds before this process met it; a
	// table missing here holds what its CREATE TABLE made, the time and the
	// tags.
	has map[string]map[string]string
	// refuse fails each statement starting with a key, that many times.
	refuse map[string]int
	// unreadable fails the catalog read that many times.
	unreadable int
}

func (f *fakeSchema) exec(_ context.Context, ddl string) error {
	for prefix, n := range f.refuse {
		if n > 0 && strings.HasPrefix(ddl, prefix) {
			f.refuse[prefix] = n - 1
			return errors.New("canceling statement due to lock timeout")
		}
	}
	f.sent = append(f.sent, ddl)
	return nil
}

func (f *fakeSchema) columns(_ context.Context, table string) (map[string]string, error) {
	if f.unreadable > 0 {
		f.unreadable--
		return nil, errors.New("connection reset")
	}
	if cols, ok := f.has[table]; ok {
		return maps.Clone(cols), nil
	}
	return map[string]string{"time": "timestamp with time zone", "full_name": "text", "owner": "text"}, nil
}

// declared runs declareAll and hands back what it sent this time.
func (f *fakeSchema) declared(t *testing.T, p *Postgres, points []Point) ([]string, error) {
	t.Helper()
	before := len(f.sent)
	err := p.declareAll(context.Background(), f, points, sqlShapes(points))
	return f.sent[before:], err
}

// TestTheDDLGoesOncePerMeasurement, not once per point: a connection does not
// rotate underneath and forget what it was told, the way a file does.
func TestTheDDLGoesOncePerMeasurement(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	points := probePoints(at)
	p := NewPostgres("postgres://ignored", 100)
	db := &fakeSchema{}

	// A new table: its key, then each of its four fields.
	first, err := db.declared(t, p, points)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 5 || !strings.HasPrefix(first[0], `CREATE TABLE IF NOT EXISTS "gh_repo"`) {
		t.Fatalf("first batch = %v, want one CREATE TABLE for the one measurement and its fields", first)
	}
	for _, ddl := range first[1:] {
		if !strings.HasPrefix(ddl, `ALTER TABLE "gh_repo" ADD COLUMN IF NOT EXISTS `) {
			t.Errorf("first batch declares %q, want each field added to the table", ddl)
		}
	}
	if again, _ := db.declared(t, p, points); len(again) != 0 {
		t.Errorf("second batch = %v, want nothing: the table was declared already", again)
	}

	// A field nobody had seen arrives as an ALTER, and only that one.
	grown := probePoints(at)
	grown[0].Fields["watchers"] = 4
	added, _ := db.declared(t, p, grown)
	if len(added) != 1 || !strings.Contains(added[0], `ADD COLUMN IF NOT EXISTS "watchers" BIGINT`) {
		t.Errorf("after a new field = %v, want one ALTER TABLE for it", added)
	}
}

// TestARestartAltersOnlyWhatTheTableLacks is a process meeting a table an
// earlier one made. Every ADD COLUMN takes an ACCESS EXCLUSIVE lock before it
// checks IF NOT EXISTS, so one per field on every restart waited for every
// Grafana query on each table and held up every query behind it. The catalog
// says what is there, and only a field the earlier release did not write is
// added.
func TestARestartAltersOnlyWhatTheTableLacks(t *testing.T) {
	t.Parallel()
	points := probePoints(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	whole := map[string]string{
		"time": "timestamp with time zone", "full_name": "text", "owner": "text",
		"stars": "bigint", "size_kb": "double precision", "archived": "boolean", "language": "text",
	}
	db := &fakeSchema{has: map[string]map[string]string{"gh_repo": whole}}
	sent, err := db.declared(t, NewPostgres("postgres://ignored", 100), points)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !strings.HasPrefix(sent[0], `CREATE TABLE IF NOT EXISTS "gh_repo"`) {
		t.Errorf("a table holding every column got %v, want the CREATE TABLE IF NOT EXISTS alone", sent)
	}

	older := maps.Clone(whole)
	delete(older, "archived")
	db = &fakeSchema{has: map[string]map[string]string{"gh_repo": older}}
	sent, err = db.declared(t, NewPostgres("postgres://ignored", 100), points)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1] != `ALTER TABLE "gh_repo" ADD COLUMN IF NOT EXISTS "archived" BOOLEAN;` {
		t.Errorf("a table an earlier release made without archived got %v, want that one column added", sent)
	}
}

// TestAStatementThatFailedIsSentAgain. Every column used to be recorded as
// declared before its statement ran, so an ADD COLUMN refused by a
// lock_timeout, a statement_timeout or a dropped connection was never sent
// again, and every INSERT naming the column was refused until the process
// restarted. The same goes for the table itself and for the catalog read.
func TestAStatementThatFailedIsSentAgain(t *testing.T) {
	t.Parallel()
	points := probePoints(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name string
		db   *fakeSchema
		want []string
	}{
		{
			"a column", &fakeSchema{refuse: map[string]int{`ALTER TABLE "gh_repo" ADD COLUMN IF NOT EXISTS "size_kb"`: 1}},
			[]string{`ALTER TABLE "gh_repo" ADD COLUMN IF NOT EXISTS "size_kb" DOUBLE PRECISION;`, `ALTER TABLE "gh_repo" ADD COLUMN IF NOT EXISTS "stars" BIGINT;`},
		},
		{
			"the table", &fakeSchema{refuse: map[string]int{`CREATE TABLE`: 1}},
			nil,
		},
		{"the catalog", &fakeSchema{unreadable: 1}, nil},
	} {
		p := NewPostgres("postgres://ignored", 100)
		if _, err := tc.db.declared(t, p, points); err == nil {
			t.Errorf("%s: the refusal was not reported", tc.name)
		}
		again, err := tc.db.declared(t, p, points)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := tc.want
		if want == nil {
			// Nothing was recorded, so the whole declaration goes again.
			want, _ = (&fakeSchema{}).declared(t, NewPostgres("postgres://ignored", 100), points)
		}
		if !slices.Equal(again, want) {
			t.Errorf("%s: after the refusal the next write sent %v, want %v", tc.name, again, want)
		}
	}
}

// TestTheTwoSinksDeclareTheSameSchema. They share the code that decides it, and
// this is the test that says so out loud: a change that moved one and not the
// other would make one of the two published dashboards wrong. Against a new
// table, which is all a file can assume, they say the same statements.
func TestTheTwoSinksDeclareTheSameSchema(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	points := probePoints(at)

	connecting, err := (&fakeSchema{}).declared(t, NewPostgres("postgres://ignored", 100), points)
	if err != nil {
		t.Fatal(err)
	}
	toFile := newSQLSchema().declare("gh_repo", sqlShapes(points)["gh_repo"])
	if strings.Join(connecting, "\n") != strings.Join(toFile, "\n") {
		t.Errorf("the two sinks declare different tables:\n%v\n%v", connecting, toFile)
	}
}

// TestASinkThatCannotConnectSaysWhichSink, rather than handing back a driver
// error with nothing around it.
func TestASinkThatCannotConnectSaysWhichSink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dsn  string
		says string
	}{
		{"a dsn that does not parse", "postgres://%zz", "could not read its dsn"},
		{
			"a server that is not there", "postgres://u:p@127.0.0.1:1/d?sslmode=disable&connect_timeout=1",
			"could not reach its database",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPostgres(tc.dsn, 100)
			t.Cleanup(func() { _ = p.Close() })
			_, err := p.Write(t.Context(), probePoints(time.Now()))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want it to carry %q", err, tc.says)
			}
		})
	}
}

// TestAnEmptyBatchConnectsToNothing. A sweep where every point was deduplicated
// away should not open a connection to write nothing down it.
func TestAnEmptyBatchConnectsToNothing(t *testing.T) {
	t.Parallel()
	p := NewPostgres("postgres://u:p@127.0.0.1:1/d?sslmode=disable&connect_timeout=1", 100)
	t.Cleanup(func() { _ = p.Close() })
	written, err := p.Write(t.Context(), nil)
	if written != 0 || err != nil {
		t.Errorf("Write(nil) = %d, %v; want 0 and no error, since it should not have dialed",
			written, err)
	}
}

// TestTheSinkNamesItselfAndTakesADefaultBatch.
func TestTheSinkNamesItselfAndTakesADefaultBatch(t *testing.T) {
	t.Parallel()
	if got := NewPostgres("x", 0).batch; got != 1000 {
		t.Errorf("batch = %d, want the default where none was asked for", got)
	}
	if got := NewPostgres("x", 7).batch; got != 7 {
		t.Errorf("batch = %d, want the one asked for", got)
	}
	if got := NewPostgres("x", 0).Name(); got != "postgres" {
		t.Errorf("Name() = %q", got)
	}
	// Closing one that never connected is not an error: a run that ended
	// before its first write still closes its sinks.
	if err := NewPostgres("x", 0).Close(); err != nil {
		t.Errorf("Close() on a sink that never dialed: %v", err)
	}
}
