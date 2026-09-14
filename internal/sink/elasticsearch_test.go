package sink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type esCapture struct {
	path, contentType, auth string
	body                    string
}

func captureES(t *testing.T, reply string, mk func(url string) *Elasticsearch, points []Point) (esCapture, error) {
	t.Helper()
	var c esCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c = esCapture{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(raw)}
		if reply == "" {
			reply = `{"errors":false,"items":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	err := mk(srv.URL).Write(context.Background(), points)
	return c, err
}

func TestElasticsearchBulkFormatAndIdempotentID(t *testing.T) {
	at := time.Unix(0, 1700000000000000000)
	p := Point{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a", "license": ""},
		Fields: map[string]any{"stars": 3, "repo": "clash", "empty": "", "pushed_at": at}, Time: at,
	}
	c, err := captureES(t, "", func(u string) *Elasticsearch { return NewElasticsearch(u, "", "", "", "key123", 0, 0) }, []Point{p})
	if err != nil {
		t.Fatal(err)
	}
	if c.path != "/_bulk" || c.contentType != "application/x-ndjson" || c.auth != "ApiKey key123" {
		t.Errorf("request = %+v", c)
	}
	lines := strings.Split(strings.TrimRight(c.body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("body has %d lines, want an action and a document:\n%s", len(lines), c.body)
	}
	// The id is the identity the line protocol uses: measurement, the tags
	// that are set (the empty license is not one), and the timestamp.
	sum := sha256.Sum256([]byte("gh_repo|repo=a,|1700000000000000000"))
	wantAction := `{"index":{"_index":"ghchronicle-gh_repo","_id":"` + hex.EncodeToString(sum[:]) + `"}}`
	if lines[0] != wantAction {
		t.Errorf("action = %s\nwant     %s", lines[0], wantAction)
	}
	wantDoc := `{"@timestamp":"2023-11-14T22:13:20Z","measurement":"gh_repo","pushed_at":"2023-11-14T22:13:20Z","repo":"a","stars":3}`
	if lines[1] != wantDoc {
		t.Errorf("document = %s\nwant       %s", lines[1], wantDoc)
	}

	// The same point again is the same request, byte for byte: that is
	// what lets a rewrite of the traffic window replace rather than add.
	again, err := captureES(t, "", func(u string) *Elasticsearch { return NewElasticsearch(u, "", "", "", "key123", 0, 0) }, []Point{p})
	if err != nil {
		t.Fatal(err)
	}
	if again.body != c.body {
		t.Errorf("a rewrite must produce the same bulk body")
	}
}

func TestElasticsearchBasicAuthAndBatching(t *testing.T) {
	var auths []string
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		auths = append(auths, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
	}))
	defer srv.Close()
	e := NewElasticsearch(srv.URL+"/", "logs", "elastic", "changeme", "", 2, 0)
	var points []Point
	for i := range 5 {
		points = append(points, Point{
			Measurement: "gh_star", Tags: map[string]string{"repo": "a"},
			Fields: map[string]any{"starred": 1}, Time: time.Unix(int64(i), 0),
		})
	}
	if err := e.Write(context.Background(), points); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Errorf("5 documents in batches of 2 should be 3 requests, got %d", requests)
	}
	if !strings.HasPrefix(auths[0], "Basic ") {
		t.Errorf("authorization = %q, want basic auth", auths[0])
	}
}

func TestElasticsearchReportsItemErrors(t *testing.T) {
	reply := `{"errors":true,"items":[{"index":{"_index":"ghchronicle-gh_repo","status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [stars]"}}}]}`
	var reasons []string
	c, err := captureES(t, reply, func(u string) *Elasticsearch {
		e := NewElasticsearch(u, "", "", "", "", 0, 0)
		e.OnReject = func(r string) { reasons = append(reasons, r) }
		return e
	}, []Point{{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"stars": "three"}, Time: time.Now(),
	}})
	if c.auth != "" {
		t.Errorf("no credentials were configured, got %q", c.auth)
	}
	var rejected *RejectedError
	if !errors.As(err, &rejected) || rejected.N != 1 {
		t.Fatalf("err = %v, want one rejected document", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "mapper_parsing_exception") {
		t.Errorf("reasons = %v", reasons)
	}
}

func TestElasticsearchNamesItselfOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := NewElasticsearch(srv.URL, "", "", "", "", 0, 0).Write(context.Background(), []Point{{
		Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Now(),
	}})
	if err == nil || !strings.HasPrefix(err.Error(), "elasticsearch write: 401") {
		t.Errorf("err = %v", err)
	}
}
