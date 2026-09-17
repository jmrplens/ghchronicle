package sink

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
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
	// tables remembers what the current file has already declared, per
	// measurement, so a CREATE TABLE goes out once and a column that turns
	// up later goes out as an ALTER TABLE.
	tables map[string]*sqlTable
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
	s := &SQL{Dialect: dialect, tables: map[string]*sqlTable{}}
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
	if s.tables == nil {
		s.tables = map[string]*sqlTable{}
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
		if err := s.declare(p.Measurement, shapes[p.Measurement]); err != nil {
			return written, err
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

// declare emits the CREATE TABLE the first time a measurement is seen in the
// current file, and an ALTER TABLE for each column the shape adds afterwards.
func (s *SQL) declare(measurement string, sh *sqlShape) error {
	tbl := s.tables[measurement]
	if tbl == nil {
		tbl = &sqlTable{key: append([]string{"time"}, sh.tags...), cols: map[string]string{"time": "TIMESTAMPTZ"}}
		var b strings.Builder
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (%s TIMESTAMPTZ NOT NULL", ident(measurement), ident("time"))
		for _, t := range sh.tags {
			tbl.cols[t] = "TEXT"
			fmt.Fprintf(&b, ", %s TEXT NOT NULL DEFAULT ''", ident(t))
		}
		for _, f := range sortedKeys3(sh.fields) {
			tbl.cols[f] = sh.fields[f]
			fmt.Fprintf(&b, ", %s %s", ident(f), sh.fields[f])
		}
		fmt.Fprintf(&b, ", PRIMARY KEY (%s));", identList(tbl.key))
		s.tables[measurement] = tbl
		return s.emit(b.String())
	}
	// A tag first seen after the table was declared cannot join the primary
	// key without rewriting it, so it becomes a plain column. That only
	// happens when a collector changes its tag set between sweeps.
	for _, t := range sh.tags {
		if _, ok := tbl.cols[t]; ok {
			continue
		}
		tbl.cols[t] = "TEXT"
		if err := s.emit(fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT NOT NULL DEFAULT '';",
			ident(measurement), ident(t))); err != nil {
			return err
		}
	}
	for _, f := range sortedKeys3(sh.fields) {
		if _, ok := tbl.cols[f]; ok {
			continue
		}
		tbl.cols[f] = sh.fields[f]
		if err := s.emit(fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s;",
			ident(measurement), ident(f), sh.fields[f])); err != nil {
			return err
		}
	}
	return nil
}

// insert renders one point. It returns "" for a point with no usable field,
// which is not a point, the same rule the line protocol applies.
func (s *SQL) insert(p Point, sh *sqlShape) string {
	cols := []string{"time"}
	vals := []string{sqlValue(stampOf(p))}
	for _, t := range sh.tags {
		cols = append(cols, t)
		vals = append(vals, quote(p.Tags[t]))
	}
	var updates []string
	for _, f := range sortedKeys(p.Fields) {
		if _, isTag := sh.fields[f]; !isTag {
			continue // a clash with a tag, or a type with no column
		}
		v := sqlValue(p.Fields[f])
		if v == "" {
			continue
		}
		cols = append(cols, f)
		vals = append(vals, v)
		updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", ident(f), ident(f)))
	}
	if len(updates) == 0 {
		return ""
	}
	key := append([]string{"time"}, sh.tags...)
	if tbl := s.tables[p.Measurement]; tbl != nil {
		key = tbl.key
	}
	// DO UPDATE rather than DO NOTHING: today's traffic row is rewritten with
	// a higher count on every sweep, and a row frozen at its first value
	// would be the one bug the whole dated-point design exists to avoid.
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s;",
		ident(p.Measurement), identList(cols), strings.Join(vals, ", "), identList(key), strings.Join(updates, ", "))
}

func (s *SQL) emit(stmt string) error {
	if s.file == nil {
		_, err := s.w.WriteString(stmt + "\n")
		return err
	}
	rotated, err := s.file.appendLine(stmt)
	if rotated {
		// The new file has declared nothing yet.
		s.tables = map[string]*sqlTable{}
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
func sqlValue(v any) string {
	switch t := v.(type) {
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "NULL"
		}
		return formatFloat(t)
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case string:
		if t == "" {
			return ""
		}
		return quote(t)
	case time.Time:
		if t.IsZero() {
			return "NULL"
		}
		// PostgreSQL keeps microseconds; the nanoseconds are rounded away on
		// the way in, which two points a nanosecond apart will never notice.
		return quote(t.UTC().Format(time.RFC3339Nano)) + "::timestamptz"
	}
	return ""
}

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
