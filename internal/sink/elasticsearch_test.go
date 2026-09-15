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

// TestNewElasticsearchFillsInOnlyWhatWasLeftOut gives a zero batch, timeout
// and prefix their documented values and keeps every one that was set: the
// batch decides how many requests a sweep costs, and a timeout of zero on an
// http.Client is no timeout at all, so a hung cluster would hold the sweep
// for ever.
func TestNewElasticsearchFillsInOnlyWhatWasLeftOut(t *testing.T) {
	e := NewElasticsearch("http://es.test/", "", "", "", "", 0, 0)
	if e.Batch != 1000 || e.client.Timeout != 60*time.Second || e.Prefix != "ghchronicle" || e.URL != "http://es.test" {
		t.Errorf("defaults = batch %d, timeout %v, prefix %q, URL %q, want 1000, 60s, ghchronicle and no trailing slash",
			e.Batch, e.client.Timeout, e.Prefix, e.URL)
	}
	e = NewElasticsearch("http://es.test", "logs", "", "", "", 7, 3*time.Second)
	if e.Batch != 7 || e.client.Timeout != 3*time.Second || e.Prefix != "logs" {
		t.Errorf("given = batch %d, timeout %v, prefix %q, want 7, 3s and logs kept", e.Batch, e.client.Timeout, e.Prefix)
	}
	if e.Name() != "elasticsearch" || e.Close() != nil {
		t.Errorf("Name = %q, want elasticsearch, and a Close with nothing to close", e.Name())
	}
}

// TestElasticsearchSendsNothingForAPointWithoutAField leaves out a point whose
// every field is empty, zero, nil or of a type no document holds, and makes no
// request at all when that is the whole batch: an empty bulk body is refused
// by the cluster.
func TestElasticsearchSendsNothingForAPointWithoutAField(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
	}))
	defer srv.Close()
	err := NewElasticsearch(srv.URL, "", "", "", "", 0, 0).Write(context.Background(), []Point{{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"none": nil, "blank": "", "never": time.Time{}, "list": []int{1}},
		Time:   time.Unix(1700000000, 0),
	}})
	if err != nil || requests != 0 {
		t.Errorf("Write = %v after %d requests, want nothing sent", err, requests)
	}
}

// TestElasticsearchKeepsTheTypeOfEachNumber writes an int64, a float and a
// boolean as JSON numbers and a boolean, so the index mapping the cluster
// infers is a number where the store has a number.
func TestElasticsearchKeepsTheTypeOfEachNumber(t *testing.T) {
	at := time.Unix(1700000000, 0)
	c, err := captureES(t, "", func(u string) *Elasticsearch { return NewElasticsearch(u, "", "", "", "", 0, 0) }, []Point{{
		Measurement: "gh_repo",
		Fields:      map[string]any{"size": int64(9000000000), "ratio": 0.25, "fork": true},
		Time:        at,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"@timestamp":"2023-11-14T22:13:20Z","fork":true,"measurement":"gh_repo","ratio":0.25,"size":9000000000}`
	if lines := strings.Split(strings.TrimRight(c.body, "\n"), "\n"); len(lines) != 2 || lines[1] != want {
		t.Errorf("body =\n%s\nwant the document %s", c.body, want)
	}
}

// TestElasticsearchSendsTheAPIKeyRatherThanBothCredentials prefers the API key
// when basic auth is configured beside it, which is what the field says.
func TestElasticsearchSendsTheAPIKeyRatherThanBothCredentials(t *testing.T) {
	c, err := captureES(t, "", func(u string) *Elasticsearch {
		return NewElasticsearch(u, "", "elastic", "changeme", "key123", 0, 0)
	}, []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(1, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	if c.auth != "ApiKey key123" {
		t.Errorf("authorization = %q, want the API key alone", c.auth)
	}
}

// TestElasticsearchStopsAtTheFirstBatchThatFails returns the failure of a full
// batch without sending the rest, so a cluster that is down costs one request
// and not one per thousand documents.
func TestElasticsearchStopsAtTheFirstBatchThatFails(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	points := make([]Point, 0, 3)
	for i := range 3 {
		points = append(points, Point{Measurement: "m", Fields: map[string]any{"v": i}, Time: time.Unix(int64(i), 0)})
	}
	err := NewElasticsearch(srv.URL, "", "", "", "", 1, 0).Write(context.Background(), points)
	if err == nil || !strings.HasPrefix(err.Error(), "elasticsearch write: 503") || requests != 1 {
		t.Errorf("Write = %v after %d requests, want the 503 from the first batch alone", err, requests)
	}
}

// TestElasticsearchTreatsAnyNonSuccessStatusAsAFailure includes 300, the first
// status that is not a success, which net/http does not follow as a redirect.
func TestElasticsearchTreatsAnyNonSuccessStatusAsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultipleChoices)
		_, _ = io.WriteString(w, "choose")
	}))
	defer srv.Close()
	err := NewElasticsearch(srv.URL, "", "", "", "", 0, 0).Write(context.Background(), []Point{{
		Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(1, 0),
	}})
	if err == nil || !strings.HasPrefix(err.Error(), "elasticsearch write: 300") {
		t.Errorf("Write = %v, want the 300 reported", err)
	}
}

// TestElasticsearchReadsTheVerdictOfTheBulkResponse counts refused items only
// when the response says there were errors, counts only the items that carry
// one, and takes a body it cannot read as the success its status already
// said. A missing OnReject hook still counts the refusal.
func TestElasticsearchReadsTheVerdictOfTheBulkResponse(t *testing.T) {
	point := []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(1, 0)}}
	item := `{"index":{"_index":"ghchronicle-m","status":400,"error":{"type":"x","reason":"y"}}}`
	ok := `{"index":{"_index":"ghchronicle-m","status":201}}`
	for _, tc := range []struct {
		name, reply string
		rejected    int
	}{
		{"errors false, whatever the items say", `{"errors":false,"items":[` + item + `]}`, 0},
		{"a body that is not JSON", `<html>proxy</html>`, 0},
		{"one refused beside one written", `{"errors":true,"items":[` + ok + `,` + item + `]}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := captureES(t, tc.reply, func(u string) *Elasticsearch { return NewElasticsearch(u, "", "", "", "", 0, 0) }, point)
			var rejected *RejectedError
			switch {
			case tc.rejected == 0 && err != nil:
				t.Errorf("Write = %v, want success", err)
			case tc.rejected > 0 && (!errors.As(err, &rejected) || rejected.N != tc.rejected):
				t.Errorf("Write = %v, want %d rejected", err, tc.rejected)
			}
		})
	}
}

// TestElasticsearchRefusesAnEndpointItCannotUse names the setting rather than
// posting, and reports a cluster that is not there.
func TestElasticsearchRefusesAnEndpointItCannotUse(t *testing.T) {
	point := []Point{{Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Unix(1, 0)}}
	if err := NewElasticsearch("es:9200", "", "", "", "", 0, 0).Write(context.Background(), point); err == nil ||
		!strings.HasPrefix(err.Error(), "elasticsearch write: endpoint ") {
		t.Errorf("Write = %v, want the endpoint refused", err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if err := NewElasticsearch(closed.URL, "", "", "", "", 0, 0).Write(context.Background(), point); err == nil {
		t.Error("Write reported success to a cluster that is not there")
	}
}
