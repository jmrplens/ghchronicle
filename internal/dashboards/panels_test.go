package dashboards

import (
	"reflect"
	"testing"
)

// optionsOf is a rendered panel's options, which every kind builds as a map.
func optionsOf(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	o, ok := p["options"].(map[string]any)
	if !ok {
		t.Fatalf("options are %T, not a map", p["options"])
	}
	return o
}

// defaultsOfKind is a rendered panel's field defaults.
func defaultsOfKind(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	fc, _ := p["fieldConfig"].(map[string]any)
	d, ok := fc["defaults"].(map[string]any)
	if !ok {
		t.Fatalf("field defaults are %T, not a map", fc["defaults"])
	}
	return d
}

// TestTimeseriesLegendTakesTheCalcsItIsGiven: a panel of a fixed handful of
// series may name its own legend values, and they win over both the Mean and
// Max a plain chart gets and the empty list a per-series legend gets.
func TestTimeseriesLegendTakesTheCalcsItIsGiven(t *testing.T) {
	t.Parallel()
	calcsOf := func(o Opts) any {
		legend, _ := optionsOf(t, timeseries(panelArgs{}, o))["legend"].(map[string]any)
		return legend["calcs"]
	}
	cases := []struct {
		name string
		opts Opts
		want []any
	}{
		{"plain", Opts{}, []any{"mean", "max"}},
		{"per series", Opts{"display": "${__field.labels.series}"}, []any{}},
		{"named", Opts{"display": "x", "calcs": []any{"sum"}}, []any{"sum"}},
	}
	for _, c := range cases {
		if got := calcsOf(c.opts); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: legend calcs are %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStatusHistoryReadsAWholeNumberCellFraction: the calendar's cell share
// is a float in Grafana's JSON, and a specification that writes it as 1 must
// get a full cell rather than the default eight tenths.
func TestStatusHistoryReadsAWholeNumberCellFraction(t *testing.T) {
	t.Parallel()
	o := optionsOf(t, statusHistory(panelArgs{}, Opts{"row_height": 1, "col_width": 0.5}))
	if o["rowHeight"] != 1.0 {
		t.Errorf("rowHeight is %#v, want 1.0", o["rowHeight"])
	}
	if o["colWidth"] != 0.5 {
		t.Errorf("colWidth is %#v, want 0.5", o["colWidth"])
	}
	o = optionsOf(t, statusHistory(panelArgs{}, Opts{"row_height": "tall"}))
	if o["rowHeight"] != 0.8 {
		t.Errorf("a row height that is not a number gave %#v, want the default 0.8", o["rowHeight"])
	}
}

// TestTableKeepsTheFooterAndTransformationsItIsGiven: a table that totals a
// column asks for its own footer, and a table that renames its columns
// carries the transformations; neither may be dropped for the default.
func TestTableKeepsTheFooterAndTransformationsItIsGiven(t *testing.T) {
	t.Parallel()
	footer := map[string]any{"show": true, "reducer": []any{"sum"}}
	tf := []any{map[string]any{"id": "organize"}}
	p := table(panelArgs{}, Opts{"footer": footer, "transformations": tf})
	if got := optionsOf(t, p)["footer"]; !reflect.DeepEqual(got, footer) {
		t.Errorf("footer is %v, want %v", got, footer)
	}
	if got := p["transformations"]; !reflect.DeepEqual(got, tf) {
		t.Errorf("transformations are %v, want %v", got, tf)
	}

	plain := table(panelArgs{}, Opts{})
	if _, ok := plain["transformations"]; ok {
		t.Error("a table given no transformations carries the key anyway")
	}
	if f, _ := optionsOf(t, plain)["footer"].(map[string]any); f["show"] != false {
		t.Errorf("the default footer is %v, want a hidden one", f)
	}
}

// TestStatKeepsTheValueMappingsItIsGiven: a stat that turns a number into a
// word does it through its mappings, and an empty list in their place would
// draw the number.
func TestStatKeepsTheValueMappingsItIsGiven(t *testing.T) {
	t.Parallel()
	mappings := []any{map[string]any{"type": "value", "options": map[string]any{"0": map[string]any{"text": "none"}}}}
	if got := defaultsOfKind(t, stat(panelArgs{}, Opts{"mappings": mappings}))["mappings"]; !reflect.DeepEqual(got, mappings) {
		t.Errorf("mappings are %v, want %v", got, mappings)
	}
	if got := defaultsOfKind(t, stat(panelArgs{}, Opts{}))["mappings"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("a stat given no mappings has %v, want an empty list", got)
	}
}

// TestGaugeWithoutThresholdsIsGreenToAHundred: the defaults a gauge falls
// back to are a percentage, one green step and a ceiling of a hundred; a
// gauge over a fraction names its own ceiling and keeps it as given.
func TestGaugeWithoutThresholdsIsGreenToAHundred(t *testing.T) {
	t.Parallel()
	d := defaultsOfKind(t, gauge(panelArgs{}, Opts{}))
	steps, _ := d["thresholds"].(map[string]any)
	want := []any{map[string]any{"color": "green", "value": nil}}
	if !reflect.DeepEqual(steps["steps"], want) {
		t.Errorf("default steps are %v, want %v", steps["steps"], want)
	}
	if d["max"] != 100 {
		t.Errorf("default max is %#v, want 100", d["max"])
	}
	if d["unit"] != "percent" {
		t.Errorf("default unit is %v, want percent", d["unit"])
	}

	own := []any{map[string]any{"color": "red", "value": nil}}
	d = defaultsOfKind(t, gauge(panelArgs{}, Opts{"thresholds": own, "maxv": 1.0}))
	if given, _ := d["thresholds"].(map[string]any); !reflect.DeepEqual(given["steps"], own) {
		t.Errorf("steps are %v, want %v", given["steps"], own)
	}
	if d["max"] != 1.0 {
		t.Errorf("max is %#v, want 1.0", d["max"])
	}
}

// TestPiechartTakesItsLegendValuesAndLabels: the share in the legend and
// nothing on the slices is the default, and a chart that wants the value in
// the legend or a label on the slices says so and gets it.
func TestPiechartTakesItsLegendValuesAndLabels(t *testing.T) {
	t.Parallel()
	o := optionsOf(t, piechart(panelArgs{}, Opts{"legend_values": []any{"value"}, "labels": []any{"name"}}))
	legend, _ := o["legend"].(map[string]any)
	if !reflect.DeepEqual(legend["values"], []any{"value"}) {
		t.Errorf("legend values are %v, want [value]", legend["values"])
	}
	if !reflect.DeepEqual(o["displayLabels"], []any{"name"}) {
		t.Errorf("display labels are %v, want [name]", o["displayLabels"])
	}

	o = optionsOf(t, piechart(panelArgs{}, Opts{}))
	legend, _ = o["legend"].(map[string]any)
	if !reflect.DeepEqual(legend["values"], []any{"percent"}) {
		t.Errorf("default legend values are %v, want [percent]", legend["values"])
	}
	if !reflect.DeepEqual(o["displayLabels"], []any{}) {
		t.Errorf("default display labels are %v, want none", o["displayLabels"])
	}
}

// TestLinkedColumnIgnoresWhatIsNotALink: panel() splits the link columns
// from the shared overrides by what this returns, so anything that is not a
// byName override with a data link to a cell or a row must read as no link,
// and a link must still be found past a property that is not one.
func TestLinkedColumnIgnoresWhatIsNotALink(t *testing.T) {
	t.Parallel()
	links := func(url string) any {
		return map[string]any{"id": "links", "value": []any{map[string]any{"url": url}}}
	}
	cases := []struct {
		name string
		o    any
		want string
	}{
		{"not a map", "Link", ""},
		{"matched by regex", map[string]any{
			"matcher":    map[string]any{"id": "byRegexp", "options": "Link"},
			"properties": []any{links(linkHref)},
		}, ""},
		{"a page of its own", override("Link", []any{links("https://github.com/")}), ""},
		{"no links at all", unitOf("Link", "s", 0), ""},
		{"past a property that is not a map", override("Link", []any{"junk", links(linkHref)}), "Link"},
		{"a link-shaped value under another property", override("Link", []any{
			map[string]any{"id": "custom.actions", "value": []any{map[string]any{"url": linkHref}}},
		}), ""},
		{"its own value", linkColumn(), "Link"},
		{"another column", linkOn("Repository"), "Link"},
	}
	for _, c := range cases {
		if got := linkedColumn(c.o); got != c.want {
			t.Errorf("%s: linkedColumn = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestUnitOfWithoutAWidthLeavesTheWidthToTheTable: a width of zero means the
// column takes the table's own, so no width property may be written, where a
// width of zero pixels would hide the column.
func TestUnitOfWithoutAWidthLeavesTheWidthToTheTable(t *testing.T) {
	t.Parallel()
	got := unitOf("Duration", "s", 0)
	want := override("Duration", []any{map[string]any{"id": "unit", "value": "s"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unitOf with no width = %v, want %v", got, want)
	}
	got = unitOf("Age", "d", 0)
	want = override("Age", []any{map[string]any{"id": "unit", "value": "d"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a day column with no width = %v, want %v", got, want)
	}
}

// TestHideLinkedFromHidesAColumnOnce: the url column a row link reads is
// hidden by one override and never two, whether the specification already
// hides it or two columns link through it; and only a byName override that
// hides a named column counts as hiding it.
func TestHideLinkedFromHidesAColumnOnce(t *testing.T) {
	t.Parallel()
	hide := func(matcher string) any {
		return map[string]any{
			"matcher":    map[string]any{"id": matcher, "options": "Link"},
			"properties": []any{map[string]any{"id": "custom.hidden", "value": true}},
		}
	}
	hiddenLink := override("Link", []any{map[string]any{"id": "custom.hidden", "value": true}})
	shownLink := override("Link", []any{map[string]any{"id": "custom.hidden", "value": false}})
	cases := []struct {
		name string
		in   []any
		want []any
	}{
		{
			"already hidden",
			[]any{"junk", hide("byName"), linkOn("Repository")},
			[]any{"junk", hide("byName"), linkOn("Repository")},
		},
		{
			"hidden by a regex, which is not the column",
			[]any{hide("byRegexp"), linkOn("Repository")},
			[]any{hide("byRegexp"), linkOn("Repository"), hiddenLink},
		},
		{
			"shown on purpose is not hidden",
			[]any{shownLink, linkOn("Repository")},
			[]any{shownLink, linkOn("Repository"), hiddenLink},
		},
		{
			"two columns link through it",
			[]any{linkOn("Repository"), linkOn("Title")},
			[]any{linkOn("Repository"), linkOn("Title"), hiddenLink},
		},
	}
	for _, c := range cases {
		if got := hideLinkedFrom(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}
}

// TestBarCellWithoutAWidthLeavesTheWidthToTheTable: the same rule as unitOf
// for a gauge cell, which is most of the numeric columns: no width asked for
// is no width property, never a column zero pixels wide.
func TestBarCellWithoutAWidthLeavesTheWidthToTheTable(t *testing.T) {
	t.Parallel()
	widthOf := func(o any) (any, bool) {
		props, _ := o.(map[string]any)["properties"].([]any)
		for _, raw := range props {
			if p, _ := raw.(map[string]any); p["id"] == panelWidthField {
				return p["value"], true
			}
		}
		return nil, false
	}
	if w, ok := widthOf(barCell("Runs", "short", 0)); ok {
		t.Errorf("a bar cell with no width has width %v", w)
	}
	if w, ok := widthOf(barCell("Runs", "short", 80)); !ok || w != 80 {
		t.Errorf("a bar cell of 80 has width %v, %v", w, ok)
	}
}
