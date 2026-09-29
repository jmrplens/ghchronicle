//go:build dockere2e

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/dashboards"
	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// What applying a migration does to the real stores, each in a namespace of
// its own: the old rows set aside under a name of their own and only those,
// the name taken at once in the new shape, the sink of the running process
// writing on across it, the dashboards' queries not reading the copy, and
// the copy purged once it has been kept its day, with every other table,
// index, schema, database and prefix as it was.

// Two namespaces: the one cleared, and one holding the same measurement that
// nothing may touch.
const (
	applyNamespace = "migrate96apply"
	applyOther     = "migrate96other"
)

// newComment is the comment of oldComments in 2.6.1's shape, no is_answer
// at all and answers saying whether it is the accepted one, as the collector
// writes it.
func newComment() sink.Point {
	return sink.Point{
		Measurement: "gh_discussion_comment",
		Tags: map[string]string{
			"author": "a", "comment": "1", "full_name": "o/r", "is_reply": "false", "number": "1",
			"own": "true", "owner": "o", "repo": "r", "user": "octocat",
		},
		Fields: map[string]any{
			"answers": 1, "comments": 1, "title": "t",
			"url": "https://github.com/o/r/discussions/1#discussioncomment-1",
		},
		Time: time.Unix(1700000000, 0).UTC(),
	}
}

func TestClearingSetsTheOldRowsAsideAndNothingElse(t *testing.T) {
	c := clearStores(t)
	t.Run("influxdb", c.influx)
	t.Run("postgres", c.postgres)
	t.Run("elasticsearch", c.elasticsearch)
	t.Run("purged after a day", c.purged)
}

// cleared is the three stores after their comments were set aside, and what
// the checks of each need.
type cleared struct {
	ctx    context.Context
	s      *Stack
	cfg    *config.Config
	pg     *sink.Postgres
	at     time.Time
	asides map[string]teardown.Aside
}

// clearStores seeds both namespaces in every store, lets the running
// process's PostgreSQL sink write the new shape beside the old rows, which is
// 2.6.1 before any migration and leaves the sink remembering the table, and
// then clears gh_discussion_comment in the one namespace.
func clearStores(t *testing.T) *cleared {
	t.Helper()
	c := &cleared{ctx: t.Context(), s: Start(t), at: time.Now().UTC(), asides: map[string]teardown.Aside{}}
	for _, ns := range []string{applyNamespace, applyOther} {
		seedInflux(c.ctx, t, c.s, ns)
		seedPostgresAsWritten(c.ctx, t, c.s, ns)
		seedElasticsearch(c.ctx, t, c.s, ns)
	}
	c.cfg = &config.Config{GitHub: config.GitHub{Token: "test-token"}, Targets: config.Targets{User: "octocat"}, Sinks: config.Sinks{
		Influx:        &config.InfluxSink{URL: c.s.InfluxURL, Token: c.s.InfluxToken, Bucket: applyNamespace},
		Postgres:      &config.PostgresSink{DSN: pgSinkDSN(c.s) + "&search_path=" + applyNamespace},
		Elasticsearch: &config.ElasticsearchSink{URL: c.s.ElasticsearchURL, Prefix: applyNamespace},
	}}
	if err := c.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c.pg = sink.NewPostgres(c.cfg.Sinks.Postgres.DSN, 10)
	t.Cleanup(func() { _ = c.pg.Close() })
	if _, err := c.pg.Write(c.ctx, []sink.Point{newComment()}); err != nil {
		t.Fatalf("the 2.6.1 write beside the old rows: %v", err)
	}
	for _, store := range teardown.Clearers(c.cfg) {
		start := time.Now()
		aside, held, err := store.Clear(c.ctx, "gh_discussion_comment", c.at)
		if err != nil || !held || aside.Name == "" {
			t.Fatalf("%s: %+v, held %v, %v", store.Name(), aside, held, err)
		}
		t.Logf("%s set gh_discussion_comment aside as %s in %s", store.Name(), aside.Name,
			time.Since(start).Round(time.Millisecond))
		c.asides[store.Name()] = aside
	}
	t.Cleanup(func() {
		// The clone, if a failure left it: an index delete names every index
		// whole, since a pattern is refused by default.
		req, err := http.NewRequestWithContext(context.WithoutCancel(c.ctx), http.MethodDelete,
			c.s.ElasticsearchURL+"/"+c.asides["elasticsearch"].Name, http.NoBody)
		if err != nil {
			return
		}
		if res, doErr := stackClient.Do(req); doErr == nil {
			_ = res.Body.Close()
		}
	})
	return c
}

// influx: the table went aside under the name InfluxDB gave it, still
// answering, the other table and the other database are as they were, and
// the name is taken at once in the new shape.
func (c *cleared) influx(t *testing.T) {
	tables := asideInfluxTables(c.ctx, t, c.s, applyNamespace)
	want := []string{c.asides["influxdb"].Name, "gh_dependabot_alert_item"}
	slices.Sort(want)
	if !slices.Equal(tables, want) {
		t.Errorf("tables %v, want %v", tables, want)
	}
	if n := asideInfluxCount(c.ctx, t, c.s, applyNamespace, c.asides["influxdb"].Name); n != 2 {
		t.Errorf("the copy answers %d rows, want the 2 it held", n)
	}
	// When the server purges it, as its _internal system table says: 72
	// hours after the delete, not the 24 every text here said before this
	// was read back (measured on 3.11.2 and 3.11.5).
	if a := c.asides["influxdb"]; !a.Until.Equal(a.At.Add(72 * time.Hour)) {
		t.Errorf("the server purges its copy from %s, want 72 hours after %s", a.Until, a.At)
	}
	if n := asideInfluxCount(c.ctx, t, c.s, applyOther, "gh_discussion_comment"); n != 2 {
		t.Errorf("the other database's table holds %d rows, want its 2", n)
	}
	influx := sink.NewInflux(c.s.InfluxURL, c.s.InfluxToken, "", applyNamespace, 10, 10*time.Second)
	if _, err := influx.Write(c.ctx, []sink.Point{newComment()}); err != nil {
		t.Fatalf("writing the new shape under the old name: %v", err)
	}
	if n := asideInfluxCount(c.ctx, t, c.s, applyNamespace, "gh_discussion_comment"); n != 1 {
		t.Errorf("the table under the old name holds %d rows, want the new one", n)
	}
}

// postgres: the sink that had declared the table writes on after it was
// renamed under it, into a table keyed the new way, and the copy, the other
// table and the other schema hold what they held.
func (c *cleared) postgres(t *testing.T) {
	if _, err := c.pg.Write(c.ctx, []sink.Point{newComment()}); err != nil {
		t.Fatalf("the sink's write after the table was set aside: %v", err)
	}
	key := asidePsql(c.ctx, t, c.s, `SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid `+
		`AND a.attnum = ANY(i.indkey) WHERE i.indisprimary AND i.indrelid = '`+applyNamespace+`.gh_discussion_comment'::regclass `+
		`ORDER BY array_position(i.indkey::int2[], a.attnum)`)
	if slices.Contains(key, "is_answer") || !slices.Contains(key, "comment") {
		t.Errorf("the table under the old name is keyed on %v", key)
	}
	for table, want := range map[string]string{
		applyNamespace + `."` + c.asides["postgres"].Name + `"`: "3",
		applyNamespace + ".gh_discussion_comment":               "1",
		applyNamespace + ".gh_dependabot_alert_item":            "1",
		applyOther + ".gh_discussion_comment":                   "2",
	} {
		if got := asidePsql(c.ctx, t, c.s, "SELECT count(*) FROM "+table); !slices.Equal(got, []string{want}) {
			t.Errorf("%s holds %v rows, want %s", table, got, want)
		}
	}
}

// elasticsearch: the sink's next write creates the index afresh, the clone
// and the other indices hold what they held, and no query of the dashboards
// reads the clone over the whole prefix.
func (c *cleared) elasticsearch(t *testing.T) {
	es := sink.NewElasticsearch(c.s.ElasticsearchURL, applyNamespace, "", "", "", 10, 10*time.Second)
	if _, err := es.Write(c.ctx, []sink.Point{newComment()}); err != nil {
		t.Fatalf("writing the new shape under the old index: %v", err)
	}
	storeCall(c.ctx, t, http.MethodPost, c.s.ElasticsearchURL+"/"+applyNamespace+"-*/_refresh", "", "")
	for index, want := range map[string]int{
		c.asides["elasticsearch"].Name:               2,
		applyNamespace + "-gh_discussion_comment":    1,
		applyNamespace + "-gh_dependabot_alert_item": 1,
		applyOther + "-gh_discussion_comment":        2,
	} {
		if got := asideESCount(c.ctx, t, c.s, index, ""); got != want {
			t.Errorf("%s holds %d documents, want %d", index, got, want)
		}
	}
	for _, query := range asideDashboardQueries(t, "gh_discussion_comment") {
		query = strings.ReplaceAll(query, "ghchronicle-", applyNamespace+"-")
		if got := asideESCount(c.ctx, t, c.s, applyNamespace+"-*", query); got > 1 {
			t.Errorf("%q counts %d documents over %s-*, the copy's among them", query, got, applyNamespace)
		}
	}
}

// purged: a day not yet over purges nothing and says when the next copy is
// due; once it is over, PostgreSQL's copy and Elasticsearch's clone go, found
// by asking the stores, as a new state file has to, and nothing else does.
func (c *cleared) purged(t *testing.T) {
	state := run.LoadState("")
	migrate.Stamp(state, c.cfg, "2.6.2")
	purge := migrate.Purging{
		Config: c.cfg, State: state, Ask: true, Now: c.at.Add(teardown.Grace - time.Minute),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if next := purge.Run(c.ctx); !next.Equal(c.at.Truncate(time.Second).Add(teardown.Grace)) {
		t.Errorf("a day not yet over: the next purge is due %s", next)
	}
	copyRows := `SELECT count(*) FROM ` + applyNamespace + `."` + c.asides["postgres"].Name + `"`
	if got := asidePsql(c.ctx, t, c.s, copyRows); !slices.Equal(got, []string{"3"}) {
		t.Fatalf("the copy went before its day was over: %v", got)
	}
	purge.Now = c.at.Add(teardown.Grace + time.Minute)
	purge.Run(c.ctx)
	left := asidePsql(c.ctx, t, c.s, `SELECT schemaname || '.' || tablename FROM pg_tables WHERE schemaname IN ('`+
		applyNamespace+`', '`+applyOther+`') ORDER BY 1`)
	want := []string{
		applyNamespace + ".gh_dependabot_alert_item", applyNamespace + ".gh_discussion_comment",
		applyOther + ".gh_dependabot_alert_item", applyOther + ".gh_discussion_comment",
	}
	if !slices.Equal(left, want) {
		t.Errorf("postgres holds %v after the purge, want %v", left, want)
	}
	if got := asideESCount(c.ctx, t, c.s, c.asides["elasticsearch"].Name, ""); got != -1 {
		t.Errorf("the clone still answers with %d documents", got)
	}
	for _, index := range []string{
		applyNamespace + "-gh_discussion_comment", applyNamespace + "-gh_dependabot_alert_item",
		applyOther + "-gh_discussion_comment",
	} {
		if got := asideESCount(c.ctx, t, c.s, index, ""); got < 1 {
			t.Errorf("%s went with the clone", index)
		}
	}
	if tables := asideInfluxTables(c.ctx, t, c.s, applyNamespace); !slices.Contains(tables, c.asides["influxdb"].Name) {
		t.Errorf("InfluxDB's copy, the server's to purge, was touched: %v", tables)
	}
}

// TestTheSQLFileReplaysIntoTheNewShape: a file 2.6.0 wrote, then what this
// release writes after the migration, piped through psql as the sink's page
// says. Without the drop the new rows are refused against the old key; with
// it the file loads whole, and the table is keyed the new way.
func TestTheSQLFileReplaysIntoTheNewShape(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	const schema = "migrate96sql"
	load := func(file string) (string, error) {
		prefix := "DROP SCHEMA IF EXISTS " + schema + " CASCADE; CREATE SCHEMA " + schema + "; SET search_path TO " + schema + ";\n"
		return s.LoadSQL(ctx, strings.NewReader(prefix+file))
	}
	t.Cleanup(func() { _, _ = s.Psql(context.WithoutCancel(ctx), "DROP SCHEMA IF EXISTS "+schema+" CASCADE;") })

	if out, err := load(sqlFileAfterUpgrade(t, false)); err == nil || !strings.Contains(out, "no unique or exclusion constraint") {
		t.Errorf("without the drop the file loaded, or failed for another reason: %v\n%s", err, out)
	}
	file := sqlFileAfterUpgrade(t, true)
	if strings.Count(file, `DROP TABLE IF EXISTS "gh_discussion_comment";`) != 1 {
		t.Fatalf("the file does not carry the drop once:\n%s", file)
	}
	if out, err := load(file); err != nil {
		t.Fatalf("the file with the drop did not load: %v\n%s", err, out)
	}
	comments := asidePsql(ctx, t, s, "SELECT comment || ':' || answers::text FROM "+schema+".gh_discussion_comment")
	if !slices.Equal(comments, []string{"1:1"}) {
		t.Errorf("the comments replayed as %v, want the one comment, accepted", comments)
	}
	if got := asidePsql(ctx, t, s, "SELECT count(*) FROM "+schema+".gh_repo"); !slices.Equal(got, []string{"1"}) {
		t.Errorf("gh_repo replayed as %v", got)
	}
}

// sqlFileAfterUpgrade is a SQL file as 2.6.0 wrote it, the comment read
// before and after its acceptance under is_answer, followed by what this
// release's sink writes into the same file in a process of its own: the drop
// first when a migration was applied, then the comment in the new shape.
func sqlFileAfterUpgrade(t *testing.T, drop bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "points.sql")
	at := time.Unix(1700000000, 0).UTC()
	repo := sink.Point{
		Measurement: "gh_repo", Tags: map[string]string{"full_name": "o/r"},
		Fields: map[string]any{"stars": 3}, Time: at,
	}
	var earlier []sink.Point
	for answers, tag := range []string{"false", "true"} {
		earlier = append(earlier, sink.Point{
			Measurement: "gh_discussion_comment", Time: at,
			Tags:   map[string]string{"comment": "1", "user": "octocat", "is_answer": tag},
			Fields: map[string]any{"answers": answers, "comments": 1},
		})
	}
	writeSQL(t, path, false, append(earlier, repo))
	writeSQL(t, path, drop, []sink.Point{{
		Measurement: "gh_discussion_comment", Time: at,
		Tags: map[string]string{"comment": "1", "user": "octocat"}, Fields: map[string]any{"answers": 1, "comments": 1},
	}, repo})
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// writeSQL is one process's SQL sink appending to the file.
func writeSQL(t *testing.T, path string, drop bool, points []sink.Point) {
	t.Helper()
	w := sink.NewSQL("postgres", path, 0, 0)
	if drop {
		if err := w.Drop("gh_discussion_comment"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(t.Context(), points); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTheGraphiteCommandRemovesTheOldDepthAlone runs the command -migrate
// prints on the Graphite host, inside the container, against files the sink
// wrote in both shapes under a prefix of its own.
func TestTheGraphiteCommandRemovesTheOldDepthAlone(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	const prefix = "migrate96g"
	root := "/opt/graphite/storage/whisper/" + prefix
	t.Cleanup(func() { _, _ = s.Exec(context.WithoutCancel(ctx), "graphite", "rm", "-rf", root) })
	files := func() []string {
		out, _ := s.Exec(ctx, "graphite", "sh", "-c", "cd "+root+" 2>/dev/null && find . -name '*.wsp' | sort")
		return strings.Fields(out)
	}

	old := newComment()
	old.Tags = maps.Clone(old.Tags)
	old.Tags["is_answer"] = "true"
	old.Fields = map[string]any{"answers": 1, "comments": 1}
	other := sink.Point{
		Measurement: "gh_discussion_comment_x", Tags: map[string]string{"full_name": "o/r"},
		Fields: map[string]any{"stars": 3}, Time: old.Time,
	}
	g := sink.NewGraphite(s.GraphiteAddr, prefix, 100, 10*time.Second)
	if _, err := g.Write(ctx, []sink.Point{old, newComment(), other}); err != nil {
		t.Fatal(err)
	}
	_ = g.Close()
	if err := WaitUntil(ctx, "carbon to write both shapes", time.Minute, func(context.Context) error {
		if n := len(files()); n < 5 {
			return fmt.Errorf("%d files so far", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := files()
	for _, c := range graphiteCommands(t, s.GraphiteAddr, prefix) {
		c = strings.ReplaceAll(c, "<storage>", "/opt/graphite/storage")
		if out, err := s.Exec(ctx, "graphite", "sh", "-c", c); err != nil {
			t.Fatalf("%s: %v\n%s", c, err, out)
		}
	}
	after := files()
	gone := slices.DeleteFunc(slices.Clone(before), func(f string) bool { return slices.Contains(after, f) })
	// A path is ./, the measurement, a node per tag and the field: eleven
	// separators in the new shape's nine tags, twelve in the old one's ten.
	if !slices.Equal(depths(gone), []int{12, 12}) {
		t.Errorf("the command took %v, want the old shape's two fields", gone)
	}
	if !slices.Equal(depths(after), []int{11, 11}) {
		t.Errorf("left %v, want the new shape's two fields", after)
	}
	if !slices.ContainsFunc(after, func(f string) bool { return strings.HasPrefix(f, "./discussion_comment_x/") }) {
		t.Errorf("the other measurement's files went: %v", after)
	}
}

// depths is how deep each file of gh_discussion_comment is.
func depths(files []string) []int {
	var out []int
	for _, f := range files {
		if strings.HasPrefix(f, "./discussion_comment/") {
			out = append(out, strings.Count(f, "/"))
		}
	}
	return out
}

// graphiteCommands is what -migrate says to run on the Graphite host for a
// store whose first writer is not known.
func graphiteCommands(t *testing.T, addr, prefix string) []string {
	t.Helper()
	cfg := &config.Config{GitHub: config.GitHub{Token: "test-token"}, Targets: config.Targets{User: "octocat"}, Sinks: config.Sinks{
		Graphite: &config.GraphiteSink{Addr: addr, Prefix: prefix},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	state := run.LoadState("")
	state.LastRun["repo"] = time.Now()
	plan := migrate.Make(t.Context(), migrate.Input{Config: cfg, State: state, Release: "2.6.2", Now: time.Now()})
	for _, it := range plan.Stores[0].Items {
		if it.Migration.Measurement == "gh_discussion_comment" && len(it.Commands) == 2 {
			return it.Commands
		}
	}
	t.Fatalf("the plan gives no commands: %+v", plan.Stores[0].Items)
	return nil
}

// oldPoints is the comment of oldComments as 2.6.0 wrote it, read before and
// after its acceptance, and a Dependabot alert beside it.
func oldPoints() []sink.Point {
	var out []sink.Point
	for _, accepted := range []bool{false, true} {
		p := newComment()
		p.Tags["is_answer"] = strconv.FormatBool(accepted)
		p.Fields = map[string]any{"answers": map[bool]int{false: 0, true: 1}[accepted], "comments": 1}
		out = append(out, p)
	}
	return append(out, sink.Point{
		Measurement: "gh_dependabot_alert_item",
		Tags:        map[string]string{"full_name": "o/r", "number": "1", "owner": "o", "repo": "r", "severity": "high"},
		Fields:      map[string]any{"alerts": 1, "alert_state": "open"}, Time: time.Unix(1705000000, 0).UTC(),
	})
}

// seedPostgresAsWritten makes the tables the way 2.6.0's sink made them, by
// replaying what its SQL file said, in a schema of its own.
func seedPostgresAsWritten(ctx context.Context, t *testing.T, s *Stack, ns string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "points.sql")
	written := sink.NewSQL("postgres", path, 0, 0)
	if _, err := written.Write(ctx, oldPoints()); err != nil {
		t.Fatal(err)
	}
	if err := written.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "DROP SCHEMA IF EXISTS " + ns + " CASCADE; CREATE SCHEMA " + ns + "; SET search_path TO " + ns + ";\n"
	if out, loadErr := s.LoadSQL(ctx, strings.NewReader(prefix+string(body))); loadErr != nil {
		t.Fatalf("%v\n%s", loadErr, out)
	}
	t.Cleanup(func() {
		_, _ = s.Psql(context.WithoutCancel(ctx), "DROP SCHEMA IF EXISTS "+ns+" CASCADE;")
	})
}

// asideInfluxTables is every table of a database, soft deleted ones among them.
func asideInfluxTables(ctx context.Context, t *testing.T, s *Stack, db string) []string {
	t.Helper()
	var rows []struct {
		Name string `json:"table_name"`
	}
	asideInfluxSQL(ctx, t, s, db, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'iox' ORDER BY table_name", &rows)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// asideInfluxCount is how many rows a table answers.
func asideInfluxCount(ctx context.Context, t *testing.T, s *Stack, db, table string) int {
	t.Helper()
	var rows []struct {
		N int `json:"n"`
	}
	asideInfluxSQL(ctx, t, s, db, `SELECT count(*) AS n FROM "`+table+`"`, &rows)
	if len(rows) != 1 {
		return -1
	}
	return rows[0].N
}

func asideInfluxSQL(ctx context.Context, t *testing.T, s *Stack, db, q string, into any) {
	t.Helper()
	endpoint := s.InfluxURL + "/api/v3/query_sql?" + url.Values{"db": {db}, "q": {q}, "format": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.InfluxToken)
	res, err := stackClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s %s", q, res.Status, body)
	}
	if err = json.Unmarshal(body, into); err != nil {
		t.Fatalf("%s: %v: %s", q, err, body)
	}
}

// asidePsql is a query's rows, one line each.
func asidePsql(ctx context.Context, t *testing.T, s *Stack, q string) []string {
	t.Helper()
	out, err := s.Psql(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v\n%s", q, err, out)
	}
	return strings.Fields(strings.TrimSpace(out))
}

// asideESCount is how many documents an index, or a pattern, holds that match a
// query_string, -1 for an index that is not there.
func asideESCount(ctx context.Context, t *testing.T, s *Stack, index, query string) int {
	t.Helper()
	body := ""
	if query != "" {
		body = `{"query":{"query_string":{"query":` + strconv.Quote(query) + `}}}`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ElasticsearchURL+"/"+index+"/_count", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := stackClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotFound {
		return -1
	}
	var counted struct {
		Count int `json:"count"`
	}
	if err = json.Unmarshal(answer, &counted); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s %s", index, res.Status, answer)
	}
	return counted.Count
}

// asideDashboardQueries is every Elasticsearch query of the shipped dashboards that
// reads a measurement's index.
func asideDashboardQueries(t *testing.T, measurement string) []string {
	t.Helper()
	var out []string
	var walk func(panels []map[string]any)
	walk = func(panels []map[string]any) {
		for _, p := range panels {
			if inner, ok := p["panels"].([]map[string]any); ok {
				walk(inner)
			}
			var targets []any
			switch list := p["targets"].(type) {
			case []any:
				targets = list
			case []map[string]any:
				for _, target := range list {
					targets = append(targets, target)
				}
			}
			for _, raw := range targets {
				target, _ := raw.(map[string]any)
				if q, _ := target["query"].(string); strings.Contains(q, "_index:ghchronicle-"+measurement) &&
					!strings.Contains(q, "_index:ghchronicle-"+measurement+"_") {
					out = append(out, q)
				}
			}
		}
	}
	walk(dashboards.Render("elasticsearch", "ds", nil))
	if len(out) == 0 {
		t.Fatalf("no dashboard query reads %s", measurement)
	}
	return out
}
