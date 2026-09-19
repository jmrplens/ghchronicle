// Package dashboards builds the Grafana dashboard, once per store.
//
// It lived under cmd/internal while its only readers were the development
// commands that write the JSON under dashboards/. The shipped binary publishes
// the dashboard itself now, so it is product code and sits where product code
// does; nothing else about it changed in the move.
//
// Everything here is deliberately plain maps. Grafana's JSON is the contract;
// a type per panel kind in between would only hide which key made a panel
// behave the way it does, and the shapes differ enough between kinds that the
// types would be mostly empty anyway.
package dashboards

import (
	"maps"
	"strings"
)

// Names Grafana and Elasticsearch answer to, spelled once because a panel that
// spells one of them its own way is not wrong, it is silently empty. The
// section files share them.
const (
	panelClassicPalette   = "palette-classic"
	panelWidthField       = "custom.width"
	panelCellOptionsField = "custom.cellOptions"
	panelRepoField        = "repo.keyword"
	// The full name, for the panels over a measurement that can hold more
	// than one owner, where the short name identifies nothing.
	panelFullNameField = "full_name.keyword"
	panelURLField      = "url.keyword"
	panelESTime        = "@timestamp"
)

// Grafana names the value column of a joined query after the query's letter,
// so a table that joins several of them renames these.
const (
	panelValueA = "Value #A"
	panelValueB = "Value #B"
	panelValueC = "Value #C"
	panelValueD = "Value #D"
	panelValueE = "Value #E"
	panelValueF = "Value #F"
	panelValueG = "Value #G"
)

// Opts is a panel builder's options: the same keyword arguments the panel
// constructors take, carried as data so a specification can set one without
// naming all the others.
type Opts map[string]any

// merge returns a copy of a with b's entries on top.
func mergeOpts(a, b Opts) Opts {
	out := Opts{}
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

func optString(o Opts, key, def string) string {
	if v, ok := o[key].(string); ok {
		return v
	}
	return def
}

func optBool(o Opts, key string, def bool) bool {
	if v, ok := o[key].(bool); ok {
		return v
	}
	return def
}

func optInt(o Opts, key string, def int) int {
	switch v := o[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

func optFloat(o Opts, key string, def float64) float64 {
	switch v := o[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func optAny(o Opts, key string) (any, bool) {
	v, ok := o[key]
	return v, ok
}

func optList(o Opts, key string) []any {
	if v, ok := o[key].([]any); ok {
		return v
	}
	return nil
}

// ids are per dashboard: Grafana wants every panel in one dashboard to have a
// distinct id, and the same panel in two dashboards may share one.
type ids struct{ n int }

func (i *ids) next() int {
	i.n++
	return i.n
}

func row(id int, title string, y int, collapsed bool, panels []map[string]any) map[string]any {
	if panels == nil {
		panels = []map[string]any{}
	}
	return map[string]any{
		"id": id, "type": "row", "title": title, "collapsed": collapsed,
		"gridPos": map[string]any{"w": 24, "h": 1, "x": 0, "y": y},
		"panels":  panels,
	}
}

// box is a panel's gridPos: how wide and tall it is, and the corner of
// Grafana's 24 column grid it starts from. The four numbers travel as one
// value because they are one thing, and because as four bare arguments in a
// row nothing at a call site told the size from the corner.
type box struct{ W, H, X, Y int }

// panelArgs is what every panel kind carries into Grafana's JSON unchanged:
// which panel it is, where it sits and what it asks its store. The kinds
// differ only in the options that follow it, so they all take this one named
// argument instead of the same nine values in the same order.
type panelArgs struct {
	ID      int
	Title   string
	DS      any
	Targets []any
	Box     box
	Desc    string
}

func base(kind string, a panelArgs) map[string]any {
	return map[string]any{
		"id": a.ID, "type": kind, "title": a.Title, "description": a.Desc,
		"datasource": a.DS,
		"gridPos":    map[string]any{"w": a.Box.W, "h": a.Box.H, "x": a.Box.X, "y": a.Box.Y},
		"targets":    a.Targets,
		"fieldConfig": map[string]any{
			"defaults": map[string]any{}, "overrides": []any{},
		},
		"options": map[string]any{},
	}
}

func timeseries(a panelArgs, o Opts) map[string]any {
	p := base("timeseries", a)
	stack := "none"
	if optBool(o, "stack", false) {
		stack = "normal"
	}
	drawStyle := "line"
	if optBool(o, "bars", false) {
		drawStyle = "bars"
	}
	// A stacked bar at ten per cent fill is a hollow box, and thirty of them
	// on one day are a tower of outlines in colors that cannot be told apart.
	// Half opacity keeps the stack readable; a line keeps the light fill.
	fill, gradient := 10, "opacity"
	if stack == "normal" && drawStyle == "bars" {
		fill, gradient = 50, "none"
	}
	custom := map[string]any{
		"drawStyle": drawStyle, "lineWidth": 1,
		"fillOpacity": optInt(o, "fill", fill), "gradientMode": gradient,
		"showPoints": "never", "spanNulls": false,
		"stacking": map[string]any{"mode": stack, "group": "A"},
	}
	if optBool(o, "min_zero", true) {
		custom["axisSoftMin"] = 0
	}
	unit := optString(o, "unit", "short")
	defaults := map[string]any{
		"unit":   unit,
		"color":  map[string]any{"mode": panelClassicPalette},
		"custom": custom,
	}
	// `short` here is a count: stars, pull requests, runs. An axis reading
	// 0.25 and a legend saying "Mean: 0.750" of them is arithmetic nobody
	// asked for.
	if unit == "short" {
		defaults["decimals"] = 0
	}
	display := optString(o, "display", "")
	if display != "" {
		defaults["displayName"] = display
	}
	p["fieldConfig"] = map[string]any{
		"defaults": defaults, "overrides": overridesOf(o),
	}
	legend := optString(o, "legend", "bottom")
	// A panel with one series per tag value can carry thirty of them, and a
	// legend that adds Mean and Max to each puts every series on its own line:
	// three fit under the plot and the busiest one scrolls off it. Without
	// values the names flow inline and thirty of them are four lines.
	calcs := []any{"mean", "max"}
	if display != "" {
		calcs = []any{}
	}
	if v := optList(o, "calcs"); v != nil {
		calcs = v
	}
	p["options"] = map[string]any{
		"legend": map[string]any{
			"displayMode": "list", "placement": legend,
			"showLegend": legend != "hidden", "calcs": calcs,
		},
		"tooltip": map[string]any{"mode": "multi", "sort": "desc"},
	}
	binned(p, o)
	return p
}

// binned sets the two query options a dated panel's bucket width is computed
// from. Grafana derives the interval a query is sent with from the range, the
// panel's own `interval` floor and `maxDataPoints`, and the bucketed queries
// read it back through $__dateBin and $__interval. A fixed one-day bin drew
// 730 bars of one pixel over two years. Grafana rounds the raw interval down
// to a day until it reaches a whole week (measured: 6.08 days was sent as
// one), so the ceiling is a hundred points, which is what makes two years
// come out as weeks; a year is still days, at five pixels each, and nothing
// is ever finer than the floor the panel names.
func binned(p map[string]any, o Opts) {
	if interval := optString(o, "interval", ""); interval != "" {
		p["interval"] = interval
		p["maxDataPoints"] = optInt(o, "max_points", 100)
	}
}

func barchart(a panelArgs, o Opts) map[string]any {
	p := base("barchart", a)
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{
			"unit": optString(o, "unit", "short"), "color": map[string]any{"mode": panelClassicPalette},
			"custom": map[string]any{
				"lineWidth": 1, "fillOpacity": 80, "gradientMode": "hue", "axisSoftMin": 0,
			},
		},
		"overrides": overridesOf(o),
	}
	orientation := "horizontal"
	if !optBool(o, "horizontal", true) {
		orientation = "vertical"
	}
	// A label longer than this is an owner/repo pair or a compound license
	// name, and unclipped it walks out of the left edge of the panel; clipped
	// the tooltip still carries the whole name.
	p["options"] = map[string]any{
		"orientation": orientation, "showValue": "auto", "stacking": "none",
		"xTickLabelRotation": 0, "xTickLabelMaxLength": 22,
		"xTickLabelSpacing": optInt(o, "tick_spacing", 0),
		"legend": map[string]any{
			"showLegend": false, "displayMode": "list", "placement": "bottom",
		},
		"tooltip": map[string]any{"mode": "single", "sort": "none"},
	}
	return p
}

func table(a panelArgs, o Opts) map[string]any {
	p := base("table", a)
	// fieldMinMax scales a gauge cell by its own column. Without it Grafana
	// takes the range over every numeric column of the frame, so a column of
	// seconds or bytes beside a count puts the count's bar at a pixel or
	// nothing: 48 merged pull requests drew a blank cell.
	// minWidth is what a column without a width of its own may shrink to.
	// Grafana's default is 150, which on a phone is half the grid: nine
	// columns at 150 are 1,270 pixels of a 356 pixel table, and the column
	// the table is sorted by was off the screen in 31 of 47 tables.
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{"fieldMinMax": true, "custom": map[string]any{
			"align": "auto", "cellOptions": map[string]any{"type": "auto"}, "filterable": false,
			"minWidth": tableMinWidth,
		}},
		"overrides": hideLinkedFrom(overridesOf(o)),
	}
	footer := any(map[string]any{"show": false, "reducer": []any{"sum"}, "countRows": false})
	if v, ok := optAny(o, "footer"); ok {
		footer = v
	}
	// `sort` is the column the table opens sorted by, biggest first, which is
	// what a ranked table wants. `sort_leading` is a column sorted the other
	// way in front of it: Grafana sorts by the whole list in order, so a flag
	// column first groups the rows without hiding any of them. "Every
	// repository, ever" is ranked by commits and a fork of a busy project
	// outranks everything the account wrote, 370,296 commits against 3,385;
	// leading on Fork puts the account's own work first and leaves the forks
	// where the title promises they are. A store whose frame has no such
	// column is not left unsorted: measured on Grafana 13.2.1, a sort key
	// naming a column that is not there is skipped and the next one still
	// sorts.
	sortBy := []any{}
	if s := optString(o, "sort_leading", ""); s != "" {
		sortBy = append(sortBy, map[string]any{"displayName": s, "desc": false})
	}
	if s := optString(o, "sort", ""); s != "" {
		sortBy = append(sortBy, map[string]any{"displayName": s, "desc": true})
	}
	// Small rows, unless the table draws something a small row cannot hold:
	// the badge images of the achievement progress need the large one.
	p["options"] = map[string]any{
		"cellHeight": optString(o, "cell_height", "sm"), "showHeader": true, "footer": footer, "sortBy": sortBy,
	}
	if tf := optList(o, "transformations"); tf != nil {
		p["transformations"] = tf
	}
	return p
}

func stat(a panelArgs, o Opts) map[string]any {
	p := base("stat", a)
	color := optString(o, "color", "text")
	thresholds := optList(o, "thresholds")
	mappings := optList(o, "mappings")
	if mappings == nil {
		mappings = []any{}
	}
	mode := "fixed"
	colorMode := "none"
	steps := []any{map[string]any{"color": color, "value": nil}}
	if thresholds != nil {
		mode, colorMode, steps = "thresholds", "value", thresholds
	}
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{
			"unit": optString(o, "unit", "short"), "mappings": mappings,
			"color":      map[string]any{"mode": mode, "fixedColor": color},
			"thresholds": map[string]any{"mode": "absolute", "steps": steps},
		},
		"overrides": overridesOf(o),
	}
	p["options"] = map[string]any{
		"reduceOptions": map[string]any{
			"calcs": []any{optString(o, "calc", "lastNotNull")}, "fields": "", "values": false,
		},
		"orientation": "auto", "textMode": optString(o, "text_mode", "value"),
		"colorMode": colorMode, "graphMode": optString(o, "graph", "none"),
		"justifyMode": "center",
	}
	return p
}

func gauge(a panelArgs, o Opts) map[string]any {
	p := base("gauge", a)
	steps := optList(o, "thresholds")
	if steps == nil {
		steps = []any{map[string]any{"color": "green", "value": nil}}
	}
	maxv := any(optInt(o, "maxv", 100))
	if v, ok := optAny(o, "maxv"); ok {
		maxv = v
	}
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{
			"unit": optString(o, "unit", "percent"), "min": optInt(o, "minv", 0), "max": maxv,
			"color":      map[string]any{"mode": "thresholds"},
			"thresholds": map[string]any{"mode": "absolute", "steps": steps},
		},
		"overrides": overridesOf(o),
	}
	p["options"] = map[string]any{
		"reduceOptions": map[string]any{
			"calcs": []any{"lastNotNull"}, "fields": "", "values": false,
		},
		"showThresholdLabels": false, "showThresholdMarkers": true,
	}
	return p
}

func piechart(a panelArgs, o Opts) map[string]any {
	p := base("piechart", a)
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{
			"unit": optString(o, "unit", "short"), "color": map[string]any{"mode": panelClassicPalette},
			"custom": map[string]any{"hideFrom": map[string]any{
				"legend": false, "tooltip": false, "viz": false,
			}},
		},
		"overrides": overridesOf(o),
	}
	// The share of each slice in the legend, and nothing written on the
	// slices: a legend of names only with the percentages on the slices
	// never drew the percentage of a thin slice, since Grafana leaves a label
	// off a slice it does not fit. The legend sits beside the chart by
	// default; `legend` moves it. Under 992 CSS pixels Grafana puts any
	// legend below the chart and caps it at 35 per cent of the panel,
	// whatever the placement says (VizLayout, measured on 13.2.1), and what
	// it draws there depends on the placement asked for: a legend placed
	// right is a column, one entry per line, and a legend placed bottom in
	// list mode flows and wraps, two or three entries per line. A chart whose
	// legend has to be complete on a phone asks for bottom and list, and the
	// spec gives it the height the 35 per cent needs. `legend_mode` "table"
	// gives each entry a line of its own and a header to sort by, at the
	// same cost on a phone as a column.
	values := []any{"percent"}
	if v := optList(o, "legend_values"); v != nil {
		values = v
	}
	labels := []any{}
	if v := optList(o, "labels"); v != nil {
		labels = v
	}
	p["options"] = map[string]any{
		"displayLabels": labels,
		"legend": map[string]any{
			"displayMode": optString(o, "legend_mode", "list"), "placement": optString(o, "legend", "right"),
			"showLegend": true, "values": values,
		},
		"pieType": "donut",
		"reduceOptions": map[string]any{
			"calcs": []any{"lastNotNull"}, "fields": "", "values": true,
		},
		"tooltip": map[string]any{"mode": "single", "sort": "none"},
	}
	return p
}

// textPanel is a panel of prose: the note a store shows in place of a panel
// it cannot answer, in markdown, or the brand header, in html. `mode` picks
// the second, and `transparent` drops the panel's background and border,
// which is what lets the header sit on the page as a masthead rather than
// in a box.
// statusHistory is a grid of colored cells: one row per field, one column
// per time bucket, the cell colored by its value through the field's
// thresholds and value mappings. It is what draws the contribution calendar
// the way GitHub does, weeks along the x axis and weekdays down the y, since
// Grafana has no calendar panel: a frame of one row per week and one field
// per weekday is that grid. The row order is the field order, first at the
// top.
func statusHistory(a panelArgs, o Opts) map[string]any {
	p := base("status-history", a)
	mappings := optList(o, "mappings")
	if mappings == nil {
		mappings = []any{}
	}
	steps := optList(o, "thresholds")
	if steps == nil {
		steps = []any{map[string]any{"color": "green", "value": nil}}
	}
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{
			"unit": optString(o, "unit", "short"), "mappings": mappings,
			"color":      map[string]any{"mode": "thresholds"},
			"thresholds": map[string]any{"mode": "absolute", "steps": steps},
			"custom": map[string]any{
				"lineWidth": 0, "fillOpacity": 100,
			},
		},
		"overrides": overridesOf(o),
	}
	// The two fractions are the cell's share of the row and of the column,
	// so they are the panel's only control over the shape of a cell: the
	// contribution calendar sets both to draw GitHub's square.
	p["options"] = map[string]any{
		"showValue": "never",
		"rowHeight": optFloat(o, "row_height", 0.8), "colWidth": optFloat(o, "col_width", 0.8),
		"legend":  map[string]any{"showLegend": false, "displayMode": "list", "placement": "bottom"},
		"tooltip": map[string]any{"mode": "single", "sort": "none"},
	}
	binned(p, o)
	return p
}

// barGauge is one horizontal bar per field, named and valued: the shape of
// a mix of a few parts, each a share of the whole.
func barGauge(a panelArgs, o Opts) map[string]any {
	p := base("bargauge", a)
	maxv := any(optInt(o, "maxv", 100))
	if v, ok := optAny(o, "maxv"); ok {
		maxv = v
	}
	p["fieldConfig"] = map[string]any{
		"defaults": map[string]any{
			"unit": optString(o, "unit", "percent"), "min": optInt(o, "minv", 0), "max": maxv,
			// One decimal: seven reviews among six thousand contributions are
			// 0.1 per cent, and a bar reading 0 beside seven is a bar that lies.
			"decimals": 1,
			"color":    map[string]any{"mode": panelClassicPalette},
		},
		"overrides": overridesOf(o),
	}
	p["options"] = map[string]any{
		"reduceOptions": map[string]any{
			"calcs": []any{optString(o, "calc", "lastNotNull")}, "fields": "", "values": false,
		},
		"orientation": "horizontal", "displayMode": "gradient", "valueMode": "color",
		"namePlacement": "left", "showUnfilled": true, "sizing": "auto",
		"minVizWidth": 8, "minVizHeight": 16, "maxVizHeight": 300,
	}
	return p
}

func textPanel(a panelArgs, content string, o Opts) map[string]any {
	// Prose asks no store anything, so the three fields that describe a query
	// are the panel's own whatever the caller carried in panelArgs: the content
	// is the panel, and there is no description beside it.
	a.DS, a.Targets, a.Desc = nil, []any{}, ""
	p := base("text", a)
	delete(p, "datasource")
	p["options"] = map[string]any{"mode": optString(o, "mode", "markdown"), "content": content}
	if optBool(o, "transparent", false) {
		p["transparent"] = true
	}
	return p
}

func overridesOf(o Opts) []any {
	if v := optList(o, "overrides"); v != nil {
		return v
	}
	return []any{}
}

// ── field overrides ─────────────────────────────────────────────────────────

func override(name string, props []any) any {
	return map[string]any{
		"matcher":    map[string]any{"id": "byName", "options": name},
		"properties": props,
	}
}

func width(name string, w int) any {
	return override(name, []any{map[string]any{"id": panelWidthField, "value": w}})
}

// colorOf pins a series to a color by its name, for the panels whose series
// are outcomes. The palette hands colors out in legend order, which put
// FAILURE in green and SUCCESS in blue on the gate chart.
func colorOf(name, color string) any {
	return override(name, []any{map[string]any{
		"id": "color", "value": map[string]any{"mode": "fixed", "fixedColor": color},
	}})
}

// dayWidth is the narrowest a column of days may be. Grafana renders the
// unit as words, "6.43 years" and "3.9 weeks", and at seventy pixels those
// read ".43 years" and ".9 weeks".
const dayWidth = 100

// noValueOf is what one value of a stat group reads when its query answers
// nothing.
//
// A stat panel with no value draws an empty space, and a group draws that
// space under the value's own label: "Time to review by someone else" was a
// labeled hole in the middle of six numbers, which reads as a broken panel
// rather than as an answer. Grafana's `noValue` fills it with a sentence. It
// is for a null that means something, not for a zero: a count that is genuinely
// zero already prints 0.
func noValueOf(name, text string) any {
	return override(name, []any{map[string]any{"id": "noValue", "value": text}})
}

func unitOf(name, unit string, w int) any {
	if unit == "d" && w > 0 {
		w = max(w, dayWidth)
	}
	props := []any{map[string]any{"id": "unit", "value": unit}}
	if w > 0 {
		props = append(props, map[string]any{"id": panelWidthField, "value": w})
	}
	return override(name, props)
}

// Every link column shares one data link: the cell's own raw value, opened
// in a new tab. The href is the value and nothing else, so a value that is
// not an absolute url is a link to the wrong place, which is what
// cmd/check_dashboards holds every link column to.
const linkHref = "${__value.raw}"

// linkColumn is the column of urls a table selects, rendered as one short word
// that opens the item. Every table names that column "Link", so the name is
// here rather than at each call. The value mapping keeps the column narrow
// without hiding anything: the link still opens the raw url, and a row whose
// item has no page of its own matches no pattern and stays empty, which is the
// honest rendering of a missing url.
func linkColumn() any {
	return urlColumn("Link", "Open", "Open on GitHub")
}

// ownerLink is linkColumn for a page GitHub shows to the owner alone: the
// settings of a repository, its traffic graph, its deployments. The word in
// the cell is the same, and the title the browser shows on hover says which
// page it is and that another reader gets a 404 from it.
func ownerLink(page string) any {
	return urlColumn("Link", "Open", "Open "+page+ownerNote)
}

// linkColumnAs is a second link column beside Link, for a row that has two
// pages: the deployment and the environment it put live.
func linkColumnAs(name, title string) any {
	return urlColumn(name, "Open", title)
}

// downloadColumn is the column whose url is a file rather than a page, which
// is what gh_release_asset.url is: the browser downloads it on the click. The
// word says so, so nobody opens a fifty megabyte binary expecting a page.
func downloadColumn() any {
	return urlColumn("Download", "Download", "Download the asset")
}

// rawURLColumn is a url shown as itself and still clickable, for the one
// table whose point is to read the url: the social accounts are a check of
// the profile's sameAs links, and a reader wants to see the host.
func rawURLColumn(name string) any {
	return override(name, []any{
		map[string]any{"id": "links", "value": []any{map[string]any{
			"title": "Open the profile", "url": linkHref, "targetBlank": true,
		}}},
	})
}

func urlColumn(name, word, title string) any {
	// Wide enough for the word and no wider: the column is one word.
	w := 70
	if word != "Open" {
		w = 100
	}
	return override(name, []any{
		map[string]any{"id": "mappings", "value": []any{map[string]any{
			"type": "regex", "options": map[string]any{
				"pattern": "^https?://.+",
				"result":  map[string]any{"text": word, "index": 0},
			},
		}}},
		map[string]any{"id": "links", "value": []any{map[string]any{
			"title": title, "url": linkHref, "targetBlank": true,
		}}},
		map[string]any{"id": panelWidthField, "value": w},
		map[string]any{"id": "custom.align", "value": "center"},
	})
}

// pageLink is a panel link: the one page a whole panel is about, for a table
// or a stat whose rows have no url of their own.
func pageLink(title, url string) Opts {
	return Opts{"links": []any{map[string]any{"title": title, "url": url, "targetBlank": true}}}
}

// ownerNote is what every link to a page GitHub shows the owner alone says
// in its title, so a reader who is not the owner learns before the click
// that the page answers them with a 404.
const ownerNote = " (owner only)"

// ownerPageLink is pageLink for such a page: the account's billing, which
// no other reader can open.
func ownerPageLink(title, url string) Opts {
	return pageLink(title+ownerNote, url)
}

// barLink hangs a link on the bars of a chart: the bar's own value has no
// url, so the link reads the row's url column, which is selected beside the
// number and hidden from the drawing. A click on a bar opens the item.
func barLink(value, urls, title string) []any {
	return []any{
		override(value, []any{
			map[string]any{"id": "links", "value": []any{map[string]any{
				"title": title, "url": rowHref(urls), "targetBlank": true,
			}}},
		}),
		override(urls, []any{
			map[string]any{"id": "custom.hideFrom", "value": map[string]any{
				"viz": true, "legend": true, "tooltip": true,
			}},
		}),
	}
}

// rowHref is the data link that reads another column of the same row.
func rowHref(column string) string { return "${__data.fields." + column + "}" }

// linkedColumn reports the column an override's data link reads, the cell's
// own value or another column of the row, and nothing for an override that
// links nowhere. It is what panel() keeps per store and what the checkers
// read back, so the two agree on what a link column is.
func linkedColumn(o any) string {
	m, ok := o.(map[string]any)
	if !ok {
		return ""
	}
	matcher, _ := m["matcher"].(map[string]any)
	if matcher["id"] != "byName" {
		return ""
	}
	name, _ := matcher["options"].(string)
	props, _ := m["properties"].([]any)
	for _, raw := range props {
		p, isMap := raw.(map[string]any)
		if !isMap || p["id"] != "links" {
			continue
		}
		links, _ := p["value"].([]any)
		for _, l := range links {
			link, _ := l.(map[string]any)
			url, _ := link["url"].(string)
			if url == linkHref {
				return name
			}
			if column, found := strings.CutPrefix(url, "${__data.fields."); found {
				return strings.TrimSuffix(column, "}")
			}
		}
	}
	return ""
}

// barCell renders a numeric column as a gradient bar. The scale always starts
// at zero: a bar whose baseline is the smallest value in the column makes the
// smallest row look like nothing at all rather than like what it is.
//
// The bar is one color at every value. A gauge cell takes Grafana's default
// thresholds when none are set, green up to eighty and red from there, and
// once each column scaled by itself (fieldMinMax) that step became visible:
// 357 views of a path and 136 thousand commits of a repository drew red, as
// if a count could be too high. None of these columns is a rate with a
// ceiling; the one that is, the cache against its ten gigabytes, sets its
// own steps and does not come through here.
func barCell(name, unit string, w int) any {
	props := []any{
		map[string]any{"id": panelCellOptionsField, "value": map[string]any{
			"type": "gauge", "mode": "gradient",
		}},
		map[string]any{"id": "unit", "value": unit},
		map[string]any{"id": "min", "value": 0},
		map[string]any{"id": "thresholds", "value": map[string]any{
			"mode": "absolute", "steps": []any{map[string]any{"color": barColor, "value": nil}},
		}},
	}
	if w > 0 {
		props = append(props, map[string]any{"id": panelWidthField, "value": w})
	}
	return override(name, props)
}

// barColor is the one color every barCell is drawn in.
const barColor = "green"

// tableMinWidth is the narrowest a column with no width override is drawn.
const tableMinWidth = 90

// whenWidth is what sixteen characters of a moment to the minute need. At
// 125 the cell padding left "2026-09-08 09:14" as "09:1", on a desktop as on
// a phone; Inter's digits are not the same width, so 18:27 fit and 09:14 did
// not.
const whenWidth = 135

// when is a column of moments. A merge or a star is dated to the second by
// the collector, and a cell showing the seconds is 180 pixels wide on a phone
// whose whole table has 356: the minute is what a reader needs.
func when(name string) any {
	return unitOf(name, "time: YYYY-MM-DD HH:mm", whenWidth)
}

// linkOn hangs the row's link on the named column, the first one of the
// table, and reads the url from the Link column the query selects. On a
// phone the Link column at the far right was reached in two of twenty-eight
// tables; the first column is always on the screen. The Link column itself
// is hidden by table(), see hideLinkedFrom, so the seventy pixels go to the
// columns that carry a value.
func linkOn(column string) any {
	return rowLinkOn(column, "Open on GitHub")
}

// ownerLinkOn is linkOn for a page GitHub shows to the owner alone, with the
// title ownerLink gives such a link.
func ownerLinkOn(column, page string) any {
	return rowLinkOn(column, "Open "+page+ownerNote)
}

func rowLinkOn(column, title string) any {
	return override(column, []any{
		map[string]any{"id": "links", "value": []any{map[string]any{
			"title": title, "url": rowHref("Link"), "targetBlank": true,
		}}},
	})
}

// hideLinkedFrom adds a hidden override for every column another column's
// data link reads. The url column has to be in the frame for the link to
// read it, and there is no reason to draw it twice; and it must carry no
// value mapping, because ${__data.fields.X} is the cell's display text, so a
// mapping that turns the url into the word Open would make the link open the
// word. The hidden column keeps its raw url for that reason.
func hideLinkedFrom(overrides []any) []any {
	hidden := map[string]bool{}
	for _, o := range overrides {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		if matcher, _ := m["matcher"].(map[string]any); matcher["id"] == "byName" {
			if name, _ := matcher["options"].(string); name != "" && hiddenColumn(m) {
				hidden[name] = true
			}
		}
	}
	out := overrides
	for _, o := range overrides {
		column := linkedColumn(o)
		m, _ := o.(map[string]any)
		matcher, _ := m["matcher"].(map[string]any)
		own, _ := matcher["options"].(string)
		if column == "" || column == own || hidden[column] {
			continue
		}
		out = append(out, override(column, []any{
			map[string]any{"id": "custom.hidden", "value": true},
		}))
		hidden[column] = true
	}
	return out
}

// hiddenColumn reports whether an override already hides its column.
func hiddenColumn(o map[string]any) bool {
	props, _ := o["properties"].([]any)
	for _, raw := range props {
		p, _ := raw.(map[string]any)
		if p["id"] == "custom.hidden" && p["value"] == true {
			return true
		}
	}
	return false
}
