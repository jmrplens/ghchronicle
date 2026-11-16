package run

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReportedWithNoReadOnRecordReadsAMonth: a state file nobody keeps, the
// Action's or a card's, has no mark, and reading the whole inbox and the
// whole global advisory database on every such run cost 98 and 379 pages and
// six and a half minutes. A sweep with no mark reads the last thirty days of
// both, and moves the marks to just before it started.
func TestReportedWithNoReadOnRecordReadsAMonth(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var since string
	global := 0
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch req.URL.Path {
		case "/notifications":
			since = req.URL.Query().Get("since")
			_, _ = w.Write([]byte("[]"))
		case "/advisories":
			global++
			// A full page of advisories updated years ago, and a next page
			// the walk must not need.
			rows := make([]string, 100)
			for i := range rows {
				rows[i] = fmt.Sprintf(`{"ghsa_id":"GHSA-old0-0000-%04d","updated_at":"2020-01-01T00:00:00Z","credits":[]}`, i)
			}
			w.Header().Set("Link", `<https://api.github.com/advisories?after=bmV4dA>; rel="next"`)
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
		case "/graphql":
			_, _ = w.Write([]byte(`{"data":{"search":{"pageInfo":{"hasNextPage":false},"nodes":[]},` +
				`"viewer":{"repositoriesContributedTo":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`))
		default:
			_, _ = w.Write([]byte("[]"))
		}
	})
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := r.reported(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-reportedWindow).Format(time.RFC3339); since != want {
		t.Errorf("inbox read since %q, want the last thirty days, %q", since, want)
	}
	if global != 1 {
		t.Errorf("%d pages of the global database read, want the one that reaches thirty days back", global)
	}
	mem := r.State.Reported
	if want := now.Add(-reportedMargin); !mem.Notified.Equal(want) || !mem.Scanned.Equal(want) {
		t.Errorf("marks %v and %v, want both at %v", mem.Notified, mem.Scanned, want)
	}
}
