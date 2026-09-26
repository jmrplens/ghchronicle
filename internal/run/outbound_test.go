package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// TestAnOutboundSearchPastTheCapIsSaidOnce pins what the outbound family does
// with an account past GitHub's thousand results: every search whose pages run
// out before its count does is one warning per process, naming the kind and
// the state, and the family is neither failed nor left unmarked. The open
// states are read whole on every sweep, so without the dedupe this would be
// two lines every twelve hours for as long as the account stays past the cap.
func TestAnOutboundSearchPastTheCapIsSaidOnce(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/graphql" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		if !bytes.Contains(body, []byte("search(type: ISSUE")) {
			// The stars given and the two comment walks, all empty.
			_, _ = w.Write([]byte(`{"data":{"viewer":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"search":{"issueCount":1500,` +
			`"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOjE="},` +
			`"nodes":[{"number":7,"title":"t","url":"https://github.com/someone/else/pull/7",` +
			`"createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-02T10:00:00Z","closedAt":null,` +
			`"comments":{"totalCount":0},"repository":{"nameWithOwner":"someone/else"}}]}}}`))
	})
	r.Cfg.Targets.User = "octocat"
	r.Cfg.Every = everyOnly("outbound")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&log, nil))

	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		r.prime = true
		r.accountFamilies(context.Background(), start.Add(time.Duration(i)*time.Minute))
	}
	if got := strings.Count(log.String(), "outbound search read fewer items than it counts"); got != 5 {
		t.Errorf("the cap was reported %d times over three sweeps, want once for each of the five searches:\n%s", got, log.String())
	}
	for _, want := range []string{
		"kind=pull_request state=merged count=1500 read=1",
		"kind=pull_request state=open count=1500 read=1",
		"kind=pull_request state=closed count=1500 read=1",
		"kind=issue state=open count=1500 read=1",
		"kind=issue state=closed count=1500 read=1",
	} {
		if strings.Count(log.String(), want) != 1 {
			t.Errorf("want %s once:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "collector failed") {
		t.Errorf("a search past the cap is a warning, not a failed collector:\n%s", log.String())
	}
	if _, marked := r.State.LastRun["outbound"]; !marked {
		t.Error("the family was not marked as run, so it would be asked on every tick")
	}
}

// TestAnOutboundSweepReadsBackToTheSweepBefore is what makes the promise of
// #76 hold: a sweep reads each closed search back to a cadence before the
// outbound sweep before it, and not one page of it. Here the collector was
// down for three days, and more than a page of items moved in that time, so
// the one that closed while it was down is on the second page. A sweep that
// read one page lost it for good, with nothing to say a backfill was needed.
// The sweep after that, a cadence on, reads back to a cadence before this
// one, and the first page ends past that.
func TestAnOutboundSweepReadsBackToTheSweepBefore(t *testing.T) {
	t.Parallel()
	down := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	back := down.Add(72 * time.Hour)
	var mu sync.Mutex
	pages := map[string]int{}
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(body.Query, "search(type: ISSUE") {
			_, _ = w.Write([]byte(`{"data":{"viewer":{}}}`))
			return
		}
		search, _ := body.Variables["query"].(string)
		state, _, _ := strings.Cut(search, " author:")
		mu.Lock()
		pages[state]++
		mu.Unlock()
		// Page one ends on an item that moved a day into the outage, page
		// two on one from before it.
		next, updated := false, down.Add(-48*time.Hour)
		if body.Variables["after"] == nil {
			next, updated = true, down.Add(24*time.Hour)
		}
		closed := `"` + updated.Format(time.RFC3339) + `"`
		if strings.Contains(state, "is:open") {
			closed = "null"
		}
		_, _ = fmt.Fprintf(w, `{"data":{"search":{"issueCount":200,`+
			`"pageInfo":{"hasNextPage":%t,"endCursor":"Y3Vyc29yOjEwMA=="},`+
			`"nodes":[{"number":7,"title":"t","url":"https://github.com/someone/else/pull/7",`+
			`"createdAt":"2026-01-01T10:00:00Z","updatedAt":%q,"closedAt":%s,`+
			`"comments":{"totalCount":0},"repository":{"nameWithOwner":"someone/else"}}]}}}`,
			next, updated.Format(time.RFC3339), closed)
	})
	r.Cfg.Targets.User = "octocat"
	r.Cfg.Every = config.Every{Default: "0", Families: map[string]string{"outbound": "12h"}}
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sweep := func(now time.Time) map[string]int {
		mu.Lock()
		clear(pages)
		mu.Unlock()
		r.accountFamilies(context.Background(), now)
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(pages)
	}

	sweep(down)
	read := sweep(back)
	for _, state := range []string{
		"is:pr is:merged", "is:pr is:closed is:unmerged", "is:issue is:closed",
		"is:pr is:open", "is:issue is:open",
	} {
		if read[state] != 2 {
			t.Errorf("back after three days, %q read %d pages, want both", state, read[state])
		}
	}
	read = sweep(back.Add(13 * time.Hour))
	for _, state := range []string{"is:pr is:merged", "is:pr is:closed is:unmerged", "is:issue is:closed"} {
		if read[state] != 1 {
			t.Errorf("a cadence later, %q read %d pages, want the one that ends before the sweep before", state, read[state])
		}
	}
}
