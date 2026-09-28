package dashboards

import (
	"maps"
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
// its override's noValue or the text it maps a null to, and under "value" the
// panel's own default.
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
			prop, _ := rawProp.(map[string]any)
			switch prop["id"] {
			case "noValue":
				out[name], _ = prop["value"].(string)
			case "mappings":
				if text, _ := nullMapping(prop["value"]); text != "" {
					out[name] = text
				}
			}
		}
	}
	return out
}

// nullMapping is the text and the color a list of value mappings gives a
// null, or two empty strings when none of them matches one.
func nullMapping(mappings any) (text, color string) {
	list, _ := mappings.([]any)
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		options, _ := m["options"].(map[string]any)
		if m["type"] != "special" || (options["match"] != "null" && options["match"] != "null+nan") {
			continue
		}
		result, _ := options["result"].(map[string]any)
		text, _ = result["text"].(string)
		color, _ = result["color"].(string)
		return text, color
	}
	return "", ""
}

// TestAWordForNothingIsNotDrawnAsAnAlarm: Grafana draws a field's noValue in
// the color of its lowest threshold, and the success rate and the signed
// share both start at red, so over a range with no run "none decided" read in
// the color of a failed build and "no commits" in that of unsigned work,
// beside "no runs" and "no jobs" in the text color: Prometheus at thirty
// days, InfluxDB and Graphite over a range before the fixtures. A sentence
// that says there is nothing to measure is no verdict, in any store.
func TestAWordForNothingIsNotDrawnAsAnAlarm(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			if p["type"] != "stat" && p["type"] != "gauge" {
				continue
			}
			for name, words := range noValues(t, p) {
				checked++
				if color := nothingColor(t, p, name); strings.Contains(color, "red") ||
					strings.Contains(color, "orange") {
					t.Errorf("%s %q: %q under %s is drawn in %s, the color of a verdict, where the "+
						"range held nothing to judge", store.Name, p["title"], words, name, color)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no stat or gauge says anything for a missing value, so this checked nothing")
	}
}

// nothingColor is the color Grafana draws one field's missing value in: the
// color its value mappings give a null, and failing that the one its color
// scheme gives the lowest value there is, which is the first threshold's
// under a thresholds scheme and the fixed color under any other. The field is
// the panel's defaults with every byName override of it laid on in order;
// "value" is the panel's own default.
func nothingColor(t *testing.T, p map[string]any, name string) string {
	t.Helper()
	config := maps.Clone(defaultsOf(t, p))
	for _, raw := range overridesOfPanel(p) {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		if name == "value" || matcher["id"] != "byName" || matcher["options"] != name {
			continue
		}
		props, _ := o["properties"].([]any)
		for _, rawProp := range props {
			prop, _ := rawProp.(map[string]any)
			if id, _ := prop["id"].(string); id != "" {
				config[id] = prop["value"]
			}
		}
	}
	if _, color := nullMapping(config["mappings"]); color != "" {
		return color
	}
	scheme, _ := config["color"].(map[string]any)
	if scheme["mode"] == "thresholds" {
		thresholds, _ := config["thresholds"].(map[string]any)
		steps, _ := thresholds["steps"].([]any)
		if len(steps) > 0 {
			lowest, _ := steps[0].(map[string]any)
			color, _ := lowest["color"].(string)
			return color
		}
	}
	color, _ := scheme["fixedColor"].(string)
	return color
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

// TestACountAxisTicksWholeNumbers: a bar chart of counts drew its axis in
// Grafana's default decimals, so one open alert stood on ticks every 0.05 up
// to 2 and three events on ticks every 0.2. A count is `short`, and every
// chart of one keeps whole numbers, the line charts as the bar charts.
func TestACountAxisTicksWholeNumbers(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, store := range AllStores() {
		for _, p := range renderedPanels(t, store.Name) {
			if p["type"] != "barchart" && p["type"] != "timeseries" {
				continue
			}
			defaults := defaultsOf(t, p)
			if defaults["unit"] != "short" {
				continue
			}
			checked++
			if defaults["decimals"] != 0 {
				t.Errorf("%s %q counts in %v decimals, so its axis ticks fractions of one",
					store.Name, p["title"], defaults["decimals"])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no chart of counts was checked")
	}
}

// TestAFlagReadsYesOrNoInEveryStoreThatDrawsIt: the SQL stores map a flag's
// 1 and 0 to yes and no, and Elasticsearch's raw documents answer the JSON
// true and false, its terms bucket 1 and 0, and an exporter label the text
// "true" and "false": "Sponsorships" read true and false in Elasticsearch and
// "Discussions" 0 and 1 there and false and true in Prometheus. Every column
// the SQL stores draw as yes or no reads so in the other stores that draw it
// under that name, whichever spelling they answer with.
func TestAFlagReadsYesOrNoInEveryStoreThatDrawsIt(t *testing.T) {
	t.Parallel()
	checked := 0
	for title, p := range rendered(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		for _, raw := range overridesOfPanel(p) {
			if column := matcherName(raw); yesNo(overrideProperty(p, column, "mappings")) != nil {
				checked += checkFlagWords(t, title, column)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no flag column of another store was checked")
	}
}

// checkFlagWords holds a flag column of Prometheus and Elasticsearch to the
// yes and no of the SQL stores, in every spelling a store answers it with,
// and reports how many stores it checked.
func checkFlagWords(t *testing.T, title, column string) int {
	t.Helper()
	checked := 0
	for _, store := range []string{"prometheus", "elasticsearch"} {
		other := mustPanel(t, rendered(t, store), title)
		if other["type"] != "table" || !namesGivenIn(other)[column] {
			continue
		}
		checked++
		words := yesNo(overrideProperty(other, column, "mappings"))
		for _, spelled := range []string{"0", "1", "false", "true"} {
			if words[spelled] == "" {
				t.Errorf("%s %q: %s reads %s as it is, where the SQL stores read yes or no",
					store, title, column, spelled)
			}
		}
	}
	return checked
}

// yesNo is the text a value mapping gives each value it maps, or nil when the
// mapping is not the yes and no of a flag.
func yesNo(mappings any) map[string]string {
	list, _ := mappings.([]any)
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		options, _ := m["options"].(map[string]any)
		out := map[string]string{}
		for value, rawResult := range options {
			result, _ := rawResult.(map[string]any)
			out[value], _ = result["text"].(string)
		}
		if out["1"] == "yes" {
			return out
		}
	}
	return nil
}

// TestAFullNameIsNotCut: repoWidth is measured against short names, and a
// table that names each repository in full, because its rows are mostly
// other people's, cut the name at 110 pixels: another/projec, octocat/hello-w.
// Such a column keeps a minimum of fullNameWidth and no fixed width below it.
func TestAFullNameIsNotCut(t *testing.T) {
	t.Parallel()
	fullName := regexp.MustCompile(`\bfull_name AS "Repository"`)
	checked := 0
	for title, p := range rendered(t, "influxdb") {
		if p["type"] != "table" || !fullName.MatchString(everySQL(t, p)) {
			continue
		}
		checked++
		if w, fixed := overrideProperty(p, "Repository", panelWidthField).(int); fixed && w < fullNameWidth {
			t.Errorf("%q draws its full names %d pixels wide", title, w)
		}
		if w, _ := overrideProperty(p, "Repository", "custom.minWidth").(int); w < fullNameWidth {
			t.Errorf("%q lets its full names shrink to %d pixels, want %d", title, w, fullNameWidth)
		}
	}
	if checked == 0 {
		t.Fatal("no table names its repositories in full, so this checked nothing")
	}
}
