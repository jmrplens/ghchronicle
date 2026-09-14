package e2e

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// esPair is one action and its document, as the _bulk body carries them.
type esPair struct {
	Index string
	ID    string
	Doc   map[string]any
}

var esDocID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// bulkReply is what a cluster answers when every item was accepted. The sink
// reads the per-item verdicts, so the body has to be a real one.
func bulkReply([]byte) (status int, body string) {
	return http.StatusOK, `{"took":1,"errors":false,"items":[]}`
}

// parseBulk reads the newline delimited action and document pairs. An odd
// number of lines, or a line that is not JSON, fails the test: Elasticsearch
// would refuse the whole request.
func parseBulk(t *testing.T, body string) []esPair {
	t.Helper()
	var lines []string
	for l := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines)%2 != 0 {
		t.Fatalf("the bulk body has %d lines, so an action or a document is missing", len(lines))
	}
	out := make([]esPair, 0, len(lines)/2)
	for i := 0; i < len(lines); i += 2 {
		var action struct {
			Index struct {
				Index string `json:"_index"`
				ID    string `json:"_id"`
			} `json:"index"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &action); err != nil {
			t.Fatalf("action line does not parse: %v\n%s", err, lines[i])
		}
		// "index" rather than "create": rewriting the traffic window has to
		// replace the documents instead of adding more.
		if action.Index.Index == "" {
			t.Fatalf("action line names no index or is not an index action: %s", lines[i])
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(lines[i+1]), &doc); err != nil {
			t.Fatalf("document line does not parse: %v\n%s", err, lines[i+1])
		}
		out = append(out, esPair{Index: action.Index.Index, ID: action.Index.ID, Doc: doc})
	}
	return out
}

// esIdentity is what a document is, apart from its id: the measurement, the
// tags that are set and the timestamp. Two runs that produce the same identity
// must produce the same id, or a rewrite duplicates instead of replacing.
func esIdentity(p esPair) string {
	keys := make([]string, 0, len(p.Doc))
	for k, v := range p.Doc {
		if s, ok := v.(string); ok {
			keys = append(keys, k+"="+s)
		}
	}
	sort.Strings(keys)
	return p.Index + "|" + strings.Join(keys, ",")
}

func runElasticsearchSweep(t *testing.T) *capture {
	t.Helper()
	gh := newFakeGitHub(t)
	rec := newCapture(t, bulkReply)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), `  elasticsearch:
    url: `+rec.URL()+`
    api_key: es-api-key`)
	sweepOnce(t, cfg)
	return rec
}

func TestElasticsearchSinkWritesBulkDocuments(t *testing.T) {
	t.Parallel()
	rec := runElasticsearchSweep(t)

	reqs := rec.Accepted()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the _bulk endpoint")
	}
	for _, r := range reqs {
		if r.Method != http.MethodPost || r.Path != "/_bulk" {
			t.Errorf("%s %s, want POST /_bulk", r.Method, r.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-ndjson" {
			t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
		}
		if got := r.Header.Get("Authorization"); got != "ApiKey es-api-key" {
			t.Errorf("Authorization = %q, want the configured API key", got)
		}
	}

	pairs := parseBulk(t, rec.Body())
	if len(pairs) < 100 {
		t.Fatalf("only %d documents arrived", len(pairs))
	}
	indices := map[string]int{}
	for _, p := range pairs {
		indices[p.Index]++
		if !esDocID.MatchString(p.ID) {
			t.Errorf("document id %q is not a SHA-256 digest", p.ID)
		}
		measurement, _ := p.Doc["measurement"].(string)
		// One index per measurement, named after it, and lowercase because an
		// index name may not carry an upper-case letter.
		if want := "ghchronicle-" + measurement; p.Index != want {
			t.Errorf("document of %s landed in %q, want %q", measurement, p.Index, want)
		}
		if p.Index != strings.ToLower(p.Index) {
			t.Errorf("index %q is not lowercase", p.Index)
		}
		stamp, _ := p.Doc["@timestamp"].(string)
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			t.Errorf("%s: @timestamp %q does not parse: %v", p.Index, stamp, err)
		}
		// Tags and fields are top-level keys, so nothing has to be unnested
		// before it can be filtered on.
		if len(p.Doc) < 3 {
			t.Errorf("%s: document carries only %v", p.Index, sortedNames(p.Doc))
		}
	}
	for _, want := range []string{"ghchronicle-gh_traffic", "ghchronicle-gh_repo", "ghchronicle-gh_workflow_run"} {
		if indices[want] == 0 {
			t.Errorf("no documents in %s; indices seen: %v", want, sortedNames(indices))
		}
	}
}

// TestElasticsearchDocumentIDsAreStableAcrossRuns is what makes a rewrite
// idempotent: the same point written twice has to replace one document rather
// than add a second.
func TestElasticsearchDocumentIDsAreStableAcrossRuns(t *testing.T) {
	t.Parallel()
	first := idsByIdentity(t, runElasticsearchSweep(t))
	second := idsByIdentity(t, runElasticsearchSweep(t))

	shared, differing := 0, 0
	for identity, id := range first {
		other, ok := second[identity]
		if !ok {
			continue // dated at the moment of the run, so not the same point
		}
		shared++
		if other != id {
			differing++
			if differing == 1 {
				t.Errorf("the same point got two ids: %s\n%s\n%s", identity, id, other)
			}
		}
	}
	if shared < 20 {
		t.Fatalf("only %d points were comparable between the two runs", shared)
	}
	if differing > 0 {
		t.Errorf("%d of %d repeated points would have been indexed twice", differing, shared)
	}
}

func idsByIdentity(t *testing.T, rec *capture) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range parseBulk(t, rec.Body()) {
		out[esIdentity(p)] = p.ID
	}
	return out
}
