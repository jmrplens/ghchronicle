package render

import (
	"fmt"
	"math"
	"strings"
)

// The band's measurements. The strip of pills lives inside a viewport of its
// own, tickBandY down from the top of the card and tickBandH tall, and every
// pill is tickPillH tall with tickPadX either side of its content.
const (
	tickBandY = 46.0
	tickBandH = 34.0
	tickPillH = 24.0
	tickPadX  = 12.0
	tickGap   = 10.0

	// tickName is the widest a repository's name is allowed to make its pill,
	// so one long name cannot make the band take a minute to come round.
	tickName = 150.0

	// tickSpeed is how fast the band scrolls, in user units per second. It is
	// a speed and not a duration on purpose: a duration would make a band of
	// four pills crawl and a band of twenty blur, and the one thing a reader
	// has to be able to do with a ticker is read it. It is also why this card
	// takes longer to come round than any other here takes to settle. A pass
	// is as long as the content is wide.
	tickSpeed = 140.0
)

// tickPill is one pill of the band. A pill with a dot is a repository, drawn
// as a language dot, a name, a star and a count; one without is a metric,
// drawn as the number and what it counts.
type tickPill struct {
	dot         string
	lead, trail string
	width       float64
}

// tickerPills is the band's content, metrics first and repositories after, in
// the order the card was asked for them.
func tickerPills(s *spec) []tickPill {
	pills := make([]tickPill, 0, len(s.nums)+len(s.repos))
	for _, m := range s.nums {
		p := tickPill{lead: compact(m.value), trail: m.short}
		p.width = 2*tickPadX + monoWidth(p.lead, 12) + 5 + textWidth(p.trail, 11)
		pills = append(pills, p)
	}
	for _, r := range s.repos {
		p := tickPill{dot: languageColor(r.Language), lead: fit(r.Name, 12, tickName), trail: compact(r.Stars)}
		p.width = 2*tickPadX + 14 + textWidth(p.lead, 12) + 8 + 11 + 3 + monoWidth(p.trail, 12)
		pills = append(pills, p)
	}
	return pills
}

// drawTickPill writes one pill with its left edge at x, inside a band whose
// own top edge is y.
func drawTickPill(b *strings.Builder, p tickPill, x, y float64) {
	top := y + (tickBandH-tickPillH)/2
	fmt.Fprintf(b, `<rect class="track" x="%s" y="%s" width="%s" height="%s" rx="%s"/>`+"\n",
		num(x), num(top), num(p.width), num(tickPillH), num(tickPillH/2))
	at := x + tickPadX
	base := top + tickPillH/2 + 4
	if p.dot == "" {
		text(b, at, base, "c mono", "start", p.lead)
		text(b, at+monoWidth(p.lead, 12)+5, base, "s", "start", p.trail)
		return
	}
	fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="4" fill="%s"/>`+"\n",
		num(at+4), num(top+tickPillH/2), p.dot)
	at += 14
	text(b, at, base, "n", "start", p.lead)
	at += textWidth(p.lead, 12) + 8
	star(b, at, base-10, 0.7)
	text(b, at+14, base, "c mono", "start", p.trail)
}

func drawTicker(b *strings.Builder, c *Card, s *spec) {
	const height = 94.0
	inner := s.width - 2*pad
	tl := newTimeline(s.motion)
	pills := tickerPills(s)

	// One copy of the content is what the band scrolls by, gap included, so
	// the copy behind it stands exactly where this one did and the seam is
	// the same picture twice rather than a jump.
	//
	// Rounded up to a whole user unit, and the copies laid out at that pitch,
	// so the shift is a whole number of pixels. A fractional shift is exact in
	// the geometry and still draws the seam wrong: the band is composited as a
	// layer of its own, and a layer translated by 1403.45 is rasterized off the
	// pixel grid an untranslated one sits on. Measured in Chromium over the
	// gallery's own card, comparing the start of a pass against one whole pass
	// on: 4005 pixels of the band differ at 1403.45, and 38 at 1404, with the
	// text at the same x to three decimals either way. The rounding costs up
	// to one unit of gap after the last pill of a copy, which nothing can see.
	strip := 0.0
	for _, p := range pills {
		strip += p.width + tickGap
	}
	strip = math.Ceil(strip)
	// Gated on there being a pill: a band with nothing in it has nothing to
	// scroll, and a beat for it would be keyframes moving an empty group.
	scroll := ""
	if strip > 0 {
		scroll = tl.addShift(effectSlide, 0, strip/tickSpeed, "linear", strip)
	}

	openDoc(b, &chronicleFamily, s, height, describe(c, s), tl.css())
	cardBG(b, s.width, height)
	login := "@" + c.Login
	text(b, pad, 30, "t", "start", fit(s.title, 18, inner-textWidth(login, 11)-16))
	text(b, s.width-pad, 30, "s", "end", login)

	// A viewport of its own, which is what cuts the strip off at the card's
	// edges. A nested svg clips to its own bounds by default, needs no
	// identifier and is referenced by nothing, where a clip-path would be
	// reached through url() and this document reaches for nothing.
	fmt.Fprintf(b, `<svg x="1" y="%s" width="%s" height="%s">`+"\n",
		num(tickBandY), num(s.width-2), num(tickBandH))
	if len(pills) == 0 {
		text(b, pad, tickBandH/2+4, "d", "start", "Nothing to show")
		b.WriteString("</svg>\n")
		return
	}
	// As many copies as it takes to keep the viewport full through a whole
	// pass, and one copy only on a card that does not move: the others exist
	// to be scrolled into view, and nothing scrolls them there.
	copies := 1
	if scroll != "" {
		fmt.Fprintf(b, `<g%s>`+"\n", classAttr(scroll))
		copies = int(math.Ceil((s.width-2)/strip)) + 1
	}
	for i := range copies {
		x := float64(i) * strip
		for _, p := range pills {
			drawTickPill(b, p, x, 0)
			x += p.width + tickGap
		}
	}
	if scroll != "" {
		b.WriteString("</g>\n")
	}
	b.WriteString("</svg>\n")
}
