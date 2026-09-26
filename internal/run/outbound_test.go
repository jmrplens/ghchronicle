package run

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
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
