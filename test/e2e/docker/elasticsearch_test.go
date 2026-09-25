//go:build dockere2e

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// What a real Elasticsearch does with what the sink writes.
//
// The capture-server suite in test/e2e proves the bulk body: one action line
// and one document line per point, the tags and fields flattened, the time as
// @timestamp. None of that says whether the cluster indexed any of it, and the
// question this suite exists for cannot be asked of a capture server at all:
// the sink writes no mapping, so what a field can be aggregated on is decided
// by Elasticsearch's dynamic mapping, and the dashboards were written against
// an assumption about it that nothing had ever checked.

// esIndex is where one measurement lands: the sink lowercases
// <prefix>-<measurement>, because an index name may not carry an upper-case
// letter.
func esIndex(s *Stack, measurement string) string {
	return strings.ToLower(s.ElasticsearchPrefix + "-" + measurement)
}

// esAll is the index pattern the Grafana datasource is provisioned with, and
// therefore the one the dashboards' queries actually run against.
func esAll(s *Stack) string { return s.ElasticsearchPrefix + "-*" }

func TestElasticsearchAcceptsTheBulkTheSinkWrites(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	esRefresh(ctx, t, s)

	t.Run("the cluster refused nothing", func(t *testing.T) {
		// A bulk request answers 200 even when every item failed, so the sink
		// reads the verdict per item and reports it. Either of these lines in
		// the log means documents were lost.
		for _, complaint := range []string{"sink rejected some lines", "sink write failed"} {
			if strings.Contains(sweep.Log, complaint) {
				t.Errorf("the sweep reported %q:\n%s", complaint, tail(sweep.Log))
			}
		}
	})

	t.Run("every point the sink kept became a document", func(t *testing.T) {
		counts := esCountByMeasurement(ctx, t, s)
		for measurement, points := range pushPointsByMeasurement(t, sweep) {
			want := esExpectedDocuments(points)
			if want == 0 {
				continue // nothing in this measurement carries a usable field
			}
			if got := counts[measurement]; got != want {
				t.Errorf("%s holds %d documents, want %d", measurement, got, want)
			}
		}
	})
}

func TestElasticsearchKeepsTheDateOfTheEvent(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	esRefresh(ctx, t, s)

	t.Run("a dated event keeps the day it happened", func(t *testing.T) {
		doc := esOne(ctx, t, s, "gh_star", esTerm("user.keyword", "alice"))
		esWantTime(t, doc, starGivenAt)
		esWantString(t, doc, "full_name", "octocat/hello-world")
		esWantString(t, doc, "url", "https://github.com/octocat/hello-world/stargazers")
		esWantString(t, doc, "user_url", "https://github.com/alice")
		esWantNumber(t, doc, "starred", 1)
	})

	t.Run("a daily snapshot keeps its own day", func(t *testing.T) {
		doc := esOne(ctx, t, s, "gh_traffic",
			esTerm("kind.keyword", "views"), esAt(trafficDayAt))
		esWantNumber(t, doc, "count", 120)
		esWantNumber(t, doc, "uniques", 18)
	})

	t.Run("a run keeps the moment it finished", func(t *testing.T) {
		doc := esOne(ctx, t, s, "gh_workflow_run", esTerm("conclusion.keyword", "success"))
		esWantTime(t, doc, runFinishedAt)
		esWantNumber(t, doc, "duration_seconds", 220)
		if ok, is := doc["success"].(bool); !is || !ok {
			t.Errorf("success = %#v, want the boolean true", doc["success"])
		}
	})

	t.Run("a day of the star history keeps its own day", func(t *testing.T) {
		// One document per repository and day, and exactly one: the id is
		// the tags and the time, so the thirty weeks every sweep rewrites
		// overwrite their own documents rather than adding a copy that the
		// sum in Stars gained over time would count again.
		for _, want := range starDays(t, pushPoints(t, sweep)) {
			doc := esOne(ctx, t, s, "gh_star_day",
				esTerm("full_name.keyword", want.Tags["full_name"]), esAt(want.Time))
			esWantNumber(t, doc, "stars", starCount(want))
			esWantString(t, doc, "owner", want.Tags["owner"])
			esWantString(t, doc, "repo", want.Tags["repo"])
			if !atMidnight(esTimeOf(t, doc)) {
				t.Errorf("a day of %s is stamped %s, not at the start of the day",
					want.Tags["full_name"], esTimeOf(t, doc).Format(time.RFC3339Nano))
			}
		}
	})

	t.Run("a current-state gauge is stamped when the sweep looked", func(t *testing.T) {
		doc := esNewest(ctx, t, s, "gh_repo")
		esWantNumber(t, doc, "stars", 80)
		esWantString(t, doc, "language", "Go")
		at := esTimeOf(t, doc)
		if at.Before(sweep.Started) || at.After(sweep.Finished) {
			t.Errorf("gh_repo is stamped %s, outside the sweep (%s to %s)",
				at.Format(time.RFC3339Nano), sweep.Started.Format(time.RFC3339),
				sweep.Finished.Format(time.RFC3339))
		}
	})

	t.Run("nothing dated was restamped with the sweep's clock", func(t *testing.T) {
		// The failure this is written against is a store that stamps a
		// document when it receives it. It would put every one of these
		// inside the sweep window, and every one of them belongs before it.
		for _, measurement := range []string{"gh_star", "gh_star_day", "gh_traffic", "gh_workflow_run"} {
			body := map[string]any{"size": 0, "query": map[string]any{
				"range": map[string]any{"@timestamp": map[string]any{
					"gte": sweep.Started.Format(time.RFC3339),
				}},
			}}
			if n := esCount(ctx, t, s, esIndex(s, measurement), body); n != 0 {
				t.Errorf("%s holds %d documents stamped inside the sweep window", measurement, n)
			}
		}
	})
}

func TestElasticsearchDynamicMappingProducesWhatTheDashboardsAggregateOn(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	esRefresh(ctx, t, s)
	mappings := esMappings(ctx, t, s)

	// Every panel that groups by a tag aggregates on <tag>.keyword, because a
	// dynamically mapped string is analyzed text and text cannot be
	// aggregated. This is that assumption, checked against every tag the
	// sweep actually wrote rather than against a sample.
	for measurement, points := range pushPointsByMeasurement(t, sweep) {
		properties := mappings[esIndex(s, measurement)]
		if properties == nil {
			continue // nothing of this measurement was a document
		}
		for tag := range esTagsOf(points) {
			esWantKeyword(t, measurement, tag, properties[tag])
		}
	}

	t.Run("@timestamp is a date", func(t *testing.T) {
		// The datasource is configured with @timestamp as its time field, so
		// a mapping that made it a string would leave every panel empty.
		for index, properties := range mappings {
			if kind, _ := properties["@timestamp"].(map[string]any)["type"].(string); kind != "date" {
				t.Errorf("%s maps @timestamp as %q, want date", index, kind)
			}
		}
	})
}

// TestElasticsearchAnswersTheAggregationsTheDashboardsAsk is the point of this
// file. Every other store in this suite is asked whether it kept the data; the
// Elasticsearch dashboard additionally depends on what dynamic mapping decided
// each field can be used for, and two audits have recorded the answer as
// unverifiable for want of a real cluster. Here is one.
func TestElasticsearchAnswersTheAggregationsTheDashboardsAsk(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	pushSweepRun(ctx, t, s)
	esRefresh(ctx, t, s)

	t.Run("a terms bucket on a tag returns its values", func(t *testing.T) {
		buckets := esTerms(ctx, t, s, esIndex(s, "gh_social_account"), "provider.keyword", nil)
		if got := slices.Sorted(maps.Keys(buckets)); !slices.Equal(got, []string{"linkedin", "mastodon"}) {
			t.Errorf("provider.keyword aggregated to %v, want both providers of the fixture", got)
		}
	})

	t.Run("a top_metrics on a number returns the newest value", func(t *testing.T) {
		got, err := esTopMetric(ctx, s, esIndex(s, "gh_repo"), "stars")
		if err != nil {
			t.Fatalf("the newest stars reading: %v", err)
		}
		if got != float64(80) {
			t.Errorf("top_metrics stars = %#v, want 80", got)
		}
	})

	// The Social accounts panel of the Elasticsearch dashboard is
	//
	//	terms provider.keyword -> top_metrics { metrics: [url.keyword], orderBy: @timestamp }
	//
	// through the keyword sub-field, and this is why. `url` is a plain string,
	// which dynamic mapping makes `text`; aggregating text needs fielddata,
	// which is off by default. Until an end to end run against a real cluster
	// measured it, the panel asked for the text field and did not merely lose
	// its URL column, it returned no rows at all: 400 against the one index,
	// and 200 with 24 of 64 shards failed and no buckets against
	// `ghchronicle-*`, which is the pattern the datasource is provisioned with
	// and therefore what the panel really sends. Both shapes were measured on
	// Elasticsearch 9.5.3.
	//
	// The panel is fixed, so what is asserted now is that it stays fixed, and
	// that the reason it was broken is still the reason: the text field is
	// still refused, and the keyword one still answers.
	t.Run("a top_metrics on the text url field is still refused", func(t *testing.T) {
		_, err := esTopMetric(ctx, s, esIndex(s, "gh_social_account"), "url")
		if err == nil {
			t.Fatal("top_metrics on the text url field was answered; " +
				"this cluster no longer needs the keyword sub-field and the panel could be simplified")
		}
		if !strings.Contains(err.Error(), "Fielddata is disabled on [url]") {
			t.Errorf("top_metrics on url failed for a different reason than the mapping: %v", err)
		}
	})

	t.Run("the same top_metrics on url.keyword returns the url", func(t *testing.T) {
		got, err := esTopMetric(ctx, s, esIndex(s, "gh_social_account"), "url.keyword")
		if err != nil {
			t.Fatalf("top_metrics on url.keyword: %v", err)
		}
		if got != "https://mastodon.social/@octocat" && got != "https://www.linkedin.com/in/octocat" {
			t.Errorf("top_metrics url.keyword = %#v, want one of the fixture's two accounts", got)
		}
		esWantThePanelAnswers(ctx, t, s)
	})

	t.Run("every field the committed dashboard aggregates is one the cluster answers", func(t *testing.T) {
		esScanCommittedDashboard(ctx, t, s)
	})
}

// esScanCommittedDashboard runs every field aggregation the committed
// Elasticsearch dashboard asks for against the cluster the sweep just loaded,
// and reports the ones the cluster refuses.
//
// It reads dashboards/ghchronicle-elasticsearch.json rather than the
// specification that generates it, for two reasons: the specification lives in
// internal/dashboards, which nothing outside cmd/ may import, and the JSON
// is what Grafana actually loads.
func esScanCommittedDashboard(ctx context.Context, t *testing.T, s *Stack) {
	t.Helper()
	fields := esDashboardFields(t)
	if len(fields) < 50 {
		t.Fatalf("only %d aggregated fields were found in the committed dashboard, which cannot be right", len(fields))
	}
	var refused []string
	for _, f := range fields {
		if err := esTryAggregation(ctx, s, esAll(s), f); err != nil {
			refused = append(refused, f.agg+" on "+f.field)
			t.Logf("%s on %s: %v", f.agg, f.field, err)
		}
	}
	sort.Strings(refused)
	// Empty, and it stays empty. Every aggregated field of the committed
	// dashboard is asked of the live cluster, so a new panel that aggregates
	// a text field fails here rather than coming back blank in front of a
	// reader. The Social accounts panel is how that used to happen.
	if len(refused) != 0 {
		t.Errorf("the cluster refuses these dashboard aggregations: %v, want none", refused)
	}
}

// esWantThePanelAnswers reproduces the Social accounts panel as the datasource
// sends it, over the index pattern rather than over one index, and fails unless
// it comes back whole.
//
// The panel buckets by provider and then by url, and takes a max of the numeric
// field, because neither of the two ways of asking for the url as a metric
// works: the text field has no fielddata, and the keyword one panics Grafana's
// plugin, which converts a top_metrics value to a float without looking.
//
// The pattern is the shape that matters. Asked of the single index a broken
// aggregation fails with 400, which is loud; asked of `ghchronicle-*`, which is
// what the datasource is provisioned with, the same break answers 200 with some
// shards failed and no buckets, which is silent and is how this panel was blank
// without anyone noticing.
func esWantThePanelAnswers(ctx context.Context, t *testing.T, s *Stack) {
	t.Helper()
	body := map[string]any{"size": 0, "aggs": map[string]any{
		"by": map[string]any{
			"terms": map[string]any{"field": "provider.keyword", "size": 20},
			"aggs": map[string]any{
				"url": map[string]any{
					"terms": map[string]any{"field": "url.keyword", "size": 20},
					"aggs":  map[string]any{"present": map[string]any{"max": map[string]any{"field": "present"}}},
				},
			},
		},
	}}
	answer, err := esRun(ctx, s, esAll(s), body)
	if err != nil {
		t.Fatalf("the panel's own query over %s: %v", esAll(s), err)
	}
	if answer.Shards.Failed != 0 {
		t.Errorf("%d of %d shards of %s could not answer the panel's aggregation",
			answer.Shards.Failed, answer.Shards.Total, esAll(s))
	}
	var aggs struct {
		By struct {
			Buckets []any `json:"buckets"`
		} `json:"by"`
	}
	if decodeErr := json.Unmarshal(answer.Aggregations, &aggs); decodeErr != nil {
		t.Fatalf("the aggregation does not decode: %v", decodeErr)
	}
	if len(aggs.By.Buckets) != 2 {
		t.Errorf("the panel returned %d rows over %s, want the fixture's two accounts",
			len(aggs.By.Buckets), esAll(s))
	}
}

// ── Reading the cluster back ────────────────────────────────────────────────

// esRefresh makes the sweep's documents searchable. A bulk request that
// answered 200 is not yet a search result: Elasticsearch refreshes on its own
// once a second, and "once a second" is not "before the next statement".
func esRefresh(ctx context.Context, tb testing.TB, s *Stack) {
	tb.Helper()
	if err := storeJSON(ctx, http.MethodPost, s.ElasticsearchURL+"/"+esAll(s)+"/_refresh", nil, nil); err != nil {
		tb.Fatalf("refreshing the indices: %v", err)
	}
}

// esSearch posts one search and decodes the answer.
func esSearch(ctx context.Context, s *Stack, index string, body map[string]any, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return storeJSON(ctx, http.MethodPost, s.ElasticsearchURL+"/"+index+"/_search", raw, into)
}

// esQuery is one clause of a bool query: the assertions here narrow by a term
// and sometimes by an exact timestamp.
type esQuery map[string]any

func esTerm(field, value string) esQuery {
	return esQuery{"term": map[string]any{field: value}}
}

func esAt(stamp string) esQuery {
	return esQuery{"term": map[string]any{"@timestamp": stamp}}
}

// esOne returns the single document matching the filters, and fails when there
// is not exactly one: a store holding two of something the fixtures describe
// once is as wrong an answer as holding none.
func esOne(ctx context.Context, t *testing.T, s *Stack, measurement string, filters ...esQuery) map[string]any {
	t.Helper()
	must := make([]any, 0, len(filters))
	for _, f := range filters {
		must = append(must, map[string]any(f))
	}
	var out struct {
		Hits struct {
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	body := map[string]any{
		"size":  10,
		"query": map[string]any{"bool": map[string]any{"filter": must}},
	}
	if err := esSearch(ctx, s, esIndex(s, measurement), body, &out); err != nil {
		t.Fatalf("searching %s: %v", measurement, err)
	}
	if len(out.Hits.Hits) != 1 {
		t.Fatalf("%s matched %d documents for %v, want exactly one", measurement, len(out.Hits.Hits), filters)
	}
	return out.Hits.Hits[0].Source
}

// esNewest is the most recent document of a measurement, which is how a
// current-state gauge is read back: a stack kept between runs holds the gauge
// rows of earlier sweeps too, each stamped inside its own.
func esNewest(ctx context.Context, t *testing.T, s *Stack, measurement string) map[string]any {
	t.Helper()
	var out struct {
		Hits struct {
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	body := map[string]any{
		"size": 1,
		"sort": []any{map[string]any{"@timestamp": "desc"}},
	}
	if err := esSearch(ctx, s, esIndex(s, measurement), body, &out); err != nil {
		t.Fatalf("searching %s: %v", measurement, err)
	}
	if len(out.Hits.Hits) == 0 {
		t.Fatalf("%s holds no document at all", measurement)
	}
	return out.Hits.Hits[0].Source
}

// esCount is how many documents match, without fetching any of them.
func esCount(ctx context.Context, t *testing.T, s *Stack, index string, body map[string]any) int {
	t.Helper()
	var out struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	if err := esSearch(ctx, s, index, body, &out); err != nil {
		t.Fatalf("counting %s: %v", index, err)
	}
	return out.Hits.Total.Value
}

// esCountByMeasurement reads every index's document count in one query.
func esCountByMeasurement(ctx context.Context, t *testing.T, s *Stack) map[string]int {
	t.Helper()
	body := map[string]any{"size": 0, "aggs": map[string]any{
		"by": map[string]any{"terms": map[string]any{
			"field": "measurement.keyword", "size": 500,
		}},
	}}
	return esTerms(ctx, t, s, esAll(s), "measurement.keyword", body)
}

// esTerms runs a terms aggregation and returns each bucket's document count.
func esTerms(ctx context.Context, t *testing.T, s *Stack, index, field string, body map[string]any) map[string]int {
	t.Helper()
	if body == nil {
		body = map[string]any{"size": 0, "aggs": map[string]any{
			"by": map[string]any{"terms": map[string]any{"field": field, "size": 500}},
		}}
	}
	var out struct {
		Aggregations struct {
			By struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount int    `json:"doc_count"`
				} `json:"buckets"`
			} `json:"by"`
		} `json:"aggregations"`
	}
	if err := esSearch(ctx, s, index, body, &out); err != nil {
		t.Fatalf("aggregating %s on %s: %v", index, field, err)
	}
	counts := make(map[string]int, len(out.Aggregations.By.Buckets))
	for _, b := range out.Aggregations.By.Buckets {
		counts[b.Key] = b.DocCount
	}
	return counts
}

// esAnswer is the part of a search response that says whether the whole of it
// was answered.
//
// Reading _shards is not pedantry here, it is the difference between the two
// ways this cluster refuses an aggregation. Asked of one index whose mapping
// cannot serve it, the search fails with 400 and nothing comes back. Asked of
// an index pattern, which is what the Grafana datasource is provisioned with,
// the shards that can answer do, the ones that cannot are listed under
// _shards.failures, and the status is 200. A caller that only checked the
// status would call that second case a success and then assert on an empty
// aggregation, which is exactly the mistake this file exists to correct.
type esAnswer struct {
	Shards struct {
		Total      int `json:"total"`
		Successful int `json:"successful"`
		Failed     int `json:"failed"`
		Failures   []struct {
			Index  string `json:"index"`
			Reason struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"reason"`
		} `json:"failures"`
	} `json:"_shards"`
	Aggregations json.RawMessage `json:"aggregations"`
}

// esRun posts one search and reports both halves of the verdict.
func esRun(ctx context.Context, s *Stack, index string, body map[string]any) (*esAnswer, error) {
	var out esAnswer
	if err := esSearch(ctx, s, index, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// esTopMetricBody is the aggregation every snapshot panel of the Elasticsearch
// dashboard is built on: the newest document's value of one field.
func esTopMetricBody(field string) map[string]any {
	return map[string]any{"size": 0, "aggs": map[string]any{
		"newest": map[string]any{"top_metrics": map[string]any{
			"metrics": []any{map[string]any{"field": field}},
			"size":    1,
			"sort":    map[string]any{"@timestamp": "desc"},
		}},
	}}
}

// esTopMetric returns the value that aggregation produces, and an error when
// the cluster would not produce one.
func esTopMetric(ctx context.Context, s *Stack, index, field string) (any, error) {
	answer, err := esRun(ctx, s, index, esTopMetricBody(field))
	if err != nil {
		return nil, err
	}
	if shardErr := esShardError(answer); shardErr != nil {
		return nil, shardErr
	}
	var aggs struct {
		Newest struct {
			Top []struct {
				Metrics map[string]any `json:"metrics"`
			} `json:"top"`
		} `json:"newest"`
	}
	if decodeErr := json.Unmarshal(answer.Aggregations, &aggs); decodeErr != nil {
		return nil, decodeErr
	}
	if len(aggs.Newest.Top) == 0 {
		return nil, errors.New("top_metrics matched no document")
	}
	return aggs.Newest.Top[0].Metrics[field], nil
}

// esShardError turns a partial failure into the error it is.
func esShardError(answer *esAnswer) error {
	if answer.Shards.Failed == 0 {
		return nil
	}
	first := answer.Shards.Failures[0]
	return fmt.Errorf("%d of %d shards failed, the first in %s: %s",
		answer.Shards.Failed, answer.Shards.Total, first.Index, first.Reason.Reason)
}

// esMappings is every index's dynamically created mapping, index to field to
// its mapping object.
func esMappings(ctx context.Context, t *testing.T, s *Stack) map[string]map[string]any {
	t.Helper()
	var raw map[string]struct {
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	url := s.ElasticsearchURL + "/" + esAll(s) + "/_mapping"
	if err := storeJSON(ctx, http.MethodGet, url, nil, &raw); err != nil {
		t.Fatalf("reading the mappings: %v", err)
	}
	out := make(map[string]map[string]any, len(raw))
	for index, m := range raw {
		out[index] = m.Mappings.Properties
	}
	return out
}

// ── What the oracle says the cluster should hold ────────────────────────────

// esExpectedDocuments is how many documents a measurement's points make.
//
// It is not simply the number of points, and both differences are the sink's
// own rules. A point whose every field is empty is not a document, the same
// rule the line protocol applies to a point with no field. And the document id
// is the measurement, the tags that are set and the timestamp, with the action
// "index" rather than "create", so two points that agree on all three are one
// document that was written twice: writing the same fourteen-day traffic
// window every six hours has to replace fourteen documents, not add fourteen.
func esExpectedDocuments(points []sqlStoresPoint) int {
	ids := map[string]bool{}
	for _, p := range points {
		if !esHasUsableField(p) {
			continue
		}
		ids[p.Measurement+"|"+esTagKey(p.Tags)+"|"+p.Time] = true
	}
	return len(ids)
}

// esHasUsableField mirrors the sink's rule for what makes a document: a tag of
// the same name wins, nothing empty counts, and a point with nothing left is
// not written at all.
func esHasUsableField(p sqlStoresPoint) bool {
	for k, v := range p.Fields {
		if _, clash := p.Tags[k]; clash {
			continue
		}
		switch value := v.(type) {
		case nil:
		case string:
			if value != "" {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func esTagKey(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(tags[k])
		b.WriteByte(',')
	}
	return b.String()
}

// esTagsOf is every tag key a measurement's points carry.
func esTagsOf(points []sqlStoresPoint) map[string]bool {
	out := map[string]bool{}
	for _, p := range points {
		for k := range p.Tags {
			out[k] = true
		}
	}
	return out
}

// ── The committed dashboard's own field list ────────────────────────────────

// esField is one field aggregation a panel asks for.
type esField struct {
	agg   string
	field string
}

// esDashboardFields reads every field aggregation out of the committed
// Elasticsearch dashboard: the terms buckets panels group by, the metrics they
// compute, and the top_metrics fields the snapshot tables read.
func esDashboardFields(t *testing.T) []esField {
	t.Helper()
	path := filepath.Join("..", "..", "..", "dashboards", "ghchronicle-elasticsearch.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the committed dashboard: %v", err)
	}
	var doc any
	if decodeErr := json.Unmarshal(raw, &doc); decodeErr != nil {
		t.Fatalf("the committed dashboard is not JSON: %v", decodeErr)
	}
	seen := map[esField]bool{}
	esWalkAggregations(doc, seen)
	out := make([]esField, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].agg != out[j].agg {
			return out[i].agg < out[j].agg
		}
		return out[i].field < out[j].field
	})
	return out
}

// esWalkAggregations collects every aggregation that names a field. Grafana's
// Elasticsearch targets are nested arrays of objects with a `type` and either
// a `field` or, for top_metrics, a list of fields under `settings`.
func esWalkAggregations(node any, into map[esField]bool) {
	switch n := node.(type) {
	case map[string]any:
		esCollectAggregation(n, into)
		for _, v := range n {
			esWalkAggregations(v, into)
		}
	case []any:
		for _, v := range n {
			esWalkAggregations(v, into)
		}
	}
}

func esCollectAggregation(n map[string]any, into map[esField]bool) {
	kind, _ := n["type"].(string)
	if kind == "top_metrics" {
		settings, _ := n["settings"].(map[string]any)
		list, _ := settings["metrics"].([]any)
		for _, f := range list {
			if name, ok := f.(string); ok {
				into[esField{"top_metrics", name}] = true
			}
		}
		return
	}
	field, ok := n["field"].(string)
	if !ok || field == "@timestamp" {
		return
	}
	switch kind {
	case "terms", "sum", "avg", "max", "min", "cardinality", "percentiles":
		into[esField{kind, field}] = true
	}
}

// esTryAggregation runs one field aggregation and reports whether the cluster
// answers it. A field no index carries is not a refusal: the aggregation is
// answered with nothing, which is what a panel for a family this account has
// none of does anyway.
func esTryAggregation(ctx context.Context, s *Stack, index string, f esField) error {
	var agg map[string]any
	switch f.agg {
	case "top_metrics":
		agg = map[string]any{"top_metrics": map[string]any{
			"metrics": []any{map[string]any{"field": f.field}},
			"size":    1,
			"sort":    map[string]any{"@timestamp": "desc"},
		}}
	case "terms":
		agg = map[string]any{"terms": map[string]any{"field": f.field, "size": 5}}
	case "percentiles":
		agg = map[string]any{"percentiles": map[string]any{"field": f.field}}
	default:
		agg = map[string]any{f.agg: map[string]any{"field": f.field}}
	}
	answer, err := esRun(ctx, s, index, map[string]any{"size": 0, "aggs": map[string]any{"probe": agg}})
	if err != nil {
		return err
	}
	return esShardError(answer)
}

// ── Assertions on one document ──────────────────────────────────────────────

func esTimeOf(t *testing.T, doc map[string]any) time.Time {
	t.Helper()
	raw, ok := doc["@timestamp"].(string)
	if !ok {
		t.Fatalf("the document carries no @timestamp: %v", doc)
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("@timestamp %q: %v", raw, err)
	}
	return at
}

func esWantTime(t *testing.T, doc map[string]any, want string) {
	t.Helper()
	if got := esTimeOf(t, doc).UTC().Format(time.RFC3339); got != want {
		t.Errorf("the document is stamped %s, want the event's own %s", got, want)
	}
}

func esWantString(t *testing.T, doc map[string]any, field, want string) {
	t.Helper()
	if got, _ := doc[field].(string); got != want {
		t.Errorf("%s = %#v, want %q", field, doc[field], want)
	}
}

func esWantNumber(t *testing.T, doc map[string]any, field string, want float64) {
	t.Helper()
	got, ok := doc[field].(float64)
	if !ok {
		t.Fatalf("%s = %#v, which is not a number", field, doc[field])
	}
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

// esWantKeyword fails when a tag has no keyword sub-field, which is the one
// thing every terms bucket in the dashboard depends on.
func esWantKeyword(t *testing.T, measurement, tag string, mapping any) {
	t.Helper()
	m, ok := mapping.(map[string]any)
	if !ok {
		t.Errorf("%s.%s is in no mapping at all", measurement, tag)
		return
	}
	if kind, _ := m["type"].(string); kind != "text" {
		t.Errorf("%s.%s is mapped as %q, want text with a keyword sub-field", measurement, tag, kind)
		return
	}
	sub, _ := m["fields"].(map[string]any)
	keyword, _ := sub["keyword"].(map[string]any)
	if kind, _ := keyword["type"].(string); kind != "keyword" {
		t.Errorf("%s.%s has no keyword sub-field, so no panel can group by it", measurement, tag)
	}
}
