//go:build dockere2e

package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// What the planner reads out of the real stores a 2.6.0 wrote.
//
// Each store is given gh_discussion_comment the way 2.6.0 wrote it, is_answer
// a tag and one comment read before and after its acceptance, beside a
// gh_dependabot_alert_item in today's shape, in a namespace of its own: a
// database, a schema and an index prefix nothing else in the suite reads, so
// the dashboards the other tests draw never see these rows. The plan has to
// find the old shape in all three, find none in the alerts, and leave every
// row where it was.

// migrateNamespace is the database, schema and prefix the rows go in.
const migrateNamespace = "migrate96"

// oldComments is 2.6.0's shape as line protocol: one comment twice at one
// instant, once per value of is_answer, and one comment of another account.
const oldComments = `gh_discussion_comment,author=a,comment=1,full_name=o/r,is_answer=false,is_reply=false,number=1,own=true,owner=o,repo=r,user=octocat answers=0i,comments=1i 1700000000
gh_discussion_comment,author=a,comment=1,full_name=o/r,is_answer=true,is_reply=false,number=1,own=true,owner=o,repo=r,user=octocat answers=1i,comments=1i 1700000000
gh_dependabot_alert_item,full_name=o/r,number=1,owner=o,repo=r,severity=high alerts=1i,alert_state="open" 1705000000
`

func TestThePlanFindsTheOldShapeInEveryStoreAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := Start(t)
	seedInflux(ctx, t, s, migrateNamespace)
	seedPostgres(ctx, t, s, migrateNamespace)
	seedElasticsearch(ctx, t, s, migrateNamespace)

	dir := t.TempDir()
	cfg := &config.Config{
		GitHub:  config.GitHub{Token: "t"},
		Targets: config.Targets{User: "octocat"},
		Sinks: config.Sinks{
			Influx:        &config.InfluxSink{URL: s.InfluxURL, Token: s.InfluxToken, Bucket: migrateNamespace},
			Postgres:      &config.PostgresSink{DSN: pgSinkDSN(s) + "&search_path=" + migrateNamespace},
			Elasticsearch: &config.ElasticsearchSink{URL: s.ElasticsearchURL, Prefix: migrateNamespace},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	before := counts(ctx, t, s)
	plan := migrate.Make(ctx, migrate.Input{
		Config: cfg, State: run.LoadState(filepath.Join(dir, "state.json")), Release: "2.6.2",
		Now: time.Now(), Repos: []string{"o/r"}, ReposKnown: true,
	})
	var out bytes.Buffer
	plan.Print(&out)
	t.Logf("the plan:\n%s", out.String())
	for _, st := range plan.Stores {
		if st.Err != nil {
			t.Fatalf("%s did not answer: %v", st.Name, st.Err)
		}
		for _, it := range st.Items {
			want := migrate.NotNeeded
			if it.Migration.Measurement == "gh_discussion_comment" {
				want = migrate.Pending
			}
			if it.Status != want {
				t.Errorf("%s in %s is %s (%s), want %s", it.Migration.ID, st.Name, it.Status, it.Evidence, want)
			}
			if want == migrate.Pending && (!it.Safe || it.Since.Format(time.DateOnly) != "2023-11-14") {
				t.Errorf("%s in %s: safe %v %v, since %s", it.Migration.ID, st.Name, it.Safe, it.Unsafe, it.Since)
			}
		}
	}
	if after := counts(ctx, t, s); after != before {
		t.Errorf("the dry run changed the stores: %s before, %s after", before, after)
	}
}

// counts is how many rows of the old shape each store holds, one string so a
// before and an after compare in one go.
func counts(ctx context.Context, t *testing.T, s *Stack) string {
	t.Helper()
	cfg := &config.Config{Sinks: config.Sinks{
		Influx:        &config.InfluxSink{URL: s.InfluxURL, Token: s.InfluxToken, Bucket: migrateNamespace},
		Postgres:      &config.PostgresSink{DSN: pgSinkDSN(s) + "&search_path=" + migrateNamespace},
		Elasticsearch: &config.ElasticsearchSink{URL: s.ElasticsearchURL, Prefix: migrateNamespace},
	}}
	var b strings.Builder
	for _, in := range teardown.Inspectors(cfg) {
		shape, err := in.Shape(ctx, "gh_discussion_comment", []string{"is_answer"})
		if err != nil {
			t.Fatalf("asking %s: %v", in.Name(), err)
		}
		fmt.Fprintf(&b, "%s=%v:%d ", in.Name(), shape.Old, shape.Rows)
	}
	return b.String()
}

// seedInflux writes the old shape into a database of its own, and drops that
// database when the test ends.
func seedInflux(ctx context.Context, t *testing.T, s *Stack, ns string) {
	t.Helper()
	write := s.InfluxURL + "/api/v2/write?" + url.Values{
		"bucket": {ns}, "org": {"x"}, "precision": {"s"},
	}.Encode()
	storeCall(ctx, t, http.MethodPost, write, oldComments, "")
	t.Cleanup(func() {
		storeCall(context.WithoutCancel(ctx), t, http.MethodDelete,
			s.InfluxURL+"/api/v3/configure/database?db="+ns, "", "")
	})
}

// seedPostgres makes the table 2.6.0's sink made, is_answer in its key, in a
// schema of its own.
func seedPostgres(ctx context.Context, t *testing.T, s *Stack, ns string) {
	t.Helper()
	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS " + ns + " CASCADE;",
		"CREATE SCHEMA " + ns + ";",
		`CREATE TABLE ` + ns + `.gh_discussion_comment ("time" TIMESTAMPTZ NOT NULL, ` +
			`"author" TEXT NOT NULL DEFAULT '', "comment" TEXT NOT NULL DEFAULT '', "full_name" TEXT NOT NULL DEFAULT '', ` +
			`"is_answer" TEXT NOT NULL DEFAULT '', "user" TEXT NOT NULL DEFAULT '', "answers" BIGINT, ` +
			`PRIMARY KEY ("time", "author", "comment", "full_name", "is_answer", "user"));`,
		`INSERT INTO ` + ns + `.gh_discussion_comment VALUES ` +
			`('2023-11-14T22:13:20Z', 'a', '1', 'o/r', 'false', 'octocat', 0), ` +
			`('2023-11-14T22:13:20Z', 'a', '1', 'o/r', 'true', 'octocat', 1);`,
		`CREATE TABLE ` + ns + `.gh_dependabot_alert_item ("time" TIMESTAMPTZ NOT NULL, ` +
			`"number" TEXT NOT NULL DEFAULT '', "owner" TEXT NOT NULL DEFAULT '', "alert_state" TEXT, PRIMARY KEY ("time", "number", "owner"));`,
		`INSERT INTO ` + ns + `.gh_dependabot_alert_item VALUES ('2024-01-11T19:06:40Z', '1', 'o', 'open');`,
	} {
		if _, err := s.Psql(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.Psql(context.WithoutCancel(ctx), "DROP SCHEMA IF EXISTS "+ns+" CASCADE;")
	})
}

// seedElasticsearch indexes the old shape under a prefix of its own.
func seedElasticsearch(ctx context.Context, t *testing.T, s *Stack, ns string) {
	t.Helper()
	index := ns + "-gh_discussion_comment"
	bulk := `{"index":{"_index":"` + index + `","_id":"a1"}}
{"@timestamp":"2023-11-14T22:13:20Z","author":"a","comment":"1","full_name":"o/r","is_answer":"false","user":"octocat","answers":0}
{"index":{"_index":"` + index + `","_id":"a2"}}
{"@timestamp":"2023-11-14T22:13:20Z","author":"a","comment":"1","full_name":"o/r","is_answer":"true","user":"octocat","answers":1}
{"index":{"_index":"` + ns + `-gh_dependabot_alert_item","_id":"d1"}}
{"@timestamp":"2024-01-11T19:06:40Z","number":"1","owner":"o","alert_state":"open"}
`
	storeCall(ctx, t, http.MethodPost, s.ElasticsearchURL+"/_bulk?refresh=true", bulk, "application/x-ndjson")
	t.Cleanup(func() {
		for _, m := range []string{"gh_discussion_comment", "gh_dependabot_alert_item"} {
			storeCall(context.WithoutCancel(ctx), t, http.MethodDelete, s.ElasticsearchURL+"/"+ns+"-"+m, "", "")
		}
	})
}

// storeCall sends one request to a store and fails the test on anything but
// a success.
func storeCall(ctx context.Context, t *testing.T, method, endpoint, body, contentType string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := stackClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(res.Body)
	if res.StatusCode > 299 {
		t.Fatalf("%s %s: %s %s", method, endpoint, res.Status, answer)
	}
}
