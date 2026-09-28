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
// stores have; a bar chart by its bars, the name each is drawn under and its
// length. A time series is not compared, nor a bar chart over time: each store
// buckets those by a step of its own, which is not a difference a reader can
// see.
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
	// stores are the stores the reason is about. A difference between two
	// stores is excused when either of them is named here.
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
		title: "Commits by repository", stores: []string{"graphite", "elasticsearch", "prometheus"},
		reason: "Named in full here", only: []string{"Repository"},
	},
	// The two years before this one are outside the thirty days.
	{
		title: "Contributions by year", stores: []string{"graphite", "elasticsearch"},
		reason: "answers only inside the dashboard range",
	},
	// No issue closed in the range: the others say so on the tile.
	{
		title: "Merged and closed in range", stores: []string{"elasticsearch"},
		reason: "a median or an average with nothing in the range", only: []string{"Time to close an issue"},
	},
	// Two of the four jobs queued in the same hour: 50 where the others
	// read 47.5.
	{
		title: "Runs in range", stores: []string{"graphite"},
		reason: "so the medians are over what the storage kept", only: []string{"Queue wait"},
	},
	{
		title: "Force pushes", stores: []string{"graphite"},
		reason: "Graphite has no way to sort by date, so this counts them per repository, branch and actor over the range.",
	},
	{
		title: "Discussions", stores: []string{"graphite"},
		reason: "Answered is the share of its discussions that have an answer", only: []string{"Answered"},
	},
	// The fixture's categories take no answer, which the SQL stores read
	// from the category and Elasticsearch cannot.
	{
		title: "Latest discussions", stores: []string{"elasticsearch"},
		reason: "a discussion in a category that takes no answer reads yes or no here rather than n/a",
		only:   []string{"Answered"},
	},
	// The one answer is from July, which the SQL stores list whatever the
	// range.
	{
		title: "Answers elsewhere", stores: []string{"graphite"},
		reason: "Graphite answers only inside the dashboard range",
	},
	{
		title: "Answers elsewhere", stores: []string{"elasticsearch"},
		reason: "Elasticsearch answers only inside the dashboard range",
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
		reason: "the second counts distinct tags, downloaded or not", only: []string{"Releases"},
	},
	{
		title: "Downloads by release", stores: []string{"graphite"},
		reason: "Graphite names each bar repository and tag from the path.", only: []string{grafana.BarAxis},
	},
	{
		title: "Downloads by release", stores: []string{"elasticsearch"},
		reason: "the bars are named by tag", only: []string{grafana.BarAxis},
	},
	// The two code scanning alerts.
	{
		title: "Oldest open alerts", stores: []string{"elasticsearch"},
		reason: "Elasticsearch lists the Dependabot alerts alone",
	},
	{
		title: "Work elsewhere", stores: []string{"graphite"},
		reason: "an item that was seen open and then closed inside the range is a row for each",
	},
	// The tags were published before the thirty days.
	{
		title: "Container tags published", stores: []string{"graphite"},
		reason: "this counts the tags published per package over the range",
	},
	// Both files were last changed before the thirty days.
	{
		title: "Policy files", stores: []string{"graphite"},
		reason: "Graphite answers only inside the dashboard range",
	},
	{
		title: "Policy files", stores: []string{"elasticsearch"},
		reason: "Elasticsearch answers only inside the dashboard range",
	},
	// The one sponsorship began in 2021.
	{
		title: "Sponsorships", stores: []string{"graphite"},
		reason: "Graphite answers only inside the dashboard range",
	},
	{
		title: "Sponsorships", stores: []string{"elasticsearch"},
		reason: "Elasticsearch answers only inside the dashboard range",
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
	c := &dashboardComparison{run: run, drift: sweepDrift(run.sweeps), used: map[int]bool{}}
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
	dashboardCheckDiffers(t, run, c.used)
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
	run           *dashboardRun
	drift         map[string]float64
	used          map[int]bool
	pairs, agreed int
	report        strings.Builder
}

// panel compares every pair of stores on one panel and answers the
// differences no entry excuses, one line per pair.
func (c *dashboardComparison) panel(t *testing.T, index int) []string {
	t.Helper()
	pictures := dashboardPictures(t, c.run, index)
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
			if len(diffs) == 0 {
				c.agreed++
				continue
			}
			pair := a.name + " against " + b.name
			verdict := "DIFFER "
			if dashboardExcused(title, a.name, b.name, &pa, &pb, like, c.used) {
				verdict = "EXCUSED"
			} else {
				failures = append(failures, pair+": "+strings.Join(diffs, "; "))
			}
			fmt.Fprintf(&c.report, "%s %3d %s, %s: %s\n", verdict, index, title, pair, strings.Join(diffs, "; "))
		}
	}
	return failures
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
func dashboardExcused(title, a, b string, pa, pb *grafana.Picture, like grafana.Likeness,
	used map[int]bool,
) bool {
	var entries []int
	var only []string
	for i, e := range dashboardsDiffer {
		if e.title != title || (!slices.Contains(e.stores, a) && !slices.Contains(e.stores, b)) {
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

// dashboardCheckDiffers holds every entry to the panel it names: the panel
// exists, each store it names says the reason in its own description of the
// panel, and the difference it excuses is still there.
func dashboardCheckDiffers(t *testing.T, run *dashboardRun, used map[int]bool) {
	t.Helper()
	for i, e := range dashboardsDiffer {
		found := false
		for _, store := range e.stores {
			for _, o := range run.outcomes[store] {
				if o.panel.Title != e.title {
					continue
				}
				found = true
				if !strings.Contains(o.panel.Description, e.reason) {
					t.Errorf("%q is excused in %s as %q, and that is not what %s's description of the "+
						"panel says: %q", e.title, store, e.reason, store, o.panel.Description)
				}
			}
		}
		switch {
		case !found:
			t.Errorf("%q is excused in %v, and no such panel is drawn by them", e.title, e.stores)
		case !used[i]:
			t.Errorf("%q is excused in %v as %q, and the stores now draw it alike: take the entry out",
				e.title, e.stores, e.reason)
		}
	}
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
