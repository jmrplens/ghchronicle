//go:build dockere2e

package docker

import (
	"context"
	"math"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// What a real PostgreSQL does with what the connecting sink writes.
//
// The file sink's half of this is postgres_test.go, which pipes statements
// through psql. This one holds the connection itself, so what it answers is
// different: whether the DDL this sends is accepted by a server rather than by
// a parser, whether the upsert converges when the same series is written twice,
// whether a field first seen on a later sweep arrives without anyone running a
// migration, and whether the rules about empty strings and numbers that are
// not numbers survive the wire rather than the rendering.

// pgSinkDSN is the connection string for the stack's PostgreSQL.
func pgSinkDSN(s *Stack) string {
	// The host and the port are joined rather than formatted, because an IPv6
	// address formatted into a URL loses the brackets that make it one.
	at := net.JoinHostPort(s.PostgresHost, s.PostgresPort)
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(s.PostgresUser, s.PostgresPassword),
		Host:     at,
		Path:     "/" + s.PostgresDatabase,
		RawQuery: "sslmode=disable",
	}).String()
}

// pgSinkPoints is one sweep's worth of the shapes that matter: a tag set, four
// field types, an empty string, and a number that is not one.
func pgSinkPoints(at time.Time, stars int) []sink.Point {
	return []sink.Point{
		{
			Measurement: "gh_sinkprobe", Time: at,
			Tags:   map[string]string{"full_name": "jmrplens/ghchronicle", "owner": "jmrplens"},
			Fields: map[string]any{"stars": stars, "size_kb": 12.5, "archived": false, "language": "Go"},
		},
		{
			Measurement: "gh_sinkprobe", Time: at,
			Tags:   map[string]string{"full_name": "acme/o'brien", "owner": "acme"},
			Fields: map[string]any{"stars": 1, "language": "", "ratio": math.NaN()},
		},
	}
}

// TestThePostgresSinkWritesWhatTheFileSinkWouldHaveSaid, against a server.
func TestThePostgresSinkWritesWhatTheFileSinkWouldHaveSaid(t *testing.T) {
	ctx := context.Background()
	s := Start(t)
	if _, err := s.Psql(ctx, "DROP TABLE IF EXISTS gh_sinkprobe;"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Psql(context.Background(), "DROP TABLE IF EXISTS gh_sinkprobe;") })

	pg := sink.NewPostgres(pgSinkDSN(s), 100)
	t.Cleanup(func() { _ = pg.Close() })
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	written, err := pg.Write(ctx, pgSinkPoints(at, 3))
	if err != nil {
		t.Fatalf("the first sweep: %v", err)
	}
	if written != 2 {
		t.Errorf("wrote %d rows, want 2", written)
	}

	// The same series again, with a higher count and a field nobody had seen.
	second := pgSinkPoints(at, 9)
	second[0].Fields["watchers"] = 4
	if _, err = pg.Write(ctx, second); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}

	rows, err := s.Psql(ctx, `SELECT count(*) FROM gh_sinkprobe;`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(rows) != "2" {
		t.Errorf("the table holds %s rows, want 2: the upsert added instead of converging",
			strings.TrimSpace(rows))
	}

	got, err := s.Psql(ctx, `SELECT stars, watchers, language, ratio, size_kb, archived `+
		`FROM gh_sinkprobe WHERE full_name = 'jmrplens/ghchronicle';`)
	if err != nil {
		t.Fatal(err)
	}
	// psql's unaligned output, pipe separated: the second sweep's values.
	if want := "9|4|Go||12.5|f"; strings.TrimSpace(got) != want {
		t.Errorf("row = %q, want %q: the rewrite, and the column added on the way",
			strings.TrimSpace(got), want)
	}

	// The rules that are about what is NOT written: an empty string is no
	// cell, and a number that is not one is NULL.
	other, err := s.Psql(ctx, `SELECT coalesce(language, '<null>'), coalesce(ratio::text, '<null>') `+
		`FROM gh_sinkprobe WHERE full_name = 'acme/o''brien';`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "<null>|<null>"; strings.TrimSpace(other) != want {
		t.Errorf("row = %q, want %q", strings.TrimSpace(other), want)
	}
}

// TestThePostgresSinkBuildsTheKeyTheDashboardsQuery. The published dashboards
// read these tables, so the key is not an implementation detail: it is what
// makes a rewrite of a window converge, and cmd/check_postgres plans every
// panel query against this shape.
func TestThePostgresSinkBuildsTheKeyTheDashboardsQuery(t *testing.T) {
	ctx := context.Background()
	s := Start(t)
	if _, err := s.Psql(ctx, "DROP TABLE IF EXISTS gh_sinkkey;"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Psql(context.Background(), "DROP TABLE IF EXISTS gh_sinkkey;") })

	pg := sink.NewPostgres(pgSinkDSN(s), 100)
	t.Cleanup(func() { _ = pg.Close() })
	if _, err := pg.Write(ctx, []sink.Point{{
		Measurement: "gh_sinkkey", Time: time.Now().UTC(),
		Tags:   map[string]string{"full_name": "a/b", "owner": "a"},
		Fields: map[string]any{"stars": 1},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := s.Psql(ctx, `SELECT string_agg(a.attname, ',' ORDER BY k.ord) `+
		`FROM pg_index i JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true `+
		`JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum `+
		`WHERE i.indrelid = 'gh_sinkkey'::regclass AND i.indisprimary;`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "time,full_name,owner"; strings.TrimSpace(key) != want {
		t.Errorf("primary key = %q, want %q", strings.TrimSpace(key), want)
	}
}

// TestUninstallTakesThePostgresTablesAndLeavesTheRest. The catalog is asked
// rather than a list carried, so a table an older version wrote goes too, and
// a table that is not this project's stays whatever it is called.
func TestUninstallTakesThePostgresTablesAndLeavesTheRest(t *testing.T) {
	ctx := context.Background()
	s := Start(t)
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS gh_sinkgone, gh_sinkretired, payments;",
		"CREATE TABLE gh_sinkgone (time TIMESTAMPTZ PRIMARY KEY);",
		"CREATE TABLE gh_sinkretired (time TIMESTAMPTZ PRIMARY KEY);",
		"CREATE TABLE payments (time TIMESTAMPTZ PRIMARY KEY);",
	} {
		if _, err := s.Psql(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.Psql(context.Background(), "DROP TABLE IF EXISTS gh_sinkgone, gh_sinkretired, payments;")
	})

	cfg := &config.Config{Sinks: config.Sinks{
		Postgres: &config.PostgresSink{DSN: pgSinkDSN(s), Batch: 100},
	}}
	stores, _ := teardown.For(cfg)
	var store teardown.Store
	for _, candidate := range stores {
		if candidate.Name() == "postgres" {
			store = candidate
		}
	}
	if store == nil {
		t.Fatal("no postgres store to tear down")
	}
	held, err := store.Holds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gh_sinkgone", "gh_sinkretired"} {
		if !slices.Contains(held, want) {
			t.Errorf("holds = %v, want it to include %s", held, want)
		}
	}
	if slices.Contains(held, "payments") {
		t.Errorf("holds = %v, want somebody else's table left out", held)
	}
	if err = store.Drop(ctx, "payments"); err == nil {
		t.Error("it dropped a table that is not one of ours")
	}
	for _, name := range held {
		if err = store.Drop(ctx, name); err != nil {
			t.Fatalf("dropping %s: %v", name, err)
		}
	}
	left, err := s.Psql(ctx, `SELECT count(*) FROM pg_tables WHERE tablename = 'payments';`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(left) != "1" {
		t.Error("it removed the table it was told was not ours")
	}
}
