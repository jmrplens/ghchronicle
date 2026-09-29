package teardown

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// After a migration's refill, the copy of the old rows and the table read
// back are compared item by item, which is how the items GitHub no longer
// serves are named while the copy still holds them. Both are read here, and
// only read.

// ItemReader is a store that can say which items a table holds.
type ItemReader interface {
	// Name is the sink's name in the config.
	Name() string
	// Items is every distinct item of a table, each the values of the tags
	// named, in that order, joined by a space, sorted. The table is a
	// measurement, or a copy of one by the name Clear gave it. A tag the
	// table does not have is an error that names it, since an item missing
	// one would read as another item.
	Items(ctx context.Context, table string, tags []string) ([]string, error)
}

// ItemReaders is one ItemReader per configured sink whose store keeps a copy
// a refill can be compared with.
func ItemReaders(cfg *config.Config) []ItemReader {
	var out []ItemReader
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

// tableName is what a table asked about has to look like: a measurement this
// project writes, or one of its copies. The name goes into a query, so
// anything else is refused before a store is called.
var tableName = regexp.MustCompile(`^gh_[a-z0-9_]+(-\d{8}[Tt]\d{6})?$`)

// tagName is what a tag asked about has to look like, for the same reason.
var tagName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// readable refuses a table or a tag nobody here would ask about.
func readable(table string, tags []string) error {
	if !tableName.MatchString(table) {
		return fmt.Errorf("%q is not a table this project writes", table)
	}
	if len(tags) == 0 {
		return errors.New("no tag names an item")
	}
	for _, t := range tags {
		if !tagName.MatchString(t) {
			return fmt.Errorf("%q is not a tag this project writes", t)
		}
	}
	return nil
}

// missingTag says which of the tags a table lacks, as an error.
func missingTag(table string, tags []string, has func(string) bool) error {
	for _, t := range tags {
		if !has(t) {
			return fmt.Errorf("%s has no %s", table, t)
		}
	}
	return nil
}

// ── InfluxDB ────────────────────────────────────────────────────────────────

// Items reads InfluxDB 3's catalog for the tags and then the distinct values.
// Measured on 3.11.2 Core: a table the server soft deleted keeps its columns
// in information_schema.columns under its new name and answers SELECT
// DISTINCT like any other, and a column it lacks fails the whole query with a
// 500, which is why the catalog is asked first. InfluxDB 2 keeps no copy.
func (i *influx) Items(ctx context.Context, table string, tags []string) ([]string, error) {
	if err := readable(table, tags); err != nil {
		return nil, err
	}
	if _, err := i.Describe(ctx); err != nil {
		return nil, err
	}
	if i.v2 {
		return nil, errors.New("InfluxDB 2 keeps no copy of what a migration cleared")
	}
	var cols []struct {
		Name string `json:"column_name"`
	}
	if err := i.sql(ctx, "SELECT column_name FROM information_schema.columns "+
		"WHERE table_schema = 'iox' AND table_name = "+sqlString(table), &cols); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("%s is not there", table)
	}
	has := map[string]bool{}
	for _, c := range cols {
		has[c.Name] = true
	}
	if err := missingTag(table, tags, func(t string) bool { return has[t] }); err != nil {
		return nil, err
	}
	selected := make([]string, len(tags))
	for n, t := range tags {
		selected[n] = quoteIdent(t) + " AS v" + strconv.Itoa(n)
	}
	var rows []map[string]*string
	if err := i.sql(ctx, "SELECT DISTINCT "+strings.Join(selected, ", ")+" FROM "+quoteIdent(table), &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		values := make([]string, len(tags))
		for n := range tags {
			if v := row["v"+strconv.Itoa(n)]; v != nil {
				values[n] = *v
			}
		}
		out = append(out, strings.Join(values, " "))
	}
	return sortedItems(out), nil
}

// ── PostgreSQL ──────────────────────────────────────────────────────────────

// Items reads the table in the schema the sink writes to, where Clear
// renames the copy.
func (p *postgres) Items(ctx context.Context, table string, tags []string) ([]string, error) {
	if err := readable(table, tags); err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()
	cols, err := p.columns(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("%s is not there", table)
	}
	if missing := missingTag(table, tags, func(t string) bool { return cols[t] }); missing != nil {
		return nil, missing
	}
	selected := make([]string, len(tags))
	for n, t := range tags {
		// A key column the point did not carry holds the empty string,
		// never NULL; coalesce keeps a NULL in a table somebody else made
		// from failing the scan.
		selected[n] = "coalesce(" + quoteIdent(t) + ", '')"
	}
	rows, err := conn.Query(ctx, "SELECT DISTINCT "+strings.Join(selected, ", ")+" FROM "+quoteIdent(table))
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		values := make([]string, len(tags))
		dest := make([]any, len(tags))
		for n := range values {
			dest[n] = &values[n]
		}
		if err = rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, strings.Join(values, " "))
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	return sortedItems(out), nil
}

// ── Elasticsearch ───────────────────────────────────────────────────────────

// itemPage is how many items one composite aggregation page asks for.
const itemPage = 1000

// Items pages a composite aggregation over the keyword sub-field dynamic
// mapping gives every string, which is the one way to have every distinct
// combination rather than the top few. The index is refreshed first: the
// sink does not wait for a refresh, and a document written a moment ago is
// not searchable until one, so a comparison straight after a refill would
// name items that are there.
func (e *elastic) Items(ctx context.Context, table string, tags []string) ([]string, error) {
	// A copy is named by its whole index, prefix and all, as Clear named it;
	// a measurement by itself, as everywhere else.
	index, name := e.index(table), table
	if rest, isCopy := strings.CutPrefix(table, strings.ToLower(e.sink.Prefix+"-")); isCopy {
		index, name = table, rest
	}
	if err := readable(name, tags); err != nil {
		return nil, err
	}
	endpoint := strings.TrimSuffix(e.sink.URL, "/") + "/" + url.PathEscape(index)
	if _, status, err := e.call(ctx, http.MethodPost, endpoint+"/_refresh",
		map[int]bool{http.StatusNotFound: true}); err != nil || status == http.StatusNotFound {
		if err == nil {
			err = fmt.Errorf("%s is not there", index)
		}
		return nil, err
	}
	total, _, err := e.count(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		n, _, countErr := e.count(ctx, endpoint, map[string]any{"exists": map[string]string{"field": t}})
		if countErr != nil {
			return nil, countErr
		}
		if n == 0 && total > 0 {
			return nil, fmt.Errorf("%s has no %s", index, t)
		}
	}
	return e.composite(ctx, endpoint, tags)
}

// composite walks the aggregation's pages until one comes back without an
// after_key, which is the last.
func (e *elastic) composite(ctx context.Context, endpoint string, tags []string) ([]string, error) {
	sources := make([]map[string]any, len(tags))
	for n, t := range tags {
		sources[n] = map[string]any{"v" + strconv.Itoa(n): map[string]any{"terms": map[string]string{"field": t + ".keyword"}}}
	}
	var out []string
	var after map[string]any
	for {
		composite := map[string]any{"size": itemPage, "sources": sources}
		if after != nil {
			composite["after"] = after
		}
		payload, err := json.Marshal(map[string]any{"size": 0, "aggs": map[string]any{
			"items": map[string]any{"composite": composite},
		}})
		if err != nil {
			return nil, err
		}
		res, err := e.read(ctx, endpoint+"/_search", payload)
		if err != nil {
			return nil, err
		}
		var page struct {
			Aggs struct {
				Items struct {
					After   map[string]any `json:"after_key"`
					Buckets []struct {
						Key map[string]any `json:"key"`
					} `json:"buckets"`
				} `json:"items"`
			} `json:"aggregations"`
		}
		if err = json.Unmarshal(res.body, &page); err != nil {
			return nil, fmt.Errorf("reading the aggregation: %w", err)
		}
		for _, b := range page.Aggs.Items.Buckets {
			values := make([]string, len(tags))
			for n := range tags {
				values[n] = fmt.Sprint(b.Key["v"+strconv.Itoa(n)])
			}
			out = append(out, strings.Join(values, " "))
		}
		if page.Aggs.Items.After == nil || len(page.Aggs.Items.Buckets) == 0 {
			return sortedItems(out), nil
		}
		after = page.Aggs.Items.After
	}
}

// sortedItems is a list of items sorted, each once.
func sortedItems(items []string) []string {
	slices.Sort(items)
	return slices.Compact(items)
}
