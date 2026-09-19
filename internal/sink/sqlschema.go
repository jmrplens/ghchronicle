package sink

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The schema two sinks share.
//
// One writes the statements to a file for somebody to replay with psql, the
// other sends them down a connection. What must not differ between them is the
// shape: the same table per measurement, the same columns, the same primary
// key, and the same rule about which of a point's fields become a row. So the
// shape lives here and each sink only decides how to say it, one as text and
// one as parameters.

// sqlSchema remembers what a destination has already been told, per
// measurement, so a CREATE TABLE goes out once and a column that turns up
// later goes out as an ALTER TABLE.
type sqlSchema struct {
	tables map[string]*sqlTable
}

func newSQLSchema() *sqlSchema { return &sqlSchema{tables: map[string]*sqlTable{}} }

// forget drops everything this schema knew, for a destination that has started
// again: a rotated file declares nothing yet.
func (s *sqlSchema) forget() { s.tables = map[string]*sqlTable{} }

// declare is the statements a measurement needs before its points can land,
// and none when the destination already knows the shape.
func (s *sqlSchema) declare(measurement string, sh *sqlShape) []string {
	tbl := s.tables[measurement]
	if tbl == nil {
		return []string{s.create(measurement, sh)}
	}
	var out []string
	// A tag first seen after the table was declared cannot join the primary
	// key without rewriting it, so it becomes a plain column. That only
	// happens when a collector changes its tag set between sweeps.
	for _, t := range sh.tags {
		if _, known := tbl.cols[t]; known {
			continue
		}
		tbl.cols[t] = "TEXT"
		out = append(out, fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT NOT NULL DEFAULT '';",
			ident(measurement), ident(t),
		))
	}
	for _, f := range sortedKeys3(sh.fields) {
		if _, known := tbl.cols[f]; known {
			continue
		}
		tbl.cols[f] = sh.fields[f]
		out = append(out, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s;",
			ident(measurement), ident(f), sh.fields[f]))
	}
	return out
}

// create is the first statement for a measurement, and records what it said.
func (s *sqlSchema) create(measurement string, sh *sqlShape) string {
	tbl := &sqlTable{
		key:  append([]string{"time"}, sh.tags...),
		cols: map[string]string{"time": "TIMESTAMPTZ"},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (%s TIMESTAMPTZ NOT NULL",
		ident(measurement), ident("time"))
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
	return b.String()
}

// key is the primary key of a measurement's table, which is what an upsert
// conflicts on.
func (s *sqlSchema) key(measurement string, sh *sqlShape) []string {
	if tbl := s.tables[measurement]; tbl != nil {
		return tbl.key
	}
	return append([]string{"time"}, sh.tags...)
}

// sqlCells is what one point writes: the columns, and for each the value to
// store. A field with no column of its own, and a string with nothing in it,
// are left out, because a row is what the point carries rather than a row of
// placeholders. The second return is false when nothing but the key survived,
// which is not a row worth writing.
func sqlCells(p Point, sh *sqlShape) (cols []string, vals []any, ok bool) {
	cols = []string{"time"}
	vals = []any{sqlArg(stampOf(p))}
	for _, t := range sh.tags {
		cols = append(cols, t)
		vals = append(vals, p.Tags[t])
	}
	fields := 0
	for _, f := range sortedKeys(p.Fields) {
		if _, hasColumn := sh.fields[f]; !hasColumn {
			continue // a clash with a tag, or a type with no column
		}
		v := p.Fields[f]
		if s, isText := v.(string); isText && s == "" {
			continue
		}
		cols = append(cols, f)
		vals = append(vals, sqlArg(v))
		fields++
	}
	return cols, vals, fields > 0
}

// sqlArg is the value a cell stores, with the three cases that become NULL:
// a float that is not a number, an infinity, and a time nobody set.
func sqlArg(v any) any {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil
		}
	case time.Time:
		if t.IsZero() {
			return nil
		}
		// PostgreSQL keeps microseconds; the nanoseconds are rounded away on
		// the way in, which two points a nanosecond apart will never notice.
		return t.UTC()
	}
	return v
}

// sqlLiteral renders a cell for a destination that takes text rather than
// parameters. It is the other half of sqlArg: the same value, said instead of
// sent.
func sqlLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return formatFloat(t)
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return quote(t)
	case time.Time:
		return quote(t.Format(time.RFC3339Nano)) + "::timestamptz"
	}
	return "NULL"
}
