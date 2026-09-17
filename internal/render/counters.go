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

// countedNumber is one number on its way into the document: its value, where
// it sits, how it is set and how much room it has. A struct and not a
// parameter list, the way gridStyle and spec are: countUp took ten positional
// arguments, six of them a float64 or a string, which is the shape where a
// caller passes the right type in the wrong place and nothing notices. Named
// fields make a call site say which is the size and which is the limit.
type countedNumber struct {
	value         int
	x, y          float64
	class, anchor string
	// size is the type size the text is measured at, limit the width it must
	// fit into; both are handed to fit, which truncates to an ellipsis.
	size, limit float64
	// format writes the value out the way the layout's family does: compact
	// for chronicle, grouped for github.
	format func(int) string
}

// countUp writes one number as the stack a count is made of: an intermediate
// value per frame, each shown only during its own beat, and the real value on
// top of them, revealed when the count lands. The marker classes go on only
// where there are frames, so a grid that does not count writes the number with
// the class it has always had and nothing else.
//
// It is shared rather than copied because two layouts count now: animated-counters
// draws its own grid and statGrid draws the github family's.
func countUp(b *strings.Builder, n countedNumber, motion counterMotion) {
	at := func(class string, value int) {
		text(b, n.x, n.y, class, n.anchor, fit(n.format(value), n.size, n.limit))
	}
	if len(motion.frames) == 0 {
		at(n.class, n.value)
		return
	}
	for i, cls := range motion.frames {
		at(classes(n.class, "cuf", cls), counterValue(n.value, i, counterFrames))
	}
	at(classes(n.class, "cuz", motion.final), n.value)
}

// counterValue is the value shown at frame i of n, eased so the count
// starts fast and lands softly.
func counterValue(v, i, n int) int {
	u := 1 - float64(i)/float64(n)
	return int(math.Round(float64(v) * (1 - u*u*u)))
}

func drawAnimatedCounters(b *strings.Builder, c *Card, s *spec) {
	inner := s.width - 2*pad
	tl := newTimeline(s.motion, s.speed)
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
			countUp(&body, countedNumber{
				value: m.value, x: x, y: y + 24,
				class: "big", anchor: "start",
				size: 24, limit: cell - 10, format: compact,
			}, count)
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
