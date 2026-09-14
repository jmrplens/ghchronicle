package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// archivedGitHub is a GitHub whose listing holds one live repository, one
// archived and one archived fork, and whose GraphQL answers the three
// queries the totals family sends. archivedQueries counts the one this test
// is about: the query for the archive dates of the repositories set aside.
type archivedGitHub struct {
	mu       sync.Mutex
	archived bool // whether o/old is still archived, which a test flips
	dates    atomic.Int32
}

func (g *archivedGitHub) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-ratelimit-limit", "5000")
	w.Header().Set("x-ratelimit-remaining", "4999")
	w.Header().Set("x-ratelimit-resource", "core")
	switch r.URL.Path {
	case "/user/repos":
		g.mu.Lock()
		archived := g.archived
		g.mu.Unlock()
		if r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		fmt.Fprintf(w, `[{"name":"n","full_name":"o/n","owner":{"login":"o"}},`+
			`{"name":"old","full_name":"o/old","archived":%t,"owner":{"login":"o"}},`+
			`{"name":"oldfork","full_name":"o/oldfork","archived":true,"fork":true,"owner":{"login":"o"}}]`, archived)
	case "/graphql":
		var env struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		w.Header().Set("x-ratelimit-resource", "graphql")
		data := map[string]any{}
		switch {
		case strings.Contains(env.Query, "fragment archived on Repository"):
			g.dates.Add(1)
			for i := 0; strings.Contains(env.Query, fmt.Sprintf("r%d:", i)); i++ {
				data[fmt.Sprintf("r%d", i)] = map[string]any{
					"nameWithOwner": "o/old", "url": "https://github.com/o/old",
					"createdAt": "2020-05-14T00:00:00Z", "archivedAt": "2026-08-29T15:22:31Z",
				}
			}
		case strings.Contains(env.Query, "fragment totals on Repository"):
			data["r0"] = map[string]any{
				"nameWithOwner": "o/n", "url": "https://github.com/o/n",
				"createdAt": "2020-01-01T00:00:00Z", "pushedAt": "2026-09-01T00:00:00Z",
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
	}
}

// TestABackfillCollectsTheArchivedRepositoriesToo is decision one of the
// 2026-09-12 review: a backfill walks every family for the archived
// repositories as well, because their history is the account's history and
// it never moves again, while forks stay as the configuration says. A sweep
// keeps them out and sets them aside instead.
func TestABackfillCollectsTheArchivedRepositoriesToo(t *testing.T) {
	gh := &archivedGitHub{archived: true}
	for _, backfill := range []bool{false, true} {
		r := sweepRunner(t, gh.handler)
		r.Backfill = backfill
		r.repos, r.reposAt = nil, time.Time{}
		if err := r.discoverRepos(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		collected, aside := names(r.repos), names(r.archived)
		if backfill {
			if collected != "o/n,o/old" || aside != "" {
				t.Errorf("backfill collects %q and sets aside %q; want o/n,o/old and nothing", collected, aside)
			}
			continue
		}
		if collected != "o/n" || aside != "o/old" {
			t.Errorf("sweep collects %q and sets aside %q; want o/n and o/old", collected, aside)
		}
	}
}

// kept is a sink that keeps every point it receives, so a test can compare
// the row one sweep wrote with the row the next one did.
type kept struct {
	mu     sync.Mutex
	points []sink.Point
}

func (k *kept) Name() string { return "kept" }
func (k *kept) Close() error { return nil }

func (k *kept) Write(_ context.Context, points []sink.Point) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.points = append(k.points, points...)
	return nil
}

// rows is every point of the measurement, in the order they were written.
func (k *kept) rows(measurement string) []sink.Point {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []sink.Point
	for _, p := range k.points {
		if p.Measurement == measurement {
			out = append(out, p)
		}
	}
	return out
}

// measured is how many points of the measurement the sink received.
func (k *kept) measured(measurement string) int { return len(k.rows(measurement)) }

func names(repos []collect.Repo) string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.FullName)
	}
	return strings.Join(out, ",")
}

// TestASweepDatesTheArchiveFromTheListing is the sweep's half of the same
// decision: the listing says which repositories are archived, and the totals
// family dates each of them from one query, so the row exists from the first
// sweep. Every totals sweep asks again and rewrites the same row, one query
// at the family's cadence, because that is the only thing an exporter can
// hold: the Prometheus one drops a series not rewritten within a day, and
// the OTLP state keeps the newest batch per series. Unarchived, the
// repository is collected again and leaves the set.
func TestASweepDatesTheArchiveFromTheListing(t *testing.T) {
	gh := &archivedGitHub{archived: true}
	got := &kept{}
	r := sweepRunner(t, gh.handler)
	r.Sinks = []sink.Sink{got}
	r.Cfg.Every = everyOnly("totals")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.State = LoadState(filepath.Join(t.TempDir(), "state.json"))
	r.repos, r.reposAt = nil, time.Time{}

	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got.measured("gh_repo_archived") != 1 || gh.dates.Load() != 1 {
		t.Fatalf("first sweep: %d archive rows from %d queries, want one of each",
			got.measured("gh_repo_archived"), gh.dates.Load())
	}
	if got.measured("gh_repo_total") != 1 {
		t.Errorf("the lifetime batch wrote %d rows, want the collected repository alone", got.measured("gh_repo_total"))
	}
	first := got.rows("gh_repo_archived")[0]

	// Due again: asked again, and the row is the same row, dated at the
	// archive and not at the sweep, so every store converges on it.
	r.State.LastRun["totals"] = time.Now().Add(-time.Hour)
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gh.dates.Load() != 2 || got.measured("gh_repo_archived") != 2 {
		t.Fatalf("second sweep: %d queries and %d archive rows in all, want two of each",
			gh.dates.Load(), got.measured("gh_repo_archived"))
	}
	if again := got.rows("gh_repo_archived")[1]; !again.Time.Equal(first.Time) || fmt.Sprint(again.Tags) != fmt.Sprint(first.Tags) {
		t.Errorf("the second sweep wrote a different row:\n%v\n%v", first, again)
	}
	if !first.Time.Equal(time.Date(2026, 8, 29, 15, 22, 31, 0, time.UTC)) {
		t.Errorf("the row is dated %s, want the archivedAt GitHub answered", first.Time)
	}

	// Unarchived: the repository is collected again and nothing is asked.
	gh.mu.Lock()
	gh.archived = false
	gh.mu.Unlock()
	r.reposAt = time.Time{}
	r.State.LastRun["totals"] = time.Now().Add(-time.Hour)
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gh.dates.Load() != 2 || names(r.archived) != "" {
		t.Errorf("unarchived, o/old is still set aside (%q) or asked about (%d queries)", names(r.archived), gh.dates.Load())
	}
	if got.measured("gh_repo_total") != 3 {
		t.Errorf("unarchived, o/old is not in the lifetime batch: %d gh_repo_total rows in all, want 3", got.measured("gh_repo_total"))
	}
}
