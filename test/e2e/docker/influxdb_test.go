//go:build dockere2e

package docker

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// What a real InfluxDB 3 does with what the sink writes.
//
// The capture-server suite in test/e2e proves the bytes: the line protocol
// parses, the fields keep their wire types, the traffic day carries the date
// the fixture gives it. None of that proves the store accepted any of it, and
// three of these four questions cannot be asked of a capture server at all: a
// server that answers 204 has no columns to type, no rows to read back and no
// second write to refuse.

// The dates the fixtures pin. Every one of them is older than the sweep, so a
// store that kept the sweep's own clock instead cannot accidentally agree.
const (
	// A star given on 1 March 2024 (testdata/stargazers_page1.json).
	starGivenAt = "2024-03-01T10:00:00Z"
)

// The two the fixtures spell as offsets, resolved the way the fake resolves
// them. A fixture that pins a calendar date drifts out of the window a
// dashboard asks for as the weeks pass; one that counts back from today does
// not, and an assertion that spelled the date by hand would be the thing left
// behind.
var (
	// The traffic day carrying 120 views (testdata/traffic_views.json).
	trafficDayAt = fakegh.DaysAgoDate(fakegh.TrafficDaysAgo) + "T00:00:00Z"
	// The workflow run that finished at 10:04:10 (testdata/actions_runs.json).
	runFinishedAt = fakegh.DaysAgoDate(fakegh.RunDaysAgo) + "T10:04:10Z"
)

// starDays is the sweep's own days of the star history that hold a star
// (testdata/stargazers_history.json). That fixture spells its weeks as offsets
// from the fake's current week, so the dates move with the calendar, and they
// are read from what the sweep emitted rather than typed here for the reason
// the two above are derived. Only the days that hold a star: the days of zero
// are real rows too, but a count that was lost or read from the wrong row can
// come back as a zero, and it cannot come back as the right number of stars.
func starDays(tb testing.TB, points []sqlStoresPoint) []sqlStoresPoint {
	tb.Helper()
	var out []sqlStoresPoint
	for _, p := range points {
		if p.Measurement == "gh_star_day" && starCount(p) > 0 {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		tb.Fatal("the sweep emitted no day of the star history with a star in it, " +
			"so no store can be asked whether it kept one")
	}
	return out
}

// starCount is a day's stars as the file sink wrote them.
func starCount(p sqlStoresPoint) float64 {
	n, _ := p.Fields["stars"].(float64)
	return n
}

// atMidnight reports whether an instant is the start of a UTC day, which is
// where the collector stamps every day of the star history whatever hour
// GitHub's own calendar day begins at.
func atMidnight(at time.Time) bool {
	return at.Equal(at.UTC().Truncate(24 * time.Hour))
}

// influxTimeLayout is how InfluxDB 3 renders a timestamp in its JSON answer:
// UTC, no zone suffix, fractional seconds only when it has any.
const influxTimeLayout = "2006-01-02T15:04:05.999999999"

func TestInfluxDBKeepsTheDateOfTheEvent(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := sqlStoresRun(ctx, t, s)

	t.Run("a dated event keeps the day it happened", func(t *testing.T) {
		rows, err := influxAwaitRow(ctx, s, sweep.Database,
			`SELECT * FROM gh_star WHERE "user" = 'alice'`)
		if err != nil {
			t.Fatalf("the star never arrived: %v", err)
		}
		row := rows[0]
		wantTime(t, row, starGivenAt)
		wantString(t, row, "repo", "hello-world")
		wantString(t, row, "full_name", "octocat/hello-world")
		wantString(t, row, "url", "https://github.com/octocat/hello-world/stargazers")
		wantString(t, row, "user_url", "https://github.com/alice")
		wantNumber(t, row, "starred", 1)
	})

	t.Run("a daily snapshot keeps its own day", func(t *testing.T) {
		rows, err := influxAwaitRow(ctx, s, sweep.Database,
			`SELECT * FROM gh_traffic WHERE kind = 'views' ORDER BY time`)
		if err != nil {
			t.Fatalf("no traffic arrived: %v", err)
		}
		row := rowAtTime(t, rows, trafficDayAt)
		wantNumber(t, row, "count", 120)
		wantNumber(t, row, "uniques", 18)
		wantString(t, row, "kind", "views")
	})

	t.Run("a run keeps the moment it finished", func(t *testing.T) {
		rows, err := influxAwaitRow(ctx, s, sweep.Database,
			`SELECT * FROM gh_workflow_run WHERE conclusion = 'success'`)
		if err != nil {
			t.Fatalf("no workflow run arrived: %v", err)
		}
		row := rows[0]
		wantTime(t, row, runFinishedAt)
		wantNumber(t, row, "run_id", 1000163135)
		// 10:00:30 to 10:04:10 is what the fixture describes, and the store
		// holding the duration proves the field survived as a number.
		wantNumber(t, row, "duration_seconds", 220)
		if enabled, ok := row["success"].(bool); !ok || !enabled {
			t.Errorf("success = %#v, want the boolean true", row["success"])
		}
	})

	t.Run("a day of the star history keeps its own day", func(t *testing.T) {
		influxWantStarDays(ctx, t, s, sweep)
	})

	t.Run("a current-state gauge is stamped when the sweep looked", func(t *testing.T) {
		// Newest first: a stack that is kept between runs still holds the
		// gauge rows earlier sweeps wrote, and each of those was stamped
		// inside its own sweep rather than this one.
		rows, err := influxAwaitRow(ctx, s, sweep.Database,
			`SELECT * FROM gh_repo ORDER BY time DESC LIMIT 1`)
		if err != nil {
			t.Fatalf("no repository row arrived: %v", err)
		}
		row := rows[0]
		wantNumber(t, row, "stars", 80)
		wantNumber(t, row, "forks", 9)
		wantString(t, row, "language", "Go")
		// The other half of the dating rule: what has no date of its own is
		// stamped now, and now is inside the sweep.
		at := timeOf(t, row)
		if at.Before(sweep.Started) || at.After(sweep.Finished) {
			t.Errorf("gh_repo is stamped %s, outside the sweep (%s to %s)",
				at.Format(time.RFC3339Nano), sweep.Started.Format(time.RFC3339), sweep.Finished.Format(time.RFC3339))
		}
	})

	t.Run("nothing dated was restamped with the sweep's clock", func(t *testing.T) {
		// The failure this is written against is a sink or a store that keeps
		// the moment of the write. It would put every one of these rows inside
		// the sweep window, and every one of them belongs before it. The star
		// history is held to it as well: its newest day is today, stamped at
		// midnight, and a day after the sweep's clock is one GitHub has not
		// counted yet.
		for _, table := range []string{"gh_star", "gh_star_day", "gh_traffic", "gh_workflow_run"} {
			rows, err := influxSQL(ctx, s, sweep.Database,
				fmt.Sprintf(`SELECT count(*) AS n FROM %q WHERE time >= '%s'`,
					table, sweep.Started.Format(time.RFC3339)))
			if err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n := numberOf(t, rows[0], "n"); n != 0 {
				t.Errorf("%s holds %.0f rows stamped after the sweep began", table, n)
			}
		}
	})
}

// influxWantStarDays holds the store to the sweep's own days of the star
// history: one row per repository and day, stamped at the start of the day
// and carrying the day's count. A stack kept between runs holds the days
// earlier runs wrote as well, each at its own date, so every day this sweep
// emitted has to be there and nothing is said about the rest.
func influxWantStarDays(ctx context.Context, t *testing.T, s *Stack, sweep *sqlStoresSweep) {
	t.Helper()
	rows, err := influxAwaitRow(ctx, s, sweep.Database,
		`SELECT * FROM gh_star_day WHERE stars > 0 ORDER BY time`)
	if err != nil {
		t.Fatalf("no day of the star history arrived: %v", err)
	}
	for _, want := range starDays(t, sqlStoresPoints(t, sweep)) {
		at, parseErr := time.Parse(time.RFC3339Nano, want.Time)
		if parseErr != nil {
			t.Fatalf("the sweep's own date %q: %v", want.Time, parseErr)
		}
		if !atMidnight(at) {
			t.Errorf("the sweep stamped a day of %s at %s, not at the start of the day",
				want.Tags["full_name"], want.Time)
		}
		row := influxRowOf(t, rows, want.Tags["full_name"], at)
		if row == nil {
			t.Errorf("%s has no row for %s, the day the sweep gave %v stars",
				want.Tags["full_name"], want.Time, starCount(want))
			continue
		}
		wantNumber(t, row, "stars", starCount(want))
		wantString(t, row, "owner", want.Tags["owner"])
		wantString(t, row, "repo", want.Tags["repo"])
		// The history names nobody, and a stargazer tag here would be a
		// series per person, which is what gh_star is for.
		if user, _ := row["user"].(string); user != "" {
			t.Errorf("a day of the star history carries user = %q", user)
		}
	}
}

// influxRowOf is the row of one repository at one instant, or nil.
func influxRowOf(t *testing.T, rows []map[string]any, fullName string, at time.Time) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["full_name"] == fullName && timeOf(t, row).Equal(at) {
			return row
		}
	}
	return nil
}

// TestInfluxDBTypedEveryColumnTheWayTheSinkMeantIt reads the schema InfluxDB
// built from the writes and checks it against what the sweep actually emitted:
// every tag a dictionary column, every field anything but. The point is not
// the type names. It is that a column's kind is decided once, by the first
// write, and everything after that has to agree with it.
func TestInfluxDBTypedEveryColumnTheWayTheSinkMeantIt(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := sqlStoresRun(ctx, t, s)

	columns, err := influxColumns(ctx, s, sweep.Database)
	if err != nil {
		t.Fatalf("reading the schema back: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("InfluxDB reports no columns at all")
	}

	checked := 0
	for _, p := range sqlStoresPoints(t, sweep) {
		table, ok := columns[p.Measurement]
		if !ok {
			// gh_job_log is excluded from this sink by default: a log line has
			// no business in a metrics database.
			if p.Measurement == "gh_job_log" {
				continue
			}
			t.Errorf("%s produced points but InfluxDB has no such table", p.Measurement)
			continue
		}
		checked++
		for tag := range p.Tags {
			switch kind := table[tag]; {
			case kind == "":
				t.Errorf("%s.%s was written as a tag and InfluxDB has no such column", p.Measurement, tag)
			case !strings.HasPrefix(kind, "Dictionary"):
				t.Errorf("%s.%s is a tag and InfluxDB typed it %s", p.Measurement, tag, kind)
			}
		}
		for field := range p.Fields {
			// A field InfluxDB never saw a value for has no column, which is
			// the sink dropping an empty string rather than a typing question.
			if kind := table[field]; strings.HasPrefix(kind, "Dictionary") {
				t.Errorf("%s.%s is a field and InfluxDB typed it as a tag (%s)", p.Measurement, field, kind)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no point was checked against the schema")
	}
	t.Logf("%d points checked against %d tables", checked, len(columns))
}

// TestInfluxDBAcceptsTheSameSweepTwice is the regression test for the failure
// this whole suite was built around: a column whose kind disagrees with what
// the database already holds is refused, and no capture server can notice.
// The second sweep writes the same measurements again, from a fresh state file
// so every family is collected, into the database the first one created.
func TestInfluxDBAcceptsTheSameSweepTwice(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	first := sqlStoresRun(ctx, t, s)

	// Dated rows only. What is stamped now is a new row on every sweep by
	// design, so counting it would prove nothing either way.
	before := datedRowCounts(ctx, t, s, first.Database)

	second, err := sqlStoresSweepInto(ctx, t, s, "second", first.Database)
	if err != nil {
		t.Fatalf("the second sweep failed: %v", err)
	}
	assertInfluxWroteCleanly(t, second.Log)

	after := datedRowCounts(ctx, t, s, first.Database)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s held %.0f dated rows and holds %.0f after the second sweep: "+
				"the same event was written as a new row instead of rewriting its own",
				table, n, after[table])
		}
	}
}

// assertInfluxWroteCleanly reads the sweep's log for the two ways this sink
// reports a store that refused something. Neither is visible in the exit
// status, which is why the log is read at all.
//
// It also counts what was written, because "nothing was refused" is also true
// of a sweep that offered the store nothing: with the ledger on, a second
// sweep of unchanged fixtures would skip almost every point and pass this
// without having asked InfluxDB anything.
func assertInfluxWroteCleanly(t *testing.T, log string) {
	t.Helper()
	written := 0
	for line := range strings.SplitSeq(log, "\n") {
		if !strings.Contains(line, "sink=influxdb") {
			continue
		}
		if strings.Contains(line, "sink write failed") || strings.Contains(line, "sink rejected some lines") {
			t.Errorf("InfluxDB refused part of the second sweep:\n%s", line)
		}
		if strings.Contains(line, "msg=written") {
			written++
		}
	}
	// One line per family that produced points, and there are twenty-three
	// families.
	if written < 20 {
		t.Errorf("the second sweep wrote to InfluxDB %d times, so it did not ask the store the question", written)
	}
}

// datedRowCounts counts the rows of the measurements whose timestamps the
// fixtures pin, so a second sweep can be shown to have rewritten them rather
// than added to them. The star history is the one every sweep rewrites on
// purpose, thirty weeks of it, zeros included, so that an unstar reaches the
// day the star was given: it converges only if each day lands on its own row.
func datedRowCounts(ctx context.Context, t *testing.T, s *Stack, database string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, table := range []string{"gh_star", "gh_star_day", "gh_traffic", "gh_workflow_run", "gh_commit"} {
		rows, err := influxSQL(ctx, s, database, fmt.Sprintf(`SELECT count(*) AS n FROM %q`, table))
		if err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		out[table] = numberOf(t, rows[0], "n")
	}
	return out
}

// TestInfluxDBRefusesAColumnThatChangesKind proves the guard the test above
// relies on is real, by breaking it on purpose in a database of its own.
//
// The refusal is InfluxDB's, and this is what it costs: the sink's reaction to
// it is a warning, not a failure, because InfluxDB phrases a column-type
// refusal with the same "parse failed" wording it uses for a line it cannot
// read, and the sink's bisect exists to drop unreadable lines rather than lose
// the batch they arrived in. So the sweep exits zero, every point of the
// offending measurement is dropped, and the only trace is the log. This test
// pins that, wording and all: if the sink learns to tell the two apart, this
// is the test that has to be rewritten, deliberately.
func TestInfluxDBRefusesAColumnThatChangesKind(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	database := s.InfluxDatabase + "_typeflip"
	influxSeedFlippedColumn(ctx, t, s, database)

	t.Run("the store refuses the write", func(t *testing.T) {
		status, body, err := influxWriteLine(ctx, s, database,
			`gh_traffic,owner=octocat,repo=hello-world,kind=views count=120i 1756512000000000000`)
		if err != nil {
			t.Fatalf("writing the flipped column: %v", err)
		}
		if status != 400 {
			t.Fatalf("InfluxDB answered %d to a column that changed kind, want 400: %s", status, body)
		}
		if !strings.Contains(body, "invalid column type for column 'kind'") {
			t.Errorf("the refusal does not name the column: %s", body)
		}
	})

	t.Run("the sink reports it and keeps the rest of the sweep", func(t *testing.T) {
		sweep, err := sqlStoresSweepInto(ctx, t, s, "typeflip", database)
		if err != nil {
			t.Fatalf("the sweep against the flipped database failed: %v", err)
		}
		if !strings.Contains(sweep.Log, "influxdb rejected a line as unparseable") {
			t.Errorf("the sink did not report the refused lines by content:\n%s", tail(sweep.Log))
		}
		if !strings.Contains(sweep.Log, "sink rejected some lines") {
			t.Errorf("the sink did not report the refusal at all:\n%s", tail(sweep.Log))
		}
		// What it cost: gh_traffic is gone from this database and nothing else
		// is. That is the bisect working as designed, on a rejection it was
		// not designed for.
		rows, err := influxSQL(ctx, s, database, `SELECT count(*) AS n FROM gh_traffic WHERE kind IS NOT NULL`)
		if err != nil {
			t.Fatalf("counting the traffic rows: %v", err)
		}
		if n := numberOf(t, rows[0], "n"); n != 1 {
			t.Errorf("gh_traffic holds %.0f rows besides the seed, so the flipped write was not refused", n-1)
		}
		if _, awaitErr := influxAwaitRow(ctx, s, database, `SELECT * FROM gh_star WHERE "user" = 'alice'`); awaitErr != nil {
			t.Errorf("the rest of the sweep was lost with the refused measurement: %v", awaitErr)
		}
	})
}

// influxSeedFlippedColumn writes gh_traffic's kind before the sink does. kind
// is a tag everywhere the sink writes gh_traffic; written first as a field, it
// fixes the column the other way round for this database.
func influxSeedFlippedColumn(ctx context.Context, t *testing.T, s *Stack, database string) {
	t.Helper()
	status, body, err := influxWriteLine(ctx, s, database,
		`gh_traffic,owner=octocat,repo=hello-world kind="views",count=1i 1700000000000000000`)
	if err != nil {
		t.Fatalf("seeding the flipped column: %v", err)
	}
	if status != 204 {
		t.Fatalf("seeding the flipped column answered %d: %s", status, body)
	}
}

// influxColumns reads the whole schema in one query: measurement to column to
// the type InfluxDB decided on.
func influxColumns(ctx context.Context, s *Stack, database string) (map[string]map[string]string, error) {
	rows, err := influxSQL(ctx, s, database,
		`SELECT table_name, column_name, data_type FROM information_schema.columns
		 WHERE table_schema = 'iox' ORDER BY table_name, column_name`)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, row := range rows {
		table, _ := row["table_name"].(string)
		column, _ := row["column_name"].(string)
		kind, _ := row["data_type"].(string)
		if out[table] == nil {
			out[table] = map[string]string{}
		}
		out[table][column] = kind
	}
	return out, nil
}

// rowAtTime picks the row stamped at want, and says which dates are there when
// none is: a store that kept the sweep's clock shows up in that list.
func rowAtTime(t *testing.T, rows []map[string]any, want string) map[string]any {
	t.Helper()
	var seen []string
	for _, row := range rows {
		at := timeOf(t, row)
		if at.Format(time.RFC3339) == want {
			return row
		}
		seen = append(seen, at.Format(time.RFC3339))
	}
	t.Fatalf("no row is stamped %s; the store holds %v", want, seen)
	return nil
}

func timeOf(t *testing.T, row map[string]any) time.Time {
	t.Helper()
	raw, ok := row["time"].(string)
	if !ok {
		t.Fatalf("the row carries no time: %v", row)
	}
	at, err := time.ParseInLocation(influxTimeLayout, raw, time.UTC)
	if err != nil {
		t.Fatalf("time %q: %v", raw, err)
	}
	return at
}

func wantTime(t *testing.T, row map[string]any, want string) {
	t.Helper()
	if got := timeOf(t, row).Format(time.RFC3339); got != want {
		t.Errorf("the row is stamped %s, want the event's own %s", got, want)
	}
}

func wantString(t *testing.T, row map[string]any, column, want string) {
	t.Helper()
	if got, _ := row[column].(string); got != want {
		t.Errorf("%s = %#v, want %q", column, row[column], want)
	}
}

func wantNumber(t *testing.T, row map[string]any, column string, want float64) {
	t.Helper()
	if got := numberOf(t, row, column); got != want {
		t.Errorf("%s = %v, want %v", column, got, want)
	}
}

// numberOf reads a numeric column. Every number comes back as a JSON one, so
// an integer that arrived as a float would read the same here; what the column
// was typed as is the schema test's question, not this one's.
func numberOf(t *testing.T, row map[string]any, column string) float64 {
	t.Helper()
	n, ok := row[column].(float64)
	if !ok {
		t.Fatalf("%s = %#v, which is not a number", column, row[column])
	}
	if math.IsNaN(n) {
		t.Fatalf("%s is NaN", column)
	}
	return n
}

// tail is the last part of a log, for a failure message that should point at
// the end of the run rather than reprint the whole sweep.
func tail(log string) string {
	const keep = 2000
	if len(log) <= keep {
		return log
	}
	return "..." + log[len(log)-keep:]
}
