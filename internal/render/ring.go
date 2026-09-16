package render

import (
	"fmt"
	"math"
	"strings"
)

// The ring's clock: the whole donut draws itself in ringDraw seconds, each
// slice taking its own share of that, and the legend fades in over ringLegend
// once the last slice has closed. ringLeast is the floor under a slice's beat,
// so a sliver of a language is a glimpse rather than a beat of no length at
// all.
const (
	ringDraw   = 1.4
	ringLegend = 0.4
	ringLeast  = 0.05
)

// ringPoint is the point at arc length s along the ring, measured clockwise
// from twelve o'clock, which is where the first slice starts.
func ringPoint(cx, cy, r, s float64) (x, y float64) {
	a := s/r - math.Pi/2
	return cx + r*math.Cos(a), cy + r*math.Sin(a)
}

func drawLanguageRing(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, "Languages")
	const band = 44.0
	const r, stroke = 54.0, 16.0
	inner := s.width - 2*ghPad
	cx, cy := ghPad+r+stroke/2, band+18+r+stroke/2
	tl := newTimeline(s.motion)
	var body strings.Builder

	// The ring is an arc per slice, each its own path rather than a dash of a
	// full circle. A dash cannot draw itself: the effect the engine has grows a
	// stroke that carries pathLength="1" from nothing to its whole length, and
	// the whole length of a dashed circle is the ring, not the slice. Written
	// as its own arc, a slice's base style is the finished slice and the dash
	// lives inside the keyframes, which is the rule every effect here obeys.
	// A short gap between slices reads as separate ones instead of one striped
	// ring.
	circ := 2 * math.Pi * r
	fmt.Fprintf(&body, `<circle class="edge" cx="%s" cy="%s" r="%s" stroke-width="%s"/>`+"\n",
		num(cx), num(cy), num(r), num(stroke))
	offset, at := 0.0, 0.0
	for _, l := range s.langs {
		arc := l.Share / 100 * circ
		gap := 3.0
		if arc <= 6 {
			gap = 0
		}
		span := arc - gap
		// Each slice draws itself over its own share of the ring's time, so
		// the stroke travels at one speed however the shares fall.
		beat := math.Max(l.Share/100*ringDraw, ringLeast)
		cls := tl.add(effectDraw, at, beat, "linear")
		at += beat
		x0, y0 := ringPoint(cx, cy, r, offset)
		x1, y1 := ringPoint(cx, cy, r, offset+span)
		large := 0
		if span > circ/2 {
			large = 1
		}
		motion := ""
		if cls != "" {
			// pathLength normalizes the dash to one unit, so the keyframes do
			// not have to know how long this arc is. It is written only on a
			// card that moves: on one that does not, it would be an attribute
			// placed for an animation that is not there.
			motion = classAttr(cls) + ` pathLength="1"`
		}
		fmt.Fprintf(&body, `<path%s d="M%s,%s A%s,%s 0 %d 1 %s,%s" fill="none" stroke="%s" stroke-width="%s"/>`+"\n",
			motion, num(x0), num(y0), num(r), num(r), large, num(x1), num(y1), l.Color, num(stroke))
		offset += arc
	}
	// The legend arrives when the ring is whole, so the reader is not asked to
	// follow a list and a drawing at the same time. A card with no language to
	// show has neither a ring nor a legend, and places no beat for either.
	legend := ""
	if len(s.langs) > 0 {
		legend = tl.add(effectFade, at, ringLegend, "ease-out")
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
		fmt.Fprintf(&body, `<circle%s cx="%s" cy="%s" r="5" fill="%s"/>`+"\n", classAttr(legend), num(lx+5), num(y-4), l.Color)
		text(&body, lx+16, y, classes("n", legend), "start", fit(l.Name, 13, s.width-ghPad-lx-16-56))
		text(&body, s.width-ghPad, y, classes("c", legend), "end", num(math.Round(l.Share))+"%")
	}
	y := math.Max(cy+r+stroke/2, ly+float64(len(s.langs)-1)*22) + 12

	if len(s.nums) > 0 {
		fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
			num(ghPad), num(y+0.5), num(ghPad+inner), num(y+0.5))
		y = statGrid(&body, s.nums, ghPad, y+34, inner, 4, bigGrid)
	}
	height := math.Ceil(y + 18)

	openDoc(b, &githubFamily, s, height, describe(c, s), tl.css())
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}
