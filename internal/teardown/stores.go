package teardown

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jmrplens/ghchronicle/internal/config"
)

// timeout bounds one call. Dropping a table can take a moment on a store that
// is compacting, and nothing here is on the sweep's path.
const timeout = 60 * time.Second

// ── InfluxDB ────────────────────────────────────────────────────────────────

type influx struct{ sink *config.InfluxSink }

func (i *influx) Name() string { return "influxdb" }

// Holds asks the catalog rather than guessing. A table this wrote and no
// longer writes is still in information_schema, which is the whole point of
// asking: those are the ones an uninstall is for.
func (i *influx) Holds(ctx context.Context) ([]string, error) {
	const q = "SELECT table_name FROM information_schema.tables " +
		"WHERE table_schema = 'iox' ORDER BY table_name"
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v3/query_sql?" + url.Values{
		"db": {i.sink.Bucket}, "q": {q}, "format": {"json"},
	}.Encode()
	body, _, err := i.call(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name string `json:"table_name"`
	}
	if err = json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("reading the table list: %w", err)
	}
	var out []string
	for _, row := range rows {
		if ours(row.Name) {
			out = append(out, row.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Drop removes one table. InfluxDB 3 answers a table that is already gone with
// a conflict, which is not a failure for something whose job is to make it be
// gone.
func (i *influx) Drop(ctx context.Context, item string) error {
	if !ours(item) {
		return fmt.Errorf("%s is not one of this project's tables", item)
	}
	endpoint := strings.TrimSuffix(i.sink.URL, "/") + "/api/v3/configure/table?" + url.Values{
		"db": {i.sink.Bucket}, "table": {item},
	}.Encode()
	_, _, err := i.call(ctx, http.MethodDelete, endpoint, map[int]bool{http.StatusConflict: true})
	return err
}

// call sends one request with the sink's credential and reads the answer.
func (i *influx) call(ctx context.Context, method, endpoint string,
	tolerate map[int]bool,
) (body []byte, status int, err error) {
	return send(ctx, method, endpoint, func(r *http.Request) {
		if i.sink.Token != "" {
			r.Header.Set("Authorization", "Bearer "+i.sink.Token)
		}
	}, tolerate)
}

// ── Elasticsearch ───────────────────────────────────────────────────────────

type elastic struct{ sink *config.ElasticsearchSink }

func (e *elastic) Name() string { return "elasticsearch" }

// Holds lists the indices under the sink's prefix. The prefix is what the sink
// writes under, so an index outside it was not put there by this.
func (e *elastic) Holds(ctx context.Context) ([]string, error) {
	endpoint := strings.TrimSuffix(e.sink.URL, "/") + "/_cat/indices/" +
		url.PathEscape(e.sink.Prefix+"*") + "?format=json&h=index"
	body, status, err := e.call(ctx, http.MethodGet, endpoint,
		map[int]bool{http.StatusNotFound: true})
	if err != nil {
		return nil, err
	}
	// A cluster with no index under the prefix answers with an empty list, and
	// an older one answers 404 with its own complaint in the body. Both mean
	// the same thing here, and only the first is a list.
	if status == http.StatusNotFound || len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if err = json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("reading the index list: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Index)
	}
	slices.Sort(out)
	return out, nil
}

// Drop deletes one index.
func (e *elastic) Drop(ctx context.Context, item string) error {
	if e.sink.Prefix == "" || !strings.HasPrefix(item, e.sink.Prefix) {
		return fmt.Errorf("%s is not under the sink's prefix %q", item, e.sink.Prefix)
	}
	endpoint := strings.TrimSuffix(e.sink.URL, "/") + "/" + url.PathEscape(item)
	_, _, err := e.call(ctx, http.MethodDelete, endpoint, map[int]bool{http.StatusNotFound: true})
	return err
}

func (e *elastic) call(ctx context.Context, method, endpoint string,
	tolerate map[int]bool,
) (body []byte, status int, err error) {
	return send(ctx, method, endpoint, func(r *http.Request) {
		switch {
		case e.sink.APIKey != "":
			r.Header.Set("Authorization", "ApiKey "+e.sink.APIKey)
		case e.sink.Username != "":
			r.SetBasicAuth(e.sink.Username, e.sink.Password)
		}
	}, tolerate)
}

// ── The SQL sink ────────────────────────────────────────────────────────────

// sqlFile is the SQL sink, which writes statements to a file rather than to a
// server. There is nothing to connect to and nothing to drop: what this wrote
// is the file, and its rotations.
type sqlFile struct{ sink *config.SQLSink }

func (s *sqlFile) Name() string { return "sql" }

// Holds is the file and whatever rotations sit beside it.
func (s *sqlFile) Holds(_ context.Context) ([]string, error) {
	if s.sink.Path == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(s.sink.Path + "*")
	if err != nil {
		return nil, err
	}
	slices.Sort(matches)
	return matches, nil
}

// Drop removes one of them. A file already gone is the outcome asked for.
func (s *sqlFile) Drop(_ context.Context, item string) error {
	if s.sink.Path == "" || !strings.HasPrefix(item, s.sink.Path) {
		return fmt.Errorf("%s is not the file this sink writes", item)
	}
	if err := os.Remove(item); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ── Shared ──────────────────────────────────────────────────────────────────

// send makes one request, applies the caller's credential, and turns anything
// that is not a success into an error carrying what the store said.
// The second return is the status, which a caller needs when it tolerated one:
// a body that came with a tolerated 404 is the store's complaint, not the
// answer, and reading it as the answer is how "no indices here" turned into a
// parse error.
func send(ctx context.Context, method, endpoint string,
	auth func(*http.Request), tolerate map[int]bool,
) (body []byte, status int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, reqErr := http.NewRequestWithContext(ctx, method, endpoint, http.NoBody)
	if reqErr != nil {
		return nil, 0, reqErr
	}
	auth(req)
	res, sendErr := http.DefaultClient.Do(req)
	if sendErr != nil {
		return nil, 0, sendErr
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		return nil, res.StatusCode, readErr
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 || tolerate[res.StatusCode] {
		return body, res.StatusCode, nil
	}
	return nil, res.StatusCode, fmt.Errorf("%s: %s", res.Status, trim(string(body), 200))
}

// trim keeps a store's complaint to one line's worth.
func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── PostgreSQL ──────────────────────────────────────────────────────────────

// postgres is the connecting sink's store, which unlike the file sink's has a
// server to ask and to tell.
type postgres struct{ sink *config.PostgresSink }

func (p *postgres) Name() string { return "postgres" }

// Holds asks the catalog which of this project's tables are there, which is
// the same question the InfluxDB store asks and for the same reason: a table
// nobody writes any more is exactly the one an uninstall is for.
func (p *postgres) Holds(ctx context.Context) ([]string, error) {
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = current_schema() `+
			`AND tablename LIKE $1 ORDER BY tablename`, Prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, scanErr
		}
		if ours(name) {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// Drop removes one table. The name is an identifier rather than a value, so it
// cannot be a parameter; it is one this project wrote, checked against the
// prefix and quoted, and nothing else reaches the statement.
func (p *postgres) Drop(ctx context.Context, item string) error {
	if !ours(item) {
		return fmt.Errorf("%s is not one of this project's tables", item)
	}
	conn, err := pgx.Connect(ctx, p.sink.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	var drop strings.Builder
	drop.WriteString("DROP TABLE IF EXISTS ")
	drop.WriteString(quoteIdent(item))
	_, err = conn.Exec(ctx, drop.String())
	return err
}

// quoteIdent is the sink's own identifier quoting, spelled again here rather
// than reached for across packages: a table this drops is one that sink wrote,
// and the two have to agree about what its name is.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
