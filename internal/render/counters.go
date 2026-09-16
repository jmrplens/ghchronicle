package render

import (
	"fmt"
	"math"
	"strings"
)

// counterFrames is how many intermediate values a counter shows on its way
// up. Sixteen over 1.4 seconds is a visible tick without a wall of text.
const counterFrames = 16

// counterDuration is how long a count takes, in seconds.
const counterDuration = 1.4

// counterMotion is the classes a counting number plays, from counterBeats.
// The zero value writes the number once, which is what a card that does not
// count shows.
type counterMotion struct {
	frames []string
	final  string
}

// counterBeats places a count on the card's timeline: one flash per
// intermediate frame, back to back, and a reveal for the final value when the
// count ends. Every number on the card shares these classes, so they count
// together. Under off it returns no frames, and the final class is "".
func counterBeats(tl *timeline) counterMotion {
	if !tl.moving() {
		return counterMotion{}
	}
	step := counterDuration / counterFrames
	frames := make([]string, counterFrames)
	for i := range frames {
		frames[i] = tl.add(effectFlash, float64(i)*step, step, "linear")
	}
	return counterMotion{frames, tl.add(effectReveal, counterDuration, 0, "linear")}
}

// counterCSS is the one base rule a counting card needs: the intermediate
// frames rest hidden, so a renderer that ignores animation, and a reader under
// prefers-reduced-motion, are left with the settled value alone. A card with
// no frames has nothing to hide and writes no rule.
func counterCSS(m counterMotion) string {
	if len(m.frames) == 0 {
		return ""
	}
	return ".cuf{opacity:0}\n"
}

// countUp writes one number as the stack a count is made of: an intermediate
// value per frame, each shown only during its own beat, and the real value on
// top of them, revealed when the count lands. The marker classes go on only
// where there are frames, so a grid that does not count writes the number with
// the class it has always had and nothing else.
//
// It is shared rather than copied because two layouts count now: animated-counters
// draws its own grid and statGrid draws the github family's.
func countUp(b *strings.Builder, x, y float64, class, anchor string, m counterMotion, value int, format func(int) string, size, limit float64) {
	if len(m.frames) == 0 {
		text(b, x, y, class, anchor, fit(format(value), size, limit))
		return
	}
	for i, cls := range m.frames {
		text(b, x, y, classes(class, "cuf", cls), anchor, fit(format(counterValue(value, i, counterFrames)), size, limit))
	}
	text(b, x, y, classes(class, "cuz", m.final), anchor, fit(format(value), size, limit))
}

// counterValue is the value shown at frame i of n, eased so the count
// starts fast and lands softly.
func counterValue(v, i, n int) int {
	u := 1 - float64(i)/float64(n)
	return int(math.Round(float64(v) * (1 - u*u*u)))
}

func drawAnimatedCounters(b *strings.Builder, c *Card, s *spec) {
	inner := s.width - 2*pad
	tl := newTimeline(s.motion)
	count := counterBeats(tl)
	var spark sparkMotion
	if s.has(fieldSparkline) {
		spark = sparkBeats(tl)
	}
	var body strings.Builder

	y := 34.0
	text(&body, pad, y, "t", "start", fit(s.title, 18, inner))
	if c.Description != "" {
		y += 18
		text(&body, pad, y, "d", "start", fit(c.Description, 11, inner))
	}
	if len(s.nums) > 0 {
		const perRow, rowH = 3, 50.0
		cell := inner / perRow
		y += 16
		for i, m := range s.nums {
			if i > 0 && i%perRow == 0 {
				y += rowH
			}
			x := pad + float64(i%perRow)*cell
			countUp(&body, x, y+24, "big", "start", count, m.value, compact, 24, cell-10)
			text(&body, x, y+40, "l", "start", fit(m.label, 11, cell-10))
		}
		y += 40
	}
	if s.has(fieldSparkline) {
		top := y + 16
		text(&body, pad, top+8, "h", "start", "CONTRIBUTIONS PER DAY")
		fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
			num(pad), num(top+58), num(pad+inner), num(top+58))
		drawSparkline(&body, c.Sparkline, pad, top+14, inner, 44, spark)
		y = top + 58
	}
	height := math.Ceil(y + 16)

	openDoc(b, &chronicleFamily, s, height, describe(c, s), counterCSS(count)+tl.css())
	cardBG(b, s.width, height)
	b.WriteString(body.String())
}
