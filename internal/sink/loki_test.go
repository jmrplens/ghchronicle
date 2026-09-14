package sink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestLokiSendsOnlyEvents(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "someone", "full_name": "o/r"},
			Fields: map[string]any{"starred": 1}, Time: now,
		},
		// A gauge, not an event. Sending it would make the log a slow copy of
		// the metrics.
		{
			Measurement: "gh_repo", Tags: map[string]string{"full_name": "o/r"},
			Fields: map[string]any{"stars": 3}, Time: now,
		},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.Streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(got.Streams))
	}
	if got.Streams[0].Stream["kind"] != "star" {
		t.Errorf("kind = %q", got.Streams[0].Stream["kind"])
	}
	line := got.Streams[0].Values[0][1]
	if !strings.HasPrefix(line, "someone starred o/r") {
		t.Errorf("line does not read as a sentence: %q", line)
	}
	if !strings.Contains(line, `user="someone"`) {
		t.Errorf("line carries no structured pairs: %q", line)
	}
}

func TestLokiOrdersEntriesInEachStream(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	// Deliberately out of order: Loki rejects a stream whose entries descend.
	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{Measurement: "gh_star", Tags: map[string]string{"user": "b"}, Fields: map[string]any{"starred": 1}, Time: now},
		{Measurement: "gh_star", Tags: map[string]string{"user": "a"}, Fields: map[string]any{"starred": 1}, Time: now.Add(-time.Minute)},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	v := got.Streams[0].Values
	if len(v) != 2 || v[0][0] >= v[1][0] {
		t.Errorf("entries are not ascending: %v", v)
	}
}

func TestLokiDropsEntriesLokiWouldReject(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	// A star from years ago is normal here: the collector recovers the whole
	// history. Loki refuses the entire push for one such line, so the sink has
	// to leave it out rather than lose the batch.
	err := NewLoki(srv.URL, "", nil, 0, 24*time.Hour, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "old"},
			Fields: map[string]any{"starred": 1}, Time: now.AddDate(-3, 0, 0),
		},
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "new"},
			Fields: map[string]any{"starred": 1}, Time: now,
		},
	})
	var dropped *DroppedError
	if !errors.As(err, &dropped) || dropped.N != 1 {
		t.Fatalf("err = %v, want a DroppedError reporting one entry", err)
	}
	if len(got.Streams) != 1 || len(got.Streams[0].Values) != 1 {
		t.Errorf("the recent entry must still be sent, got %+v", got.Streams)
	}
}

func TestLokiReportsWhenEverythingWasTooOld(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := NewLoki(srv.URL, "", nil, 0, time.Hour, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "old"},
			Fields: map[string]any{"starred": 1}, Time: time.Now().AddDate(-1, 0, 0),
		},
	})
	if _, ok := errors.AsType[*DroppedError](err); !ok {
		t.Fatalf("err = %v, want a DroppedError", err)
	}
	if called {
		t.Error("an empty push must not be sent at all")
	}
}

func TestLokiRefusesToFallBehindItsOwnStream(t *testing.T) {
	var pushes []lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var p lokiPush
		_ = json.Unmarshal(raw, &p)
		pushes = append(pushes, p)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	l := NewLoki(srv.URL, "", nil, 0, 30*time.Minute, 0)

	// A recent entry establishes the stream's high-water mark.
	if err := l.Write(context.Background(), []Point{
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "new"},
			Fields: map[string]any{"starred": 1}, Time: now,
		},
	}); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// Loki refuses an entry more than its out-of-order window behind the
	// newest one already in that stream, even when the entry is well inside
	// reject_old_samples_max_age. Measured against a real Loki 3: a stream
	// holding 19:14 rejected 00:35 of the same day.
	err := l.Write(context.Background(), []Point{
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "behind"},
			Fields: map[string]any{"starred": 1}, Time: now.Add(-45 * time.Minute),
		},
	})
	var dropped *DroppedError
	if !errors.As(err, &dropped) || dropped.N != 1 {
		t.Fatalf("err = %v, want a DroppedError reporting one entry behind the watermark", err)
	}
	if len(pushes) != 1 {
		t.Errorf("the second push had nothing to send, got %d pushes", len(pushes))
	}
}

func TestLokiKeepsTheSpreadInsideOneBatch(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	// The rule has to be applied within the batch as well. Loki takes the
	// newest entry of the push and judges the rest against it, so a batch
	// spanning a day fails on its own first push without this.
	err := NewLoki(srv.URL, "", nil, 0, 20*time.Minute, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "new"},
			Fields: map[string]any{"starred": 1}, Time: now,
		},
		{
			Measurement: "gh_star", Tags: map[string]string{"user": "old"},
			Fields: map[string]any{"starred": 1}, Time: now.Add(-40 * time.Minute),
		},
	})
	var dropped *DroppedError
	if !errors.As(err, &dropped) || dropped.N != 1 {
		t.Fatalf("err = %v, want one entry dropped", err)
	}
	if len(got.Streams) != 1 || len(got.Streams[0].Values) != 1 {
		t.Errorf("expected exactly the newest entry, got %+v", got.Streams)
	}
}

func TestLokiNamesTheWorkflowByItsPath(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// A dynamic run, where the two candidates disagree the most: the path
	// identifies CodeQL and the name is whatever the pull request is called.
	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_workflow_run",
			Tags: map[string]string{
				"workflow":  "dynamic/github-code-scanning/codeql",
				"full_name": "o/r", "conclusion": "failure",
			},
			Fields: map[string]any{"duration_seconds": 42, "name": "PR #495"},
			Time:   time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	line := got.Streams[0].Values[0][1]
	if !strings.HasPrefix(line, "dynamic/github-code-scanning/codeql on o/r: failure after 42s") {
		t.Errorf("the sentence must lead with the workflow's stable identity: %q", line)
	}
	if !strings.Contains(line, `name="PR #495"`) {
		t.Errorf("the human name still travels in the structured half: %q", line)
	}
}

// TestEveryDatedItemIsRenderedOrRefused is the forcing function for the event
// table.
//
// eventsByStream drops what it has no rule for without a word, which is right
// for a gauge and wrong for an event: gh_deployment and gh_review_thread were
// dated events with no rendering for months, and the fourth table in this
// repository to mirror the collectors and go stale unannounced. The reduction
// already decides which measurements are dated items, one per fact, so that
// list is the domain here and a new one cannot be added without answering the
// question.
func TestEveryDatedItemIsRenderedOrRefused(t *testing.T) {
	for _, m := range datedItems() {
		_, rendered := lokiEvents[m]
		why, refused := lokiNotEvents[m]
		switch {
		case rendered && refused:
			t.Errorf("%s is both rendered and refused: %q", m, why)
		case !rendered && !refused:
			t.Errorf("%s is a dated item (promRules counts it) and Loki does nothing with it. "+
				"eventsByStream drops it silently, so the log simply lacks it and nothing says so. "+
				"Give it a rendering in lokiEvents, or a reason in lokiNotEvents. "+
				"A reason is a decision; absence is an accident.", m)
		}
	}
}

// TestNoRefusalOutlivesItsMeasurement is the other direction. A refusal for a
// measurement that no longer exists, or one that has since been rendered, is a
// reason nobody will read and a decision that is no longer being made.
func TestNoRefusalOutlivesItsMeasurement(t *testing.T) {
	dated := map[string]bool{}
	for _, m := range datedItems() {
		dated[m] = true
	}
	for _, m := range sortedKeys2(lokiNotEvents) {
		why := lokiNotEvents[m]
		if !dated[m] {
			t.Errorf("lokiNotEvents refuses %s, which promRules does not count as a dated item", m)
		}
		if _, rendered := lokiEvents[m]; rendered {
			t.Errorf("lokiNotEvents refuses %s and lokiEvents renders it", m)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is refused with no reason, which is the silence this table replaces", m)
		}
		if strings.Contains(why, "—") {
			t.Errorf("%s: no em dash characters anywhere in this repository", m)
		}
	}
}

// TestEveryRenderingHasACollector catches the third way this table rots: a
// rendering for a measurement nothing writes any more, which is a stream that
// can never appear and a line of code nobody can test.
func TestEveryRenderingHasACollector(t *testing.T) {
	written := measurementsInCollectors(t)
	rendered := make([]string, 0, len(lokiEvents))
	for m := range lokiEvents {
		rendered = append(rendered, m)
	}
	sort.Strings(rendered)
	for _, m := range rendered {
		if _, ok := written[m]; !ok {
			t.Errorf("lokiEvents renders %s, which no collector writes any more", m)
		}
	}
}

// datedItems is every measurement the reduction counts, which is its own
// definition of a point per fact dated when the fact happened.
func datedItems() []string {
	var out []string
	for m, r := range promRules {
		if r.mode == count {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// TestLokiRendersTheTwoDatedEventsThatHadNoLine checks the sentences, not only
// that a stream appeared: a rendering that reads the wrong tag produces a
// perfectly valid line saying nothing, which is how the workflow name got into
// these lines and had to come out again.
func TestLokiRendersTheTwoDatedEventsThatHadNoLine(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_deployment",
			Tags: map[string]string{
				"full_name": "o/r", "environment": "production", "deployment": "1234",
			},
			Fields: map[string]any{"deployments": 1, "creator": "octocat", "run_id": int64(9001), "outcome": "success"},
			Time:   now,
		},
		{
			Measurement: "gh_review_thread",
			Tags: map[string]string{
				"full_name": "o/r", "number": "494", "author": "reviewer", "bot": "false",
			},
			Fields: map[string]any{"path": "internal/sink/loki.go", "comments": 3, "resolved": 0},
			Time:   now,
		},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	lines := map[string]string{}
	for _, s := range got.Streams {
		if len(s.Values) != 1 {
			t.Fatalf("stream %q carries %d entries, want 1", s.Stream["kind"], len(s.Values))
		}
		lines[s.Stream["kind"]] = s.Values[0][1]
	}
	for kind, want := range map[string][]string{
		"deployment": {
			"octocat deployed o/r to production: success",
			`environment="production"`, "run_id=9001",
		},
		"review_thread": {
			"reviewer opened a thread on o/r#494 (internal/sink/loki.go), 3 comments",
			`bot="false"`, "resolved=0",
		},
	} {
		line, ok := lines[kind]
		if !ok {
			t.Errorf("no stream named %q; got %v", kind, lines)
			continue
		}
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Errorf("the %s line does not carry %q:\n%s", kind, w, line)
			}
		}
	}
}

// TestLokiRendersACodeScanningAnalysis pins the sentence of the one rendering
// whose verb was spelled the British way. A LogQL line filter is an exact
// substring match, so the spelling is part of what a reader greps for, and it
// is the same US spelling the rest of the repository uses.
func TestLokiRendersACodeScanningAnalysis(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_code_scanning_analysis",
			Tags: map[string]string{
				"full_name": "o/r", "tool": "CodeQL", "version": "2.20.0",
				"ref": "main", "category": "/language:go",
			},
			Fields: map[string]any{"analyses": 1, "results": 3, "rules": 40, "commit": "abc123"},
			Time:   time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.Streams) != 1 || len(got.Streams[0].Values) != 1 {
		t.Fatalf("expected one code_scanning entry, got %+v", got.Streams)
	}
	if kind := got.Streams[0].Stream["kind"]; kind != "code_scanning" {
		t.Errorf("kind = %q, want code_scanning", kind)
	}
	line := got.Streams[0].Values[0][1]
	if !strings.HasPrefix(line, "CodeQL analyzed o/r (main): 3 results") {
		t.Errorf("the code scanning line does not read as its sentence: %q", line)
	}
}

// TestLokiRendersARulesetVersion pins the sentence of the one dated event that
// is a change to a protection rather than to code: the line names the ruleset
// and the repository, and the actor the way GitHub names it, by type and id.
func TestLokiRendersARulesetVersion(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_ruleset_version",
			Tags: map[string]string{
				"full_name": "o/r", "ruleset": "Protect main", "target": "branch",
				"actor_type": "user",
			},
			Fields: map[string]any{
				"versions": 1, "version_id": int64(48940500), "ruleset_id": int64(21),
				"actor_id": int64(583231),
			},
			Time: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.Streams) != 1 || len(got.Streams[0].Values) != 1 {
		t.Fatalf("expected one ruleset entry, got %+v", got.Streams)
	}
	if kind := got.Streams[0].Stream["kind"]; kind != "ruleset" {
		t.Errorf("kind = %q, want ruleset", kind)
	}
	line := got.Streams[0].Values[0][1]
	if !strings.HasPrefix(line, "ruleset Protect main of o/r saved by user 583231") {
		t.Errorf("the ruleset line does not read as its sentence: %q", line)
	}
	if !strings.Contains(line, "version_id=48940500") {
		t.Errorf("the ruleset line does not carry the version in its tail: %q", line)
	}
}

// TestLokiReadsTheDemotedStatesFromFields pins the sentences whose state
// moved from a tag to a field: an alert's state, whether a discussion has its
// answer, the branch a repository activity touched, and a review's state.
// Each was read with tagOf, which answers the empty string for a field, so the
// line would have said "state " and "answered " with nothing after them.
func TestLokiReadsTheDemotedStatesFromFields(t *testing.T) {
	var got lokiPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now()
	err := NewLoki(srv.URL, "", nil, 0, 0, 0).Write(context.Background(), []Point{
		{
			Measurement: "gh_dependabot_alert_item",
			Tags:        map[string]string{"severity": "high", "package": "vite", "full_name": "o/r", "ecosystem": "npm"},
			Fields:      map[string]any{"alerts": 1, "alert_state": "fixed"},
			Time:        now,
		},
		{
			Measurement: "gh_discussion",
			Tags:        map[string]string{"full_name": "o/r", "category": "Q&A"},
			Fields:      map[string]any{"comments": 1, "has_answer": true},
			Time:        now.Add(time.Second),
		},
		{
			Measurement: "gh_repo_activity",
			Tags:        map[string]string{"activity": "force_push", "full_name": "o/r", "actor": "octocat"},
			Fields:      map[string]any{"events": 1, "ref_name": "feature/x"},
			Time:        now.Add(2 * time.Second),
		},
		{
			Measurement: "gh_pull_request_review",
			Tags:        map[string]string{"reviewer": "bob", "full_name": "o/r", "number": "42"},
			Fields:      map[string]any{"reviews": 1, "review_state": "DISMISSED"},
			Time:        now.Add(3 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	var lines []string
	for _, s := range got.Streams {
		for _, v := range s.Values {
			lines = append(lines, v[1])
		}
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"high alert on vite in o/r (npm), state fixed",
		"discussion in o/r (Q&A), answered true",
		"force_push feature/x on o/r by octocat",
		"bob reviewed o/r#42: dismissed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no line reads %q in:\n%s", want, joined)
		}
	}
}
