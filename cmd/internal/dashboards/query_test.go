package dashboards

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// panicOf runs f and returns what it panicked with, as text, failing the test
// when it returns normally. The generator stops on a mistake in the
// specification by panicking, so the message is the whole of what a reader
// of the failed run learns about the mistake.
func panicOf(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("returned normally, want a panic")
		}
		msg = fmt.Sprint(r)
	}()
	f()
	return ""
}

// TestOutsideQuotesCopiesAnUnterminatedQuoteAsItIs: a statement whose last
// quote never closes still translates what comes before it, and the open
// span is handed on untouched rather than read past its end.
func TestOutsideQuotesCopiesAnUnterminatedQuoteAsItIs(t *testing.T) {
	t.Parallel()
	got := outsideQuotes(`select a from t where b = 'it''s open`, strings.ToUpper)
	want := `SELECT A FROM T WHERE B = 'it''s open`
	if got != want {
		t.Errorf("outsideQuotes = %q, want %q", got, want)
	}
	got = outsideQuotes(`select "col" from t`, strings.ToUpper)
	want = `SELECT "col" FROM T`
	if got != want {
		t.Errorf("outsideQuotes = %q, want %q", got, want)
	}
}

// TestToPGRefusesSQLItCannotTranslate: each of the four DataFusion spellings
// that PostgreSQL would refuse stops the generator on its own, so a panel
// written in a shape the rewrites do not know is found when the file is
// generated and not when a PostgreSQL user opens it.
func TestToPGRefusesSQLItCannotTranslate(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"SELECT date_bin(INTERVAL '3 days', time) FROM gh_repo",
		"SELECT approx_percentile_cont(Duration, 0.5) FROM gh_workflow_run",
		"SELECT $__dateBin(Time) FROM gh_repo",
		"SELECT arrow_cast(stars, 'Utf8') FROM gh_repo",
	} {
		msg := panicOf(t, func() { toPG(q) })
		if !strings.HasPrefix(msg, "untranslated SQL for PostgreSQL: ") || !strings.Contains(msg, "FROM gh_") {
			t.Errorf("toPG(%q) panicked with %q, want the untranslated statement", q, msg)
		}
	}
	if got := toPG("SELECT stars FROM gh_repo"); got != "SELECT stars FROM gh_repo" {
		t.Errorf("a statement with nothing to translate came back as %q", got)
	}
}

// TestGraphiteNodesRefuseATagTheMeasurementLacks: a Graphite path is built
// from the tag table, so a tag or a measurement it does not list is a
// mistake that would draw an empty panel, and it stops the generator.
func TestGraphiteNodesRefuseATagTheMeasurementLacks(t *testing.T) {
	t.Parallel()
	// github.<measurement>.<tags...>: the first tag is node 2.
	if got := gn("gh_repo", "archived"); got != 2 {
		t.Errorf("the first tag of gh_repo is node %d, want 2", got)
	}
	if got := gn("gh_repo", "visibility"); got != 10 {
		t.Errorf("the ninth tag of gh_repo is node %d, want 10", got)
	}
	if msg := panicOf(t, func() { gn("gh_repo", "stars") }); msg != "no tag stars on gh_repo" {
		t.Errorf("an unknown tag panicked with %q", msg)
	}
	if msg := panicOf(t, func() { tagsOf("gh_nothing") }); msg != "no tag list for gh_nothing" {
		t.Errorf("an unknown measurement panicked with %q", msg)
	}
}

// TestElasticsearchHelpersRefuseAMalformedAggregation: the column headings
// of an Elasticsearch table are computed from its aggregations, so one that
// is not a map, has no list of strings where a heading is made from, or
// groups by no field stops the generator with what was wrong.
func TestElasticsearchHelpersRefuseAMalformedAggregation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    func()
		want string
	}{
		{"not a map", func() { agg("count") }, "an Elasticsearch aggregation is a map, not string"},
		{
			"no list", func() { settingStrings(map[string]any{"type": "percentiles"}, "percents") },
			"a percentiles aggregation has no percents list in its settings",
		},
		{"not strings", func() {
			settingStrings(map[string]any{"type": "percentiles", "settings": map[string]any{"percents": []any{50}}}, "percents")
		}, "a percentiles aggregation's percents holds a int, not a string"},
		{"no field", func() { bucketField(map[string]any{"type": "terms"}) }, "a terms bucket names no field to group by"},
	}
	for _, c := range cases {
		if msg := panicOf(t, c.f); msg != c.want {
			t.Errorf("%s: panicked with %q, want %q", c.name, msg, c.want)
		}
	}
	got := settingStrings(map[string]any{"type": "percentiles", "settings": map[string]any{"percents": []any{"50", "95"}}}, "percents")
	if !reflect.DeepEqual(got, []string{"50", "95"}) {
		t.Errorf("settingStrings = %v, want [50 95]", got)
	}
}

// TestEsTblLeavesColumnsBeyondTheNamesAsTheyAre: a table may name fewer
// metric columns than its aggregations produce, and the ones past the names
// keep the heading Grafana gives them rather than stopping the generator.
func TestEsTblLeavesColumnsBeyondTheNamesAsTheyAre(t *testing.T) {
	t.Parallel()
	b := &builder{}
	_, tf := esTbl("gh_workflow_run",
		[]any{b.terms("repo.keyword", 10)},
		[]any{b.mCount(), b.mPct("duration_seconds", 50)},
		[]named{{"repo.keyword", "Repository"}, {"Count", "Runs"}}, nil)
	organize, _ := tf[0].(map[string]any)
	options, _ := organize["options"].(map[string]any)
	want := map[string]any{"repo.keyword": "Repository", "Count": "Runs"}
	if got := options["renameByName"]; !reflect.DeepEqual(got, want) {
		t.Errorf("renameByName = %v, want %v", got, want)
	}
}

// TestOutsideQuotesClosesAnEmptyLiteral: two quotes side by side open and
// close an empty string when nothing but the pair stands there, so the text
// after it is still translated. Reading the pair as an escaped quote would
// swallow the rest of the statement into a literal that never ends.
func TestOutsideQuotesClosesAnEmptyLiteral(t *testing.T) {
	t.Parallel()
	got := outsideQuotes(`select coalesce(b, '') from t`, strings.ToUpper)
	want := `SELECT COALESCE(B, '') FROM T`
	if got != want {
		t.Errorf("outsideQuotes = %q, want %q", got, want)
	}
}

// TestGraphitePathIgnoresATagGivenWithoutAValue: the pinned tags come in
// name and value pairs, so a name left over at the end pins nothing and its
// node stays a wildcard, rather than reading a value past the end of the list.
func TestGraphitePathIgnoresATagGivenWithoutAValue(t *testing.T) {
	t.Parallel()
	got := gp("gh_repo_community", "health", "repo", "ghchronicle", "owner")
	if want := "github.repo_community.*.*.ghchronicle.health"; got != want {
		t.Errorf("gp = %q, want %q", got, want)
	}
}
