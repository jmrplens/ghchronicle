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

// counterBeats places a count on the card's timeline: one flash per
// intermediate frame, back to back, and a reveal for the final value when the
// count ends. Every number on the card shares these classes, so they count
// together. Under off it returns no frames, and the final class is "".
func counterBeats(tl *timeline) (frames []string, final string) {
	if !tl.moving() {
		return nil, ""
	}
	step := counterDuration / counterFrames
	frames = make([]string, counterFrames)
	for i := range frames {
		frames[i] = tl.add(effectFlash, float64(i)*step, step, "linear")
	}
	return frames, tl.add(effectReveal, counterDuration, 0, "linear")
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
	frames, final := counterBeats(tl)
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
			for f, cls := range frames {
				text(&body, x, y+24, classes("big cuf", cls), "start", fit(compact(counterValue(m.value, f, counterFrames)), 24, cell-10))
			}
			text(&body, x, y+24, classes("big cuz", final), "start", fit(compact(m.value), 24, cell-10))
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

	// The intermediate frames rest hidden. Under off there are none, and the
	// rule would be a style for nothing.
	base := ""
	if tl.moving() {
		base = ".cuf{opacity:0}\n"
	}
	openDoc(b, &chronicleFamily, s, height, describe(c, s), base+tl.css())
	cardBG(b, s.width, height)
	b.WriteString(body.String())
}
