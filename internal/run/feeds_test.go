package run

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// feedServer answers the two account feeds: an inbox of five threads whose
// newest moved at noon on the 7th, and an activity feed of three full pages
// with descending ids, which is the shape that never answers a 304. Every
// request is kept so a test can read what was asked.
type feedServer struct {
	mu       sync.Mutex
	requests []request
}

// request is the path and query of one call, which is all a test reads.
type request struct{ path, query string }

func (s *feedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, request{r.URL.Path, r.URL.RawQuery})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/notifications":
		_, _ = w.Write([]byte(`[{"reason":"mention","unread":true,"updated_at":"2026-09-07T12:00:00Z",` +
			`"repository":{"full_name":"o/n","private":false},"subject":{"title":"t","type":"Issue","url":null}},` +
			`{"reason":"ci_activity","unread":true,"updated_at":"2026-09-07T09:00:00Z",` +
			`"repository":{"full_name":"o/n","private":false},"subject":{"title":"u","type":"CheckSuite","url":null}}]`))
	case strings.HasSuffix(r.URL.Path, "/events"):
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		rows := make([]string, 0, 100)
		for i := range 100 {
			rows = append(rows, `{"id":"`+strconv.Itoa(5000-100*page-i)+`","type":"WatchEvent","public":true,`+
				`"created_at":"2026-09-07T09:14:53Z","actor":{"login":"o"},"repo":{"name":"o/n"},"payload":{"action":"started"}}`)
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

// asked returns the queries made to a path, in order.
func (s *feedServer) asked(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.requests {
		if strings.HasSuffix(r.path, path) {
			out = append(out, r.query)
		}
	}
	return out
}

// feedRunner is a runner over feedServer with only the two feed families
// enabled, its state kept in a file so a test can also read it back the way
// a restart would.
func feedRunner(t *testing.T, statePath string) (*Runner, *feedServer) {
	t.Helper()
	srv := &feedServer{}
	r := sweepRunner(t, srv.ServeHTTP)
	r.Cfg.Every = everyOnly("events", "notifs")
	r.Cfg.Every.Families["events"], r.Cfg.Every.Families["notifs"] = "30m", "30m"
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.State = LoadState(statePath)
	return r, srv
}

// TestTheInboxIsReadSinceTheLastThreadThatMoved pins A3: the first sweep reads
// the inbox whole, the next asks only for what moved after the newest thread
// seen minus two cadences, and a day later the whole inbox is read again,
// which bounds to a day whatever the window left out.
func TestTheInboxIsReadSinceTheLastThreadThatMoved(t *testing.T) {
	t.Parallel()
	r, srv := feedRunner(t, filepath.Join(t.TempDir(), "state.json"))
	start := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)

	r.accountFamilies(context.Background(), start)
	if got := srv.asked("/notifications"); len(got) != 1 || strings.Contains(got[0], "since=") || !strings.Contains(got[0], "all=true") {
		t.Fatalf("the first sweep must read the whole inbox, read threads included, asked %v", got)
	}
	if want := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC); !r.State.LastNotified.Equal(want) {
		t.Errorf("LastNotified = %s, want the newest updated_at on the page, %s", r.State.LastNotified, want)
	}

	// Half an hour later: a window from the newest thread minus two
	// cadences, which is an hour before noon on the 7th.
	r.accountFamilies(context.Background(), start.Add(30*time.Minute))
	got := srv.asked("/notifications")
	if len(got) != 2 || !strings.Contains(got[1], "since=2026-09-07T11:00:00Z") || !strings.Contains(got[1], "all=false") {
		t.Errorf("the second sweep must ask for the unread threads since the newest one minus two cadences, asked %v", got)
	}

	// A day later the full pass is due again.
	r.accountFamilies(context.Background(), start.Add(24*time.Hour+30*time.Minute))
	// Read threads included: a thread read without a reply leaves the unread
	// listing, and this is the one read that writes it back with unread false.
	if got = srv.asked("/notifications"); len(got) != 3 || strings.Contains(got[2], "since=") || !strings.Contains(got[2], "all=true") {
		t.Errorf("the daily pass must read the whole inbox, read threads included, asked %v", got)
	}
	// And a backfill always does.
	r.Backfill = true
	r.accountFamilies(context.Background(), start.Add(25*time.Hour+30*time.Minute))
	if got = srv.asked("/notifications"); len(got) != 4 || strings.Contains(got[3], "since=") {
		t.Errorf("a backfill must read the whole inbox, asked %v", got)
	}
}

// TestAnOlderStateFileReadsTheInboxWhole: a state.json written before the
// window existed has no last_notified, and the sweep after the upgrade must
// take the whole inbox once rather than fail or ask since the zero time.
func TestAnOlderStateFileReadsTheInboxWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	older := `{"last_run":{"notifs":"2026-09-08T14:30:00Z","events":"2026-09-08T14:30:00Z"},"first_saw":{},"last_head":{}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	r, srv := feedRunner(t, path)
	r.accountFamilies(context.Background(), time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC))
	if got := srv.asked("/notifications"); len(got) != 1 || strings.Contains(got[0], "since=") {
		t.Errorf("a state without last_notified must read the whole inbox, asked %v", got)
	}
	if got := srv.asked("/events"); len(got) != 3 {
		t.Errorf("a state without last_event must read the whole feed, asked %d pages", len(got))
	}
	// What the sweep learned reaches the file, under the names an older
	// binary ignores.
	if err := r.State.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if string(saved["last_event"]) != `"4900"` || !strings.Contains(string(saved["last_notified"]), "2026-09-07T12:00:00Z") {
		t.Errorf("saved state = %s", b)
	}
}

// TestTheFeedStopsAtThePageWithTheLastEventSeen pins A6: the first sweep reads
// all three pages, and the next stops at the page that carries the newest id
// of the first, which on a quiet account is the first page.
func TestTheFeedStopsAtThePageWithTheLastEventSeen(t *testing.T) {
	t.Parallel()
	r, srv := feedRunner(t, filepath.Join(t.TempDir(), "state.json"))
	start := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	r.accountFamilies(context.Background(), start)
	if got := srv.asked("/events"); len(got) != 3 {
		t.Fatalf("the first sweep must read the whole feed, asked %d pages", len(got))
	}
	if r.State.LastEvent != "4900" {
		t.Errorf("LastEvent = %q, want the first id of the first page", r.State.LastEvent)
	}
	r.accountFamilies(context.Background(), start.Add(30*time.Minute))
	if got := srv.asked("/events"); len(got) != 4 {
		t.Errorf("the second sweep must stop at the page that carries the last event seen, asked %d pages in total", len(got))
	}
	// A backfill reads the whole feed whatever the state says.
	r.Backfill = true
	r.accountFamilies(context.Background(), start.Add(time.Hour))
	if got := srv.asked("/events"); len(got) != 7 {
		t.Errorf("a backfill must read the whole feed, asked %d pages in total", len(got))
	}
}

// TestIssueEventsWindowFollowsTheCadence pins D3's runner side: a first
// sweep leaves the window to the collector's thirty day default, a later one
// reads twice the cadence back, one after a gap reads from a cadence before
// the last run, and a backfill walks.
func TestIssueEventsWindowFollowsTheCadence(t *testing.T) {
	t.Parallel()
	r, _ := feedRunner(t, filepath.Join(t.TempDir(), "state.json"))
	// An hourly cadence, so the window is a number and not the zero a
	// disabled family would hand back.
	r.Cfg.Every.Families["issueevents"] = "1h"
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	every, _ := r.Cfg.Interval("issueevents")
	if every != time.Hour {
		t.Fatalf("issueevents cadence = %s, want an hour", every)
	}
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	if got := r.issueEvents(now); !got.Since.IsZero() || got.Walk.Pages != 0 {
		t.Errorf("first sweep = %+v, want the collector's own default window", got)
	}
	r.State.Mark("issueevents", now.Add(-time.Hour))
	if got := r.issueEvents(now); !got.Since.Equal(now.Add(-2*every)) || got.Walk.Pages != 0 {
		t.Errorf("sweep = %+v, want since twice the cadence back", got)
	}
	// The process was stopped for a day: the first sweep after it reads
	// from a cadence before the last run, or the day's transitions would be
	// in no series until someone thought to backfill.
	r.State.Mark("issueevents", now.Add(-26*time.Hour))
	if got := r.issueEvents(now); !got.Since.Equal(now.Add(-26*time.Hour - every)) {
		t.Errorf("sweep after a gap = %+v, want since a cadence before the last run", got)
	}
	r.Backfill = true
	r.BackfillSince = now.AddDate(-1, 0, 0)
	if got := r.issueEvents(now); got.Walk.Pages != -1 || !got.Walk.Since.Equal(r.BackfillSince) {
		t.Errorf("backfill = %+v, want the walk back to BackfillSince", got)
	}
}

// TestAnEmptyOrFailedFeedKeepsTheLastEventSeen: the event a sweep stops at
// moves only when the feed was read and had a newest event. An empty feed
// has none to offer, and a feed that failed on a later page has not been
// read down to the old mark, so moving it would leave a gap for good.
func TestAnEmptyOrFailedFeedKeepsTheLastEventSeen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, page int)
	}{
		{"an empty feed", func(w http.ResponseWriter, _ int) { _, _ = w.Write([]byte(`[]`)) }},
		{"a feed that fails on its second page", func(w http.ResponseWriter, page int) {
			if page > 1 {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			rows := make([]string, 0, 100)
			for i := range 100 {
				rows = append(rows, `{"id":"`+strconv.Itoa(4900-i)+`","type":"WatchEvent","public":true,`+
					`"created_at":"2026-09-07T09:14:53Z","actor":{"login":"o"},"repo":{"name":"o/n"},"payload":{}}`)
			}
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
		}},
	} {
		r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
			page, _ := strconv.Atoi(req.URL.Query().Get("page"))
			tc.answer(w, page)
		})
		r.State.LastEvent = "4242"
		_, _ = r.events(context.Background(), "o", time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC))
		if r.State.LastEvent != "4242" {
			t.Errorf("%s moved the last event seen to %q, want 4242 kept", tc.name, r.State.LastEvent)
		}
	}
}

// TestIssueEventsReadTwoCadencesBackAfterARecentSweep: when the last run is
// within the window, the window is two cadences back from now and not a
// cadence back from the last run, which would read half an hour less.
func TestIssueEventsReadTwoCadencesBackAfterARecentSweep(t *testing.T) {
	t.Parallel()
	r := pullsRunner(t)
	every, _ := r.Cfg.Interval("issueevents")
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	r.State.Mark("issueevents", now.Add(-every/2))
	if got := r.issueEvents(now); !got.Since.Equal(now.Add(-2 * every)) {
		t.Errorf("a sweep half a cadence after the last reads from %s, want %s", got.Since, now.Add(-2*every))
	}
}

// TestAFailedInboxReadKeepsItsWindowAndItsDailyPassDue: an inbox that did
// not answer moves neither the window nor the record of the whole read, so
// the next sweep asks for the same thing again.
func TestAFailedInboxReadKeepsItsWindowAndItsDailyPassDue(t *testing.T) {
	t.Parallel()
	r := sweepRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	cut := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r.State.LastNotified = cut
	if _, err := r.notifications(context.Background(), time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("the inbox failed and notifications returned no error")
	}
	if !r.State.LastNotified.Equal(cut) {
		t.Errorf("a failed read moved the window to %s", r.State.LastNotified)
	}
	if when, full := r.State.LastFull["notifs"]; full {
		t.Errorf("a failed whole read was put on record at %s", when)
	}
}
