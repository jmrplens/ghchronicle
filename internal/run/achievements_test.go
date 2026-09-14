package run

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
