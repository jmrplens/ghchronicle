package teardown

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/httpx"
)

// timeout bounds one call. Dropping a table can take a moment on a store that
// is compacting, and nothing here is on the sweep's path.
const timeout = 60 * time.Second

// client carries every call to a store, with a pool of its own rather than
// http.DefaultClient's, for the reason internal/httpx gives: the tests here
// run in parallel against httptest servers, and one of them closing its
// server empties the process-wide pool under a request another test has in
// flight, which then fails with nothing to say about itself.
var client = &http.Client{Transport: httpx.OwnTransport()}

// ── InfluxDB ────────────────────────────────────────────────────────────────

type influx struct {
	sink *config.InfluxSink
	// described is what /ping said, once asked, and v2 whether it is an
	// InfluxDB 2, which is asked in Flux rather than in SQL.
	described string
	v2        bool
	// lingering is what the last Holds found deleted and not yet purged.
	lingering []string
}

func (i *influx) Name() string { return "influxdb" }

// Holds asks the catalog rather than guessing. A table this wrote and no
// longer writes is still in information_schema, which is the whole point of
// asking: those are the ones an uninstall is for.
//
// A table InfluxDB 3 has already deleted is left out. The server renames it
// to <name>-<instant>, keeps listing it and answering queries of it until it
// purges it 24 hours later, and answers a delete of it with a 409 whether or
// not hard_delete_at is sent (measured on 3.11.2). Listed, it was "removed"
// on every uninstall and still there after. Lingering says which they are.
func (i *influx) Holds(ctx context.Context) ([]string, error) {
	names, err := i.tables(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	i.lingering = nil
	for _, name := range names {
		switch {
		case !ours(name):
		case softDeleted(name):
			i.lingering = append(i.lingering, name)
		default:
			out = append(out, name)
		}
	}
	return out, nil
}

// softDeleted says whether an InfluxDB 3 table name is one the server gave a
// table it deleted: the name, a dash and the instant. Such a table is still
// listed and still answers queries until the server purges it.
func softDeleted(name string) bool {
	i := strings.LastIndexByte(name, '-')
	return i > 0 && asideStamp.MatchString(name[i:])
}

// asideStamp is the tail of the name InfluxDB 3 gives a table it deleted: a
// dash and the instant, YYYYMMDDTHHMMSS in UTC (measured on 3.11.2). A copy
// a migration keeps elsewhere is named the same way, in lower case in an
// Elasticsearch index.
var asideStamp = regexp.MustCompile(`^-(\d{8}[Tt]\d{6})$`)

// Lingering is the tables of this project's that the last Holds found
// deleted and not yet purged.
func (i *influx) Lingering() []string { return i.lingering }

// tables is every table of the database, sorted.
func (i *influx) tables(ctx context.Context) ([]string, error) {
	const q = "SELECT table_name FROM information_schema.tables " +
		"WHERE table_schema = 'iox' ORDER BY table_name"
	var rows []struct {
		Name string `json:"table_name"`
	}
	if err := i.sql(ctx, q, &rows); err != nil {
		return nil, fmt.Errorf("reading the table list: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Name)
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
//
// In lower case, as the sink writes it: an index name may not carry an
// upper-case letter, and _cat/indices matches case and all, so a prefix
// configured as Rev96Up listed nothing (measured on 9.5.3) and a copy set
// aside under it was never found to be purged.
func (e *elastic) Holds(ctx context.Context) ([]string, error) {
	endpoint := strings.TrimSuffix(e.sink.URL, "/") + "/_cat/indices/" +
		url.PathEscape(e.prefix()+"*") + "?format=json&h=index"
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
	if e.sink.Prefix == "" || !strings.HasPrefix(item, e.prefix()) {
		return fmt.Errorf("%s is not under the sink's prefix %q", item, e.prefix())
	}
	endpoint := strings.TrimSuffix(e.sink.URL, "/") + "/" + url.PathEscape(item)
	_, _, err := e.call(ctx, http.MethodDelete, endpoint, map[int]bool{http.StatusNotFound: true})
	return err
}

// prefix is the sink's prefix as its index names carry it.
func (e *elastic) prefix() string { return strings.ToLower(e.sink.Prefix) }

func (e *elastic) call(ctx context.Context, method, endpoint string,
	tolerate map[int]bool,
) (body []byte, status int, err error) {
	return send(ctx, method, endpoint, e.authorize, tolerate)
}

// authorize applies the sink's credential, an API key or a user and password.
func (e *elastic) authorize(r *http.Request) {
	switch {
	case e.sink.APIKey != "":
		r.Header.Set("Authorization", "ApiKey "+e.sink.APIKey)
	case e.sink.Username != "":
		r.SetBasicAuth(e.sink.Username, e.sink.Password)
	}
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
	answer, err := exchange(ctx, request{method: method, endpoint: endpoint, prepare: auth}, tolerate)
	return answer.body, answer.status, err
}

// request is one call to a store: what to send, and what to add to it before
// it goes, a credential and the headers a body needs.
type request struct {
	method, endpoint string
	payload          []byte
	prepare          func(*http.Request)
}

// answer is what a store sent back.
type answer struct {
	body   []byte
	header http.Header
	status int
}

// exchange sends one request within the timeout and reads the whole answer.
// A status outside 2xx that the caller did not tolerate is an error carrying
// the store's own complaint.
func exchange(ctx context.Context, r request, tolerate map[int]bool) (answer, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var payload io.Reader = http.NoBody
	if r.payload != nil {
		payload = bytes.NewReader(r.payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.endpoint, payload)
	if err != nil {
		return answer{}, err
	}
	if r.prepare != nil {
		r.prepare(req)
	}
	res, err := client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer res.Body.Close()
	out := answer{header: res.Header, status: res.StatusCode}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return out, err
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 || tolerate[res.StatusCode] {
		out.body = body
		return out, nil
	}
	return out, &statusError{status: res.StatusCode, text: res.Status + ": " + trim(string(body))}
}

// statusError is a store's answer outside 2xx, with the store's own
// complaint.
type statusError struct {
	status int
	text   string
}

func (e *statusError) Error() string { return e.text }

// refused says whether a store turned a request down: a 4xx is an answer
// about the request, given before anything was done, where a 5xx or a
// connection that broke says nothing about whether it was.
func refused(err error) bool {
	var s *statusError
	return errors.As(err, &s) && s.status >= 400 && s.status < 500
}

// complaint is how much of a store's complaint an error carries: one line's
// worth.
const complaint = 200

// trim keeps a store's complaint to one line's worth.
func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= complaint {
		return s
	}
	return s[:complaint] + "..."
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
