package dashboards

import (
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// One panel specification for every dashboard.
//
// A user who picks Prometheus, PostgreSQL, Graphite or Elasticsearch should
// get the same dashboard as one who picks InfluxDB, not a smaller cousin. So
// there is one ordered list of sections and panels here, and each panel
// carries one query set per store: SQL for InfluxDB 3 (DataFusion dialect),
// Prom for Prometheus, PG for PostgreSQL, GR for Graphite and ES for
// Elasticsearch. Nothing about a panel's place, size or title lives anywhere
// but here.
//
// The only legitimate differences between the outputs are what each store can
// honestly answer and the query language:
//
//   - InfluxDB and PostgreSQL keep a row per fact, dated when it happened, so
//     their queries look back over the dashboard range. The PostgreSQL set is
//     the InfluxDB SQL translated (see toPG), because the SQL sink writes the
//     same facts as tables.
//   - Prometheus stamps every sample at scrape time, so the exporter reduces
//     the per-item rows to gauges plus, for the counted measurements, a
//     monotonic `_total` of distinct items seen since the process started.
//     `increase()` over that is the Prometheus twin of counting rows per day
//     in SQL, and it is what every "per day" and "over the range" panel uses.
//     `_count` and the `_mean` fields describe the collector's last sweep
//     only, so they feed the "current value" stats and nothing dated.
//   - Graphite keeps the dated points, one series per tag set, as a path. It
//     has no rows: a table here is one number per series reduced over the
//     range, so a table that needs several fields of the same row shows the
//     field it is sorted by and says which it left out.
//   - Elasticsearch keeps the dated documents. Every panel is a Lucene filter
//     plus bucket and metric aggregations; a per-item table is the newest
//     documents themselves.
//   - A panel that has no answer at all in a store is still emitted, as a text
//     panel with the same title saying what it would show and why the store
//     cannot, so every layout stays identical.

// Pull requests and issues gained a `number` tag once it became clear that two
// open ones by the same author were sharing a series. Rows written before that
// have no identity and sit alongside the identified ones as duplicates, so the
// per-item panels ask for the identified ones. On a database that only ever
// saw the current collector this changes nothing.
const (
	identified   = "number IS NOT NULL"
	esIdentified = "_exists_:number"
)

// The series column the InfluxDB dialect names, so a legend reads by series
// rather than by query. The PostgreSQL twin is derived from this one in
// panel(), which rewrites the label name the PostgreSQL datasource uses.
var seriesOpts = Opts{"display": "${__field.labels.series}"}

// The sentences the Prometheus side appends to a description. One each, so a
// panel that is honestly different says why in the same words everywhere.
const (
	sinceStart = "In Prometheus each item is dated by the sweep that first saw it, " +
		"counted from the moment the exporter started, not by its own date."
	lastSweep = "In Prometheus this is the mean over the collector's last sweep, " +
		"not a median over the dashboard range."
	windowNote = "In Prometheus this is GitHub's whole trailing window as one number that " +
		"moves, not days."
	sweepCount = "In Prometheus this counts the collector's last sweep, not the dashboard range."

	// The same for Graphite and Elasticsearch.
	grRows = "Graphite has no rows: each series is one number reduced over the range, so " +
		"this table keeps the column it is sorted by and drops the others."
	grSlot = "In Graphite two facts landing in the same storage slot of one series are " +
		"reduced to one point, so the medians are over what the storage kept."
	grRange    = "Graphite answers only inside the dashboard range, so years outside it are missing."
	grSnapshot = "In Graphite the curve is the repositories' star count as each sweep read it, " +
		"so it starts the day the collector did and sits at GitHub's count."
	grWorst    = "Graphite has no percentile of a bucket, so the upper line is the worst of each bucket."
	grPerRepo  = "Graphite keeps the identity in the path, so this is the count per repository over the range."
	esRange    = "Elasticsearch answers only inside the dashboard range, so years outside it are missing."
	esSnapshot = "In Elasticsearch the curve is the repositories' star count as each sweep read " +
		"it, so it sits at GitHub's count and starts the day the collector did: " + esStackNote
	// How a per-repository snapshot becomes one curve in Elasticsearch: see
	// esSnapshotStack in query.go.
	esStackNote = "one series per repository, its largest reading of the day, stacked so the " +
		"top of the stack is the total."
	esNewest  = "In Elasticsearch this lists the newest 500 documents and the panel sorts them."
	esPerRepo = "In Elasticsearch the rows are per repository as well, because the newest snapshot is taken per repository."

	// What a store says when the InfluxDB table links each row to its page
	// and this one cannot: the exporter carries no url and Graphite keeps
	// no string, so the column is not there to draw. Elasticsearch keeps
	// the url on every document, so a panel of its own that lacks the
	// column is one whose query cannot return it: the three today are joins,
	// the url belongs to another measurement, and an aggregation that did
	// not bucket on it would read the same.
	promNoLink = "The Prometheus exporter carries no url, so the Link column of the " +
		"InfluxDB dashboard is absent here."
	grNoLink = "Graphite keeps no string beside a number, so the Link column of the " +
		"InfluxDB dashboard is absent here."
	esNoLink = "This Elasticsearch query does not return the url the row links from, " +
		"so the Link column of the InfluxDB dashboard is absent here."
	// The same for a panel the store cannot answer at all, appended to the
	// note that stands in for it.
	noteLink = "Each row of it links to the item on GitHub."
)

var (
	alertThresholds = []any{
		map[string]any{"color": "green", "value": nil},
		map[string]any{"color": "orange", "value": 1},
		map[string]any{"color": "red", "value": 10},
	}
	// Green from ninety: at ninety-five a week at 90.9 read orange, and on
	// an account whose runs are mostly its own CI a tenth of them failing
	// is the ordinary week, not a warning.
	rateThresholds = []any{
		map[string]any{"color": "red", "value": nil},
		map[string]any{"color": "orange", "value": 80},
		map[string]any{"color": "green", "value": 90},
	}
)

func pctThresholds(steps []any) Opts {
	out := Opts{"unit": "percentunit", "maxv": 1.0}
	scaled := make([]any, len(steps))
	for i, raw := range steps {
		s, ok := raw.(map[string]any)
		if !ok {
			panic("threshold step " + strconv.Itoa(i) + " is not a color and a value")
		}
		v := s["value"]
		switch n := v.(type) {
		case int:
			v = float64(n) / 100
		case float64:
			v = n / 100
		}
		scaled[i] = map[string]any{"color": s["color"], "value": v}
	}
	out["thresholds"] = scaled
	return out
}

// store is one store's answer for one panel: its queries, the sentence that
// explains how its answer differs, and the transformations and options it
// needs on top of the panel's own.
type store struct {
	Q         []Target
	Desc      string
	Note      string
	TF        []any
	Overrides []any
	Opts      Opts
}

// Logs is the one panel that reads a log store rather than the metrics
// store: the tail of every failed job, which the Loki sink writes and no
// metrics store holds. An exported dashboard is bound to one datasource and
// an importer may have no Loki, so the file keeps a text panel saying where
// the lines went. A dashboard built with a second datasource (Render's
// `logs`, which cmd/publish_dashboard takes from -loki) draws the lines in
// its place, under the same title and at the same grid position, so the
// five-store layout is not a sixth.
type Logs struct {
	// Selector is the stream selector the Loki sink's labels answer to.
	Selector string
	// Desc is the panel's description; the repository sentence is appended
	// per store, see lokiRepoStage.
	Desc string
}

// Panel is one panel, in every store.
type Panel struct {
	Kind       string
	Title      string
	W, H, X, Y int
	Desc       string
	Stores     map[string]*store
	Overrides  []any
	Opts       Opts
	// Logs is set on the text panel a log store can replace; nil elsewhere.
	Logs *Logs
	// PromTitle is the title the Prometheus dashboard shows in place of
	// Title, for the few panels whose Prometheus number is a different
	// quantity under the same name: the traffic tiles read GitHub's whole
	// fourteen day window there and the dashboard range everywhere else,
	// and a reader with both dashboards open saw 2.25K and 1.06K under one
	// word. Empty for every other panel; the layout test holds the two
	// dashboards to the same title unless this is set.
	PromTitle string
}

// P carries the arguments panel() takes beyond the ones every panel needs.
// A store whose query set is nil gets a text panel carrying that store's note,
// which must then be given.
type P struct {
	Desc string
	Logs *Logs

	Prom      []Target
	PromTitle string
	PromDesc  string
	PromNote  string
	PromTF    []any
	PromOver  []any
	PromOpts  Opts

	SQLTF   []any
	SQLOver []any
	SQLOpts Opts

	PG     []Target
	PGDesc string
	PGTF   []any
	PGOver []any
	PGOpts Opts

	GR     []Target
	GRDesc string
	GRNote string
	GRTF   []any
	GROver []any
	GROpts Opts

	ES     []Target
	ESDesc string
	ESNote string
	ESTF   []any
	ESOver []any
	ESOpts Opts

	Overrides []any
	Opts      Opts
}

// panel builds one panel for every store. PostgreSQL defaults to the InfluxDB
// SQL translated, since both hold the same rows.
func panel(kind, title string, at box, sql []Target, p *P) Panel {
	pg, pgOpts := p.PG, p.PGOpts
	if pg == nil && sql != nil {
		pg = pgOf(sql)
	}
	if pgOpts == nil {
		pgOpts = Opts{}
		for k, v := range p.SQLOpts {
			if s, ok := v.(string); ok {
				v = strings.ReplaceAll(s, "labels.series", "labels.metric")
			}
			pgOpts[k] = v
		}
	}
	pgTF := p.PGTF
	if pgTF == nil {
		pgTF = p.SQLTF
	}
	// An override that names a series the SQL spells, "Open that day" or
	// "Merged", belongs to the two SQL stores and to nobody else: Prometheus
	// legends carry the raw label and the match would find nothing there.
	pgOver := p.PGOver
	if pgOver == nil {
		pgOver = p.SQLOver
	}
	stores := map[string]*store{
		"influxdb": {Q: sql, TF: p.SQLTF, Overrides: orEmptyList(p.SQLOver), Opts: orEmpty(p.SQLOpts)},
		"prometheus": {
			Q: p.Prom, Desc: p.PromDesc, Note: p.PromNote, TF: p.PromTF,
			Overrides: orEmptyList(p.PromOver), Opts: orEmpty(p.PromOpts),
		},
		"postgres": {
			Q: pg, Desc: p.PGDesc, TF: pgTF,
			Overrides: orEmptyList(pgOver), Opts: pgOpts,
		},
		"graphite": {
			Q: p.GR, Desc: p.GRDesc, Note: p.GRNote, TF: p.GRTF,
			Overrides: orEmptyList(p.GROver), Opts: orEmpty(p.GROpts),
		},
		"elasticsearch": {
			Q: p.ES, Desc: p.ESDesc, Note: p.ESNote, TF: p.ESTF,
			Overrides: orEmptyList(p.ESOver), Opts: orEmpty(p.ESOpts),
		},
	}
	for name, st := range stores {
		if st.Q == nil && st.Note == "" && kind != "text" {
			panic(title + ": a panel without a " + Stores[name] + " query needs a note")
		}
	}
	if p.Logs != nil && kind != "text" {
		panic(title + ": only a text panel can give way to the log store")
	}
	shared := placeLinks(title, p.Overrides, stores)
	return Panel{
		Kind: kind, Title: title, W: at.W, H: at.H, X: at.X, Y: at.Y, Desc: p.Desc,
		Stores: stores, Overrides: shared, Opts: orEmpty(p.Opts),
		PromTitle: p.PromTitle, Logs: p.Logs,
	}
}

// lokiRepoStage is the LogQL that applies the dashboard's repository
// variable to the log lines, per store, and the sentence the panel carries
// where it cannot. The Loki sink keeps `repo` in the line's logfmt tail
// rather than as a label (a label per repository is a stream per
// repository), so the stage parses the tail and filters on it. Whether the
// variable can be applied is the store's business: the Loki datasource
// joins a multi-value selection into a regular expression, and reads a
// dashboard's "All" as every option unless the variable names its own
// allValue, which Graphite and Elasticsearch do as the glob star. A star is
// not a regular expression, so in those two dashboards the panel shows
// every repository and says so.
func lokiRepoStage(storeName string) (stage, note string) {
	if s, ok := ByName(storeName); ok && s.Variable["allValue"] == "*" {
		return "", " The repository variable is not applied here: in this dashboard " +
			"its All value is the glob star, which is not a regular expression, " +
			"so every repository's lines are shown."
	}
	return ` | repo=~"$repo"`, ""
}

// lokiOneLine is what the panel draws of each line.
//
// The sink's message for a job log is the line itself, and the logfmt tail
// repeats it under `line`, so Grafana drew the message twice with seven
// fields between the copies. Measured at 430 pixels on 2026-09-14: one entry
// was eight wrapped rows and the panel held two of them. This rebuilds the
// line as the repository, the job and the message, which is what identifies
// a line in a panel already filtered to one repository at a time; what it
// drops is `full_name`, which is `owner` and `repo` again, the run and
// workflow ids, and the duplicate. The parsed fields are still each line's
// own, so expanding one shows all of them.
const lokiOneLine = ` | line_format "{{.repo}} {{.job_name}}: {{.line}}"`

// logsPanel is the Loki panel that stands in for a text panel when the
// dashboard is built with a log store.
func logsPanel(id int, p *Panel, storeName string, logs any, y int) map[string]any {
	stage, note := lokiRepoStage(storeName)
	out := base("logs", panelArgs{
		ID: id, Title: p.Title, DS: logs,
		Targets: []any{map[string]any{
			"datasource": logs, "refId": "A", "queryType": "range",
			// logfmt before both: the repository filter reads the tail and so
			// does the line the panel draws.
			"expr": p.Logs.Selector + " | logfmt" + stage + lokiOneLine, "maxLines": 1000,
		}},
		Box: box{W: p.W, H: p.H, X: p.X, Y: y}, Desc: strings.TrimSpace(p.Logs.Desc + note),
	})
	// Newest first, the moment beside each line, and the line wrapped: a
	// job's last lines are read top down from the failure, and a compiler
	// error is longer than the panel is wide.
	out["options"] = map[string]any{
		"showTime": true, "showLabels": false, "showCommonLabels": false,
		"wrapLogMessage": true, "prettifyLogMessage": false, "enableLogDetails": true,
		"dedupStrategy": "none", "sortOrder": "Descending",
	}
	return out
}

// The sentence each store gets when a link column of the shared list is not
// in its own answer.
var noLink = map[string]string{
	"prometheus": promNoLink, "graphite": grNoLink, "elasticsearch": esNoLink,
}

// placeLinks moves every link column override out of the shared list into
// the stores whose query returns that column, and returns what stays shared.
//
// A link override is matched byName against the frame, so on a store whose
// query has no such column it draws nothing, silently. That was the
// arrangement for thirty-two tables, and it hid two things: Prometheus and
// Graphite carried the override in every one of them with three descriptions
// saying the link was gone, and Elasticsearch carried it on twenty-five
// tables whose documents held the url and whose aggregation never returned
// it. Keeping the override only where the column is makes the second case
// visible as a missing bucket, and lets the first say so in the same words
// on every panel. A store answering with a text panel gets the sentence on
// its note instead, so the reader of that panel learns what the row would
// have linked to.
func placeLinks(title string, overrides []any, stores map[string]*store) []any {
	shared := []any{}
	var links []any
	for _, o := range overrides {
		if linkedColumn(o) == "" {
			shared = append(shared, o)
		} else {
			links = append(links, o)
		}
	}
	if len(links) == 0 {
		return shared
	}
	for name, st := range stores {
		var kept []any
		for _, o := range links {
			if st.returns(linkedColumn(o)) {
				kept = append(kept, o)
			}
		}
		st.Overrides = append(kept, st.Overrides...)
		if len(kept) == len(links) {
			continue
		}
		switch {
		case st.Q == nil:
			st.Note = strings.TrimSpace(st.Note + "\n\n" + noteLink)
		case noLink[name] != "":
			st.Desc = strings.TrimSpace(st.Desc + " " + noLink[name])
		default:
			// The SQL stores select the column by name, so a link column
			// their statement does not alias is a mistake in the panel.
			panic(title + ": a link column " + Stores[name] + " does not select")
		}
	}
	return shared
}

// returns reports whether this store's answer has a column of that name:
// a SQL alias, or a field a transformation renames to it. Both are what
// the checkers read back from a frame, so a column is live here exactly
// when they would find it.
func (st *store) returns(column string) bool {
	alias := regexp.MustCompile(`AS\s+"` + regexp.QuoteMeta(column) + `"`)
	for i := range st.Q {
		if alias.MatchString(st.Q[i].SQL) {
			return true
		}
	}
	for _, raw := range st.TF {
		tf, _ := raw.(map[string]any)
		if tf["id"] != "organize" {
			continue
		}
		options, _ := tf["options"].(map[string]any)
		rename, _ := options["renameByName"].(map[string]any)
		for _, to := range rename {
			if to == column {
				return true
			}
		}
	}
	return false
}

func orEmpty(o Opts) Opts {
	if o == nil {
		return Opts{}
	}
	return o
}

func orEmptyList(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

// ── rendering ───────────────────────────────────────────────────────────────

var exprDS = map[string]any{"type": "__expr__", "uid": "__expr__"}

// A panel target of either SQL store carries the statement once, under
// `rawSql`. That is the only key read: the InfluxDB plugin sends a SQL-mode
// target on `rawSql` alone, and posting the same target with `query` and no
// `rawSql` comes back 400, "No SQL statements were provided in the query
// string". So the second copy under `query` was weight in the exported file,
// 38 KB of it, and one more place for the two to disagree. The repository
// variable in stores.go is the reverse case and keeps its `query`; the comment
// there says why.
func target(q *Target, store string, ds any) any {
	switch store {
	case "influxdb":
		return map[string]any{
			"datasource": ds, "format": q.Format, "rawQuery": true,
			"rawSql": q.SQL, "refId": q.Ref,
		}
	case "postgres":
		return map[string]any{
			"datasource": ds, "format": q.Format, "rawSql": q.SQL, "rawQuery": true,
			"editorMode": "code", "refId": q.Ref,
		}
	case "prometheus":
		t := map[string]any{
			"datasource": ds, "expr": q.Expr, "refId": q.Ref, "instant": q.Instant,
			"range": !q.Instant, "editorMode": "code",
		}
		if q.HasLeg {
			t["legendFormat"] = q.Legend
		}
		if q.Fmt != "" {
			t["format"] = q.Fmt
		}
		if q.Step != "" {
			t["interval"] = q.Step
		}
		return t
	case "graphite":
		return map[string]any{
			"datasource": ds, "target": q.GTarget, "refId": q.Ref, "textEditor": true,
		}
	default: // elasticsearch
		if q.Kind == "expr" {
			t := map[string]any{
				"datasource": exprDS, "refId": q.Ref,
				"type": q.ExprType, "expression": q.Expression,
			}
			maps.Copy(t, q.Settings)
			if q.Hide {
				t["hide"] = true
			}
			return t
		}
		t := map[string]any{
			"datasource": ds, "refId": q.Ref, "query": q.Query, "metrics": q.Metrics,
			"bucketAggs": q.Buckets, "timeField": "@timestamp",
		}
		if q.Alias != "" {
			t["alias"] = q.Alias
		}
		if q.Hide {
			t["hide"] = true
		}
		return t
	}
}

// materialize turns one specification entry into one Grafana panel for the
// given store. `logs` is the log store datasource, or nil for a dashboard
// bound to its metrics store alone, which is what every exported file is.
func materialize(id *ids, p *Panel, storeName string, ds, logs any, y0 int) map[string]any {
	y := y0 + p.Y
	if p.Kind == "text" {
		if p.Logs != nil && logs != nil {
			return logsPanel(id.next(), p, storeName, logs, y)
		}
		return textPanel(
			panelArgs{ID: id.next(), Title: p.Title, Box: box{W: p.W, H: p.H, X: p.X, Y: y}},
			optString(p.Opts, "content", ""), p.Opts,
		)
	}
	st := p.Stores[storeName]
	title := p.Title
	if storeName == "prometheus" && p.PromTitle != "" {
		title = p.PromTitle
	}
	if st.Q == nil {
		return textPanel(
			panelArgs{ID: id.next(), Title: title, Box: box{W: p.W, H: p.H, X: p.X, Y: y}},
			st.Note, nil,
		)
	}

	targets := make([]any, len(st.Q))
	for i := range st.Q {
		targets[i] = target(&st.Q[i], storeName, ds)
	}
	desc := strings.TrimSpace(strings.Join(nonEmpty(p.Desc, st.Desc), " "))
	opts := mergeOpts(p.Opts, st.Opts)
	// Every kind takes the shared overrides and the store's own: a stat of
	// several values names them through overrides as much as a table does.
	opts["overrides"] = append(append([]any{}, p.Overrides...), st.Overrides...)

	// Every kind is placed and identified the same way, so the arguments are
	// built once and the switch chooses nothing but the builder.
	a := panelArgs{
		ID: id.next(), Title: title, DS: ds, Targets: targets,
		Box: box{W: p.W, H: p.H, X: p.X, Y: y}, Desc: desc,
	}
	var out map[string]any
	switch p.Kind {
	case "stat":
		out = stat(a, opts)
	case "gauge":
		out = gauge(a, opts)
	case "timeseries":
		out = timeseries(a, opts)
	case "barchart":
		out = barchart(a, opts)
	case "table":
		out = table(a, opts)
	case "piechart":
		out = piechart(a, opts)
	case "status-history":
		out = statusHistory(a, opts)
	case "bargauge":
		out = barGauge(a, opts)
	default:
		panic("unknown panel kind " + p.Kind)
	}
	if len(st.TF) > 0 {
		out["transformations"] = st.TF
	}
	// A panel whose whole subject is one page on GitHub carries the page as
	// a panel link, in the header, since no row of it has a url of its own.
	if links := optList(opts, "links"); links != nil {
		out["links"] = links
	}
	// A panel whose shape only reads at one range keeps that range whatever
	// the dashboard is set to, the way GitHub's calendar is always a year.
	// Grafana overrides the range before the query is sent, so every store's
	// query sees it through its own time filter macro, and the panel header
	// shows the override so the reader knows the rest of the page moved and
	// this did not.
	if v := optString(opts, "time_from", ""); v != "" {
		out["timeFrom"] = v
	}
	return out
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Section is one row of the dashboard and the panels under it.
type Section struct {
	Title     string
	Collapsed bool
	Build     func(b *builder) []Panel
}

// builder is what one walk of the specification carries from panel to panel:
// the Elasticsearch metric and bucket ids handed out so far. Grafana only needs
// them unique inside a query, and renumberES rewrites them in panel order
// anyway, but inside one query the order they were asked for in is the order
// the exported file keeps, so they are still handed out one at a time.
//
// It belongs to one walk. Render, Count and Titles each make their own, so two
// dashboards built at once, of the same store or of two, never number each
// other's aggregations.
type builder struct{ esIDs int }

// nextESID is the next Elasticsearch aggregation id of this walk.
func (b *builder) nextESID() string {
	b.esIDs++
	return strconv.Itoa(b.esIDs)
}

// Render returns the panel list for one store, rows included, laid out top to
// bottom. `logs` is a Loki datasource to draw the job log panel from, or nil
// to keep the text panel the exported files carry.
func Render(storeName string, ds, logs any) []map[string]any {
	id := &ids{}
	b := &builder{}
	var panels []map[string]any
	y := 0
	for _, sec := range Sections {
		specs := sec.Build(b)
		body := make([]map[string]any, len(specs))
		// A section's panels start on the line below its row, and each one
		// sits its own Y further down from there, which is where materialize
		// places it.
		top := y + 1
		height := 0
		for i := range specs {
			p := &specs[i]
			body[i] = materialize(id, p, storeName, ds, logs, top)
			if bottom := top + p.Y + p.H; bottom > height {
				height = bottom
			}
		}
		height -= y
		if sec.Collapsed {
			// A collapsed row carries its panels inside itself. Grafana ignores
			// their gridPos until the row is opened, but keeps it on save.
			panels = append(panels, row(id.next(), sec.Title, y, true, body))
		} else {
			panels = append(panels, row(id.next(), sec.Title, y, false, nil))
			panels = append(panels, body...)
		}
		y += height + 1
	}
	if storeName == "elasticsearch" {
		renumberES(panels)
	}
	return panels
}

// renumberES gives the Elasticsearch metric and bucket ids one sequence in
// panel order. They only have to be unique inside a query, but a spec that
// builds a panel's helpers before its neighbors would otherwise hand out the
// numbers in a different order every time the file is reshuffled; this keeps
// the exported JSON stable against that.
func renumberES(panels []map[string]any) {
	n := 0
	for _, t := range targetsOf(panels) {
		for _, e := range numbered(t) {
			n++
			e["id"] = strconv.Itoa(n)
		}
	}
}

// targetsOf is every target of every panel, in the order Grafana reads them.
// A collapsed row carries its panels inside itself, so those come before the
// row's own targets, of which it has none.
func targetsOf(panels []map[string]any) []map[string]any {
	var out []map[string]any
	for _, p := range panels {
		if inner, ok := p["panels"].([]map[string]any); ok {
			out = append(out, targetsOf(inner)...)
		}
		// A row has no targets of its own, and ranging over none adds nothing.
		targets, _ := p["targets"].([]any)
		for _, raw := range targets {
			if t, ok := raw.(map[string]any); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// numbered is one target's metrics and bucket aggregations, the two lists that
// carry an id, ordered by the id they already have. That order is the one the
// specification handed them out in, so renumbering preserves it rather than
// the order the two lists happen to sit in.
func numbered(t map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"metrics", "bucketAggs"} {
		for _, e := range asList(t[key]) {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return idNumber(out[i]) < idNumber(out[j])
	})
	return out
}

// idNumber is the number an aggregation's id holds. metric(), terms() and dh()
// give every one a string id, so an aggregation without one is a mistake in
// the specification and stops the generator the way agg does. A string that
// is not a number reads as zero and sorts ahead of the numbered ones.
func idNumber(e map[string]any) int {
	id, ok := e["id"].(string)
	if !ok {
		panic(fmt.Sprintf("a %v aggregation has no string id", e["type"]))
	}
	n, _ := strconv.Atoi(id)
	return n
}

// asList accepts either of the two shapes a metric or bucket list is held in.
func asList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	default:
		return nil
	}
}

// Dashboard is the whole document, in Grafana's shareable export format.
// `logs` is the log store datasource or nil, as for Render. Everything the
// document says about itself already differs per store and no further, so it
// is read off the store rather than handed over field by field; `variable` is
// the one part that cannot be, since it is the store's own repository
// variable with the datasource of this build filled in.
func Dashboard(s *Store, ds, logs any, variable map[string]any) map[string]any {
	return map[string]any{
		"__inputs":    s.Inputs,
		"__requires":  s.Requires,
		"uid":         s.UID,
		"title":       s.Title,
		"description": s.Description,
		"tags":        []any{"github", "ghchronicle"},
		// UTC, because every bucket in every query is: a point is stamped at
		// the moment GitHub says the thing happened, a day bin cuts at
		// midnight UTC and date_part reads the weekday there. Read in the
		// browser's zone the labels named one bucket and drew another, and
		// the calendar's tooltip said 02:00 over a row stamped at midnight.
		"timezone":      "utc",
		"editable":      true,
		"graphTooltip":  1,
		"schemaVersion": 39,
		"refresh":       "5m",
		"time":          map[string]any{"from": "now-30d", "to": "now"},
		"templating":    map[string]any{"list": []any{variable}},
		"panels":        Render(s.Name, ds, logs),
	}
}

// PromRetitled is, by position in the flattened layout rows included, every
// panel that carries a title of its own on the Prometheus dashboard. The
// generator and the layout test both hold the five dashboards to one title
// per position, and this is the one allowance: the position and size still
// have to match there.
func PromRetitled() map[int]bool {
	out := map[int]bool{}
	i := 0
	b := &builder{}
	for _, sec := range Sections {
		i++ // the row
		for _, p := range sec.Build(b) {
			if p.PromTitle != "" {
				out[i] = true
			}
			i++
		}
	}
	return out
}

// Count is the number of panels per dashboard, rows excluded. Identical for
// every store by construction; exposed so the README's number is not typed by
// hand.
func Count() int {
	n := 0
	b := &builder{}
	for _, sec := range Sections {
		n += len(sec.Build(b))
	}
	return n
}
