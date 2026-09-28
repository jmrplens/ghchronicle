package sink

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres writes to a PostgreSQL that is running, rather than to a file for
// somebody to replay later.
//
// It is the SQL sink's other half and not its replacement. The file is still
// what you want when the database is somewhere this cannot reach, when the
// load is meant to happen later or under review, or when the reader is not
// PostgreSQL at all: the statements are ordinary SQL and another engine can
// take them. This one is for the Grafana user whose PostgreSQL is right there,
// and it saves them the step of piping a file into psql on a timer.
//
// The schema is the same one, deliberately, down to the primary key: the
// dashboards this project publishes query these tables, and cmd/check_postgres
// EXPLAINs every one of those queries against this shape. Two sinks writing
// two schemas would make one of the two dashboards a lie.
//
// The statements are parameterised rather than rendered. The file sink has to
// say its values because its output is text somebody else runs; here there is
// a connection, so the values go as parameters and only the identifiers, which
// this project chooses and quotes, are part of the statement.
type Postgres struct {
	dsn string
	// batch is how many upserts go in one round trip.
	batch int

	mu     sync.Mutex
	pool   *pgxpool.Pool
	schema *sqlSchema
}

// NewPostgres returns a sink that has not connected yet. Connecting on the
// first write rather than here is what lets a run that never reaches this sink
// start with the database down, which is the ordinary case for `-list` or a
// config check.
func NewPostgres(dsn string, batch int) *Postgres {
	if batch <= 0 {
		batch = 1000
	}
	return &Postgres{dsn: dsn, batch: batch, schema: newSQLSchema()}
}

// Name is what this sink is called in the config and in a log line.
func (p *Postgres) Name() string { return "postgres" }

// Close gives the connections back.
func (p *Postgres) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pool != nil {
		p.pool.Close()
		p.pool = nil
	}
	return nil
}

// connect opens the pool once.
func (p *Postgres) connect(ctx context.Context) error {
	if p.pool != nil {
		return nil
	}
	pool, err := pgxpool.New(ctx, p.dsn)
	if err != nil {
		return fmt.Errorf("the postgres sink could not read its dsn: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return fmt.Errorf("the postgres sink could not reach its database: %w", err)
	}
	p.pool = pool
	return nil
}

// Write upserts every point. The count is the rows written; a point with no
// field worth a column is not one.
func (p *Postgres) Write(ctx context.Context, points []Point) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(points) == 0 {
		return 0, nil
	}
	if err := p.connect(ctx); err != nil {
		return 0, err
	}
	return p.write(ctx, poolSchema{p.pool}, p.pool, points)
}

// Forget drops what this sink remembers of a measurement's table, so that
// its next write declares it again from the catalog. A migration calls it
// once it has set the table aside in the same process.
func (p *Postgres) Forget(measurement string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.schema.tables, measurement)
}

// batchSender is what sending the upserts asks of the database, an interface
// so that a refusal can be answered without a server.
type batchSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// write declares and upserts one batch, and once more after forgetting its
// tables when the database says one of them is not there.
//
// The sink remembers every table it declared for the life of the process, so
// a table dropped or renamed by anyone else made every later write of it fail
// with 42P01 and took the rest of its batch along: measured on 18.6, three
// writes of three after a drop by another connection, the gh_discussion row
// of the same batch with them, until a restart. A migration sets a table
// aside exactly that way, and an operator dropping one by hand is the other
// way it happens. Forgotten, the batch declares the tables again, the one
// that went is created afresh, and the batch goes through; the cost is a
// catalog read per table of the batch, 15 ms each, once.
func (p *Postgres) write(ctx context.Context, conn schemaConn, sender batchSender, points []Point) (int, error) {
	shapes := sqlShapes(points)
	n, err := p.declareAndUpsert(ctx, conn, sender, points, shapes)
	if !undefinedTable(err) {
		return n, err
	}
	for m := range shapes {
		delete(p.schema.tables, m)
	}
	return p.declareAndUpsert(ctx, conn, sender, points, shapes)
}

func (p *Postgres) declareAndUpsert(ctx context.Context, conn schemaConn, sender batchSender, points []Point,
	shapes map[string]*sqlShape,
) (int, error) {
	if err := p.declareAll(ctx, conn, points, shapes); err != nil {
		return 0, err
	}
	return p.upsertAll(ctx, sender, points, shapes)
}

// undefinedTable says whether PostgreSQL refused a statement for naming a
// table that is not there.
func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// schemaConn is what declaring a table asks of the database: to run a
// statement, and to say which columns a table already has and which of them
// are its primary key. An interface so that what is sent, and what a failed
// statement leaves recorded, can be read without a server.
type schemaConn interface {
	exec(ctx context.Context, ddl string) error
	columns(ctx context.Context, table string) (map[string]string, error)
	primaryKey(ctx context.Context, table string) ([]string, error)
}

// poolSchema is schemaConn over the sink's own pool.
type poolSchema struct{ pool *pgxpool.Pool }

func (c poolSchema) exec(ctx context.Context, ddl string) error {
	_, err := c.pool.Exec(ctx, ddl)
	return err
}

// columns reads the table the CREATE TABLE IF NOT EXISTS just named, in the
// schema that statement created it in or found it in.
func (c poolSchema) columns(ctx context.Context, table string) (map[string]string, error) {
	rows, err := c.pool.Query(ctx, `SELECT column_name, data_type FROM information_schema.columns `+
		`WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var name, typ string
	_, err = pgx.ForEachRow(rows, []any{&name, &typ}, func() error {
		out[name] = typ
		return nil
	})
	return out, err
}

// primaryKey reads the columns of the table's primary key, in the key's own
// order, from the same schema columns reads. None for a table without one.
func (c poolSchema) primaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := c.pool.Query(ctx, `SELECT a.attname FROM pg_index i `+
		`JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey) `+
		`WHERE i.indisprimary AND i.indrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1)) `+
		`ORDER BY array_position(i.indkey::int2[], a.attnum)`, table)
	if err != nil {
		return nil, err
	}
	var out []string
	var name string
	_, err = pgx.ForEachRow(rows, []any{&name}, func() error {
		out = append(out, name)
		return nil
	})
	return out, err
}

// declareAll makes each measurement's table ready for this batch, once per
// measurement rather than once per point: unlike the file, a connection does
// not rotate underneath and forget what it was told.
//
// The first time a process meets a table it asks which columns the table
// already has, and adds only the ones it lacks. Sending ADD COLUMN IF NOT
// EXISTS for every field, which is what the file says, cost a restart one
// ACCESS EXCLUSIVE lock per field on every table: PostgreSQL takes that lock
// before it checks IF NOT EXISTS, so each statement waited for any Grafana
// query reading the table, and every query after it waited behind it.
// Measured on 2026-09-27 against PostgreSQL 18.6 with a reader holding the
// table open: CREATE TABLE IF NOT EXISTS returned in 0.4 ms and the catalog
// read in 15 ms, an ADD COLUMN IF NOT EXISTS for a column already there hit a
// one second lock_timeout, and a SELECT sent behind that ALTER waited 3 s,
// until the reader finished. A restarted sink with lock_timeout=500 in its
// DSN failed its first write that way, and then its next one, on a table the
// failed batch had recorded as declared and never created; declaring from the
// catalog, its first write took 32 ms.
//
// A column is recorded only once the server has taken the statement that adds
// it, and a table only once it exists and has been read. A statement that
// fails, to a lock_timeout, a statement_timeout or a dropped connection, is
// then sent again on the next write rather than taken as done, which had left
// every INSERT naming that column refused until the process restarted.
//
// The upsert conflicts on the key the table has, read from the catalog with
// its columns, rather than on the key this release would declare. The two
// differ once a tag has moved to a field: 2.6.1 stopped writing is_answer on
// gh_discussion_comment, whose tables made earlier keep it in their key, and
// an ON CONFLICT without it matches no unique constraint. Measured on
// 2026-09-27 against PostgreSQL 18.6, "there is no unique or exclusion
// constraint matching the ON CONFLICT specification" refused the batch, and
// the gh_discussion row in the same batch with it. Conflicting on the table's
// own key, a column the point no longer carries takes its default, the empty
// string, and the new rows sit beside the old ones, the two shapes InfluxDB
// holds too, until the table is dropped and filled again.
func (p *Postgres) declareAll(ctx context.Context, conn schemaConn, points []Point,
	shapes map[string]*sqlShape,
) error {
	seen := map[string]bool{}
	for _, point := range points {
		m := point.Measurement
		if seen[m] {
			continue
		}
		seen[m] = true
		sh := shapes[m]
		tbl := p.schema.tables[m]
		if tbl == nil {
			fresh, create := newSQLTable(m, sh)
			if err := conn.exec(ctx, create); err != nil {
				return fmt.Errorf("%s: %w", strings.TrimSuffix(create, ";"), err)
			}
			existing, err := conn.columns(ctx, m)
			if err != nil {
				return fmt.Errorf("reading the columns of %s: %w", ident(m), err)
			}
			key, err := conn.primaryKey(ctx, m)
			if err != nil {
				return fmt.Errorf("reading the primary key of %s: %w", ident(m), err)
			}
			maps.Copy(fresh.cols, existing)
			if len(key) > 0 {
				fresh.key = key
			}
			p.schema.tables[m], tbl = fresh, fresh
		}
		for _, c := range tbl.missing(m, sh) {
			if err := conn.exec(ctx, c.ddl); err != nil {
				return fmt.Errorf("%s: %w", strings.TrimSuffix(c.ddl, ";"), err)
			}
			tbl.cols[c.name] = c.typ
		}
	}
	return nil
}

// upsertAll sends the rows in batches. One failed statement fails the batch it
// is in: a sweep that reports having written what the database refused is
// worse than a sweep that reports the refusal.
// row is one upsert waiting to be sent: the statement and what fills it.
type row struct {
	statement string
	values    []any
}

// rowsFor is every point that has a row to write, turned into one. A point
// with no field worth a column is not one, the rule the line protocol applies.
//
// Separate from sending, because this is the part worth reading, and reading
// it needs no server.
func (p *Postgres) rowsFor(points []Point, shapes map[string]*sqlShape) []row {
	out := make([]row, 0, len(points))
	for _, point := range points {
		cols, vals, ok := sqlCells(point, shapes[point.Measurement])
		if !ok {
			continue
		}
		out = append(out, row{
			statement: p.upsert(point.Measurement, cols, shapes[point.Measurement]),
			values:    vals,
		})
	}
	return out
}

func (p *Postgres) upsertAll(ctx context.Context, sender batchSender, points []Point,
	shapes map[string]*sqlShape,
) (int, error) {
	rows := p.rowsFor(points, shapes)
	written := 0
	batch := &pgx.Batch{}
	send := func() error {
		if batch.Len() == 0 {
			return nil
		}
		results := sender.SendBatch(ctx, batch)
		err := results.Close()
		batch = &pgx.Batch{}
		return err
	}
	for _, r := range rows {
		batch.Queue(r.statement, r.values...)
		written++
		if batch.Len() >= p.batch {
			if err := send(); err != nil {
				return written - batch.Len(), err
			}
		}
	}
	if err := send(); err != nil {
		return 0, err
	}
	return written, nil
}

// upsert is the statement for one row's worth of columns. Only identifiers go
// into it, each quoted, and every value is a placeholder.
func (p *Postgres) upsert(measurement string, cols []string, sh *sqlShape) string {
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	var updates []string
	for _, c := range cols {
		if c == "time" || slices.Contains(sh.tags, c) {
			continue
		}
		updates = append(updates, ident(c)+" = EXCLUDED."+ident(c))
	}
	// DO UPDATE rather than DO NOTHING, for the file sink's reason: today's
	// traffic row is rewritten with a higher count on every sweep, and a row
	// frozen at its first value would be the one bug the dated-point design
	// exists to avoid.
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(ident(measurement))
	b.WriteString(" (")
	b.WriteString(identList(cols))
	b.WriteString(") VALUES (")
	b.WriteString(strings.Join(placeholders, ", "))
	b.WriteString(") ON CONFLICT (")
	b.WriteString(identList(p.schema.key(measurement, sh)))
	b.WriteString(") DO UPDATE SET ")
	b.WriteString(strings.Join(updates, ", "))
	return b.String()
}
