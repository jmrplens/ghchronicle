package collect

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// commitsServer serves the first page to a query with no cursor and the
// second to one that carries it, recording every set of variables.
func commitsServer(t *testing.T, f *fixtureServer, record *[]map[string]any) {
	t.Helper()
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		if record != nil {
			*record = append(*record, vars)
		}
		if !strings.Contains(query, "history(first: $first, since: $since, after: $after)") {
			t.Errorf("query does not page the history by cursor:\n%s", query)
		}
		if vars["after"] != nil {
			f.write(w, "graphql_commits_page2.json")
			return
		}
		f.write(w, "graphql_commits_page1.json")
	})
}

func TestCommitsPaginateByCursor(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	commitsServer(t, f, &calls)

	since := testNow.AddDate(0, 0, -30)
	points, err := Commits{Since: since, Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	// Two pages: the second carries the first's end cursor, and the walk
	// stops there because hasNextPage is false, page budget or not.
	if len(calls) != 2 {
		t.Fatalf("made %d queries, want 2", len(calls))
	}
	checkCommitQueries(t, calls, since)

	commits := only(t, points, "gh_commit")
	if len(commits) != 3 {
		t.Fatalf("got %d commits, want 3 across both pages", len(commits))
	}
	checkSignedCommit(t, points)
	// No signature at all is a different fact from one that failed to verify.
	unsigned := find(t, points, "gh_commit", map[string]string{"sha": "b2c3d4e5f607"})
	if unsigned.Tags["signature"] != "unsigned" || unsigned.Fields["signed"] != false {
		t.Errorf("null signature: tags %v fields %v", unsigned.Tags, unsigned.Fields)
	}
	// An author without a GitHub account keeps the name from the commit.
	if unsigned.Tags["author"] != "Alice Example" {
		t.Errorf("author without user = %q", unsigned.Tags["author"])
	}
	if hasField(unsigned, "pull_request") {
		t.Error("a commit with no associated pull request must not carry the field")
	}
	invalid := find(t, points, "gh_commit", map[string]string{"sha": "c3d4e5f60718"})
	if invalid.Tags["signature"] != "UNKNOWN_KEY" || invalid.Fields["signed"] != false {
		t.Errorf("unverified signature: tags %v fields %v", invalid.Tags, invalid.Fields)
	}
}

// checkCommitQueries reads the variables of the two page queries: the first
// bounded by since, the second resuming at the first's end cursor.
func checkCommitQueries(t *testing.T, calls []map[string]any, since time.Time) {
	t.Helper()
	// Fifty rather than a hundred: each commit brings up to a hundred check
	// contexts with it, and a hundred commits of a busy repository sits on the
	// gateway's ten second ceiling.
	if calls[0]["after"] != nil || calls[0]["first"] != float64(50) || calls[0]["owner"] != "octocat" || calls[0]["name"] != "hello-world" {
		t.Errorf("first query variables = %v", calls[0])
	}
	if calls[0]["since"] != since.UTC().Format(time.RFC3339) {
		t.Errorf("since = %v, want %s", calls[0]["since"], since.UTC().Format(time.RFC3339))
	}
	if calls[1]["after"] != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 1" {
		t.Errorf("second query after = %v, want the first page's endCursor", calls[1]["after"])
	}
}

// checkSignedCommit reads the commit with a valid signature, dated when it
// was committed.
func checkSignedCommit(t *testing.T, points []sink.Point) {
	t.Helper()
	signed := find(t, points, "gh_commit", map[string]string{"sha": "a1b2c3d4e5f6"})
	if signed.Tags["author"] != "octocat" || signed.Tags["branch"] != "main" || signed.Tags["signature"] != "VALID" {
		t.Errorf("signed commit tags = %v", signed.Tags)
	}
	if signed.Fields["signed"] != true || fieldInt(t, signed, "churn") != 45 || fieldInt(t, signed, "pull_request") != 42 || signed.Fields["headline"] != "feat: add thing" {
		t.Errorf("signed commit fields = %v", signed.Fields)
	}
	if want := time.Date(2026, 9, 7, 9, 14, 53, 0, time.UTC); !signed.Time.Equal(want) {
		t.Errorf("commit stamped %s, want committedDate %s", signed.Time, want)
	}
}

func TestCommitsOnePageByDefault(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	commitsServer(t, f, &calls)
	points, err := Commits{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Errorf("made %d queries, want 1: an increment does not walk", len(calls))
	}
	if _, set := calls[0]["since"]; set {
		t.Error("a zero Since must not be sent")
	}
	if got := len(only(t, points, "gh_commit")); got != 2 {
		t.Errorf("got %d commits from the first page", got)
	}
}

func TestCommitsPageIsCappedForTheChecksItCarries(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	commitsServer(t, f, &calls)
	if _, err := (Commits{First: 500}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	// GitHub caps a page at a hundred; the check contexts each commit brings
	// cap it lower. Fifty commits of a busy repository measured 5,050 nodes
	// and 3.8 seconds against a gateway that gives up at ten.
	if calls[0]["first"] != float64(50) {
		t.Errorf("first = %v, want 50", calls[0]["first"])
	}
}

func TestCommitsEmptyRepositoryIsNotAnError(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		// An empty repository has no default branch; GitHub reports it as an
		// error on the field.
		_, _ = w.Write([]byte(`{"data":{"repository":{"defaultBranchRef":null}},"errors":[{"type":"NOT_FOUND","message":"no default branch"}]}`))
	})
	points, err := Commits{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("err=%v points=%d", err, len(points))
	}
}

func TestCommitsCarryTheGateStateAndTheChecksThatAreNotActions(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	commitsServer(t, f, nil)

	points, err := Commits{Walk: Walk{Pages: 1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	// The rollup is what says whether the commit itself came out red. A
	// workflow run that failed says a job failed, which is not the same claim.
	failed := find(t, points, "gh_commit", map[string]string{"sha": "a1b2c3d4e5f6"})
	if failed.Fields["gate"] != "FAILURE" || fieldInt(t, failed, "checks_total") != 3 || fieldInt(t, failed, "checks_failed") != 1 {
		t.Errorf("commit with a failing gate = %v %v", failed.Tags, failed.Fields)
	}
	// The gate finishes after the commit's own date, so as a tag it opened a
	// second series at the same instant when PENDING became FAILURE.
	if _, isTag := failed.Tags["checks"]; isTag {
		t.Error("the gate state must be a field, not a tag")
	}
	if hasField(failed, "checks") {
		t.Error("the demoted gate must not reuse the old tag's column name")
	}
	// A commit nothing ran on is a different fact from one that passed.
	none := find(t, points, "gh_commit", map[string]string{"sha": "b2c3d4e5f607"})
	if none.Fields["gate"] != "none" || hasField(none, "checks_failed") {
		t.Errorf("commit with no checks = %v %v", none.Tags, none.Fields)
	}

	// Actions is collected in far more detail by its own family, so only the
	// checks that are not Actions get a row.
	checks := only(t, points, "gh_commit_check")
	if len(checks) != 2 {
		t.Fatalf("got %d check rows, want the two that are not Actions: %v", len(checks), checks)
	}
	sonar := find(t, points, "gh_commit_check", map[string]string{"app": "sonarqubecloud"})
	if sonar.Tags["check"] != "SonarQube Code Analysis" || sonar.Tags["conclusion"] != "FAILURE" || fieldInt(t, sonar, "failed") != 1 {
		t.Errorf("sonar check = %v %v", sonar.Tags, sonar.Fields)
	}
	if want := time.Date(2026, 9, 7, 9, 25, 0, 0, time.UTC); !sonar.Time.Equal(want) {
		t.Errorf("check stamped %s, want completedAt %s", sonar.Time, want)
	}
	// A commit status, the older mechanism, reports its state where a check
	// run reports a conclusion.
	status := find(t, points, "gh_commit_check", map[string]string{"app": "status"})
	if status.Tags["check"] != "continuous-integration/jenkins" || status.Tags["conclusion"] != "SUCCESS" {
		t.Errorf("commit status = %v", status.Tags)
	}
}

func TestCommitsRateLimitIsReported(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		// A spent budget arrives as an ordinary errors array, exactly like a
		// switched-off feature. Reporting it as no commits would let the rest
		// of the sweep run against a budget that is already gone.
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	})
	_, err := Commits{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMITED") {
		t.Fatalf("err = %v, want the rate limit reported", err)
	}
}
