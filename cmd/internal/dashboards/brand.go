package dashboards

import (
	_ "embed"
	"encoding/base64"
	"fmt"
)

// The mark at the top of every dashboard. A copy of brand/mark-dark.svg,
// because go:embed cannot reach outside the package directory; the test in
// brand_test.go fails the moment the two files differ, so the copy cannot
// drift from the one cmd/gen_brand writes.
//
// It travels inside the page as a data URI. Grafana's text panel sanitizer
// keeps an <img> whose src starts with data:image/ and strips every <svg>
// element and every external image it does not trust, so a data URI is the
// one way the mark renders on every Grafana without a server setting. The
// dark mark is used on both themes: the panel cannot pick one per theme, and
// the green GitHub uses on dark backgrounds reads on white as well, if with
// less contrast than the light mark would have.
//
//go:embed mark-dark.svg
var brandMark string

// The two pages the header links to.
const (
	docsURL   = "https://jmrplens.github.io/ghchronicle/"
	sourceURL = "https://github.com/jmrplens/ghchronicle"
)

// The header's measurements, in CSS pixels. The mark is drawn at markSize on
// every screen: on a phone the panel is the width of the screen and the mark
// is the one thing on it, on a desktop it is the size of a stat's number.
// The drawing keeps an eighth of its canvas clear on every side, so the
// cells themselves span 92 of the 128. The button is one SVG image of
// buttonHeight, wide enough for its word in any of the system fonts an
// <img> may be drawn with, since an image cannot reach the page's own font.
const (
	markSize     = 128
	buttonHeight = 36
)

// brandHeight is the header's height in grid units, which is where the
// Overview's first line of numbers starts. Seven units are 258 pixels, and
// the column is 128 of mark, 29 of name, 36 of buttons, the gaps between
// them and the panel's own padding: 225.
const brandHeight = 7

// brandHeader is the content of the untitled text panel that opens the
// dashboard: the mark, the name under it, and under the name a button to the
// documentation and one to the source, all centered in a column. The panel is
// transparent, so on the page it reads as a masthead rather than as one more
// box among the numbers.
//
// Every element is one the text panel sanitizer keeps: a div, an img, an a,
// each with an inline style of properties the sanitizer's CSS list allows.
// Measured on Grafana 13.2: display, flex-direction, flex-wrap, align-items,
// justify-content, gap, margin-bottom, font-size, font-weight and text-align
// survive; line-height and an anchor's rel do not, so neither is written.
// The buttons are SVG images rather than styled anchors so that they look
// the same on every theme and every Grafana: the sanitizer keeps an image
// and keeps a style, but a button drawn in CSS depends on which properties
// survive, and an image depends on nothing.
func brandHeader() string {
	return `<div style="display:flex;flex-direction:column;align-items:center;` +
		`justify-content:center;gap:12px;text-align:center">` +
		// The mark's own clear margin is the gap under it: the cells end
		// sixteen pixels above the image's edge at this size.
		fmt.Sprintf(`<img src=%q alt="ghchronicle" width="%d" height="%d" style="margin-bottom:-8px">`,
			dataURI(brandMark), markSize, markSize) +
		`<div style="font-size:26px;font-weight:600">ghchronicle</div>` +
		`<div style="display:flex;flex-wrap:wrap;gap:10px;justify-content:center">` +
		buttonLink(docsURL, "Docs", "Documentation", docsButton()) +
		buttonLink(sourceURL, "Source", "Source on GitHub", sourceButton()) +
		`</div></div>`
}

// buttonLink is one button: an anchor around the SVG image, the image's alt
// naming the link for a screen reader and the anchor's title for a hover.
func buttonLink(href, word, title, svg string) string {
	return fmt.Sprintf(`<a href=%q target="_blank" title=%q>`+
		`<img src=%q alt=%q width="%d" height="%d"></a>`,
		href, title, dataURI(svg), word, buttonWidth(svg), buttonHeight)
}

func dataURI(svg string) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}

// The two button colors: GitHub's own primary green for the documentation,
// the page a reader most likely wants, and GitHub's own dark gray for the
// source. Both carry white type, which is what lets one drawing serve the
// dark theme and the light one: a filled button reads against either page,
// where an outlined one would need a color per theme.
const (
	docsFill   = "#1f883d"
	sourceFill = "#24292f"
)

// svgOpen is how every button document starts, and what buttonWidth reads
// the width back from.
const svgOpen = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `

// button draws one button as an SVG document: a rounded rectangle, an icon
// and a word. The word is set in the system sans-serif at 14 pixels semibold,
// so its width is not known to the pixel; the rectangle leaves room for the
// widest system font that renders it, and the icon and the word are placed
// from the center so a narrower font leaves the pair centered rather than
// pushed to one side.
func button(width int, fill, word, icon string, wordWidth int) string {
	const (
		iconSize = 16
		iconGap  = 8
	)
	// The icon and the word together are one centered block.
	block := iconSize + iconGap + wordWidth
	iconX := width/2 - block/2
	textX := iconX + iconSize + iconGap + wordWidth/2
	return fmt.Sprintf(svgOpen+`%d %d" width="%d" height="%d" role="img" aria-label="%s">`+
		`<rect x="0.5" y="0.5" width="%d" height="%d" rx="8" fill="%s" stroke="rgba(255,255,255,0.14)"/>`+
		`<g transform="translate(%d 10)" fill="none" stroke="#ffffff" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">%s</g>`+
		`<text x="%d" y="23" text-anchor="middle" fill="#ffffff" font-family="-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif" font-size="14" font-weight="600">%s</text>`+
		`</svg>`,
		width, buttonHeight, width, buttonHeight, word,
		width-1, buttonHeight-1, fill,
		iconX, icon,
		textX, word)
}

// The icons, sixteen pixel line drawings in the style of GitHub's own: an
// open book for the documentation, a pair of angle brackets for the source.
// Neither is GitHub's trademark.
const (
	bookIcon = `<path d="M8 3.5c-1.5-1.2-3.5-1.5-6-1.2v10c2.5-.3 4.5 0 6 1.2 1.5-1.2 3.5-1.5 6-1.2v-10c-2.5-.3-4.5 0-6 1.2z"/><path d="M8 3.5v10"/>`
	codeIcon = `<path d="M5 4.5 1.5 8 5 11.5"/><path d="m11 4.5 3.5 3.5-3.5 3.5"/><path d="m9.5 2.5-3 11"/>`
)

func docsButton() string   { return button(104, docsFill, "Docs", bookIcon, 34) }
func sourceButton() string { return button(120, sourceFill, "Source", codeIcon, 48) }

// buttonWidth reads the width a button was drawn at, so the <img> is sized
// to it and the page never scales the drawing.
func buttonWidth(svg string) int {
	var w, h int
	if _, err := fmt.Sscanf(svg[len(svgOpen):], "%d %d", &w, &h); err != nil {
		panic("a button was not drawn by button(): " + err.Error())
	}
	return w
}

// brandPanel is that panel, on the first line of the Overview section so it
// sits above the numbers in every store and counts in the layout the five
// dashboards share. It has no title: a title bar would be a second line
// saying the same word. Transparent, so there is no box: on the page it is
// a masthead over the numbers, not one more panel among them.
func brandPanel() Panel {
	return panel("text", "", 24, brandHeight, 0, 0, nil, &P{Opts: Opts{
		"content": brandHeader(), "mode": "html", "transparent": true,
	}})
}
