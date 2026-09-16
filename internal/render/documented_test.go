package render

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The pages that repeat the Supports table of every layout in prose. A reader
// picking a layout meets this table long before he meets -card-layouts, and a
// row naming a layout that cannot draw the field is a checkable lie: the card
// drops an unsupported field in silence, so the page is the only warning there
// is.
//
// It was one. Both pages said nine of the fifteen fields were numbers, where
// numericFields holds twelve, and both left repo-list out of the layouts that
// cannot draw a sparkline. Nothing caught either, because nothing read the
// pages.
var layoutFieldTables = []string{
	filepath.Join("..", "..", "site", "src", "content", "docs", "card", "layouts.mdx"),
	filepath.Join("..", "..", "site", "src", "content", "docs", "es", "card", "layouts.mdx"),
}

// blockFields are the three that need room of their own, so each layout
// chooses whether to draw them. Every other field is a number every layout
// takes, which is what makes these three the only rows worth a table.
var blockFields = []string{fieldLanguages, fieldTopRepos, fieldSparkline}

// TestDocumentedLayoutFieldsMatchTheCode fails when a page's table of which
// layouts draw the three block fields has come to disagree with the registry.
func TestDocumentedLayoutFieldsMatchTheCode(t *testing.T) {
	row := regexp.MustCompile("(?m)^\\| `(languages|top_repos|sparkline)` +\\|(.+)\\|")
	name := regexp.MustCompile("`([a-z-]+)`")
	for _, path := range layoutFieldTables {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		rows := row.FindAllStringSubmatch(string(body), -1)
		if len(rows) != len(blockFields) {
			t.Fatalf("%s has %d field rows, want %d", path, len(rows), len(blockFields))
		}
		for _, r := range rows {
			field := r[1]
			var documented []string
			for _, m := range name.FindAllStringSubmatch(r[2], -1) {
				documented = append(documented, m[1])
			}
			slices.Sort(documented)
			want := layoutsSupporting(field)
			if !slices.Equal(documented, want) {
				t.Errorf("%s says %s is drawn by %v, the code says %v",
					path, field, documented, want)
			}
		}
	}
}

// numberWords is how these pages write a small count, which is in words. A
// number written in prose is what a reader believes, and knowing its word is
// the only way to hold it to the registry.
var numberWords = map[int][2]string{
	1:  {"One", "Uno"},
	2:  {"Two", "Dos"},
	3:  {"Three", "Tres"},
	4:  {"Four", "Cuatro"},
	5:  {"Five", "Cinco"},
	6:  {"Six", "Seis"},
	7:  {"Seven", "Siete"},
	8:  {"Eight", "Ocho"},
	9:  {"Nine", "Nueve"},
	10: {"Ten", "Diez"},
	11: {"Eleven", "Once"},
	12: {"Twelve", "Doce"},
	13: {"Thirteen", "Trece"},
	14: {"Fourteen", "Catorce"},
	15: {"Fifteen", "Quince"},
	16: {"Sixteen", "Dieciséis"},
	17: {"Seventeen", "Diecisiete"},
	18: {"Eighteen", "Dieciocho"},
	19: {"Nineteen", "Diecinueve"},
	20: {"Twenty", "Veinte"},
}

// word is how a page writes n, in the language of the page at index i of
// layoutFieldTables: 0 English, 1 Spanish. Mid-sentence it is lowercase.
func word(t *testing.T, n, i int, capital bool) string {
	t.Helper()
	pair, ok := numberWords[n]
	if !ok {
		t.Fatalf("no word for %d, and a page writes it out", n)
	}
	if capital {
		return pair[i]
	}
	return strings.ToLower(pair[i])
}

// TestEveryCountWrittenInProseMatchesTheCode fails when a number these pages
// spell out has come to disagree with the registry. Every one of them is a
// count of something in the code and none of them can be generated, because
// they are sentences and one is a frontmatter description, which YAML cannot
// interpolate. So they are checked here instead, in both languages.
//
// It caught one already: both pages said nine of the fifteen fields were
// numbers, where numericFields holds twelve. The layout counts joined it when
// every other registry fact on the page stopped being hand-written, which left
// them the only ones a fourteenth layout could have falsified in silence.
func TestEveryCountWrittenInProseMatchesTheCode(t *testing.T) {
	layouts := Layouts()
	loopers := 0
	for _, l := range layouts {
		if l.Loops {
			loopers++
		}
	}
	for i, path := range layoutFieldTables {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		fields := word(t, len(numericFields), i, true)
		all := word(t, len(Fields()), i, false)
		every := word(t, len(layouts), i, true)
		these := word(t, len(layouts), i, false)
		still := word(t, len(layouts)-loopers, i, false)
		two := word(t, loopers, i, false)
		claims := [][2]string{
			// "Twelve of the fifteen fields are numbers", above the table of
			// the three that are not.
			{fields + " of the " + all, fields + " de los " + all},
			// The frontmatter description of the page.
			{every + " layouts", every + " diseños"},
			// The note on looping, which counts both sides of it.
			{"Only " + two + " layouts loop", "Solo " + two + " diseños hacen bucle"},
			{still + " of these " + these + " layouts", still + " de estos " + these + " diseños"},
		}
		for _, claim := range claims {
			want := claim[i]
			if !strings.Contains(string(body), want) {
				t.Errorf("%s does not say %q, and the registry holds %d layouts, "+
					"%d of which loop, and %d numeric fields of %d. Correct the page",
					path, want, len(layouts), loopers, len(numericFields), len(Fields()))
			}
		}
	}
}

// TestEveryLayoutHasItsFactsOnBothPages fails when a layout has no section on
// a page, in either direction: one the pages never present, or one they present
// that the registry no longer holds.
//
// What the pages used to carry was the registry itself, as a table of thirteen
// rows, and the test here read two of its columns back and compared them with
// the code. Four hand-written copies of two booleans, and before this file
// existed nothing read any of them. The facts now come out of
// site/src/data/layouts.json, which cmd/gen_layouts writes from this registry
// and `make check-layouts` holds to it, so there is nothing left to compare:
// a page cannot state a wrong family, width or default field, only leave a
// layout out. That is what this checks, and it checks it in registry order,
// which is the order -card-layouts prints and the order a reader meets them
// in everywhere else.
func TestEveryLayoutHasItsFactsOnBothPages(t *testing.T) {
	var want []string
	for _, l := range Layouts() {
		want = append(want, l.Name)
	}
	for _, path := range layoutFieldTables {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var documented []string
		for _, stated := range factsOnPage(string(body)) {
			documented = append(documented, stated.layout)
			// The facts are right by construction; the heading over them is
			// not, and a heading naming another layout puts a reader in front
			// of the wrong picture with the right numbers under it. This is
			// what the removed "link every index row to its section" rule used
			// to catch on the way past.
			if !headingNames(stated.heading, stated.layout) {
				t.Errorf("%s states the facts of %q under the heading %q, which does not name it. "+
					"Move the component under its layout's own heading, or correct the heading",
					path, stated.layout, stated.heading)
			}
		}
		if !slices.Equal(documented, want) {
			t.Errorf("%s states the facts of %v, the registry holds %v", path, documented, want)
		}
	}
}

// statedFacts is one <LayoutFacts /> on a page and the heading it sits under.
type statedFacts struct{ layout, heading string }

// factsOnPage reads every <LayoutFacts /> of a page in document order, each
// with the nearest heading above it. A component before any heading carries an
// empty one, which names no layout and so fails.
func factsOnPage(body string) []statedFacts {
	invocation := regexp.MustCompile(`^<LayoutFacts name="([a-z-]+)" */>`)
	heading := regexp.MustCompile(`^#{1,6} +(.*[^ ]) *$`)
	var out []statedFacts
	var current string
	for line := range strings.SplitSeq(body, "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if m := invocation.FindStringSubmatch(line); m != nil {
			out = append(out, statedFacts{layout: m[1], heading: current})
		}
	}
	return out
}

// headingNames is whether a heading names a layout: the identifier is not
// translated, so both pages head a layout's section with its name, alone or in
// a sentence. The name has to stand on its own, with nothing a name is made of
// against either end, so that `github` cannot answer for `github-stats`.
func headingNames(heading, layout string) bool {
	const edge = "[^A-Za-z0-9-]"
	return regexp.MustCompile(
		"(^|" + edge + ")" + regexp.QuoteMeta(layout) + "($|" + edge + ")",
	).MatchString(heading)
}

// actionYAML is the Action's own manifest, whose card-layout input names every
// layout a workflow may ask for. It is the third place the registry is written
// out, after the two pages above, and the design asked for it to be checked
// here: nothing read it, so a layout added to the registry was a layout the
// Action's own documentation did not offer and nothing failed.
var actionYAML = filepath.Join("..", "..", "action.yml")

// TestTheActionOffersEveryRegisteredLayout fails when action.yml's card-layout
// description has come to disagree with the registry, in either direction: a
// name it does not list, or one it lists that no longer exists.
func TestTheActionOffersEveryRegisteredLayout(t *testing.T) {
	body, err := os.ReadFile(actionYAML)
	if err != nil {
		t.Fatalf("%s: %v", actionYAML, err)
	}
	line := regexp.MustCompile(`(?m)^ +description: One of the registered layouts \(([^)]+)\)\.$`).FindSubmatch(body)
	if line == nil {
		t.Fatalf("%s has no card-layout description of the shape this reads", actionYAML)
	}
	documented := strings.Split(string(line[1]), ", ")
	var want []string
	for _, l := range Layouts() {
		want = append(want, l.Name)
	}
	// In registry order, which is the order -card-layouts prints and the order
	// the pages list them in, so a reader meets them the same way everywhere.
	if !slices.Equal(documented, want) {
		t.Errorf("%s offers %v, the registry holds %v", actionYAML, documented, want)
	}
}

// layoutsSupporting is every layout that can draw one field, sorted, which is
// the order a table reads best in and the one the comparison above needs.
func layoutsSupporting(field string) []string {
	var out []string
	for _, l := range Layouts() {
		if slices.Contains(l.Supports, field) {
			out = append(out, l.Name)
		}
	}
	slices.Sort(out)
	return out
}
