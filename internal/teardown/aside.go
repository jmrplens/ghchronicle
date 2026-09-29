package teardown

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// A migration clears one measurement so that the rows written after it start
// the table, or the index, afresh in the shape this release writes. Where the
// store allows it the rows are not destroyed but set aside under a name of
// their own for at least a day: PostgreSQL renames the table and
// Elasticsearch clones the index, each purged by ghchronicle a day later, and
// InfluxDB 3's delete is itself a rename the server purges on its own
// schedule, 72 hours later by default. InfluxDB 2 has neither, and deletes.
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
	// ByServer says the copy is the store's to purge, which ghchronicle
	// never asks for: InfluxDB 3 answers a delete of a table it has soft
	// deleted with another rename before 3.2, and with a 409 from 3.2.1 on
	// unless hard_delete_at is sent, which 3.10.0 and later refuse as well
	// (measured on 3.0.0 to 3.11.5).
	ByServer bool
	// Until is when the store itself purges the copy, as it said, zero when
	// it did not say or purges nothing itself.
	Until time.Time
	// ForGood says nothing will ever purge the copy on its own: InfluxDB 3
	// before 3.2 has no hard deletion, and a copy one of those releases
	// made keeps none through an upgrade. Stays is why, and what removes
	// it, as a sentence for the operator, empty when ForGood is not set.
	ForGood bool
	Stays   string
}

// ErrUntouched is a failure that left the store as it was: nothing was set
// aside and nothing deleted. Any other failure of Clear may have changed the
// store, the connection having broken after the store did what it was asked,
// and a caller has to go on as though it had.
var ErrUntouched = errors.New("the store was left as it was")

// untouched marks err as a failure that changed nothing, keeping its words.
func untouched(err error) error {
	if err == nil {
		return nil
	}
	return untouchedError{err}
}

type untouchedError struct{ error }

func (u untouchedError) Unwrap() error      { return u.error }
func (untouchedError) Is(target error) bool { return target == ErrUntouched }

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

// Grace is how long ghchronicle keeps a copy it purges itself, PostgreSQL's
// and Elasticsearch's, before it purges it: a day to undo a migration in.
const Grace = 24 * time.Hour

// ServerKeeps is how long InfluxDB 3 keeps a table it soft deleted when it
// does not say: measured on 3.2.1 to 3.11.5, a delete sent without
// hard_delete_at is scheduled for hard deletion 72 hours later, and the
// catalog keeps the name for the delete grace period after that, 24 hours by
// default. 3.2.0 does not say, and keeps the same 72 hours in its source.
// Before 3.2 nothing is ever scheduled: see KeepsForGood.
const ServerKeeps = 72 * time.Hour

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
// keeps it queryable until it purges it, and takes a write under the old name
// at once, even one that makes a tag column a field, measured on 3.0.0 to
// 3.11.5. The name it chose is read back from the catalog, as the one table of
// that measurement's that was not there before the delete, and when it purges
// it from its own system table, or, on a release that never does, why the
// copy stays and what removes it. hard_delete_at is not sent: measured on
// 3.11.5, now had not removed the rows eleven minutes later, and it takes the
// days to undo away.
//
// A delete whose answer did not arrive is read back the same way: the copy
// listed, or the table gone, is a delete that happened, and reported as a
// failure it would be recorded as never applied, and the history would never
// be read again into the table the delete emptied.
//
// InfluxDB 2 keeps no table aside and has no rename: it deletes every row of
// the measurement over all time, which is final.
func (i *influx) Clear(ctx context.Context, measurement string, _ time.Time) (Aside, bool, error) {
	if err := clearable(measurement); err != nil {
		return Aside{}, false, untouched(err)
	}
	if _, err := i.Describe(ctx); err != nil {
		return Aside{}, false, untouched(err)
	}
	if i.v2 {
		err := i.deleteV2(ctx, measurement)
		if refused(err) {
			err = untouched(err)
		}
		return Aside{Measurement: measurement}, true, err
	}
	before, err := i.tables(ctx)
	if err != nil {
		return Aside{}, false, untouched(err)
	}
	if !slices.Contains(before, measurement) {
		return Aside{}, false, nil
	}
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v3/configure/table?" + url.Values{
		"db": {i.sink.Bucket}, "table": {measurement},
	}.Encode()
	_, _, deleteErr := i.call(ctx, http.MethodDelete, endpoint, nil)
	after, listErr := i.tables(ctx)
	var made Aside
	for _, name := range after {
		a, ok := asideOf(name, []string{measurement})
		if ok && !slices.Contains(before, name) && a.At.After(made.At) {
			made = a
		}
	}
	if deleteErr != nil {
		byHand := fmt.Errorf("%w; the same delete by hand: curl -X DELETE '%s' -H 'Authorization: Bearer <token>'",
			deleteErr, endpoint)
		switch {
		case refused(deleteErr):
			return Aside{}, true, untouched(byHand)
		case listErr != nil:
			return Aside{}, true, fmt.Errorf("%w; whether the table was deleted could not be read back: %w", byHand, listErr)
		case made.Name == "" && slices.Contains(after, measurement):
			return Aside{}, true, untouched(byHand)
		}
	}
	made.Measurement, made.ByServer = measurement, true
	switch {
	case made.Name == "":
	case KeepsForGood(i.described):
		made.ForGood, made.Stays = true, InfluxStay(i.described, i.sink, made.Name).String()
	default:
		made.Until = i.purgedFrom(ctx, made.Name)
	}
	return made, true, nil
}

// influx3Minor is the minor release of an InfluxDB 3 as Describe names it,
// and whether the name is one of an InfluxDB 3's.
func influx3Minor(server string) (int, bool) {
	fields := strings.Fields(server)
	if len(fields) == 0 {
		return 0, false
	}
	major, rest, ok := strings.Cut(fields[len(fields)-1], ".")
	if !ok || major != "3" {
		return 0, false
	}
	minor, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(minor)
	return n, err == nil && n >= 0
}

// KeepsForGood says whether an InfluxDB 3, as Describe names it, keeps every
// table it deletes for good. Measured on Core 3.0.0, 3.0.3 and 3.1.0: the
// delete renames the table as every release does, and nothing ever purges
// it. There is no hard deletion: no deleter task starts, hard_delete_at is
// taken and ignored, _internal has no system.tables, and a delete of the
// renamed table answers 200 and renames it again. 3.2.0 is the first
// release with a deleter.
func KeepsForGood(server string) bool {
	minor, ok := influx3Minor(server)
	return ok && minor < 2
}

// Stay is why an InfluxDB 3 keeps a copy for good, and what removes it.
type Stay struct {
	// Why is the reason, as a clause.
	Why string
	// How says where a removal works, as a sentence without its full stop,
	// and Command is the request, empty where no request removes it.
	How, Command string
}

// String is the whole of it, for a log line or the plan.
func (s Stay) String() string {
	switch {
	case s.How == "":
		return s.Why + "."
	case s.Command == "":
		return s.Why + ". " + s.How + "."
	}
	return s.Why + ". " + s.How + ": " + s.Command
}

// InfluxStay says why a copy nothing will purge on its own stays, and what
// removes it, on the server Describe named; table is the copy's name, or a
// placeholder where it is not known yet. A server whose release cannot be
// read is told what the releases measured do.
//
// Measured on Core, a copy made by 3.0.3 or 3.1.0 holds no hard deletion
// time on 3.2.0, 3.4.0, 3.9.13 or 3.11.5 after an upgrade. A delete of it
// with hard_delete_at=now schedules it on 3.2.0 (which renames it once more
// first), 3.4.0 and 3.9.13, and each dropped it from its catalog when its
// delete grace period had passed; 3.2.1 to 3.9.13 take hard_delete_at=now
// for a copy of their own too, and 3.10.0 to 3.11.5 answer 409 with or
// without it.
func InfluxStay(server string, s *config.InfluxSink, table string) Stay {
	command := "curl -X DELETE '" + strings.TrimSuffix(s.URL, "/") + "/api/v3/configure/table?db=" +
		url.QueryEscape(s.Bucket) + "&table=" + table + "&hard_delete_at=now' -H 'Authorization: Bearer <token>'"
	const unscheduled = "a release before 3.2 set it aside, so nothing is scheduled to purge it"
	minor, known := influx3Minor(server)
	switch {
	case known && minor < 2:
		return Stay{
			Why: server + " has no hard deletion, so it never purges a table it deleted, and a delete of one " +
				"only renames it again",
			How: "A release from 3.2 to 3.9 removes it when told to, once the server runs one", Command: command,
		}
	case known && minor < 10:
		return Stay{Why: unscheduled, How: server + " removes it when told to", Command: command}
	case known:
		return Stay{Why: unscheduled, How: server + " refuses to be told to, with or without hard_delete_at, as " +
			"every release from 3.10.0 to 3.11.5 was measured to, so no request removes it on this release"}
	}
	return Stay{Why: unscheduled, How: "A release from 3.2 to 3.9 removes it when told to", Command: command}
}

// Unpurged is every copy of the measurements named that this InfluxDB 3 will
// never purge on its own, with why and what removes it: every copy on a
// release before 3.2, and on a later one each copy its system table lists as
// deleted with no hard deletion time, which is one an earlier release set
// aside. 3.2.0's system table has no such column, so there the question
// fails, and InfluxDB 2 keeps no copy at all.
func (i *influx) Unpurged(ctx context.Context, measurements []string) ([]Aside, error) {
	server, err := i.Describe(ctx)
	if err != nil || i.v2 {
		return nil, err
	}
	names, err := i.tables(ctx)
	if err != nil {
		return nil, err
	}
	var copies []Aside
	for _, name := range names {
		if a, ok := asideOf(name, measurements); ok {
			copies = append(copies, a)
		}
	}
	if len(copies) == 0 {
		return nil, nil
	}
	if !KeepsForGood(server) {
		unscheduled, askErr := i.unscheduled(ctx)
		if askErr != nil {
			return nil, askErr
		}
		copies = slices.DeleteFunc(copies, func(a Aside) bool { return !unscheduled[a.Name] })
	}
	for k := range copies {
		copies[k].ByServer, copies[k].ForGood = true, true
		copies[k].Stays = InfluxStay(server, i.sink, copies[k].Name).String()
	}
	return copies, nil
}

// unscheduled is every table of the database the server deleted and has not
// scheduled a hard deletion of, from the system table of its _internal
// database: measured on 3.11.5, the query answers each deleted table with its
// hard_deletion_time, which a table an earlier release deleted lacks.
func (i *influx) unscheduled(ctx context.Context) (map[string]bool, error) {
	var rows []struct {
		Name  string  `json:"table_name"`
		Until *string `json:"hard_deletion_time"`
	}
	if err := i.sqlIn(ctx, "_internal", "SELECT table_name, hard_deletion_time FROM system.tables "+
		"WHERE database_name = "+sqlString(i.sink.Bucket)+" AND deleted", &rows); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.Until == nil {
			out[r.Name] = true
		}
	}
	return out, nil
}

// purgedFrom is when InfluxDB 3 has scheduled the hard deletion of a table it
// soft deleted, from the system table of its _internal database (measured on
// 3.11.2: hard_deletion_time, 72 hours after the delete), or zero when the
// server does not say.
func (i *influx) purgedFrom(ctx context.Context, table string) time.Time {
	var rows []struct {
		Until string `json:"hard_deletion_time"`
	}
	if err := i.sqlIn(ctx, "_internal", "SELECT hard_deletion_time FROM system.tables WHERE database_name = "+
		sqlString(i.sink.Bucket)+" AND table_name = "+sqlString(table), &rows); err != nil || len(rows) != 1 {
		return time.Time{}
	}
	until, err := time.Parse(time.RFC3339Nano, rows[0].Until)
	if err != nil {
		// The JSON format writes a timestamp without a zone, in UTC, on the
		// columns of a table; the system table was measured with one.
		until, _ = time.Parse("2006-01-02T15:04:05.999999999", rows[0].Until)
	}
	return until.UTC()
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
//
// A rename whose answer did not arrive may have been committed, so the
// catalog is asked again, on a connection of its own, which of the two names
// is there.
func (p *postgres) Clear(ctx context.Context, measurement string, at time.Time) (Aside, bool, error) {
	if err := clearable(measurement); err != nil {
		return Aside{}, false, untouched(err)
	}
	aside := Aside{Name: measurement + "-" + at.UTC().Format(stampLayout), Measurement: measurement, At: at.UTC().Truncate(time.Second)}
	if len(aside.Name) > identifierMax {
		return Aside{}, false, untouched(fmt.Errorf("%s is longer than PostgreSQL keeps a name", aside.Name))
	}
	conn, schema, err := p.open(ctx)
	if err != nil {
		return Aside{}, false, untouched(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var held bool
	if err = conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", quoteIdent(schema)+"."+quoteIdent(measurement)).
		Scan(&held); err != nil || !held {
		return Aside{}, false, untouched(err)
	}
	rename := "ALTER TABLE " + quoteIdent(schema) + "." + quoteIdent(measurement) + " RENAME TO " + quoteIdent(aside.Name)
	for try := 1; ; try++ {
		_, err = conn.Exec(ctx, rename)
		if err == nil || !lockTimedOut(err) || try == lockTries {
			break
		}
	}
	switch {
	case err == nil:
		return aside, true, nil
	case lockTimedOut(err):
		return Aside{}, true, untouched(err)
	}
	live, kept, lookErr := p.lookAgain(ctx, schema, measurement, aside.Name)
	switch {
	case lookErr != nil:
		return Aside{}, true, fmt.Errorf("%w; whether the table was renamed could not be read back: %w", err, lookErr)
	case kept:
		return aside, true, nil
	case live:
		return Aside{}, true, untouched(err)
	}
	return Aside{}, true, fmt.Errorf("%w; neither %s nor %s is there any more", err, measurement, aside.Name)
}

// lookAgain asks which of a table and its copy the schema holds.
func (p *postgres) lookAgain(ctx context.Context, schema, table, aside string) (live, kept bool, err error) {
	conn, _, err := p.open(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = conn.Close(ctx) }()
	err = conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL, to_regclass($2) IS NOT NULL",
		quoteIdent(schema)+"."+quoteIdent(table), quoteIdent(schema)+"."+quoteIdent(aside)).Scan(&live, &kept)
	return live, kept, err
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
// store that could not be cleared goes on taking the sink's writes. A delete
// that fails is looked at again before anything is undone: through a proxy
// that answered 502 to a delete the cluster had carried out, the index was
// gone, and purging the clone as well left neither.
func (e *elastic) Clear(ctx context.Context, measurement string, at time.Time) (Aside, bool, error) {
	if err := clearable(measurement); err != nil {
		return Aside{}, false, untouched(err)
	}
	index := e.index(measurement)
	aside := Aside{
		Name: index + "-" + strings.ToLower(at.UTC().Format(stampLayout)), Measurement: measurement,
		At: at.UTC().Truncate(time.Second),
	}
	base := strings.TrimSuffix(e.sink.URL, "/") + "/"
	_, found, err := e.count(ctx, base+url.PathEscape(index), nil)
	if err != nil || !found {
		return Aside{}, false, untouched(err)
	}
	if _, _, err = e.call(ctx, http.MethodPut, base+url.PathEscape(index)+"/_block/write", nil); err != nil {
		return Aside{}, true, untouched(errors.Join(err, e.unblock(ctx, index)))
	}
	if err = e.cloneAside(ctx, index, aside.Name); err != nil {
		return Aside{}, true, untouched(errors.Join(err, e.unblock(ctx, index)))
	}
	if _, _, err = e.call(ctx, http.MethodDelete, base+url.PathEscape(index), nil); err == nil {
		return aside, true, nil
	}
	_, still, lookErr := e.count(ctx, base+url.PathEscape(index), nil)
	switch {
	case lookErr != nil:
		// Not known either way: the block comes off in case the index is
		// there, and the clone stays, named, to be purged in its turn.
		return aside, true, errors.Join(err, fmt.Errorf("whether %s was deleted could not be read back: %w", index, lookErr),
			e.unblock(ctx, index))
	case !still:
		return aside, true, nil
	}
	if purgeErr := e.Purge(ctx, aside.Name); purgeErr != nil {
		return aside, true, untouched(errors.Join(err, e.unblock(ctx, index), purgeErr))
	}
	return Aside{}, true, untouched(errors.Join(err, e.unblock(ctx, index)))
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
