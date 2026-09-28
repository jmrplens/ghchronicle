package dashboards

import (
	"regexp"
	"strings"
	"testing"
)

// The visual half of the 2.6.1 review: the five dashboards were imported into
// the containerised stack's Grafana, every panel was photographed, and what a
// reader saw was compared across the stores. Each test here pins one of the
// answers to the rendered JSON, in every store.

// TestASeriesOfOnePointIsDrawn: a line needs two points, and under
// `showPoints: never` a series whose range holds one value drew nothing while
// its legend read that value. "Time to merge over time" was an empty grid in
// all five stores, and a snapshot written once was one in Graphite and
// Elasticsearch.
func TestASeriesOfOnePointIsDrawn(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			if p["type"] != "timeseries" {
				continue
			}
			checked++
			if got := customOf(t, p)["showPoints"]; got != "auto" {
				t.Errorf("%s %q draws its points %v: a series of one value is drawn as nothing",
					store.Name, p["title"], got)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no timeseries panel was checked")
	}
}

// TestATileOfNothingSaysWhatIsMissing: a stat or a gauge whose value is a
// median, or a share of a count, has no number in a range that holds nothing
// to measure, and Grafana drew it as an empty space under the value's label:
// "Time to close an issue" in InfluxDB, PostgreSQL and Graphite, and the
// success rate, the signed share and the webhook failure rate in Prometheus.
// Each such value carries a sentence for that range, in every store.
func TestATileOfNothingSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	nullable := nullableTiles(t)
	if len(nullable) == 0 {
		t.Fatal("no stat value is a median or a share, so this checked nothing")
	}
	for _, store := range AllStores() {
		panels := rendered(t, store.Name)
		for title, names := range nullable {
			p := mustPanel(t, panels, title)
			if p["type"] == "text" {
				continue
			}
			said := noValues(t, p)
			for _, name := range names {
				if said[name] == "" {
					t.Errorf("%s %q: %s is empty in a range with nothing to measure, and says nothing",
						store.Name, title, name)
				}
			}
		}
	}
}

// nullableMeasure is an aggregate the SQL answers with null over no rows
// rather than with zero: a median, or a count divided by a count.
var nullableMeasure = regexp.MustCompile(`median\(|/ NULLIF\(|/ COUNT\(`)

// nullableTiles is, per stat and gauge panel of the InfluxDB dashboard, the
// name of every value whose SQL can answer null: the column's alias, or
// "value" for a panel of one value.
func nullableTiles(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for title, p := range rendered(t, "influxdb") {
		if p["type"] != "stat" && p["type"] != "gauge" {
			continue
		}
		targets, _ := p["targets"].([]any)
		for _, raw := range targets {
			target, _ := raw.(map[string]any)
			sql, _ := target["rawSql"].(string)
			for _, item := range selectList(sql) {
				// The alias is the last AS of the item: a CAST has one of its own.
				at := strings.LastIndex(item, " AS ")
				if at >= 0 && nullableMeasure.MatchString(item[:at]) {
					out[title] = append(out[title], strings.Trim(item[at+len(" AS "):], `"`))
				}
			}
		}
	}
	return out
}

// selectList is the items of a statement's outermost SELECT, split on the
// commas outside any parenthesis.
func selectList(sql string) []string {
	body, found := strings.CutPrefix(sql, "SELECT ")
	if !found {
		return nil
	}
	var items []string
	depth, start := 0, 0
	for i := range len(body) {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				items = append(items, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
		if depth == 0 && strings.HasPrefix(body[i:], " FROM ") {
			return append(items, strings.TrimSpace(body[start:i]))
		}
	}
	return items
}

// noValues is what a panel reads where a value is missing: per field name
// its override's noValue, and under "value" the panel's own default.
func noValues(t *testing.T, p map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	if v, ok := defaultsOf(t, p)["noValue"].(string); ok {
		out["value"] = v
	}
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		props, _ := o["properties"].([]any)
		for _, rawProp := range props {
			if prop, _ := rawProp.(map[string]any); prop["id"] == "noValue" {
				out[name], _ = prop["value"].(string)
			}
		}
	}
	return out
}

// checkPlaced holds one column of an Elasticsearch table to the place the
// SQL statement selects it at, relative to every other column both order.
func checkPlaced(t *testing.T, title, name string, selected map[string]int, index map[string]any) {
	t.Helper()
	got, ordered := index[name].(int)
	if !ordered {
		t.Errorf("%q: Elasticsearch leaves %s where its parser put it, not where the SQL selects it",
			title, name)
		return
	}
	for other, otherAt := range selected {
		if otherGot, ok := index[other].(int); ok && (otherAt < selected[name]) != (otherGot < got) {
			t.Errorf("%q: Elasticsearch orders %s and %s the other way round from the SQL",
				title, name, other)
		}
	}
}

// selectedAt is, for every column a SQL panel's statements select, its place
// among them, the first place a UNION's second half repeats.
func selectedAt(t *testing.T, p map[string]any) map[string]int {
	t.Helper()
	selected := map[string]int{}
	targets, _ := p["targets"].([]any)
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		sql, _ := target["rawSql"].(string)
		for _, item := range selectList(sql) {
			at := strings.LastIndex(item, ` AS "`)
			if at < 0 {
				continue
			}
			if name := strings.Trim(item[at+len(" AS "):], `"`); !hasKey(selected, name) {
				selected[name] = len(selected)
			}
		}
	}
	return selected
}

func hasKey(m map[string]int, key string) bool {
	_, ok := m[key]
	return ok
}

// finalOrder is the column order a panel's last transformation gives, or
// nil when the last one is not an organize.
func finalOrder(p map[string]any) map[string]any {
	tfs, _ := p["transformations"].([]any)
	if len(tfs) == 0 {
		return nil
	}
	last, _ := tfs[len(tfs)-1].(map[string]any)
	options, _ := last["options"].(map[string]any)
	if last["id"] != "organize" {
		return nil
	}
	index, _ := options["indexByName"].(map[string]any)
	return index
}

// namesGivenIn is every column name a panel's organize transformations give.
func namesGivenIn(p map[string]any) map[string]bool {
	out := map[string]bool{}
	tfs, _ := p["transformations"].([]any)
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		rename, _ := options["renameByName"].(map[string]any)
		for _, to := range rename {
			if name, ok := to.(string); ok {
				out[name] = true
			}
		}
	}
	return out
}

// TestElasticsearchTablesKeepTheSQLColumnOrder: an Elasticsearch table's
// columns came in the order the response parser met them, so "Every
// repository, ever" led with Fork, "Security features" with Enabled and
// "Largest merged pull requests" with the date. Every Elasticsearch table
// ends by ordering the columns it draws under a SQL name as the SQL stores'
// statement selects them.
func TestElasticsearchTablesKeepTheSQLColumnOrder(t *testing.T) {
	t.Parallel()
	es := rendered(t, "elasticsearch")
	checked := 0
	for title, p := range rendered(t, "influxdb") {
		e := mustPanel(t, es, title)
		if p["type"] != "table" || e["type"] != "table" || p["transformations"] != nil {
			continue
		}
		checked++
		selected, index := selectedAt(t, p), finalOrder(e)
		for name := range namesGivenIn(e) {
			if _, isSQL := selected[name]; isSQL {
				checkPlaced(t, title, name, selected, index)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Elasticsearch table was checked")
	}
}

// TestEnumSeriesAreDrawnUnderTheSQLWords: the SQL stores name the series of a
// split on a tag with words, Merged and "Open that day", Bot and Human, and
// the other three drew the tag's raw value, MERGED and true, so the legends
// read differently and the colors and the line matched by the words missed.
// Every word the SQL's CASE spells is a name the other three give their
// series, before any override that addresses it.
func TestEnumSeriesAreDrawnUnderTheSQLWords(t *testing.T) {
	t.Parallel()
	words := regexp.MustCompile(`(?:THEN|ELSE) '([^']+)'`)
	influx := rendered(t, "influxdb")
	for _, title := range []string{"Pull requests over time", "Issues over time", "Review threads over time"} {
		var spelled []string
		for _, m := range words.FindAllStringSubmatch(sqlOf(t, mustPanel(t, influx, title)), -1) {
			spelled = append(spelled, m[1])
		}
		if len(spelled) == 0 {
			t.Fatalf("%q spells no word in SQL, so this checks nothing", title)
		}
		for _, store := range []string{"prometheus", "graphite", "elasticsearch"} {
			given := map[string]int{}
			addressed := map[string]int{}
			for i, raw := range overridesOfPanel(mustPanel(t, rendered(t, store), title)) {
				o, _ := raw.(map[string]any)
				matcher, _ := o["matcher"].(map[string]any)
				name, _ := matcher["options"].(string)
				if givesName(o) {
					props, _ := o["properties"].([]any)
					prop, _ := props[0].(map[string]any)
					word, _ := prop["value"].(string)
					given[word] = i + 1
				} else if _, seen := addressed[name]; !seen {
					addressed[name] = i + 1
				}
			}
			for _, word := range spelled {
				switch {
				case given[word] == 0:
					t.Errorf("%s %q draws no series under %q, the SQL's word", store, title, word)
				case addressed[word] != 0 && addressed[word] < given[word]:
					t.Errorf("%s %q addresses %q before any series is named so", store, title, word)
				}
			}
		}
	}
}

// TestGraphiteContributionTotalsReadAsTheOthers: the other stores transpose
// one row of six columns into a table headed Metric and Last year whose rows
// are the SQL's column names in its order. Graphite, which arrives at rows
// already, drew them under "Field" and "Value" as the path's last node,
// pull_requests and restricted, in the alphabet's order.
func TestGraphiteContributionTotalsReadAsTheOthers(t *testing.T) {
	t.Parallel()
	const title = "Contribution totals"
	var rows []string
	for _, item := range selectList(sqlOf(t, mustPanel(t, rendered(t, "influxdb"), title))) {
		rows = append(rows, strings.Trim(item[strings.LastIndex(item, " AS ")+len(" AS "):], `"`))
	}
	p := mustPanel(t, rendered(t, "graphite"), title)
	targets, _ := p["targets"].([]any)
	target, _ := targets[0].(map[string]any)
	expr, _ := target["target"].(string)
	var named []string
	for _, m := range regexp.MustCompile(`alias\([^,]+, "([^"]+)"\)`).FindAllStringSubmatch(expr, -1) {
		named = append(named, m[1])
	}
	if strings.Join(named, "|") != strings.Join(rows, "|") {
		t.Errorf("Graphite names its rows %q, the other stores %q", named, rows)
	}
	tf := asJSON(t, p["transformations"])
	for _, want := range []string{`"Field":"Metric"`, `"Last *":"Last year"`} {
		if !strings.Contains(tf, want) {
			t.Errorf("Graphite does not head its columns as the others, missing %s: %s", want, tf)
		}
	}
}
