package dashboards

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The visual review of 2026-09-12 read the rendered InfluxDB dashboard at
// 1920 pixels and on two phones and found the panels below drawing something
// other than what their titles promised. Each test here pins one of those
// repairs to the rendered JSON, which is what Grafana reads, so the next
// person who reshuffles a builder finds out from the test and not from a
// screenshot.

// rendered is every panel of one store's dashboard, rows unfolded, keyed by
// title. Two panels never share a title inside a section and the tests below
// name panels that are unique on the whole page.
func rendered(t *testing.T, store string) map[string]map[string]any {
	t.Helper()
	s, ok := ByName(store)
	if !ok {
		t.Fatalf("there is no %s dashboard", store)
	}
	out := map[string]map[string]any{}
	var walk func(list []map[string]any)
	walk = func(list []map[string]any) {
		for _, p := range list {
			if inner, isRow := p["panels"].([]map[string]any); isRow {
				walk(inner)
			}
			if p["type"] == "row" {
				continue
			}
			title, _ := p["title"].(string)
			out[title] = p
		}
	}
	list, ok := s.Build(nil)["panels"].([]map[string]any)
	if !ok {
		t.Fatal("the dashboard renders no panel list")
	}
	walk(list)
	if len(out) == 0 {
		t.Fatal("walked the dashboard and found no panels, so this checks nothing")
	}
	return out
}

// sqlOf is the first target's statement of a rendered SQL panel.
// ciStats is the title of the stat group the continuous integration numbers
// live in, named once so the tests that read one of its seven values do not
// each spell it.
const ciStats = "Runs in range"

// everySQL is every SQL statement of a panel joined, for a stat group whose
// values are a statement each.
func everySQL(t *testing.T, p map[string]any) string {
	t.Helper()
	targets, _ := p["targets"].([]any)
	var out []string
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		if sql, _ := target["rawSql"].(string); sql != "" {
			out = append(out, sql)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%v has no SQL", p["title"])
	}
	return strings.Join(out, "\n")
}

func sqlOf(t *testing.T, p map[string]any) string {
	t.Helper()
	targets, _ := p["targets"].([]any)
	if len(targets) == 0 {
		t.Fatalf("%v has no targets", p["title"])
	}
	first, _ := targets[0].(map[string]any)
	sql, _ := first["rawSql"].(string)
	if sql == "" {
		t.Fatalf("%v has no SQL", p["title"])
	}
	return sql
}

func defaultsOf(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	fc, _ := p["fieldConfig"].(map[string]any)
	d, ok := fc["defaults"].(map[string]any)
	if !ok {
		t.Fatalf("%v has no field defaults", p["title"])
	}
	return d
}

func customOf(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	c, ok := defaultsOf(t, p)["custom"].(map[string]any)
	if !ok {
		t.Fatalf("%v has no custom field defaults", p["title"])
	}
	return c
}

// asJSON is a rendered value as text, for the checks that read an override or
// a transformation by its shape.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mustPanel(t *testing.T, panels map[string]map[string]any, title string) map[string]any {
	t.Helper()
	p, ok := panels[title]
	if !ok {
		t.Fatalf("no panel called %q", title)
	}
	return p
}

// TestTableCellsScaleByTheirOwnColumn: a gauge cell scaled over every numeric
// column of the frame drew 48 merged pull requests as a blank bar beside a
// column of seconds. fieldMinMax makes each column its own scale.
func TestTableCellsScaleByTheirOwnColumn(t *testing.T) {
	t.Parallel()
	n := 0
	for _, p := range rendered(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		n++
		if v, _ := defaultsOf(t, p)["fieldMinMax"].(bool); !v {
			t.Errorf("%v scales its gauge cells over the whole frame", p["title"])
		}
	}
	if n == 0 {
		t.Fatal("no table found")
	}
}

// TestCommitsPerWeekKeepsTheWeekTheCollectorStamped: the rows are stamped at
// the Sunday each week starts on; a seven-day bin aligned to the epoch moved
// them to the Thursday before and out of a seven-day range.
func TestCommitsPerWeekKeepsTheWeekTheCollectorStamped(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		sql := sqlOf(t, mustPanel(t, rendered(t, store), "Commits per week"))
		if strings.Contains(sql, "date_bin") || strings.Contains(sql, "$__dateBin") ||
			strings.Contains(sql, "$__timeGroup") {
			t.Errorf("%s bins the weekly rows again: %s", store, sql)
		}
		if !strings.Contains(sql, "SELECT time, SUM(commits)") {
			t.Errorf("%s does not sum the repositories at the week's own stamp: %s", store, sql)
		}
	}
}

// TestContributionTotalsNamesItsColumns: a transpose with no options headed
// the table "Field" and "1".
func TestContributionTotalsNamesItsColumns(t *testing.T) {
	t.Parallel()
	p := mustPanel(t, rendered(t, "influxdb"), "Contribution totals")
	raw := asJSON(t, p["transformations"])
	if !strings.Contains(raw, `"firstFieldName":"Metric"`) ||
		!strings.Contains(raw, `"restFieldsName":"Last year"`) {
		t.Errorf("the transpose is unnamed: %s", raw)
	}
}

// TestDatedPanelsBucketByThePanelInterval: a fixed one-day bin was 730 bars
// of one pixel over two years. Every bucketed SQL panel now reads the bucket
// from the plugin macro and names the floor and the point ceiling Grafana
// computes the interval from; the PostgreSQL twin reads $__interval; and the
// Prometheus increases span the same step rather than a fixed day.
func TestDatedPanelsBucketByThePanelInterval(t *testing.T) {
	t.Parallel()
	fixed := regexp.MustCompile(`date_bin\(INTERVAL '1 (day|hour)'`)
	binned := 0
	for title, p := range rendered(t, "influxdb") {
		if p["type"] != "timeseries" {
			continue
		}
		sql := sqlOf(t, p)
		if fixed.MatchString(sql) {
			t.Errorf("%s still bins by a fixed interval: %s", title, sql)
		}
		if !strings.Contains(sql, "$__dateBin(") {
			continue
		}
		binned++
		if _, ok := p["interval"].(string); !ok {
			t.Errorf("%s buckets by the panel interval and names no floor", title)
		}
		if _, ok := p["maxDataPoints"].(int); !ok {
			t.Errorf("%s buckets by the panel interval and names no point ceiling", title)
		}
	}
	if binned < 20 {
		t.Errorf("only %d panels bucket by the panel interval, which is too few for this to have checked the per-day panels", binned)
	}
}

// TestTheOtherStoresFollowThePanelInterval is the same promise for the
// PostgreSQL translation and the Prometheus increases.
func TestTheOtherStoresFollowThePanelInterval(t *testing.T) {
	t.Parallel()
	for title, p := range rendered(t, "postgres") {
		if p["type"] != "timeseries" {
			continue
		}
		sql := sqlOf(t, p)
		if strings.Contains(sql, "$__dateBin") {
			t.Errorf("%s reached PostgreSQL with the InfluxDB macro: %s", title, sql)
		}
		if strings.Contains(sql, "$__timeGroupAlias(time, 1d)") || strings.Contains(sql, "$__timeGroupAlias(time, 1h)") {
			t.Errorf("%s still groups PostgreSQL by a fixed interval: %s", title, sql)
		}
	}
	for title, p := range rendered(t, "prometheus") {
		if expr := asJSON(t, p["targets"]); strings.Contains(expr, "[1d]") || strings.Contains(expr, "[1h]") {
			t.Errorf("%s increases over a fixed window rather than the step: %s", title, expr)
		}
	}
}

// TestToPGTranslatesTheBucketMacro pins the translation the test above relies
// on, and the seconds-ago arithmetic beside it.
func TestToPGTranslatesTheBucketMacro(t *testing.T) {
	t.Parallel()
	got := toPG("SELECT " + timeBin + ", x, ROW_NUMBER() OVER (PARTITION BY $__dateBin(time), repo ORDER BY time DESC) AS rn")
	// The bare bucket inside the partition is parenthesised: the plugin
	// appends AS "time" to a $__timeGroup that a comma follows, and did, on
	// the three panels that partition by the bucket and a tag.
	want := "SELECT $__timeGroupAlias(time, $__interval), x, ROW_NUMBER() OVER (PARTITION BY ($__timeGroup(time, $__interval)), repo ORDER BY time DESC) AS rn"
	if got != want {
		t.Errorf("toPG = %q, want %q", got, want)
	}
	got = toPG("SELECT " + agoSQL("time", "seconds_open") + ` AS "Opened"`)
	want = `SELECT time - seconds_open * INTERVAL '1 second' AS "Opened"`
	if got != want {
		t.Errorf("toPG = %q, want %q", got, want)
	}
}

// TestPerSeriesLegendsCarryNoCalcs: a legend of thirty repositories with Mean
// and Max on each put every series on its own line and hid the busiest one
// under the panel's edge. A panel with a series per tag value lists names
// only; a panel with a fixed handful of series keeps its numbers.
func TestPerSeriesLegendsCarryNoCalcs(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	calcs := func(p map[string]any) []any {
		o, _ := p["options"].(map[string]any)
		l, _ := o["legend"].(map[string]any)
		c, _ := l["calcs"].([]any)
		return c
	}
	if c := calcs(mustPanel(t, panels, "Views over time")); len(c) != 0 {
		t.Errorf("Views over time, one series per repository, lists %v in its legend", c)
	}
	if c := calcs(mustPanel(t, panels, "Time to merge over time")); len(c) != 2 {
		t.Errorf("Time to merge over time, two fixed series, lost its legend values: %v", c)
	}
}

// TestStackedBarsAreFilled: a stack of ten-per-cent bars was a tower of
// outlines; a line keeps the light fill.
func TestStackedBarsAreFilled(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	if v := customOf(t, mustPanel(t, panels, "Stars gained over time"))["fillOpacity"]; v != 50 {
		t.Errorf("a stacked bar panel fills at %v, want 50", v)
	}
	if v := customOf(t, mustPanel(t, panels, "Time to merge over time"))["fillOpacity"]; v != 10 {
		t.Errorf("a line panel fills at %v, want 10", v)
	}
}

// TestCountsShowNoDecimals: the axis of stars gained per day read 0.25 and
// the legend "Mean: 0.750" of a count.
func TestCountsShowNoDecimals(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	if v := defaultsOf(t, mustPanel(t, panels, "Stars gained over time"))["decimals"]; v != 0 {
		t.Errorf("a count shows %v decimals", v)
	}
	if _, set := defaultsOf(t, mustPanel(t, panels, "Time to merge over time"))["decimals"]; set {
		t.Error("a panel of seconds had its decimals pinned")
	}
}

// TestStackedSeriesAreLimited: the panels a series per repository or type
// name the biggest eight and fold the rest into `other`.
func TestStackedSeriesAreLimited(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for _, title := range []string{
		"Views over time", "Unique visitors over time", "Clones over time",
		"Commits by repository, dated", "Artifacts created over time", "Events over time",
		"Transitions over time", "Artifact storage over time",
	} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if !strings.Contains(sql, "rk <= 8") || !strings.Contains(sql, "'other'") {
			t.Errorf("%s draws every series: %s", title, sql)
		}
	}
}

// TestOpenItemsAreReadFromTheirNewestRow: an open pull request writes a row a
// day and those rows stay when it merges, so a filter on the open rows listed
// merged pull requests as open. The tables take the newest row per item first;
// the per-day panels count the open ones as distinct numbers under their own
// name and draw them as a line outside the stack.
func TestOpenItemsAreReadFromTheirNewestRow(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for _, title := range []string{"Open the longest", "Open issues the longest"} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if !strings.Contains(sql, "PARTITION BY repo, number ORDER BY time DESC") ||
			!strings.Contains(sql, "x.rn = 1 AND x.state = 'OPEN'") {
			t.Errorf("%s filters the open rows before taking the newest: %s", title, sql)
		}
	}
	for _, title := range []string{"Pull requests over time", "Issues over time"} {
		p := mustPanel(t, panels, title)
		sql := sqlOf(t, p)
		if !strings.Contains(sql, "COUNT(DISTINCT number)") || !strings.Contains(sql, "'Open that day'") {
			t.Errorf("%s counts the daily rows of an open item as events: %s", title, sql)
		}
		raw := asJSON(t, p["fieldConfig"])
		if !strings.Contains(raw, `"options":"Open that day"`) ||
			!strings.Contains(raw, `"custom.drawStyle","value":"line"`) {
			t.Errorf("%s stacks the open count with the events: %s", title, raw)
		}
	}
	promRaw := asJSON(t, mustPanel(t, rendered(t, "prometheus"), "Pull requests over time")["fieldConfig"])
	if strings.Contains(promRaw, "Open that day") {
		t.Error("the SQL-only overrides reached the Prometheus dashboard")
	}
	sql := sqlOf(t, mustPanel(t, panels, "Work elsewhere"))
	if !strings.Contains(sql, `AS "Opened"`) || !strings.Contains(sql, `time AS "Seen"`) ||
		!strings.Contains(sql, "PARTITION BY full_name, kind, number ORDER BY time DESC") {
		t.Errorf("Work elsewhere dates the snapshot as the fact: %s", sql)
	}
}

// TestFirstReviewIsHuman: the tile read five seconds, the time a review bot
// takes, and the reviewer table crowned the author as the busiest reviewer of
// their own pull requests.
func TestFirstReviewIsHuman(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	// The value lives in the section's stat group now, one statement of
	// several, so every statement of the panel is read.
	if sql := everySQL(t, mustPanel(t, panels, "Merged and closed in range")); !strings.Contains(sql, "seconds_to_first_human_review") ||
		strings.Contains(sql, "seconds_to_first_review ") {
		t.Errorf("the value reads the first review by anyone: %s", sql)
	}
	for _, title := range []string{"Reviewers", "Reviews over time"} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if !strings.Contains(sql, "self = 'true'") || !strings.Contains(sql, "bot = 'true'") {
			t.Errorf("%s names the author and the bots as reviewers: %s", title, sql)
		}
	}
}

// TestSuccessRateExcludesTheUndecided: canceled and skipped runs counted as
// failures put a week at 76.7 in red; over success and failure it is 91.
func TestSuccessRateExcludesTheUndecided(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	p := mustPanel(t, panels, ciStats)
	sql := everySQL(t, p)
	if !strings.Contains(sql, "conclusion IN ('success', 'failure')") {
		t.Errorf("the denominator is every run: %s", sql)
	}
	if p["type"] != "stat" {
		t.Errorf("the success rate is a %v, and a gauge four units tall loses its arc on a phone", p["type"])
	}
	if !strings.Contains(sql, "conclusion IN ('"+cancelledRun+"', 'skipped')") {
		t.Errorf("the undecided runs are not counted apart: %s", sql)
	}
	prom := mustPanel(t, rendered(t, "prometheus"), ciStats)
	raw := asJSON(t, prom["targets"])
	if !strings.Contains(raw, `conclusion=~\"success|failure\"`) {
		t.Errorf("the Prometheus denominator is every run: %s", raw)
	}
}

// TestSuccessRateIsGreenFromNinety: the stat turned orange at 90.9 when the
// green step sat at ninety-five. Every store carries the same steps, the
// Elasticsearch one as fractions since its value is a mean of a boolean.
func TestSuccessRateIsGreenFromNinety(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		p := mustPanel(t, rendered(t, store), ciStats)
		if p["type"] == "text" {
			continue
		}
		// Inside a group the steps are the field's, not the panel's: a
		// panel-wide threshold would paint the run count beside the rate.
		thresholds, _ := overrideProperty(p, "Success rate", "thresholds").(map[string]any)
		steps, _ := thresholds["steps"].([]any)
		green := -1.0
		for _, raw := range steps {
			step, _ := raw.(map[string]any)
			if step["color"] != "green" {
				continue
			}
			switch v := step["value"].(type) {
			case int:
				green = float64(v)
			case float64:
				green = v
			}
		}
		if overrideProperty(p, "Success rate", "unit") == "percentunit" {
			green *= 100
		}
		if green != 90 {
			t.Errorf("%s: the success rate turns green at %v, want 90", store, green)
		}
	}
}

// TestOpenAlertsOverTimeSumTheNewestPerSeries: a MAX per severity took the
// largest series and ended at 4 under a tile saying 6.
func TestOpenAlertsOverTimeSumTheNewestPerSeries(t *testing.T) {
	t.Parallel()
	sql := sqlOf(t, mustPanel(t, rendered(t, "influxdb"), "Open alerts over time"))
	if !strings.Contains(sql, "PARTITION BY $__dateBin(time), repo, severity, ecosystem ORDER BY time DESC") ||
		!strings.Contains(sql, "SUM(open)") {
		t.Errorf("the curve takes the largest series rather than the sum of the newest: %s", sql)
	}
}

// TestSnapshotsAreDrawnAsHistoryOnlyWhereThereIsOne: the fork curve is
// rebuilt from each fork's own date, like the stars; the rate budget draws the
// share used, as the newest reading per bucket, so a bucket of fifteen thousand
// no longer flattens the two of five thousand.
func TestSnapshotsAreDrawnAsHistoryOnlyWhereThereIsOne(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	if sql := sqlOf(t, mustPanel(t, panels, "Forks over time")); !strings.Contains(sql, "gh_fork") ||
		strings.Contains(sql, "gh_repo ") {
		t.Errorf("the fork curve is the per-sweep reading: %s", sql)
	}
	p := mustPanel(t, panels, "Rate budget used")
	if sql := sqlOf(t, p); !strings.Contains(sql, "used_ratio") ||
		!strings.Contains(sql, "PARTITION BY $__dateBin(time), resource ORDER BY time DESC") {
		t.Errorf("the budget draws the remaining count rather than the newest share used: %s", sql)
	}
	if u := defaultsOf(t, p)["unit"]; u != "percentunit" {
		t.Errorf("the budget's unit is %v", u)
	}
}

// TestBarChartsFitTheirBars: a bar per repository in eight units of height
// was twenty two labels of seven pixels. The busy charts keep a handful and
// fold the rest into `other`, and every chart clips a long label.
func TestBarChartsFitTheirBars(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for title, n := range map[string]string{
		"Workflow runs, ever": "10", "Events by repository": "10",
		"Dependencies by license": "8", "Stars by repository": "12",
	} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if !strings.Contains(sql, "rn <= "+n) || !strings.Contains(sql, "'other'") {
			t.Errorf("%s draws every bar: %s", title, sql)
		}
	}
	if sql := sqlOf(t, mustPanel(t, panels, "Downloads by release")); !strings.Contains(sql, "LIMIT 12") {
		t.Errorf("Downloads by release draws every release: %s", sql)
	}
	for title, p := range panels {
		if p["type"] != "barchart" {
			continue
		}
		o, _ := p["options"].(map[string]any)
		if o["xTickLabelMaxLength"] != 22 {
			t.Errorf("%s does not clip long labels: %v", title, o["xTickLabelMaxLength"])
		}
	}
	o, _ := mustPanel(t, panels, "Commits by hour of day")["options"].(map[string]any)
	if o["xTickLabelSpacing"] != 100 {
		t.Errorf("the hour chart prints all twenty four labels: %v", o["xTickLabelSpacing"])
	}
}

// TestOtherRowsFoldsAndSortsLast pins the SQL shape the fold takes: the
// rank leaves the grouping as a column of its own, because DataFusion refuses
// to order by an aggregate the projection dropped.
func TestOtherRowsFoldsAndSortsLast(t *testing.T) {
	t.Parallel()
	got := otherRows(`SELECT repo AS "Repository", runs AS "Runs", ROW_NUMBER() OVER (ORDER BY runs DESC) AS rn FROM x`,
		"Repository", "Runs", 10)
	want := `SELECT "Repository", "Runs" FROM (SELECT CASE WHEN rn <= 10 THEN "Repository" ELSE 'other' END AS "Repository",` +
		` SUM("Runs") AS "Runs", MIN(rn) AS o FROM (SELECT repo AS "Repository", runs AS "Runs",` +
		` ROW_NUMBER() OVER (ORDER BY runs DESC) AS rn FROM x) z GROUP BY 1) w ORDER BY o`
	if got != want {
		t.Errorf("otherRows =\n%s\nwant\n%s", got, want)
	}
}

// TestDayColumnsAreWideEnough: at seventy pixels "6.43 years" read ".43
// years".
func TestDayColumnsAreWideEnough(t *testing.T) {
	t.Parallel()
	raw := asJSON(t, unitOf("Age", "d", 70))
	if !strings.Contains(raw, `"custom.width","value":100`) {
		t.Errorf("a day column at 70 pixels was allowed: %s", raw)
	}
	raw = asJSON(t, unitOf("Age", "d", 130))
	if !strings.Contains(raw, `"custom.width","value":130`) {
		t.Errorf("a wider day column was narrowed: %s", raw)
	}
	raw = asJSON(t, unitOf("Wait", "s", 70))
	if !strings.Contains(raw, `"custom.width","value":70`) {
		t.Errorf("a column of seconds was widened: %s", raw)
	}
}

// TestColumnsNoLongerTruncate: the workflow column showed the directory and
// not the file, the ruleset changelog carried two internal ids that pushed the
// date's seconds off the panel, and the half-width tables carried headings
// wider than their panels.
func TestColumnsNoLongerTruncate(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	if sql := sqlOf(t, mustPanel(t, panels, "Workflows")); !strings.Contains(sql, "REPLACE(workflow, '.github/workflows/', '')") {
		t.Errorf("the workflow column shows the directory: %s", sql)
	}
	if sql := sqlOf(t, mustPanel(t, panels, "Ruleset changes")); strings.Contains(sql, "actor_id") || strings.Contains(sql, "version_id") {
		t.Errorf("the changelog carries GitHub's internal ids: %s", sql)
	}
	for _, title := range []string{"Branch protection rules", "Deployments by environment"} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if strings.Contains(sql, "Conversation resolution") || strings.Contains(sql, "Median time to status") {
			t.Errorf("%s keeps a heading wider than a half-width panel: %s", title, sql)
		}
	}
	raw := asJSON(t, mustPanel(t, panels, "Slowest steps")["fieldConfig"])
	if strings.Contains(raw, `"options":"Step"`) {
		t.Error("the first column of the full-width steps table is pinned, so the rest of the panel is blank")
	}
}

// TestTitlesFitAPhone: a title longer than this is cut at 430 pixels.
func TestTitlesFitAPhone(t *testing.T) {
	t.Parallel()
	const widest = 29
	for title := range rendered(t, "influxdb") {
		if len(title) > widest {
			t.Errorf("%q is %d characters, and a phone cuts a title after %d", title, len(title), widest)
		}
	}
}

// TestSeriesAreNamedAndColored: legends read the raw tag, "true" for the bot
// threads and CLOSED for the issues, and the palette put FAILURE in green.
func TestSeriesAreNamedAndColored(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	if sql := sqlOf(t, mustPanel(t, panels, "Review threads over time")); !strings.Contains(sql, "'Bot'") || !strings.Contains(sql, "'Human'") {
		t.Errorf("the thread series is named by the raw tag: %s", sql)
	}
	for title, want := range map[string][]string{
		"Commits by gate state":     {`"options":"FAILURE"`, `"fixedColor":"red"`, `"options":"SUCCESS"`, `"fixedColor":"green"`},
		"Runs by outcome over time": {`"options":"failure"`, `"options":"success"`},
		"Open alerts over time":     {`"options":"critical"`, `"options":"high"`},
	} {
		raw := asJSON(t, mustPanel(t, panels, title)["fieldConfig"])
		for _, w := range want {
			if !strings.Contains(raw, w) {
				t.Errorf("%s lacks %s in its overrides", title, w)
			}
		}
	}
	// `repo` is the short name on every measurement now, so this column is
	// read rather than parsed. The window still partitions by the full name,
	// because two owners can use one short name and the newest row has to be
	// picked per repository.
	if sql := sqlOf(t, mustPanel(t, panels, "Commits by repository")); strings.Contains(sql, "regexp_replace") ||
		!strings.Contains(sql, `SELECT repo AS "Repository"`) ||
		!strings.Contains(sql, "PARTITION BY full_name ORDER BY time DESC") {
		t.Errorf("the one table that shows the short name reads it rather than parsing it: %s", sql)
	}
	for _, title := range []string{"Community profile", "Repository settings", "Policy files", "Discussions"} {
		raw := asJSON(t, mustPanel(t, panels, title)["fieldConfig"])
		if !strings.Contains(raw, `"text":"yes"`) {
			t.Errorf("%s shows its booleans as 1 and 0", title)
		}
	}
}

// TestInventoriesIgnoreTheRange: the sponsorships table is an inventory and
// read No data beside a tile saying one, because its only row was two days
// outside the range.
func TestInventoriesIgnoreTheRange(t *testing.T) {
	t.Parallel()
	sql := sqlOf(t, mustPanel(t, rendered(t, "influxdb"), "Sponsorships"))
	if strings.Contains(sql, "$__timeFilter") || !strings.Contains(sql, wholeHistory) {
		t.Errorf("the sponsorships table takes the dashboard range: %s", sql)
	}
}

// TestNewFieldsAreShown: the run number, the pull request's title and labels
// and the advisory's summary, collected for these tables, are in them.
func TestNewFieldsAreShown(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for title, cols := range map[string][]string{
		"Commits behind a red branch":  {`run_number AS "Run number"`},
		"Open the longest":             {`title AS "Title"`, `label_names AS "Labels"`},
		"Largest merged pull requests": {`title AS "Title"`, `label_names AS "Labels"`, `author_association AS "Association"`},
		"Time to resolve an alert":     {`summary AS "Advisory"`, `COALESCE(cvss_v4, cvss) AS "CVSS"`, `alert_state AS "Outcome"`},
	} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		for _, c := range cols {
			if !strings.Contains(sql, c) {
				t.Errorf("%s lacks %s: %s", title, c, sql)
			}
		}
	}
}

// TestGroupingNeverSplitsOnTheURL: a repository renamed inside the range has
// two urls, and a GROUP BY that included it was two rows.
func TestGroupingNeverSplitsOnTheURL(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for _, title := range []string{"Every repository, ever", "Clone amplification"} {
		sql := sqlOf(t, mustPanel(t, panels, title))
		if !strings.Contains(sql, "MAX(url)") || regexp.MustCompile(`GROUP BY 1, \d`).MatchString(sql) {
			t.Errorf("%s groups on the url: %s", title, sql)
		}
	}
}

// TestToPGLeavesQuotedIdentifiersAlone: the reserved-word pass rewrote every
// occurrence of `by`, `user`, `key`, `limit` and `check` in the statement,
// including the ones inside a quoted identifier. A stat group names its
// values, one of them "Covered by the plan", and PostgreSQL was handed
// `AS "Covered "by" the plan"` and answered `syntax error at or near "by"`.
// Only the containerised suite saw it, because the offline checks never send
// the statement to a server.
func TestToPGLeavesQuotedIdentifiersAlone(t *testing.T) {
	t.Parallel()
	got := toPG(`SELECT SUM(discount) AS "Covered by the plan" FROM gh_billing_usage`)
	want := `SELECT SUM(discount) AS "Covered by the plan" FROM gh_billing_usage`
	if got != want {
		t.Errorf("toPG = %q, want %q", got, want)
	}
	// A reserved word outside a quoted identifier is still quoted, and a
	// statement that has both gets one treatment each.
	got = toPG(`SELECT "user", key FROM gh_key WHERE key = 'a "quoted" key'`)
	want = `SELECT "user", "key" FROM gh_key WHERE "key" = 'a "quoted" key'`
	if got != want {
		t.Errorf("toPG = %q, want %q", got, want)
	}
}

// TestToPGRenamesEveryReferenceToTheSeriesColumn: the translation renamed the
// alias (" AS series") and nothing else, so a query that wraps a subquery and
// selects the column by name read `SELECT time, series, used FROM (... AS
// metric ...)`. PostgreSQL answered `column "series" does not exist` on
// "Rate budget used", which had just grown that wrapper.
func TestToPGRenamesEveryReferenceToTheSeriesColumn(t *testing.T) {
	t.Parallel()
	got := toPG("SELECT time, series, used FROM (SELECT resource AS series, x AS used FROM gh_rate_limit) y")
	want := "SELECT time, metric, used FROM (SELECT resource AS metric, x AS used FROM gh_rate_limit) y"
	if got != want {
		t.Errorf("toPG = %q, want %q", got, want)
	}
}

// TestEveryRatioIsComputedInFloatingPoint: "Clones each" divided one SUM of an
// integer column by another, which DataFusion answers with integer division.
// The only panel about a ratio therefore floored it on InfluxDB, the store
// production reads: 122152 clones over 1650 cloners drew 74 rather than 74.03,
// and 817 over 205 drew 3 rather than 3.99, while PostgreSQL, whose columns
// are double precision, drew the real number. Nothing said the two dashboards
// would differ, and the containerised comparison is what noticed.
//
// The rule rather than the one panel: a division whose result is not a whole
// number has to start from a float, which is what the 100.0 in the two
// percentage panels already does.
func TestEveryRatioIsComputedInFloatingPoint(t *testing.T) {
	t.Parallel()
	// A ratio in this specification is a division by a NULLIF guard. Every
	// one of them must have something floating point to its left.
	ratio := regexp.MustCompile(`(?s)(.{0,120})/ NULLIF\(`)
	for _, store := range []string{"influxdb", "postgres"} {
		for title, p := range rendered(t, store) {
			for _, m := range ratio.FindAllStringSubmatch(asJSON(t, p["targets"]), -1) {
				if !strings.Contains(m[1], "1.0") && !strings.Contains(m[1], "100.0") {
					t.Errorf("%s of %s divides integers and truncates: %s", title, store, m[1])
				}
			}
		}
	}
}

// TestCumulativeCurvesSpanTheWholeRange: a running count over dated rows has
// a point only in a bucket that holds one, so the star and fork curves ended
// on the day of the last star or fork and left the rest of the range blank,
// which reads as collection having stopped. Both ends carry a bucket of zero
// so the line runs from the start of the range to its end.
func TestCumulativeCurvesSpanTheWholeRange(t *testing.T) {
	t.Parallel()
	// The bucket column is a timestamp in InfluxDB and the epoch seconds the
	// PostgreSQL bucket macro returns, so each store anchors in its own.
	for store, edges := range map[string][2]string{
		"influxdb": {"$__timeFrom()", "$__timeTo()"},
		"postgres": {"$__unixEpochFrom()", "$__unixEpochTo()"},
	} {
		panels := rendered(t, store)
		for _, title := range []string{"Stars over time", "Forks over time"} {
			sql := sqlOf(t, mustPanel(t, panels, title))
			for _, edge := range edges {
				if !strings.Contains(sql, "UNION ALL SELECT "+edge+", 0") {
					t.Errorf("%s, %s: the curve is not anchored at %s: %s", store, title, edge, sql)
				}
			}
			if desc, _ := mustPanel(t, panels, title)["description"].(string); !strings.Contains(desc, "end of the range") {
				t.Errorf("%s, %s: the description does not say the curve holds to the end: %q", store, title, desc)
			}
		}
	}
}

// preRange is the scalar subquery a running count starts from: what the rows
// dated before the range add up to.
var preRange = regexp.MustCompile(`\(SELECT (.+?) FROM (\w+) WHERE time < \$__timeFrom\(\)`)

// TestASummedClimbStartsFromZeroRatherThanNull: the star curve adds up
// gh_star_day's `stars` where it used to count gh_star's rows. COUNT over no
// rows is 0, SUM over no rows is NULL, and NULL plus the running sum is NULL
// at every point, so a range that begins before a repository's first star
// drew nothing at all. Every climb whose start is a SUM has to COALESCE it.
func TestASummedClimbStartsFromZeroRatherThanNull(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		summed := 0
		for title, p := range rendered(t, store) {
			targets, _ := p["targets"].([]any)
			for _, raw := range targets {
				target, _ := raw.(map[string]any)
				sql, _ := target["rawSql"].(string)
				for _, m := range preRange.FindAllStringSubmatch(sql, -1) {
					start := m[1]
					if start == "COUNT(*)" {
						continue
					}
					summed++
					if !strings.HasPrefix(start, "COALESCE(") || !strings.HasSuffix(start, ", 0)") {
						t.Errorf("%s %q starts its curve from %s, which is NULL on a range "+
							"with nothing before it and blanks the whole curve", store, title, start)
					}
				}
			}
		}
		if summed == 0 {
			t.Errorf("%s: no climb starts from a sum, so this checked nothing; the star "+
				"curve is meant to add up gh_star_day", store)
		}
	}
}

// TestTheForkClimbIsTheStatementItWas: the climb was generalised so the star
// curve could sum a day's count, and the forks, one row per fork, pass
// COUNT(*) on both sides. The five dashboard files are what a user imports,
// so the generalisation must not have touched a byte of the fork curve.
func TestTheForkClimbIsTheStatementItWas(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"influxdb": `SELECT time, (SELECT COUNT(*) FROM gh_fork WHERE time < $__timeFrom() ` +
			`AND repo IN (${repo:singlequote})) + SUM(n) OVER (ORDER BY time) AS "Forks" FROM ` +
			`(SELECT time, SUM(n) AS n FROM (SELECT $__dateBin(time) AS time, COUNT(*) AS n ` +
			`FROM gh_fork WHERE $__timeFilter(time) AND repo IN (${repo:singlequote}) GROUP BY 1 ` +
			`UNION ALL SELECT $__timeFrom(), 0 UNION ALL SELECT $__timeTo(), 0) a GROUP BY 1) x ` +
			`ORDER BY time`,
		"postgres": `SELECT time, (SELECT COUNT(*) FROM gh_fork WHERE time < $__timeFrom() ` +
			`AND repo IN (${repo:sqlstring})) + SUM(n) OVER (ORDER BY time) AS "Forks" FROM ` +
			`(SELECT time, SUM(n) AS n FROM (SELECT $__timeGroupAlias(time, $__interval), ` +
			`COUNT(*) AS n FROM gh_fork WHERE $__timeFilter(time) AND repo IN (${repo:sqlstring}) ` +
			`GROUP BY 1 UNION ALL SELECT $__unixEpochFrom(), 0 UNION ALL SELECT $__unixEpochTo(), 0) ` +
			`a GROUP BY 1) x ORDER BY time`,
	}
	for store, sql := range want {
		if got := sqlOf(t, mustPanel(t, rendered(t, store), "Forks over time")); got != sql {
			t.Errorf("%s: the fork curve changed:\n got %s\nwant %s", store, got, sql)
		}
	}
}

// readsStar and readsStarDay tell the two star measurements apart in any
// store's query: a table or index name in SQL and Elasticsearch, a path in
// Graphite. gh_star is followed by something that cannot continue a name,
// so gh_star_day, gh_star_given and gh_star_list are not it.
var (
	readsStar    = regexp.MustCompile(`\bgh_star(?:[^_a-z0-9]|$)|\bgithub\.star\.`)
	readsStarDay = regexp.MustCompile(`\bgh_star_day\b|\bgithub\.star_day\.`)
)

// inRangeStarDay is the aggregate a climb over gh_star_day takes per time
// bucket inside the range, the n its running SUM(n) OVER adds up.
var inRangeStarDay = regexp.MustCompile(`(\w+\([^()]*\)) AS n FROM gh_star_day WHERE \$__timeFilter\(time\)`)

// TestTheStarCountsReadTheDailyHistory: since July 2026 GitHub serves the
// stargazer list behind gh_star only to a repository's admins and
// collaborators, and gh_star_day is the daily history it serves for every
// repository. The two panels that count stars read the history wherever the
// store can hold it, and Recent stars keeps gh_star, the only measurement
// that names who starred, except in Graphite, which keeps no names and
// counts from the history too.
func TestTheStarCountsReadTheDailyHistory(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		store, title string
		day, star    bool
	}{
		{"influxdb", "Stars gained over time", true, false},
		{"influxdb", "Stars over time", true, false},
		{"influxdb", "Recent stars", false, true},
		{"postgres", "Stars gained over time", true, false},
		{"postgres", "Stars over time", true, false},
		{"postgres", "Recent stars", false, true},
		{"graphite", "Stars gained over time", true, false},
		{"graphite", "Recent stars", true, false},
		{"elasticsearch", "Stars gained over time", true, false},
		{"elasticsearch", "Recent stars", false, true},
		// Graphite cannot add the stars from before the range to a sum
		// inside it, and no Elasticsearch pipeline has been validated for
		// it, so both keep the per-sweep reading of gh_repo.
		{"graphite", "Stars over time", false, false},
		{"elasticsearch", "Stars over time", false, false},
	} {
		raw := asJSON(t, mustPanel(t, rendered(t, c.store), c.title)["targets"])
		if got := readsStarDay.MatchString(raw); got != c.day {
			t.Errorf("%s %q reads gh_star_day: %v, want %v: %s", c.store, c.title, got, c.day, raw)
		}
		if got := readsStar.MatchString(raw); got != c.star {
			t.Errorf("%s %q reads gh_star: %v, want %v: %s", c.store, c.title, got, c.star, raw)
		}
	}
	// A day of the history is a count, so every store adds it up rather
	// than counting the rows, which would give a day of three stars one.
	for _, store := range []string{"influxdb", "postgres"} {
		if sql := sqlOf(t, mustPanel(t, rendered(t, store), "Stars gained over time")); !strings.Contains(sql, "SUM(stars)") {
			t.Errorf("%s counts the days rather than adding up their stars: %s", store, sql)
		}
	}
	// The running total has two aggregates, and a SUM(stars) anywhere in it
	// would be satisfied by the start alone, which
	// TestASummedClimbStartsFromZeroRatherThanNull holds. This is the climb
	// inside the range. Counting rows there costs more than on the panel
	// above: page one of every history stores each day of its thirty weeks,
	// zeros included, so a repository without a single star would climb by
	// one a day.
	for _, store := range []string{"influxdb", "postgres"} {
		sql := sqlOf(t, mustPanel(t, rendered(t, store), "Stars over time"))
		m := inRangeStarDay.FindStringSubmatch(sql)
		switch {
		case m == nil:
			t.Errorf("%s: Stars over time adds nothing up from gh_star_day inside the range: %s", store, sql)
		case m[1] != "SUM(stars)":
			t.Errorf("%s: Stars over time climbs by %s a bucket rather than adding up its stars: %s", store, m[1], sql)
		}
	}
	es := asJSON(t, mustPanel(t, rendered(t, "elasticsearch"), "Stars gained over time")["targets"])
	if !strings.Contains(es, `{"field":"stars","id":`) || !strings.Contains(es, `"type":"sum"`) {
		t.Errorf("Elasticsearch counts the days rather than adding up their stars: %s", es)
	}
	gr := asJSON(t, mustPanel(t, rendered(t, "graphite"), "Stars gained over time")["targets"])
	if strings.Contains(gr, "isNonNull") {
		t.Errorf("Graphite turns each day into a 1 before summing it: %s", gr)
	}
	// Prometheus has neither: the reduction skips gh_star_day, a history with
	// no current value, so the exporter's count of gh_star is what it has.
	for _, title := range []string{"Stars gained over time", "Recent stars"} {
		raw := asJSON(t, mustPanel(t, rendered(t, "prometheus"), title)["targets"])
		if !strings.Contains(raw, "github_stars_gained_total") || strings.Contains(raw, "star_day") {
			t.Errorf("Prometheus %q no longer reads the stars gained the exporter publishes: %s", title, raw)
		}
	}
}

// TestNoPanelCountsAStarTwice: gh_star and gh_star_day hold the same stars
// twice over, on different days (the history buckets by GitHub's Pacific
// calendar day, gh_star by the UTC instant) and with different memories (an
// unstar is taken off the history and kept in gh_star). A panel that read
// both would count every star a repository has twice, so none may.
func TestNoPanelCountsAStarTwice(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		for title, p := range rendered(t, store) {
			raw := asJSON(t, p["targets"])
			if readsStar.MatchString(raw) && readsStarDay.MatchString(raw) {
				t.Errorf("%s %q reads both gh_star and gh_star_day: %s", store, title, raw)
			}
		}
	}
}

// TestEveryPanelReadingTheArtifactSizeSaysWhatItIsOver is the gate on a
// number that is right and reads as something else.
//
// `live_bytes` is the size of the artifacts the walk reached, and the walk
// stops at five hundred: on the account this was developed against it read
// 11.5 GB beside a declared 29,361, short by a factor of fifty six, with
// nothing beside it saying so. `count` is not its denominator either, since
// GitHub counts the artifacts it has already expired in it. So a panel that
// shows the size either shows the counts it is over or says in its
// description that it is a floor. Both is what the two panels do.
func TestEveryPanelReadingTheArtifactSizeSaysWhatItIsOver(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		panels := rendered(t, store)
		read := 0
		for title, p := range panels {
			sql := allSQL(p)
			if !strings.Contains(sql, "live_bytes") {
				continue
			}
			read++
			desc, _ := p["description"].(string)
			counted := strings.Contains(sql, "walked") && strings.Contains(sql, "live_count")
			if !counted && !strings.Contains(desc, "floor") {
				t.Errorf("%s: %q shows the artifact size over the walked artifacts alone,"+
					" without the counts that say so and without calling it a floor:\n%s",
					store, title, desc)
			}
		}
		if read == 0 {
			t.Errorf("%s: no panel reads live_bytes, so this checks nothing", store)
		}
	}
}

// allSQL is every statement of a panel, and nothing when it has none.
func allSQL(p map[string]any) string {
	targets, _ := p["targets"].([]any)
	var out []string
	for _, raw := range targets {
		target, _ := raw.(map[string]any)
		if sql, _ := target["rawSql"].(string); sql != "" {
			out = append(out, sql)
		}
	}
	return strings.Join(out, "\n")
}

// TestEveryStoreExcludesTheSentinelTheSameWay holds the three spellings of one
// exclusion together.
//
// A charge that belongs to no repository, which is what a Copilot seat is,
// carries the `(none)` a collector writes for a tag GitHub gives nothing for.
// A table of repositories leaves it out, and each store spells that
// differently: the SQL stores compare the value, Graphite matches the path
// node, where the parentheses are not path characters and become underscores,
// and Elasticsearch negates a term. All three were written from a guess and
// all three were wrong, so the seat was listed as a repository called (none)
// in all five stores, and nothing failed: the generated files match the
// specification whether the string in it is right or not.
//
// Every expectation below is written out rather than built from the constants
// the specification uses. Derived from them it would pass whatever they said,
// which is the whole of what went wrong here. The same three shapes are used
// by the dependency changes panel, which is why the specification keeps one
// constant each.
func TestEveryStoreExcludesTheSentinelTheSameWay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		store, want string
		sql         bool
	}{
		{store: "influxdb", want: `repo <> '(none)'`, sql: true},
		{store: "postgres", want: `repo <> '(none)'`, sql: true},
		// Inside the JSON the pattern's backslash is escaped once more.
		{store: "graphite", want: `exclude(aliasByNode(github.billing_usage.*.*.*.*.*.*.*.gross, 5, 6), \"^_none_\\.\")`},
		{store: "elasticsearch", want: `NOT repo.keyword:\"(none)\"`},
		// The fifth store, added after it was found still asking for a label
		// that is not empty while the other four had been corrected. The
		// sentinel is not empty, so the seat was a repository called (none)
		// here and nowhere else, and this list not having a Prometheus row is
		// the whole reason nothing said so.
		{store: "prometheus", want: `github_billing_usage_gross{repo!=\"(none)\"}`},
	} {
		panel := mustPanel(t, rendered(t, tc.store), "Usage by repository")
		// The SQL is read as SQL: inside the JSON its comparison operator is
		// escaped and the test would be asserting on the escaping.
		got := asJSON(t, panel["targets"])
		if tc.sql {
			got = allSQL(panel)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: the cost table does not exclude the unattributed charge with %s:\n%s",
				tc.store, tc.want, got)
		}
	}
	// And the value the collectors write, read from where they write it:
	// everything above is that value in three disguises, and a sentinel that
	// drifted between the two halves of the repository would leave all three
	// excluding nothing again, silently, which is what this test is for.
	if written := collectorSentinel(t); noneValue != written {
		t.Errorf("the specification excludes %q and the collectors write %q", noneValue, written)
	}
}

// TestTheCollectorScopesAreSpelledAsTheCollectorWritesThem: both halves of
// the collector's own row select on gh_collector_family's `scope` tag, so a
// spelling that drifted from the collector's would leave both tables empty on
// a store that is full, and nothing would say so. The same hole the sentinel
// above had, closed the same way.
func TestTheCollectorScopesAreSpelledAsTheCollectorWritesThem(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"scopeFamily": scopeFamilyTag,
		"scopeRepo":   scopeRepoTag,
	} {
		if written := collectConst(t, "health.go", name); written != want {
			t.Errorf("the specification selects %q and the collectors write %q for %s",
				want, written, name)
		}
	}
}

// collectorSentinel is the value of noneTag in internal/collect, read from its
// source.
//
// Read rather than imported: the dashboard specification is generated by a
// command that has no business linking the collectors in, and the one thing it
// needs from them is this string.
func collectorSentinel(t *testing.T) string {
	t.Helper()
	return collectConst(t, "strings.go", "noneTag")
}

// collectConst is the value of one string constant of internal/collect, read
// from the file that declares it.
//
// Read rather than imported, for the reason collectorSentinel gives: the
// command that generates this specification has no business linking the
// collectors in, and the few strings it needs from them are these.
func collectConst(t *testing.T, file, name string) string {
	t.Helper()
	path := filepath.Join("..", "collect", file)
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("cannot read internal/collect/%s: %v", file, err)
	}
	var got string
	ast.Inspect(parsed, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != name || len(spec.Values) != 1 {
			return true
		}
		lit, isLiteral := spec.Values[0].(*ast.BasicLit)
		if !isLiteral || lit.Kind != token.STRING {
			return true
		}
		value, unquoteErr := strconv.Unquote(lit.Value)
		if unquoteErr != nil {
			t.Fatalf("%s is not a plain string literal: %s", name, lit.Value)
		}
		got = value
		return false
	})
	if got == "" {
		t.Fatalf("no %s constant in %s, so this checks nothing", name, path)
	}
	return got
}
