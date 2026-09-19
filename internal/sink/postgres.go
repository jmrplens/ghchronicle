package sink

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
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
	shapes := sqlShapes(points)
	if err := p.declareAll(ctx, points, shapes); err != nil {
		return 0, err
	}
	return p.upsertAll(ctx, points, shapes)
}

// declareAll brings the tables up to the shape of this batch, once per
// measurement rather than once per point: unlike the file, a connection does
// not rotate underneath and forget what it was told.
func (p *Postgres) declareAll(ctx context.Context, points []Point,
	shapes map[string]*sqlShape,
) error {
	seen := map[string]bool{}
	for _, point := range points {
		if seen[point.Measurement] {
			continue
		}
		seen[point.Measurement] = true
		for _, ddl := range p.schema.declare(point.Measurement, shapes[point.Measurement]) {
			if _, err := p.pool.Exec(ctx, ddl); err != nil {
				return fmt.Errorf("%s: %w", strings.TrimSuffix(ddl, ";"), err)
			}
		}
	}
	return nil
}

// upsertAll sends the rows in batches. One failed statement fails the batch it
// is in: a sweep that reports having written what the database refused is
// worse than a sweep that reports the refusal.
func (p *Postgres) upsertAll(ctx context.Context, points []Point,
	shapes map[string]*sqlShape,
) (int, error) {
	written := 0
	batch := &pgx.Batch{}
	send := func() error {
		if batch.Len() == 0 {
			return nil
		}
		results := p.pool.SendBatch(ctx, batch)
		err := results.Close()
		batch = &pgx.Batch{}
		return err
	}
	for _, point := range points {
		cols, vals, ok := sqlCells(point, shapes[point.Measurement])
		if !ok {
			continue
		}
		batch.Queue(p.upsert(point.Measurement, cols, shapes[point.Measurement]), vals...)
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
