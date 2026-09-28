package dashboards

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestDeploymentsByEnvironmentCountsEveryDeploymentInElasticsearch is the
// Elasticsearch half of "Deployments by environment" in the 2.6.1 review: 1
// deployment and a To status of 0 s where InfluxDB and PostgreSQL read 2 and
// 33 s from the same two deployments. The address an environment was put
// live at is a url per deployment, and as a bucket of one under the others it
// kept one address's deployments and dropped the rest from the count and the
// medians. Two deployments of one environment, one that put an address live
// and one that did not, are aggregated here the way Elasticsearch buckets
// them and merged the way the panel merges its queries, and the row has to
// count both and still carry the address, as MAX(environment_url) does.
func TestDeploymentsByEnvironmentCountsEveryDeploymentInElasticsearch(t *testing.T) {
	t.Parallel()
	page := "https://github.com/alice/site/deployments"
	live := "https://alice.github.io/site/"
	docs := []esDoc{
		{
			"full_name": "alice/site", "repo": "site", "environment": "github-pages",
			"outcome": "success", "url": page, "environment_url": live,
			"deployments": 1, "seconds_to_status": 33,
		},
		{
			"full_name": "alice/site", "repo": "site", "environment": "github-pages",
			"outcome": "success", "url": page, "deployments": 1, "seconds_live": 8041,
		},
	}
	p := mustPanel(t, rendered(t, "elasticsearch"), "Deployments by environment")
	rows := evalESPanel(t, p, docs)
	if len(rows) != 1 {
		t.Fatalf("two deployments of one environment are %d rows: %v", len(rows), rows)
	}
	row := rows[0]
	for column, want := range map[string]any{
		"Repository": "site", "Environment": "github-pages", "Deployments": 2.0,
		deliveryTimeToStatus: 33.0, deliveryTimeLive: 8041.0, "Link": page, "Live": live,
	} {
		if got := row[column]; got != want {
			t.Errorf("%s reads %v, want %v, as the SQL stores read it: %v", column, got, want, row)
		}
	}
}

// ── An Elasticsearch table evaluator for the aggregations the panels use ────

// esDoc is one document of an index, its tags and fields by their own names.
type esDoc map[string]any

// evalESPanel answers every query of an Elasticsearch table over the
// documents, merges the answers when the panel merges them, and applies its
// renames and exclusions, which is what the table draws.
func evalESPanel(t *testing.T, p map[string]any, docs []esDoc) []map[string]any {
	t.Helper()
	var frames [][]map[string]any
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		frames = append(frames, evalESTable(t, raw.(map[string]any), docs))
	}
	tfs, _ := p["transformations"].([]any)
	rows := slices.Concat(frames...)
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		switch tf["id"] {
		case "merge":
			rows = mergeFrames(frames)
		case "organize":
			rows = organized(rows, options)
		case "calculateField":
			rows = calculated(t, rows, options)
		case "filterByValue":
			rows = filteredByValue(t, rows, options)
		case "sortBy":
			rows = sortedBy(rows, options)
		case "limit":
			if n, _ := options["limitField"].(int); n < len(rows) {
				rows = rows[:n]
			}
		default:
			t.Fatalf("%q: no evaluator for the %v transformation", p["title"], tf["id"])
		}
	}
	return rows
}

// evalESTable is one query's table: a row per bucket of the innermost terms
// aggregation, each carrying the keys of the buckets above it and its metrics
// named as esCols names them.
func evalESTable(t *testing.T, target map[string]any, docs []esDoc) []map[string]any {
	t.Helper()
	buckets, _ := target["bucketAggs"].([]any)
	metrics, _ := target["metrics"].([]any)
	var rows []map[string]any
	var walk func(level int, docs []esDoc, row map[string]any)
	walk = func(level int, docs []esDoc, row map[string]any) {
		if level == len(buckets) {
			rows = append(rows, withMetrics(t, row, metrics, docs))
			return
		}
		b, _ := buckets[level].(map[string]any)
		if b["type"] != "terms" {
			t.Fatalf("no evaluator for a %v bucket", b["type"])
		}
		field, _ := b["field"].(string)
		settings, _ := b["settings"].(map[string]any)
		groups := termsOf(strings.TrimSuffix(field, ".keyword"), settings, docs)
		for _, key := range orderedKeys(t, groups, settings) {
			next := maps.Clone(row)
			next[field] = key
			walk(level+1, groups[key], next)
		}
	}
	walk(0, docs, map[string]any{})
	return rows
}

// termsOf is a terms aggregation's buckets: the documents per value, and those
// without the field under the `missing` value when one is set, which is the
// only way a document without it stays in.
func termsOf(field string, settings map[string]any, docs []esDoc) map[string][]esDoc {
	groups := map[string][]esDoc{}
	missing, keepsMissing := settings["missing"].(string)
	for _, d := range docs {
		v, has := d[field]
		key := fmt.Sprint(v)
		if !has {
			if !keepsMissing {
				continue
			}
			key = missing
		}
		groups[key] = append(groups[key], d)
	}
	return groups
}

// orderedKeys is the buckets a terms aggregation keeps, in its order: by
// document count, largest first and the key breaking a tie as Elasticsearch
// breaks it, or by the key itself, cut to its size.
func orderedKeys(t *testing.T, groups map[string][]esDoc, settings map[string]any) []string {
	t.Helper()
	keys := slices.Sorted(maps.Keys(groups))
	switch by, order := settings["orderBy"], settings["order"]; {
	case by == "_count" && order == "desc":
		sort.SliceStable(keys, func(i, j int) bool { return len(groups[keys[i]]) > len(groups[keys[j]]) })
	case by == "_key" && order == "desc":
		slices.Reverse(keys)
	case by == "_key" && order == "asc":
	default:
		t.Fatalf("no evaluator for a terms bucket ordered by %v %v", by, order)
	}
	size, _ := strconv.Atoi(fmt.Sprint(settings["size"]))
	return keys[:min(size, len(keys))]
}

// withMetrics is one row with the metrics of its bucket's documents.
func withMetrics(t *testing.T, row map[string]any, metrics []any, docs []esDoc) map[string]any {
	t.Helper()
	out := maps.Clone(row)
	for i, names := range esCols(metrics) {
		m, _ := metrics[i].(map[string]any)
		field, _ := m["field"].(string)
		values := numbersOf(docs, field)
		switch m["type"] {
		case "count":
			out[names[0]] = float64(len(docs))
		case "sum":
			total := 0.0
			for _, v := range values {
				total += v
			}
			out[names[0]] = total
		case "avg":
			// No value at all is null, as for a percentile.
			var mean any
			if len(values) > 0 {
				total := 0.0
				for _, v := range values {
					total += v
				}
				mean = total / float64(len(values))
			}
			out[names[0]] = mean
		case "percentiles":
			for j, p := range settingStrings(m, "percents") {
				want, _ := strconv.ParseFloat(p, 64)
				out[names[j]] = percentileOf(values, want)
			}
		default:
			t.Fatalf("no evaluator for a %v metric", m["type"])
		}
	}
	return out
}

func numbersOf(docs []esDoc, field string) []float64 {
	var out []float64
	for _, d := range docs {
		switch v := d[field].(type) {
		case int:
			out = append(out, float64(v))
		case float64:
			out = append(out, v)
		case bool:
			// A boolean aggregates as 1 and 0, which is what a sum of the
			// success flag counts.
			out = append(out, float64(map[bool]int{true: 1}[v]))
		}
	}
	return out
}

// percentileOf interpolates between the two nearest values, which is what
// Elasticsearch's estimate comes to on as few values as a test holds; no
// value at all is null, which is what it answers then.
func percentileOf(values []float64, p float64) any {
	if len(values) == 0 {
		return nil
	}
	sorted := slices.Sorted(slices.Values(values))
	rank := p / 100 * float64(len(sorted)-1)
	low, high := int(math.Floor(rank)), int(math.Ceil(rank))
	return sorted[low] + (sorted[high]-sorted[low])*(rank-float64(low))
}

// mergeFrames is Grafana's merge transformation: the fields every frame has
// are the key, and rows of different frames with the same key and no value in
// conflict become one row.
func mergeFrames(frames [][]map[string]any) []map[string]any {
	shared := map[string]int{}
	for _, frame := range frames {
		fields := map[string]bool{}
		for _, row := range frame {
			for k := range row {
				fields[k] = true
			}
		}
		for k := range fields {
			shared[k]++
		}
	}
	var key []string
	for k, n := range shared {
		if n == len(frames) {
			key = append(key, k)
		}
	}
	slices.Sort(key)
	var out []map[string]any
	index := map[string]int{}
	for _, frame := range frames {
		for _, row := range frame {
			var parts []string
			for _, k := range key {
				parts = append(parts, fmt.Sprint(row[k]))
			}
			id := strings.Join(parts, "\x00")
			if i, seen := index[id]; seen && !conflicts(out[i], row) {
				maps.Copy(out[i], row)
				continue
			}
			index[id] = len(out)
			out = append(out, maps.Clone(row))
		}
	}
	return out
}

func conflicts(a, b map[string]any) bool {
	for k, v := range b {
		if w, has := a[k]; has && w != nil && w != v {
			return true
		}
	}
	return false
}

// organized is the organize transformation's renames and exclusions.
func organized(rows []map[string]any, options map[string]any) []map[string]any {
	rename, _ := options["renameByName"].(map[string]any)
	exclude, _ := options["excludeByName"].(map[string]any)
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = map[string]any{}
		for k, v := range row {
			if exclude[k] == true {
				continue
			}
			to, _ := rename[k].(string)
			out[i][cmp.Or(to, k)] = v
		}
	}
	return out
}

// calculated is the calculateField transformation in its binary mode, between
// two columns of the row.
func calculated(t *testing.T, rows []map[string]any, options map[string]any) []map[string]any {
	t.Helper()
	binary, _ := options["binary"].(map[string]any)
	if options["mode"] != "binary" {
		t.Fatalf("no evaluator for a calculateField in %v mode", options["mode"])
	}
	alias, _ := options["alias"].(string)
	for _, row := range rows {
		left, _ := row[binary["left"].(string)].(float64)
		right, _ := row[binary["right"].(string)].(float64)
		switch binary["operator"] {
		case "-":
			row[alias] = left - right
		case "/":
			row[alias] = left / right
		default:
			t.Fatalf("no evaluator for the operator %v", binary["operator"])
		}
	}
	return rows
}

// filteredByValue is the filterByValue transformation keeping the rows whose
// one column is greater than a number.
func filteredByValue(t *testing.T, rows []map[string]any, options map[string]any) []map[string]any {
	t.Helper()
	filters, _ := options["filters"].([]any)
	if options["type"] != "include" || len(filters) != 1 {
		t.Fatalf("no evaluator for the filter %v", options)
	}
	filter, _ := filters[0].(map[string]any)
	config, _ := filter["config"].(map[string]any)
	bound, _ := config["options"].(map[string]any)
	if config["id"] != "greater" {
		t.Fatalf("no evaluator for a %v filter", config["id"])
	}
	var out []map[string]any
	for _, row := range rows {
		v, _ := row[filter["fieldName"].(string)].(float64)
		if v > float64(bound["value"].(int)) {
			out = append(out, row)
		}
	}
	return out
}

// sortedBy is the sortBy transformation over its one column.
func sortedBy(rows []map[string]any, options map[string]any) []map[string]any {
	by, _ := options["sort"].([]any)
	s, _ := by[0].(map[string]any)
	field, _ := s["field"].(string)
	desc, _ := s["desc"].(bool)
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := rows[i][field].(float64)
		b, _ := rows[j][field].(float64)
		if desc {
			return a > b
		}
		return a < b
	})
	return rows
}
