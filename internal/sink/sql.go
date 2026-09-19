package sink

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// SQL writes INSERT statements for a PostgreSQL database, to a file or to
// standard output, so a user pipes them into psql.
//
// This is the sink for the Grafana user who self-hosts PostgreSQL or
// TimescaleDB and runs no InfluxDB. The tool cannot speak the PostgreSQL wire
// protocol without a driver, and a driver is a dependency this repository
// does not take, so it emits the SQL and leaves the connection to psql. The
// text is the contract: a table per measurement, a TEXT column per tag, a
// typed column per field, and a primary key on the time plus the tag columns.
// That key is what makes a rewrite of the traffic window converge instead of
// accumulate, exactly as InfluxDB's series key does.
//
// Each file starts with its own CREATE TABLE statements, and a rotated file
// starts them again, so any one file can be replayed on its own.
type SQL struct {
	// Dialect is "postgres". It is recorded rather than assumed so a second
	// dialect has somewhere to live.
	Dialect string

	mu sync.Mutex
	// file is the rotating destination; nil when writing to w directly.
	file *File
	// w is standard output, or whatever the tests hand in.
	w *bufio.Writer
	// schema remembers what the current file has already declared. It is the
	// same one the connecting sink keeps, so the two cannot drift into
	// writing different tables for the same points.
	schema *sqlSchema
}

type sqlTable struct {
	// key is the primary key: "time", then the tag columns in name order.
	key []string
	// cols maps every declared column to its type.
	cols map[string]string
}

// NewSQL returns a sink. A path of "-" means standard output; anything else
// is a file rotated by size, as the file sink does.
func NewSQL(dialect, path string, maxBytes int64, keep int) *SQL {
	if dialect == "" {
		dialect = "postgres"
	}
	s := &SQL{Dialect: dialect, schema: newSQLSchema()}
	if path == "-" {
		s.w = bufio.NewWriter(os.Stdout)
	} else {
		s.file = NewFile(path, "raw", maxBytes, keep)
	}
	return s
}

func (s *SQL) Name() string { return "sql" }

func (s *SQL) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	if s.w != nil {
		return s.w.Flush()
	}
	return nil
}

// Write emits one INSERT per point. A point that builds no statement, which is
// one with no column to set, is not counted as written.
func (s *SQL) Write(_ context.Context, points []Point) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schema == nil {
		s.schema = newSQLSchema()
	}
	if s.file != nil {
		if err := s.file.open(); err != nil {
			return 0, err
		}
	}
	shapes := sqlShapes(points)
	written := 0
	for _, p := range points {
		stmt := s.insert(p, shapes[p.Measurement])
		if stmt == "" {
			continue
		}
		// Declared per point rather than per batch, because a rotation in
		// the middle of the batch starts a new file that has to declare the
		// table again before the next INSERT lands in it.
		for _, ddl := range s.schema.declare(p.Measurement, shapes[p.Measurement]) {
			if err := s.emit(ddl); err != nil {
				return written, err
			}
		}
		if err := s.emit(stmt); err != nil {
			return written, err
		}
		written++
	}
	if s.w != nil {
		return written, s.w.Flush()
	}
	return written, nil
}

// sqlShape is the union of what a batch carries for one measurement. The
// union rather than the first point, because the first sweep of a family is
// one batch and a column missing from the first row is usually present in the
// tenth.
type sqlShape struct {
	tags   []string
	fields map[string]string
}

func sqlShapes(points []Point) map[string]*sqlShape {
	shapes := map[string]*sqlShape{}
	for _, p := range points {
		sh := shapes[p.Measurement]
		if sh == nil {
			sh = &sqlShape{fields: map[string]string{}}
			shapes[p.Measurement] = sh
		}
		for k := range p.Tags {
			if !slices.Contains(sh.tags, k) {
				sh.tags = append(sh.tags, k)
			}
		}
		for k, v := range p.Fields {
			if _, clash := p.Tags[k]; clash {
				continue // the tag wins, as in the line protocol
			}
			if _, seen := sh.fields[k]; seen {
				continue // the first type seen is the column's type
			}
			if t := sqlType(v); t != "" {
				sh.fields[k] = t
			}
		}
	}
	for _, sh := range shapes {
		sort.Strings(sh.tags)
	}
	return shapes
}

// insert renders one point. It returns "" for a point with no usable field,
// which is not a point, the same rule the line protocol applies.
// insert renders one point. It returns "" for a point with no usable field,
// which is not a point, the same rule the line protocol applies.
func (s *SQL) insert(p Point, sh *sqlShape) string {
	cols, vals, ok := sqlCells(p, sh)
	if !ok {
		return ""
	}
	text := make([]string, len(vals))
	for i, v := range vals {
		text[i] = sqlLiteral(v)
	}
	var updates []string
	for _, c := range cols {
		if c == "time" || slices.Contains(sh.tags, c) {
			continue
		}
		updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", ident(c), ident(c)))
	}
	// DO UPDATE rather than DO NOTHING: today's traffic row is rewritten with
	// a higher count on every sweep, and a row frozen at its first value
	// would be the one bug the whole dated-point design exists to avoid.
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s;",
		ident(p.Measurement), identList(cols), strings.Join(text, ", "),
		identList(s.schema.key(p.Measurement, sh)), strings.Join(updates, ", "))
}

func (s *SQL) emit(stmt string) error {
	if s.file == nil {
		_, err := s.w.WriteString(stmt + "\n")
		return err
	}
	rotated, err := s.file.appendLine(stmt)
	if rotated {
		// The new file has declared nothing yet.
		s.schema.forget()
	}
	return err
}

func sqlType(v any) string {
	switch v.(type) {
	case int, int64:
		return "BIGINT"
	case float64:
		return "DOUBLE PRECISION"
	case bool:
		return "BOOLEAN"
	case string:
		return "TEXT"
	case time.Time:
		return "TIMESTAMPTZ"
	}
	return ""
}

// sqlValue renders a literal, or "" for a value with nothing to say.
// quote renders a string literal. Doubling the quote is the whole of the
// escaping PostgreSQL needs with standard_conforming_strings on, which has
// been the default since 9.1. A NUL cannot be stored in text at all, so it
// is dropped rather than left to fail the statement.
func quote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\x00", ""), "'", "''") + "'"
}

// ident quotes an identifier. Always, because "user", "type" and "state" are
// tag names here and reserved words there.
func ident(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func identList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ident(n)
	}
	return strings.Join(out, ", ")
}

func sortedKeys3(m map[string]string) []string { return sortedKeys2(m) }
