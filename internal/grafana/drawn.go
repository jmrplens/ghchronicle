package grafana

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// What a panel draws, as distinct from what its queries answer.
//
// /api/ds/query hands back the datasource's frames, and a panel is not those
// frames. The Prometheus datasource reshapes a table-format answer in the
// browser into label columns and a "Value #A"; the panel's transformations
// rename, merge, reduce, group and filter what is left; and the field
// configuration gives each field the name, the unit and the words a reader
// sees. Two stores compared at the frames compare "p50.0 seconds_to_merge"
// with "Time to merge", cannot tell which of the two lost its unit, and never
// see the seven rows a merge that cannot join draws for one repository: the
// 2.6.1 review found all three by looking at the screen.
//
// So Draw replays that pipeline over an answer, for the steps the committed
// dashboards use and with Grafana's own semantics, read out of the 13.2
// frontend: a byName override matches the name the field carries when the
// override is reached, which a displayName earlier in the list changes; a
// reducer skips nulls; a merge joins rows on the fields every frame has by
// the same name. A step it does not know is an error rather than a guess, so
// a dashboard that starts using one fails the comparison until it is taught
// here.

// Frame is one frame of a panel's data between the query and the panel.
type Frame struct {
	RefID  string
	Name   string
	Fields []*Field
	// resultType is the Prometheus result type of the frame, which is how
	// the datasource's own reshaping recognizes one of its answers.
	resultType string
}

// Field is one field of a frame.
type Field struct {
	// Name is the name the frame carries; Display is the name the reader
	// sees once the panel's overrides have run, which Draw sets.
	Name    string
	Display string
	Type    string
	// Labels is nil when the field has none, which Grafana tells apart from
	// an empty set when it names the field.
	Labels map[string]string
	Config map[string]any
	Values []any
}

// Unit is the unit the field's values are formatted in. Grafana formats a
// field with no unit and one whose unit is "none" alike, so both are "".
func (f *Field) Unit() string {
	u, _ := f.Config["unit"].(string)
	if u == "none" {
		return ""
	}
	return u
}

// Hidden reports whether a table leaves the field out.
func (f *Field) Hidden() bool {
	custom, _ := f.Config["custom"].(map[string]any)
	hidden, _ := custom["hidden"].(bool)
	return hidden
}

// NoValue is what the panel draws for a value that is not there.
func (f *Field) NoValue() string {
	s, _ := f.Config["noValue"].(string)
	return s
}

// Mapped is the text one of the field's value mappings gives v, and whether
// any of them matched. A mapping that only colors the value leaves the text
// alone, so it is not a match here.
func (f *Field) Mapped(v any) (string, bool) {
	for _, raw := range list(f.Config["mappings"]) {
		m, _ := raw.(map[string]any)
		options, _ := m["options"].(map[string]any)
		var result map[string]any
		switch m["type"] {
		case "value":
			if v != nil {
				result, _ = options[jsString(v)].(map[string]any)
			}
		case "regex":
			s, isString := v.(string)
			pattern, _ := options["pattern"].(string)
			re := jsRegex(pattern)
			if !isString || re == nil {
				continue
			}
			result, _ = options["result"].(map[string]any)
			text, _ := result["text"].(string)
			if at := re.FindStringSubmatchIndex(s); at != nil && text != "" {
				// String.replace without the g flag: the first match only.
				return s[:at[0]] + string(re.ExpandString(nil, text, s, at)) + s[at[1]:], true
			}
			continue
		case "special":
			if specialMatch(options["match"], v) {
				result, _ = options["result"].(map[string]any)
			}
		}
		if text, _ := result["text"].(string); text != "" {
			return text, true
		}
	}
	return "", false
}

// jsRegex is Grafana's stringToJsRegex: /body/flags is a regular expression,
// and anything else must match the whole value. Nil when it does not compile.
func jsRegex(pattern string) *regexp.Regexp {
	expr := "^" + pattern + "$"
	if strings.HasPrefix(pattern, "/") {
		m := regexp.MustCompile(`^/(.*?)/([gimys]*)$`).FindStringSubmatch(pattern)
		if m == nil {
			return nil
		}
		expr = m[1]
		if strings.Contains(m[2], "i") {
			expr = "(?i)" + expr
		}
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil
	}
	return re
}

func specialMatch(match, v any) bool {
	n, isNumber := v.(float64)
	switch match {
	case "null":
		return v == nil
	case "nan":
		return isNumber && math.IsNaN(n)
	case "null+nan":
		return v == nil || (isNumber && math.IsNaN(n))
	case "true":
		return v == true || v == "true"
	case "false":
		return v == false || v == "false"
	case "empty":
		return v == ""
	}
	return false
}

// Draw replays what Grafana does between the answer and the panel: the
// datasource's reshaping, the panel's transformations and its field
// configuration. Every field of the frames it returns carries its display
// name and its final configuration.
func Draw(panel, answer map[string]any) ([]*Frame, error) {
	frames, err := answerFrames(panel, answer)
	if err != nil {
		return nil, err
	}
	frames, err = transform(prometheusTables(panel, frames), list(panel["transformations"]))
	if err != nil {
		return nil, err
	}
	fc, _ := panel["fieldConfig"].(map[string]any)
	return applyFieldConfig(frames, fc)
}

// answerFrames decodes the frames of the answer in the order of the panel's
// targets, which is the order Grafana hands them to the panel. A hidden
// target is only an input to an expression and draws nothing.
func answerFrames(panel, answer map[string]any) ([]*Frame, error) {
	results, _ := answer["results"].(map[string]any)
	var out []*Frame
	seen := map[string]bool{}
	for _, raw := range list(panel["targets"]) {
		target, _ := raw.(map[string]any)
		ref, _ := target["refId"].(string)
		if hidden, _ := target["hide"].(bool); hidden || seen[ref] {
			continue
		}
		seen[ref] = true
		result, _ := results[ref].(map[string]any)
		for _, f := range list(result["frames"]) {
			frame, err := decodeFrame(f)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref, err)
			}
			if frame.RefID == "" {
				frame.RefID = ref
			}
			out = append(out, frame)
		}
	}
	return out, nil
}

// decodeFrame reads one frame of the wire format, with the entities that
// carry the floats JSON cannot spell.
func decodeFrame(raw any) (*Frame, error) {
	frame, _ := raw.(map[string]any)
	schema, _ := frame["schema"].(map[string]any)
	data, _ := frame["data"].(map[string]any)
	out := &Frame{}
	out.RefID, _ = schema["refId"].(string)
	out.Name, _ = schema["name"].(string)
	meta, _ := schema["meta"].(map[string]any)
	custom, _ := meta["custom"].(map[string]any)
	out.resultType, _ = custom["resultType"].(string)
	columns := list(data["values"])
	entities := list(data["entities"])
	for i, rawField := range list(schema["fields"]) {
		field, _ := rawField.(map[string]any)
		f := &Field{Config: map[string]any{}}
		f.Name, _ = field["name"].(string)
		f.Type, _ = field["type"].(string)
		if config, isMap := field["config"].(map[string]any); isMap {
			f.Config = deepCopy(config)
		}
		if labels, has := field["labels"].(map[string]any); has {
			f.Labels = map[string]string{}
			for k, v := range labels {
				f.Labels[k] = jsString(v)
			}
		}
		if i < len(columns) {
			f.Values = slices.Clone(list(columns[i]))
		}
		if i < len(entities) {
			if err := applyEntities(f.Values, entities[i]); err != nil {
				return nil, err
			}
		}
		out.Fields = append(out.Fields, f)
	}
	return out, nil
}

func applyEntities(values []any, raw any) error {
	entity, _ := raw.(map[string]any)
	for key, special := range map[string]float64{
		"NaN": math.NaN(), "Inf": math.Inf(1), "NegInf": math.Inf(-1),
	} {
		for _, at := range list(entity[key]) {
			i, isNumber := at.(float64)
			if !isNumber || int(i) < 0 || int(i) >= len(values) {
				return fmt.Errorf("an entity %s at %v, outside the field", key, at)
			}
			values[int(i)] = special
		}
	}
	return nil
}

// prometheusTables is what the Prometheus datasource does in the browser to
// every frame of a target whose format is table (transformDFToTable): one
// frame per query, a Time column, a column per label and a value column,
// named "Value" when one query answered and "Value #<refId>" when several
// did. The label columns keep __name__ when the query kept it, which is how a
// merge of seven plain selectors drew seven rows for one repository.
func prometheusTables(panel map[string]any, frames []*Frame) []*Frame {
	tableRefs := map[string]bool{}
	for _, raw := range list(panel["targets"]) {
		target, _ := raw.(map[string]any)
		if ref, _ := target["refId"].(string); target["format"] == "table" {
			tableRefs[ref] = true
		}
	}
	var tables, rest []*Frame
	for _, f := range frames {
		if f.resultType != "" && tableRefs[f.RefID] {
			tables = append(tables, f)
		} else {
			rest = append(rest, f)
		}
	}
	if len(tables) == 0 || (len(tables) == 1 && frameLength(tables[0]) == 0) {
		return frames
	}
	var refs []string
	byRef := map[string][]*Frame{}
	for _, f := range tables {
		if byRef[f.RefID] == nil {
			refs = append(refs, f.RefID)
		}
		byRef[f.RefID] = append(byRef[f.RefID], f)
	}
	for _, ref := range refs {
		valueName := "Value"
		if len(refs) > 1 {
			valueName = "Value #" + ref
		}
		rest = append(rest, promTable(ref, valueName, byRef[ref]))
	}
	return rest
}

// promTable is one query's series as one table: a row per series, its time,
// a column per label any of them carries, and its value.
func promTable(ref, valueName string, series []*Frame) *Frame {
	var labelNames []string
	for _, f := range series {
		for _, k := range slices.Sorted(maps.Keys(seriesLabels(f))) {
			if !slices.Contains(labelNames, k) {
				labelNames = append(labelNames, k)
			}
		}
	}
	timeField := &Field{Name: "Time", Type: "time", Config: map[string]any{}}
	valueField := &Field{Name: valueName, Type: "number", Config: map[string]any{}}
	labelFields := make([]*Field, len(labelNames))
	for i, name := range labelNames {
		labelFields[i] = &Field{Name: name, Type: "string", Config: map[string]any{"filterable": true}}
	}
	for _, f := range series {
		if len(f.Fields) > 0 {
			timeField.Values = append(timeField.Values, f.Fields[0].Values...)
		}
		if len(f.Fields) < 2 {
			continue
		}
		labels := seriesLabels(f)
		for _, v := range f.Fields[1].Values {
			valueField.Values = append(valueField.Values, v)
			for _, lf := range labelFields {
				lf.Values = append(lf.Values, labels[lf.Name])
			}
		}
	}
	fields := append([]*Field{timeField}, labelFields...)
	return &Frame{RefID: ref, Fields: append(fields, valueField)}
}

func seriesLabels(f *Frame) map[string]string {
	if len(f.Fields) < 2 {
		return nil
	}
	return f.Fields[1].Labels
}

func frameLength(f *Frame) int {
	n := 0
	for _, field := range f.Fields {
		n = max(n, len(field.Values))
	}
	return n
}

// ── Transformations ─────────────────────────────────────────────────────────

func transform(frames []*Frame, tfs []any) ([]*Frame, error) {
	for _, raw := range tfs {
		tf, _ := raw.(map[string]any)
		if disabled, _ := tf["disabled"].(bool); disabled {
			continue
		}
		if tf["filter"] != nil {
			return nil, fmt.Errorf("a %v limited to some frames, which Draw does not replay", tf["id"])
		}
		options, _ := tf["options"].(map[string]any)
		var err error
		switch tf["id"] {
		case "organize":
			frames = organize(frames, options)
		case "merge":
			frames = merge(frames)
		case "reduce":
			frames, err = reduceToRows(frames, options)
		case "calculateField":
			frames, err = calculateField(frames, options)
		case "groupBy":
			frames, err = groupBy(frames, options)
		case "filterByValue":
			frames, err = filterByValue(frames, options)
		case "sortBy":
			frames = sortBy(frames, options)
		case "limit":
			frames = limitRows(frames, options)
		case "transpose":
			frames = transpose(frames, options)
		case "rowsToFields":
			frames = rowsToFields(frames, options)
		default:
			err = fmt.Errorf("the %v transformation, which Draw does not replay", tf["id"])
		}
		if err != nil {
			return nil, err
		}
	}
	return frames, nil
}

// names is the keys of an options map whose value is true, or a string.
func names(options map[string]any, key string) map[string]bool {
	m, _ := options[key].(map[string]any)
	out := map[string]bool{}
	for k, v := range m {
		if b, isBool := v.(bool); !isBool || b {
			out[k] = true
		}
	}
	return out
}

// organize filters, orders, then renames, each by the name a field is
// displayed under. The order is not what a table is compared by, but it is
// which field a bar chart puts on its axis.
func organize(frames []*Frame, options map[string]any) []*Frame {
	include, exclude := names(options, "includeByName"), names(options, "excludeByName")
	rename, _ := options["renameByName"].(map[string]any)
	// A frame the filter leaves without a field is dropped, and only when
	// there is a filter: an organize that only renames keeps every frame.
	if len(include) > 0 || len(exclude) > 0 {
		var kept []*Frame
		for _, frame := range frames {
			var fields []*Field
			for _, f := range frame.Fields {
				name := displayName(f, frame, frames)
				if (len(include) > 0 && !include[name]) || exclude[name] {
					continue
				}
				fields = append(fields, f)
			}
			if len(fields) > 0 {
				kept = append(kept, &Frame{RefID: frame.RefID, Name: frame.Name, Fields: fields, resultType: frame.resultType})
			}
		}
		frames = kept
	}
	if order, _ := options["indexByName"].(map[string]any); len(order) > 0 {
		for _, frame := range frames {
			slices.SortStableFunc(frame.Fields, func(a, b *Field) int {
				return cmp.Compare(fieldIndex(order, displayName(a, frame, frames)),
					fieldIndex(order, displayName(b, frame, frames)))
			})
		}
	}
	for _, frame := range frames {
		for _, f := range frame.Fields {
			if to, _ := rename[displayName(f, frame, frames)].(string); to != "" {
				f.Config["displayName"] = to
			}
		}
	}
	return frames
}

// fieldIndex is where indexByName puts a field, and after every field it
// names when it names this one with no whole number.
func fieldIndex(order map[string]any, name string) int {
	if n, isNumber := order[name].(float64); isNumber && n == math.Trunc(n) {
		return int(n)
	}
	if n, isInt := order[name].(int); isInt {
		return n
	}
	return math.MaxInt
}

// merge is Grafana's merge: the rows of every frame in one, joined on the
// fields every frame carries by the same name, a row absorbing another only
// where no field they share holds two different values.
func merge(frames []*Frame) []*Frame {
	if len(frames) <= 1 {
		return frames
	}
	var nonEmpty []*Frame
	for _, f := range frames {
		if len(f.Fields) > 0 {
			nonEmpty = append(nonEmpty, f)
		}
	}
	if len(nonEmpty) == 0 {
		return frames[:1]
	}
	keys := mergeKeys(nonEmpty)
	if len(keys) == 0 {
		return frames
	}
	var order []string
	proto := map[string]*Field{}
	for _, frame := range nonEmpty {
		for _, f := range frame.Fields {
			if proto[f.Name] == nil {
				order = append(order, f.Name)
				config := deepCopy(f.Config)
				delete(config, "displayName")
				proto[f.Name] = &Field{Name: f.Name, Type: f.Type, Config: config}
			}
		}
	}
	out := &Frame{RefID: "merge"}
	rows := mergeRows(nonEmpty, keys)
	for _, name := range order {
		f := proto[name]
		for _, row := range rows {
			f.Values = append(f.Values, row[name])
		}
		out.Fields = append(out.Fields, f)
	}
	return []*Frame{out}
}

// mergeKeys is the fields a merge joins rows on: those every frame carries
// by the same name, in the order the last frame gives them.
func mergeKeys(frames []*Frame) []string {
	var keys []string
	for _, f := range frames[len(frames)-1].Fields {
		inAll := true
		for _, frame := range frames {
			if fieldByName(frame, f.Name) == nil {
				inAll = false
			}
		}
		if inAll && !slices.Contains(keys, f.Name) {
			keys = append(keys, f.Name)
		}
	}
	return keys
}

// mergeRows is every row of the frames, a row absorbed into an earlier one of
// the same key wherever no field they share holds two different values, in
// the order the rows were first seen.
func mergeRows(frames []*Frame, keys []string) []map[string]any {
	type slot struct {
		key   string
		index int
	}
	byKey := map[string][]map[string]any{}
	var slots []slot
	for _, frame := range frames {
		for r := range frameLength(frame) {
			var key strings.Builder
			for _, k := range keys {
				key.WriteString(jsString(valueAt(fieldByName(frame, k), r)))
			}
			row := map[string]any{}
			for _, f := range frame.Fields {
				row[f.Name] = valueAt(f, r)
			}
			existing := byKey[key.String()]
			merged := false
			for _, e := range existing {
				if mergeable(e, row) {
					merged = true
					maps.Copy(e, row)
				}
			}
			if !merged {
				byKey[key.String()] = append(existing, row)
				slots = append(slots, slot{key.String(), len(existing)})
			}
		}
	}
	rows := make([]map[string]any, len(slots))
	for i, s := range slots {
		rows[i] = byKey[s.key][s.index]
	}
	return rows
}

func mergeable(existing, row map[string]any) bool {
	for k, v := range row {
		e, has := existing[k]
		if has && e != nil && !sameValue(e, v) {
			return false
		}
	}
	return true
}

func sameValue(a, b any) bool {
	fa, aNumber := a.(float64)
	fb, bNumber := b.(float64)
	if aNumber && bNumber {
		return fa == fb
	}
	return a == b
}

func fieldByName(frame *Frame, name string) *Field {
	for _, f := range frame.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func valueAt(f *Field, row int) any {
	if f == nil || row >= len(f.Values) {
		return nil
	}
	return f.Values[row]
}

// reducerNames is what Grafana calls the column a reducer fills.
var reducerNames = map[string]string{
	"lastNotNull": "Last *", "last": "Last", "firstNotNull": "First *", "first": "First",
	"min": "Min", "max": "Max", "mean": "Mean", "median": "Median", "sum": "Total", "count": "Count",
}

// reduceToRows is the reduce transformation in its series-to-rows mode: a row
// per field that is not a time, named by the field's display name, with a
// column per reducer; a reducer with nothing to reduce gives NaN.
func reduceToRows(frames []*Frame, options map[string]any) ([]*Frame, error) {
	if mode, _ := options["mode"].(string); mode != "" && mode != "seriesToRows" {
		return nil, fmt.Errorf("the reduce transformation in %s mode, which Draw does not replay", mode)
	}
	if options["fields"] != nil || options["labelsToFields"] == true {
		return nil, errors.New("a reduce transformation with a field matcher, which Draw does not replay")
	}
	var reducers []string
	for _, r := range list(options["reducers"]) {
		id, _ := r.(string)
		if _, known := reducerNames[id]; !known {
			return nil, fmt.Errorf("the %q reducer, which Draw does not replay", id)
		}
		reducers = append(reducers, id)
	}
	if len(frames) == 0 {
		return frames, nil
	}
	out := &Frame{RefID: "reduce"}
	fieldCol := &Field{Name: "Field", Type: "string", Config: map[string]any{}}
	out.Fields = append(out.Fields, fieldCol)
	calcs := make([]*Field, len(reducers))
	for i, r := range reducers {
		calcs[i] = &Field{Name: reducerNames[r], Type: "number", Config: map[string]any{}}
		out.Fields = append(out.Fields, calcs[i])
	}
	for _, frame := range frames {
		for _, f := range frame.Fields {
			if f.Type == "time" {
				continue
			}
			fieldCol.Values = append(fieldCol.Values, displayName(f, frame, frames))
			for i, r := range reducers {
				v := reduceField(f.Values, r)
				if v == nil {
					v = math.NaN()
				}
				calcs[i].Values = append(calcs[i].Values, v)
			}
		}
	}
	return []*Frame{out}, nil
}

// reduceField is one of Grafana's reducers over values, skipping nulls, which
// is what a field whose configuration names no null handling gets. The
// answer is a float64, the value itself for first and last, or nil.
func reduceField(values []any, reducer string) any {
	switch reducer {
	case "first":
		return valueAt(&Field{Values: values}, 0)
	case "last":
		if len(values) == 0 {
			return nil
		}
		return values[len(values)-1]
	case "firstNotNull", "lastNotNull":
		var found any
		for _, v := range values {
			if v == nil || isNaN(v) {
				continue
			}
			found = v
			if reducer == "firstNotNull" {
				break
			}
		}
		return found
	case "count":
		n := 0
		for _, v := range values {
			if v != nil {
				n++
			}
		}
		return float64(n)
	}
	return reduceNumbers(values, reducer)
}

func isNaN(v any) bool {
	n, isNumber := v.(float64)
	return isNumber && math.IsNaN(n)
}

// reduceNumbers is a reducer over the numbers among values: a sum of nothing
// is 0, and anything else of nothing is nil.
func reduceNumbers(values []any, reducer string) any {
	var numbers []float64
	for _, v := range values {
		if n, isNumber := v.(float64); isNumber && !math.IsNaN(n) {
			numbers = append(numbers, n)
		}
	}
	sum := 0.0
	for _, n := range numbers {
		sum += n
	}
	if reducer == "sum" {
		return sum
	}
	if len(numbers) == 0 {
		return nil
	}
	switch reducer {
	case "min":
		return slices.Min(numbers)
	case "max":
		return slices.Max(numbers)
	case "mean":
		return sum / float64(len(numbers))
	case "median":
		sorted := slices.Sorted(slices.Values(numbers))
		m := len(sorted) / 2
		if len(sorted)%2 == 0 {
			return (sorted[m-1] + sorted[m]) / 2
		}
		return sorted[m]
	}
	return nil
}

// calculateField appends one field computed row by row: binary, between two
// fields named by their display names, or reduceRow, over the fields listed.
// The arithmetic is JavaScript's, where a null operand counts as zero.
func calculateField(frames []*Frame, options map[string]any) ([]*Frame, error) {
	alias, _ := options["alias"].(string)
	if alias == "" {
		return nil, errors.New("a calculateField with no alias, which Draw does not name")
	}
	for _, frame := range frames {
		var values []any
		var err error
		switch options["mode"] {
		case "binary":
			binary, _ := options["binary"].(map[string]any)
			values, err = binaryColumn(frame, frames, binary)
		case "reduceRow":
			reduce, _ := options["reduce"].(map[string]any)
			values, err = rowSums(frame, frames, reduce)
		default:
			err = fmt.Errorf("a calculateField in %v mode, which Draw does not replay", options["mode"])
		}
		if err != nil {
			return nil, err
		}
		if values == nil {
			continue // Grafana leaves a frame it cannot compute as it was
		}
		frame.Fields = append(frame.Fields, &Field{
			Name: alias, Type: "number", Config: map[string]any{"displayName": alias}, Values: values,
		})
	}
	return frames, nil
}

// binaryColumn is one field computed from two, row by row, or nil when the
// frame lacks either of them.
func binaryColumn(frame *Frame, frames []*Frame, binary map[string]any) ([]any, error) {
	left, right := operand(frame, frames, binary["left"]), operand(frame, frames, binary["right"])
	if left == nil || right == nil {
		return nil, nil
	}
	values := make([]any, frameLength(frame))
	for r := range values {
		v, err := arithmetic(binary["operator"], jsNumber(valueAt(left, r)), jsNumber(valueAt(right, r)))
		if err != nil {
			return nil, err
		}
		values[r] = v
	}
	return values, nil
}

// rowSums is the sum of the fields a reduceRow lists, row by row.
func rowSums(frame *Frame, frames []*Frame, reduce map[string]any) ([]any, error) {
	if reduce["reducer"] != "sum" {
		return nil, fmt.Errorf("a calculateField reducing a row with %v, which Draw does not replay", reduce["reducer"])
	}
	include := map[string]bool{}
	for _, name := range list(reduce["include"]) {
		s, _ := name.(string)
		include[s] = true
	}
	values := make([]any, frameLength(frame))
	for r := range values {
		row := []any{}
		for _, f := range frame.Fields {
			if include[displayName(f, frame, frames)] {
				row = append(row, valueAt(f, r))
			}
		}
		values[r] = reduceField(row, "sum")
	}
	return values, nil
}

func operand(frame *Frame, frames []*Frame, spec any) *Field {
	name, isName := spec.(string)
	if !isName {
		m, _ := spec.(map[string]any)
		matcher, _ := m["matcher"].(map[string]any)
		name, _ = matcher["options"].(string)
	}
	for _, f := range frame.Fields {
		if displayName(f, frame, frames) == name {
			return f
		}
	}
	return nil
}

func jsNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return math.NaN()
		}
		return n
	}
	return math.NaN()
}

func arithmetic(operator any, a, b float64) (float64, error) {
	switch operator {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "*":
		return a * b, nil
	case "/":
		return a / b, nil
	}
	return 0, fmt.Errorf("the %v operator, which Draw does not replay", operator)
}

// groupBy is Grafana's group by: a row per distinct combination of the
// grouping fields, and a column "<display name> (<reducer>)" per aggregation.
// A frame holding none of the grouping fields is dropped.
func groupBy(frames []*Frame, options map[string]any) ([]*Frame, error) {
	rules, _ := options["fields"].(map[string]any)
	grouping := map[string]bool{}
	for name, raw := range rules {
		rule, _ := raw.(map[string]any)
		if rule["operation"] == "groupby" {
			grouping[name] = true
		}
	}
	if len(grouping) == 0 {
		return frames, nil
	}
	var out []*Frame
	for _, frame := range frames {
		var keys []*Field
		for _, f := range frame.Fields {
			if grouping[displayName(f, frame, frames)] {
				keys = append(keys, f)
			}
		}
		if len(keys) == 0 {
			continue
		}
		groups, members := groupRows(frame, keys)
		grouped := &Frame{RefID: frame.RefID}
		for _, k := range keys {
			g := &Field{Name: k.Name, Type: k.Type, Config: deepCopy(k.Config)}
			for _, key := range groups {
				g.Values = append(g.Values, valueAt(k, members[key][0]))
			}
			grouped.Fields = append(grouped.Fields, g)
		}
		for _, f := range frame.Fields {
			aggregated, err := aggregate(f, displayName(f, frame, frames), rules, groups, members)
			if err != nil {
				return nil, err
			}
			grouped.Fields = append(grouped.Fields, aggregated...)
		}
		out = append(out, grouped)
	}
	return out, nil
}

// groupRows is the distinct combinations of the grouping fields, in the order
// first met, and the rows of each.
func groupRows(frame *Frame, keys []*Field) (groups []string, members map[string][]int) {
	members = map[string][]int{}
	for r := range frameLength(frame) {
		parts := make([]string, len(keys))
		for i, k := range keys {
			if v := valueAt(k, r); v != nil {
				parts[i] = jsString(v)
			}
		}
		key := strings.Join(parts, ",")
		if members[key] == nil {
			groups = append(groups, key)
		}
		members[key] = append(members[key], r)
	}
	return groups, members
}

// aggregate is the columns a field's rule asks of each group, named
// "<display name> (<reducer>)".
func aggregate(f *Field, name string, rules map[string]any, groups []string, members map[string][]int,
) ([]*Field, error) {
	rule, _ := rules[name].(map[string]any)
	if rule == nil {
		rule, _ = rules[f.Name].(map[string]any)
	}
	aggregations := list(rule["aggregations"])
	if rule["operation"] != "aggregate" || len(aggregations) == 0 {
		if rule["operation"] == "groupby" && len(aggregations) > 0 {
			return nil, fmt.Errorf("a groupBy that also counts its key %q, which Draw does not replay", name)
		}
		return nil, nil
	}
	var out []*Field
	for _, raw := range aggregations {
		reducer, _ := raw.(string)
		if _, known := reducerNames[reducer]; !known {
			return nil, fmt.Errorf("the %q aggregation, which Draw does not replay", reducer)
		}
		typ := "number"
		if strings.HasPrefix(reducer, "first") || strings.HasPrefix(reducer, "last") {
			typ = f.Type
		}
		agg := &Field{Name: name + " (" + reducer + ")", Type: typ, Config: map[string]any{}}
		for _, key := range groups {
			var in []any
			for _, r := range members[key] {
				in = append(in, valueAt(f, r))
			}
			agg.Values = append(agg.Values, reduceField(in, reducer))
		}
		out = append(out, agg)
	}
	return out, nil
}

// filterByValue keeps, or drops, the rows the filters match.
func filterByValue(frames []*Frame, options map[string]any) ([]*Frame, error) {
	filters := list(options["filters"])
	if len(filters) == 0 {
		return frames, nil
	}
	all, include := options["match"] == "all", options["type"] != "exclude"
	for _, frame := range frames {
		var kept []int
		for r := range frameLength(frame) {
			matched := true
			for _, raw := range filters {
				filter, _ := raw.(map[string]any)
				ok, err := valueMatches(frame, frames, filter, r)
				if err != nil {
					return nil, err
				}
				matched = ok
				if ok != all {
					break // any: the first match decides; all: the first miss does
				}
			}
			if matched == include {
				kept = append(kept, r)
			}
		}
		for _, f := range frame.Fields {
			values := make([]any, 0, len(kept))
			for _, r := range kept {
				values = append(values, valueAt(f, r))
			}
			f.Values = values
		}
	}
	return frames, nil
}

func valueMatches(frame *Frame, frames []*Frame, filter map[string]any, row int) (bool, error) {
	name, _ := filter["fieldName"].(string)
	config, _ := filter["config"].(map[string]any)
	options, _ := config["options"].(map[string]any)
	var field *Field
	for _, f := range frame.Fields {
		if displayName(f, frame, frames) == name {
			field = f
		}
	}
	if field == nil {
		return false, nil // Grafana warns and matches nothing
	}
	v := valueAt(field, row)
	n, isNumber := v.(float64)
	limit := jsNumber(options["value"])
	switch config["id"] {
	case "greater":
		return isNumber && n > limit, nil
	case "greaterOrEqual":
		return isNumber && n >= limit, nil
	case "lower":
		return isNumber && n < limit, nil
	case "lowerOrEqual":
		return isNumber && n <= limit, nil
	case "isNull":
		return v == nil, nil
	case "isNotNull":
		return v != nil, nil
	}
	return false, fmt.Errorf("the %v value filter, which Draw does not replay", config["id"])
}

// sortBy sorts every frame by the first field it names, which is the only
// one Grafana reads.
func sortBy(frames []*Frame, options map[string]any) []*Frame {
	sorts := list(options["sort"])
	if len(sorts) == 0 {
		return frames
	}
	by, _ := sorts[0].(map[string]any)
	name, _ := by["field"].(string)
	desc, _ := by["desc"].(bool)
	for _, frame := range frames {
		var key *Field
		for _, f := range frame.Fields {
			if displayName(f, frame, frames) == name {
				key = f
				break
			}
		}
		if key == nil {
			continue
		}
		order := make([]int, frameLength(frame))
		for i := range order {
			order[i] = i
		}
		slices.SortStableFunc(order, func(a, b int) int {
			c := compareValues(valueAt(key, a), valueAt(key, b))
			if desc {
				return -c
			}
			return c
		})
		for _, f := range frame.Fields {
			sorted := make([]any, len(order))
			for i, r := range order {
				sorted[i] = valueAt(f, r)
			}
			f.Values = sorted
		}
	}
	return frames
}

func compareValues(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	fa, aNumber := a.(float64)
	fb, bNumber := b.(float64)
	if aNumber && bNumber {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(jsString(a), jsString(b))
}

// limitRows keeps the first rows of every frame, or the last for a negative
// limit.
func limitRows(frames []*Frame, options map[string]any) []*Frame {
	n := 10
	switch v := options["limitField"].(type) {
	case float64:
		n = int(v)
	case string:
		if parsed, err := strconv.Atoi(v); err == nil {
			n = parsed
		}
	}
	for _, frame := range frames {
		length := frameLength(frame)
		if length <= int(math.Abs(float64(n))) {
			continue
		}
		for _, f := range frame.Fields {
			if n >= 0 {
				f.Values = f.Values[:min(n, len(f.Values))]
			} else {
				f.Values = f.Values[max(len(f.Values)+n, 0):]
			}
		}
	}
	return frames
}

// transpose turns fields into rows. A frame led by a string or a time takes
// its column headers from that field's values; any other frame gets one
// column per row, named restFieldsName and labeled with the row's number.
func transpose(frames []*Frame, options map[string]any) []*Frame {
	firstName, _ := options["firstFieldName"].(string)
	if firstName == "" {
		firstName = "Field"
	}
	restName, _ := options["restFieldsName"].(string)
	if restName == "" {
		restName = "Value"
	}
	out := make([]*Frame, 0, len(frames))
	for _, frame := range frames {
		if len(frame.Fields) == 0 {
			out = append(out, frame)
			continue
		}
		out = append(out, transposeFrame(frame, frames, firstName, restName))
	}
	return out
}

func transposeFrame(frame *Frame, frames []*Frame, firstName, restName string) *Frame {
	head := frame.Fields[0]
	keyed := head.Type == "string" || head.Type == "time" || head.Type == "enum"
	body := frame.Fields
	if keyed {
		body = frame.Fields[1:]
	}
	first := &Field{Name: firstName, Type: "string", Config: map[string]any{}}
	typ := "string"
	for i, f := range body {
		first.Values = append(first.Values, displayName(f, frame, frames))
		switch {
		case i == 0:
			typ = f.Type
		case typ != f.Type:
			typ = "string"
		}
	}
	fields := []*Field{first}
	for r := range frameLength(frame) {
		col := &Field{Name: restName, Type: typ, Config: map[string]any{}}
		if keyed {
			col.Labels = map[string]string{head.Name: jsString(valueAt(head, r))}
		} else {
			col.Labels = map[string]string{"row": strconv.Itoa(r + 1)}
		}
		for _, f := range body {
			v := valueAt(f, r)
			if typ == "string" && v != nil {
				v = jsString(v)
			}
			col.Values = append(col.Values, v)
		}
		fields = append(fields, col)
	}
	return &Frame{RefID: "transpose-" + frame.RefID, Name: frame.Name, Fields: fields}
}

// rowsToFields turns every row into a field, named by the row's value in the
// field mapped to the name and holding its value in the field mapped to the
// value: the first string and the first number when nothing is mapped.
func rowsToFields(frames []*Frame, options map[string]any) []*Frame {
	mapped := map[string]string{}
	for _, raw := range list(options["mappings"]) {
		m, _ := raw.(map[string]any)
		name, _ := m["fieldName"].(string)
		key, _ := m["handlerKey"].(string)
		mapped[key] = name
	}
	out := make([]*Frame, 0, len(frames))
	for _, frame := range frames {
		var nameField, valueField *Field
		for _, f := range frame.Fields {
			display := displayName(f, frame, frames)
			switch {
			case mapped["field.name"] == display, mapped["field.name"] == "" && nameField == nil && f.Type == "string":
				nameField = f
			case mapped["field.value"] == display, mapped["field.value"] == "" && valueField == nil && f.Type == "number":
				valueField = f
			}
		}
		if nameField == nil || valueField == nil {
			out = append(out, frame)
			continue
		}
		turned := &Frame{RefID: frame.RefID}
		for r, name := range nameField.Values {
			turned.Fields = append(turned.Fields, &Field{
				Name: jsString(name), Type: valueField.Type, Config: map[string]any{},
				Values: []any{valueAt(valueField, r)},
			})
		}
		out = append(out, turned)
	}
	return out
}

// ── Field configuration ─────────────────────────────────────────────────────

// applyFieldConfig gives every field the panel's defaults where it has no
// value of its own, then runs the overrides in the order the list gives,
// each matched against the name the field carries when it is reached.
func applyFieldConfig(frames []*Frame, fc map[string]any) ([]*Frame, error) {
	defaults, _ := fc["defaults"].(map[string]any)
	overrides := list(fc["overrides"])
	for _, frame := range frames {
		for _, f := range frame.Fields {
			withDefaults(f, defaults)
			if err := applyOverrides(f, frame, frames, overrides); err != nil {
				return nil, err
			}
		}
	}
	for _, frame := range frames {
		for _, f := range frame.Fields {
			f.Display = displayName(f, frame, frames)
		}
	}
	return frames, nil
}

// withDefaults gives a field every default it has no value of its own for,
// the custom options one by one.
func withDefaults(f *Field, defaults map[string]any) {
	for k, v := range defaults {
		if k == "custom" {
			custom, _ := f.Config["custom"].(map[string]any)
			if custom == nil {
				custom = map[string]any{}
			}
			for ck, cv := range deepCopy(asMap(v)) {
				if _, set := custom[ck]; !set {
					custom[ck] = cv
				}
			}
			f.Config["custom"] = custom
			continue
		}
		if _, set := f.Config[k]; !set {
			f.Config[k] = deepCopyValue(v)
		}
	}
}

// applyOverrides runs a panel's overrides over one field, in order.
func applyOverrides(f *Field, frame *Frame, frames []*Frame, overrides []any) error {
	for _, raw := range overrides {
		o, _ := raw.(map[string]any)
		matcher, _ := o["matcher"].(map[string]any)
		option, _ := matcher["options"].(string)
		var match bool
		switch matcher["id"] {
		case "byName":
			match = displayName(f, frame, frames) == option
		case "byFrameRefID":
			match = frame.RefID == option
		default:
			return fmt.Errorf("the %v override matcher, which Draw does not replay", matcher["id"])
		}
		if !match {
			continue
		}
		for _, rawProp := range list(o["properties"]) {
			prop, _ := rawProp.(map[string]any)
			id, _ := prop["id"].(string)
			setConfig(f.Config, id, deepCopyValue(prop["value"]))
		}
	}
	return nil
}

// setConfig sets a property by its dotted id: custom.hidden is config.custom.hidden.
func setConfig(config map[string]any, id string, value any) {
	path := strings.Split(id, ".")
	m := config
	for _, key := range path[:len(path)-1] {
		next, _ := m[key].(map[string]any)
		if next == nil {
			next = map[string]any{}
			m[key] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

// displayName is Grafana's getFieldDisplayName: the configured name, the
// datasource's, or one made from the frame's name, the field's and its
// labels.
func displayName(f *Field, frame *Frame, frames []*Frame) string {
	if name, given := givenName(f, frame); given {
		return name
	}
	var parts []string
	frameAdded := false
	if frame != nil && frame.Name != "" && frameNamesDiffer(frames) {
		parts = append(parts, frame.Name)
		frameAdded = true
	}
	if f.Name != "" && f.Name != "Value" {
		parts = append(parts, f.Name)
	}
	labels := labelPart(f, frame, frames)
	if labels != "" {
		parts = append(parts, labels)
	}
	if frame != nil && !frameAdded && labels == "" && f.Name == "Value" && frame.Name != "" {
		parts = append(parts, frame.Name)
	}
	name := "Value"
	switch {
	case len(parts) > 0:
		name = strings.Join(parts, " ")
	case f.Name != "":
		name = f.Name
	}
	if name == f.Name {
		name = uniqueName(f, frame)
	}
	return name
}

// givenName is the name a field is displayed under before its own name and
// labels are consulted: the configured one, the datasource's, or a time's.
func givenName(f *Field, frame *Frame) (string, bool) {
	if s, _ := f.Config["displayName"].(string); s != "" {
		return s, true
	}
	if s, _ := f.Config["displayNameFromDS"].(string); s != "" && frame != nil {
		return s, true
	}
	if f.Type == "time" && f.Labels == nil {
		if f.Name != "" {
			return f.Name, true
		}
		return "Time", true
	}
	return "", false
}

// frameNamesDiffer reports whether two frames of the panel carry different
// names, which is when a field's name takes its frame's.
func frameNamesDiffer(frames []*Frame) bool {
	for i := 1; i < len(frames); i++ {
		if frames[i].Name != frames[i-1].Name {
			return true
		}
	}
	return false
}

// labelPart is what a field's labels add to its name: the value of the one
// label every field of the frames shares, or all of them.
func labelPart(f *Field, frame *Frame, frames []*Frame) string {
	if f.Labels == nil || frame == nil {
		return ""
	}
	scope := frames
	if len(scope) == 0 {
		scope = []*Frame{frame}
	}
	if single := singleLabelName(scope); single != "" {
		return f.Labels[single]
	}
	return formatLabels(f.Labels)
}

func singleLabelName(frames []*Frame) string {
	single := ""
	for _, frame := range frames {
		for _, f := range frame.Fields {
			for k := range f.Labels {
				switch single {
				case "":
					single = k
				case k:
				default:
					return ""
				}
			}
		}
	}
	return single
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+`="`+labels[k]+`"`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// uniqueName numbers a field whose name another field of the frame shares.
func uniqueName(f *Field, frame *Frame) string {
	if frame == nil {
		return f.Name
	}
	dupes, self := 0, false
	for _, other := range frame.Fields {
		if other == f {
			self = true
			if dupes > 0 {
				dupes++
				break
			}
			continue
		}
		if other.Name == f.Name {
			dupes++
			if self {
				break
			}
		}
	}
	if dupes > 0 {
		return f.Name + " " + strconv.Itoa(dupes)
	}
	return f.Name
}

// ── What a stat, a gauge or a pie shows ─────────────────────────────────────

// Reading is one value a stat, a gauge, a bar gauge or a pie chart draws,
// under the name it draws it with.
type Reading struct {
	Name string
	// Value is a float64, or nil when the reduction found nothing, which the
	// panel draws as the field's noValue.
	Value any
	Field *Field
}

// Readings reduces drawn frames the way a stat, a gauge, a bar gauge and a
// pie chart do: one reading per number field under the panel's calculation,
// or, with values shown, one per row, named by the row.
func Readings(panel map[string]any, frames []*Frame) ([]Reading, error) {
	options, _ := panel["options"].(map[string]any)
	reduce, _ := options["reduceOptions"].(map[string]any)
	if fields, _ := reduce["fields"].(string); fields != "" {
		return nil, fmt.Errorf("reduceOptions.fields %q, which Readings does not replay", fields)
	}
	calc := "lastNotNull"
	if calcs := list(reduce["calcs"]); len(calcs) > 0 {
		calc, _ = calcs[0].(string)
	}
	if _, known := reducerNames[calc]; !known {
		return nil, fmt.Errorf("the %q calculation, which Readings does not replay", calc)
	}
	perRow, _ := reduce["values"].(bool)
	var out []Reading
	for _, frame := range frames {
		for _, f := range frame.Fields {
			if f.Type != "number" {
				continue
			}
			if !perRow {
				out = append(out, Reading{Name: f.Display, Value: nanAsNil(reduceField(f.Values, calc)), Field: f})
				continue
			}
			for r := range f.Values {
				out = append(out, Reading{Name: rowName(frame, f, r), Value: nanAsNil(f.Values[r]), Field: f})
			}
		}
	}
	return out, nil
}

func nanAsNil(v any) any {
	if n, isNumber := v.(float64); isNumber && math.IsNaN(n) {
		return nil
	}
	return v
}

// rowName is what a panel showing every row calls one: the row's strings,
// and the field's own name as well when the frame has more numbers than one.
func rowName(frame *Frame, f *Field, row int) string {
	if s, _ := f.Config["displayName"].(string); s != "" {
		return s
	}
	var parts []string
	otherNumbers := 0
	for _, other := range frame.Fields {
		if other == f {
			continue
		}
		switch other.Type {
		case "string":
			v := valueAt(other, row)
			text, mapped := other.Mapped(v)
			if !mapped && v != nil {
				text = jsString(v)
			}
			if text != "" {
				parts = append(parts, text)
			}
		case "number":
			otherNumbers++
		}
	}
	if otherNumbers > 0 || len(parts) == 0 {
		parts = append(parts, f.Display)
	}
	return strings.Join(parts, " ")
}

// ── Values as JavaScript sees them ──────────────────────────────────────────

// jsString is String(v) in JavaScript, for the values a frame holds.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		switch {
		case math.IsNaN(t):
			return "NaN"
		case math.IsInf(t, 1):
			return "Infinity"
		case math.IsInf(t, -1):
			return "-Infinity"
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func deepCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopyValue(x)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	}
	return v
}
