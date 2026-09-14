//go:build dockere2e

package docker

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// The Link column, against the stores rather than against the file.
//
// Two dozen tables select the row's url and render it as one short word, Open,
// through a regex value mapping, with a data link on the raw value, so that a
// row whose item has no page of its own stays empty. Nothing checked that until
// this file. The specification's own test compares JSON with JSON, and
// /api/ds/query answers with values alone: a value mapping and a data link are
// the browser's work, done over the frame the datasource returned. Which is the
// point of asking here. The mapping is a regex over what the store hands back,
// and that is a different string in five stores, or no string at all.
//
// So this asks the questions a cell is drawn by, in order:
//
//  1. is there a column called Link at all? The name is made by the panel's
//     own organize transformation, which is also the browser's work, so the
//     answer is the frame's field names put through that transformation.
//  2. is it a string? A regex mapping is tried against strings and skipped for
//     everything else, so a column that arrives as a number is drawn as the
//     number and the override is decoration.
//  3. does every value match the mapping, so that the cell reads Open rather
//     than a raw url?
//  4. is the value a url the collector really wrote for what this panel reads?
//     The data link is ${__value.raw}, so the href is the cell's own value and
//     nothing else: a wrong value is a link to the wrong page and no error
//     anywhere.
//
// And the same questions from the other end, which need no store at all: a
// panel that selects a column called Link and carries no override draws the raw
// url in the table, which is the thing the override exists to prevent, and an
// override that no store's query has a column for draws nothing at all
// anywhere. Why the second is a whole test rather than a count is under
// TestEveryLinkOverrideIsLiveInSomeStore.
//
// A table has two ways of drawing the column. The first is the column itself,
// the word Open through the mapping, with the data link on the cell's own
// value. The second, which every table now takes, hides the Link column and
// hangs the link on the table's first column, reading the row's url through
// ${__data.fields.Link}: on a phone the Link column at the far right was
// reached in two tables of twenty-eight, and the first column is always on
// the screen. That form carries no mapping on purpose, because Grafana hands
// ${__data.fields.X} the cell's display text, which a mapping would have
// turned into the word. The four questions are the same for both: the column
// still has to be in the frame, a string, every value a url the collector
// wrote, and the data link opened in a new tab.
//
// What this cannot do is prove that Grafana draws the word, since nothing
// served by /api/ds/query knows about mappings and the suite has no browser.
// That was settled once by hand against this stack, in Grafana 13.2.1: the
// InfluxDB dashboard's Pinned items renders its cell as
// `<a href="https://github.com/octocat/hello-world" target="_blank"
// title="Open on GitHub">Open</a>`, and Sponsorships, whose second row has no
// url, renders that row's cell as nothing at all. The hidden form was settled
// the same way: the Path cell of Top paths renders as
// `<a href="https://github.com/jmrplens/gitlab-mcp-server" target="_blank">`
// with the Link column absent from the grid. What is left to regress is the
// values and the column, and that is what this file holds.

// What the specification writes on every Link column. All four are asserted
// rather than read out of the file, because each is load-bearing: the pattern
// decides whether the cell reads Open, the href decides where it goes, and a
// link that is not opened in a new tab loses the dashboard.
const (
	dashboardLinkName    = "Link"
	dashboardLinkPattern = "^https?://.+"
	dashboardLinkText    = "Open"
	dashboardLinkHref    = "${__value.raw}"
	// dashboardLinkRowHref is the data link of the hidden form: another
	// column of the same row reading the url out of the Link column.
	dashboardLinkRowHref = "${__data.fields." + dashboardLinkName + "}"
)

// dashboardLinkRendering is Grafana's own rule for a regex value mapping, and
// it is stricter than the pattern reads.
//
// getValueMappingResult tries a RegexToText mapping against strings alone, and
// compiles the pattern with stringToJsRegex, which anchors a bare pattern at
// both ends before matching: `^` + pattern + `$`. The pattern here already
// opens with a `^` of its own, which JavaScript would accept twice and Go reads
// as a dangling anchor, so the leading one is taken off rather than doubled;
// the language is the same either way. The end anchor is the half that does
// work: a value carrying anything after the url, a newline included, matches
// nothing and is drawn as itself.
var dashboardLinkRendering = regexp.MustCompile(
	"^" + strings.TrimPrefix(dashboardLinkPattern, "^") + "$",
)

// dashboardLinkAliased matches the column a SQL statement selects as Link.
var dashboardLinkAliased = regexp.MustCompile(`AS\s+"` + dashboardLinkName + `"`)

// dashboardLinkSource names the field a SQL statement selects as Link, bare
// or under an aggregate: `url AS "Link"`, `MAX(referrer_url) AS "Link"`.
var dashboardLinkSource = regexp.MustCompile(`([a-z_]+)\)?\s+AS\s+"` + dashboardLinkName + `"`)

// dashboardLinkColumn is what one panel's Link column turned out to be in one
// store, after the panel's own transformations have been applied to the frames
// the datasource returned.
type dashboardLinkColumn struct {
	// from is the field the transformation renamed, empty when no field became
	// the Link column.
	from string
	// kind is the frame's type for that field, which decides whether a value
	// mapping is tried at all.
	kind string
	// values is every cell, in row order, a cell with no value as the empty
	// string.
	values []string
	// columns is every column the panel ends with, so a panel that gets no Link
	// column can say what it did get. A panel answered with nothing at all has
	// none of them, which is a different finding from a rename that no longer
	// matches and belongs to
	// TestDashboardPanelsAnswerWhenThereIsSomethingToShow.
	columns []string
}

// filled is every cell that will be drawn as something.
func (c *dashboardLinkColumn) filled() []string {
	out := make([]string, 0, len(c.values))
	for _, v := range c.values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// TestDashboardLinkColumnsOpenTheItem is the four questions above, per store,
// over the same loaded stack the rest of the dashboard suite asks.
func TestDashboardLinkColumnsOpenTheItem(t *testing.T) {
	s := Start(t)
	run := dashboardsRun(t, s)
	urls := dashboardLinkURLs(t, s)

	var total dashboardLinkTally
	for _, store := range dashboardStores {
		t.Run(store.name, func(t *testing.T) {
			if store.name == "prometheus" && run.promSkip != "" {
				t.Skipf("Prometheus holds nothing on this machine: %s", run.promSkip)
			}
			total.add(dashboardLinkCheckStore(t, s, store, urls))
		})
	}
	// Both of the assertions above are of the form "every value that is there".
	// A sweep whose urls all went missing would satisfy both and prove nothing,
	// so the run has to have drawn some, and the rule about a cell left empty
	// has to have found a panel it applies to.
	if total.drawn == 0 {
		t.Error("not one Link column in any of the five dashboards held a url, so nothing above " +
			"was actually checked: either the sweep stopped writing urls or the column stopped " +
			"reaching the panels")
	}
	if total.complete == 0 {
		t.Error("no panel of any dashboard reads a measurement this sweep wrote a url on every " +
			"point of, so nothing above checked a Link cell for being empty when the row has a page")
	}
}

// TestEveryLinkOverrideIsLiveInSomeStore is the one thing the arrangement above
// cannot excuse, and it needs no store to ask it.
//
// linkColumn() sits in the panel's own Overrides, and panel() hands it to a
// store only when that store's query returns the column it reads (placeLinks
// in cmd/internal/dashboards/spec.go): the two SQL stores select it by name,
// Elasticsearch buckets on url.keyword, and Prometheus and Graphite, which
// keep no url, get a sentence in the description instead. Before that the
// override reached all five dashboards and drew nothing on three of them,
// Grafana matching an override byName against the fields the frame actually
// has, so a field that is not there is not an error but silence.
//
// What the placement cannot tell from a real link is an override that no
// store uses at all. That one is a panel whose Link column was renamed or
// dropped with the override left behind: it renders nothing anywhere, so it
// is invisible in exactly the way the silence above was, and nothing but
// this would ever say so.
func TestEveryLinkOverrideIsLiveInSomeStore(t *testing.T) {
	carried := map[int]string{}
	live := map[int]bool{}
	for _, store := range dashboardStores {
		doc, err := dashboardDocument(store.name)
		if err != nil {
			t.Fatalf("reading the %s dashboard: %v", store.name, err)
		}
		for index, p := range dashboardLinkPanels(doc) {
			if !p.override {
				continue
			}
			carried[index] = p.title
			live[index] = live[index] || p.selects
		}
	}
	if len(carried) == 0 {
		t.Fatal("not one panel of the five dashboards carries a Link override, so either the " +
			"specification stopped writing them or this file stopped finding them")
	}
	for _, index := range slices.Sorted(maps.Keys(carried)) {
		if live[index] {
			continue
		}
		t.Errorf("panel %d %q carries the %s override in all five dashboards and no store's "+
			"query returns a column that becomes one, so it renders nothing anywhere: either "+
			"the queries stopped naming the column or the panel never had one",
			index, carried[index], dashboardLinkName)
	}
	t.Logf("%d panels carry the Link override, each of them drawn by at least one store",
		len(carried))
}

// dashboardLinkTally is what one dashboard, or all five, actually checked.
type dashboardLinkTally struct {
	// drawn is how many cells held a url the collector wrote.
	drawn int
	// complete is how many panels the empty-cell rule could be applied to.
	complete int
}

func (t *dashboardLinkTally) add(other dashboardLinkTally) {
	t.drawn += other.drawn
	t.complete += other.complete
}

// dashboardLinkCheckStore runs one dashboard past the four questions and
// returns what it checked.
func dashboardLinkCheckStore(t *testing.T, s *Stack, store dashboardStore,
	urls map[string]*dashboardLinkFacts,
) dashboardLinkTally {
	t.Helper()
	doc, err := dashboardDocument(store.name)
	if err != nil {
		t.Fatalf("reading the %s dashboard: %v", store.name, err)
	}
	panels := dashboardLinkPanels(doc)
	selecting := dashboardLinkSelecting(panels)
	if len(panels) == 0 {
		// Prometheus and Graphite keep no string beside a number, so the
		// specification hands neither dashboard the override and their
		// tables say so in prose (placeLinks in spec.go). Nothing to check
		// here; TestEveryLinkOverrideIsLiveInSomeStore is what holds every
		// override to being drawn by some store.
		t.Logf("the %s dashboard carries no Link column: its queries return no url", store.name)
		return dashboardLinkTally{}
	}
	columns := dashboardLinkAnswers(t, s, doc, store, selecting)

	var tally dashboardLinkTally
	for _, index := range slices.Sorted(maps.Keys(panels)) {
		p := panels[index]
		if !p.override {
			t.Errorf("panel %d %q of %s selects a column called %s and carries no override for it, "+
				"so the table draws the raw url in it", p.index, p.title, store.name, dashboardLinkName)
			continue
		}
		dashboardLinkCheckOverride(t, store.name, p)
		if !p.selects {
			// The override is on a panel whose query in this store returns no
			// such column, which is the shared Overrides list working as it is
			// meant to: an override matched byName against a field the frame
			// does not have is invisible, and the count is in the log below.
			// TestEveryLinkOverrideIsLiveInSomeStore is where that is held to
			// something, an override no store at all uses being a different
			// thing entirely.
			continue
		}
		column := columns[index]
		if column.from == "" {
			dashboardLinkCheckAbsence(t, store.name, p, &column, urls)
			continue
		}
		tally.add(dashboardLinkCheckValues(t, store.name, p, &column, urls))
	}
	t.Logf("%d panels carry a Link override, %d of them select the column, %d urls drawn, %d "+
		"panels held to having no empty cell", len(panels), len(selecting), tally.drawn, tally.complete)
	return tally
}

// dashboardLinkCheckOverride is the half of question 2 that needs no store: the
// override has to be the one the rendering above assumes, on every panel that
// carries it.
func dashboardLinkCheckOverride(t *testing.T, store string, p *dashboardLinkPanel) {
	t.Helper()
	if p.hidden {
		dashboardLinkCheckHidden(t, store, p)
		return
	}
	if p.pattern != dashboardLinkPattern || p.text != dashboardLinkText {
		t.Errorf("panel %d %q of %s maps %q to %q, and this file checks the rendering of %q to %q",
			p.index, p.title, store, p.pattern, p.text, dashboardLinkPattern, dashboardLinkText)
	}
	if p.href != dashboardLinkHref {
		t.Errorf("panel %d %q of %s links to %q rather than to the row's own url %q, so the cell "+
			"opens something other than the item it names",
			p.index, p.title, store, p.href, dashboardLinkHref)
	}
	if !p.newTab {
		t.Errorf("panel %d %q of %s opens its link in the same tab, which replaces the dashboard",
			p.index, p.title, store)
	}
}

// dashboardLinkCheckHidden is the same for the hidden form: no mapping, since
// the link would open the word rather than the url, another column reading
// the row's url, and that one opened in a new tab.
func dashboardLinkCheckHidden(t *testing.T, store string, p *dashboardLinkPanel) {
	t.Helper()
	if p.pattern != "" {
		t.Errorf("panel %d %q of %s hides its Link column and still maps %q, so the column another "+
			"cell reads through ${__data.fields.%s} is the mapped word, not the url",
			p.index, p.title, store, p.pattern, dashboardLinkName)
	}
	if p.from == "" {
		t.Errorf("panel %d %q of %s hides its Link column and no column of the table reads it "+
			"through %q, so the row links nowhere", p.index, p.title, store, dashboardLinkRowHref)
		return
	}
	if !p.newTab {
		t.Errorf("panel %d %q of %s opens the link on its %s column in the same tab, which "+
			"replaces the dashboard", p.index, p.title, store, p.from)
	}
}

// dashboardLinkCheckAbsence is question 1 asked backwards: the panel selects
// the column and the store answered without it.
func dashboardLinkCheckAbsence(t *testing.T, store string, p *dashboardLinkPanel,
	c *dashboardLinkColumn, urls map[string]*dashboardLinkFacts,
) {
	t.Helper()
	if len(c.columns) == 0 {
		// Answered with no columns at all: either the store refused the query
		// or the sweep wrote nothing this panel reads. Both are reported, with
		// their reasons, by the two tests above, and neither is anything this
		// file can say about the Link column.
		return
	}
	if !dashboardLinkAnyURL(p, urls) {
		// Elasticsearch keeps the documents, and a document the collector wrote
		// no url on has no such key, so the field is missing from the frame
		// rather than present and empty and the whole column disappears. The
		// rendering is the SQL stores' empty column, and the sweep is what says
		// which of the two this is.
		t.Logf("panel %d %q of %s selects a Link column and gets none: the sweep wrote no url on "+
			"any %s, so no document carries the field",
			p.index, p.title, store, strings.Join(p.subject, " or "))
		return
	}
	t.Errorf("panel %d %q of %s answered with %s, none of them the Link column it selects, and the "+
		"sweep did write urls for %s: the column the query names is not the one the datasource "+
		"returns", p.index, p.title, store, strings.Join(c.columns, ", "),
		strings.Join(p.subject, " or "))
}

// dashboardLinkCheckValues is questions 2, 3 and 4 over one column, and returns
// what it checked.
func dashboardLinkCheckValues(t *testing.T, store string, p *dashboardLinkPanel,
	c *dashboardLinkColumn, urls map[string]*dashboardLinkFacts,
) dashboardLinkTally {
	t.Helper()
	var tally dashboardLinkTally
	if c.kind != "string" {
		t.Errorf("panel %d %q of %s gets its Link column from %q as a %s, and a regex value "+
			"mapping is tried against strings alone, so the cell is drawn as the raw value",
			p.index, p.title, store, c.from, c.kind)
		return tally
	}
	if len(p.subject) == 0 && len(c.filled()) > 0 {
		t.Errorf("panel %d %q of %s draws urls and the InfluxDB dashboard names no measurement for "+
			"it, so nothing here can check which item they belong to", p.index, p.title, store)
		return tally
	}
	for _, v := range c.filled() {
		if dashboardLinkCheckValue(t, store, p, v, urls) {
			tally.drawn++
		}
	}
	if dashboardLinkCheckEmpty(t, store, p, c, urls) {
		tally.complete++
	}
	return tally
}

// dashboardLinkCheckEmpty is the question the other way round: a cell left
// empty for a row that does have a page.
//
// Which rows those are is not a thing this file can know in general, since a
// panel joins, filters and limits before anything is drawn. What it can know is
// the case where the answer cannot be anything else: every point of every
// measurement the panel reads carried a url, so whatever survived the query is
// a row of an item that has one, and an empty cell in it is a url the query
// dropped on the way.
func dashboardLinkCheckEmpty(t *testing.T, store string, p *dashboardLinkPanel,
	c *dashboardLinkColumn, urls map[string]*dashboardLinkFacts,
) bool {
	t.Helper()
	if !dashboardLinkAlwaysURL(p, urls) {
		return false
	}
	empty := len(c.values) - len(c.filled())
	if empty == 0 {
		return true
	}
	t.Errorf("panel %d %q of %s draws %d rows and %d of them have an empty Link cell, and the "+
		"sweep wrote a url on every point of %s: every row it can be showing is an item with a "+
		"page of its own", p.index, p.title, store, len(c.values), empty,
		strings.Join(p.subject, " and "))
	return true
}

// dashboardLinkAlwaysURL reports whether every point of everything this panel
// reads carried the url its Link column is selected from. That is the item's
// own `url` for most tables, and a weaker one for a few: Top referrers links
// from `referrer_url`, which the collector writes only when the referrer is a
// host, so a search engine GitHub names without one is drawn with an empty
// cell on purpose and the rule has to be asked about that field, not `url`.
func dashboardLinkAlwaysURL(p *dashboardLinkPanel, urls map[string]*dashboardLinkFacts) bool {
	if len(p.subject) == 0 {
		return false
	}
	for _, m := range p.subject {
		facts := urls[m]
		if facts == nil || facts.points == 0 || facts.with[p.field] != facts.points {
			return false
		}
	}
	return true
}

// dashboardLinkCheckValue is one cell: how it renders, and whether it opens the
// row's own item.
func dashboardLinkCheckValue(t *testing.T, store string, p *dashboardLinkPanel,
	v string, urls map[string]*dashboardLinkFacts,
) bool {
	t.Helper()
	if !dashboardLinkRendering.MatchString(v) {
		t.Errorf("panel %d %q of %s holds %q in its Link column, which is not an absolute url, so "+
			"the link points the browser at it rather than at the item",
			p.index, p.title, store, grafana.Trim(v, 120))
		return false
	}
	if !dashboardLinkWritten(p, v, urls) {
		t.Errorf("panel %d %q of %s draws %q in its Link column, and the sweep wrote no such url "+
			"for %s, so the cell opens a page this row is not about",
			p.index, p.title, store, grafana.Trim(v, 120), strings.Join(p.subject, " or "))
		return false
	}
	return true
}

// dashboardLinkWritten reports whether the collector wrote this url for one of
// the measurements the panel reads.
func dashboardLinkWritten(p *dashboardLinkPanel, value string, urls map[string]*dashboardLinkFacts) bool {
	for _, m := range p.subject {
		if urls[m] != nil && urls[m].values[value] {
			return true
		}
	}
	return false
}

// dashboardLinkAnyURL reports whether the sweep wrote any url at all for what
// this panel reads.
func dashboardLinkAnyURL(p *dashboardLinkPanel, urls map[string]*dashboardLinkFacts) bool {
	for _, m := range p.subject {
		if urls[m] != nil && urls[m].withURL > 0 {
			return true
		}
	}
	return false
}

// dashboardLinkFacts is what the sweep wrote about one measurement's urls: the
// oracle every question but the second is answered out of.
type dashboardLinkFacts struct {
	// values is every url written for it, which is what a cell may hold.
	values map[string]bool
	// points is how many points the measurement got, and withURL how many of
	// them carried a url. Equal, and non-zero, means every item of this
	// measurement has a page, so a row of it drawn with an empty cell has lost
	// something. with is the same count per url field, `url` included, for
	// the panels that link from another one.
	points  int
	withURL int
	with    map[string]int
}

// dashboardLinkURLs is that, per measurement. A point whose url is empty
// contributes no value, which is the rendering a row with no url gets anyway,
// but it does count, because it is what makes the measurement one whose items
// do not all have a page.
func dashboardLinkURLs(t *testing.T, s *Stack) map[string]*dashboardLinkFacts {
	t.Helper()
	out := map[string]*dashboardLinkFacts{}
	for _, p := range sqlStoresPoints(t, sqlStoresRun(t.Context(), t, s)) {
		if out[p.Measurement] == nil {
			out[p.Measurement] = &dashboardLinkFacts{values: map[string]bool{}, with: map[string]int{}}
		}
		facts := out[p.Measurement]
		facts.points++
		if v, ok := p.Fields["url"].(string); ok && v != "" {
			facts.withURL++
			facts.with["url"]++
			facts.values[v] = true
		}
		// The other urls a row carries, the stargazer's profile or a
		// referrer's host, are what a panel links from when it has no page of
		// its own to open; they count as written and never as the url the
		// empty-cell rule is about.
		for name, raw := range p.Fields {
			if v, ok := raw.(string); ok && v != "" && name != "url" && strings.HasSuffix(name, "_url") {
				facts.values[v] = true
				facts.with[name]++
			}
		}
	}
	// Elasticsearch and Graphite hold the push sweep, which ran against a fake
	// GitHub of its own on a port of its own. Every url but one is GitHub's
	// and the same in both; the achievements page is the one the collector
	// builds from the site it reads, so its url names the fake, and the fake
	// it names is the push sweep's. Its urls are written too; the counts stay
	// the SQL sweep's, since both sweeps wrote the same rows.
	for _, p := range pushPoints(t, pushSweepRun(t.Context(), t, s)) {
		facts := out[p.Measurement]
		if facts == nil {
			continue
		}
		for name, raw := range p.Fields {
			if v, ok := raw.(string); ok && v != "" && (name == "url" || strings.HasSuffix(name, "_url")) {
				facts.values[v] = true
			}
		}
	}
	return out
}

// ── reading the dashboards ──────────────────────────────────────────────────

// dashboardLinkPanel is one panel that has anything to do with the column: what
// the override says, whether the query selects it, and what the panel's own
// transformations will do to the field names the datasource returns.
type dashboardLinkPanel struct {
	index int
	title string
	query grafana.PanelQuery
	// subject is the measurements this panel reads, taken from the InfluxDB
	// dashboard because it is the dialect the others are translated from;
	// field is the column that dialect selects as Link, `url` unless the
	// statement names another.
	subject []string
	field   string
	// override is whether the panel carries the Link override; selects is
	// whether its query in this store returns a column that becomes Link.
	override bool
	selects  bool

	pattern string
	text    string
	href    string
	newTab  bool
	// hidden is the second form: the Link column is not drawn, and `from`
	// names the column whose data link reads it.
	hidden bool
	from   string

	// rename, include and exclude are the organize transformation's three maps,
	// which are how a field called `url` becomes a column called Link.
	rename  map[string]string
	include map[string]bool
	exclude map[string]bool
}

// dashboardLinkPanels is every panel of one dashboard that either carries the
// Link override or selects a Link column, keyed by the ordinal grafana.Panels
// gives it, which is the same panel in all five dashboards.
func dashboardLinkPanels(doc map[string]any) map[int]*dashboardLinkPanel {
	subjects, fields := dashboardLinkSubjects()
	out := map[int]*dashboardLinkPanel{}
	for _, q := range grafana.Panels(doc["panels"]) {
		raw := dashboardLinkPanelAt(doc, q.Index)
		p := &dashboardLinkPanel{
			index: q.Index, title: q.Title, query: q, subject: subjects[q.Index], field: fields[q.Index],
		}
		p.rename, p.include, p.exclude = dashboardLinkOrganize(raw)
		p.selects = dashboardLinkSelects(&q, p)
		if override := dashboardLinkOverride(raw); override != nil {
			p.override = true
			dashboardLinkReadOverride(p, override)
			dashboardLinkReadFrom(p, raw)
		}
		if p.override || p.selects {
			out[q.Index] = p
		}
	}
	return out
}

// dashboardLinkSubjects is the measurements every panel reads and the field
// each selects as Link. Both are the InfluxDB dashboard's, whatever store is
// being asked, because the same panel reads the same thing everywhere and
// only one dialect is worth parsing.
func dashboardLinkSubjects() (subjects map[int][]string, fields map[int]string) {
	doc, err := dashboardDocument("influxdb")
	if err != nil {
		return nil, nil
	}
	panels := grafana.Panels(doc["panels"])
	fields = map[int]string{}
	for _, p := range panels {
		fields[p.Index] = "url"
		for _, t := range p.Targets {
			sql, _ := t["rawSql"].(string)
			if m := dashboardLinkSource.FindStringSubmatch(sql); m != nil {
				fields[p.Index] = m[1]
			}
		}
	}
	return dashboardSubjects(panels), fields
}

// dashboardLinkSelecting is the panels whose query really returns the column.
func dashboardLinkSelecting(panels map[int]*dashboardLinkPanel) map[int]*dashboardLinkPanel {
	out := map[int]*dashboardLinkPanel{}
	for index, p := range panels {
		if p.selects {
			out[index] = p
		}
	}
	return out
}

// dashboardLinkSelects reports whether the panel's query returns a column that
// becomes Link: a SQL statement that aliases one, or a transformation that
// renames a field to it. Graphite and Prometheus do neither, because neither
// keeps a string beside a number, and it is the query rather than a list of
// stores that says so here.
func dashboardLinkSelects(q *grafana.PanelQuery, p *dashboardLinkPanel) bool {
	for _, to := range p.rename {
		if to == dashboardLinkName {
			return true
		}
	}
	for _, t := range q.Targets {
		if sql, _ := t["rawSql"].(string); dashboardLinkAliased.MatchString(sql) {
			return true
		}
	}
	return false
}

// dashboardLinkPanelAt is the panel body behind one ordinal, counted the way
// grafana.Panels counts: every panel that is not a row, rows descended into,
// text panels included.
func dashboardLinkPanelAt(doc map[string]any, want int) map[string]any {
	seen := 0
	var walk func(any) map[string]any
	walk = func(panels any) map[string]any {
		for _, entry := range dashboardLinkList(panels) {
			p, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if p["type"] == "row" {
				if found := walk(p["panels"]); found != nil {
					return found
				}
				continue
			}
			if seen == want {
				return p
			}
			seen++
		}
		return nil
	}
	return walk(doc["panels"])
}

func dashboardLinkList(v any) []any {
	out, _ := v.([]any)
	return out
}

// dashboardLinkOverride is the panel's override on the Link column, or nil.
func dashboardLinkOverride(panel map[string]any) map[string]any {
	config, _ := panel["fieldConfig"].(map[string]any)
	for _, entry := range dashboardLinkList(config["overrides"]) {
		o, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := o["matcher"].(map[string]any)
		if matcher["id"] == "byName" && matcher["options"] == dashboardLinkName {
			return o
		}
	}
	return nil
}

// dashboardLinkReadOverride pulls the mapping and the data link out of one
// override. A property this file does not read is one it does not check, so
// only the four that decide the rendering are taken.
func dashboardLinkReadOverride(p *dashboardLinkPanel, override map[string]any) {
	for _, entry := range dashboardLinkList(override["properties"]) {
		prop, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		switch prop["id"] {
		case "mappings":
			dashboardLinkReadMapping(p, prop["value"])
		case "links":
			for _, l := range dashboardLinkList(prop["value"]) {
				link, _ := l.(map[string]any)
				p.href, _ = link["url"].(string)
				p.newTab, _ = link["targetBlank"].(bool)
			}
		}
	}
}

// dashboardLinkReadFrom reads the hidden form off the panel: whether the Link
// override hides the column, and which other override's data link reads it.
func dashboardLinkReadFrom(p *dashboardLinkPanel, panel map[string]any) {
	config, _ := panel["fieldConfig"].(map[string]any)
	for _, entry := range dashboardLinkList(config["overrides"]) {
		o, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := o["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		for _, raw := range dashboardLinkList(o["properties"]) {
			prop, isMap := raw.(map[string]any)
			if !isMap {
				continue
			}
			if name == dashboardLinkName && prop["id"] == "custom.hidden" && prop["value"] == true {
				p.hidden = true
			}
			if prop["id"] != "links" {
				continue
			}
			for _, l := range dashboardLinkList(prop["value"]) {
				link, _ := l.(map[string]any)
				if link["url"] == dashboardLinkRowHref {
					p.from = name
					p.newTab, _ = link["targetBlank"].(bool)
				}
			}
		}
	}
}

func dashboardLinkReadMapping(p *dashboardLinkPanel, value any) {
	for _, m := range dashboardLinkList(value) {
		mapping, _ := m.(map[string]any)
		if mapping["type"] != "regex" {
			continue
		}
		options, _ := mapping["options"].(map[string]any)
		p.pattern, _ = options["pattern"].(string)
		result, _ := options["result"].(map[string]any)
		p.text, _ = result["text"].(string)
	}
}

// dashboardLinkOrganize is the panel's organize transformation, which is what names
// the columns. Grafana applies it in the browser over the frame the datasource
// returned, so a test that reads the frame has to apply it too.
func dashboardLinkOrganize(panel map[string]any) (rename map[string]string, include, exclude map[string]bool) {
	rename, include, exclude = map[string]string{}, map[string]bool{}, map[string]bool{}
	for _, entry := range dashboardLinkList(panel["transformations"]) {
		tf, ok := entry.(map[string]any)
		if !ok || tf["id"] != "organize" {
			continue
		}
		options, _ := tf["options"].(map[string]any)
		maps.Copy(rename, dashboardLinkStringMap(options["renameByName"]))
		maps.Copy(include, dashboardLinkBoolMap(options["includeByName"]))
		maps.Copy(exclude, dashboardLinkBoolMap(options["excludeByName"]))
	}
	return rename, include, exclude
}

func dashboardLinkStringMap(v any) map[string]string {
	raw, _ := v.(map[string]any)
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func dashboardLinkBoolMap(v any) map[string]bool {
	raw, _ := v.(map[string]any)
	out := make(map[string]bool, len(raw))
	for k, val := range raw {
		if b, ok := val.(bool); ok {
			out[k] = b
		}
	}
	return out
}

// ── asking the stores ───────────────────────────────────────────────────────

// dashboardLinkAnswers asks one store for every panel that selects a Link
// column and returns what became of the column in each.
//
// The panels are asked again rather than read out of the shared run: that run
// keeps how many values a panel drew and not which, and a second pass over two
// dozen panels costs seconds, where carrying every frame of every panel through
// the whole suite would cost memory for the rest of it.
func dashboardLinkAnswers(t *testing.T, s *Stack, doc map[string]any,
	store dashboardStore, panels map[int]*dashboardLinkPanel,
) map[int]dashboardLinkColumn {
	t.Helper()
	queries := make([]grafana.PanelQuery, 0, len(panels))
	for _, index := range slices.Sorted(maps.Keys(panels)) {
		queries = append(queries, panels[index].query)
	}
	repos := dashboardRepos(sqlStoresPoints(t, sqlStoresRun(t.Context(), t, s)))
	client := grafana.Client{URL: s.GrafanaURL, Token: s.GrafanaToken}
	results := client.CheckPanels(t.Context(), dashboardRange, "now", queries,
		dashboardVars(doc, store, repos), grafana.Options{
			Timeout: 120 * time.Second, Workers: dashboardWorkers(store.name),
			IntervalMs: dashboardInterval, MaxDataPoints: dashboardMaxDataPoints,
		})
	out := make(map[int]dashboardLinkColumn, len(results))
	for i := range results {
		index := results[i].Panel.Index
		out[index] = dashboardLinkRead(results[i].Answer, panels[index])
	}
	return out
}

// dashboardLinkRead finds the field that becomes the Link column in one answer,
// and reads it.
func dashboardLinkRead(res map[string]any, p *dashboardLinkPanel) dashboardLinkColumn {
	var out dashboardLinkColumn
	results, _ := res["results"].(map[string]any)
	for _, ref := range slices.Sorted(maps.Keys(results)) {
		answer, _ := results[ref].(map[string]any)
		for _, entry := range dashboardLinkList(answer["frames"]) {
			frame, _ := entry.(map[string]any)
			dashboardLinkReadFrame(frame, p, &out)
		}
	}
	return out
}

func dashboardLinkReadFrame(frame map[string]any, p *dashboardLinkPanel, out *dashboardLinkColumn) {
	schema, _ := frame["schema"].(map[string]any)
	fields := dashboardLinkList(schema["fields"])
	data, _ := frame["data"].(map[string]any)
	columns := dashboardLinkList(data["values"])
	for i, entry := range fields {
		field, _ := entry.(map[string]any)
		name, _ := field["name"].(string)
		final := dashboardLinkNameOf(name, p)
		if final != "" && !slices.Contains(out.columns, final) {
			out.columns = append(out.columns, final)
		}
		if final != dashboardLinkName {
			continue
		}
		out.from = name
		if kind, ok := field["type"].(string); ok {
			out.kind = kind
		}
		if i < len(columns) {
			dashboardLinkReadValues(dashboardLinkList(columns[i]), out)
		}
	}
}

func dashboardLinkReadValues(column []any, out *dashboardLinkColumn) {
	for _, v := range column {
		text, _ := v.(string)
		out.values = append(out.values, text)
	}
}

// dashboardLinkNameOf is what one field of the frame is called once the panel's
// organize transformation has run: renamed, kept, or dropped.
func dashboardLinkNameOf(name string, p *dashboardLinkPanel) string {
	if p.exclude[name] {
		return ""
	}
	if len(p.include) > 0 && !p.include[name] {
		return ""
	}
	if to, ok := p.rename[name]; ok && to != "" {
		return to
	}
	return name
}

// TestTheSweepWritesNoURLThatIsNotOne asks the same question of the sweep
// rather than of the panels, which is the wider half of it: the checks above
// only see the values a panel drew, after its own filters and its own limit,
// and this sees every url the sweep wrote for every measurement.
//
// It used to hold a list of two: gh_repo_community and gh_repo_policy each
// built their url by appending a path to the repository's own url, and the
// fixtures answered without one, so what reached the store was the suffix
// alone. The list is gone because the concatenation is: internal/collect/urls.go
// is the only place a url is assembled now, it answers with nothing when a part
// it needs is missing, and setNonEmpty keeps the field off the point. So there
// is nothing left to excuse, and any value landing here is a defect.
func TestTheSweepWritesNoURLThatIsNotOne(t *testing.T) {
	s := Start(t)
	written := map[string]bool{}
	for _, facts := range dashboardLinkURLs(t, s) {
		for v := range facts.values {
			if !dashboardLinkRendering.MatchString(v) {
				written[v] = true
			}
		}
	}
	for _, value := range slices.Sorted(maps.Keys(written)) {
		t.Errorf("the sweep writes %q as a url, and no Link column can render that as %q: a "+
			"relative path sends the reader to the Grafana host rather than to GitHub",
			value, dashboardLinkText)
	}
}
