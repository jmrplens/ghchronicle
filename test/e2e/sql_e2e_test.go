package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// psqlClient is however psql can reach the local server on this machine. The
// tests skip rather than fail when it cannot: the emitted SQL is the contract,
// and loading it is a stronger check that not every machine can run.
// sudo says to reach the server as the postgres role rather than as the
// current user. There is no third way in, so the program each one runs is a
// literal here rather than a list a caller could fill with anything.
type psqlClient struct{ sudo bool }

// findPsql probes the ways in, in order of least privilege: the current user
// first, then the postgres role through sudo, which is what a stock Debian
// install leaves as the only peer-authenticated way in for root.
func findPsql(t *testing.T) *psqlClient {
	t.Helper()
	if _, err := exec.LookPath("psql"); err != nil {
		return nil
	}
	candidates := []*psqlClient{{}}
	if _, err := exec.LookPath("sudo"); err == nil {
		candidates = append(candidates, &psqlClient{sudo: true})
	}
	for _, c := range candidates {
		out, err := c.run("postgres", "", "-tAc", "select 1")
		if err == nil && strings.TrimSpace(out) == "1" {
			return c
		}
	}
	return nil
}

func (c *psqlClient) run(db, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	full := append([]string{"-v", "ON_ERROR_STOP=1", "-q", "-d", db}, args...)
	cmd := c.command(ctx, full)
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=5")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// command builds the psql invocation. Both branches name their program
// outright, and sudo is handed the role it may assume rather than a command
// line assembled elsewhere.
func (c *psqlClient) command(ctx context.Context, args []string) *exec.Cmd {
	if c.sudo {
		// sudo stops reading options at the first non-option, so everything
		// from "psql" on is the command it runs and that command's own
		// arguments, never more options for sudo itself.
		sudoArgs := append([]string{"-n", "-u", "postgres", "psql"}, args...)
		return exec.CommandContext(ctx, "sudo", sudoArgs...)
	}
	return exec.CommandContext(ctx, "psql", args...)
}

func TestSQLSinkEmitsLoadablePostgres(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "points.sql")
	cfg := writeSinkConfig(t, dir, gh.URL(), `  sql:
    dialect: postgres
    path: `+out)

	sweepOnce(t, cfg)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the SQL sink wrote nothing: %v", err)
	}
	statements := sqlStatements(string(raw))
	if len(statements) < 100 {
		t.Fatalf("only %d statements were written", len(statements))
	}

	check := sqlStatementCheck{tables: map[string]bool{}}
	for _, s := range statements {
		check.statement(t, s)
	}
	if check.creates == 0 || check.inserts == 0 {
		t.Fatalf("%d CREATE and %d INSERT statements", check.creates, check.inserts)
	}
	// Every identifier is quoted, because user, type and state are tag names
	// here and reserved words there.
	if !strings.Contains(string(raw), `"state"`) || !strings.Contains(string(raw), `"user"`) {
		t.Errorf("the reserved tag names were not written as quoted identifiers")
	}

	psql := findPsql(t)
	if psql == nil {
		t.Skip("no psql that can reach a local server, so the load half is skipped")
	}
	loadIntoPostgres(t, psql, string(raw), len(check.tables), check.trafficInserts)
}

// sqlStatementCheck checks the emitted statements in the order the sink wrote
// them and counts them as it goes: an INSERT is only valid after the CREATE
// TABLE for its table, and the load half needs to know how many tables and
// traffic rows there were.
type sqlStatementCheck struct {
	creates, inserts, trafficInserts int
	// tables is every table declared so far.
	tables map[string]bool
}

func (c *sqlStatementCheck) statement(t *testing.T, s string) {
	t.Helper()
	switch {
	case strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS "):
		c.create(t, s)
	case strings.HasPrefix(s, "INSERT INTO "):
		c.insert(t, s)
	case strings.HasPrefix(s, "ALTER TABLE "):
	default:
		t.Errorf("unexpected statement: %s", s)
	}
}

func (c *sqlStatementCheck) create(t *testing.T, s string) {
	t.Helper()
	c.creates++
	c.tables[sqlQuotedName(t, s, "CREATE TABLE IF NOT EXISTS ")] = true
	if !strings.Contains(s, `"time" TIMESTAMPTZ NOT NULL`) {
		t.Errorf("no time column in: %s", s)
	}
	// InfluxDB's series key spelled as a constraint. Without it a rewrite of
	// the traffic window accumulates rather than converges.
	if !strings.Contains(s, `PRIMARY KEY ("time"`) {
		t.Errorf("the primary key does not start at the time column: %s", s)
	}
}

func (c *sqlStatementCheck) insert(t *testing.T, s string) {
	t.Helper()
	c.inserts++
	name := sqlQuotedName(t, s, "INSERT INTO ")
	if !c.tables[name] {
		t.Errorf("INSERT into %q before its CREATE TABLE", name)
	}
	if name == "gh_traffic" {
		c.trafficInserts++
	}
	// DO UPDATE rather than DO NOTHING: today's traffic row is rewritten with
	// a higher count on every sweep.
	if !strings.Contains(s, ") DO UPDATE SET ") || !strings.Contains(s, "ON CONFLICT (") {
		t.Errorf("an INSERT does not converge on conflict: %s", s)
	}
	if !strings.Contains(s, "EXCLUDED.") {
		t.Errorf("the update does not take the new values: %s", s)
	}
}

// loadIntoPostgres pipes the emitted SQL into a scratch database, twice, and
// checks that the second copy replaces rows rather than adding them. Every
// load runs in a transaction that is rolled back, and the database is dropped
// afterwards.
func loadIntoPostgres(t *testing.T, psql *psqlClient, sql string, wantTables, wantTraffic int) {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	db := "ghc_e2e_" + hex.EncodeToString(suffix)
	if _, err := psql.run("postgres", "", "-c", "CREATE DATABASE "+db); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := psql.run("postgres", "", "-c", "DROP DATABASE IF EXISTS "+db); err != nil {
			t.Errorf("the scratch database was left behind: %v", err)
		}
	})

	// The traffic day the fixture spells as an offset, so this asks for the row
	// the sweep actually wrote rather than for a date that leaves the fixture a
	// few weeks from now.
	queries := `SELECT count(*) FROM "gh_traffic";
SELECT "count" FROM "gh_traffic" WHERE "kind" = 'views' AND "time" = '` +
		fakegh.DaysAgoDate(fakegh.TrafficDaysAgo) + `T00:00:00Z';
SELECT count(*) FROM pg_tables WHERE schemaname = 'public';
`
	load := func(copies int) []string {
		t.Helper()
		var b strings.Builder
		b.WriteString("BEGIN;\n")
		for range copies {
			b.WriteString(sql)
		}
		b.WriteString(queries)
		b.WriteString("ROLLBACK;\n")
		out, err := psql.run(db, b.String(), "-tA")
		if err != nil {
			t.Fatalf("the emitted SQL did not load: %v", err)
		}
		var rows []string
		for l := range strings.SplitSeq(out, "\n") {
			if strings.TrimSpace(l) != "" {
				rows = append(rows, strings.TrimSpace(l))
			}
		}
		if len(rows) != 3 {
			t.Fatalf("expected three answers from the queries, got %v", rows)
		}
		return rows
	}

	once := load(1)
	if got := atoiOrFail(t, once[0]); got != wantTraffic {
		t.Errorf("gh_traffic holds %d rows, want the %d the file inserts", got, wantTraffic)
	}
	if once[1] != "120" {
		t.Errorf("the traffic day loaded as %q, want the fixture's 120", once[1])
	}
	if got := atoiOrFail(t, once[2]); got != wantTables {
		t.Errorf("%d tables were created, want %d, one per measurement", got, wantTables)
	}

	// The same file loaded twice is the rewrite the design rests on: the same
	// rows, updated, not a second copy of them.
	twice := load(2)
	if twice[0] != once[0] {
		t.Errorf("loading the file twice left %s rows in gh_traffic, want %s", twice[0], once[0])
	}
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%q is not a number: %v", s, err)
	}
	return n
}

// sqlStatements returns the non-empty lines. The sink writes one statement per
// line, which is what lets a rotated file be replayed with a shell.
func sqlStatements(raw string) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// sqlQuotedName reads the double quoted identifier that follows prefix.
func sqlQuotedName(t *testing.T, stmt, prefix string) string {
	t.Helper()
	rest := strings.TrimPrefix(stmt, prefix)
	if !strings.HasPrefix(rest, `"`) {
		t.Fatalf("the table name is not a quoted identifier: %s", stmt)
	}
	end := strings.Index(rest[1:], `"`)
	if end < 0 {
		t.Fatalf("unterminated identifier: %s", stmt)
	}
	return rest[1 : 1+end]
}
