package teardown

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// Shape is what a store says about one measurement it may hold.
type Shape struct {
	// Exists is whether the table, or the index, is there.
	Exists bool
	// Old is the tags, of the ones asked about, that the store holds rows
	// under.
	Old []string
	// Rows is how many rows the store holds, or -1 when it does not count
	// them the way a reader would: InfluxDB 2 keeps a row per field.
	Rows int64
	// Oldest is when the earliest row is dated. Zero when there is none.
	Oldest time.Time
	// Values is, per tag asked about, the distinct values its rows hold.
	Values map[string][]string
}

// Inspector is a store that can be asked what shape a measurement is in.
// Every call reads; nothing here writes, deletes or renames.
type Inspector interface {
	// Name is the sink's name in the config.
	Name() string
	// Describe says which server answers, as a person names it.
	Describe(ctx context.Context) (string, error)
	// Shape asks about one measurement: whether the store holds it, which of
	// oldTags its rows carry as tags, how many rows it holds and since when,
	// and the distinct values of each tag in values that its rows carry.
	Shape(ctx context.Context, measurement string, oldTags, values []string) (Shape, error)
}

// Inspectors is one Inspector per configured sink whose store can be asked.
func Inspectors(cfg *config.Config) []Inspector {
	var out []Inspector
	if s := cfg.Sinks.Influx; s != nil {
		out = append(out, &influx{sink: s})
	}
	if s := cfg.Sinks.Postgres; s != nil {
		out = append(out, &postgres{sink: s})
	}
	if s := cfg.Sinks.Elasticsearch; s != nil {
		out = append(out, &elastic{sink: s})
	}
	return out
}

// ── InfluxDB ────────────────────────────────────────────────────────────────

// influxPing is what /ping says. InfluxDB 3 answers with a JSON body naming
// the product; InfluxDB 2 with a 204 and the version in a header.
type influxPing struct {
	Product string `json:"product_name"`
	Version string `json:"version"`
}

// Describe asks /ping which server this is. Measured: 3.11.2 Core and 3.11.5
// Enterprise answer {"product_name":"InfluxDB 3 Core","version":"3.11.2"}
// and the Enterprise equivalent, 2.7.12 answers 204 with X-Influxdb-Version:
// v2.7.12 and X-Influxdb-Build: OSS.
func (i *influx) Describe(ctx context.Context) (string, error) {
	if i.described != "" {
		return i.described, nil
	}
	ping, err := exchange(ctx, request{
		method: http.MethodGet, endpoint: strings.TrimSuffix(i.sink.URL, "/") + "/ping", prepare: i.token,
	}, nil)
	if err != nil {
		return "", err
	}
	var p influxPing
	_ = json.Unmarshal(ping.body, &p)
	version := strings.TrimPrefix(firstOf(p.Version, ping.header.Get("X-Influxdb-Version")), "v")
	i.v2 = strings.HasPrefix(version, "2.")
	name := p.Product
	if name == "" {
		name = strings.TrimSpace("InfluxDB " + strings.SplitN(version, ".", 2)[0] + " " +
			ping.header.Get("X-Influxdb-Build"))
	}
	i.described = strings.TrimSpace(name + " " + version)
	return i.described, nil
}

// token applies the sink's credential the way its writes do, which both
// InfluxDB 2 and InfluxDB 3 take.
func (i *influx) token(r *http.Request) {
	if i.sink.Token != "" {
		r.Header.Set("Authorization", "Token "+i.sink.Token)
	}
}

// Shape asks InfluxDB 3's catalog, or InfluxDB 2's data, which differ.
func (i *influx) Shape(ctx context.Context, measurement string, oldTags, values []string) (Shape, error) {
	if _, err := i.Describe(ctx); err != nil {
		return Shape{}, err
	}
	if i.v2 {
		return i.shapeV2(ctx, measurement, oldTags, values)
	}
	return i.shapeV3(ctx, measurement, oldTags, values)
}

// shapeV3 reads the catalog of the live table. A tag is a column of type
// Dictionary(Int32, Utf8) and a string field Utf8, measured on 3.0.0 to
// 3.11.5, and only a tag makes the old identity; a table InfluxDB has soft
// deleted is listed under another name, so the exact name matches the live
// table alone.
func (i *influx) shapeV3(ctx context.Context, measurement string, oldTags, values []string) (Shape, error) {
	var cols []struct {
		Name string `json:"column_name"`
		Type string `json:"data_type"`
	}
	if err := i.sql(ctx, "SELECT column_name, data_type FROM information_schema.columns "+
		"WHERE table_schema = 'iox' AND table_name = "+sqlString(measurement), &cols); err != nil {
		return Shape{}, err
	}
	shape := Shape{Values: map[string][]string{}}
	tags := map[string]bool{}
	for _, c := range cols {
		shape.Exists = true
		tags[c.Name] = strings.HasPrefix(c.Type, "Dictionary(")
	}
	if !shape.Exists {
		return shape, nil
	}
	for _, t := range oldTags {
		if tags[t] {
			shape.Old = append(shape.Old, t)
		}
	}
	var span []struct {
		N      int64   `json:"n"`
		Oldest *string `json:"oldest"`
	}
	if err := i.sql(ctx, "SELECT count(*) AS n, min(time) AS oldest FROM "+quoteIdent(measurement), &span); err != nil {
		return Shape{}, err
	}
	if len(span) == 1 {
		shape.Rows = span[0].N
		if span[0].Oldest != nil {
			// The JSON format writes a timestamp without a zone, in UTC.
			shape.Oldest, _ = time.Parse("2006-01-02T15:04:05.999999999", *span[0].Oldest)
		}
	}
	for _, t := range values {
		if _, has := tags[t]; !has {
			continue
		}
		var rows []struct {
			V *string `json:"v"`
		}
		if err := i.sql(ctx, "SELECT DISTINCT "+quoteIdent(t)+" AS v FROM "+quoteIdent(measurement)+
			" ORDER BY v", &rows); err != nil {
			return Shape{}, err
		}
		for _, r := range rows {
			if r.V != nil {
				shape.Values[t] = append(shape.Values[t], *r.V)
			}
		}
	}
	return shape, nil
}

// sql runs one query through /api/v3/query_sql and reads its JSON rows.
func (i *influx) sql(ctx context.Context, q string, into any) error {
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v3/query_sql?" + url.Values{
		"db": {i.sink.Bucket}, "q": {q}, "format": {"json"},
	}.Encode()
	body, _, err := i.call(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil
	}
	if err = json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("reading %q: %w", q, err)
	}
	return nil
}

// shapeV2 asks InfluxDB 2 for rows, never for keys: after a delete by
// predicate schema.measurementTagKeys still lists the tag the delete took
// away (measured on 2.7.12), while a filter on exists r.<tag> finds none.
func (i *influx) shapeV2(ctx context.Context, measurement string, oldTags, values []string) (Shape, error) {
	shape := Shape{Rows: -1, Values: map[string][]string{}}
	from := `from(bucket: ` + fluxString(i.sink.Bucket) + `) |> range(start: 0) |> filter(fn: (r) => r._measurement == ` +
		fluxString(measurement)
	oldest, err := i.flux(ctx, from+`) |> keep(columns: ["_time"]) |> group() |> min(column: "_time")`, "_time")
	if err != nil || len(oldest) == 0 {
		return shape, err
	}
	shape.Exists = true
	shape.Oldest, _ = time.Parse(time.RFC3339Nano, oldest[0])
	for _, t := range oldTags {
		rows, askErr := i.flux(ctx, from+` and exists r[`+fluxString(t)+`]) |> keep(columns: ["_time"]) |> limit(n: 1)`, "_time")
		if askErr != nil {
			return Shape{}, askErr
		}
		if len(rows) > 0 {
			shape.Old = append(shape.Old, t)
		}
	}
	for _, t := range values {
		col := fluxString(t)
		rows, askErr := i.flux(ctx, from+` and exists r[`+col+`]) |> keep(columns: [`+col+`]) |> group() |> distinct(column: `+col+`)`, "_value")
		if askErr != nil {
			return Shape{}, askErr
		}
		if len(rows) > 0 {
			slices.Sort(rows)
			shape.Values[t] = rows
		}
	}
	return shape, nil
}

// flux runs one Flux query and returns the column named of every row. The
// answer is CSV with no annotations, a header line per table.
func (i *influx) flux(ctx context.Context, query, column string) ([]string, error) {
	payload, err := json.Marshal(map[string]any{
		"query": query, "type": "flux", "dialect": map[string]any{"annotations": []string{}},
	})
	if err != nil {
		return nil, err
	}
	res, err := exchange(ctx, request{
		method:   http.MethodPost,
		endpoint: strings.TrimSuffix(i.sink.URL, "/") + "/api/v2/query?" + url.Values{"org": {i.sink.Org}}.Encode(),
		payload:  payload,
		prepare: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/csv")
			i.token(r)
		},
	}, nil)
	if err != nil {
		return nil, err
	}
	return csvColumn(res.body, column)
}

// csvColumn reads one column out of Flux's CSV, whose header is repeated
// before every table and whose tables are separated by an empty line.
func csvColumn(body []byte, column string) ([]string, error) {
	r := csv.NewReader(strings.NewReader(string(body)))
	r.FieldsPerRecord = -1
	var out []string
	at := -1
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the answer: %w", err)
		}
		if idx := slices.Index(rec, column); idx >= 0 && slices.Contains(rec, "result") {
			at = idx
			continue
		}
		if at >= 0 && at < len(rec) && rec[at] != "" {
			out = append(out, rec[at])
		}
	}
}

// ── PostgreSQL ──────────────────────────────────────────────────────────────

// Describe is the server's own version.
func (p *postgres) Describe(ctx context.Context) (string, error) {
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(ctx) }()
	var v string
	if scanErr := conn.QueryRow(ctx, "SHOW server_version").Scan(&v); scanErr != nil {
		return "", scanErr
	}
	return "PostgreSQL " + v, nil
}

// Shape asks the table in the schema the sink writes to. An old tag is a
// column with a value in some row: the sink keys a table on the tags it was
// created with and writes the empty string into a key column a point no
// longer carries, so a column whose every value is empty holds no row of the
// old shape.
func (p *postgres) Shape(ctx context.Context, measurement string, oldTags, values []string) (Shape, error) {
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return Shape{}, err
	}
	defer func() { _ = conn.Close(ctx) }()
	shape := Shape{Values: map[string][]string{}}
	cols, err := p.columns(ctx, conn, measurement)
	if err != nil || len(cols) == 0 {
		return shape, err
	}
	shape.Exists = true
	table := quoteIdent(measurement)
	var oldest *time.Time
	if scanErr := conn.QueryRow(ctx, "SELECT count(*), min(time) FROM "+table).Scan(&shape.Rows, &oldest); scanErr != nil {
		return Shape{}, scanErr
	}
	if oldest != nil {
		shape.Oldest = oldest.UTC()
	}
	for _, t := range oldTags {
		if !cols[t] {
			continue
		}
		var held bool
		if scanErr := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE "+
			quoteIdent(t)+" <> '')").Scan(&held); scanErr != nil {
			return Shape{}, scanErr
		}
		if held {
			shape.Old = append(shape.Old, t)
		}
	}
	for _, t := range values {
		if !cols[t] {
			continue
		}
		if shape.Values[t], err = distinct(ctx, conn, table, quoteIdent(t)); err != nil {
			return Shape{}, err
		}
	}
	return shape, nil
}

// columns is the table's columns in the schema the sink's CREATE TABLE lands
// in, none when there is no such table.
func (p *postgres) columns(ctx context.Context, conn *pgx.Conn, table string) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `SELECT column_name FROM information_schema.columns `+
		`WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	var name string
	_, err = pgx.ForEachRow(rows, []any{&name}, func() error {
		out[name] = true
		return nil
	})
	return out, err
}

// distinct is the values one column of a table holds, sorted.
func distinct(ctx context.Context, conn *pgx.Conn, table, column string) ([]string, error) {
	rows, err := conn.Query(ctx, "SELECT DISTINCT "+column+" FROM "+table+" ORDER BY 1")
	if err != nil {
		return nil, err
	}
	var out []string
	var v string
	_, err = pgx.ForEachRow(rows, []any{&v}, func() error {
		out = append(out, v)
		return nil
	})
	return out, err
}

// ── Elasticsearch ───────────────────────────────────────────────────────────

// Describe is the cluster's version.
func (e *elastic) Describe(ctx context.Context) (string, error) {
	body, _, err := e.call(ctx, http.MethodGet, strings.TrimSuffix(e.sink.URL, "/")+"/", nil)
	if err != nil {
		return "", err
	}
	var root struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err = json.Unmarshal(body, &root); err != nil {
		return "", fmt.Errorf("reading the cluster's answer: %w", err)
	}
	return "Elasticsearch " + root.Version.Number, nil
}

// Shape counts documents rather than reading the mapping: a field stays in
// the mapping after every document that had it is gone (measured on 9.5.3),
// so only a count says whether any row of the old shape is left.
func (e *elastic) Shape(ctx context.Context, measurement string, oldTags, values []string) (Shape, error) {
	index := strings.TrimSuffix(e.sink.URL, "/") + "/" + url.PathEscape(strings.ToLower(e.sink.Prefix+"-"+measurement))
	shape := Shape{Values: map[string][]string{}}
	total, found, err := e.count(ctx, index, nil)
	if err != nil || !found {
		return shape, err
	}
	shape.Exists, shape.Rows = true, total
	for _, t := range oldTags {
		n, _, countErr := e.count(ctx, index, map[string]any{"exists": map[string]string{"field": t}})
		if countErr != nil {
			return Shape{}, countErr
		}
		if n > 0 {
			shape.Old = append(shape.Old, t)
		}
	}
	return e.spread(ctx, index, shape, values)
}

// count is _count, with a query when one is given, and whether the index is
// there at all.
func (e *elastic) count(ctx context.Context, index string, query map[string]any) (n int64, found bool, err error) {
	var payload []byte
	if query != nil {
		if payload, err = json.Marshal(map[string]any{"query": query}); err != nil {
			return 0, false, err
		}
	}
	res, err := e.read(ctx, index+"/_count", payload)
	if err != nil || res.status == http.StatusNotFound {
		return 0, false, err
	}
	var counted struct {
		Count int64 `json:"count"`
	}
	if err = json.Unmarshal(res.body, &counted); err != nil {
		return 0, false, fmt.Errorf("reading the count: %w", err)
	}
	return counted.Count, true, nil
}

// spread asks one search for the earliest document and the distinct values
// of each tag asked about, from the keyword sub-field dynamic mapping gives
// every string.
func (e *elastic) spread(ctx context.Context, index string, shape Shape, values []string) (Shape, error) {
	aggs := map[string]any{"oldest": map[string]any{"min": map[string]string{"field": "@timestamp"}}}
	for n, t := range values {
		aggs["v"+strconv.Itoa(n)] = map[string]any{"terms": map[string]any{"field": t + ".keyword", "size": 10000}}
	}
	payload, err := json.Marshal(map[string]any{"size": 0, "aggs": aggs})
	if err != nil {
		return Shape{}, err
	}
	res, err := e.read(ctx, index+"/_search", payload)
	if err != nil {
		return Shape{}, err
	}
	var searched struct {
		Aggs map[string]struct {
			Oldest  string `json:"value_as_string"`
			Buckets []struct {
				Key string `json:"key"`
			} `json:"buckets"`
		} `json:"aggregations"`
	}
	if err = json.Unmarshal(res.body, &searched); err != nil {
		return Shape{}, fmt.Errorf("reading the search: %w", err)
	}
	shape.Oldest, _ = time.Parse(time.RFC3339Nano, searched.Aggs["oldest"].Oldest)
	for n, t := range values {
		for _, b := range searched.Aggs["v"+strconv.Itoa(n)].Buckets {
			shape.Values[t] = append(shape.Values[t], b.Key)
		}
		slices.Sort(shape.Values[t])
	}
	return shape, nil
}

// read sends a read with a JSON body, which _count and _search take on GET,
// and tolerates the 404 of an index that is not there.
func (e *elastic) read(ctx context.Context, endpoint string, payload []byte) (answer, error) {
	return exchange(ctx, request{
		method: http.MethodGet, endpoint: endpoint, payload: payload,
		prepare: func(r *http.Request) {
			if payload != nil {
				r.Header.Set("Content-Type", "application/json")
			}
			e.authorize(r)
		},
	}, map[int]bool{http.StatusNotFound: true})
}

// ── Shared ──────────────────────────────────────────────────────────────────

// sqlString is a SQL string literal. The values are measurement names from
// the registry, quoted anyway because a literal is the one place a quote in
// a name would end the statement.
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// fluxString is a Flux string literal: a backslash, a double quote and the
// start of an interpolation are the three things Flux reads inside one.
func fluxString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "${", `\${`).Replace(s) + `"`
}

// firstOf is the first of two strings that is not empty.
func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
