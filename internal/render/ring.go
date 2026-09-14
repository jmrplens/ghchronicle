package render

import (
	"fmt"
	"math"
	"strings"
)

func drawLanguageRing(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, "Languages")
	const band = 44.0
	const r, stroke = 54.0, 16.0
	inner := s.width - 2*ghPad
	cx, cy := ghPad+r+stroke/2, band+18+r+stroke/2
	var body strings.Builder

	// The ring is a circle per segment, each dashed to its arc length. The
	// dash is in real units rather than through pathLength="100": librsvg
	// ignores pathLength on a circle and would repeat the dash around the
	// ring. A short gap between segments reads as separate slices instead
	// of one striped ring.
	circ := 2 * math.Pi * r
	fmt.Fprintf(&body, `<circle class="edge" cx="%s" cy="%s" r="%s" stroke-width="%s"/>`+"\n",
		num(cx), num(cy), num(r), num(stroke))
	offset := 0.0
	for _, l := range s.langs {
		arc := l.Share / 100 * circ
		gap := 3.0
		if arc <= 6 {
			gap = 0
		}
		fmt.Fprintf(&body, `<circle cx="%s" cy="%s" r="%s" fill="none" stroke="%s" stroke-width="%s" stroke-dasharray="%s %s" stroke-dashoffset="%s" transform="rotate(-90 %s %s)"/>`+"\n",
			num(cx), num(cy), num(r), l.Color, num(stroke),
			num(arc-gap), num(circ-arc+gap), num(-offset), num(cx), num(cy))
		offset += arc
	}
	if len(s.langs) > 0 {
		top := s.langs[0]
		text(&body, cx, cy+4, "big t", "middle", num(math.Round(top.Share))+"%")
		text(&body, cx, cy+20, "s", "middle", fit(top.Name, 11, 2*r-stroke-12))
	} else {
		text(&body, cx, cy+4, "s", "middle", "no data")
	}

	lx := ghPad + 2*r + stroke + 24
	ly := band + 32
	for i, l := range s.langs {
		y := ly + float64(i)*22
		fmt.Fprintf(&body, `<circle cx="%s" cy="%s" r="5" fill="%s"/>`+"\n", num(lx+5), num(y-4), l.Color)
		text(&body, lx+16, y, "n", "start", fit(l.Name, 13, s.width-ghPad-lx-16-56))
		text(&body, s.width-ghPad, y, "c", "end", num(math.Round(l.Share))+"%")
	}
	y := math.Max(cy+r+stroke/2, ly+float64(len(s.langs)-1)*22) + 12

	if len(s.nums) > 0 {
		fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
			num(ghPad), num(y+0.5), num(ghPad+inner), num(y+0.5))
		y = statGrid(&body, s.nums, ghPad, y+34, inner, 4, bigGrid)
	}
	height := math.Ceil(y + 18)

	openDoc(b, &githubFamily, s, height, describe(c, s), "")
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}
