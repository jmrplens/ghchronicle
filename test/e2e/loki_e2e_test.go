package e2e

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type lokiBody struct {
	Streams []lokiStream `json:"streams"`
}

// lokiKinds is every stream label the sink can emit, one per measurement that
// has an event rendering. Anything else is a gauge in disguise and must not
// reach a log store.
var lokiKinds = map[string]bool{
	"star": true, "star_given": true, "fork": true, "release": true, "package": true,
	"pull_request": true, "review": true, "issue": true, "commit": true,
	"workflow_run": true, "repo_activity": true, "alert": true, "code_scanning": true,
	"event": true, "notification": true, "discussion": true, "webhook": true,
	"job_log": true, "external_contribution": true,
	"deployment": true, "review_thread": true, "ruleset": true,
}

func decodeLoki(t *testing.T, body []byte) lokiBody {
	t.Helper()
	var out lokiBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the Loki push is not JSON: %v\n%s", err, truncate(body))
	}
	return out
}

// lokiEntryCount is how many log lines the receiver accepted, across every
// push and every stream in it.
func lokiEntryCount(t *testing.T, rec *capture) int {
	t.Helper()
	entries := 0
	for _, r := range rec.Accepted() {
		for _, s := range decodeLoki(t, r.Body).Streams {
			entries += len(s.Values)
		}
	}
	return entries
}

func TestLokiSinkPushesEventsAsLogLines(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	// Ten years of horizon, so the fixtures' own dates decide what is sent
	// rather than the age of the fixtures.
	cfg := writeSinkConfig(t, dir, gh.URL(), `  loki:
    url: `+rec.URL()+`/loki/api/v1/push
    tenant_id: e2e-tenant
    max_age: 87600h
    labels:
      job: ghchronicle-e2e
      env: test`)

	sweepOnce(t, cfg)

	reqs := rec.Accepted()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the Loki push endpoint")
	}
	kinds := map[string]int{}
	for _, r := range reqs {
		assertLokiEnvelope(t, &r)
		for _, s := range decodeLoki(t, r.Body).Streams {
			kinds[s.Stream["kind"]] += len(s.Values)
			assertLokiStream(t, s)
		}
	}

	for _, want := range []string{"workflow_run", "star", "commit", "event"} {
		if kinds[want] == 0 {
			t.Errorf("no %s entries were pushed; kinds seen: %v", want, sortedNames(kinds))
		}
	}
}

// assertLokiEnvelope checks what every push carries whatever is in it: where
// it went, how it was encoded, and the tenant the config asked for.
func assertLokiEnvelope(t *testing.T, r *capturedRequest) {
	t.Helper()
	if r.Method != http.MethodPost || r.Path != "/loki/api/v1/push" {
		t.Errorf("%s %s, want POST /loki/api/v1/push", r.Method, r.Path)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := r.Header.Get("X-Scope-OrgID"); got != "e2e-tenant" {
		t.Errorf("X-Scope-OrgID = %q, want the configured tenant", got)
	}
}

// assertLokiStream checks one stream's labels and the entries under it.
func assertLokiStream(t *testing.T, s lokiStream) {
	t.Helper()
	kind := s.Stream["kind"]
	if !lokiKinds[kind] {
		t.Errorf("stream label kind = %q, which is not an event rendering: %v", kind, s.Stream)
	}
	if s.Stream["job"] != "ghchronicle-e2e" || s.Stream["env"] != "test" {
		t.Errorf("the configured labels did not reach the stream: %v", s.Stream)
	}
	if len(s.Values) == 0 {
		t.Errorf("stream %q carries no entry", kind)
	}
	// Loki rejects a stream whose entries are not in ascending order.
	var previous int64
	for _, v := range s.Values {
		at, err := strconv.ParseInt(v[0], 10, 64)
		if err != nil {
			t.Fatalf("stream %q: entry timestamp %q does not parse: %v", kind, v[0], err)
		}
		if at < previous {
			t.Errorf("stream %q is not in ascending time order: %d after %d", kind, at, previous)
		}
		previous = at
		assertLokiEntry(t, kind, v[1])
	}
}

// assertLokiEntry checks one rendered entry: a sentence, then the structure
// after it.
func assertLokiEntry(t *testing.T, kind, entry string) {
	t.Helper()
	message, pairs, n := splitLogfmt(entry)
	if n < 2 {
		t.Errorf("stream %q: entry carries %d logfmt pairs: %s", kind, n, entry)
	}
	// The job log's line is the message, by design; every other rendering is a
	// sentence with the structure after it.
	if kind != "job_log" && strings.TrimSpace(message) == "" {
		t.Errorf("stream %q: entry has no sentence before its pairs: %s", kind, entry)
	}
	if kind == "workflow_run" && pairs["full_name"] != "octocat/hello-world" {
		t.Errorf("workflow_run entry lost its tags: %s", entry)
	}
	// A gauge-only field would mean a measurement with no event rendering had
	// been let through.
	if _, ok := pairs["has_code_of_conduct"]; ok {
		t.Errorf("a repository health gauge was pushed as a log line: %s", entry)
	}
}

// TestLokiDropsWhatItWouldBeRefusedFor pins the documented horizon: an entry
// too far behind is left out and counted, at debug level, rather than costing
// the whole push. It is a *sink.DroppedError, which the runner does not treat as a
// failure.
func TestLokiDropsWhatItWouldBeRefusedFor(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), `  loki:
    url: `+rec.URL()+`/loki/api/v1/push
    max_age: 1h`)

	out := sweepOnce(t, cfg)

	if !strings.Contains(out, "sink dropped old entries") || !strings.Contains(out, "sink=loki") {
		t.Errorf("the horizon dropped nothing, or said nothing about it:\n%s", out)
	}
	if strings.Contains(out, "sink write failed") {
		t.Errorf("a drop was reported as a failure:\n%s", out)
	}
	// What is inside the horizon still goes: the fixtures date one workflow
	// run at the moment of the run.
	if lokiEntryCount(t, rec) == 0 {
		t.Errorf("the horizon dropped everything, including the entries inside it")
	}
}
