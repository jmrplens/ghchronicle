package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// golden is the line protocol the REST path wrote for a fixture before the
// same rows were read through GraphQL, recorded once at fdf12d1 and kept
// under testdata/golden. A GraphQL collector that renders the twin fixture
// has to produce these bytes exactly: same measurement, tags, fields, field
// types and timestamps.
func golden(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name+".lp"))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// lines renders the points of the named measurements as sorted line
// protocol, the form the golden files hold.
func lines(points []sink.Point, measurements ...string) []string {
	var out []string
	for _, p := range points {
		if len(measurements) > 0 && !slices.Contains(measurements, p.Measurement) {
			continue
		}
		out = append(out, sink.LineProtocol(p))
	}
	slices.Sort(out)
	return out
}

// checkGolden fails on the first line that differs from the recording.
func checkGolden(t *testing.T, name string, points []sink.Point, measurements ...string) {
	t.Helper()
	got, want := lines(points, measurements...), golden(t, name)
	if len(got) != len(want) {
		t.Errorf("%s: %d lines, the REST recording has %d:\n%s", name, len(got), len(want), strings.Join(got, "\n"))
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s line %d differs from the REST recording:\n got %s\nwant %s", name, i, got[i], want[i])
		}
	}
}

// answerAudience answers every alias the query names with the one fixture
// repository, as answerTotals does for the totals batch.
func answerAudience(t *testing.T, w http.ResponseWriter, query string) {
	t.Helper()
	var fx struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "graphql_audience.json"), &fx); err != nil {
		t.Fatal(err)
	}
	data := map[string]json.RawMessage{}
	for i := 0; strings.Contains(query, fmt.Sprintf("r%d:", i)); i++ {
		data[fmt.Sprintf("r%d", i)] = fx.Data["r0"]
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("answer the audience query: %v", err)
	}
}

func audienceFixture(t *testing.T, queries *[]string) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if queries != nil {
			*queries = append(*queries, query)
		}
		answerAudience(t, w, query)
	})
	return f
}

// The batch replaces the REST walks of two families in ordinary sweeps, so
// the rows it writes have to be the rows they wrote: the golden files are the
// REST path's own output for the same four stars and two forks.
func TestAudienceWritesTheRowsTheRESTWalksWrote(t *testing.T) {
	t.Parallel()
	var queries []string
	f := audienceFixture(t, &queries)

	points, err := Audience{Repos: []Repo{testRepo}, Stars: true, Forks: true}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	checkGolden(t, "stars", points, "gh_star")
	checkGolden(t, "forks", points, "gh_fork")
	if len(queries) != 1 {
		t.Fatalf("%d queries for one repository asking both connections, want 1", len(queries))
	}
	// The order both lists are asked in is the order REST serves them, and
	// the newest hundred is the page that can have changed.
	for _, want := range []string{
		"stargazers(last: 100, orderBy: {field: STARRED_AT, direction: ASC})",
		"forks(last: 100, orderBy: {field: CREATED_AT, direction: ASC})",
		"starredAt node { login }",
	} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("query does not ask for %s:\n%s", want, queries[0])
		}
	}
	if n := len(f.calls("/repos/octocat/hello-world/stargazers")) + len(f.calls("/repos/octocat/hello-world/forks")); n != 0 {
		t.Errorf("the batch made %d REST calls", n)
	}
}

// The two families run on different cadences, so each asks for its own
// connection only: a stars sweep must not pay for, or write, the forks.
func TestAudienceAsksOnlyForTheFamilyThatIsDue(t *testing.T) {
	t.Parallel()
	var queries []string
	f := audienceFixture(t, &queries)

	points, err := Audience{Repos: []Repo{testRepo}, Stars: true}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(queries[0], "forks(") {
		t.Errorf("a stars sweep asked for forks:\n%s", queries[0])
	}
	if got := byMeasurement(points); len(got["gh_fork"]) != 0 || len(got["gh_star"]) != 4 {
		t.Errorf("stars sweep wrote %v", measurements(points))
	}

	queries = nil
	points, err = Audience{Repos: []Repo{testRepo}, Forks: true}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(queries[0], "stargazers(") {
		t.Errorf("a forks sweep asked for stargazers:\n%s", queries[0])
	}
	if got := byMeasurement(points); len(got["gh_star"]) != 0 || len(got["gh_fork"]) != 3 {
		t.Errorf("forks sweep wrote %v", measurements(points))
	}
}

// Ten to a query, not eighteen: one query for all eighteen repositories of
// the account took 7.7 to 8.9 s against the ten the gateway allows.
func TestAudienceBatchesTenRepositoriesToAQuery(t *testing.T) {
	t.Parallel()
	var queries []string
	f := audienceFixture(t, &queries)

	repos := make([]Repo, 23)
	for i := range repos {
		repos[i] = Repo{Owner: "octocat", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("octocat/r%d", i)}
	}
	points, err := Audience{Repos: repos, Stars: true, Forks: true}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 3 {
		t.Errorf("%d queries for 23 repositories, want 3 batches of at most ten", len(queries))
	}
	if got := len(only(t, points, "gh_star")); got != 23*4 {
		t.Errorf("got %d stars for 23 repositories, want %d", got, 23*4)
	}
	// Every repository's rows carry its own name, not the batch's first.
	find(t, points, "gh_fork", map[string]string{"full_name": "octocat/r22", "by": "bob"})
}

func TestAudienceWithNothingToAskWritesNothing(t *testing.T) {
	t.Parallel()
	var queries []string
	f := audienceFixture(t, &queries)
	points, err := Audience{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 || len(queries) != 0 {
		t.Errorf("err=%v points=%d queries=%d, want nothing asked and nothing written", err, len(points), len(queries))
	}
}

// The batch reads the newest hundred forks and a row past them never gets
// its star count or days_since_push refreshed again, so a repository whose
// list holds more than that is reported for the REST walk; one that fits is
// not.
func TestAudienceReportsTheForkListsItCannotFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		total int
		want  []string
	}{{total: 150, want: []string{"octocat/hello-world"}}, {total: 100, want: nil}} {
		f := newFixtureServer(t)
		f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
			body := strings.Replace(string(fixture(t, "graphql_audience.json")), `"totalCount": 3`, fmt.Sprintf(`"totalCount": %d`, tc.total), 1)
			_, _ = w.Write([]byte(body))
		})
		var overflow []string
		a := Audience{Repos: []Repo{testRepo}, Forks: true, Overflow: func(repo Repo) { overflow = append(overflow, repo.FullName) }}
		points, err := a.Collect(ctx(t), f.Client, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(overflow, tc.want) {
			t.Errorf("with %d forks the batch reported %v for the walk, want %v", tc.total, overflow, tc.want)
		}
		// The hundred it did read are written either way.
		if got := len(only(t, points, "gh_fork")); got != 3 {
			t.Errorf("with %d forks the batch wrote %d rows, want the 3 it was served", tc.total, got)
		}
	}
}
