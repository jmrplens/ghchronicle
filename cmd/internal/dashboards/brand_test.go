package dashboards

import (
	"encoding/base64"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBrandMarkIsTheRepositoryOne holds the embedded copy of the mark to the
// file cmd/gen_brand writes. The copy exists because go:embed cannot leave
// the package directory, and a copy that is not compared is a second mark.
func TestBrandMarkIsTheRepositoryOne(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "brand", "mark-dark.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != brandMark {
		t.Error("cmd/internal/dashboards/mark-dark.svg differs from brand/mark-dark.svg; " +
			"copy the brand one over it")
	}
}

// TestBrandHeaderOpensEveryDashboard pins the header to the first panel of
// every store: an untitled text panel, the same in all five, transparent so
// it reads as a masthead and not as a box, in html mode so the markdown
// renderer never touches the markup, carrying the mark as a data URI the
// text panel sanitizer lets through, and the two links a reader may want
// from the top of the page.
func TestBrandHeaderOpensEveryDashboard(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		panels, ok := store.Build(nil)["panels"].([]map[string]any)
		if !ok || len(panels) < 2 {
			t.Fatalf("%s has no panels to open with", store.Name)
		}
		// The Overview row comes first; the header is the first panel under it.
		header := panels[1]
		if header["type"] != "text" || header["title"] != "" {
			t.Errorf("%s opens with %v %q, not the untitled brand header", store.Name,
				header["type"], header["title"])
			continue
		}
		if header["transparent"] != true {
			t.Errorf("%s draws the brand header in a box", store.Name)
		}
		options, _ := header["options"].(map[string]any)
		if options["mode"] != "html" {
			t.Errorf("%s renders the brand header as %v, not html", store.Name, options["mode"])
		}
		content, _ := options["content"].(string)
		for _, want := range []string{
			`src="data:image/svg+xml;base64,`, "ghchronicle", docsURL, sourceURL,
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s header lacks %q", store.Name, want)
			}
		}
		if strings.Contains(content, "<svg") {
			t.Errorf("%s header inlines the svg element, which the text panel sanitizer strips", store.Name)
		}
	}
}

// The style properties the text panel sanitizer was measured to keep, on
// Grafana 13.2. It dropped line-height, and an anchor's rel, without a word;
// a property outside this list is one nobody has seen survive.
var keptCSS = map[string]bool{
	"display": true, "flex-direction": true, "flex-wrap": true, "align-items": true,
	"justify-content": true, "gap": true, "margin-bottom": true, "font-size": true,
	"font-weight": true, "text-align": true,
}

// TestBrandHeaderUsesOnlyWhatTheSanitizerKeeps reads every inline style of
// the header and holds each property to the measured list, so the next
// property added is one that has been seen to render.
func TestBrandHeaderUsesOnlyWhatTheSanitizerKeeps(t *testing.T) {
	t.Parallel()
	for _, style := range regexp.MustCompile(`style="([^"]*)"`).FindAllStringSubmatch(brandHeader(), -1) {
		for decl := range strings.SplitSeq(style[1], ";") {
			name, _, found := strings.Cut(strings.TrimSpace(decl), ":")
			if found && !keptCSS[name] {
				t.Errorf("the brand header sets %q, which the sanitizer has not been seen to keep", name)
			}
		}
	}
	if strings.Contains(brandHeader(), `rel="`) {
		t.Error("the brand header writes a rel the sanitizer drops")
	}
}

// TestBrandHeaderIsAColumnThatFitsItsPanel: the mark, the name and the two
// buttons, stacked and centered, have to fit the height brandPanel asks for
// on a phone, where the panel is as wide as the screen and Grafana keeps its
// height. A grid unit is thirty pixels plus an eight pixel gutter.
func TestBrandHeaderIsAColumnThatFitsItsPanel(t *testing.T) {
	t.Parallel()
	const (
		nameHeight   = 29 // 26 pixel type at Grafana's line height
		gap          = 12
		panelPadding = 16
		markOverlap  = 8 // the negative margin under the mark
	)
	column := markSize - markOverlap + gap + nameHeight + gap + buttonHeight + panelPadding
	if height := brandHeight*30 + (brandHeight-1)*8; column > height {
		t.Errorf("the brand column is %d pixels and the panel %d", column, height)
	}
	if markSize < 96 {
		t.Errorf("the mark is drawn at %d pixels, and the header is meant to carry it large", markSize)
	}
	header := brandHeader()
	mark := regexp.MustCompile(`<img src="[^"]+" alt="ghchronicle" width="(\d+)" height="(\d+)"`).FindStringSubmatch(header)
	if mark == nil || mark[1] != mark[2] {
		t.Fatalf("the mark is not drawn square at the top of the header")
	}
	// Each button is an anchor around one image, with a title on the anchor
	// and an alt on the image, so the link has a name however it is read.
	buttons := regexp.MustCompile(`<a href="([^"]+)" target="_blank" title="[^"]+"><img src="data:image/svg\+xml;base64,([^"]+)" alt="([^"]+)" width="(\d+)" height="(\d+)"></a>`).FindAllStringSubmatch(header, -1)
	if len(buttons) != 2 {
		t.Fatalf("the header carries %d buttons, want Docs and Source", len(buttons))
	}
	for _, b := range buttons {
		raw, err := base64.StdEncoding.DecodeString(b[2])
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Width  string `xml:"width,attr"`
			Height string `xml:"height,attr"`
			Text   string `xml:"text"`
		}
		if parseErr := xml.Unmarshal(raw, &doc); parseErr != nil {
			t.Errorf("the %s button is not a well-formed SVG: %v", b[3], parseErr)
			continue
		}
		if doc.Width != b[4] || doc.Height != b[5] {
			t.Errorf("the %s button is drawn at %sx%s and placed at %sx%s", b[3], doc.Width, doc.Height, b[4], b[5])
		}
		if doc.Text != b[3] {
			t.Errorf("the %s button reads %q", b[3], doc.Text)
		}
	}
	if buttons[0][1] != docsURL || buttons[1][1] != sourceURL {
		t.Errorf("the buttons open %s and %s", buttons[0][1], buttons[1][1])
	}
}
