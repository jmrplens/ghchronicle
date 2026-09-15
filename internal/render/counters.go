package render

import (
	"fmt"
	"math"
	"strings"
)

// counterFrames is how many intermediate values a counter shows on its way
// up. Sixteen over 1.4 seconds is a visible tick without a wall of text.
const counterFrames = 16

// counterCSS stacks one <text> per frame at the same spot and shows them one
// at a time. The intermediate frames rest at opacity 0 and the final one at
// opacity 1: that is the base style, the animation only borrows it for 1.4
// seconds, so the static state of the document is the finished count.
func counterCSS() string {
	var b strings.Builder
	b.WriteString(".cu{animation-duration:1.4s;animation-timing-function:linear;animation-iteration-count:1}\n")
	step := 100.0 / counterFrames
	for i := range counterFrames {
		from := float64(i) * step
		to := float64(i+1) * step
		fmt.Fprintf(&b, ".cu%d{opacity:0;animation-name:k%d}\n", i, i)
		fmt.Fprintf(&b, "@keyframes k%d{", i)
		if i > 0 {
			fmt.Fprintf(&b, "0%%,%s%%{opacity:0}", num(from-0.01))
		}
		fmt.Fprintf(&b, "%s%%,%s%%{opacity:1}%s%%,100%%{opacity:0}}\n", num(from), num(to-0.01), num(to))
	}
	b.WriteString(".cuz{animation-name:kz}\n@keyframes kz{0%,99.99%{opacity:0}}\n")
	return b.String()
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
			for f := range counterFrames {
				text(&body, x, y+24, fmt.Sprintf("big cu cu%d", f), "start", fit(compact(counterValue(m.value, f, counterFrames)), 24, cell-10))
			}
			text(&body, x, y+24, "big cu cuz", "start", fit(compact(m.value), 24, cell-10))
			text(&body, x, y+40, "l", "start", fit(m.label, 11, cell-10))
		}
		y += 40
	}
	if s.has(fieldSparkline) {
		top := y + 16
		text(&body, pad, top+8, "h", "start", "CONTRIBUTIONS PER DAY")
		fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
			num(pad), num(top+58), num(pad+inner), num(top+58))
		drawSparkline(&body, c.Sparkline, pad, top+14, inner, 44, sparkBeats(tl))
		y = top + 58
	}
	height := math.Ceil(y + 16)

	openDoc(b, &chronicleFamily, s, height, describe(c, s), counterCSS()+tl.css())
	cardBG(b, s.width, height)
	b.WriteString(body.String())
}
