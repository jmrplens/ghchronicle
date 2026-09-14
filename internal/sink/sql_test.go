package sink

import (
	"bufio"
	"bytes"
	"context"
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
	err := s.Write(context.Background(), []Point{
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
	if err := s.Write(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), second); err != nil {
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
	if err := s.Write(context.Background(), []Point{{
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
		if err := s.Write(context.Background(), []Point{{
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
