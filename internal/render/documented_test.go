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

// speedRangePages are the pages that state how far each end of Options.Speed
// reaches, by language: English first, Spanish second, the way every other
// pair here is ordered. Two per language, because a reader meets the option on
// the card page and again in the flag table, and both had to say what the ends
// do or neither sentence was worth reading.
var speedRangePages = [2][]string{
	{
		filepath.Join("..", "..", "site", "src", "content", "docs", "card", "index.mdx"),
		filepath.Join("..", "..", "site", "src", "content", "docs", "reference", "cli.mdx"),
	},
	{
		filepath.Join("..", "..", "site", "src", "content", "docs", "es", "card", "index.mdx"),
		filepath.Join("..", "..", "site", "src", "content", "docs", "es", "reference", "cli.mdx"),
	},
}

// reachWording is how a page writes speedReach: what the slow end does to the
// length of a card's animation, and what the fast end does to it. Two
// fragments and not a whole sentence, so the pages can put them in a table
// cell and in a line of prose without either of them being a second spelling
// nothing checks.
type reachWording struct{ slow, fast string }

// reachWords is that wording per reach, in the languages of speedRangePages.
// A reach this table has no entry for fails the test below rather than passing
// it in silence, which is the same arrangement numberWords has, and for the
// same reason: these are sentences and a sentence cannot be generated.
//
// The entry is keyed by the reach and not by the word, so the table is the one
// place the two have to agree. Adding a reach means writing what it reads like
// in both languages before the pages may say it.
var reachWords = map[float64][2]reachWording{
	2: {
		{slow: "twice as long as the default", fast: "half as long as the default"},
		{slow: "el doble de larga que la de por omisión", fast: "la mitad de larga que la de por omisión"},
	},
}

// TestTheReachOfTheSpeedRangeIsWrittenAsTheEngineSetsIt holds the four pages
// that say what speed 0 and speed 1 do to the constant that decides it.
//
// speedReach is one number, and these pages write it out in words: "twice as
// long", "la mitad de larga". Written there it is a copy, and a copy of a
// number nothing regenerates is a number waiting to go stale. This is the same
// defect TestTheCardWidthInputNamesNoWidth exists to prevent, arriving from the
// other side: there the remedy is to forbid the copy, because -card-layouts
// already prints the widths; here nothing prints the reach, so a page that
// named no number would leave a reader with a range and no idea what its ends
// do. So the copy is allowed and held to its source instead.
//
// The generated files under docs/ carry the same sentences and are not read
// here. They do not need to be: make check-docs already fails when they no
// longer match the pages, so the page is the only place a sentence can be
// wrong on its own.
func TestTheReachOfTheSpeedRangeIsWrittenAsTheEngineSetsIt(t *testing.T) {
	wording, known := reachWords[speedReach]
	if !known {
		t.Fatalf("no wording for a reach of %v, and %d pages write it out in words. "+
			"Add its entry to reachWords, in both languages, and correct the pages",
			speedReach, len(speedRangePages[0])+len(speedRangePages[1]))
	}
	for i, pages := range speedRangePages {
		for _, path := range pages {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			// Collapsed, because these pages are wrapped at about seventy-six
			// columns and one of the two fragments is split across two lines.
			body := strings.Join(strings.Fields(string(raw)), " ")
			for _, want := range []string{wording[i].slow, wording[i].fast} {
				if !strings.Contains(body, want) {
					t.Errorf("%s does not say %q, and the engine reaches %v either way "+
						"of the default. Correct the page", path, want, speedReach)
				}
			}
		}
	}
}

// TestEveryCountTheReadmeWritesMatchesTheCode is the same rule for the file
// that is read more than any page of the site and had no check at all.
//
// The README states counts in prose the way the pages do, and this round found
// one already wrong: it called the dashboards eighteen sections when they are
// seventeen. Everything the layouts pages state is generated now, and the front
// page is where the last hand-written registry facts live, so they are held
// here. Only the two the registry answers directly are pinned; the dashboard
// sections are pinned by internal/dashboards' own tests.
func TestEveryCountTheReadmeWritesMatchesTheCode(t *testing.T) {
	path := filepath.Join("..", "..", "README.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	body := strings.Join(strings.Fields(string(raw)), " ")
	layouts := Layouts()
	animated := 0
	for _, l := range layouts {
		if l.Animated {
			animated++
		}
	}
	claims := []string{
		// "Thirteen layouts in two visual families", in the card section.
		word(t, len(layouts), 0, true) + " layouts",
		// "Nine of the thirteen animate", and the sentence that follows it
		// turns on the difference between the two numbers.
		word(t, animated, 0, true) + " of the " + word(t, len(layouts), 0, false),
	}
	for _, want := range claims {
		if !strings.Contains(body, want) {
			t.Errorf("README.md does not say %q, and the registry holds %d layouts, "+
				"%d of which animate. Correct the README", want, len(layouts), animated)
		}
	}
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
//
// The page is prose wrapped at about seventy-six columns, so a claim can be
// split across two lines by an edit that does not touch a word of it. The body
// is read with its runs of whitespace collapsed for that reason: a test that
// pins a sentence has to survive the sentence being rewrapped, or it fails for
// the one reason it is not about.
func TestEveryCountWrittenInProseMatchesTheCode(t *testing.T) {
	layouts := Layouts()
	loopers := 0
	for _, l := range layouts {
		if l.Loops {
			loopers++
		}
	}
	for i, path := range layoutFieldTables {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body := strings.Join(strings.Fields(string(raw)), " ")
		fields := word(t, len(numericFields), i, true)
		all := word(t, len(Fields()), i, false)
		every := word(t, len(layouts), i, true)
		two := word(t, loopers, i, false)
		claims := [][2]string{
			// "Twelve of the fifteen fields are numbers", above the table of
			// the three that are not.
			{fields + " of the " + all, fields + " de los " + all},
			// The frontmatter description of the page.
			{every + " layouts", every + " diseños"},
			// The Motion section, which names how many layouts have motion
			// that ends nothing. It used to count the other side too, as
			// "eleven of these thirteen", and that sentence is gone: the
			// section now says loop draws what once draws on every layout but
			// those two, which is the same fact without the arithmetic. The
			// count that is left is the one a fourteenth looping layout would
			// falsify.
			{
				"but the " + two + " whose motion ends nothing",
				"salvo los " + two + " cuyo movimiento no termina",
			},
		}
		for _, claim := range claims {
			want := claim[i]
			if !strings.Contains(body, want) {
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
