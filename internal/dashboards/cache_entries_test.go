package dashboards

import (
	"fmt"
	"strings"
	"testing"
)

// TestCacheEntriesReadEachRepositorysNewestSnapshot is what checking 2.6.0
// against GitHub panel by panel found in "Cache entries by key". A cache is
// what GitHub holds now, and the table added up each ref's newest row anywhere
// in the range, so every ref GitHub had evicted since still counted, at its
// size on the last day it was listed. Measured on 2026-09-27 over thirty days,
// jmrplens/gitlab-mcp-server's golangci-lint read 128 entries and 9.46 GiB
// over 108 refs, the oldest row from 2026-09-18, where the rows of that day,
// and GitHub's listing of that cache, held 44 entries and 3.08 GiB over 24.
//
// Each store is held to the rows of a repository's newest snapshot and no
// older one, in its own terms: the SQL stores keep the rows at the newest
// timestamp of each repository, which is its newest day since the rows are
// stamped at the start of the UTC day; Elasticsearch buckets each repository
// by its newest timestamp before it sums; Graphite, which cannot find each
// repository's newest day, drops every ref with nothing on the last day of the
// range before it adds, where keepLastValue carried an evicted ref to the end.
// Prometheus holds the value last pushed for each ref and cannot tell an
// evicted one from a current one, so its description says for how long an
// evicted ref still counts there.
func TestCacheEntriesReadEachRepositorysNewestSnapshot(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Cache entries by key", "table")
		targets, _ := p["targets"].([]any)
		if len(targets) == 0 {
			t.Fatalf("%s: Cache entries by key has no query", store.Name)
		}
		switch store.Name {
		case "influxdb", "postgres":
			for _, raw := range targets {
				sql, _ := raw.(map[string]any)["rawSql"].(string)
				if !strings.Contains(sql, "MAX(time) OVER (PARTITION BY full_name) AS newest") ||
					!strings.Contains(sql, ") x WHERE time = newest ") ||
					strings.Contains(sql, "PARTITION BY repo, cache, ref") {
					t.Errorf("%s: Cache entries by key does not keep each repository's newest "+
						"snapshot alone:\n%s", store.Name, sql)
				}
			}
		case "elasticsearch":
			checkNewestTimestampBucket(t, targets[0].(map[string]any))
		case "graphite":
			expr, _ := targets[0].(map[string]any)["target"].(string)
			lastDay := fmt.Sprintf(`filterSeries(summarize(transformNull(%s, -1), "1d", "max"), "last", ">=", 0)`,
				rp("gh_actions_cache_entry", "size_bytes"))
			if strings.Contains(expr, "keepLastValue") || !strings.Contains(expr, lastDay) {
				t.Errorf("graphite: Cache entries by key carries refs with nothing on the range's "+
					"last day, want %s in:\n%s", lastDay, expr)
			}
		case "prometheus":
			if desc, _ := p["description"].(string); !strings.Contains(desc, "until the process restarts") {
				t.Errorf("prometheus: Cache entries by key does not say an evicted ref still counts "+
					"until the OTLP sink's process restarts: %q", desc)
			}
		}
	}
}

// checkNewestTimestampBucket holds the Elasticsearch target of the cache table
// to a bucket per repository, then its one newest timestamp, then the caches
// of that timestamp, which the metrics add up.
func checkNewestTimestampBucket(t *testing.T, target map[string]any) {
	t.Helper()
	buckets, _ := target["bucketAggs"].([]any)
	fields := bucketFieldsOf(buckets)
	if len(buckets) != 4 || fields[0] != panelFullNameField || fields[1] != panelRepoField ||
		fields[2] != "@timestamp" || fields[3] != "cache.keyword" {
		t.Errorf("elasticsearch: Cache entries by key buckets by %v, want the repository by "+
			"its full name, its newest timestamp and the cache", fields)
		return
	}
	settings, _ := buckets[2].(map[string]any)["settings"].(map[string]any)
	if settings["size"] != "1" || settings["orderBy"] != "_key" || settings["order"] != "desc" {
		t.Errorf("elasticsearch: the timestamp bucket is %v, want the one newest", settings)
	}
	metrics, _ := target["metrics"].([]any)
	for _, raw := range metrics {
		if m := raw.(map[string]any); m["type"] == "top_metrics" {
			t.Errorf("elasticsearch: Cache entries by key reads %v, a ref's newest document "+
				"wherever in the range it is", m)
		}
	}
}

// TestNoGraphiteQueryFillsTheDayAfterItsRange: graphite-web 1.1.10 rounds the
// end of what it reads up to the next storage slot, and a summarize not
// aligned to its from ends its buckets on the interval after that end, so a
// range ending in the last UTC hour of a day holds one more daily bucket, the
// next day, with no point in it. A transformNull or a keepLastValue over such a
// summarize fills that bucket like any other, and whatever reads the last
// value then reads the fill. "Cache entries by key" filled it with -1 and
// dropped every ref whose last value was below zero, so for an hour a day it
// answered nothing: measured against graphiteapp/graphite-statsd:1.1.10-5,
// its two refs at 22:30 UTC, and nothing at 23:30 and 23:59. Filled before the
// buckets are made, the bucket after the range has nothing to fill and stays
// null, which every last-value read passes over.
func TestNoGraphiteQueryFillsTheDayAfterItsRange(t *testing.T) {
	t.Parallel()
	summarized := 0
	for _, p := range renderedPanels(t, "graphite") {
		for _, raw := range targetList(p) {
			target, _ := raw.(map[string]any)
			expr, _ := target["target"].(string)
			if expr == "" {
				continue
			}
			parser := &exprParser{t: t, s: expr}
			tree := parseGraphiteCall(parser)
			summarized += strings.Count(expr, "summarize(")
			for _, f := range fillsAfterSummarize(tree, "") {
				t.Errorf("graphite: %q fills the empty bucket after its range with %s: %s", p["title"], f, expr)
			}
		}
	}
	if summarized == 0 {
		t.Fatal("no Graphite query summarizes, so this checked nothing")
	}
	for expr, want := range map[string]int{
		`transformNull(summarize(a.b, "1d", "last"), -1)`:        1,
		`keepLastValue(summarize(a.b, "1h", "max"))`:             1,
		`summarize(transformNull(a.b, -1), "1d", "max")`:         0,
		`keepLastValue(summarize(a.b, "100y", "sum", true))`:     0,
		`sumSeries(keepLastValue(a.b))`:                          0,
		`alias(transformNull(summarize(a.b, "7d", "sum")), "x")`: 1,
	} {
		p := &exprParser{t: t, s: expr}
		if got := fillsAfterSummarize(parseGraphiteCall(p), ""); len(got) != want {
			t.Errorf("%s: found %v, want %d", expr, got, want)
		}
	}
}

// fillsAfterSummarize is every summarize not aligned to its from that a
// transformNull or a keepLastValue above it fills; `filling` is the nearest
// such function on the way down, or empty.
func fillsAfterSummarize(c grCall, filling string) []string {
	var out []string
	if c.fn == "summarize" && filling != "" {
		last := c.args[len(c.args)-1]
		if last.quoted || last.word != "true" {
			interval := ""
			if len(c.args) > 1 {
				interval = c.args[1].word
			}
			out = append(out, fmt.Sprintf("%s over summarize(…, %q)", filling, interval))
		}
	}
	if c.fn == "transformNull" || c.fn == "keepLastValue" {
		filling = c.fn
	}
	for _, a := range c.args {
		out = append(out, fillsAfterSummarize(a, filling)...)
	}
	return out
}
