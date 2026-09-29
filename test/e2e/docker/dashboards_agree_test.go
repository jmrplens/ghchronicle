//go:build dockere2e

package docker

import (
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// ── 3. The stores draw the same thing ───────────────────────────────────────

// The five dashboards are one specification over one sweep of the same
// fixtures, so a panel that draws a different value in two stores is either a
// defect in one of them or a difference the panel's own description owes the
// reader. The 2.6.1 review found eleven of the first kind by putting the five
// dashboards side by side on a screen: a table drawing seven rows for one
// repository, stat tiles without their units, a deployment count of one
// against two, a list that ignored its own threshold, pie slices all called
// "Events". This is that look, done on every run.
//
// It compares what a reader sees rather than what a query answers. Every
// panel's answer goes through grafana.Draw, which replays what Grafana does
// between /api/ds/query and the screen: the Prometheus datasource's own
// reshaping, the panel's transformations and its field overrides. The frames
// alone would have shown none of those eleven: the rows came from a merge,
// the units from the order of the overrides, the names from a displayName.
//
// Then every pair of stores is compared on the panel: a stat, a gauge, a bar
// gauge and a pie by the values they draw, each with its unit and the text it
// shows for nothing; a table by the rows it draws over the columns both
// stores have, and apart from that by the order it heads them in and by the
// columns one store lacks; a bar chart by its bars, the name each is drawn
// under and its length. A time series is not compared, nor a bar chart over
// time: each store buckets those by a step of its own, which is not a
// difference a reader can see.
//
// Four things are the harness's and not the dashboards', and each is absorbed
// where it arises rather than listed:
//
//   - The exporter lives for the length of the test, so a Prometheus panel
//     built on a range function or drawn over time has half a minute of
//     history behind it and answers nothing, or a count of zero. Those are
//     left out of the comparison exactly as promNeedsHistory leaves them out
//     of the second question.
//   - The stores were loaded by three sweeps a minute apart, and a value the
//     collector computes from its own clock, how long a pull request has been
//     open, differs between them by the time between the sweeps. The sweeps'
//     own points say how far each field moved, and that is the slack the
//     field's column is compared with; a field that did not move gets none.
//   - The stores are asked one after another, and a column a query computes
//     from now() moves by as much as the time between two stores' answers,
//     which the run records and allows.
//   - Graphite holds a name as a path node, and its page says so once: a name
//     is the same when Graphite's is the node the other store's name becomes.
//     Where a panel's Graphite description says it names each row from the
//     path, the row's name is several nodes joined by dots, and one of them
//     has to be that node.
//
// What is left is either fixed or listed in dashboardsDiffer below.

// dashboardDiffer is one panel on which some stores draw what the others do
// not, for a reason the panel states.
type dashboardDiffer struct {
	// title is the panel's, since an ordinal moves every time a panel is
	// added above it.
	title string
	// kind is the panel's type, for a title two panels share: the Overview's
	// stat group and the Inventory table are both "Repositories". Empty, the
	// entry is about every panel of its title.
	kind string
	// stores are the stores the reason is about. A difference between two
	// stores is excused when either of them is named here, so each of them
	// has to draw the difference: dashboardDifferTooWide reports one that
	// draws the panel as the stores the entry does not name.
	stores []string
	// reason is words of each named store's own description of the panel,
	// verbatim. The test holds it to the description, so an entry cannot
	// outlive the sentence that justified it, and holds it to a difference,
	// so it cannot outlive the difference either.
	reason string
	// only is the tiles or columns the reason is about, when it is about some
	// of them: the rest of the panel is still compared, so a repository named
	// in full cannot hide a count that is wrong beside it. Empty, the reason
	// is about which rows there are, and the panel is not compared further.
	only []string
}

// dashboardsDiffer is every panel the stores draw differently on purpose.
// Each entry names a panel, the stores and the reason that panel's own
// description gives a reader, and nothing here excuses a difference the
// description does not state: a store that draws a different number without
// saying so is fixed rather than listed.
//
// Most of them are what a store can hold at all. Graphite keeps a number per
// path and no string beside it, and cannot list by date or read outside the
// range it is asked for; Elasticsearch answers inside the range and buckets
// by what it can aggregate; the Prometheus exporter keeps a series per thing
// that can move, and drops what would be a series per item.
var dashboardsDiffer = []dashboardDiffer{
	// The row per repository and day of the star history, where the SQL
	// stores list who starred; this fixture has no star by name in the range.
	{
		title: "Recent stars", stores: []string{"graphite"},
		reason: "Graphite has no way to sort by date, so this is the stars gained per repository over the range instead",
	},
	// A Graphite series, an Elasticsearch bucket and a Prometheus label are
	// each the grouping key and the label shown, so two owners' repositories
	// of one name stay two rows.
	{
		title: "Commits by repository", stores: []string{"graphite"},
		reason: "Named in full here: a Graphite series is labeled by the node it is grouped under, " +
			"so the owner cannot be dropped without merging two repositories that share a short name",
		only: []string{"Repository"},
	},
	{
		title: "Commits by repository", stores: []string{"elasticsearch"},
		reason: "Named in full here, for the reason Graphite gives: the terms bucket is both the grouping key and the label",
		only:   []string{"Repository"},
	},
	{
		title: "Commits by repository", stores: []string{"prometheus"},
		reason: "Named in full here, for the reason Graphite gives: the label summed by is the label shown",
		only:   []string{"Repository"},
	},
	// The two years before this one are outside the thirty days.
	{
		title: "Contributions by year", stores: []string{"graphite", "elasticsearch"},
		reason: "answers only inside the dashboard range, so years outside it are missing",
	},
	// Two of the four jobs queued in the same hour: 50 where the others
	// read 47.5.
	{
		title: "Runs in range", stores: []string{"graphite"},
		reason: "In Graphite two facts landing in the same storage slot of one series are reduced to " +
			"one point, so the medians are over what the storage kept",
		only: []string{"Queue wait"},
	},
	{
		title: "Force pushes", stores: []string{"graphite"},
		reason: "Graphite has no way to sort by date, so this counts them per repository, branch and actor over the range",
	},
	{
		title: "Discussions", stores: []string{"graphite"},
		reason: "cannot group by the answered flag, so each category is one row and Answered is the " +
			"share of its discussions that have an answer",
		only: []string{"Answered"},
	},
	// The one answer is from July, which the SQL stores list whatever the
	// range.
	{
		title: "Answers elsewhere", stores: []string{"graphite", "elasticsearch"},
		reason: "answers only inside the dashboard range, so years outside it are missing",
	},
	{
		title: "Webhook endpoints", stores: []string{"graphite", "elasticsearch"},
		reason: "the failures are their own rows",
	},
	{
		title: "Deploy keys", stores: []string{"graphite", "prometheus"},
		reason: "so such a key has no row here",
	},
	{
		title: "Ruleset changes", stores: []string{"graphite"},
		reason: "Graphite has no way to list by date, so this is the number of versions per repository, ruleset and actor type over the range",
	},
	{
		title: "Ruleset changes", stores: []string{"prometheus"},
		reason: "Prometheus counts versions per ruleset and actor type and drops the dates",
	},
	// v1.3.0-rc1, which nobody has downloaded.
	{
		title: "Downloads", stores: []string{"elasticsearch"},
		reason: "In Elasticsearch the second counts distinct tags, downloaded or not: a cardinality " +
			"cannot be filtered on the newest value",
		only: []string{"Releases"},
	},
	{
		title: "Downloads by release", stores: []string{"graphite"},
		reason: "Graphite names each bar repository and tag from the path.", only: []string{grafana.BarAxis},
	},
	{
		title: "Downloads by release", stores: []string{"elasticsearch"},
		reason: "In Elasticsearch the bars are named by tag; the repository is the next column",
		only:   []string{grafana.BarAxis},
	},
	// The two code scanning alerts.
	{
		title: "Oldest open alerts", stores: []string{"elasticsearch"},
		reason: "Elasticsearch lists the Dependabot alerts alone, the two families being two indices",
	},
	// The tags were published before the thirty days.
	{
		title: "Container tags published", stores: []string{"graphite"},
		reason: "Graphite has no way to list by date, so this counts the tags published per package over the range",
	},
	// Both files were last changed before the thirty days.
	{
		title: "Policy files", stores: []string{"graphite", "elasticsearch"},
		reason: "answers only inside the dashboard range, so years outside it are missing",
	},
	// The one sponsorship began in 2021.
	{
		title: "Sponsorships", stores: []string{"graphite", "elasticsearch"},
		reason: "answers only inside the dashboard range, so years outside it are missing",
	},
	{
		title: "Sponsorships", stores: []string{"prometheus"},
		reason: "Prometheus counts sponsorships per direction and drops the other party",
	},
	{
		title: "Achievements", stores: []string{"graphite", "elasticsearch"},
		reason: "names each row by the badge's slug", only: []string{"Achievement"},
	},
	{
		title: "Achievements", stores: []string{"prometheus"},
		reason: "keeps the badge by its slug", only: []string{"Achievement"},
	},
	{
		title: "Achievement progress", stores: []string{"graphite", "elasticsearch"},
		reason: "names each row by the badge's slug", only: []string{"Achievement"},
	},
	{
		title: "Achievement progress", stores: []string{"prometheus"},
		reason: "keeps the badge by its slug", only: []string{"Achievement"},
	},
}

func TestTheStoresDrawTheSameValues(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	c := &dashboardComparison{
		run: run, entries: dashboardsDiffer, drift: sweepDrift(run.sweeps),
		used: map[int]bool{}, compared: map[string]map[string]bool{},
	}
	indexes := map[int]bool{}
	for _, store := range dashboardStores {
		for i := range run.outcomes[store.name] {
			indexes[i] = true
		}
	}
	for _, index := range slices.Sorted(maps.Keys(indexes)) {
		if failures := c.panel(t, index); len(failures) > 0 {
			t.Errorf("panel %d %q draws differently in stores whose descriptions do not say why, so a "+
				"reader comparing two dashboards would be right to think one is wrong:\n  %s",
				index, dashboardPanelTitle(run, index), strings.Join(failures, "\n  "))
		}
	}
	stale, unasked := dashboardDifferProblems(dashboardsDiffer, run, c.used, c.compared)
	for _, problem := range append(stale, dashboardDifferTooWide(dashboardsDiffer, c.alike)...) {
		t.Error(problem)
	}
	for _, note := range unasked {
		t.Log(note)
	}
	if err := os.WriteFile(filepath.Join("out", "dashboards", "agree.txt"), []byte(c.report.String()), 0o600); err != nil {
		t.Errorf("keeping the comparison: %v", err)
	}
	t.Logf("%d pairs of stores drew the same panel, and %d of them drew the same values", c.pairs, c.agreed)
	// A floor rather than a measurement, so that a change that quietly stops
	// the comparison drawing anything fails instead of passing with nothing
	// asked. Measured at 866 pairs, 771 of them alike, on the 2.6.2 branch.
	if c.pairs < 500 {
		t.Errorf("only %d pairs of stores were compared, which is too few for this to have asked "+
			"anything: the sweep, the dashboards or the replay have changed shape", c.pairs)
	}
}

// TestTheSQLStoresDrawTheRowsInOneOrder holds InfluxDB and PostgreSQL to the
// order of what they draw, which the comparison above leaves out: they run one
// statement, translated between dialects, over the rows of one sweep, so a
// table or a bar chart they draw with the same rows draws them in the order
// the statement gives. Where it gives none, each store's sort breaks the tie
// its own way. Measured on the 2.6.2 branch before the statements named their
// tie-breakers, twelve and then eleven of the 49 panels both stores draw with
// more than one row drew the same rows in another order on two runs: two open
// items elsewhere, two authors with one pull request each and two ecosystems
// of one repository among them.
//
// Where it sorts by text, the order is also the collation's: InfluxDB compares
// bytes and PostgreSQL the database's collation, unless the translation says
// bytes. This suite's PostgreSQL is a linguistic one for that reason (see the
// compose file), and on it three panels drew another order until it did.
// TestThePostgreSQLDashboardLeavesNoTextToTheCollation holds the rest, whose
// rows here happen to sort alike either way.
func TestTheSQLStoresDrawTheRowsInOneOrder(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	drift := sweepDrift(run.sweeps)
	apart := askedApart(run, "influxdb", "postgres")
	held := 0
	for _, index := range slices.Sorted(maps.Keys(run.outcomes["influxdb"])) {
		pictures := dashboardPictures(t, run, index)
		a, okA := pictures["influxdb"]
		b, okB := pictures["postgres"]
		if !okA || !okB {
			continue
		}
		sql := dashboardPanelSQL(run, index)
		slack, fromNow := columnSlack(sql, drift), nowColumns(sql)
		like := grafana.Likeness{Slack: func(name string) float64 {
			if fromNow[name] {
				return max(slack[name], apart)
			}
			return slack[name]
		}}
		if diff := grafana.Order(&a, &b, like); diff != "" {
			t.Errorf("panel %d %q draws the same rows in another order in InfluxDB and PostgreSQL, "+
				"so its ORDER BY leaves a tie to each store's sort or a text to PostgreSQL's collation: %s",
				index, dashboardPanelTitle(run, index), diff)
		}
		if len(a.Columns) > 0 && len(a.Columns[0].Values) > 1 {
			held++
		}
	}
	t.Logf("%d tables and bar charts of more than one row were drawn by both SQL stores", held)
	// A floor rather than a count, so a change that stops drawing tables in
	// both stores fails instead of passing with nothing held. Measured at 49
	// on the 2.6.2 branch.
	if held < 30 {
		t.Errorf("only %d tables and bar charts of more than one row were drawn by both SQL stores, "+
			"which is too few for this to have held anything", held)
	}
}

// TestTheStoresHeadEveryTableInOneOrder holds every table to one order of the
// columns it draws in every store, over the columns each pair of stores both
// draws. The comparison above matches rows over the shared columns wherever
// they stand, which is right for the values and blind to the heading: on the
// 2.6.2 branch the Prometheus "Every bucket" read Bucket, Limit, Lowest
// remaining, Most used where the SQL stores read Bucket, Most used, Limit,
// Lowest remaining, and "Artifact storage counted" put its counts in another
// order. A reader moving between two dashboards reads a table by where its
// columns stand.
//
// A Prometheus panel that needs history is asked here too, unlike in the
// comparison of values: a count of zero still heads its columns.
func TestTheStoresHeadEveryTableInOneOrder(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	var report strings.Builder
	held := 0
	indexes := map[int]bool{}
	for _, store := range dashboardStores {
		for i := range run.outcomes[store.name] {
			indexes[i] = true
		}
	}
	for _, index := range slices.Sorted(maps.Keys(indexes)) {
		tables := dashboardTables(t, run, index)
		if len(tables) == 0 {
			continue
		}
		title := dashboardPanelTitle(run, index)
		fmt.Fprintf(&report, "%3d %s\n", index, title)
		for _, store := range dashboardStores {
			if pic, ok := tables[store.name]; ok {
				fmt.Fprintf(&report, "    %-14s %s\n", store.name, strings.Join(pic.Names(), " | "))
			}
		}
		pairs, diffs := headingOrders(tables)
		held += pairs
		for _, diff := range diffs {
			t.Errorf("panel %d %q heads its columns in another order in %s", index, title, diff)
		}
	}
	if err := os.WriteFile(filepath.Join("out", "dashboards", "columns.txt"), []byte(report.String()), 0o600); err != nil {
		t.Errorf("keeping the headings: %v", err)
	}
	t.Logf("%d pairs of stores drew the same table with rows", held)
	// A floor rather than a count, so a change that stops the tables drawing
	// rows fails instead of passing with nothing held. Measured at 645 on the
	// 2.6.2 branch.
	if held < 300 {
		t.Errorf("only %d pairs of stores drew the same table with rows, which is too few for this "+
			"to have held anything", held)
	}
}

// headingOrders holds every pair of stores that drew one table to one order
// of the columns both draw, and answers how many pairs it held and a line
// per pair that heads them in another order.
func headingOrders(tables map[string]grafana.Picture) (pairs int, diffs []string) {
	for x, a := range dashboardStores {
		for _, b := range dashboardStores[x+1:] {
			pa, okA := tables[a.name]
			pb, okB := tables[b.name]
			if !okA || !okB {
				continue
			}
			pairs++
			if diff := grafana.ColumnOrder(&pa, &pb); diff != "" {
				diffs = append(diffs, a.name+" and "+b.name+": "+diff)
			}
		}
	}
	return pairs, diffs
}

// dashboardTables is every store's drawing of one panel when it is a table
// that draws a row, which is when a reader sees its heading.
func dashboardTables(t *testing.T, run *dashboardRun, index int) map[string]grafana.Picture {
	t.Helper()
	out := map[string]grafana.Picture{}
	for _, store := range dashboardStores {
		o, ok := run.outcomes[store.name][index]
		if !ok || o.err != "" || o.panel.Type != "table" || (store.name == "prometheus" && run.promSkip != "") {
			continue
		}
		pic, drawn, err := grafana.Drawing(o.panel.Source, o.answer)
		if err != nil {
			t.Errorf("panel %d %q of %s cannot be replayed, so its heading is not held: %v",
				index, o.panel.Title, store.name, err)
			continue
		}
		if drawn && !pic.Empty() {
			out[store.name] = pic
		}
	}
	return out
}

// TestAColumnAStoreLacksIsSaidInItsDescription holds every column the SQL
// stores draw in a table to being drawn by each other store that draws the
// table, or named in that store's own words about the panel. The comparison
// of values is over the columns two stores share and says nothing of one a
// store lacks, which is how Elasticsearch's "Open the longest" went without
// Title, Author, Labels and Fork through the first comparison and was found
// by a reader: what a store can hold at all is its description's business,
// and this holds the description to it. Measured on the 2.6.2 branch before
// the descriptions said them, 33 tables of Graphite, Elasticsearch and
// Prometheus lacked a column their own words did not name.
//
// It holds what is drawn, so a table the fixture leaves empty in both SQL
// stores is not asked here: TestEveryColumnAStoreLacksIsNamedInItsDescription
// in internal/dashboards holds every table of the specification to the same
// rule, grafana.DrawsColumn and grafana.NamesColumn, whatever the fixture
// fills. A Graphite table says what its reduction drops by naming it, since
// the one sentence every such table carried, "keeps the column it is sorted
// by and drops the others", was false on eleven of them.
func TestAColumnAStoreLacksIsSaidInItsDescription(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	held := 0
	indexes := map[int]bool{}
	for _, store := range []string{"influxdb", "postgres"} {
		for i := range run.outcomes[store] {
			indexes[i] = true
		}
	}
	for _, index := range slices.Sorted(maps.Keys(indexes)) {
		tables := dashboardTables(t, run, index)
		sqlColumns := sqlColumnsOf(tables)
		if len(sqlColumns) == 0 {
			continue
		}
		for _, store := range []string{"graphite", "elasticsearch", "prometheus"} {
			pic, ok := tables[store]
			if !ok {
				continue
			}
			held++
			o := run.outcomes[store][index]
			own := grafana.OwnWords(o.panel.Description, dashboardPanelDescription(run, index))
			if unsaid := columnsUnsaid(store, &o, &pic, sqlColumns, own); len(unsaid) > 0 {
				t.Errorf("panel %d %q in %s draws no %s, which the SQL stores draw, and its description "+
					"does not say so: draw the column or name it where the description says what %s "+
					"cannot hold. What it adds to the shared text: %q",
					index, dashboardPanelTitle(run, index), store, strings.Join(unsaid, ", "), store, own)
			}
		}
	}
	t.Logf("%d tables of Graphite, Elasticsearch and Prometheus were held to the SQL stores' columns", held)
	// A floor rather than a count, so a change that stops the tables drawing
	// rows fails instead of passing with nothing held. Measured at 204 on the
	// 2.6.2 branch.
	if held < 100 {
		t.Errorf("only %d tables were held to the SQL stores' columns, which is too few for this "+
			"to have held anything", held)
	}
}

// sqlColumnsOf is every column InfluxDB or PostgreSQL draws in a table, in
// the order they head it.
func sqlColumnsOf(tables map[string]grafana.Picture) []string {
	var out []string
	for _, store := range []string{"influxdb", "postgres"} {
		pic, ok := tables[store]
		if !ok {
			continue
		}
		for _, name := range pic.Names() {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// columnsUnsaid is every column of the SQL stores one store's table does not
// draw and its own words about the panel do not name.
func columnsUnsaid(store string, o *dashboardOutcome, pic *grafana.Picture, sqlColumns []string, own string) []string {
	drawn := map[string]bool{}
	for _, name := range pic.Names() {
		drawn[name] = true
	}
	if store == "prometheus" && promNeedsHistory(&o.panel) {
		// A column of a range function over the exporter's half minute can be
		// no series at all, which is no column: the one the panel names is
		// what a Prometheus with history draws.
		for _, name := range panelNamesGiven(o.panel.Source) {
			drawn[name] = true
		}
	}
	var out []string
	for _, column := range sqlColumns {
		if !grafana.DrawsColumn(drawn, sqlColumns, column, store == "graphite") && !grafana.NamesColumn(own, column) {
			out = append(out, column)
		}
	}
	return out
}

// panelNamesGiven is every column name a panel's transformations give, what
// an organize renames a column to and what a calculation names its result.
func panelNamesGiven(panel map[string]any) []string {
	var out []string
	tfs, _ := panel["transformations"].([]any)
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		options, _ := tf["options"].(map[string]any)
		rename, _ := options["renameByName"].(map[string]any)
		for _, to := range rename {
			if name, ok := to.(string); ok {
				out = append(out, name)
			}
		}
		if alias, ok := options["alias"].(string); ok && tf["id"] == "calculateField" {
			out = append(out, alias)
		}
	}
	return out
}

// dashboardPanelDescription is the panel's description in the SQL stores,
// which is the text every store shares.
func dashboardPanelDescription(run *dashboardRun, index int) string {
	for _, store := range []string{"influxdb", "postgres"} {
		if o, ok := run.outcomes[store][index]; ok {
			return o.panel.Description
		}
	}
	return ""
}

// rawFieldName is a name a datasource gives a field that the panel is meant
// to give another: an Elasticsearch bucket's field, a metric as the response
// parser names it, a Prometheus value column, the metric name label. The
// 2.6.1 review found "p50.0 seconds_to_merge" in two legends and a
// full_name.keyword column leading a table.
var rawFieldName = regexp.MustCompile(`\.keyword$|^Top Metrics\b|^p[0-9]+\.[0-9] |^Value #[A-Z]+$|^__name__$`)

// TestNoPanelDrawsAFieldUnderTheNameItsDatasourceGave holds every name a
// reader sees, in every panel of every store, legends included, to being one
// a panel chose. It is not a comparison: a raw name is wrong in one store
// whatever the others draw, and the comparison above leaves out the time
// series, where two of them were.
func TestNoPanelDrawsAFieldUnderTheNameItsDatasourceGave(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	checked := 0
	for _, store := range dashboardStores {
		for _, index := range slices.Sorted(maps.Keys(run.outcomes[store.name])) {
			o := run.outcomes[store.name][index]
			if o.err != "" || o.answer == nil {
				continue
			}
			frames, err := grafana.Draw(o.panel.Source, o.answer)
			if err != nil {
				t.Errorf("panel %d %q of %s cannot be replayed: %v", index, o.panel.Title, store.name, err)
				continue
			}
			names, err := grafana.DrawnNames(o.panel.Source, frames)
			if err != nil {
				t.Errorf("panel %d %q of %s cannot be replayed: %v", index, o.panel.Title, store.name, err)
				continue
			}
			for _, name := range names {
				checked++
				if rawFieldName.MatchString(name) {
					t.Errorf("panel %d %q of %s draws %q, the name the datasource gave the field "+
						"rather than one the panel gave it", index, o.panel.Title, store.name, name)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no panel drew a name, so this checked nothing")
	}
}

// dashboardComparison is one pass of the comparison over every panel, and
// what it found.
type dashboardComparison struct {
	run *dashboardRun
	// entries is what excuses a difference in this pass.
	entries []dashboardDiffer
	drift   map[string]float64
	used    map[int]bool
	// compared is, per panel title, the stores whose picture of it was
	// compared with another store's. An entry none of whose stores is here
	// was never asked, which is not the same as no longer being needed.
	compared map[string]map[string]bool
	// alike is, per panel, every pair of stores compared on it and whether
	// the two drew it alike, which is what says whether an entry names a
	// store it need not.
	alike         map[dashboardPanelKey]map[[2]string]bool
	pairs, agreed int
	report        strings.Builder
}

// dashboardPanelKey is a panel as an entry names it: its title, and its type
// for the titles two panels share.
type dashboardPanelKey struct{ title, kind string }

// panel compares every pair of stores on one panel and answers the
// differences no entry excuses, one line per pair.
func (c *dashboardComparison) panel(t *testing.T, index int) []string {
	t.Helper()
	return c.compare(index, dashboardPictures(t, c.run, index))
}

// compare is panel over pictures drawn elsewhere, from another question put
// to the same panels.
func (c *dashboardComparison) compare(index int, pictures map[string]grafana.Picture) []string {
	title := dashboardPanelTitle(c.run, index)
	sql := dashboardPanelSQL(c.run, index)
	slack, fromNow := columnSlack(sql, c.drift), nowColumns(sql)
	var failures []string
	for x, a := range dashboardStores {
		for _, b := range dashboardStores[x+1:] {
			pa, okA := pictures[a.name]
			pb, okB := pictures[b.name]
			if !okA || !okB {
				continue
			}
			c.pairs++
			if c.compared[title] == nil {
				c.compared[title] = map[string]bool{}
			}
			c.compared[title][a.name], c.compared[title][b.name] = true, true
			apart := askedApart(c.run, a.name, b.name)
			like := grafana.Likeness{
				Equal: dashboardSameName(c.run, index, a.name, b.name),
				Slack: func(name string) float64 {
					if fromNow[name] {
						return max(slack[name], apart)
					}
					return slack[name]
				},
			}
			diffs := grafana.Compare(&pa, &pb, like)
			c.remember(dashboardPanelKey{title, pa.Kind}, a.name, b.name, len(diffs) == 0)
			if len(diffs) == 0 {
				c.agreed++
				continue
			}
			pair := a.name + " against " + b.name
			verdict := "DIFFER "
			if dashboardExcused(c.entries, title, a.name, b.name, &pa, &pb, like, c.used) {
				verdict = "EXCUSED"
			} else {
				failures = append(failures, pair+": "+strings.Join(diffs, "; "))
			}
			fmt.Fprintf(&c.report, "%s %3d %s, %s: %s\n", verdict, index, title, pair, strings.Join(diffs, "; "))
		}
	}
	return failures
}

// remember keeps whether two stores drew a panel alike: alike only while they
// have drawn it alike every time they were asked, since a comparison that puts
// two questions to the same panels holds an entry to what either of them
// drew.
func (c *dashboardComparison) remember(key dashboardPanelKey, a, b string, same bool) {
	if c.alike == nil {
		c.alike = map[dashboardPanelKey]map[[2]string]bool{}
	}
	if c.alike[key] == nil {
		c.alike[key] = map[[2]string]bool{}
	}
	pair := [2]string{a, b}
	if before, asked := c.alike[key][pair]; asked {
		same = same && before
	}
	c.alike[key][pair] = same
}

// dashboardPictures is what each store draws for one panel, for the stores
// that drew it without an error and whose answer is not the harness's.
func dashboardPictures(t *testing.T, run *dashboardRun, index int) map[string]grafana.Picture {
	t.Helper()
	out := map[string]grafana.Picture{}
	for _, store := range dashboardStores {
		o, ok := run.outcomes[store.name][index]
		if !ok || o.err != "" {
			continue // a query that failed is the first question's business
		}
		if store.name == "prometheus" && (run.promSkip != "" || promNeedsHistory(&o.panel)) {
			continue
		}
		pic, drawn, err := grafana.Drawing(o.panel.Source, o.answer)
		if err != nil {
			t.Errorf("panel %d %q of %s cannot be replayed, so it is not compared: %v",
				index, o.panel.Title, store.name, err)
			continue
		}
		if drawn {
			out[store.name] = pic
		}
	}
	return out
}

func dashboardPanelTitle(run *dashboardRun, index int) string {
	for _, store := range dashboardStores {
		if o, ok := run.outcomes[store.name][index]; ok {
			return o.panel.Title
		}
	}
	return ""
}

// dashboardPanelSQL is the panel's statement in the dialect the others are
// translated from, or PostgreSQL's where InfluxDB has none.
func dashboardPanelSQL(run *dashboardRun, index int) string {
	for _, store := range []string{"influxdb", "postgres"} {
		o, ok := run.outcomes[store][index]
		if !ok {
			continue
		}
		var sql strings.Builder
		for _, target := range o.panel.Targets {
			if s, isSQL := target["rawSql"].(string); isSQL {
				sql.WriteString(s)
				sql.WriteByte('\n')
			}
		}
		if sql.Len() > 0 {
			return sql.String()
		}
	}
	return ""
}

// graphiteNamesRows is how a Graphite description says a row is named by
// several path nodes: "Graphite names each row job and repository from the
// path".
const graphiteNamesRows = "Graphite names each row"

// dashboardSameName is the text equivalence between two stores on one panel:
// none unless one of them is Graphite.
func dashboardSameName(run *dashboardRun, index int, a, b string) func(x, y string) bool {
	if a != "graphite" && b != "graphite" {
		return nil
	}
	o := run.outcomes["graphite"][index]
	nodes := strings.Contains(o.panel.Description, graphiteNamesRows)
	return func(x, y string) bool {
		return graphiteSameName(x, y, nodes) || graphiteSameName(y, x, nodes)
	}
}

// graphiteSameName reports whether a name Graphite drew is the other store's:
// the node the sink makes of it, or, when the panel names its rows from the
// path, one of the nodes the row's name is made of. The sink turns a dot into
// an underscore, so a dot in a Graphite name is always a joint between nodes.
func graphiteSameName(graphite, other string, nodes bool) bool {
	node := graphiteNodeOf(other)
	if graphite == node {
		return true
	}
	return nodes && slices.Contains(strings.Split(graphite, "."), node)
}

// dashboardExcused reports whether the entries naming either store excuse
// every difference between the two on the panel, and marks the ones that did.
// An entry without `only` excuses the pair; entries that name tiles or columns
// excuse it when the pair draws the rest alike.
func dashboardExcused(differ []dashboardDiffer, title, a, b string, pa, pb *grafana.Picture,
	like grafana.Likeness, used map[int]bool,
) bool {
	var entries []int
	var only []string
	for i, e := range differ {
		if e.title != title || (!slices.Contains(e.stores, a) && !slices.Contains(e.stores, b)) ||
			(e.kind != "" && e.kind != pa.Kind) {
			continue
		}
		if len(e.only) == 0 {
			used[i] = true
			return true
		}
		entries = append(entries, i)
		only = append(only, e.only...)
	}
	if len(entries) == 0 {
		return false
	}
	restA, restB := pa.Without(only), pb.Without(only)
	if len(grafana.Compare(&restA, &restB, like)) > 0 {
		return false
	}
	for _, i := range entries {
		used[i] = true
	}
	return true
}

// dashboardDifferProblems holds every entry to the panel it names: the panel
// exists, each store it names says the reason in its own description of the
// panel, and the difference it excuses is still there.
//
// The last is known only of an entry one of whose stores was compared on the
// panel. Where the exporter could not be loaded, a firewalled docker bridge
// or a scrape that never came, every Prometheus picture is left out, the
// entries that name Prometheus alone excuse nothing, and they are still
// needed: calling them stale told the reader to delete four valid entries on
// exactly the machines where the other two questions skip Prometheus. Those
// are returned as unasked, to be logged rather than failed.
func dashboardDifferProblems(entries []dashboardDiffer, run *dashboardRun, used map[int]bool,
	compared map[string]map[string]bool,
) (problems, unasked []string) {
	for i, e := range entries {
		found := false
		for _, store := range e.stores {
			for _, o := range run.outcomes[store] {
				if o.panel.Title != e.title || (e.kind != "" && o.panel.Type != e.kind) {
					continue
				}
				found = true
				if !strings.Contains(o.panel.Description, e.reason) {
					problems = append(problems, fmt.Sprintf("%q is excused in %s as %q, and that is not "+
						"what %s's description of the panel says: %q",
						e.title, store, e.reason, store, o.panel.Description))
				}
			}
		}
		asked := slices.ContainsFunc(e.stores, func(store string) bool { return compared[e.title][store] })
		switch {
		case !found:
			problems = append(problems, fmt.Sprintf("%q is excused in %v, and no such panel is drawn by them",
				e.title, e.stores))
		case used[i]:
		case asked:
			problems = append(problems, fmt.Sprintf("%q is excused in %v as %q, and the stores now draw "+
				"it alike: take the entry out", e.title, e.stores, e.reason))
		default:
			unasked = append(unasked, fmt.Sprintf("%q is excused in %v, and none of them was compared on "+
				"it in this run, so whether it is still needed was not asked", e.title, e.stores))
		}
	}
	return problems, unasked
}

// dashboardDifferTooWide holds every entry to the stores it names: each of
// them has to draw the panel differently from a store the entry does not
// name, since one that draws it as those do is excused of nothing, and an
// entry naming it would excuse whatever it drew wrong next. A store compared
// only with stores the entry names too is not asked, as a pair of them cannot
// say which of the two needed the entry.
func dashboardDifferTooWide(entries []dashboardDiffer, alike map[dashboardPanelKey]map[[2]string]bool) []string {
	var out []string
	for _, e := range entries {
		for _, store := range e.stores {
			if asked, differs := differsFromUnnamed(&e, store, alike); asked && !differs {
				out = append(out, fmt.Sprintf("%q is excused in %v as %q, and %s draws it as every "+
					"store the entry does not name does: take %s out of the entry", e.title, e.stores,
					e.reason, store, store))
			}
		}
	}
	return out
}

// differsFromUnnamed reports whether one store an entry names was compared
// with a store the entry does not name, and whether it drew the panel
// differently from any of them.
func differsFromUnnamed(e *dashboardDiffer, store string, alike map[dashboardPanelKey]map[[2]string]bool) (asked, differs bool) {
	for key, pairs := range alike {
		if key.title != e.title || (e.kind != "" && key.kind != e.kind) {
			continue
		}
		for pair, same := range pairs {
			other := pair[0]
			if other == store {
				other = pair[1]
			} else if pair[1] != store {
				continue
			}
			if !slices.Contains(e.stores, other) {
				asked, differs = true, differs || !same
			}
		}
	}
	return asked, differs
}

// ── What moved between the sweeps ───────────────────────────────────────────

// sweepDrift is how far each numeric field moved between any two of the
// sweeps, for the same point in both: the same measurement, tags and time, or
// the one point a series has in each when it is stamped when the sweep
// looked. The fixtures are the same, so a field that moved is one the
// collector computes from something that is not, which is its own clock.
func sweepDrift(sweeps [][]sqlStoresPoint) map[string]float64 {
	drift := map[string]float64{}
	for i := range sweeps {
		for j := i + 1; j < len(sweeps); j++ {
			for _, pair := range samePoints(sweeps[i], sweeps[j]) {
				for field, v := range pair[0].Fields {
					a, aNumber := v.(float64)
					b, bNumber := pair[1].Fields[field].(float64)
					if aNumber && bNumber && math.Abs(a-b) > drift[field] {
						drift[field] = math.Abs(a - b)
					}
				}
			}
		}
	}
	return drift
}

// samePoints pairs the points two sweeps wrote for the same thing.
func samePoints(a, b []sqlStoresPoint) [][2]sqlStoresPoint {
	series := func(p sqlStoresPoint) string {
		keys := slices.Sorted(maps.Keys(p.Tags))
		parts := []string{p.Measurement}
		for _, k := range keys {
			parts = append(parts, k+"="+p.Tags[k])
		}
		return strings.Join(parts, ",")
	}
	byPoint, bySeries := map[string]sqlStoresPoint{}, map[string][]sqlStoresPoint{}
	for _, p := range b {
		byPoint[series(p)+"@"+p.Time] = p
		bySeries[series(p)] = append(bySeries[series(p)], p)
	}
	countA := map[string]int{}
	for _, p := range a {
		countA[series(p)]++
	}
	var out [][2]sqlStoresPoint
	for _, p := range a {
		if q, ok := byPoint[series(p)+"@"+p.Time]; ok {
			out = append(out, [2]sqlStoresPoint{p, q})
			continue
		}
		if others := bySeries[series(p)]; countA[series(p)] == 1 && len(others) == 1 {
			out = append(out, [2]sqlStoresPoint{p, others[0]})
		}
	}
	return out
}

// sqlAlias is a column a statement names.
var (
	sqlAlias      = regexp.MustCompile(`\s+AS\s+"([^"]+)"`)
	sqlIdentifier = regexp.MustCompile(`[a-z_][a-z0-9_]*`)
)

// columnExpressions is what each column of a statement is made of: the text
// before its alias, back to the comma or the parenthesis that encloses it,
// with a column named in two halves of a UNION given both.
func columnExpressions(sql string) map[string]string {
	out := map[string]string{}
	for _, m := range sqlAlias.FindAllStringSubmatchIndex(sql, -1) {
		depth, start := 0, 0
	scan:
		for i := m[0] - 1; i >= 0; i-- {
			switch sql[i] {
			case ')':
				depth++
			case '(':
				if depth == 0 {
					start = i + 1
					break scan
				}
				depth--
			case ',':
				if depth == 0 {
					start = i + 1
					break scan
				}
			}
		}
		expr := sql[start:m[0]]
		if k := strings.LastIndex(expr, "SELECT "); k >= 0 {
			expr = expr[k+len("SELECT "):]
		}
		name := sql[m[2]:m[3]]
		out[name] += " " + expr
	}
	return out
}

// columnSlack is how far apart each column of a panel may be between stores
// loaded by different sweeps: the most any field its expression reads moved
// between them. Every store draws a column under the name the SQL gives it,
// which is what lets the SQL name the field.
func columnSlack(sql string, drift map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for name, expr := range columnExpressions(sql) {
		for _, id := range sqlIdentifier.FindAllString(expr, -1) {
			out[name] = max(out[name], drift[id])
		}
	}
	return out
}

// nowColumns is every column a statement computes from now(): how long an
// alert has been open, how long a fork has been idle. Both are in seconds.
func nowColumns(sql string) map[string]bool {
	out := map[string]bool{}
	for name, expr := range columnExpressions(sql) {
		if strings.Contains(expr, "now()") {
			out[name] = true
		}
	}
	return out
}

// askedApart is the most seconds that passed between the answers of two
// stores, which is how far a value computed from now() moves between them:
// InfluxDB and PostgreSQL, asked a second apart, drew an alert open for
// 1240837 and 1240838.3 seconds.
func askedApart(run *dashboardRun, a, b string) float64 {
	wa, wb := run.asked[a], run.asked[b]
	first, last := wa[0], wa[1]
	if wb[0].Before(first) {
		first = wb[0]
	}
	if wb[1].After(last) {
		last = wb[1]
	}
	return last.Sub(first).Seconds()
}
