package e2e

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

func TestTelegrafSinkPostsLineProtocol(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	// The URL names the host only, so the sink has to supply the listener's
	// default path itself: http_listener_v2 answers 404 to "/" without ever
	// saying why.
	cfg := writeSinkConfig(t, dir, gh.URL(), `  telegraf:
    url: `+rec.URL()+`
    username: telegraf-user
    password: telegraf-pass`)

	sweepOnce(t, cfg)

	reqs := rec.Accepted()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the Telegraf listener")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("telegraf-user:telegraf-pass"))
	for _, r := range reqs {
		if r.Method != http.MethodPost || r.Path != "/telegraf" {
			t.Errorf("%s %s, want POST /telegraf", r.Method, r.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want basic auth from the config", got)
		}
	}

	points := parseLineProtocol(t, rec.Body())
	if len(points) < 100 {
		t.Fatalf("only %d lines reached Telegraf", len(points))
	}
	byName := measurementsOf(points)
	// Telegraf is the door to every other output, so it gets everything,
	// the job log included.
	for _, name := range []string{"gh_traffic", "gh_repo", "gh_workflow_run", "gh_job_log"} {
		if byName[name] == 0 {
			t.Errorf("no %s line reached Telegraf; measurements seen: %v", name, sortedNames(byName))
		}
	}
	assertNanosecondStamps(t, points)
}
