//go:build dockere2e

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// What the review of the migration found in the real stores, each measured
// here against the versions this suite runs: whose rows a store holds, read
// whatever the mapping and whoever the rows name; the copies of an
// Elasticsearch prefix written with capitals; a delete answered with a 502
// that the cluster carried out; and the SQL stream -migrate -yes writes on
// standard output, piped into psql.

// reviewNamespace is the database, schema and prefix these rows go in, and
// reviewKeyword a prefix an index template maps every string of as keyword.
const (
	reviewNamespace = "migrate96review"
	reviewKeyword   = "migrate96reviewkw"
)

// reviewComments are three comments: the account's own on a repository it
// does not own, another's on one this configuration no longer covers, and
// one written by a configuration of organizations alone, which names no user.
func reviewComments() []sink.Point {
	comment := func(id, user, author, full string) sink.Point {
		owner, repo, _ := strings.Cut(full, "/")
		tags := map[string]string{
			"author": author, "comment": id, "full_name": full, "is_reply": "false", "number": "1",
			"own": "false", "owner": owner, "repo": repo,
		}
		if user != "" {
			tags["user"] = user
		}
		return sink.Point{
			Measurement: "gh_discussion_comment", Tags: tags,
			Fields: map[string]any{"answers": 0, "comments": 1}, Time: time.Unix(1700000000, 0).UTC(),
		}
	}
	return []sink.Point{
		comment("1", "octocat", "octocat", "cli/cli"),
		comment("2", "octocat", "hubot", "octocat/excluded"),
		comment("3", "", "hubot", "acme/x"),
	}
}

// reviewQuestions is what the planner asks of the comments' store, for
// octocat's configuration.
var reviewQuestions = []teardown.Distinct{
	{Tag: "user"}, {Tag: "full_name", ExceptTag: "author", Except: "OctoCat"},
}

// TestEveryStoreSaysWhoseRowsItHolds: every store answers the planner's
// questions alike. The row with no user is the empty value, not left out as
// if nobody's; the repositories asked about leave out the rows octocat wrote,
// which outbound reads again wherever they are, compared without case; and an
// Elasticsearch whose template maps strings as keyword, which has no keyword
// sub-field, answers the same as one dynamic mapping made.
//
// Before, the last answered no values at all and no error, measured here
// with the query the planner sent, and the planner read that as an index
// holding nobody else's rows.
func TestEveryStoreSaysWhoseRowsItHolds(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	points := reviewComments()

	influx := sink.NewInflux(s.InfluxURL, s.InfluxToken, "", reviewNamespace, 10, 10*time.Second)
	if _, err := influx.Write(ctx, points); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.InfluxURL+"/api/v3/configure/database?db="+reviewNamespace, "", "")
	})
	if _, err := s.Psql(ctx, "DROP SCHEMA IF EXISTS "+reviewNamespace+" CASCADE; CREATE SCHEMA "+reviewNamespace+";"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.Psql(context.WithoutCancel(ctx), "DROP SCHEMA IF EXISTS "+reviewNamespace+" CASCADE;")
	})
	pgDSN := pgSinkDSN(s) + "&search_path=" + reviewNamespace
	pg := sink.NewPostgres(pgDSN, 10)
	if _, err := pg.Write(ctx, points); err != nil {
		t.Fatal(err)
	}
	_ = pg.Close()
	storeCall(ctx, t, http.MethodPut, s.ElasticsearchURL+"/_index_template/"+reviewKeyword,
		`{"index_patterns":["`+reviewKeyword+`-*"],"template":{"mappings":{"dynamic_templates":[`+
			`{"strings":{"match_mapping_type":"string","mapping":{"type":"keyword"}}}]}}}`, "application/json")
	for _, prefix := range []string{reviewNamespace, reviewKeyword} {
		es := sink.NewElasticsearch(s.ElasticsearchURL, prefix, "", "", "", 10, 10*time.Second)
		if _, err := es.Write(ctx, points); err != nil {
			t.Fatal(err)
		}
		storeCall(ctx, t, http.MethodPost, s.ElasticsearchURL+"/"+prefix+"-gh_discussion_comment/_refresh", "", "")
	}
	t.Cleanup(func() {
		for _, prefix := range []string{reviewNamespace, reviewKeyword} {
			storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.ElasticsearchURL+"/"+prefix+"-gh_discussion_comment", "", "")
		}
		storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.ElasticsearchURL+"/_index_template/"+reviewKeyword, "", "")
	})
	if n := reviewOldQuery(ctx, t, s, reviewKeyword+"-gh_discussion_comment"); n != 0 {
		t.Errorf("the query the planner used to send answered %d buckets over the keyword mapping; the measurement "+
			"this test stands on no longer holds", n)
	}

	stores := map[string]teardown.Inspector{}
	for _, in := range teardown.Inspectors(&config.Config{Sinks: config.Sinks{
		Influx:        &config.InfluxSink{URL: s.InfluxURL, Token: s.InfluxToken, Bucket: reviewNamespace},
		Postgres:      &config.PostgresSink{DSN: pgDSN},
		Elasticsearch: &config.ElasticsearchSink{URL: s.ElasticsearchURL, Prefix: reviewNamespace},
	}}) {
		stores[in.Name()] = in
	}
	for _, in := range teardown.Inspectors(&config.Config{Sinks: config.Sinks{
		Elasticsearch: &config.ElasticsearchSink{URL: s.ElasticsearchURL, Prefix: reviewKeyword},
	}}) {
		stores["elasticsearch, strings mapped as keyword"] = in
	}
	for name, in := range stores {
		spread, err := in.Spread(ctx, "gh_discussion_comment", reviewQuestions)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := spread.Values["user"]; !slices.Equal(got, []string{"", "octocat"}) || len(spread.Unread) > 0 {
			t.Errorf("%s: users %q, unread %v, want the row with none as the empty value", name, got, spread.Unread)
		}
		if got := spread.Values["full_name"]; !slices.Equal(got, []string{"acme/x", "octocat/excluded"}) {
			t.Errorf("%s: the repositories of the rows octocat did not write are %q", name, got)
		}
		if reader, ok := in.(teardown.ItemReader); ok {
			if items, readErr := reader.Items(ctx, "gh_discussion_comment", []string{"comment"}); readErr != nil ||
				!slices.Equal(items, []string{"1", "2", "3"}) {
				t.Errorf("%s: items %q, %v", name, items, readErr)
			}
		}
	}
}

// reviewOldQuery is how many buckets the terms aggregation the planner used
// to send, on the keyword sub-field, answers over an index.
func reviewOldQuery(ctx context.Context, t *testing.T, s *Stack, index string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ElasticsearchURL+"/"+index+"/_search",
		strings.NewReader(`{"size":0,"aggs":{"v0":{"terms":{"field":"user.keyword","size":10000}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := stackClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var answer struct {
		Aggs struct {
			V0 struct {
				Buckets []json.RawMessage `json:"buckets"`
			} `json:"v0"`
		} `json:"aggregations"`
	}
	if err = json.NewDecoder(res.Body).Decode(&answer); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%s: %v", res.Status, err)
	}
	return len(answer.Aggs.V0.Buckets)
}

// TestAPrefixWithCapitalsFindsItsCopies: the sink writes an index name in
// lower case, and a copy a migration set aside under a prefix configured with
// capitals is found by asking the store, listed for -uninstall data, and
// purged. Before, _cat/indices was asked with the prefix as configured and
// matched nothing.
func TestAPrefixWithCapitalsFindsItsCopies(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	const prefix = "Migrate96Caps"
	lower := strings.ToLower(prefix)
	es := sink.NewElasticsearch(s.ElasticsearchURL, prefix, "", "", "", 10, 10*time.Second)
	if _, err := es.Write(ctx, reviewComments()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, index := range reviewIndices(context.WithoutCancel(ctx), t, s, lower) {
			storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.ElasticsearchURL+"/"+index, "", "")
		}
	})
	cfg := &config.Config{Sinks: config.Sinks{Elasticsearch: &config.ElasticsearchSink{URL: s.ElasticsearchURL, Prefix: prefix}}}
	clearer := teardown.Clearers(cfg)[0]
	aside, held, err := clearer.Clear(ctx, "gh_discussion_comment", time.Now())
	if err != nil || !held || !strings.HasPrefix(aside.Name, lower+"-gh_discussion_comment-") {
		t.Fatalf("Clear = %+v, %v, %v", aside, held, err)
	}
	purger := teardown.Purgers(cfg)[0]
	found, err := purger.Asides(ctx, []string{"gh_discussion_comment"})
	if err != nil || len(found) != 1 || found[0].Name != aside.Name {
		t.Fatalf("the copies under %s are %+v, %v, want %s", prefix, found, err, aside.Name)
	}
	stores, _ := teardown.For(cfg)
	listed, err := stores[0].Holds(ctx)
	if err != nil || !slices.Contains(listed, aside.Name) {
		t.Errorf("-uninstall data lists %v, %v, want the copy among them", listed, err)
	}
	if err = purger.Purge(ctx, aside.Name); err != nil {
		t.Fatal(err)
	}
	if left := reviewIndices(ctx, t, s, lower); slices.Contains(left, aside.Name) {
		t.Errorf("the copy is still there after its purge: %v", left)
	}
}

// reviewIndices is every index under a prefix.
func reviewIndices(ctx context.Context, t *testing.T, s *Stack, prefix string) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.ElasticsearchURL+"/_cat/indices/"+prefix+"*?format=json&h=index", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := stackClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var rows []struct {
		Index string `json:"index"`
	}
	_ = json.NewDecoder(res.Body).Decode(&rows)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Index)
	}
	return out
}

// TestADeleteAnsweredWithA502IsADeleteDone: behind a proxy that passes the
// delete of the index to the cluster and answers 502, which the cluster
// carried out, Clear reports the index set aside and keeps the clone, the one
// copy of its rows. Before, it purged the clone as well, and neither was left
// (measured on 9.5.3 by the review, through the same proxy).
func TestADeleteAnsweredWithA502IsADeleteDone(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	const prefix = "migrate96lied"
	index := prefix + "-gh_discussion_comment"
	es := sink.NewElasticsearch(s.ElasticsearchURL, prefix, "", "", "", 10, 10*time.Second)
	if _, err := es.Write(ctx, reviewComments()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range reviewIndices(context.WithoutCancel(ctx), t, s, prefix) {
			storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.ElasticsearchURL+"/"+name, "", "")
		}
	})
	target, err := url.Parse(s.ElasticsearchURL)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	forward.Transport = stackClient.Transport
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/"+index {
			forward.ServeHTTP(httptest.NewRecorder(), r)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	cfg := &config.Config{Sinks: config.Sinks{Elasticsearch: &config.ElasticsearchSink{URL: proxy.URL, Prefix: prefix}}}
	aside, held, err := teardown.Clearers(cfg)[0].Clear(ctx, "gh_discussion_comment", time.Now())
	if err != nil || !held || aside.Name == "" {
		t.Fatalf("a delete carried out behind a 502 reads as %+v, %v, %v", aside, held, err)
	}
	storeCall(ctx, t, http.MethodPost, s.ElasticsearchURL+"/"+aside.Name+"/_refresh", "", "")
	if n := asideESCount(ctx, t, s, aside.Name, ""); n != 3 {
		t.Errorf("the clone holds %d documents, want the 3 the index held", n)
	}
	if n := asideESCount(ctx, t, s, index, ""); n != -1 {
		t.Errorf("the index answers %d documents after a delete the cluster carried out", n)
	}
}

// TestMigrateYesPipesIntoPsql: with the SQL sink on standard output, what
// -migrate -yes writes there, piped into psql the way the sink's page pipes
// -once, drops the table 2.6.0 made and loads the rows read again into one
// keyed the new way. Before, the plan went first on the same stream, psql
// read its first line as the start of a statement, the DROP was lost with
// it, and the target kept the old table.
func TestMigrateYesPipesIntoPsql(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	const schema = "migrate96stdout"
	seedPostgresAsWritten(ctx, t, s, schema)
	dir, err := sqlStoresWorkDir("migrate-stdout")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	gh := newSQLStoresGitHub(t)
	if err = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("github:\n  token: e2e-token\n  base_url: %s\n  timeout: 30s\ntargets:\n  user: %s\n"+
		"sinks:\n  sql:\n    path: \"-\"\n    dialect: postgres\nstate_file: %s\n", gh.URL(), sqlStoresLogin,
		filepath.Join(dir, "state.json"))
	if err = os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := collectorExec(ctx, t, binary, "-config", cfg, "-migrate", "-yes")
	if err != nil {
		t.Fatalf("-migrate -yes: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "what this release would change") || strings.Contains(stdout, "what this release") {
		t.Errorf("the plan is not on standard error alone:\n%s", stderr)
	}
	out, err := s.LoadSQL(ctx, strings.NewReader("SET search_path TO "+schema+";\n"+stdout))
	if err != nil {
		t.Fatalf("psql refused what -migrate -yes wrote: %v\n%s", err, out)
	}
	key := asidePsql(ctx, t, s, `SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid `+
		`AND a.attnum = ANY(i.indkey) WHERE i.indisprimary AND i.indrelid = '`+schema+`.gh_discussion_comment'::regclass`)
	if slices.Contains(key, "is_answer") || !slices.Contains(key, "comment") {
		t.Errorf("after the pipe the table is keyed on %v, want the new key", key)
	}
	rows := asidePsql(ctx, t, s, "SELECT count(*) FROM "+schema+".gh_discussion_comment")
	if len(rows) != 1 || rows[0] == "0" {
		t.Errorf("the table holds %v rows after the pipe, want the comments read again", rows)
	}
}
