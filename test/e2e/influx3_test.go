package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// influx3 is an InfluxDB 3 Core held in memory: tables made by the line
// protocol written to it, a catalog read back from them, and the table delete
// InfluxDB 3 answers with a rename. The capture servers of this suite answer
// what a test scripted, which proves what a run sends; this one answers from
// what was written, so a test can write a store the way an earlier release
// did, run this one against it, and read what it left.
//
// Every answer is the one InfluxDB 3.11.2 Core gave in a throwaway container
// on 2026-09-29, to the same writes and the same queries:
//
//   - a tag is a column of type Dictionary(Int32, Utf8), a string field
//     Utf8, an integer Int64, a float Float64, a boolean Boolean, and the
//     time Timestamp(ns); the catalog lists a table's columns by name;
//   - a column keeps the kind it was first written with, and a batch holding
//     one line that disagrees is refused whole with a 400;
//   - a query of a table that is not there is refused with a 400, and one
//     naming a column the table lacks with a 500;
//   - a delete renames the table <name>-<UTC instant>, which stays listed and
//     queryable; the name is free at once, a delete of it after that answers
//     404, and a delete of the renamed table 409.
//
// A query it does not know fails the test that sent it, rather than being
// answered with a guess.
type influx3 struct {
	t        *testing.T
	database string

	mu      sync.Mutex
	tables  map[string]*influxTable
	deleted map[string]bool
	// deletedAt is when each table it holds deleted was deleted.
	deletedAt map[string]time.Time
	sent      []string
	// wrote is the measurement of every line a write it took carried.
	wrote []string
}

type influxTable struct {
	// kinds is the data type of each column, the time's among them.
	kinds map[string]string
	// rows is keyed by the row's time and tag set, which is what InfluxDB
	// keys a point by: a second write of the same key replaces the fields
	// it carries.
	rows map[string]*influxRow
}

type influxRow struct {
	time   int64
	tags   map[string]string
	fields map[string]any
}

// newInflux3 starts an empty one holding the one database the sinks name.
func newInflux3(t *testing.T, database string) (*influx3, string) {
	t.Helper()
	s := &influx3{t: t, database: database, tables: map[string]*influxTable{}, deleted: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *influx3) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, r.Method+" "+r.URL.Path+" "+q.Get("q"))
	if q.Get("db") == "_internal" && r.URL.Path == "/api/v3/query_sql" {
		s.system(w, q.Get("q"))
		return
	}
	if db := q.Get("db") + q.Get("bucket"); r.URL.Path != "/ping" && db != s.database {
		http.Error(w, "database not found: "+db, http.StatusNotFound)
		return
	}
	switch {
	case r.URL.Path == "/ping":
		w.Header().Set("X-Influxdb-Build", "Core")
		w.Header().Set("X-Influxdb-Version", "3.11.2")
		_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
	case r.URL.Path == "/api/v2/write" && r.Method == http.MethodPost:
		s.write(w, string(body))
	case r.URL.Path == "/api/v3/query_sql" && r.Method == http.MethodGet:
		s.query(w, q.Get("q"))
	case r.URL.Path == "/api/v3/configure/table" && r.Method == http.MethodDelete:
		s.delete(w, q.Get("table"))
	default:
		s.t.Errorf("InfluxDB 3 was sent %s %s, which the model does not answer", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// columnKind is the data type InfluxDB 3 gives a field's value.
func columnKind(v any) string {
	switch v.(type) {
	case int64:
		return "Int64"
	case float64:
		return "Float64"
	case bool:
		return "Boolean"
	default:
		return "Utf8"
	}
}

const tagKind = "Dictionary(Int32, Utf8)"

// write takes a batch whole or refuses it whole.
func (s *influx3) write(w http.ResponseWriter, body string) {
	var points []lpPoint
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := parseLPLine(line)
		if err != nil {
			http.Error(w, "line protocol parse failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		points = append(points, p)
	}
	kinds, refusal := s.kindsAfter(points)
	if refusal != "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, refusal)
		return
	}
	for _, p := range points {
		s.wrote = append(s.wrote, p.Measurement)
		table := s.tables[p.Measurement]
		if table == nil {
			table = &influxTable{rows: map[string]*influxRow{}}
			s.tables[p.Measurement] = table
		}
		table.kinds = kinds[p.Measurement]
		key := rowKey(p.Time, p.Tags)
		row := table.rows[key]
		if row == nil {
			row = &influxRow{time: p.Time, tags: p.Tags, fields: map[string]any{}}
			table.rows[key] = row
		}
		maps.Copy(row.fields, p.Fields)
	}
	w.WriteHeader(http.StatusNoContent)
}

// kindsAfter is every column's kind once a batch is taken, per table, or the
// refusal InfluxDB answers the batch with when a line disagrees with a kind
// already fixed.
func (s *influx3) kindsAfter(points []lpPoint) (kinds map[string]map[string]string, refusal string) {
	kinds = map[string]map[string]string{}
	for _, p := range points {
		have := kinds[p.Measurement]
		if have == nil {
			have = map[string]string{"time": "Timestamp(ns)"}
			if table := s.tables[p.Measurement]; table != nil {
				maps.Copy(have, table.kinds)
			}
			kinds[p.Measurement] = have
		}
		want := map[string]string{}
		for k := range p.Tags {
			want[k] = tagKind
		}
		for k, v := range p.Fields {
			want[k] = columnKind(v)
		}
		for col, kind := range want {
			if was, known := have[col]; known && was != kind {
				return nil, fmt.Sprintf(`{"code":"invalid","message":"write buffer error: line protocol parse `+
					`failed: invalid column type for column '%s', expected %s, got %s"}`, col, was, kind)
			}
			have[col] = kind
		}
	}
	return kinds, ""
}

func rowKey(at int64, tags map[string]string) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(at, 10))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		b.WriteString("," + k + "=" + tags[k])
	}
	return b.String()
}

// The queries a run sends, and nothing wider: each is matched whole.
var (
	queryTables = regexp.MustCompile(`^SELECT table_name FROM information_schema\.tables ` +
		`WHERE table_schema = 'iox' ORDER BY table_name$`)
	queryColumns = regexp.MustCompile(`^SELECT column_name(, data_type)? FROM information_schema\.columns ` +
		`WHERE table_schema = 'iox' AND table_name = '([^']+)'$`)
	querySpan     = regexp.MustCompile(`^SELECT count\(\*\) AS n, min\(time\) AS oldest FROM "([^"]+)"$`)
	queryDistinct = regexp.MustCompile(`^SELECT DISTINCT (.+?) FROM "([^"]+)"` +
		`(?: WHERE "([^"]+)" IS NULL OR lower\("[^"]+"\) <> lower\('([^']*)'\))?( ORDER BY v)?$`)
	selected = regexp.MustCompile(`^"([^"]+)" AS (v\d*)$`)
	// querySystem is when the server purges a table it soft deleted, which
	// 3.11.2 keeps in the system table of its _internal database, and
	// queryDeleted the same for every table it deleted, which -migrate asks
	// to find the copies nothing will purge.
	querySystem = regexp.MustCompile(`^SELECT hard_deletion_time FROM system\.tables ` +
		`WHERE database_name = '([^']+)' AND table_name = '([^']+)'$`)
	queryDeleted = regexp.MustCompile(`^SELECT table_name, hard_deletion_time FROM system\.tables ` +
		`WHERE database_name = '([^']+)' AND deleted$`)
)

// system answers the questions asked of _internal with the 72 hours 3.11.2
// schedules a soft deleted table's hard deletion for (measured).
func (s *influx3) system(w http.ResponseWriter, q string) {
	rows := []map[string]any{}
	purged := func(at time.Time) string { return at.Add(72 * time.Hour).Format(time.RFC3339) }
	switch m := querySystem.FindStringSubmatch(q); {
	case m != nil:
		if at, deleted := s.deletedAt[m[2]]; deleted && m[1] == s.database {
			rows = append(rows, map[string]any{"hard_deletion_time": purged(at)})
		}
	case queryDeleted.MatchString(q):
		if queryDeleted.FindStringSubmatch(q)[1] == s.database {
			for _, name := range slices.Sorted(maps.Keys(s.deletedAt)) {
				rows = append(rows, map[string]any{"table_name": name, "hard_deletion_time": purged(s.deletedAt[name])})
			}
		}
	default:
		s.t.Errorf("InfluxDB 3 was asked %q of _internal, which the model does not answer", q)
		http.Error(w, "the model does not answer this query", http.StatusBadRequest)
		return
	}
	s.answer(w, rows)
}

func (s *influx3) query(w http.ResponseWriter, q string) {
	switch {
	case queryTables.MatchString(q):
		rows := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(s.tables)) {
			rows = append(rows, map[string]any{"table_name": name})
		}
		s.answer(w, rows)
	case queryColumns.MatchString(q):
		m := queryColumns.FindStringSubmatch(q)
		rows := []map[string]any{}
		if table := s.tables[m[2]]; table != nil {
			for _, col := range slices.Sorted(maps.Keys(table.kinds)) {
				row := map[string]any{"column_name": col}
				if m[1] != "" {
					row["data_type"] = table.kinds[col]
				}
				rows = append(rows, row)
			}
		}
		s.answer(w, rows)
	case querySpan.MatchString(q):
		table := s.table(w, querySpan.FindStringSubmatch(q)[1])
		if table == nil {
			return
		}
		oldest := int64(0)
		for _, r := range table.rows {
			if oldest == 0 || r.time < oldest {
				oldest = r.time
			}
		}
		s.answer(w, []map[string]any{{
			"n": len(table.rows), "oldest": time.Unix(0, oldest).UTC().Format("2006-01-02T15:04:05.999999999"),
		}})
	case queryDistinct.MatchString(q):
		s.distinct(w, queryDistinct.FindStringSubmatch(q))
	default:
		s.t.Errorf("InfluxDB 3 was asked %q, which the model does not answer", q)
		http.Error(w, "the model does not answer this query", http.StatusBadRequest)
	}
}

// distinct is SELECT DISTINCT over tag columns, each named "tag" AS v<n>.
func (s *influx3) distinct(w http.ResponseWriter, m []string) {
	table := s.table(w, m[2])
	if table == nil {
		return
	}
	var cols, as []string
	for part := range strings.SplitSeq(m[1], ", ") {
		sel := selected.FindStringSubmatch(part)
		if sel == nil {
			s.t.Errorf("InfluxDB 3 was asked for %q, which the model does not answer", part)
			http.Error(w, "the model does not answer this selection", http.StatusBadRequest)
			return
		}
		if _, has := table.kinds[sel[1]]; !has {
			http.Error(w, "Schema error: No field named "+sel[1]+".", http.StatusInternalServerError)
			return
		}
		cols, as = append(cols, sel[1]), append(as, sel[2])
	}
	seen := map[string]map[string]any{}
	for _, r := range table.rows {
		if except, has := r.tags[m[3]]; m[3] != "" && has && strings.EqualFold(except, m[4]) {
			continue
		}
		row := map[string]any{}
		var key strings.Builder
		for n, col := range cols {
			v, ok := r.tags[col]
			if ok {
				row[as[n]] = v
			}
			key.WriteString(strconv.FormatBool(ok) + v + "\x00")
		}
		seen[key.String()] = row
	}
	rows := make([]map[string]any, 0, len(seen))
	for _, key := range slices.Sorted(maps.Keys(seen)) {
		rows = append(rows, seen[key])
	}
	s.answer(w, rows)
}

// table is the named table, or the refusal InfluxDB answers without one.
func (s *influx3) table(w http.ResponseWriter, name string) *influxTable {
	table := s.tables[name]
	if table == nil {
		http.Error(w, "Error during planning: table 'public.iox."+name+"' not found", http.StatusBadRequest)
	}
	return table
}

func (s *influx3) answer(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rows); err != nil {
		s.t.Errorf("answering a query: %v", err)
	}
}

// delete sets the table aside under the name InfluxDB gives it.
func (s *influx3) delete(w http.ResponseWriter, name string) {
	switch {
	case s.deleted[name]:
		http.Error(w, "attempted to modify resource that was already deleted: "+name, http.StatusConflict)
	case s.tables[name] == nil:
		http.Error(w, "Table "+name+" not in DB schema for "+s.database, http.StatusNotFound)
	default:
		now := time.Now().UTC()
		aside := name + "-" + now.Format("20060102T150405")
		s.tables[aside], s.deleted[aside] = s.tables[name], true
		if s.deletedAt == nil {
			s.deletedAt = map[string]time.Time{}
		}
		s.deletedAt[aside] = now.Truncate(time.Second)
		delete(s.tables, name)
	}
}

// forget drops the record of what the store was sent, keeping what it
// holds, so a test's own writes are not taken for the binary's.
func (s *influx3) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent, s.wrote = nil, nil
}

// requests is everything the store was sent, method, path and query.
func (s *influx3) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// measurementsWritten is the measurement of every line written, in order.
func (s *influx3) measurementsWritten() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.wrote)
}

// changes is every request that could change what the store holds.
func (s *influx3) changes() []string {
	var out []string
	for _, r := range s.requests() {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			out = append(out, r)
		}
	}
	return out
}

// dump is everything the store holds, one line per row in a fixed order, so
// a before and an after compare whole.
func (s *influx3) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(s.tables)) {
		table := s.tables[name]
		fmt.Fprintf(&b, "%s %v\n", name, table.kinds)
		for _, key := range slices.Sorted(maps.Keys(table.rows)) {
			fmt.Fprintf(&b, "  %s %v\n", key, table.rows[key].fields)
		}
	}
	return b.String()
}

// tableNames is every table, set-asides among them.
func (s *influx3) tableNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.tables))
}

// rowsOf is a table's rows, or nil when it is not there.
func (s *influx3) rowsOf(name string) []influxRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	table := s.tables[name]
	if table == nil {
		return nil
	}
	out := make([]influxRow, 0, len(table.rows))
	for _, key := range slices.Sorted(maps.Keys(table.rows)) {
		out = append(out, *table.rows[key])
	}
	return out
}

// kindsOf is the data type of each column of a table.
func (s *influx3) kindsOf(name string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if table := s.tables[name]; table != nil {
		return maps.Clone(table.kinds)
	}
	return nil
}
