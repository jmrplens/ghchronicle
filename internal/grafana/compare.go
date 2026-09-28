package grafana

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Picture is what one panel puts in front of a reader, reduced to what two
// stores can be held to: the named values of a stat, a gauge, a bar gauge or
// a pie chart, or the visible columns of a table or a bar chart.
type Picture struct {
	Kind string
	// Tiles is every value a stat-like panel draws, under its name.
	Tiles []Reading
	// Columns is every column of the table the panel draws, the hidden ones
	// left out. A table of several frames draws the first and offers the
	// rest in a picker, so the first is the one compared.
	Columns []*Field
}

// Drawing replays a panel over its answer and reduces it to a Picture. It
// answers false for a panel whose values are not comparable between stores:
// a time series, and a bar chart whose axis is time, both of which bucket by
// the store's own step rather than by anything the panel says.
func Drawing(panel, answer map[string]any) (Picture, bool, error) {
	kind, _ := panel["type"].(string)
	switch kind {
	case "stat", "gauge", "bargauge", "piechart", "table", "barchart":
	default:
		return Picture{}, false, nil
	}
	frames, err := Draw(panel, answer)
	if err != nil {
		return Picture{}, false, err
	}
	pic := Picture{Kind: kind}
	if kind == "table" || kind == "barchart" {
		if len(frames) == 0 {
			return pic, true, nil
		}
		for _, f := range frames[0].Fields {
			if !f.Hidden() {
				pic.Columns = append(pic.Columns, f)
			}
		}
		if kind == "barchart" {
			return barPicture(panel, pic)
		}
		return pic, true, nil
	}
	if pic.Tiles, err = Readings(panel, frames); err != nil {
		return Picture{}, false, err
	}
	return pic, true, nil
}

// BarAxis is what a bar chart's axis is called in its picture. The axis has
// no heading, so what each store names the field it comes from is nothing a
// reader sees, and the bars' names are compared under this one.
const BarAxis = "(the bars)"

// barPicture is what a bar chart draws: a bar per value of its axis, the
// field its options name or else the first string field, and a length per
// number. A string field that is not the axis is not drawn at all, which is
// how Elasticsearch's "Downloads by release" named both of its bars
// hello-world, its repository column standing ahead of the tag. A chart
// whose axis is a time buckets by the store's own step and is not compared.
func barPicture(panel map[string]any, pic Picture) (Picture, bool, error) {
	options, _ := panel["options"].(map[string]any)
	name, _ := options["xField"].(string)
	var axis *Field
	for _, f := range pic.Columns {
		if (name != "" && f.Display == name) || (name == "" && axis == nil && f.Type == "string") {
			axis = f
		}
	}
	if axis == nil {
		if !slices.ContainsFunc(pic.Columns, func(f *Field) bool { return f.Type == "time" }) {
			// What Prometheus's "Languages starred" drew once the exporter
			// had dropped the label it summed by.
			return Picture{}, false, errors.New(`no string or time field for the bars' axis, ` +
				`where Grafana draws "Bar charts require a string or time field"`)
		}
		return Picture{}, false, nil
	}
	bars := *axis
	bars.Display = BarAxis
	out := Picture{Kind: pic.Kind, Columns: []*Field{&bars}}
	for _, f := range pic.Columns {
		if f != axis && f.Type == "number" {
			out.Columns = append(out.Columns, f)
		}
	}
	return out, true, nil
}

// Empty reports whether the picture shows nothing at all, which is the
// business of whoever asks whether a panel answered, not of a comparison.
func (p *Picture) Empty() bool {
	if p.Kind == "table" || p.Kind == "barchart" {
		for _, f := range p.Columns {
			if len(f.Values) > 0 {
				return false
			}
		}
		return true
	}
	return len(p.Tiles) == 0
}

// describe says what a picture draws, for a report.
func describe(p *Picture) string {
	if p.Kind == "table" || p.Kind == "barchart" {
		n := 0
		for _, f := range p.Columns {
			n = max(n, len(f.Values))
		}
		return fmt.Sprintf("%d rows of %v", n, p.Names())
	}
	return fmt.Sprintf("the tiles %v", p.Names())
}

// Without is the picture with the named tiles or columns left out.
func (p *Picture) Without(names []string) Picture {
	out := Picture{Kind: p.Kind}
	for _, t := range p.Tiles {
		if !slices.Contains(names, t.Name) {
			out.Tiles = append(out.Tiles, t)
		}
	}
	for _, f := range p.Columns {
		if !slices.Contains(names, f.Display) {
			out.Columns = append(out.Columns, f)
		}
	}
	return out
}

// DrawnNames is every name a panel puts in front of a reader: the values of a
// stat, a gauge, a bar gauge or a pie, the headings of a table, and the
// series of a chart, which its legend and its tooltip name.
func DrawnNames(panel map[string]any, frames []*Frame) ([]string, error) {
	switch panel["type"] {
	case "stat", "gauge", "bargauge", "piechart":
		readings, err := Readings(panel, frames)
		if err != nil || len(readings) < 2 {
			return nil, err // a single value is drawn without its name
		}
		out := make([]string, len(readings))
		for i, r := range readings {
			out[i] = r.Name
		}
		return out, nil
	}
	var out []string
	for _, frame := range frames {
		for _, f := range frame.Fields {
			switch {
			case f.Hidden() || f.Type == "time":
			case panel["type"] == "table", f.Type == "number":
				out = append(out, f.Display)
			}
		}
	}
	return out, nil
}

// Names are the names a reader sees, one per tile or column.
func (p *Picture) Names() []string {
	var out []string
	for _, t := range p.Tiles {
		out = append(out, t.Name)
	}
	for _, f := range p.Columns {
		out = append(out, f.Display)
	}
	return out
}

// Likeness is what two stores' drawings of one panel may differ by and still
// show a reader the same thing.
type Likeness struct {
	// Equal holds two texts that are not identical to be the same. Every
	// store draws a name as it holds it, and Graphite holds one as a path
	// node, so the caller decides what counts as the same name there.
	Equal func(x, y string) bool
	// Slack is how far apart two numbers under a column's or a tile's name
	// may be beyond the rounding every number is allowed.
	Slack func(name string) float64
}

func (l Likeness) equal(x, y string) bool { return l.Equal != nil && l.Equal(x, y) }

func (l Likeness) slack(name string) float64 {
	if l.Slack == nil {
		return 0
	}
	return l.Slack(name)
}

// Compare says how two stores draw one panel differently, a sentence per
// difference, and nothing when they draw the same thing.
//
// A tile is compared by its name: its value, its unit, and the text it shows
// when it has none; a tile one store draws and the other does not is a
// difference. A table is compared on the columns both stores draw, as a set
// of rows, which is what survives the stores ordering and bucketing rows
// their own way; a column only one of them draws is not, because what a store
// can hold at all is its own description's business, and no column in common
// is a difference.
func Compare(a, b *Picture, like Likeness) []string {
	if a.Kind != b.Kind {
		return []string{fmt.Sprintf("drawn as a %s against a %s", a.Kind, b.Kind)}
	}
	// Two panels that draw nothing draw the same thing, whatever columns
	// each would have had; one that draws nothing where the other draws
	// something is the difference, and naming the columns says nothing more.
	switch aEmpty, bEmpty := a.Empty(), b.Empty(); {
	case aEmpty && bEmpty:
		return nil
	case aEmpty:
		return []string{"the first draws nothing and the second draws " + describe(b)}
	case bEmpty:
		return []string{"the second draws nothing and the first draws " + describe(a)}
	}
	if a.Kind == "table" || a.Kind == "barchart" {
		return compareRows(a, b, like)
	}
	return compareTiles(a, b, like)
}

func compareTiles(a, b *Picture, like Likeness) []string {
	// A panel of one value draws it without a name, so what each store
	// happens to call that one field is nothing a reader sees.
	if len(a.Tiles) == 1 && len(b.Tiles) == 1 {
		one := *b
		one.Tiles = []Reading{b.Tiles[0]}
		one.Tiles[0].Name = a.Tiles[0].Name
		b = &one
	}
	var out []string
	byName := func(p *Picture) (map[string]Reading, []string) {
		m := map[string]Reading{}
		var order []string
		for _, t := range p.Tiles {
			if _, dup := m[t.Name]; !dup {
				order = append(order, t.Name)
			}
			m[t.Name] = t
		}
		return m, order
	}
	am, aOrder := byName(a)
	bm, bOrder := byName(b)
	for _, name := range aOrder {
		ta := am[name]
		tb, found := bm[name]
		if !found {
			out = append(out, fmt.Sprintf("tile %q is drawn by the first and not the second", name))
			continue
		}
		ca, cb := tileCell(&ta), tileCell(&tb)
		if !ca.same(cb, like, name) {
			out = append(out, fmt.Sprintf("tile %q reads %s against %s", name, ca, cb))
		}
		if ua, ub := drawnUnit(ta.Field), drawnUnit(tb.Field); ua != ub && (ca.kind == cellNumber || cb.kind == cellNumber) {
			out = append(out, fmt.Sprintf("tile %q is in %s against %s", name, unitName(ua), unitName(ub)))
		}
	}
	for _, name := range bOrder {
		if _, found := am[name]; !found {
			out = append(out, fmt.Sprintf("tile %q is drawn by the second and not the first", name))
		}
	}
	return out
}

// drawnUnit is the unit as the reader sees it. A share is drawn the same
// whether its value is a fraction under percentunit or a hundredth under
// percent, which is how the Elasticsearch mix is written, so the two are one
// unit here and valueCell scales the fraction.
func drawnUnit(f *Field) string {
	if u := f.Unit(); u != "percentunit" {
		return u
	}
	return "percent"
}

func unitName(u string) string {
	if u == "" {
		return "no unit"
	}
	return strconv.Quote(u)
}

// tileCell is the value a tile shows: its mapped text, its number, or the
// text it shows for nothing.
func tileCell(t *Reading) cell {
	if t.Value == nil {
		if text, mapped := t.Field.Mapped(nil); mapped {
			return cell{kind: cellText, text: text}
		}
		return cell{kind: cellText, text: "(" + t.Field.NoValue() + ")"}
	}
	return valueCell(t.Field, t.Value)
}

func compareRows(a, b *Picture, like Likeness) []string {
	var common []string
	aCols, bCols := columnsByName(a.Columns), columnsByName(b.Columns)
	for _, f := range a.Columns {
		if bCols[f.Display] != nil && !slices.Contains(common, f.Display) {
			common = append(common, f.Display)
		}
	}
	if len(common) == 0 {
		return []string{fmt.Sprintf("no column in common: %v against %v", a.Names(), b.Names())}
	}
	var out []string
	for _, name := range common {
		if ua, ub := drawnUnit(aCols[name]), drawnUnit(bCols[name]); ua != ub {
			out = append(out, fmt.Sprintf("column %q is in %s against %s", name, unitName(ua), unitName(ub)))
		}
	}
	aRows, bRows := rowsOf(aCols, common), rowsOf(bCols, common)
	matched := make([]bool, len(bRows))
	var onlyA []string
	for _, ra := range aRows {
		found := false
		for j, rb := range bRows {
			if !matched[j] && sameRow(ra, rb, common, like) {
				matched[j], found = true, true
				break
			}
		}
		if !found {
			onlyA = append(onlyA, rowString(ra))
		}
	}
	var onlyB []string
	for j, rb := range bRows {
		if !matched[j] {
			onlyB = append(onlyB, rowString(rb))
		}
	}
	if len(onlyA) > 0 || len(onlyB) > 0 {
		out = append(out, fmt.Sprintf("the rows over %v differ: %d only in the first %s, %d only in the second %s",
			common, len(onlyA), sample(onlyA), len(onlyB), sample(onlyB)))
	}
	return out
}

func columnsByName(columns []*Field) map[string]*Field {
	out := map[string]*Field{}
	for _, f := range columns {
		if out[f.Display] == nil {
			out[f.Display] = f
		}
	}
	return out
}

func rowsOf(columns map[string]*Field, names []string) [][]cell {
	n := 0
	for _, name := range names {
		n = max(n, len(columns[name].Values))
	}
	rows := make([][]cell, n)
	for r := range rows {
		for _, name := range names {
			f := columns[name]
			rows[r] = append(rows[r], valueCell(f, valueAt(f, r)))
		}
	}
	return rows
}

func sameRow(a, b []cell, names []string, like Likeness) bool {
	for i := range a {
		if !a[i].same(b[i], like, names[i]) {
			return false
		}
	}
	return true
}

func rowString(r []cell) string {
	parts := make([]string, len(r))
	for i, c := range r {
		parts[i] = c.String()
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

func sample(rows []string) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows) > 4 {
		return strings.Join(rows[:4], ", ") + ", ..."
	}
	return strings.Join(rows, ", ")
}

// A cell is one value as the reader sees it: a number, an instant, a text or
// nothing.
type cellKind int

const (
	cellEmpty cellKind = iota
	cellNumber
	cellTime
	cellText
)

type cell struct {
	kind cellKind
	num  float64
	text string
}

// isTimeUnit reports whether a unit draws a number as a date: one of
// Grafana's dateTime units, or a format of its own written "time: <format>",
// which is how Elasticsearch, whose dates come back as epoch milliseconds, is
// told to draw what the SQL stores return as a time.
func isTimeUnit(u string) bool {
	return strings.HasPrefix(u, "dateTime") || strings.HasPrefix(u, "time:")
}

// valueCell is what a field draws for one of its values: the text a value
// mapping gives it, an instant for a time, the number, or the text.
func valueCell(f *Field, v any) cell {
	if text, mapped := f.Mapped(v); mapped {
		return cell{kind: cellText, text: text}
	}
	switch t := v.(type) {
	case nil:
		return cell{kind: cellEmpty}
	case float64:
		if f.Type == "time" || isTimeUnit(f.Unit()) {
			return cell{kind: cellTime, num: t}
		}
		if f.Unit() == "percentunit" {
			t *= 100 // drawn as the percent a percent unit draws
		}
		return cell{kind: cellNumber, num: t}
	case bool:
		return cell{kind: cellText, text: strconv.FormatBool(t)}
	case string:
		if t == "" {
			return cell{kind: cellEmpty}
		}
		if f.Type == "time" {
			if at, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return cell{kind: cellTime, num: float64(at.UnixMilli())}
			}
		}
		return cell{kind: cellText, text: t}
	}
	return cell{kind: cellText, text: jsString(v)}
}

// same is equality as a reader would judge it. A number is the same to a
// millionth of itself, because Elasticsearch keeps a float as a float32 and
// hands the same 11.712 back as 11.712000012397766; an instant to the second,
// the finest any of these tables draws; a number held as text in one store and
// as a number in another, as a label value against a column, is the number.
func (c cell) same(o cell, like Likeness, name string) bool {
	switch {
	case c.kind == cellNumber && o.kind == cellNumber:
		return sameNumber(c.num, o.num, 1e-6*max(math.Abs(c.num), 1)+like.slack(name))
	case c.kind == cellTime && o.kind == cellTime:
		return sameNumber(c.num, o.num, 1000)
	case c.kind == cellText && o.kind == cellText:
		return c.text == o.text || like.equal(c.text, o.text)
	case c.kind == cellNumber && o.kind == cellText:
		n, err := strconv.ParseFloat(o.text, 64)
		return err == nil && sameNumber(c.num, n, 1e-6*max(math.Abs(n), 1)+like.slack(name))
	case c.kind == cellText && o.kind == cellNumber:
		return o.same(c, like, name)
	}
	return c.kind == o.kind
}

func sameNumber(a, b, tolerance float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) <= tolerance
}

func (c cell) String() string {
	switch c.kind {
	case cellEmpty:
		return "(empty)"
	case cellNumber:
		return strconv.FormatFloat(c.num, 'g', 8, 64)
	case cellTime:
		return time.UnixMilli(int64(c.num)).UTC().Format(time.RFC3339)
	}
	return strconv.Quote(c.text)
}
