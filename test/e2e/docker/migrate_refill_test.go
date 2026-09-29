//go:build dockere2e

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/test/e2e/racereport"
)

// What -migrate -yes does to the real stores from start to end: the old
// comments set aside in each, their history read back from the fake GitHub
// by the two families that write them, written into the three stores that
// were cleared and nowhere else, and each copy compared with what came back,
// with the one comment GitHub does not serve named. The same measurement in
// a second namespace, the other measurement beside it, and every other
// family's measurements, which the refill must not write, say whether it
// touched only what it had to.

// Two namespaces: the one the configuration writes, and one nothing may
// touch.
const (
	refillNamespace = "migrate96refill"
	refillOther     = "migrate96refillother"
)

func TestMigrateYesReadsTheHistoryBackIntoTheStoresItCleared(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	for _, ns := range []string{refillNamespace, refillOther} {
		seedInflux(ctx, t, s, ns)
		seedPostgresAsWritten(ctx, t, s, ns)
		seedElasticsearch(ctx, t, s, ns)
	}
	t.Cleanup(func() {
		// The clone of the index, which the seed's own clean-up does not
		// know; the index read back is under the name the seed deletes.
		for _, index := range refillIndices(context.WithoutCancel(ctx), t, s, refillNamespace) {
			if strings.HasPrefix(index, refillNamespace+"-gh_discussion_comment-") {
				storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.ElasticsearchURL+"/"+index, "", "")
			}
		}
	})
	before := refillOtherNamespace(ctx, t, s)

	dir, err := sqlStoresWorkDir("migrate-refill")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	gh := newSQLStoresGitHub(t)
	cfg := refillConfig(t, s, dir, gh.URL())
	started := time.Now()
	stdout, stderr, err := refillExec(ctx, t, binary, cfg)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("-migrate -yes: %v\n%s\n%s", err, stdout, stderr)
	}
	t.Logf("-migrate -yes took %s:\n%s", took.Round(time.Millisecond), stdout)

	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.Contains(line, "msg=written run=refill") && !strings.Contains(line, "family=outbound") &&
			!strings.Contains(line, "family=discussions") {
			t.Errorf("the refill wrote for a family it was not asked for: %s", line)
		}
	}
	// What came back, as the fake serves it: every comment either family
	// writes, and not the seed's comment 1, which it does not.
	served := refillInfluxComments(ctx, t, s)
	if len(served) == 0 || slices.Contains(served, "1") {
		t.Fatalf("InfluxDB holds the comments %v after the refill, want the fake's and not the seed's", served)
	}
	refillReported(t, stdout, len(served))
	t.Run("influxdb", func(t *testing.T) { refillInflux(ctx, t, s) })
	t.Run("postgres", func(t *testing.T) { refillPostgres(ctx, t, s, served) })
	t.Run("elasticsearch", func(t *testing.T) { refillElasticsearch(ctx, t, s, len(served)) })
	t.Run("nothing else", func(t *testing.T) {
		if after := refillOtherNamespace(ctx, t, s); after != before {
			t.Errorf("the namespace nobody configured changed:\nbefore %s\nafter  %s", before, after)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "points.jsonl")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the file sink was written to by a refill: %v", statErr)
		}
		state, readErr := os.ReadFile(filepath.Join(dir, "state.json"))
		if readErr != nil || strings.Contains(string(state), `"refill"`) {
			t.Errorf("the state file owes a refill that ended: %v\n%s", readErr, state)
		}
	})
}

// refillReported holds the report to one walk into the three stores, and to
// each copy compared with what came back: one comment in it, the seed's,
// which GitHub does not serve.
func refillReported(t *testing.T, stdout string, served int) {
	t.Helper()
	if want := "  refill      read discussions and outbound again, since 2023-11-14, writing gh_discussion_comment to " +
		"elasticsearch, influxdb and postgres\n"; !strings.Contains(stdout, want) {
		t.Errorf("-migrate -yes does not say %q", want)
	}
	for _, store := range []string{"influxdb", "postgres", "elasticsearch"} {
		want := fmt.Sprintf("  reconciled  gh_discussion_comment in %s: 1 item in ", store)
		tail := fmt.Sprintf(", %d now; 1 GitHub no longer serves, whose rows are only in the copy until it is purged: 1\n",
			served)
		i := strings.Index(stdout, want)
		if i < 0 || !strings.HasPrefix(stdout[strings.Index(stdout[i:], ",")+i:], tail) {
			t.Errorf("%s is not reconciled as one comment in the copy, %d now, and comment 1 gone", store, served)
		}
	}
}

// refillInflux: the alerts, the comments read back and the copy, no other
// measurement, and no is_answer in the table read back.
func refillInflux(ctx context.Context, t *testing.T, s *Stack) {
	t.Helper()
	tables := asideInfluxTables(ctx, t, s, refillNamespace)
	if len(tables) != 3 || tables[0] != "gh_dependabot_alert_item" || tables[1] != "gh_discussion_comment" ||
		!strings.HasPrefix(tables[2], "gh_discussion_comment-") {
		t.Errorf("tables %v, want the alerts, the comments read back and the copy, and no other measurement", tables)
	}
	var cols []struct {
		Name string `json:"column_name"`
		Type string `json:"data_type"`
	}
	asideInfluxSQL(ctx, t, s, refillNamespace, "SELECT column_name, data_type FROM information_schema.columns "+
		"WHERE table_schema = 'iox' AND table_name = 'gh_discussion_comment'", &cols)
	for _, c := range cols {
		if c.Name == "is_answer" {
			t.Errorf("the table read back has is_answer as %s", c.Type)
		}
	}
}

// refillPostgres: the same tables, the same comments as InfluxDB, and a key
// without is_answer.
func refillPostgres(ctx context.Context, t *testing.T, s *Stack, served []string) {
	t.Helper()
	tables := asidePsql(ctx, t, s, "SELECT table_name FROM information_schema.tables WHERE table_schema = '"+
		refillNamespace+"' ORDER BY 1")
	if len(tables) != 3 || tables[0] != "gh_dependabot_alert_item" || tables[1] != "gh_discussion_comment" ||
		!strings.HasPrefix(tables[2], "gh_discussion_comment-") {
		t.Errorf("tables %v, want the alerts, the comments read back and the copy, and no other measurement", tables)
	}
	comments := asidePsql(ctx, t, s, "SELECT DISTINCT comment FROM "+refillNamespace+".gh_discussion_comment ORDER BY 1")
	if !slices.Equal(comments, served) {
		t.Errorf("PostgreSQL holds %v, InfluxDB %v", comments, served)
	}
	cols := asidePsql(ctx, t, s, "SELECT column_name FROM information_schema.columns WHERE table_schema = '"+
		refillNamespace+"' AND table_name = 'gh_discussion_comment'")
	if slices.Contains(cols, "is_answer") {
		t.Errorf("the table read back keys on is_answer: %v", cols)
	}
}

// refillElasticsearch: the same indices, as many documents as comments came
// back, and none carrying is_answer.
func refillElasticsearch(ctx context.Context, t *testing.T, s *Stack, served int) {
	t.Helper()
	indices := refillIndices(ctx, t, s, refillNamespace)
	if len(indices) != 3 || indices[0] != refillNamespace+"-gh_dependabot_alert_item" ||
		indices[1] != refillNamespace+"-gh_discussion_comment" ||
		!strings.HasPrefix(indices[2], refillNamespace+"-gh_discussion_comment-") {
		t.Errorf("indices %v, want the alerts, the comments read back and the clone, and no other measurement", indices)
	}
	storeCall(ctx, t, http.MethodPost, s.ElasticsearchURL+"/"+refillNamespace+"-gh_discussion_comment/_refresh", "", "")
	if n := asideESCount(ctx, t, s, refillNamespace+"-gh_discussion_comment", ""); n != served {
		t.Errorf("the index read back holds %d documents, want %d", n, served)
	}
	if n := asideESCount(ctx, t, s, refillNamespace+"-gh_discussion_comment", "_exists_:is_answer"); n != 0 {
		t.Errorf("%d documents read back carry is_answer", n)
	}
}

// refillConfig is the configuration -migrate -yes runs with: the three
// stores in the namespace, a file sink a refill must leave alone, and a state
// file 2.6.0 left.
func refillConfig(t *testing.T, s *Stack, dir, github string) string {
	t.Helper()
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(state, []byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var every strings.Builder
	for _, f := range sqlStoresFamilies {
		fmt.Fprintf(&every, "    %s: 1m\n", f)
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
  file:
    path: %s
    format: json
every:
  families:
%sstate_file: %s
log:
  level: debug
`, github, sqlStoresLogin, s.InfluxURL, s.InfluxToken, refillNamespace,
		pgSinkDSN(s)+"&search_path="+refillNamespace, s.ElasticsearchURL, refillNamespace,
		filepath.Join(dir, "points.jsonl"), every.String(), state)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// refillExec runs -migrate -yes and returns what it printed on each stream.
func refillExec(ctx context.Context, t *testing.T, binary, cfg string) (stdout, stderr string, err error) {
	t.Helper()
	return collectorExec(ctx, t, binary, "-config", cfg, "-migrate", "-yes")
}

// collectorExec runs the collector with the arguments given and returns what
// it printed on each stream, failing the test on a race report in the log.
func collectorExec(ctx context.Context, t *testing.T, binary string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = collectorEnviron()
	var out, log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &log
	err = cmd.Run()
	racereport.Check(t, log.String())
	return out.String(), log.String(), err
}

// refillInfluxComments is the distinct comments the live table holds.
func refillInfluxComments(ctx context.Context, t *testing.T, s *Stack) []string {
	t.Helper()
	var rows []struct {
		Comment string `json:"comment"`
	}
	asideInfluxSQL(ctx, t, s, refillNamespace,
		`SELECT DISTINCT comment FROM gh_discussion_comment ORDER BY comment`, &rows)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Comment)
	}
	return out
}

// refillIndices is every index under a prefix, sorted.
func refillIndices(ctx context.Context, t *testing.T, s *Stack, ns string) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.ElasticsearchURL+"/_cat/indices/"+ns+"-*?format=json&h=index", http.NoBody)
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
	if err = json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Index)
	}
	slices.Sort(out)
	return out
}

// refillOtherNamespace is what the namespace nobody configured holds in each
// store, as one string a before and an after compare in.
func refillOtherNamespace(ctx context.Context, t *testing.T, s *Stack) string {
	t.Helper()
	var b strings.Builder
	for _, table := range asideInfluxTables(ctx, t, s, refillOther) {
		fmt.Fprintf(&b, "influx %s=%d ", table, asideInfluxCount(ctx, t, s, refillOther, table))
	}
	for _, table := range asidePsql(ctx, t, s, "SELECT table_name FROM information_schema.tables WHERE table_schema = '"+
		refillOther+"' ORDER BY 1") {
		n := asidePsql(ctx, t, s, `SELECT count(*) FROM `+refillOther+`."`+table+`"`)
		fmt.Fprintf(&b, "postgres %s=%v ", table, n)
	}
	for _, index := range refillIndices(ctx, t, s, refillOther) {
		fmt.Fprintf(&b, "es %s=%d ", index, asideESCount(ctx, t, s, index, ""))
	}
	return b.String()
}
