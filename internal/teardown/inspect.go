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
	// Rows is how many rows the store holds, or -1 when they were not
	// counted: InfluxDB 2 keeps a row per field, a store that holds no old
	// tag is not asked, and InfluxDB 3 Core refuses to count a table stored
	// in more Parquet files than its query file limit. Oldest is when the
	// earliest row is dated, zero when that is not known or there is none.
	Rows   int64
	Oldest time.Time
	// Uncounted is why the rows were asked about and not counted, empty
	// when they were or were not asked about.
	Uncounted string
}

// Distinct asks for the distinct values of one tag.
type Distinct struct {
	Tag string
	// ExceptTag and Except leave out the rows whose ExceptTag holds Except,
	// compared without case, the way GitHub compares logins. Empty asks
	// about every row.
	ExceptTag, Except string
}

// Spread is the distinct values of the tags asked about.
type Spread struct {
	// Values is, per tag, the distinct values its rows hold, sorted. A row
	// that lacks the tag, or holds it empty, is the value "": a store keeps
	// no empty tag, and a row without one is a row whose owner it cannot
	// name, which is not the same as a row of nobody's.
	Values map[string][]string
	// Unread is, per tag, why its values could not all be read. A tag here
	// has no answer in Values that can be trusted to be whole.
	Unread map[string]string
}

// Inspector is a store that can be asked what shape a measurement is in.
// Every call reads; nothing here writes, deletes or renames.
type Inspector interface {
	// Name is the sink's name in the config.
	Name() string
	// Describe says which server answers, as a person names it.
	Describe(ctx context.Context) (string, error)
	// Shape asks about one measurement: whether the store holds it, and
	// which of oldTags its rows carry as tags. How many rows it holds and
	// since when is asked only when that matters: when an old tag is there,
	// or when none was asked about.
	Shape(ctx context.Context, measurement string, oldTags []string) (Shape, error)
	// Spread asks for the distinct values of each tag named, which is how a
	// store says whose rows it holds. A question the store would not
	// answer is in Spread.Unread; an error is a store that answered nothing.
	Spread(ctx context.Context, measurement string, questions []Distinct) (Spread, error)
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
func (i *influx) Shape(ctx context.Context, measurement string, oldTags []string) (Shape, error) {
	if _, err := i.Describe(ctx); err != nil {
		return Shape{}, err
	}
	if i.v2 {
		return i.shapeV2(ctx, measurement, oldTags)
	}
	return i.shapeV3(ctx, measurement, oldTags)
}

// Spread asks InfluxDB 3 in SQL, or InfluxDB 2 in Flux.
func (i *influx) Spread(ctx context.Context, measurement string, questions []Distinct) (Spread, error) {
	if _, err := i.Describe(ctx); err != nil {
		return Spread{}, err
	}
	if i.v2 {
		return i.spreadV2(ctx, measurement, questions)
	}
	return i.spreadV3(ctx, measurement, questions)
}

// catalog is the live table's columns, each saying whether it is a tag, and
// nothing when there is no such table. A tag is a column of type
// Dictionary(Int32, Utf8) and a string field Utf8, measured on 3.0.0 to
// 3.11.5; a table InfluxDB has soft deleted is listed under another name, so
// the exact name matches the live table alone. The catalog is read without
// opening a Parquet file, which is why every question starts here.
func (i *influx) catalog(ctx context.Context, measurement string) (map[string]bool, error) {
	var cols []struct {
		Name string `json:"column_name"`
		Type string `json:"data_type"`
	}
	if err := i.sql(ctx, "SELECT column_name, data_type FROM information_schema.columns "+
		"WHERE table_schema = 'iox' AND table_name = "+sqlString(measurement), &cols); err != nil {
		return nil, err
	}
	tags := make(map[string]bool, len(cols))
	for _, c := range cols {
		tags[c.Name] = strings.HasPrefix(c.Type, "Dictionary(")
	}
	return tags, nil
}

// shapeV3 reads the catalog of the live table, and counts its rows only when
// the count is wanted.
//
// The count is the one question here that reads the table's files, and
// InfluxDB 3 Core refuses a query that would open more of them than its
// --query-file-limit, 432 by default: measured on 3.11.2, a table of 600
// Parquet files answered the catalog and refused count(*) with a 500 naming
// the limit. A table a sweep writes a row of every ten minutes is past that in
// days. So a table that holds no old tag is not counted at all, and a refused
// count leaves the rows unknown rather than the store unreachable: what it
// costs is the refill's bound, which is then none.
func (i *influx) shapeV3(ctx context.Context, measurement string, oldTags []string) (Shape, error) {
	tags, err := i.catalog(ctx, measurement)
	if err != nil {
		return Shape{}, err
	}
	shape := Shape{}
	if len(tags) == 0 {
		return shape, nil
	}
	shape.Exists = true
	for _, t := range oldTags {
		if tags[t] {
			shape.Old = append(shape.Old, t)
		}
	}
	shape.Rows = -1
	if len(oldTags) > 0 && len(shape.Old) == 0 {
		return shape, nil
	}
	shape.Rows, shape.Oldest, shape.Uncounted = i.span(ctx, measurement)
	return shape, nil
}

// span is how many rows a table holds and since when, or, when the server
// would not count them, -1 and its refusal.
func (i *influx) span(ctx context.Context, measurement string) (rows int64, oldest time.Time, refused string) {
	var answer []struct {
		N      int64   `json:"n"`
		Oldest *string `json:"oldest"`
	}
	if err := i.sql(ctx, "SELECT count(*) AS n, min(time) AS oldest FROM "+quoteIdent(measurement), &answer); err != nil {
		return -1, time.Time{}, err.Error()
	}
	if len(answer) != 1 {
		return -1, time.Time{}, ""
	}
	if answer[0].Oldest != nil {
		// The JSON format writes a timestamp without a zone, in UTC.
		oldest, _ = time.Parse("2006-01-02T15:04:05.999999999", *answer[0].Oldest)
	}
	return answer[0].N, oldest, ""
}

// spreadV3 asks one SELECT DISTINCT per tag. A tag the table has no column
// for is a tag no row carries. A question the server refuses, the query file
// limit among the reasons, is that tag unread and the others still asked.
func (i *influx) spreadV3(ctx context.Context, measurement string, questions []Distinct) (Spread, error) {
	tags, err := i.catalog(ctx, measurement)
	if err != nil {
		return Spread{}, err
	}
	out := Spread{Values: map[string][]string{}, Unread: map[string]string{}}
	if len(tags) == 0 {
		return out, nil
	}
	for _, d := range questions {
		if _, has := tags[d.Tag]; !has {
			out.Values[d.Tag] = []string{""}
			continue
		}
		q := "SELECT DISTINCT " + quoteIdent(d.Tag) + " AS v FROM " + quoteIdent(measurement)
		if _, has := tags[d.ExceptTag]; has && d.Except != "" {
			// Measured on 3.11.2: lower() reads a tag column, and a row
			// without the tag is NULL, which <> would leave out.
			q += " WHERE " + quoteIdent(d.ExceptTag) + " IS NULL OR lower(" + quoteIdent(d.ExceptTag) +
				") <> lower(" + sqlString(d.Except) + ")"
		}
		var rows []struct {
			V *string `json:"v"`
		}
		if askErr := i.sql(ctx, q+" ORDER BY v", &rows); askErr != nil {
			out.Unread[d.Tag] = askErr.Error()
			continue
		}
		for _, r := range rows {
			v := ""
			if r.V != nil {
				v = *r.V
			}
			out.Values[d.Tag] = append(out.Values[d.Tag], v)
		}
		out.Values[d.Tag] = sortedItems(out.Values[d.Tag])
	}
	return out, nil
}

// sql runs one query through /api/v3/query_sql against the sink's database
// and reads its JSON rows.
func (i *influx) sql(ctx context.Context, q string, into any) error {
	return i.sqlIn(ctx, i.sink.Bucket, q, into)
}

// sqlIn is sql against another database, the server's own _internal.
func (i *influx) sqlIn(ctx context.Context, db, q string, into any) error {
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v3/query_sql?" + url.Values{
		"db": {db}, "q": {q}, "format": {"json"},
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
func (i *influx) shapeV2(ctx context.Context, measurement string, oldTags []string) (Shape, error) {
	shape := Shape{Rows: -1}
	from := i.fluxFrom(measurement)
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
	return shape, nil
}

// fluxFrom is the start of every Flux question about one measurement, left
// open for the caller to add to the filter.
func (i *influx) fluxFrom(measurement string) string {
	return `from(bucket: ` + fluxString(i.sink.Bucket) + `) |> range(start: 0) |> filter(fn: (r) => r._measurement == ` +
		fluxString(measurement)
}

// spreadV2 asks for the distinct values of each tag, and separately whether
// any row lacks it, since Flux's CSV writes a missing value as an empty cell
// the reader cannot tell from no row.
func (i *influx) spreadV2(ctx context.Context, measurement string, questions []Distinct) (Spread, error) {
	out := Spread{Values: map[string][]string{}, Unread: map[string]string{}}
	for _, d := range questions {
		from := i.fluxFrom(measurement)
		if d.ExceptTag != "" && d.Except != "" {
			except := fluxString(d.ExceptTag)
			from += ` and not (exists r[` + except + `] and strings.toLower(v: r[` + except + `]) == ` +
				fluxString(strings.ToLower(d.Except)) + `)`
			from = "import \"strings\"\n" + from
		}
		col := fluxString(d.Tag)
		values, err := i.flux(ctx, from+` and exists r[`+col+`]) |> keep(columns: [`+col+`]) |> group() |> distinct(column: `+col+`)`, "_value")
		if err != nil {
			out.Unread[d.Tag] = err.Error()
			continue
		}
		lacking, err := i.flux(ctx, from+` and not exists r[`+col+`]) |> keep(columns: ["_time"]) |> limit(n: 1)`, "_time")
		if err != nil {
			out.Unread[d.Tag] = err.Error()
			continue
		}
		if len(lacking) > 0 {
			values = append(values, "")
		}
		out.Values[d.Tag] = sortedItems(values)
	}
	return out, nil
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
func (p *postgres) Shape(ctx context.Context, measurement string, oldTags []string) (Shape, error) {
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return Shape{}, err
	}
	defer func() { _ = conn.Close(ctx) }()
	shape := Shape{}
	cols, err := p.columns(ctx, conn, measurement)
	if err != nil || len(cols) == 0 {
		return shape, err
	}
	shape.Exists = true
	table := quoteIdent(measurement)
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
	shape.Rows = -1
	if len(oldTags) > 0 && len(shape.Old) == 0 {
		return shape, nil
	}
	var oldest *time.Time
	if scanErr := conn.QueryRow(ctx, "SELECT count(*), min(time) FROM "+table).Scan(&shape.Rows, &oldest); scanErr != nil {
		return Shape{}, scanErr
	}
	if oldest != nil {
		shape.Oldest = oldest.UTC()
	}
	return shape, nil
}

// Spread reads each tag's distinct values. The sink writes a key column a
// point does not carry as the empty string, and never NULL; coalesce reads a
// NULL in a table somebody else made as the same thing.
func (p *postgres) Spread(ctx context.Context, measurement string, questions []Distinct) (Spread, error) {
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return Spread{}, err
	}
	defer func() { _ = conn.Close(ctx) }()
	out := Spread{Values: map[string][]string{}, Unread: map[string]string{}}
	cols, err := p.columns(ctx, conn, measurement)
	if err != nil || len(cols) == 0 {
		return out, err
	}
	table := quoteIdent(measurement)
	for _, d := range questions {
		column := "''"
		if cols[d.Tag] {
			column = "coalesce(" + quoteIdent(d.Tag) + ", '')"
		}
		q := "SELECT DISTINCT " + column + " FROM " + table
		var args []any
		if cols[d.ExceptTag] && d.Except != "" {
			q += " WHERE lower(coalesce(" + quoteIdent(d.ExceptTag) + ", '')) <> lower($1)"
			args = append(args, d.Except)
		}
		values, askErr := distinct(ctx, conn, q+" ORDER BY 1", args...)
		if askErr != nil {
			return Spread{}, askErr
		}
		out.Values[d.Tag] = values
	}
	return out, nil
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

// distinct is the values a SELECT DISTINCT of one column answers.
func distinct(ctx context.Context, conn *pgx.Conn, query string, args ...any) ([]string, error) {
	rows, err := conn.Query(ctx, query, args...)
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
func (e *elastic) Shape(ctx context.Context, measurement string, oldTags []string) (Shape, error) {
	index := e.indexURL(measurement)
	shape := Shape{}
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
	if len(oldTags) > 0 && len(shape.Old) == 0 {
		return shape, nil
	}
	payload, err := json.Marshal(map[string]any{"size": 0, "aggs": map[string]any{
		"oldest": map[string]any{"min": map[string]string{"field": "@timestamp"}},
	}})
	if err != nil {
		return Shape{}, err
	}
	res, err := e.read(ctx, index+"/_search", payload)
	if err != nil {
		return Shape{}, err
	}
	var searched struct {
		Aggs struct {
			Oldest struct {
				Value string `json:"value_as_string"`
			} `json:"oldest"`
		} `json:"aggregations"`
	}
	if err = json.Unmarshal(res.body, &searched); err != nil {
		return Shape{}, fmt.Errorf("reading the search: %w", err)
	}
	shape.Oldest, _ = time.Parse(time.RFC3339Nano, searched.Aggs.Oldest.Value)
	return shape, nil
}

// indexURL is the address of a measurement's index.
func (e *elastic) indexURL(measurement string) string {
	return strings.TrimSuffix(e.sink.URL, "/") + "/" + url.PathEscape(e.index(measurement))
}

// termsPage is how many values one terms aggregation is asked for. A tag with
// more than this is unread rather than cut short.
const termsPage = 10000

// Spread asks one search for every tag, each a terms aggregation inside a
// filter that leaves out the rows Except names, beside a count of the rows
// that hold the tag at all.
//
// The values are read from whatever field the mapping makes aggregatable:
// the keyword sub-field dynamic mapping gives every string, or the tag itself
// where an index template maps strings as keyword, which has no sub-field.
// Measured on 9.5.3: asked for user.keyword over an index whose template
// mapped user as a keyword, a terms aggregation answered no buckets and no
// error, which read as an index holding nobody's rows. So the buckets are
// held to the count: they have to add up to every row that holds the tag, or
// the tag is unread. A value longer than the sub-field's ignore_above, 256 by
// default, is a row in the count and in no bucket, and is caught the same way.
func (e *elastic) Spread(ctx context.Context, measurement string, questions []Distinct) (Spread, error) {
	index := e.indexURL(measurement)
	out := Spread{Values: map[string][]string{}, Unread: map[string]string{}}
	total, found, err := e.count(ctx, index, nil)
	if err != nil || !found || total == 0 {
		return out, err
	}
	tags := make([]string, 0, 2*len(questions))
	for _, d := range questions {
		tags = append(tags, d.Tag)
		if d.ExceptTag != "" {
			tags = append(tags, d.ExceptTag)
		}
	}
	fields, err := e.keywordFields(ctx, index, tags)
	if err != nil {
		return Spread{}, err
	}
	aggs := map[string]any{}
	for n, d := range questions {
		agg, why := spreadAgg(d, fields)
		if why != "" {
			out.Unread[d.Tag] = why
			continue
		}
		aggs["d"+strconv.Itoa(n)] = agg
	}
	payload, err := json.Marshal(map[string]any{"size": 0, "aggs": aggs})
	if err != nil {
		return Spread{}, err
	}
	res, err := e.read(ctx, index+"/_search", payload)
	if err != nil {
		return Spread{}, err
	}
	var searched struct {
		Aggs map[string]struct {
			Rows int64 `json:"doc_count"`
			Has  struct {
				Rows int64 `json:"doc_count"`
			} `json:"has"`
			V *termsAgg `json:"v"`
		} `json:"aggregations"`
	}
	if err = json.Unmarshal(res.body, &searched); err != nil {
		return Spread{}, fmt.Errorf("reading the search: %w", err)
	}
	for n, d := range questions {
		agg, asked := searched.Aggs["d"+strconv.Itoa(n)]
		if !asked {
			continue
		}
		values, why := bucketsOf(d.Tag, fields[d.Tag], agg.Has.Rows, agg.V)
		if why != "" {
			out.Unread[d.Tag] = why
			continue
		}
		if agg.Rows > agg.Has.Rows {
			values = append(values, "")
		}
		out.Values[d.Tag] = sortedItems(values)
	}
	return out, nil
}

// spreadAgg is the aggregation that answers one question: a terms
// aggregation on the tag's aggregatable field, beside a count of the rows
// that hold it, inside a filter that leaves out the rows Except names. Or why
// it cannot be asked.
func spreadAgg(d Distinct, fields map[string]string) (agg map[string]any, why string) {
	scope := map[string]any{"match_all": map[string]any{}}
	if d.ExceptTag != "" && d.Except != "" {
		field, ok := fields[d.ExceptTag]
		if _, mapped := fields[d.ExceptTag+"?"]; mapped {
			return nil, fmt.Sprintf("%s is mapped as nothing a term query can compare, so the rows of %s cannot "+
				"be left out", d.ExceptTag, d.Except)
		}
		if ok {
			scope = map[string]any{"bool": map[string]any{"must_not": map[string]any{
				"term": map[string]any{field: map[string]any{"value": d.Except, "case_insensitive": true}},
			}}}
		}
	}
	inner := map[string]any{"has": map[string]any{"filter": map[string]any{"exists": map[string]string{"field": d.Tag}}}}
	if field, ok := fields[d.Tag]; ok {
		inner["v"] = map[string]any{"terms": map[string]any{"field": field, "size": termsPage}}
	}
	return map[string]any{"filter": scope, "aggs": inner}, ""
}

// termsAgg is what a terms aggregation answers.
type termsAgg struct {
	Other   int64 `json:"sum_other_doc_count"`
	Buckets []struct {
		Key  string `json:"key"`
		Rows int64  `json:"doc_count"`
	} `json:"buckets"`
}

// bucketsOf is a terms aggregation's values, or why they are not every value
// the rows that hold the tag carry.
func bucketsOf(tag, field string, holding int64, v *termsAgg) (values []string, why string) {
	if holding == 0 {
		return nil, ""
	}
	if v == nil {
		return nil, fmt.Sprintf("%s is mapped as neither a keyword nor text with a keyword sub-field, so its "+
			"values cannot be aggregated", tag)
	}
	if v.Other > 0 {
		return nil, fmt.Sprintf("%s holds more than %d values", tag, termsPage)
	}
	var read int64
	values = make([]string, 0, len(v.Buckets))
	for _, b := range v.Buckets {
		read += b.Rows
		values = append(values, b.Key)
	}
	if read < holding {
		return nil, fmt.Sprintf("%d of the %d rows that hold %s have no value in %s, which the mapping or an "+
			"ignore_above keeps out of it", holding-read, holding, tag, field)
	}
	return values, ""
}

// keywordFields is, per tag, the field a terms aggregation or a term query
// reads it from: the tag itself where it is mapped as a keyword, its keyword
// sub-field where it is text with one. A tag mapped as something else is
// named with a ? after it, and one the index does not map at all is absent,
// which is a tag no document carries.
func (e *elastic) keywordFields(ctx context.Context, index string, tags []string) (map[string]string, error) {
	names := slices.Compact(slices.Sorted(slices.Values(tags)))
	res, err := e.read(ctx, index+"/_mapping/field/"+url.PathEscape(strings.Join(names, ",")), nil)
	if err != nil {
		return nil, err
	}
	type leaf struct {
		Type   string `json:"type"`
		Fields map[string]struct {
			Type string `json:"type"`
		} `json:"fields"`
	}
	var mapped map[string]struct {
		Mappings map[string]struct {
			Mapping map[string]leaf `json:"mapping"`
		} `json:"mappings"`
	}
	if err = json.Unmarshal(res.body, &mapped); err != nil {
		return nil, fmt.Errorf("reading the mapping: %w", err)
	}
	out := map[string]string{}
	for _, idx := range mapped {
		for name, m := range idx.Mappings {
			l := m.Mapping[name]
			switch {
			case l.Type == "keyword":
				out[name] = name
			case l.Fields["keyword"].Type == "keyword":
				out[name] = name + ".keyword"
			default:
				out[name+"?"] = l.Type
			}
		}
	}
	return out, nil
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
