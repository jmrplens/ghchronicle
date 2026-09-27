package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// TestAChangedAchievementsPageIsSaidOnce pins what the family promises when
// GitHub redesigns the profile page: one warning, no points, no error, and
// the family still marked as run so it is asked again at its own cadence
// rather than on every tick.
func TestAChangedAchievementsPageIsSaidOnce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	served := 0
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/o" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		served++
		mu.Unlock()
		if auth := req.Header.Get("Authorization"); auth != "" {
			t.Errorf("the API token traveled to the profile page: %q", auth)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Achievements, redesigned</h1></body></html>"))
	})
	r.Cfg.Every = everyOnly("achievements")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&log, nil))

	start := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	r.prime = true
	r.accountFamilies(context.Background(), start)
	r.prime = true
	r.accountFamilies(context.Background(), start.Add(time.Minute))

	mu.Lock()
	n := served
	mu.Unlock()
	if n != 2 {
		t.Errorf("the page was asked %d times over two sweeps, want both", n)
	}
	if got := strings.Count(log.String(), "achievements page changed"); got != 1 {
		t.Errorf("the redesign was reported %d times, want once:\n%s", got, log.String())
	}
	if strings.Contains(log.String(), "collector failed") {
		t.Errorf("a changed page is a warning, not a failed collector:\n%s", log.String())
	}
	if _, marked := r.State.LastRun["achievements"]; !marked {
		t.Error("the family was not marked as run, so it would be asked on every tick")
	}
}

// TestAnAchievementDisagreementIsSaidOnce pins the other promise of the
// family: a badge whose count implies a tier the page does not show is one
// warning per process, not one per day, since the rule and the page stay
// apart for as long as either holds. The page is the e2e fixture, which
// shows no Pull Shark, against counts that say 1,696 merged pull requests.
func TestAnAchievementDisagreementIsSaidOnce(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join("..", "..", "test", "e2e", "testdata", "achievements_page.html"))
	if err != nil {
		t.Fatal(err)
	}
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/octocat":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(page)
		case "/graphql":
			body, _ := io.ReadAll(req.Body)
			w.Header().Set("Content-Type", "application/json")
			if bytes.Contains(body, []byte("query achievementCounts(")) {
				_, _ = w.Write([]byte(`{"data":{"pulls":{"issueCount":1696},"answers":{"discussionCount":0},"user":{"createdAt":"2024-03-05T08:00:00Z","repositories":{"nodes":[{"stargazerCount":4321}]}}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"search":{"issueCount":0,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	r.Cfg.Targets.User = "octocat"
	r.Cfg.Every = everyOnly("achievements")
	if err = r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&log, nil))

	start := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		r.prime = true
		r.accountFamilies(context.Background(), start.Add(time.Duration(i)*time.Minute))
	}
	// Pair Extraordinaire disagrees too, the page's gold against a walk of
	// nothing, and it is its own line: the dedupe is per warning, not per
	// family. Two badges, two lines, three sweeps.
	if got := strings.Count(log.String(), "disagrees with the profile page"); got != 2 {
		t.Errorf("the disagreements were reported %d times over three sweeps, want once each:\n%s", got, log.String())
	}
	for _, want := range []string{
		`badge="pull-shark: count 1696 implies tier 4, page shows 0"`,
		`badge="pair-extraordinaire: count 0 implies tier 0, page shows 4"`,
	} {
		if strings.Count(log.String(), want) != 1 {
			t.Errorf("want %s once:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "collector failed") {
		t.Errorf("a disagreement is a warning, not a failed collector:\n%s", log.String())
	}
}

// coauthoredGitHub answers the achievements family for an account created on
// 2026-09-01: the recorded e2e page, the counts, and the walk from a table of
// merged: ranges, each answered with its pull requests and when they merged.
// It records the ranges the walk asked for, in order.
type coauthoredGitHub struct {
	t      *testing.T
	page   []byte
	ranges map[string][]coauthoredPull
	mu     sync.Mutex
	asked  []string
}

// coauthoredPull is one merged pull request of the fake: when, and whether a
// commit of it carries the trailer.
type coauthoredPull struct {
	merged     string
	coauthored bool
}

var mergedRange = regexp.MustCompile(`merged:(\S+)`)

func (g *coauthoredGitHub) handler(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/octocat":
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(g.page)
	case "/graphql":
		body, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte("query achievementCounts(")) {
			_, _ = w.Write([]byte(`{"data":{"pulls":{"issueCount":1},"answers":{"discussionCount":0},"user":{"createdAt":"2026-09-01T08:00:00Z","repositories":{"nodes":[{"stargazerCount":4321}]}}}}`))
			return
		}
		var sent struct {
			Variables struct {
				Query string `json:"query"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(body, &sent)
		m := mergedRange.FindStringSubmatch(sent.Variables.Query)
		if m == nil {
			g.t.Errorf("a walk query with no merged: range: %s", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := m[1]
		g.mu.Lock()
		g.asked = append(g.asked, key)
		g.mu.Unlock()
		pulls, known := g.ranges[key]
		if !known {
			g.t.Errorf("the walk asked for merged:%s, which this history does not expect", key)
		}
		nodes := make([]string, 0, len(pulls))
		for _, p := range pulls {
			message := "Solo (#1)"
			if p.coauthored {
				message = "Pair (#2)\n\nCo-authored-by: Mona <mona@example.com>"
			}
			nodes = append(nodes, fmt.Sprintf(`{"mergedAt":%q,"mergeCommit":{"message":%q},"commits":{"totalCount":1,"nodes":[{"commit":{"message":"work"}}]}}`,
				p.merged, message))
		}
		fmt.Fprintf(w, `{"data":{"search":{"issueCount":%d,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}}}`,
			len(pulls), strings.Join(nodes, ","))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// walked is every merged: range asked so far.
func (g *coauthoredGitHub) walked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.asked...)
}

// coauthoredCount is the count on the Pair Extraordinaire row a sink got.
func coauthoredCount(t *testing.T, got *kept) any {
	t.Helper()
	for _, p := range got.rows("gh_achievement_progress") {
		if p.Tags["achievement"] == "pair-extraordinaire" {
			return p.Fields["count"]
		}
	}
	t.Fatal("no Pair Extraordinaire row")
	return nil
}

// TestTheCoauthoredCountIsAddedToAcrossARestart pins what issue #90 asked
// for: the co-authored count is kept in the state file with the last day it
// covers, and the next day's pass, in a process started from that file, asks
// only for the pull requests merged since, and adds them. The day a pass is
// made on is walked again by the next one, since pull requests are still
// merged into it after the pass: the pull request merged on the 12th is in
// both walks and counted once.
func TestTheCoauthoredCountIsAddedToAcrossARestart(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join("..", "..", "test", "e2e", "testdata", "achievements_page.html"))
	if err != nil {
		t.Fatal(err)
	}
	gh := &coauthoredGitHub{t: t, page: page, ranges: map[string][]coauthoredPull{
		"2026-09-01..2026-09-12": {
			{"2026-09-03T10:00:00Z", true},
			{"2026-09-04T11:00:00Z", false},
			{"2026-09-11T23:59:00Z", true},
			{"2026-09-12T08:00:00Z", true},
		},
		"2026-09-12..2026-09-13": {
			{"2026-09-12T08:00:00Z", true},
			{"2026-09-12T20:00:00Z", true},
			{"2026-09-13T09:00:00Z", false},
			{"2026-09-13T10:00:00Z", true},
		},
	}}
	path := filepath.Join(t.TempDir(), "state.json")
	pass := func(at time.Time) *kept {
		t.Helper()
		got := &kept{}
		r := sweepRunner(t, gh.handler)
		r.Sinks = []sink.Sink{got}
		r.Cfg.Targets.User = "octocat"
		r.Cfg.Every = everyOnly("achievements")
		if cfgErr := r.Cfg.Validate(); cfgErr != nil {
			t.Fatal(cfgErr)
		}
		r.State = LoadState(path)
		r.accountFamilies(context.Background(), at)
		if saveErr := r.State.Save(); saveErr != nil {
			t.Fatal(saveErr)
		}
		return got
	}

	first := pass(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC))
	if got := coauthoredCount(t, first); got != 3 {
		t.Errorf("the first pass counted %v, want the 3 co-authored pull requests of the whole history", got)
	}
	second := pass(time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))
	if got := coauthoredCount(t, second); got != 5 {
		t.Errorf("the second pass counted %v, want the 2 settled on the 11th and before, plus the 2 of the 12th and the 1 of the 13th", got)
	}
	want := []string{"2026-09-01..2026-09-12", "2026-09-12..2026-09-13"}
	if got := gh.walked(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the walks asked for %q, want the whole history and then the days since the one the count covers, %q", got, want)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Coauthored struct {
			Count   int       `json:"count"`
			Through time.Time `json:"through"`
		} `json:"coauthored"`
	}
	if err = json.Unmarshal(saved, &state); err != nil {
		t.Fatal(err)
	}
	if state.Coauthored.Count != 4 || !state.Coauthored.Through.Equal(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the state file keeps %+v, want 4 through the 12th: the 13th is still being merged into", state.Coauthored)
	}
}
