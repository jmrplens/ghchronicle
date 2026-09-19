package dashboards

import (
	"regexp"
	"strings"
	"testing"
)

// The phone rules. Grafana lays a dashboard out in one column on a phone and
// keeps each panel's height, and the review of 2026-09-12 measured what that
// did to this one: twenty-two single-number tiles before the first chart,
// twenty-three screens before the index, and the column a table is sorted by
// off the screen in thirty-one tables of forty-seven. Each rule below is one
// of those measurements turned into a check on the specification.

// TestEveryRowButTheOverviewIsCollapsed: the second phone screen is the index
// of the sections, and a desktop's first render asks for one section's
// panels rather than all of them.
func TestEveryRowButTheOverviewIsCollapsed(t *testing.T) {
	t.Parallel()
	for i, sec := range Sections {
		if want := i > 0; sec.Collapsed != want {
			t.Errorf("section %q collapsed = %v, want %v", sec.Title, sec.Collapsed, want)
		}
	}
}

// TestEveryStatIsAGroupOfNamedValues: a stat of several named values rather
// than a tile per number, so a phone shows a group per block and a desktop a
// row of numbers. Every value is named in every store's own language, because
// the panel shows the name beside the number.
//
// Overview and Lifetime were grouped first; the review of 2026-09-14 counted
// what the other sections still cost a phone, which was twenty-nine tiles at
// 160 pixels each, 4,640 pixels, five screens of single numbers inside the
// collapsed sections. So the rule is now every section's, and a new tile
// anywhere fails here rather than in a later review.
func TestEveryStatIsAGroupOfNamedValues(t *testing.T) {
	t.Parallel()
	b := &builder{}
	groups := 0
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			if p.Kind != "stat" {
				continue
			}
			groups++
			if p.Opts["text_mode"] != "value_and_name" {
				t.Errorf("%s: %q shows values without their names", sec.Title, p.Title)
			}
			for name, st := range p.Stores {
				if n := namedValues(name, st, &p); n < 2 {
					t.Errorf("%s: %q names %d values in %s, and a tile of one number is "+
						"160 pixels of a phone; group it with its neighbors (statGroup)",
						sec.Title, p.Title, n, name)
				}
			}
		}
	}
	if groups < 12 {
		t.Fatalf("found %d stat panels, and the specification has 12 groups", groups)
	}
}

// TestAGroupColorsByFieldAndNotByPanel: a stat's unit, thresholds and color
// mode are panel-wide, so the success rate at 88.5% painted the run count
// beside it orange the moment the two shared a panel. A group therefore
// carries plainSteps and each value that is not a plain number names its own
// unit and thresholds on its own field.
func TestAGroupColorsByFieldAndNotByPanel(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, p := range renderedPanels(t, "influxdb") {
		if p["type"] != "stat" {
			continue
		}
		options, _ := p["options"].(map[string]any)
		if options["colorMode"] != "value" {
			continue
		}
		checked++
		config, _ := p["fieldConfig"].(map[string]any)
		defaults, _ := config["defaults"].(map[string]any)
		thresholds, _ := defaults["thresholds"].(map[string]any)
		steps, _ := thresholds["steps"].([]any)
		if len(steps) != 1 {
			t.Errorf("%q colors every value by %d panel-wide steps, so one value's "+
				"warning paints its neighbors", p["title"], len(steps))
		}
	}
	if checked == 0 {
		t.Fatal("no stat group colors by threshold, so this checked nothing")
	}
}

var (
	sqlAlias = regexp.MustCompile(`AS "([^"]+)"`)
	grAlias  = regexp.MustCompile(`^alias\(.*, "[^"]+"\)$`)
)

// namedValues counts the values a store's answer names: SQL column aliases,
// Prometheus legends, Graphite alias() calls, and for Elasticsearch the
// columns an organize transformation renames plus the queries an override
// names by refId.
func namedValues(store string, st *store, p *Panel) int {
	n := 0
	switch store {
	case "influxdb", "postgres":
		for i := range st.Q {
			n += len(sqlAlias.FindAllString(st.Q[i].SQL, -1))
		}
	case "prometheus":
		for i := range st.Q {
			if st.Q[i].HasLeg && st.Q[i].Legend != "" {
				n++
			}
		}
	case "graphite":
		for i := range st.Q {
			if grAlias.MatchString(st.Q[i].GTarget) {
				n++
			}
		}
	case "elasticsearch":
		for _, raw := range st.TF {
			tf, _ := raw.(map[string]any)
			options, _ := tf["options"].(map[string]any)
			rename, _ := options["renameByName"].(map[string]any)
			n += len(rename)
		}
		for _, raw := range append(append([]any{}, p.Overrides...), st.Overrides...) {
			o, _ := raw.(map[string]any)
			matcher, _ := o["matcher"].(map[string]any)
			if matcher["id"] == "byFrameRefID" {
				n++
			}
		}
	}
	return n
}

// TestTablesLetColumnsShrink: Grafana's default minimum column width is 150,
// half of a phone's table, so nine columns were 1,270 pixels of a 356 pixel
// grid.
func TestTablesLetColumnsShrink(t *testing.T) {
	t.Parallel()
	for _, p := range renderedPanels(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		config, _ := p["fieldConfig"].(map[string]any)
		defaults, _ := config["defaults"].(map[string]any)
		custom, _ := defaults["custom"].(map[string]any)
		if custom["minWidth"] != tableMinWidth {
			t.Errorf("%q lets a column shrink to %v, want %d", p["title"], custom["minWidth"], tableMinWidth)
		}
	}
}

// TestTheSortedColumnIsBesideTheFirst: a table sorted by a column the phone
// cannot show is a table sorted by nothing the reader can see, so the sorted
// column is the first or the second one the InfluxDB statement selects.
func TestTheSortedColumnIsBesideTheFirst(t *testing.T) {
	t.Parallel()
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			sort, _ := p.Opts["sort"].(string)
			st := p.Stores["influxdb"]
			if p.Kind != "table" || sort == "" || st.Q == nil {
				continue
			}
			cols := sqlAlias.FindAllStringSubmatch(st.Q[0].SQL, -1)
			if len(cols) == 0 {
				continue
			}
			first := cols[0][1]
			second := first
			if len(cols) > 1 {
				second = cols[1][1]
			}
			if sort != first && sort != second {
				t.Errorf("%s: %q is sorted by %q, which its statement selects after %q and %q",
					sec.Title, p.Title, sort, first, second)
			}
		}
	}
}

// TestDatedColumnsShowTheMinute: a column of moments to the second is 180
// pixels wide; to the minute it is whenWidth, and nobody needs the seconds
// of a merge. The width is checked too: at 125 the sixteen characters of
// "2026-09-08 09:14" lost their last digit on a desktop and on a phone alike.
func TestDatedColumnsShowTheMinute(t *testing.T) {
	t.Parallel()
	dated := regexp.MustCompile(`\b(?:[a-z]\.)?time AS "([^"]+)"`)
	for _, p := range renderedPanels(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		targets, _ := p["targets"].([]any)
		for _, raw := range targets {
			target, _ := raw.(map[string]any)
			sql, _ := target["rawSql"].(string)
			for _, m := range dated.FindAllStringSubmatch(sql, -1) {
				if unit := overrideProperty(p, m[1], "unit"); unit != "time: YYYY-MM-DD HH:mm" {
					t.Errorf("%q dates its %s column with unit %v, want the minute (when())",
						p["title"], m[1], unit)
				}
				if w := overrideProperty(p, m[1], "custom.width"); w != whenWidth {
					t.Errorf("%q draws its %s column %v pixels wide, want %d (when())",
						p["title"], m[1], w, whenWidth)
				}
			}
		}
	}
}

// TestRowLinksHangOnAVisibleColumn: every table that selects the row's url
// links from a column other than Link, hides Link, and leaves the hidden
// column unmapped, since ${__data.fields.Link} reads the cell's display text
// and a mapping to the word Open would make the link open the word.
func TestRowLinksHangOnAVisibleColumn(t *testing.T) {
	t.Parallel()
	linked := 0
	for _, p := range renderedPanels(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		targets, _ := p["targets"].([]any)
		target, _ := targets[0].(map[string]any)
		sql, _ := target["rawSql"].(string)
		if !strings.Contains(sql, `AS "Link"`) {
			continue
		}
		linked++
		from := ""
		for _, raw := range overridesOfPanel(p) {
			if linkedColumn(raw) == "Link" && matcherName(raw) != "Link" {
				from = matcherName(raw)
			}
		}
		if from == "" {
			t.Errorf("%q selects Link and no other column links from it", p["title"])
			continue
		}
		if overrideProperty(p, "Link", "custom.hidden") != true {
			t.Errorf("%q links from %s and still draws the Link column", p["title"], from)
		}
		if overrideProperty(p, "Link", "mappings") != nil {
			t.Errorf("%q maps its hidden Link column, so %s links to the mapped word", p["title"], from)
		}
	}
	if linked == 0 {
		t.Fatal("no table selects a Link column, so this checked nothing")
	}
}

func renderedPanels(t *testing.T, store string) []map[string]any {
	t.Helper()
	var out []map[string]any
	var walk func(panels []map[string]any)
	walk = func(panels []map[string]any) {
		for _, p := range panels {
			if inner, ok := p["panels"].([]map[string]any); ok {
				walk(inner)
			}
			if p["type"] != "row" {
				out = append(out, p)
			}
		}
	}
	walk(Render(store, "ds", nil))
	if len(out) == 0 {
		t.Fatal("rendered no panels")
	}
	return out
}

func overridesOfPanel(p map[string]any) []any {
	config, _ := p["fieldConfig"].(map[string]any)
	overrides, _ := config["overrides"].([]any)
	return overrides
}

func matcherName(raw any) string {
	o, _ := raw.(map[string]any)
	matcher, _ := o["matcher"].(map[string]any)
	name, _ := matcher["options"].(string)
	return name
}

// overrideProperty is the value of one property of the byName override on a
// column, or nil when no override sets it.
func overrideProperty(p map[string]any, column, id string) any {
	for _, raw := range overridesOfPanel(p) {
		if matcherName(raw) != column {
			continue
		}
		o, _ := raw.(map[string]any)
		props, _ := o["properties"].([]any)
		for _, entry := range props {
			prop, _ := entry.(map[string]any)
			if prop["id"] == id {
				return prop["value"]
			}
		}
	}
	return nil
}
