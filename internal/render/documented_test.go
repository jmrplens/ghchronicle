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

// TestDocumentedNumericFieldCountMatchesTheCode fails when the sentence above
// that table counts the numbers wrongly. It is one sentence in each language,
// and the count in it is the reason the table has three rows and not fifteen.
func TestDocumentedNumericFieldCountMatchesTheCode(t *testing.T) {
	words := map[int]string{9: "Nine", 10: "Ten", 11: "Eleven", 12: "Twelve", 13: "Thirteen"}
	spanish := map[int]string{9: "Nueve", 10: "Diez", 11: "Once", 12: "Doce", 13: "Trece"}
	for i, path := range layoutFieldTables {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		want := words[len(numericFields)]
		if i == 1 {
			want = spanish[len(numericFields)]
		}
		if want == "" {
			t.Fatalf("no word for %d numeric fields", len(numericFields))
		}
		if !strings.Contains(string(body), want+" of the fifteen") &&
			!strings.Contains(string(body), want+" de los quince") {
			t.Errorf("%s does not say %q: there are %d numeric fields",
				path, want, len(numericFields))
		}
	}
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
