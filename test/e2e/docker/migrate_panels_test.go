//go:build dockere2e

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// What a reader sees after an upgrade from 2.6.0, in every store and through
// the dashboards' own queries.
//
// Each store is given gh_discussion_comment the way 2.6.0 wrote it, through
// the sinks themselves, in a namespace of its own: a database, an index
// prefix and a Graphite prefix nothing else in the suite reads. Between that
// and the upgrade the maintainer of fosrl/pangolin#118 took back the answer
// they had accepted. 2.6.0 held that comment as two rows, one per value of
// is_answer, and the two panels over the measurement read one row per
// comment, accepted when any of its rows says so, so until the store is
// brought along they read it accepted however often this release writes it
// as it now stands. That is what the panels are asked here, before and after:
// under migrate: auto, where a start brings along what loses nothing and
// leaves Graphite and the SQL file to their operator, and under -migrate
// -yes, which brings along every store, Graphite by the commands it prints
// and the SQL file by the DROP it writes into it, replayed after what 2.6.0
// wrote there. A third namespace holding the same rows, which no
// configuration names, says whether anything else was touched.

// The namespaces.
const (
	panelsAuto  = "migrate96auto"
	panelsYes   = "migrate96yes"
	panelsOther = "migrate96untouched"
)

// commentPanels are the two panels that read gh_discussion_comment.
var commentPanels = []string{"Answers elsewhere", "Discussion answers"}

func TestAfterAMigrationThePanelsReadTheAnswerTakenBack(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	dir, err := sqlStoresWorkDir("migrate-panels")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	old := panelsWrittenBy260(ctx, t, binary, dir, at)

	other := seedPanels(ctx, t, s, panelsOther, old, dir)
	untouched := other.holds(ctx, t)

	for _, mode := range []struct {
		name, ns string
		args     []string
	}{
		{"migrate: auto", panelsAuto, []string{"-once"}},
		{"-migrate -yes", panelsYes, []string{"-migrate", "-yes"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ns := seedPanels(ctx, t, s, mode.ns, old, dir)
			ns.replay(ctx, t, ns.sqlBefore(t))
			for _, store := range ns.stores(true) {
				ns.wantTakenBack(ctx, t, store, 1, "before the upgrade")
			}

			beside := ns.holdsBeside(ctx, t, "gh_discussion_comment")
			gh := fakegh.New(t, sqlStoresFixtures, fakegh.TakenBackOverlay(t, sqlStoresFixtures))
			gh.FreezeAt(at)
			cfg := ns.config(t, dir, gh.URL())
			ns.dryRun(ctx, t, binary, cfg)
			stdout, stderr, runErr := panelsExec(ctx, t, binary, cfg, mode.args...)
			if runErr != nil {
				t.Fatalf("%s: %v\n%s\n%s", mode.args, runErr, stdout, stderr)
			}
			t.Logf("%s:\n%s", mode.args, stdout)
			if mode.ns == panelsAuto {
				ns.afterAStart(ctx, t, stderr)
				return
			}
			ns.afterMigrateYes(ctx, t, stdout)
			// A start sweeps after it applies, and writes every measurement
			// of the two families; -migrate -yes writes the comments alone.
			if got := ns.holdsBeside(ctx, t, "gh_discussion_comment"); got != beside {
				t.Errorf("-migrate -yes changed what it did not migrate:\nbefore %s\nafter  %s", beside, got)
			}
		})
	}
	if got := other.holds(ctx, t); got != untouched {
		t.Errorf("the namespace no configuration names changed:\nbefore %s\nafter  %s", untouched, got)
	}
}

// dryRun is -migrate against the namespace: every store found pending, and
// every store, the SQL file and the state file as they were.
func (n *panelsNS) dryRun(ctx context.Context, t *testing.T, binary, cfg string) {
	t.Helper()
	state := filepath.Join(filepath.Dir(cfg), n.name+"-state.json")
	before, stateBefore := n.holds(ctx, t), readOrEmpty(t, state)
	plan, stderr, err := panelsExec(ctx, t, binary, cfg, "-migrate")
	if err != nil {
		t.Fatalf("-migrate: %v\n%s\n%s", err, plan, stderr)
	}
	if got := strings.Count(plan, "  pending     2.6.1/gh_discussion_comment/is_answer\n"); got != 5 ||
		!strings.Contains(plan, "Nothing was changed.") {
		t.Errorf("the plan finds %d of the five stores pending:\n%s", got, plan)
	}
	if after := n.holds(ctx, t); after != before {
		t.Errorf("-migrate changed the stores:\nbefore %s\nafter  %s", before, after)
	}
	if readOrEmpty(t, state) != stateBefore {
		t.Error("-migrate wrote the state file")
	}
}

// readOrEmpty is a file's contents, or nothing for a file that is not there.
func readOrEmpty(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(body)
}

// afterAStart: the three stores that set rows aside read the answer taken
// back; Graphite and the SQL file, which a start never touches, are warned
// about with the commands, and Graphite still reads it accepted.
func (n *panelsNS) afterAStart(ctx context.Context, t *testing.T, log string) {
	t.Helper()
	for _, store := range []string{"influxdb", "postgres", "elasticsearch"} {
		want := `msg="applying a migration before the first sweep" sink=` + store + ` measurement=gh_discussion_comment`
		if !strings.Contains(log, want) {
			t.Errorf("the start does not say it applies to %s", store)
		}
	}
	for _, store := range []string{"graphite", "sql"} {
		want := `msg="migration pending" sink=` + store + ` measurement=gh_discussion_comment`
		if !strings.Contains(log, want) {
			t.Errorf("the start does not warn that %s is pending", store)
		}
	}
	if !strings.Contains(log, `msg="refill complete" families=discussions,outbound measurements=gh_discussion_comment `+
		`sinks=elasticsearch,influxdb,postgres`) {
		t.Error("the start does not say it read the comments back into the three stores it cleared")
	}
	n.onlyTheCommentsAside(ctx, t)
	for _, store := range []string{"influxdb", "postgres", "elasticsearch"} {
		n.wantTakenBack(ctx, t, store, 0, "after the start")
	}
	n.wantTakenBack(ctx, t, "graphite", 1, "after a start, which leaves Graphite to its operator")
	if body, err := os.ReadFile(n.sql); err != nil || strings.Contains(string(body), "DROP TABLE") {
		t.Errorf("a start dropped the SQL file's table, which it may not do on its own: %v", err)
	}
}

// afterMigrateYes: every store reads the answer taken back once the
// Graphite commands have been run on its host and the SQL file has been
// replayed from where it was.
func (n *panelsNS) afterMigrateYes(ctx context.Context, t *testing.T, stdout string) {
	t.Helper()
	for _, store := range []string{"influxdb", "postgres", "elasticsearch", "graphite", "sql"} {
		if !strings.Contains(stdout, "  applied     2.6.1/gh_discussion_comment/is_answer in "+store+": ") {
			t.Errorf("-migrate -yes does not say it applied to %s", store)
		}
	}
	if !strings.Contains(stdout, "  refill      read discussions and outbound again, ") ||
		!strings.Contains(stdout, "writing gh_discussion_comment to elasticsearch, graphite, influxdb, postgres and sql\n") {
		t.Error("-migrate -yes does not say it read the comments back into the five stores")
	}
	n.onlyTheCommentsAside(ctx, t)
	var commands []string
	for line := range strings.SplitSeq(stdout, "\n") {
		// The plan says them and the report says them again, as applied.
		c := strings.ReplaceAll(strings.TrimSpace(line), "<storage>", "/opt/graphite/storage")
		if strings.HasPrefix(c, "find /opt/graphite/storage/whisper/"+n.name+"/") && !slices.Contains(commands, c) {
			commands = append(commands, c)
		}
	}
	if len(commands) != 2 {
		t.Fatalf("-migrate -yes printed %q for the Graphite host", commands)
	}
	for _, c := range commands {
		if out, err := n.s.Exec(ctx, "graphite", "sh", "-c", c); err != nil {
			t.Fatalf("%s: %v\n%s", c, err, out)
		}
	}
	after, err := os.ReadFile(n.sql)
	if err != nil {
		t.Fatal(err)
	}
	appended, ok := strings.CutPrefix(string(after), n.sqlBefore(t))
	if !ok || !strings.HasPrefix(appended, `DROP TABLE IF EXISTS "gh_discussion_comment";`) {
		t.Fatalf("the SQL file does not go on from what 2.6.0 wrote with the drop:\n%.300s", appended)
	}
	n.replay(ctx, t, appended)
	for _, store := range n.stores(true) {
		n.wantTakenBack(ctx, t, store, 0, "after -migrate -yes")
	}
}

// panelsWrittenBy260 is what 2.6.0 wrote for the two families that write
// gh_discussion_comment, against the fake as it stood before the answer was
// taken back: this release's sweep through the file sink, with is_answer put
// back as the tag it was, and each accepted comment written again at the
// same instant as it was read before its acceptance.
func panelsWrittenBy260(ctx context.Context, t *testing.T, binary, dir string, at time.Time) []sink.Point {
	t.Helper()
	gh := newSQLStoresGitHub(t)
	gh.FreezeAt(at)
	dump := filepath.Join(dir, "written-by-2.6.0.lp")
	cfg := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
targets:
  user: %s
sinks:
  file:
    path: %s
every:
  default: 0
  families:
    discussions: 1m
    outbound: 1m
state_file: %s
`, gh.URL(), sqlStoresLogin, dump, filepath.Join(dir, "written-by-2.6.0.json"))
	path := filepath.Join(dir, "written-by-2.6.0.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := sqlStoresExec(ctx, t, binary, path); err != nil {
		t.Fatalf("the sweep 2.6.0's shape is made from: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	var out []sink.Point
	for line := range strings.SplitSeq(string(raw), "\n") {
		p, ok := influxParse(line)
		if !ok {
			continue
		}
		point := sink.Point{Measurement: p.measurement, Tags: p.tags, Fields: p.fields, Time: p.at}
		if p.measurement != "gh_discussion_comment" {
			out = append(out, point)
			continue
		}
		accepted := p.fields["answers"] == int64(1)
		point.Tags = maps.Clone(p.tags)
		point.Tags["is_answer"] = strconv.FormatBool(accepted)
		out = append(out, point)
		if accepted {
			before := point
			before.Tags, before.Fields = maps.Clone(point.Tags), maps.Clone(point.Fields)
			before.Tags["is_answer"], before.Fields["answers"] = "false", int64(0)
			out = append(out, before)
		}
	}
	if !slices.ContainsFunc(out, func(p sink.Point) bool {
		return p.Tags["comment"] == fakegh.TakenBack && p.Tags["is_answer"] == "true"
	}) {
		t.Fatalf("2.6.0's shape holds no accepted row of comment %s", fakegh.TakenBack)
	}
	return out
}

// panelsNS is one namespace in every store, and the Grafana datasources
// that read it.
type panelsNS struct {
	s    *Stack
	name string
	// sql is the SQL file 2.6.0 wrote, and before what it held then.
	sql, before string
	// uids is the datasource each store is read through, by store.
	uids map[string]string
}

// seedPanels writes points into a namespace of every store through the
// sinks, and makes the datasources that read it: here 2.6.0's shape, and the
// contributions snapshots TestTheContributionMixReadsTheNewestSnapshot asks
// the mix about. Everything it makes is removed when the test ends.
func seedPanels(ctx context.Context, t *testing.T, s *Stack, ns string, points []sink.Point, dir string) *panelsNS {
	t.Helper()
	n := &panelsNS{
		s: s, name: ns, sql: filepath.Join(dir, ns+".sql"), uids: map[string]string{},
	}
	leftover := []string{n.sql, filepath.Join(dir, ns+"-state.json"), filepath.Join(dir, ns+"-state-refill.json")}
	for _, f := range leftover {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	for _, db := range []string{ns, ns + "_sql"} {
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + db + " WITH (FORCE)", "CREATE DATABASE " + db} {
			if out, err := s.Psql(ctx, stmt); err != nil {
				t.Fatalf("%s: %v\n%s", stmt, err, out)
			}
		}
	}
	t.Cleanup(func() { n.remove(context.WithoutCancel(ctx), t) })
	for _, w := range []sink.Sink{
		sink.NewInflux(s.InfluxURL, s.InfluxToken, "", ns, 1000, 30*time.Second),
		sink.NewPostgres(n.dsn(), 100),
		sink.NewElasticsearch(s.ElasticsearchURL, ns, "", "", "", 1000, 30*time.Second),
		sink.NewGraphite(s.GraphiteAddr, ns, 1000, 30*time.Second),
		sink.NewSQL("postgres", n.sql, 0, 0),
	} {
		if written, err := w.Write(ctx, points); err != nil || written != len(points) {
			t.Fatalf("the seed through the %s sink into %s: %d of %d, %v", w.Name(), ns, written, len(points), err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(n.sql)
	if err != nil {
		t.Fatal(err)
	}
	n.before = string(body)
	storeCall(ctx, t, http.MethodPost, s.ElasticsearchURL+"/"+ns+"-*/_refresh", "", "")
	n.awaitCarbon(ctx, t, graphiteFilesSent(ctx, t, ns, points))
	n.datasources(ctx, t)
	return n
}

// awaitCarbon waits until carbon has a whisper file for every path it was
// sent. The sink's write returns once the lines are on the socket, and carbon
// creates the files afterwards: measured against the suite's graphite, five
// seeds of 132 paths in a row showed none of them on disk right after the
// write and all of them ten seconds later. What a namespace holds is read as
// a fingerprint and compared later, and one read before carbon caught up
// compared a partial listing with the whole one: "the namespace no
// configuration names changed", with nothing having touched it.
func (n *panelsNS) awaitCarbon(ctx context.Context, t *testing.T, want []string) {
	t.Helper()
	err := WaitUntil(ctx, "carbon writing what "+n.name+" was sent", 2*time.Minute, func(ctx context.Context) error {
		out, _ := n.s.Exec(ctx, "graphite", "sh", "-c",
			"cd /opt/graphite/storage/whisper/"+n.name+" 2>/dev/null && find . -name '*.wsp'")
		have := strings.Fields(out)
		missing := slices.DeleteFunc(slices.Clone(want), func(f string) bool { return slices.Contains(have, f) })
		if len(missing) > 0 {
			return fmt.Errorf("%d of %d files missing, %s among them", len(missing), len(want), missing[0])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// graphiteFilesSent is the whisper file each path the Graphite sink sends for
// these points lands in, relative to the prefix's directory. The sink renders
// them into a listener here, so the list is what it sends and not a second
// account of how it names a path.
func graphiteFilesSent(ctx context.Context, t *testing.T, prefix string, points []sink.Point) []string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			received <- nil
			return
		}
		defer conn.Close()
		body, _ := io.ReadAll(conn)
		received <- body
	}()
	g := sink.NewGraphite(ln.Addr().String(), prefix, 1000, 10*time.Second)
	if _, err = g.Write(ctx, points); err != nil {
		t.Fatal(err)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	var files []string
	for line := range strings.SplitSeq(string(<-received), "\n") {
		path, _, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		file := "./" + strings.ReplaceAll(strings.TrimPrefix(path, prefix+"."), ".", "/") + ".wsp"
		if !slices.Contains(files, file) {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		t.Fatalf("the Graphite sink sent nothing for %d points", len(points))
	}
	return files
}

// dsn is the connecting sink's DSN for the namespace's database.
func (n *panelsNS) dsn() string {
	return strings.Replace(pgSinkDSN(n.s), "/"+n.s.PostgresDatabase+"?", "/"+n.name+"?", 1)
}

// sqlBefore is what the SQL file held before this release ran.
func (n *panelsNS) sqlBefore(t *testing.T) string {
	t.Helper()
	if n.before == "" {
		t.Fatal("the SQL file 2.6.0 wrote is empty")
	}
	return n.before
}

// datasources makes a datasource per store that reads the namespace, since
// the provisioned ones read the suite's own. Graphite's needs none: the
// prefix is in the panel's target, which read rewrites.
func (n *panelsNS) datasources(ctx context.Context, t *testing.T) {
	t.Helper()
	client := grafana.Client{URL: n.s.GrafanaURL, Token: n.s.GrafanaToken}
	pg := func(db string) grafana.Datasource {
		return grafana.Datasource{
			UID: "e2e-" + db, Name: "e2e-" + db, Type: "grafana-postgresql-datasource", URL: "postgres:5432",
			Database: db, User: postgresUser,
			JSON:   map[string]any{"sslmode": "disable", "postgresVersion": 1800, "timescaledb": false},
			Secret: map[string]string{"password": postgresPassword},
		}
	}
	for store, ds := range map[string]grafana.Datasource{
		"influxdb": {
			UID: "e2e-influxdb-" + n.name, Name: "e2e-influxdb-" + n.name, Type: "influxdb", URL: "http://influxdb:8181",
			JSON:   map[string]any{"version": "SQL", "dbName": n.name, "httpMode": "POST", "insecureGrpc": true},
			Secret: map[string]string{"token": influxToken},
		},
		"postgres": pg(n.name),
		"sql":      pg(n.name + "_sql"),
		"elasticsearch": {
			UID: "e2e-elasticsearch-" + n.name, Name: "e2e-elasticsearch-" + n.name, Type: "elasticsearch",
			URL: "http://elasticsearch:9200", Database: n.name + "-*",
			JSON: map[string]any{"index": n.name + "-*", "timeField": "@timestamp", "maxConcurrentShardRequests": 5},
		},
	} {
		if _, err := client.EnsureDatasource(ctx, ds, time.Minute); err != nil {
			t.Fatalf("the %s datasource over %s: %v", store, n.name, err)
		}
		n.uids[store] = ds.UID
	}
	n.uids["graphite"] = DatasourceGraphite
}

// remove takes out everything seedPanels made, and what the migration made
// beside it.
func (n *panelsNS) remove(ctx context.Context, t *testing.T) {
	t.Helper()
	client := grafana.Client{URL: n.s.GrafanaURL, Token: n.s.GrafanaToken}
	for store, uid := range n.uids {
		if store != "graphite" {
			_ = client.Delete(ctx, grafana.DatasourcePath(uid), time.Minute)
		}
	}
	for _, db := range []string{n.name, n.name + "_sql"} {
		_, _ = n.s.Psql(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
	}
	storeCall(ctx, t, http.MethodDelete, n.s.InfluxURL+"/api/v3/configure/database?db="+n.name, "", "")
	for _, index := range refillIndices(ctx, t, n.s, n.name) {
		storeCall(ctx, t, http.MethodDelete, n.s.ElasticsearchURL+"/"+index, "", "")
	}
	_, _ = n.s.Exec(ctx, "graphite", "rm", "-rf", "/opt/graphite/storage/whisper/"+n.name)
}

// config is the configuration this release runs with: every store in the
// namespace, the SQL file 2.6.0 wrote, the two families that write the
// comments, and the state file 2.6.0 left.
func (n *panelsNS) config(t *testing.T, dir, github string) string {
	t.Helper()
	state := filepath.Join(dir, n.name+"-state.json")
	if err := os.WriteFile(state, []byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 30s
targets:
  user: %s
sinks:
  influxdb:
    url: %s
    token: %s
    bucket: %s
  postgres:
    dsn: %s
  elasticsearch:
    url: %s
    prefix: %s
  graphite:
    addr: %s
    prefix: %s
  sql:
    path: %s
every:
  default: 0
  families:
    discussions: 1m
    outbound: 1m
state_file: %s
log:
  level: debug
`, github, sqlStoresLogin, n.s.InfluxURL, n.s.InfluxToken, n.name, n.dsn(), n.s.ElasticsearchURL, n.name,
		n.s.GraphiteAddr, n.name, n.sql, state)
	path := filepath.Join(dir, n.name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// replay pipes SQL through psql into the namespace's second database, the
// way the SQL sink's page says to load its file.
func (n *panelsNS) replay(ctx context.Context, t *testing.T, statements string) {
	t.Helper()
	if out, err := n.s.LoadSQLInto(ctx, n.name+"_sql", strings.NewReader(statements)); err != nil {
		t.Fatalf("replaying the SQL file into %s_sql: %v\n%s", n.name, err, out)
	}
}

// stores is every store whose panels read the namespace, the SQL file's
// replay among them when it is.
func (n *panelsNS) stores(withSQL bool) []string {
	out := []string{"influxdb", "postgres", "elasticsearch", "graphite"}
	if withSQL {
		out = append(out, "sql")
	}
	return out
}

// onlyTheCommentsAside: in each store that sets rows aside, the one copy is
// of gh_discussion_comment, and no other table or index was set aside.
func (n *panelsNS) onlyTheCommentsAside(ctx context.Context, t *testing.T) {
	t.Helper()
	isAside := func(name string) bool { return strings.Contains(name, "-20") }
	asides := func(names []string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(s string) bool { return !isAside(s) })
	}
	pgTables := strings.Fields(n.psql(ctx, t, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1"))
	for store, names := range map[string][]string{
		"influxdb":      asideInfluxTables(ctx, t, n.s, n.name),
		"postgres":      pgTables,
		"elasticsearch": refillIndices(ctx, t, n.s, n.name),
	} {
		got := asides(names)
		if len(got) != 1 || !strings.Contains(got[0], "gh_discussion_comment-") {
			t.Errorf("%s set aside %v, want the comments alone", store, got)
		}
	}
}

// psql is one query against the namespace's database.
func (n *panelsNS) psql(ctx context.Context, t *testing.T, q string) string {
	t.Helper()
	out, err := n.s.Exec(ctx, "postgres", "psql", "--username", postgresUser, "--dbname", n.name,
		"--no-align", "--tuples-only", "--quiet", "--command", q)
	if err != nil {
		t.Fatalf("%s: %v\n%s", q, err, out)
	}
	return out
}

// holds is what the namespace holds in every store, as one string a before
// and an after compare in.
func (n *panelsNS) holds(ctx context.Context, t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(n.sql)
	if err != nil {
		t.Fatal(err)
	}
	return n.holdsBeside(ctx, t, "") + fmt.Sprintf("sql %d bytes", len(body))
}

// holdsBeside is what the namespace holds in every store but the SQL file,
// leaving out every table, index and path of one measurement, and its copies.
func (n *panelsNS) holdsBeside(ctx context.Context, t *testing.T, measurement string) string {
	t.Helper()
	left := func(name string) bool {
		return measurement != "" && (strings.HasPrefix(name, measurement) || strings.Contains(name, "-"+measurement))
	}
	var b strings.Builder
	for _, table := range asideInfluxTables(ctx, t, n.s, n.name) {
		if !left(table) {
			fmt.Fprintf(&b, "influx %s=%d ", table, asideInfluxCount(ctx, t, n.s, n.name, table))
		}
	}
	for table := range strings.FieldsSeq(n.psql(ctx, t, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1")) {
		if !left(table) {
			fmt.Fprintf(&b, "postgres %s=%s ", table, strings.TrimSpace(n.psql(ctx, t, `SELECT count(*) FROM "`+table+`"`)))
		}
	}
	for _, index := range refillIndices(ctx, t, n.s, n.name) {
		if !left(index) {
			fmt.Fprintf(&b, "es %s=%d ", index, asideESCount(ctx, t, n.s, index, ""))
		}
	}
	skip := ""
	if measurement != "" {
		skip = " -not -path './" + strings.TrimPrefix(measurement, "gh_") + "/*'"
	}
	files, _ := n.s.Exec(ctx, "graphite", "sh", "-c",
		"cd /opt/graphite/storage/whisper/"+n.name+" && find . -name '*.wsp'"+skip+" | sort | md5sum")
	fmt.Fprintf(&b, "graphite %s ", strings.TrimSpace(files))
	return b.String()
}

// wantTakenBack asks one store's two comment panels, through Grafana, what
// they draw for the answer taken back, and holds them to one row for that
// comment reading accepted as want says. Graphite is asked until carbon has
// what it was sent, since a whisper file is written after the point arrives.
func (n *panelsNS) wantTakenBack(ctx context.Context, t *testing.T, store string, want float64, when string) {
	t.Helper()
	var last string
	err := WaitUntil(ctx, store+" drawing the answer taken back "+when, 2*time.Minute, func(ctx context.Context) error {
		last = ""
		for title, pic := range n.drawn(ctx, t, store) {
			rows, accepted, comments := takenBackRow(pic)
			switch {
			case rows != 1:
				last += fmt.Sprintf("%s draws %d rows for the comment; ", title, rows)
			case accepted != nil && *accepted != want:
				last += fmt.Sprintf("%s reads it accepted %v; ", title, *accepted)
			case comments != nil && *comments != 1:
				last += fmt.Sprintf("%s counts it %v times; ", title, *comments)
			}
		}
		if last != "" {
			return errors.New(last)
		}
		return nil
	})
	if err != nil {
		t.Errorf("%s, want one row for it reading accepted %v: %v", store, want, err)
	}
}

// drawn is what each comment panel of one store draws over the namespace.
func (n *panelsNS) drawn(ctx context.Context, t *testing.T, store string) map[string]grafana.Picture {
	t.Helper()
	return n.drawnPanels(ctx, t, store, commentPanels, "discussion_comment")
}

// drawnPanels is what each panel of those titles in one store's dashboard
// draws over the namespace, each target turned from the suite's own index
// prefix and the Graphite paths of measurement to the namespace's.
func (n *panelsNS) drawnPanels(ctx context.Context, t *testing.T, store string, titles []string,
	measurement string,
) map[string]grafana.Picture {
	t.Helper()
	dashboard, plugin := store, map[string]string{
		"influxdb": "influxdb", "postgres": "grafana-postgresql-datasource", "sql": "grafana-postgresql-datasource",
		"elasticsearch": "elasticsearch", "graphite": "graphite",
	}[store]
	if store == "sql" {
		dashboard = "postgres"
	}
	doc, err := dashboardDocument(dashboard)
	if err != nil {
		t.Fatal(err)
	}
	var panels []grafana.PanelQuery
	for _, p := range grafana.Panels(doc["panels"]) {
		if !slices.Contains(titles, p.Title) {
			continue
		}
		targets := make([]map[string]any, 0, len(p.Targets))
		for _, target := range p.Targets {
			rewritten := maps.Clone(target)
			for _, key := range []string{"query", "target"} {
				if q, ok := rewritten[key].(string); ok {
					q = strings.ReplaceAll(q, "_index:"+elasticsearchPrefix+"-", "_index:"+n.name+"-")
					rewritten[key] = strings.ReplaceAll(q, "github."+measurement+".", n.name+"."+measurement+".")
				}
			}
			targets = append(targets, rewritten)
		}
		p.Targets = targets
		panels = append(panels, p)
	}
	if len(panels) != len(titles) {
		t.Fatalf("the %s dashboard has %d of the panels %v", dashboard, len(panels), titles)
	}
	client := grafana.Client{URL: n.s.GrafanaURL, Token: n.s.GrafanaToken}
	vars := dashboardVars(doc, dashboardStore{name: dashboard, uid: n.uids[store], plugin: plugin}, nil)
	out := map[string]grafana.Picture{}
	for _, r := range client.CheckPanels(ctx, dashboardRange, "now", panels, vars, grafana.Options{
		Timeout: time.Minute, Workers: 1, IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
	}) {
		if r.Err != "" {
			t.Fatalf("%s's %q: %s", store, r.Panel.Title, r.Err)
		}
		pic, _, drawErr := grafana.Drawing(r.Panel.Source, r.Answer)
		if drawErr != nil {
			t.Fatalf("%s's %q: %v", store, r.Panel.Title, drawErr)
		}
		out[r.Panel.Title] = pic
	}
	return out
}

// takenBackRow is how many rows of a table name the answer taken back, in
// fosrl/pangolin, and what the one that does reads as accepted and as its
// count of comments, where the table has those columns.
func takenBackRow(pic grafana.Picture) (rows int, accepted, comments *float64) {
	if len(pic.Columns) == 0 {
		return 0, nil, nil
	}
	for i := range pic.Columns[0].Values {
		named := false
		for _, col := range pic.Columns {
			if s, ok := col.Values[i].(string); ok && strings.Contains(s, "pangolin") {
				named = true
			}
		}
		if !named {
			continue
		}
		rows++
		for _, col := range pic.Columns {
			name := firstNonEmpty(col.Display, col.Name)
			v, ok := number(col.Values[i])
			switch {
			case !ok:
			case strings.HasPrefix(name, "Accepted"):
				accepted = &v
			case strings.HasPrefix(name, "Comments") || name == "Comment (count)":
				comments = &v
			}
		}
	}
	return rows, accepted, comments
}

// number reads a cell as a number.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// panelsExec runs this release and returns what it printed on each stream.
func panelsExec(ctx context.Context, t *testing.T, binary, cfg string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return collectorExec(ctx, t, binary, append([]string{"-config", cfg}, args...)...)
}
