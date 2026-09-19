package sink

import (
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

// TestTheDDLGoesOncePerMeasurement, not once per point: a connection does not
// rotate underneath and forget what it was told, the way a file does.
func TestTheDDLGoesOncePerMeasurement(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	points := probePoints(at)
	shapes := sqlShapes(points)
	p := NewPostgres("postgres://ignored", 100)

	first := p.pendingDDL(points, shapes)
	if len(first) != 1 || !strings.HasPrefix(first[0], `CREATE TABLE IF NOT EXISTS "gh_repo"`) {
		t.Fatalf("first batch = %v, want one CREATE TABLE for the one measurement", first)
	}
	if again := p.pendingDDL(points, shapes); len(again) != 0 {
		t.Errorf("second batch = %v, want nothing: the table was declared already", again)
	}

	// A field nobody had seen arrives as an ALTER, and only that one.
	grown := probePoints(at)
	grown[0].Fields["watchers"] = 4
	added := p.pendingDDL(grown, sqlShapes(grown))
	if len(added) != 1 || !strings.Contains(added[0], `ADD COLUMN IF NOT EXISTS "watchers" BIGINT`) {
		t.Errorf("after a new field = %v, want one ALTER TABLE for it", added)
	}
}

// TestTheTwoSinksDeclareTheSameSchema. They share the code that decides it, and
// this is the test that says so out loud: a change that moved one and not the
// other would make one of the two published dashboards wrong.
func TestTheTwoSinksDeclareTheSameSchema(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	points := probePoints(at)
	shapes := sqlShapes(points)

	connecting := NewPostgres("postgres://ignored", 100).pendingDDL(points, shapes)
	toFile := newSQLSchema().declare("gh_repo", shapes["gh_repo"])
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
