package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The documents that repeat the family table in prose. `ghchronicle -groups`
// prints the authoritative list, but a reader meets the table in a document
// long before he meets the binary, and a table naming a group a family is no
// longer in is a checkable lie.
//
// Adding a thirty-third family and wiring it into the runner leaves every one
// of these stale while the whole suite stays green, which is how this was
// found: the two AST tests in families_test.go pin the code to itself and
// nothing pinned the prose to either.
//
// Only the pages are listed. docs/ used to carry its own copies of both tables
// and was pinned here beside them; it is now generated from these same pages by
// site/scripts/gen-docs.mjs, and `make check-docs` is what holds it to them, so
// pinning the output as well would be the second copy this stopped being.
var (
	groupTables = []string{
		filepath.Join("..", "..", "site", "src", "content", "docs", "configuration", "cadences.mdx"),
		filepath.Join("..", "..", "site", "src", "content", "docs", "es", "configuration", "cadences.mdx"),
	}
	cadenceTables = []string{
		filepath.Join("..", "..", "site", "src", "content", "docs", "configuration", "cadences.mdx"),
		filepath.Join("..", "..", "site", "src", "content", "docs", "es", "configuration", "cadences.mdx"),
	}
	exampleConfig = filepath.Join("..", "..", "config.example.yaml")
)

// TestDocumentedGroupTablesMatchTheCode fails when a document's group table
// has come to disagree with defaultEvery about who is in what.
func TestDocumentedGroupTablesMatchTheCode(t *testing.T) {
	for _, path := range groupTables {
		tbl := markdownTable(t, path, "Group", "Families", "Grupo", "Familias")
		group, families := tbl.column(t, "Group", "Grupo"), tbl.column(t, "Families", "Familias")
		documented := map[string][]string{}
		for _, row := range tbl.rows {
			documented[unquote(row[group])] = splitNames(row[families])
		}
		got := slices.Sorted(maps.Keys(documented))
		if want := Groups(); !slices.Equal(got, want) {
			t.Errorf("%s lists groups %v, the code has %v", path, got, want)
			continue
		}
		for _, group := range Groups() {
			if want := FamiliesIn(group); !slices.Equal(documented[group], want) {
				t.Errorf("%s says group %q holds %v, the code says %v", path, group, documented[group], want)
			}
		}
	}
}

// TestDocumentedCadencesMatchTheCode is the same guard for the tables that
// print every family's default interval, so a family added to the code cannot
// stay missing from the reference a reader configures from.
func TestDocumentedCadencesMatchTheCode(t *testing.T) {
	for _, path := range cadenceTables {
		tbl := markdownTable(t, path, "Family", "Default", "Familia", "Por omisión")
		family, every := tbl.column(t, "Family", "Familia"), tbl.column(t, "Default", "Por omisión")
		group, why := tbl.column(t, "Group", "Grupo"), tbl.column(t, "Why that value", "Motivo")
		documented := map[string]string{}
		for _, row := range tbl.rows {
			name := unquote(row[family])
			documented[name] = unquote(row[every])
			checkRow(t, tbl, name, unquote(row[group]), strings.TrimSpace(row[why]))
		}
		checkCadences(t, path, documented)
	}
	checkCadences(t, exampleConfig, exampleEvery(t))
}

// checkRow pins the two columns the cadence table gained with the three
// layers: the group a family answers to, and the reason its built-in cadence
// is the number it is.
//
// The reason is compared literally in English, because defaultEvery is where
// it is written and a table that retypes it is a table that can drift from the
// warning quoting the same words. A translation cannot be compared that way,
// so the Spanish column is held to what is still checkable: one row per
// family, and never empty. A blank reason is the failure that matters, since
// it is what a family added without one produces.
func checkRow(t *testing.T, tbl docTable, family, group, why string) {
	t.Helper()
	if want := defaultEvery[family].group; group != want {
		t.Errorf("%s puts %s in group %q, the code says %q", tbl.path, family, group, want)
	}
	if why == "" {
		t.Errorf("%s gives %s no reason for its cadence", tbl.path, family)
		return
	}
	if !tbl.english {
		return
	}
	if want := Why(family); why != want {
		t.Errorf("%s says %s is %v because %q, the code says %q", tbl.path, family, defaultEvery[family].every, why, want)
	}
}

// checkCadences compares one document's family-to-duration table with
// defaultEvery, in both directions.
func checkCadences(t *testing.T, path string, documented map[string]string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(documented))
	if want := Families(); !slices.Equal(got, want) {
		t.Errorf("%s documents families %v, the code has %v", path, got, want)
		return
	}
	for name, raw := range documented {
		want := defaultEvery[name].every
		if raw == "0" {
			if want != 0 {
				t.Errorf("%s says every.%s is 0, the code says %v", path, name, want)
			}
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Errorf("%s: every.%s reads %q, which is not a duration: %v", path, name, raw, err)
			continue
		}
		if d != want {
			t.Errorf("%s says every.%s is %v, the code says %v", path, name, d, want)
		}
	}
}

// exampleEvery reads the every.families block of config.example.yaml. The file
// is parsed by hand rather than as YAML because what is being checked is the
// block a reader copies, comments and all, not a decoded value.
//
// Only families, because that is the layer that carries one row per family.
// default and groups are shown commented out, and a reader who uncomments one
// has not changed the reference this pins.
func exampleEvery(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	inEvery, inFamilies := false, false
	for _, line := range readLines(t, exampleConfig) {
		if line == "every:" {
			inEvery = true
			continue
		}
		if !inEvery {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break // the next top-level key ends the block
		}
		if line == "  families:" {
			inFamilies = true
			continue
		}
		if !inFamilies || (line != "" && !strings.HasPrefix(line, "    ")) {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok || strings.HasPrefix(name, "#") {
			continue
		}
		if value, _, _ = strings.Cut(value, "#"); value != "" {
			out[name] = strings.TrimSpace(value)
		}
	}
	if len(out) == 0 {
		// Failing here rather than passing an empty table: a parse that finds
		// nothing would otherwise satisfy every check below it.
		t.Fatalf("%s: no every.families block found, so this test proves nothing", exampleConfig)
	}
	return out
}

// TestTheExampleConfigLoads is the check no amount of table pinning gives: the
// file a reader copies has to be a file this package accepts. The every block
// changed shape under it once already.
func TestTheExampleConfigLoads(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(exampleConfig)
	if err != nil {
		t.Fatalf("%s does not load: %v", exampleConfig, err)
	}
	// It documents the built-in values, so it must ask for nothing the audit
	// did not already choose and must earn no warning at all.
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("%s starts with warnings, which the reference configuration should never do: %v", exampleConfig, got)
	}
}

// docTable is one table lifted out of a document: where it came from, its
// header row and its body rows.
//
// The header is kept rather than thrown away because these tables now have
// four columns and gained one this week. A check that counted from the left
// would read the wrong column the next time one is inserted, and read it
// silently.
type docTable struct {
	path    string
	header  []string
	rows    [][]string
	english bool
}

// column is the index of the header cell carrying one of the given names, so
// a check can ask for the column it means. Fatal rather than a sentinel: a
// missing column is a table that no longer proves what it was written to.
func (tbl docTable) column(t *testing.T, names ...string) int {
	t.Helper()
	for i, cell := range tbl.header {
		if slices.Contains(names, cell) {
			return i
		}
	}
	t.Fatalf("%s: the table has columns %v and none of them is %v", tbl.path, tbl.header, names)
	return -1
}

// markdownTable returns the first table whose header row carries at least two
// of the given column names, so one parser serves the English documents and
// their Spanish translations.
//
// Which of the two it found is recorded on the way past: the first header name
// is the English one, so a document whose header matches it is the document
// the code's own prose can be compared against literally.
func markdownTable(t *testing.T, path string, headers ...string) docTable {
	t.Helper()
	tbl := docTable{path: path}
	for _, line := range readLines(t, path) {
		if !strings.HasPrefix(line, "|") {
			if tbl.header != nil {
				break
			}
			continue
		}
		cells := splitRow(line)
		if tbl.header == nil {
			if matched(cells, headers) >= 2 {
				tbl.header = cells
				tbl.english = slices.Contains(cells, headers[0])
			}
			continue
		}
		if strings.HasPrefix(cells[0], "--") {
			continue // the header separator
		}
		tbl.rows = append(tbl.rows, cells)
	}
	if len(tbl.rows) == 0 {
		// A document that lost its table entirely would otherwise pass, which
		// is the failure this whole file exists to prevent.
		t.Fatalf("%s: no table with headers %v, so this test proves nothing", path, headers)
	}
	return tbl
}

func matched(cells, headers []string) int {
	n := 0
	for _, cell := range cells {
		if slices.Contains(headers, cell) {
			n++
		}
	}
	return n
}

func splitRow(line string) []string {
	var out []string
	for cell := range strings.SplitSeq(strings.Trim(line, "|"), "|") {
		out = append(out, strings.TrimSpace(cell))
	}
	return out
}

func splitNames(cell string) []string {
	var out []string
	for name := range strings.SplitSeq(cell, ",") {
		if name = unquote(name); name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func unquote(s string) string { return strings.Trim(strings.TrimSpace(s), "`") }

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return strings.Split(string(b), "\n")
}

// TestTheExampleConfigNamesEverySetting walks the yaml tags of this package's
// own types and fails when config.example.yaml does not name one of them.
//
// The file calls itself the documented configuration and CLAUDE.md asks for it
// to be kept in step by hand, which it was not: five settings were reachable
// from a config file and appeared nowhere in the reference a reader takes as
// the inventory. Adding a field is now enough to fail here, which is the only
// way that stops happening.
//
// Commented-out blocks count. Most of the file is a comment on purpose: a
// reader reads them, and uncommenting one is how a setting is used.
func TestTheExampleConfigNamesEverySetting(t *testing.T) {
	named := exampleKeys(t)
	for _, key := range yamlKeys(reflect.TypeFor[Config](), "") {
		if !named[key] {
			t.Errorf("%s never names %s, and it is the file a reader takes as the inventory", exampleConfig, key)
		}
	}
}

// yamlKeys is every dotted key a config file may carry, read off the struct
// tags. It stops at a map or a slice: what is under `every.families` or
// `sinks.otlp.headers` is the user's own vocabulary, not this package's.
func yamlKeys(t reflect.Type, prefix string) []string {
	var out []string
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue // unexported, or set by a flag rather than by the file
		}
		key := prefix + name
		out = append(out, key)
		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, yamlKeys(ft, key+".")...)
		}
	}
	return out
}

// exampleKeys reads config.example.yaml as a reader does, comments included,
// and returns every dotted key it names.
//
// A commented block keeps its own indentation after the "# ", so the marker is
// removed and what is left is measured where it sits. Prose comments produce
// no key: a line only counts when what follows the marker is a bare key.
func exampleKeys(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	type level struct {
		indent int
		key    string
	}
	var stack []level
	for _, line := range readLines(t, exampleConfig) {
		indent, text, ok := uncomment(line)
		if !ok {
			continue
		}
		m := exampleKeyRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, level{indent, m[1]})
		parts := make([]string, len(stack))
		for i, l := range stack {
			parts[i] = l.key
		}
		out[strings.Join(parts, ".")] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to no keys at all, so this test proves nothing", exampleConfig)
	}
	return out
}

// exampleKeyRe matches a bare YAML key, which is what separates a setting from
// the prose around it.
var exampleKeyRe = regexp.MustCompile(`^([a-z_][a-z0-9_]*):(\s|$)`)

// uncomment returns a line's effective indentation and its text with the
// comment marker taken off, and whether there is anything left to read.
func uncomment(line string) (int, string, bool) {
	body := strings.TrimLeft(line, " ")
	indent := len(line) - len(body)
	if rest, found := strings.CutPrefix(body, "#"); found {
		rest = strings.TrimPrefix(rest, " ")
		inner := strings.TrimLeft(rest, " ")
		indent += len(rest) - len(inner)
		body = inner
	}
	return indent, body, body != ""
}

// TestTheExampleConfigShowsDedupeAsItResolves is the other half of naming a
// setting: the value beside the name.
//
// Every commented line in config.example.yaml is read as the default, because
// that is what the rest of the file is: `batch: 5000` is the batch a sink
// takes when nothing says otherwise. `dedupe` was written as false in all five
// sinks that have it, and an absent `dedupe` resolves to on (Dedupe is a
// *bool, nil means on, and buildSinks hands the ledger to a sink unless the
// key refuses it). A reader copying the block got the opposite of the
// behavior, and the coverage test above cannot see it: it asks whether the
// key is named, not what it is named with.
func TestTheExampleConfigShowsDedupeAsItResolves(t *testing.T) {
	var shown int
	for i, line := range readLines(t, exampleConfig) {
		_, text, ok := uncomment(line)
		if !ok || !strings.HasPrefix(text, "dedupe:") {
			continue // dedupe_file and dedupe_horizon are other settings
		}
		shown++
		if value := strings.TrimSpace(strings.TrimPrefix(text, "dedupe:")); value != "true" {
			t.Errorf("%s:%d shows dedupe: %s, and an absent dedupe resolves to on",
				exampleConfig, i+1, value)
		}
	}
	// One per sink that carries the setting. A sink that grows a Dedupe and is
	// documented without a value would otherwise pass this silently.
	if want := dedupeFields(); shown != want {
		t.Errorf("%s shows dedupe for %d sinks; %d sink types declare it", exampleConfig, shown, want)
	}
}

// dedupeFields counts the sink types that carry a dedupe setting.
func dedupeFields() int {
	var n int
	sinks := reflect.TypeFor[Sinks]()
	for field := range sinks.Fields() {
		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		for inner := range ft.Fields() {
			if name, _, _ := strings.Cut(inner.Tag.Get("yaml"), ","); name == "dedupe" {
				n++
			}
		}
	}
	return n
}
