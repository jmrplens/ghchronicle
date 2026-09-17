package sink

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegrafPostsLineProtocolWithBasicAuth(t *testing.T) {
	var body, contentType, user, pass, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, contentType, path = string(raw), r.Header.Get("Content-Type"), r.URL.Path
		user, pass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// A bare host: the listener's default path has to be filled in, because
	// http_listener_v2 answers 404 to "/" without saying why.
	tg := NewTelegraf(srv.URL, "ghc", "secret", 0, 0)
	_, err := tg.Write(context.Background(), []Point{
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
			Fields: map[string]any{"stars": 3}, Time: time.Unix(0, 1700000000000000000),
		},
		{
			Measurement: "gh_repo", Tags: map[string]string{"repo": "b"},
			Fields: map[string]any{"stars": 4.5}, Time: time.Unix(0, 1700000000000000000),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/telegraf" {
		t.Errorf("path = %q, want /telegraf", path)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("content type = %q", contentType)
	}
	if user != "ghc" || pass != "secret" {
		t.Errorf("basic auth = %q/%q", user, pass)
	}
	want := "gh_repo,repo=a stars=3i 1700000000000000000\ngh_repo,repo=b stars=4.5 1700000000000000000"
	if body != want {
		t.Errorf("body = %q\nwant %q", body, want)
	}
}

func TestTelegrafKeepsAnExplicitPathAndBatches(t *testing.T) {
	var paths []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(raw))
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("no credentials were configured, none should be sent")
		}
	}))
	defer srv.Close()

	tg := NewTelegraf(srv.URL+"/ingest", "", "", 2, 0)
	var points []Point
	for i := range 5 {
		points = append(points, Point{
			Measurement: "m", Tags: map[string]string{"n": "x"},
			Fields: map[string]any{"v": i}, Time: time.Now(),
		})
	}
	if _, err := tg.Write(context.Background(), points); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("5 lines in batches of 2 should be 3 requests, got %d", len(bodies))
	}
	for _, p := range paths {
		if p != "/ingest" {
			t.Errorf("path = %q, the configured one must be kept", p)
		}
	}
	if n := strings.Count(bodies[0], "\n"); n != 1 {
		t.Errorf("first batch has %d newlines, want 1 (two lines)", n)
	}
}

func TestTelegrafNamesItselfOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unable to parse", http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := NewTelegraf(srv.URL, "", "", 0, 0).Write(context.Background(), []Point{{
		Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Now(),
	}})
	if err == nil || !strings.HasPrefix(err.Error(), "telegraf write: 400") || !strings.Contains(err.Error(), "unable to parse") {
		t.Errorf("err = %v", err)
	}
}

// TestNewTelegrafFillsInOnlyWhatWasLeftOut gives a zero batch and timeout their
// documented values and keeps the ones that were set.
func TestNewTelegrafFillsInOnlyWhatWasLeftOut(t *testing.T) {
	tg := NewTelegraf("http://telegraf.test:8186/ingest", "", "", 0, 0)
	if tg.Batch != 5000 || tg.client.Timeout != 60*time.Second || tg.Name() != "telegraf" || tg.Close() != nil {
		t.Errorf("defaults = batch %d, timeout %v, name %q", tg.Batch, tg.client.Timeout, tg.Name())
	}
	tg = NewTelegraf("http://telegraf.test:8186/ingest", "", "", 3, 2*time.Second)
	if tg.Batch != 3 || tg.client.Timeout != 2*time.Second {
		t.Errorf("given = batch %d, timeout %v, want 3 and 2s kept", tg.Batch, tg.client.Timeout)
	}
}

// TestTelegrafDefaultPathFillsOnlyAnEmptyPath treats a lone slash as no path,
// keeps any other, and leaves a value that is not a URL for the push to
// refuse by name.
func TestTelegrafDefaultPathFillsOnlyAnEmptyPath(t *testing.T) {
	for raw, want := range map[string]string{
		"http://telegraf.test:8186":         "http://telegraf.test:8186/telegraf",
		"http://telegraf.test:8186/":        "http://telegraf.test:8186/telegraf",
		"http://telegraf.test:8186/ingest":  "http://telegraf.test:8186/ingest",
		"http://telegraf.test:8186/\x7fbad": "http://telegraf.test:8186/\x7fbad",
	} {
		if got := withDefaultPath(raw, "/telegraf"); got != want {
			t.Errorf("withDefaultPath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestTelegrafSendsNothingWhenNothingRenders makes no request for a batch with
// no line, and none past the last line when the batch divides exactly.
func TestTelegrafSendsNothingWhenNothingRenders(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	tg := NewTelegraf(srv.URL, "", "", 2, 0)
	if _, err := tg.Write(context.Background(), []Point{{Measurement: "m", Fields: map[string]any{"none": nil}}}); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Errorf("%d requests for a batch with no line, want none", requests)
	}
	if _, err := tg.Write(context.Background(), []Point{
		{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)},
		{Measurement: "m", Fields: map[string]any{"v": 2}, Time: time.Unix(0, 2)},
	}); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Errorf("%d requests for two lines in batches of two, want one", requests)
	}
}

// TestTelegrafTreatsAnyNonSuccessStatusAsAFailure includes 300, and reports an
// unusable endpoint and a listener that is not there.
func TestTelegrafTreatsAnyNonSuccessStatusAsAFailure(t *testing.T) {
	point := []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(0, 1)}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultipleChoices)
	}))
	defer srv.Close()
	if _, err := NewTelegraf(srv.URL, "", "", 0, 0).Write(context.Background(), point); err == nil ||
		!strings.HasPrefix(err.Error(), "telegraf write: 300") {
		t.Errorf("Write = %v, want the 300 reported", err)
	}
	if _, err := NewTelegraf("telegraf:8186", "", "", 0, 0).Write(context.Background(), point); err == nil ||
		!strings.HasPrefix(err.Error(), "telegraf write: endpoint ") {
		t.Errorf("Write = %v, want the endpoint refused", err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := NewTelegraf(closed.URL, "", "", 0, 0).Write(context.Background(), point); err == nil {
		t.Error("Write reported success to a listener that is not there")
	}
}
