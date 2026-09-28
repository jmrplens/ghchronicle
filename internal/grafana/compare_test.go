package grafana

import (
	"strings"
	"testing"
)

func tiles(values map[string]any, unit string) *Picture {
	p := &Picture{Kind: "stat"}
	for name, v := range values {
		p.Tiles = append(p.Tiles, Reading{Name: name, Value: v, Field: &Field{
			Display: name, Type: "number", Config: map[string]any{"unit": unit, "noValue": "none"},
		}})
	}
	return p
}

func table(columns ...*Field) *Picture { return &Picture{Kind: "table", Columns: columns} }

func col(name, typ string, values ...any) *Field {
	return &Field{Display: name, Type: typ, Config: map[string]any{}, Values: values}
}

// TestCompareTiles holds a tile to its value, its unit and the text it shows
// for nothing, and a group to the tiles it draws.
func TestCompareTiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		a, b *Picture
		want string
	}{
		{
			"the same", tiles(map[string]any{"Time to merge": 194400.0}, "s"),
			tiles(map[string]any{"Time to merge": 194400.0}, "s"), "",
		},
		{
			"a unit lost", tiles(map[string]any{"Time to merge": 194400.0}, "s"),
			tiles(map[string]any{"Time to merge": 194400.0}, ""), `is in "s" against no unit`,
		},
		{
			"a different number", tiles(map[string]any{"Queue wait": 47.5}, "s"),
			tiles(map[string]any{"Queue wait": 50.0}, "s"), "reads 47.5 against 50",
		},
		{
			"a float32 widened", tiles(map[string]any{"Spend": 11.712}, "currencyUSD"),
			tiles(map[string]any{"Spend": 11.712000012397766}, "currencyUSD"), "",
		},
		{
			"nothing against a number", tiles(map[string]any{"Issues closed": nil, "Merged": 1.0}, ""),
			tiles(map[string]any{"Issues closed": 0.0, "Merged": 1.0}, ""), `reads "(none)" against 0`,
		},
		{
			"a tile missing", tiles(map[string]any{"Issues closed": 0.0, "Merged": 1.0}, ""),
			tiles(map[string]any{"Merged": 1.0}, ""), "drawn by the first and not the second",
		},
		{
			"one value, named differently", tiles(map[string]any{"value": 12.5}, "percent"),
			tiles(map[string]any{"E": 12.5}, "percent"), "",
		},
	} {
		got := strings.Join(Compare(tc.a, tc.b, Likeness{}), "; ")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: Compare = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCompareRows holds a table to the rows it draws over the columns both
// stores have, in any order.
func TestCompareRows(t *testing.T) {
	t.Parallel()
	sql := table(col("Repository", "string", "hello-world", "else"), col("Commits", "number", 500.0, 12.0),
		col("Fork", "string", "false", "true"))
	graphite := table(col("Repository", "string", "else", "hello-world"), col("Commits", "number", 12.0, 500.0))
	if d := Compare(sql, graphite, Likeness{}); len(d) > 0 {
		t.Errorf("the same rows in another order, a column fewer: %v", d)
	}
	twice := table(col("Repository", "string", "hello-world", "hello-world"), col("Commits", "number", 405.0, 0.0))
	once := table(col("Repository", "string", "hello-world"), col("Commits", "number", 405.0))
	if d := Compare(once, twice, Likeness{}); len(d) != 1 || !strings.Contains(d[0], "1 only in the second") {
		t.Errorf("a repository drawn twice: %v", d)
	}
	empty := table(col("Workflow", "string"), col("Runs", "number"))
	if d := Compare(empty, table(col("Repository", "string")), Likeness{}); len(d) > 0 {
		t.Errorf("two tables of nothing: %v", d)
	}
	if d := Compare(empty, once, Likeness{}); len(d) != 1 || !strings.Contains(d[0], "the first draws nothing") {
		t.Errorf("nothing against a row: %v", d)
	}
	if d := Compare(once, table(col("Name", "string", "hello-world")), Likeness{}); len(d) != 1 ||
		!strings.Contains(d[0], "no column in common") {
		t.Errorf("two tables sharing no column: %v", d)
	}
}

// TestOrderIsHeldOnlyWhereTheRowsAgree: two stores that draw the same rows
// are held to one order, over the columns both draw, and two that draw
// different rows are left to Compare.
func TestOrderIsHeldOnlyWhereTheRowsAgree(t *testing.T) {
	t.Parallel()
	influx := table(col("Repository", "string", "someone/else", "another/project"), col("Comments", "number", 2.0, 0.0),
		col("Kind", "string", "pull_request", "issue"))
	postgres := table(col("Repository", "string", "another/project", "someone/else"), col("Comments", "number", 0.0, 2.0),
		col("Kind", "string", "issue", "pull_request"))
	if got := Order(influx, postgres, Likeness{}); !strings.Contains(got, `row 1 over [Repository Comments Kind] is ["someone/else" | 2 | "pull_request"]`) {
		t.Errorf("the same two rows swapped: Order = %q", got)
	}
	if got := Order(influx, influx, Likeness{}); got != "" {
		t.Errorf("one drawing against itself: Order = %q", got)
	}
	fewer := table(col("Repository", "string", "someone/else", "another/project"), col("Comments", "number", 2.0, 0.0))
	if got := Order(postgres, fewer, Likeness{}); got == "" {
		t.Error("the same rows in another order, over the columns both draw, read as one order")
	}
	other := table(col("Repository", "string", "another/project", "hello-world"), col("Comments", "number", 0.0, 2.0))
	if got := Order(influx, other, Likeness{}); got != "" {
		t.Errorf("different rows are Compare's to report, and Order = %q", got)
	}
	if got := Order(tiles(map[string]any{"Merged": 1.0}, ""), tiles(map[string]any{"Merged": 1.0}, ""), Likeness{}); got != "" {
		t.Errorf("a stat has no rows to order, and Order = %q", got)
	}
}

// TestLikenessIsWhatTheCallerAllows: a name as Graphite holds it and a number
// that moved between two sweeps are the caller's to allow, and nothing else is.
func TestLikenessIsWhatTheCallerAllows(t *testing.T) {
	t.Parallel()
	a := table(col("Author", "string", "(ghost)"), col("Open for", "number", 183041.0))
	b := table(col("Author", "string", "_ghost_"), col("Open for", "number", 183073.0))
	if d := Compare(a, b, Likeness{}); len(d) != 1 {
		t.Errorf("with nothing allowed: %v, want the one row that differs", d)
	}
	like := Likeness{
		Equal: func(x, y string) bool { return x == "(ghost)" && y == "_ghost_" },
		Slack: func(name string) float64 {
			if name == "Open for" {
				return 32
			}
			return 0
		},
	}
	if d := Compare(a, b, like); len(d) > 0 {
		t.Errorf("with the name and the drift allowed: %v", d)
	}
	like.Slack = func(string) float64 { return 31 }
	if d := Compare(a, b, like); len(d) != 1 {
		t.Errorf("with a second less drift than the sweeps had: %v", d)
	}
}

// TestAShareReadsTheSameInEitherUnit: the Elasticsearch mix is a fraction
// under percentunit and the others' a hundredth under percent, and both draw
// 82.9%.
func TestAShareReadsTheSameInEitherUnit(t *testing.T) {
	t.Parallel()
	if d := Compare(tiles(map[string]any{"Commits": 82.94}, "percent"),
		tiles(map[string]any{"Commits": 0.8294}, "percentunit"), Likeness{}); len(d) > 0 {
		t.Errorf("%v", d)
	}
}

// TestWithoutLeavesTheRestCompared: an excuse for one column still compares
// the others.
func TestWithoutLeavesTheRestCompared(t *testing.T) {
	t.Parallel()
	a := table(col("Repository", "string", "hello-world"), col("Commits", "number", 500.0))
	b := table(col("Repository", "string", "octocat/hello-world"), col("Commits", "number", 501.0))
	restA, restB := a.Without([]string{"Repository"}), b.Without([]string{"Repository"})
	if d := Compare(&restA, &restB, Likeness{}); len(d) != 1 {
		t.Errorf("a count one out beside the excused name: %v", d)
	}
}

// TestABarChartWithNothingToNameItsBarsIsAnError is "Languages starred" in
// Prometheus on 2.6.1: the exporter had dropped the label the query summed by,
// and the panel drew "Bar charts require a string or time field".
func TestABarChartWithNothingToNameItsBarsIsAnError(t *testing.T) {
	t.Parallel()
	panel := map[string]any{"type": "barchart", "targets": []any{map[string]any{"refId": "A"}}}
	labeled := answerOf(map[string][]map[string]any{"A": {wireFrame("A",
		wireField{name: "language", typ: "string", values: []any{"Go"}},
		wireField{name: "Stars", typ: "number", values: []any{3.0}})}})
	if pic, ok, err := Drawing(panel, labeled); err != nil || !ok || pic.Columns[0].Display != BarAxis {
		t.Errorf("a bar per language: %+v, %v, %v", pic, ok, err)
	}
	bare := answerOf(map[string][]map[string]any{"A": {wireFrame("A",
		wireField{name: "Stars", typ: "number", values: []any{3.0}})}})
	if _, _, err := Drawing(panel, bare); err == nil || !strings.Contains(err.Error(), "Bar charts require") {
		t.Errorf("a bar chart of one number and no name: err = %v, want Grafana's refusal", err)
	}
}

// TestDrawnNamesAreWhatAReaderSees: the tiles of a group, the headings of a
// table and the series of a chart, and not a field the panel does not draw.
func TestDrawnNamesAreWhatAReaderSees(t *testing.T) {
	t.Parallel()
	frames := []*Frame{{Fields: []*Field{
		{Display: "full_name.keyword", Type: "string", Config: map[string]any{}},
		{Display: "Link", Type: "string", Config: map[string]any{"custom": map[string]any{"hidden": true}}},
		{Display: "p50.0 duration_seconds", Type: "number", Config: map[string]any{}, Values: []any{1.0}},
		{Display: "Time", Type: "time", Config: map[string]any{}},
	}}}
	for kind, want := range map[string]string{
		"table":      "full_name.keyword p50.0 duration_seconds",
		"timeseries": "p50.0 duration_seconds",
		"stat":       "",
	} {
		names, err := DrawnNames(map[string]any{"type": kind}, frames)
		if got := strings.Join(names, " "); err != nil || got != want {
			t.Errorf("%s draws %q, %v, want %q", kind, got, err, want)
		}
	}
}
