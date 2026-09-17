package sink

import (
	"bufio"
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqlToBuffer() (*SQL, *bytes.Buffer) {
	var buf bytes.Buffer
	return &SQL{Dialect: "postgres", w: bufio.NewWriter(&buf)}, &buf
}

func TestSQLDeclaresTheTableOnceAndUpserts(t *testing.T) {
	s, buf := sqlToBuffer()
	at := time.Unix(0, 1700000000000000000)
	_, err := s.Write(context.Background(), []Point{
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "user": "o'reilly"},
			Fields: map[string]any{
				"stars": 3, "ratio": 0.5, "archived": false, "license": "mit",
				"pushed_at": at, "user": "ignored",
			}, Time: at,
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "b", "user": ""},
			Fields: map[string]any{"stars": int64(4)}, Time: at,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE TABLE IF NOT EXISTS "gh_repo" ("time" TIMESTAMPTZ NOT NULL, "repo" TEXT NOT NULL DEFAULT '', "user" TEXT NOT NULL DEFAULT '', "archived" BOOLEAN, "license" TEXT, "pushed_at" TIMESTAMPTZ, "ratio" DOUBLE PRECISION, "stars" BIGINT, PRIMARY KEY ("time", "repo", "user"));
INSERT INTO "gh_repo" ("time", "repo", "user", "archived", "license", "pushed_at", "ratio", "stars") VALUES ('2023-11-14T22:13:20Z'::timestamptz, 'a', 'o''reilly', FALSE, 'mit', '2023-11-14T22:13:20Z'::timestamptz, 0.5, 3) ON CONFLICT ("time", "repo", "user") DO UPDATE SET "archived" = EXCLUDED."archived", "license" = EXCLUDED."license", "pushed_at" = EXCLUDED."pushed_at", "ratio" = EXCLUDED."ratio", "stars" = EXCLUDED."stars";
INSERT INTO "gh_repo" ("time", "repo", "user", "stars") VALUES ('2023-11-14T22:13:20Z'::timestamptz, 'b', '', 4) ON CONFLICT ("time", "repo", "user") DO UPDATE SET "stars" = EXCLUDED."stars";
`
	if got := buf.String(); got != want {
		t.Errorf("sql =\n%s\nwant\n%s", got, want)
	}
}

func TestSQLAddsAColumnThatArrivesLater(t *testing.T) {
	s, buf := sqlToBuffer()
	at := time.Unix(1700000000, 0)
	first := []Point{{
		Measurement: "gh_actions_cache", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"bytes": 10}, Time: at,
	}}
	second := []Point{{
		Measurement: "gh_actions_cache", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"bytes": 12, "entries": 2}, Time: at.Add(time.Hour),
	}}
	if _, err := s.Write(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "CREATE TABLE") != 1 {
		t.Errorf("the table must be declared once per file:\n%s", out)
	}
	if !strings.Contains(out, `ALTER TABLE "gh_actions_cache" ADD COLUMN IF NOT EXISTS "entries" BIGINT;`) {
		t.Errorf("a new field must arrive as ALTER TABLE:\n%s", out)
	}
}

func TestSQLSkipsPointsWithNoUsableField(t *testing.T) {
	s, buf := sqlToBuffer()
	if _, err := s.Write(context.Background(), []Point{{
		Measurement: "gh_x",
		Tags:        map[string]string{"a": "b"}, Fields: map[string]any{"note": "", "n": nil}, Time: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be written for a point with no field:\n%s", buf.String())
	}
}

func TestSQLRotatedFileDeclaresItsOwnTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "points.sql")
	s := NewSQL("", path, 200, 3)
	at := time.Unix(1700000000, 0)
	for i := range 6 {
		if _, err := s.Write(context.Background(), []Point{{
			Measurement: "gh_repo",
			Tags:        map[string]string{"repo": "a"}, Fields: map[string]any{"stars": i},
			Time: at.Add(time.Duration(i) * time.Hour),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Every statement is longer than the cap, so each write rotates: the
	// rotated files carry the rows and each has to open with its own schema.
	for _, name := range []string{path + ".1", path + ".2"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(string(b), "CREATE TABLE IF NOT EXISTS") {
			t.Errorf("%s must be replayable on its own, but starts with %q", filepath.Base(name), firstLine(b))
		}
		if !strings.Contains(string(b), "INSERT INTO") {
			t.Errorf("%s carries no row", filepath.Base(name))
		}
	}
}

func firstLine(b []byte) string {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	return string(line)
}

// TestNewSQLChoosesItsDestination writes to standard output for "-" and to a
// rotated file otherwise, and names the dialect it was given or postgres.
func TestNewSQLChoosesItsDestination(t *testing.T) {
	s := NewSQL("", "-", 0, 0)
	if s.Dialect != "postgres" || s.w == nil || s.file != nil || s.Name() != "sql" {
		t.Errorf("NewSQL(-) = dialect %q, writer %v, file %v, name %q, want postgres to standard output",
			s.Dialect, s.w != nil, s.file != nil, s.Name())
	}
	s = NewSQL("timescale", filepath.Join(t.TempDir(), "points.sql"), 0, 0)
	if s.Dialect != "timescale" || s.w != nil || s.file == nil {
		t.Errorf("NewSQL(path) = dialect %q, writer %v, file %v, want timescale to a file", s.Dialect, s.w != nil, s.file != nil)
	}
}

// TestSQLCloseFlushesWhatIsBuffered hands the last statements to the writer on
// Close, closes a file sink's file, and has nothing to do for a sink with
// neither.
func TestSQLCloseFlushesWhatIsBuffered(t *testing.T) {
	s, buf := sqlToBuffer()
	if err := s.emit("SELECT 1;"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil || buf.String() != "SELECT 1;\n" {
		t.Errorf("Close = %v with %q written, want the buffered statement", err, buf.String())
	}

	f := NewSQL("", filepath.Join(t.TempDir(), "points.sql"), 0, 0)
	if _, err := f.Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(1, 0)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil || f.file.fh != nil {
		t.Errorf("Close = %v, open file %v, want the file closed", err, f.file.fh != nil)
	}

	if err := (&SQL{}).Close(); err != nil {
		t.Errorf("Close of a sink with no destination = %v", err)
	}
}

// TestSQLAddsATagThatArrivesLaterAsAPlainColumn alters the table for every tag
// and field the second batch brings, and keeps the primary key the table was
// declared with, since a tag added later cannot join it.
func TestSQLAddsATagThatArrivesLaterAsAPlainColumn(t *testing.T) {
	s, buf := sqlToBuffer()
	at := time.Unix(1700000000, 0)
	if _, err := s.Write(context.Background(), []Point{{
		Measurement: "gh_x", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"v": 1}, Time: at,
	}}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := s.Write(context.Background(), []Point{{
		Measurement: "gh_x", Tags: map[string]string{"repo": "a", "branch": "main"},
		Fields: map[string]any{"v": 2, "size": 2.5, "ok": true}, Time: at,
	}}); err != nil {
		t.Fatal(err)
	}
	want := `ALTER TABLE "gh_x" ADD COLUMN IF NOT EXISTS "branch" TEXT NOT NULL DEFAULT '';
ALTER TABLE "gh_x" ADD COLUMN IF NOT EXISTS "ok" BOOLEAN;
ALTER TABLE "gh_x" ADD COLUMN IF NOT EXISTS "size" DOUBLE PRECISION;
INSERT INTO "gh_x" ("time", "branch", "repo", "ok", "size", "v") VALUES ('2023-11-14T22:13:20Z'::timestamptz, 'main', 'a', TRUE, 2.5, 2) ON CONFLICT ("time", "repo") DO UPDATE SET "ok" = EXCLUDED."ok", "size" = EXCLUDED."size", "v" = EXCLUDED."v";
`
	if got := buf.String(); got != want {
		t.Errorf("sql =\n%s\nwant\n%s", got, want)
	}
}

// TestSQLReportsAStatementItCannotWrite returns the first failed write,
// whether it is the table, a column added later or the row itself, rather than
// reporting a batch as written.
func TestSQLReportsAStatementItCannotWrite(t *testing.T) {
	at := time.Unix(1700000000, 0)
	first := []Point{{Measurement: "gh_x", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"v": 1}, Time: at}}
	// A buffer smaller than any statement, so the first one reaches the writer.
	broken := func() *bufio.Writer { return bufio.NewWriterSize(failingWriter{}, 16) }
	for name, second := range map[string][]Point{
		"the row":             first,
		"a tag added later":   {{Measurement: "gh_x", Tags: map[string]string{"repo": "a", "branch": "main"}, Fields: map[string]any{"v": 1}, Time: at}},
		"a field added later": {{Measurement: "gh_x", Tags: map[string]string{"repo": "a"}, Fields: map[string]any{"v": 1, "w": 2}, Time: at}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := sqlToBuffer()
			if _, err := s.Write(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			s.w = broken()
			if _, err := s.Write(context.Background(), second); err == nil {
				t.Error("Write reported success through a writer that refused it")
			}
		})
	}
	s := &SQL{w: broken()}
	if _, err := s.Write(context.Background(), first); err == nil {
		t.Error("Write reported success when the table could not be declared")
	}
}

// TestSQLDeclaresEveryFieldThatArrivesLater emits an ALTER TABLE for each new
// field, not only the first.
func TestSQLDeclaresEveryFieldThatArrivesLater(t *testing.T) {
	s, buf := sqlToBuffer()
	at := time.Unix(1700000000, 0)
	for _, fields := range []map[string]any{{"v": 1}, {"v": 1, "w": 2, "x": 3}} {
		if _, err := s.Write(context.Background(), []Point{{Measurement: "gh_x", Fields: fields, Time: at}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range []string{`"w" BIGINT;`, `"x" BIGINT;`} {
		if !strings.Contains(buf.String(), `ALTER TABLE "gh_x" ADD COLUMN IF NOT EXISTS `+col) {
			t.Errorf("no ALTER TABLE for %s in:\n%s", col, buf.String())
		}
	}
}

// TestSQLReportsAFileItCannotOpen fails the write when the file cannot be
// created.
func TestSQLReportsAFileItCannotOpen(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSQL("", filepath.Join(blocker, "points.sql"), 0, 0)
	if _, err := s.Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(1, 0)}}); err == nil {
		t.Error("Write reported success under a path that is a file")
	}
}

// TestSQLTypesAndLiterals pins the column type and the literal of every value
// a field can hold. A float that is not a number has no literal PostgreSQL
// accepts in a DOUBLE PRECISION column without quotes, and an unset time is
// no time at all, so both become NULL rather than failing the statement.
func TestSQLTypesAndLiterals(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 5000, time.UTC)
	for _, tc := range []struct {
		v            any
		typ, literal string
	}{
		{3, "BIGINT", "3"},
		{int64(-4), "BIGINT", "-4"},
		{2.5, "DOUBLE PRECISION", "2.5"},
		{math.NaN(), "DOUBLE PRECISION", "NULL"},
		{math.Inf(1), "DOUBLE PRECISION", "NULL"},
		{math.Inf(-1), "DOUBLE PRECISION", "NULL"},
		{true, "BOOLEAN", "TRUE"},
		{false, "BOOLEAN", "FALSE"},
		{"it's\x00", "TEXT", "'it''s'"},
		{"", "TEXT", ""},
		{at, "TIMESTAMPTZ", "'2026-09-01T12:00:00.000005Z'::timestamptz"},
		{time.Time{}, "TIMESTAMPTZ", "NULL"},
		{nil, "", ""},
		{[]int{1}, "", ""},
	} {
		if got := sqlType(tc.v); got != tc.typ {
			t.Errorf("sqlType(%#v) = %q, want %q", tc.v, got, tc.typ)
		}
		if got := sqlValue(tc.v); got != tc.literal {
			t.Errorf("sqlValue(%#v) = %q, want %q", tc.v, got, tc.literal)
		}
	}
}
