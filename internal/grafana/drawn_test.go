package grafana

import (
	"math"
	"slices"
	"strings"
	"testing"
)

// field is one field of a frame in the wire format /api/ds/query answers in.
type wireField struct {
	name, typ string
	labels    map[string]any
	config    map[string]any
	values    []any
}

func wireFrame(ref string, fields ...wireField) map[string]any {
	schema := []any{}
	values := []any{}
	for _, f := range fields {
		s := map[string]any{"name": f.name, "type": f.typ}
		if f.labels != nil {
			s["labels"] = f.labels
		}
		if f.config != nil {
			s["config"] = f.config
		}
		schema = append(schema, s)
		values = append(values, f.values)
	}
	return map[string]any{
		"schema": map[string]any{"refId": ref, "fields": schema, "meta": map[string]any{}},
		"data":   map[string]any{"values": values},
	}
}

// promFrame is one series of an instant query as the Prometheus backend
// answers it, before the datasource reshapes a table-format one.
func promFrame(ref string, at float64, labels map[string]any, v float64) map[string]any {
	f := wireFrame(ref,
		wireField{name: "Time", typ: "time", values: []any{at}},
		wireField{name: "Value", typ: "number", labels: labels, values: []any{v}})
	f["schema"].(map[string]any)["meta"] = map[string]any{"custom": map[string]any{"resultType": "vector"}}
	return f
}

func answerOf(frames map[string][]map[string]any) map[string]any {
	results := map[string]any{}
	for ref, fs := range frames {
		list := make([]any, len(fs))
		for i := range fs {
			list[i] = fs[i]
		}
		results[ref] = map[string]any{"frames": list}
	}
	return map[string]any{"results": results}
}

func mustDraw(t *testing.T, panel, answer map[string]any) []*Frame {
	t.Helper()
	frames, err := Draw(panel, answer)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	return frames
}

func column(t *testing.T, frames []*Frame, name string) *Field {
	t.Helper()
	for _, f := range frames[0].Fields {
		if f.Display == name {
			return f
		}
	}
	var have []string
	for _, f := range frames[0].Fields {
		have = append(have, f.Display)
	}
	t.Fatalf("no column %q, only %v", name, have)
	return nil
}

// promTablePanel is "Every repository, ever" in Prometheus, cut to two of its
// seven queries: two instant queries as tables, merged into one, the label
// columns the scrape adds left out.
func promTablePanel() map[string]any {
	return map[string]any{
		"type": "table",
		"targets": []any{
			map[string]any{"refId": "A", "format": "table", "expr": "a"},
			map[string]any{"refId": "B", "format": "table", "expr": "b"},
		},
		"transformations": []any{
			map[string]any{"id": "merge", "options": map[string]any{}},
			map[string]any{"id": "organize", "options": map[string]any{
				"excludeByName": map[string]any{"Time": true, "__name__": true},
				"renameByName": map[string]any{
					"Value #A": "Commits", "Value #B": "Stars", "repo": "Repository",
				},
			}},
		},
	}
}

// TestDrawMergesThePrometheusTablesAGrafanaRenderMerges is the review's first
// finding: plain selectors keep __name__, a merge keys its rows on every field
// the frames share, and the seven values of one repository came out as seven
// rows. A query that aggregates the name away draws one.
func TestDrawMergesThePrometheusTablesAGrafanaRenderMerges(t *testing.T) {
	t.Parallel()
	const now = 1790000000000.0
	for _, tc := range []struct {
		name     string
		a, b     map[string]any
		at       float64
		wantRows int
	}{
		{
			"an aggregation, which drops the name",
			map[string]any{"repo": "hello-world"},
			map[string]any{"repo": "hello-world"},
			now, 1,
		},
		{
			"two plain selectors, which keep it",
			map[string]any{"__name__": "commits", "repo": "hello-world"},
			map[string]any{"__name__": "stars", "repo": "hello-world"},
			now, 2,
		},
		{
			"two instants a millisecond apart",
			map[string]any{"repo": "hello-world"},
			map[string]any{"repo": "hello-world"},
			now + 1, 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frames := mustDraw(t, promTablePanel(), answerOf(map[string][]map[string]any{
				"A": {promFrame("A", now, tc.a, 405)},
				"B": {promFrame("B", tc.at, tc.b, 80)},
			}))
			repo := column(t, frames, "Repository")
			if len(repo.Values) != tc.wantRows {
				t.Errorf("drew %d rows, want %d: %v", len(repo.Values), tc.wantRows, repo.Values)
			}
			if tc.wantRows == 1 {
				if c, s := column(t, frames, "Commits"), column(t, frames, "Stars"); c.Values[0] != 405.0 || s.Values[0] != 80.0 {
					t.Errorf("Commits %v and Stars %v, want 405 and 80 on the one row", c.Values, s.Values)
				}
			}
		})
	}
}

// esStatPanel is an Elasticsearch stat whose value is named by the response
// parser and renamed by a displayName on its query's refId, with the unit
// addressed to the name the panel draws.
func esStatPanel(renameFirst bool) map[string]any {
	unit := map[string]any{
		"matcher":    map[string]any{"id": "byName", "options": "Time to merge"},
		"properties": []any{map[string]any{"id": "unit", "value": "s"}},
	}
	name := map[string]any{
		"matcher":    map[string]any{"id": "byFrameRefID", "options": "B"},
		"properties": []any{map[string]any{"id": "displayName", "value": "Time to merge"}},
	}
	overrides := []any{unit, name}
	if renameFirst {
		overrides = []any{name, unit}
	}
	return map[string]any{
		"type":        "stat",
		"targets":     []any{map[string]any{"refId": "B"}},
		"fieldConfig": map[string]any{"defaults": map[string]any{}, "overrides": overrides},
		"options":     map[string]any{"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}}},
	}
}

// TestAnOverrideMatchesTheNameTheFieldCarriesWhenItIsReached is the review's
// third finding, as Grafana 13 draws it: a unit placed before the displayName
// that names its field looks for a name nothing carries yet, and Time to merge
// read "194 K" instead of 2.25 days.
func TestAnOverrideMatchesTheNameTheFieldCarriesWhenItIsReached(t *testing.T) {
	t.Parallel()
	answer := answerOf(map[string][]map[string]any{"B": {wireFrame("B",
		wireField{name: "measurement.keyword", typ: "string", values: []any{"gh_pull_request"}},
		wireField{name: "p50.0 seconds_to_merge", typ: "number", values: []any{194400.0}},
	)}})
	for renameFirst, want := range map[bool]string{false: "", true: "s"} {
		panel := esStatPanel(renameFirst)
		readings, err := Readings(panel, mustDraw(t, panel, answer))
		if err != nil {
			t.Fatal(err)
		}
		if len(readings) != 1 || readings[0].Name != "Time to merge" || readings[0].Field.Unit() != want {
			t.Errorf("renamed first %v: read %+v, want one Time to merge in %q", renameFirst, readings, want)
		}
	}
}

// TestReduceNamesARowForEachSeries is how every Graphite table is drawn: a row
// per series under the name Graphite gave it, a column per reducer, and a
// reducer with nothing to reduce read as NaN.
func TestReduceNamesARowForEachSeries(t *testing.T) {
	t.Parallel()
	series := func(name string, values ...any) map[string]any {
		return wireFrame("A",
			wireField{name: "time", typ: "time", values: []any{1.0, 2.0, 3.0}},
			wireField{name: "value", typ: "number", config: map[string]any{"displayNameFromDS": name}, values: values})
	}
	panel := map[string]any{
		"type":    "table",
		"targets": []any{map[string]any{"refId": "A"}},
		"transformations": []any{
			map[string]any{"id": "reduce", "options": map[string]any{
				"mode": "seriesToRows", "reducers": []any{"lastNotNull", "count"},
			}},
			map[string]any{"id": "organize", "options": map[string]any{
				"renameByName": map[string]any{"Field": "Repository", "Last *": "Stars", "Count": "Days"},
			}},
		},
	}
	frames := mustDraw(t, panel, answerOf(map[string][]map[string]any{"A": {
		series("hello-world", nil, 4.0, nil), series("else", nil, nil, nil),
	}}))
	repo, stars, days := column(t, frames, "Repository"), column(t, frames, "Stars"), column(t, frames, "Days")
	if !slices.Equal(repo.Values, []any{"hello-world", "else"}) || stars.Values[0] != 4.0 ||
		days.Values[0] != 1.0 || days.Values[1] != 0.0 {
		t.Errorf("drew %v, %v, %v", repo.Values, stars.Values, days.Values)
	}
	if n, _ := stars.Values[1].(float64); !math.IsNaN(n) {
		t.Errorf("a series of nothing reads %v, want NaN", stars.Values[1])
	}
}

// TestTheElasticsearchTransformationsAreReplayed is "Workflows that keep
// failing" and "Dependencies by ecosystem" in Elasticsearch: arithmetic
// between two columns, a threshold, and a grouping named by its reducer.
func TestTheElasticsearchTransformationsAreReplayed(t *testing.T) {
	t.Parallel()
	answer := answerOf(map[string][]map[string]any{"A": {wireFrame("A",
		wireField{name: "Workflow", typ: "string", values: []any{"ci", "ci", "lint"}},
		wireField{name: "Runs", typ: "number", values: []any{9.0, 1.0, 2.0}},
		wireField{name: "Succeeded", typ: "number", values: []any{4.0, 0.0, 2.0}},
	)}})
	panel := map[string]any{
		"type":    "table",
		"targets": []any{map[string]any{"refId": "A"}},
		"transformations": []any{
			map[string]any{"id": "calculateField", "options": map[string]any{
				"mode": "binary", "alias": "Failures",
				"binary": map[string]any{"left": "Runs", "operator": "-", "right": "Succeeded"},
			}},
			map[string]any{"id": "groupBy", "options": map[string]any{"fields": map[string]any{
				"Workflow": map[string]any{"operation": "groupby", "aggregations": []any{}},
				"Failures": map[string]any{"operation": "aggregate", "aggregations": []any{"sum"}},
			}}},
			map[string]any{"id": "filterByValue", "options": map[string]any{
				"type": "include", "match": "any",
				"filters": []any{map[string]any{
					"fieldName": "Failures (sum)",
					"config":    map[string]any{"id": "greater", "options": map[string]any{"value": 3.0}},
				}},
			}},
		},
	}
	frames := mustDraw(t, panel, answer)
	if w, f := column(t, frames, "Workflow"), column(t, frames, "Failures (sum)"); !slices.Equal(w.Values, []any{"ci"}) ||
		!slices.Equal(f.Values, []any{6.0}) {
		t.Errorf("kept %v with %v failures, want ci with 6", w.Values, f.Values)
	}
}

// TestDrawRefusesWhatItCannotReplay: a step replayed wrongly would compare two
// stores on something neither draws, so a step Draw does not know is an error.
func TestDrawRefusesWhatItCannotReplay(t *testing.T) {
	t.Parallel()
	answer := answerOf(map[string][]map[string]any{"A": {wireFrame("A",
		wireField{name: "x", typ: "number", values: []any{1.0}})}})
	for name, panel := range map[string]map[string]any{
		"transformation": {
			"targets":         []any{map[string]any{"refId": "A"}},
			"transformations": []any{map[string]any{"id": "joinByField"}},
		},
		"matcher": {"targets": []any{map[string]any{"refId": "A"}}, "fieldConfig": map[string]any{
			"overrides": []any{map[string]any{"matcher": map[string]any{"id": "byRegexp", "options": ".*"}}},
		}},
	} {
		if _, err := Draw(panel, answer); err == nil || !strings.Contains(err.Error(), "does not replay") {
			t.Errorf("an unknown %s: err = %v, want a refusal", name, err)
		}
	}
}

// TestAValueIsTheTextItsMappingGivesIt: a flag reads as the word it is mapped
// to, which is what was compared when the stores read yes and true.
func TestAValueIsTheTextItsMappingGivesIt(t *testing.T) {
	t.Parallel()
	f := &Field{Config: map[string]any{"mappings": []any{
		map[string]any{"type": "value", "options": map[string]any{
			"false": map[string]any{"text": "no"}, "1": map[string]any{"text": "yes"},
			"2": map[string]any{"color": "red"},
		}},
		map[string]any{"type": "special", "options": map[string]any{
			"match": "null", "result": map[string]any{"text": "n/a"},
		}},
		map[string]any{"type": "regex", "options": map[string]any{
			"pattern": "^https?://.+", "result": map[string]any{"text": "Open"},
		}},
	}}}
	for _, tc := range []struct {
		v      any
		want   string
		mapped bool
	}{
		{"false", "no", true},
		{1.0, "yes", true},
		{nil, "n/a", true},
		{"https://github.com/o/r", "Open", true},
		{2.0, "", false},
		{"true", "", false},
	} {
		if got, mapped := f.Mapped(tc.v); got != tc.want || mapped != tc.mapped {
			t.Errorf("Mapped(%v) = %q, %v, want %q, %v", tc.v, got, mapped, tc.want, tc.mapped)
		}
	}
}
