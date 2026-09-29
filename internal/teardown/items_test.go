package teardown

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// TestInfluxDBItemsAsksTheCatalogAndThenTheValues: the copy InfluxDB set
// aside is asked by the name it gave it, first for its columns, since a
// column it lacks fails the whole query with a 500 (measured on 3.11.2),
// and then for the distinct values, which is all it is ever sent.
func TestInfluxDBItemsAsksTheCatalogAndThenTheValues(t *testing.T) {
	t.Parallel()
	a := &asked{}
	const aside = "gh_discussion_comment-20261001T091004"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.note(r, "")
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/ping":
			_, _ = io.WriteString(w, `{"product_name":"InfluxDB 3 Core","version":"3.11.2"}`)
		case q == "SELECT column_name FROM information_schema.columns WHERE table_schema = 'iox' AND table_name = '"+aside+"'":
			_, _ = io.WriteString(w, `[{"column_name":"comment"},{"column_name":"full_name"},{"column_name":"is_answer"}]`)
		case q == `SELECT DISTINCT "full_name" AS v0, "comment" AS v1 FROM "`+aside+`"`:
			_, _ = io.WriteString(w, `[{"v0":"o/r","v1":"2"},{"v0":"o/r","v1":"1"},{"v0":"o/r"}]`)
		case strings.Contains(q, "information_schema.columns"):
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Errorf("an unexpected question: %s", q)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	store := &influx{sink: &config.InfluxSink{URL: srv.URL, Token: "t", Bucket: "github"}}

	items, err := store.Items(t.Context(), aside, []string{"full_name", "comment"})
	if err != nil || !slices.Equal(items, []string{"o/r ", "o/r 1", "o/r 2"}) {
		t.Errorf("items %q, %v", items, err)
	}
	if _, err = store.Items(t.Context(), aside, []string{"number"}); err == nil ||
		err.Error() != aside+" has no number" {
		t.Errorf("a tag the copy lacks: %v", err)
	}
	if _, err = store.Items(t.Context(), "gh_discussion_comment", []string{"comment"}); err == nil ||
		!strings.Contains(err.Error(), "is not there") {
		t.Errorf("a table that is not there: %v", err)
	}
	for _, bad := range []struct{ table, tag string }{
		{"demo_discussion_comment", "comment"}, {`gh_x" ; DROP`, "comment"}, {"gh_x", `comment"`},
	} {
		if _, err = store.Items(t.Context(), bad.table, []string{bad.tag}); err == nil {
			t.Errorf("%q %q was asked about", bad.table, bad.tag)
		}
	}
	a.onlyRead(t)
}

// TestElasticsearchItemsPagesEveryCombination: the copy is refreshed, each
// tag is checked to be there, and a composite aggregation is followed page
// by page until one comes back without an after_key.
func TestElasticsearchItemsPagesEveryCombination(t *testing.T) {
	t.Parallel()
	const copyIndex = "ghchronicle-gh_code_scanning_alert_item-20261001t091004"
	a := &asked{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.note(r, "")
		switch r.URL.Path {
		case "/" + copyIndex + "/_refresh":
			_, _ = io.WriteString(w, `{}`)
		case "/" + copyIndex + "/_count":
			if strings.Contains(string(body), `"number"`) || len(body) == 0 || strings.Contains(string(body), "full_name") {
				_, _ = io.WriteString(w, `{"count":3}`)
				return
			}
			_, _ = io.WriteString(w, `{"count":0}`)
		case "/" + copyIndex + "/_search":
			var q struct {
				Aggs struct {
					Items struct {
						Composite struct {
							Sources []map[string]map[string]map[string]string `json:"sources"`
							After   map[string]any                            `json:"after"`
						} `json:"composite"`
					} `json:"items"`
				} `json:"aggs"`
			}
			if err := json.Unmarshal(body, &q); err != nil {
				t.Fatal(err)
			}
			c := q.Aggs.Items.Composite
			if c.Sources[0]["v0"]["terms"]["field"] != "full_name.keyword" || c.Sources[1]["v1"]["terms"]["field"] != "number.keyword" {
				t.Errorf("the aggregation's sources are %v", c.Sources)
			}
			if c.After == nil {
				_, _ = io.WriteString(w, `{"aggregations":{"items":{"after_key":{"v0":"o/r","v1":"2"},`+
					`"buckets":[{"key":{"v0":"o/r","v1":"1"}},{"key":{"v0":"o/r","v1":"2"}}]}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"aggregations":{"items":{"buckets":[{"key":{"v0":"o/s","v1":"1"}}]}}}`)
		default:
			t.Errorf("an unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	store := &elastic{sink: &config.ElasticsearchSink{URL: srv.URL, Prefix: "ghchronicle"}}
	items, err := store.Items(t.Context(), copyIndex, []string{"full_name", "number"})
	if err != nil || !slices.Equal(items, []string{"o/r 1", "o/r 2", "o/s 1"}) {
		t.Errorf("items %q, %v", items, err)
	}
	if _, err = store.Items(t.Context(), copyIndex, []string{"state"}); err == nil || !strings.Contains(err.Error(), "has no state") {
		t.Errorf("a field no document has: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.seen {
		// A refresh is the one POST: it makes what was written searchable,
		// and changes no document.
		if !strings.HasPrefix(s, http.MethodGet) && !strings.HasPrefix(s, http.MethodPost+" /"+copyIndex+"/_refresh") {
			t.Errorf("reading the items sent %s", s)
		}
	}
}
