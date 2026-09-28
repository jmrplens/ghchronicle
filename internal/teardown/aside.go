package teardown

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// A migration clears one measurement so that the rows written after it start
// the table, or the index, afresh in the shape this release writes. Where the
// store allows it the rows are not destroyed but set aside under a name of
// their own for a day, which is the undo window InfluxDB 3 gives its own
// deletes: PostgreSQL renames the table, Elasticsearch clones the index, and
// InfluxDB 3's delete is itself a rename the server purges 24 hours later.
// InfluxDB 2 has neither, and deletes.
//
// Every name acted on is exactly the measurement a caller names, which has to
// look like one this project writes, or a copy named the way Clear names one:
// the measurement, a dash and the UTC instant, YYYYMMDDTHHMMSS. Nothing is
// matched by a pattern a store expands, so a table or an index of anybody
// else's cannot be reached from here, however it is named.

// Aside is one copy of a measurement's rows kept out of the way of the rows
// written after it.
type Aside struct {
	// Name is the table or index the rows are kept in.
	Name        string
	Measurement string
	// At is when the rows were set aside, read from the name.
	At time.Time
	// ByServer says the store purges the copy itself and refuses to be told
	// to: InfluxDB 3 answers a delete of a table it has soft deleted with a
	// 409, measured on 3.11.2 with and without hard_delete_at=now.
	ByServer bool
}

// Clearer is a store a migration can clear a measurement in.
type Clearer interface {
	// Name is the sink's name in the config.
	Name() string
	// Clear takes every row of measurement out of the way of the next write,
	// which then starts it afresh. It returns where the rows are kept, an
	// Aside with no Name when the store keeps nothing, and held false when
	// there was no such measurement to clear.
	Clear(ctx context.Context, measurement string, at time.Time) (aside Aside, held bool, err error)
}

// Purger is a store whose copies this binary purges itself once they fall
// due, which is every store that keeps one but InfluxDB 3.
type Purger interface {
	// Name is the sink's name in the config.
	Name() string
	// Asides is every copy of the measurements named that the store holds,
	// named the way Clear names them.
	Asides(ctx context.Context, measurements []string) ([]Aside, error)
	// Purge removes one copy Clear made. A copy already gone is not a
	// failure.
	Purge(ctx context.Context, aside string) error
}

// Clearers is one Clearer per configured sink whose store a migration can
// clear from here.
func Clearers(cfg *config.Config) []Clearer {
	var out []Clearer
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

// Purgers is one Purger per configured sink whose copies this binary purges.
func Purgers(cfg *config.Config) []Purger {
	var out []Purger
	if s := cfg.Sinks.Postgres; s != nil {
		out = append(out, &postgres{sink: s})
	}
	if s := cfg.Sinks.Elasticsearch; s != nil {
		out = append(out, &elastic{sink: s})
	}
	return out
}

// Grace is how long a copy is kept before it is purged: the grace InfluxDB 3
// gives its own soft deletes by default, so every store keeps its copy for
// the same day.
const Grace = 24 * time.Hour

// stampLayout is the instant in an aside's name. InfluxDB 3 names its soft
// deleted tables with the same layout, in UTC, measured on 3.11.2; an
// Elasticsearch index name takes it in lower case.
const stampLayout = "20060102T150405"

// measurementName is what a measurement this project writes looks like. A
// name outside it is refused before any store is called, which also keeps a
// comma, a star or a slash, each of which a store's URL or SQL reads as more
// than one name, out of every request here.
var measurementName = regexp.MustCompile(`^gh_[a-z0-9_]+$`)

// clearable refuses a measurement name no collector of this project writes.
func clearable(measurement string) error {
	if !measurementName.MatchString(measurement) {
		return fmt.Errorf("%q is not a measurement this project writes", measurement)
	}
	return nil
}

// asideOf reads a name as a copy of one of the measurements named, set aside
// by Clear or by InfluxDB 3's own delete.
func asideOf(name string, measurements []string) (Aside, bool) {
	for _, m := range measurements {
		rest, ok := strings.CutPrefix(name, m)
		if !ok {
			continue
		}
		stamp := asideStamp.FindStringSubmatch(rest)
		if stamp == nil {
			continue
		}
		at, err := time.Parse(stampLayout, strings.ToUpper(stamp[1]))
		if err != nil {
			continue
		}
		return Aside{Name: name, Measurement: m, At: at}, true
	}
	return Aside{}, false
}

// ── InfluxDB ────────────────────────────────────────────────────────────────

// Clear deletes the table. InfluxDB 3 renames it to <measurement>-<instant>,
// keeps it queryable and purges it 24 hours later (3.4.0 and later; 3.0.0 has
// no deleter), and takes a write under the old name at once, even one that
// makes a tag column a field, measured on 3.0.0 to 3.11.5. The name it chose
// is read back from the catalog, as the one table of that measurement's that
// was not there before the delete. hard_delete_at is not sent: measured on
// 3.11.5, now had not removed the rows eleven minutes later, and it takes the
// day to undo away.
//
// InfluxDB 2 keeps no table aside and has no rename: it deletes every row of
// the measurement over all time, which is final.
func (i *influx) Clear(ctx context.Context, measurement string, _ time.Time) (Aside, bool, error) {
	if err := clearable(measurement); err != nil {
		return Aside{}, false, err
	}
	if _, err := i.Describe(ctx); err != nil {
		return Aside{}, false, err
	}
	if i.v2 {
		return Aside{Measurement: measurement}, true, i.deleteV2(ctx, measurement)
	}
	before, err := i.tables(ctx)
	if err != nil {
		return Aside{}, false, err
	}
	if !slices.Contains(before, measurement) {
		return Aside{}, false, nil
	}
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v3/configure/table?" + url.Values{
		"db": {i.sink.Bucket}, "table": {measurement},
	}.Encode()
	if _, _, err = i.call(ctx, http.MethodDelete, endpoint, nil); err != nil {
		return Aside{}, true, fmt.Errorf("%w; the same delete by hand: curl -X DELETE '%s' -H 'Authorization: Bearer <token>'",
			err, endpoint)
	}
	// The table is set aside from here on whatever the read below says, so a
	// read that fails leaves the copy unnamed rather than the change undone:
	// reported as a failure, it would be recorded as never applied, and the
	// history would never be read again into the table the delete emptied.
	after, _ := i.tables(ctx)
	var made Aside
	for _, name := range after {
		a, ok := asideOf(name, []string{measurement})
		if ok && !slices.Contains(before, name) && a.At.After(made.At) {
			made = a
		}
	}
	made.Measurement, made.ByServer = measurement, true
	return made, true, nil
}

// deleteV2 deletes every row of one measurement in the sink's bucket, from the
// first instant InfluxDB 2 can hold to the last. Measured on 2.7.12: the
// predicate took that measurement's rows and left gh_discussion_comment_x,
// the other measurements of the bucket and the same measurement in another
// bucket as they were.
func (i *influx) deleteV2(ctx context.Context, measurement string) error {
	payload := fmt.Sprintf(`{"start":"1677-09-21T00:12:43.145224194Z","stop":"2262-04-11T23:47:16.854775806Z",`+
		`"predicate":"_measurement=\"%s\""}`, measurement)
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v2/delete?" + url.Values{
		"org": {i.sink.Org}, "bucket": {i.sink.Bucket},
	}.Encode()
	_, err := exchange(ctx, request{
		method: http.MethodPost, endpoint: endpoint, payload: []byte(payload),
		prepare: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			i.token(r)
		},
	}, nil)
	return err
}

// ── PostgreSQL ──────────────────────────────────────────────────────────────

// lockTimeout bounds how long a rename or a drop waits for the table's lock.
// Both take ACCESS EXCLUSIVE, which waits behind every Grafana query reading
// the table and holds every query after it: measured on 18.6, a DROP waited
// 4.08 s behind a reader holding the table for 4 s, and failed cleanly after
// 2.06 s under a 2 s lock_timeout.
const lockTimeout = "5s"

// lockTries is how many times a rename is sent while a reader holds the
// table, each waiting up to lockTimeout.
const lockTries = 3

// identifierMax is PostgreSQL's longest identifier. A longer one is cut to it
// with only a notice, so the copy would not be under the name recorded.
const identifierMax = 63

// Clear renames the table <measurement>-<instant> in the schema the sink
// writes to, which keeps every row, index and key it had. The sink's next
// CREATE TABLE IF NOT EXISTS makes a new one under the old name: measured on
// 18.6, the new primary key is named gh_discussion_comment_pkey1 beside the
// copy's, with no collision.
func (p *postgres) Clear(ctx context.Context, measurement string, at time.Time) (Aside, bool, error) {
	if err := clearable(measurement); err != nil {
		return Aside{}, false, err
	}
	aside := Aside{Name: measurement + "-" + at.UTC().Format(stampLayout), Measurement: measurement, At: at.UTC().Truncate(time.Second)}
	if len(aside.Name) > identifierMax {
		return Aside{}, false, fmt.Errorf("%s is longer than PostgreSQL keeps a name", aside.Name)
	}
	conn, schema, err := p.open(ctx)
	if err != nil {
		return Aside{}, false, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var held bool
	if err = conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", quoteIdent(schema)+"."+quoteIdent(measurement)).
		Scan(&held); err != nil || !held {
		return Aside{}, false, err
	}
	rename := "ALTER TABLE " + quoteIdent(schema) + "." + quoteIdent(measurement) + " RENAME TO " + quoteIdent(aside.Name)
	for try := 1; ; try++ {
		_, err = conn.Exec(ctx, rename)
		if err == nil || !lockTimedOut(err) || try == lockTries {
			break
		}
	}
	if err != nil {
		return Aside{}, true, err
	}
	return aside, true, nil
}

// Asides is the copies Clear made of the measurements named, in the schema
// the sink writes to.
func (p *postgres) Asides(ctx context.Context, measurements []string) ([]Aside, error) {
	names, err := p.Holds(ctx)
	if err != nil {
		return nil, err
	}
	var out []Aside
	for _, name := range names {
		if a, ok := asideOf(name, measurements); ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// Purge drops one copy Clear made, and nothing that is not named like one.
func (p *postgres) Purge(ctx context.Context, aside string) error {
	if !ours(aside) || !softDeleted(aside) {
		return fmt.Errorf("%s is not a copy a migration set aside", aside)
	}
	conn, schema, err := p.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, "DROP TABLE IF EXISTS "+quoteIdent(schema)+"."+quoteIdent(aside))
	return err
}

// open connects with the sink's DSN, bounds every lock wait and reads the
// schema the sink's own statements land in, which every statement here then
// names: an unqualified name would reach a table of the same name further
// down the search_path when the sink's schema has none.
func (p *postgres) open(ctx context.Context) (*pgx.Conn, string, error) {
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return nil, "", err
	}
	var schema *string
	if _, err = conn.Exec(ctx, "SET lock_timeout = '"+lockTimeout+"'"); err == nil {
		err = conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema)
	}
	if err == nil && schema == nil {
		err = errors.New("the DSN's search_path names no schema that exists")
	}
	if err != nil {
		_ = conn.Close(ctx)
		return nil, "", err
	}
	return conn, *schema, nil
}

// lockTimedOut says whether a statement gave up waiting for a lock.
func lockTimedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// ── Elasticsearch ───────────────────────────────────────────────────────────

// Clear sets the index aside: it takes no more writes, is cloned to
// <index>-<instant>, and is deleted once the clone holds every document, so
// the sink's next bulk write creates it afresh with a mapping of its own.
// Measured on 9.5.3: the clone kept the write block, answered with the same
// count, was not read by the dashboards' queries, which name _index exactly,
// and could be deleted with the block on.
//
// Anything that fails before the delete takes the block off again, so that a
// store that could not be cleared goes on taking the sink's writes.
func (e *elastic) Clear(ctx context.Context, measurement string, at time.Time) (Aside, bool, error) {
	if err := clearable(measurement); err != nil {
		return Aside{}, false, err
	}
	index := e.index(measurement)
	aside := Aside{
		Name: index + "-" + strings.ToLower(at.UTC().Format(stampLayout)), Measurement: measurement,
		At: at.UTC().Truncate(time.Second),
	}
	base := strings.TrimSuffix(e.sink.URL, "/") + "/"
	_, found, err := e.count(ctx, base+url.PathEscape(index), nil)
	if err != nil || !found {
		return Aside{}, false, err
	}
	if _, _, err = e.call(ctx, http.MethodPut, base+url.PathEscape(index)+"/_block/write", nil); err != nil {
		return Aside{}, true, err
	}
	if err = e.cloneAside(ctx, index, aside.Name); err != nil {
		return Aside{}, true, errors.Join(err, e.unblock(ctx, index))
	}
	if _, _, err = e.call(ctx, http.MethodDelete, base+url.PathEscape(index), nil); err != nil {
		return Aside{}, true, errors.Join(err, e.unblock(ctx, index), e.Purge(ctx, aside.Name))
	}
	return aside, true, nil
}

// cloneAside clones a write blocked index and checks the clone holds as many
// documents as the index, which is what makes deleting the index safe.
func (e *elastic) cloneAside(ctx context.Context, index, aside string) error {
	base := strings.TrimSuffix(e.sink.URL, "/") + "/"
	if _, _, err := e.call(ctx, http.MethodPost, base+url.PathEscape(index)+"/_refresh", nil); err != nil {
		return err
	}
	want, _, err := e.count(ctx, base+url.PathEscape(index), nil)
	if err != nil {
		return err
	}
	if _, _, err = e.call(ctx, http.MethodPost, base+url.PathEscape(index)+"/_clone/"+url.PathEscape(aside), nil); err != nil {
		return err
	}
	got, found, err := e.count(ctx, base+url.PathEscape(aside), nil)
	if err == nil && (!found || got != want) {
		err = fmt.Errorf("the clone %s holds %d documents of the %d in %s", aside, got, want, index)
	}
	if err != nil {
		return errors.Join(err, e.Purge(ctx, aside))
	}
	return nil
}

// unblock lets an index take writes again.
func (e *elastic) unblock(ctx context.Context, index string) error {
	endpoint := strings.TrimSuffix(e.sink.URL, "/") + "/" + url.PathEscape(index) + "/_settings"
	_, err := exchange(ctx, request{
		method: http.MethodPut, endpoint: endpoint, payload: []byte(`{"index":{"blocks":{"write":false}}}`),
		prepare: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			e.authorize(r)
		},
	}, nil)
	return err
}

// index is the sink's index for a measurement: lower case, because an index
// name may not carry an upper-case letter.
func (e *elastic) index(measurement string) string {
	return strings.ToLower(e.sink.Prefix + "-" + measurement)
}

// Asides is the clones Clear made of the measurements named, under the
// sink's prefix.
func (e *elastic) Asides(ctx context.Context, measurements []string) ([]Aside, error) {
	names, err := e.Holds(ctx)
	if err != nil {
		return nil, err
	}
	head := strings.ToLower(e.sink.Prefix + "-")
	var out []Aside
	for _, name := range names {
		rest, ok := strings.CutPrefix(name, head)
		if !ok {
			continue
		}
		if a, isAside := asideOf(rest, measurements); isAside {
			a.Name = name
			out = append(out, a)
		}
	}
	return out, nil
}

// Purge deletes one clone Clear made, and nothing that is not named like one.
func (e *elastic) Purge(ctx context.Context, aside string) error {
	rest, ok := strings.CutPrefix(aside, strings.ToLower(e.sink.Prefix+"-"))
	if !ok || !ours(rest) || !softDeleted(rest) || strings.ContainsAny(aside, "*,/") {
		return fmt.Errorf("%s is not a copy a migration set aside", aside)
	}
	_, _, err := e.call(ctx, http.MethodDelete, strings.TrimSuffix(e.sink.URL, "/")+"/"+url.PathEscape(aside),
		map[int]bool{http.StatusNotFound: true})
	return err
}
