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
	err := tg.Write(context.Background(), []Point{
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
	if err := tg.Write(context.Background(), points); err != nil {
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
	err := NewTelegraf(srv.URL, "", "", 0, 0).Write(context.Background(), []Point{{
		Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Now(),
	}})
	if err == nil || !strings.HasPrefix(err.Error(), "telegraf write: 400") || !strings.Contains(err.Error(), "unable to parse") {
		t.Errorf("err = %v", err)
	}
}
