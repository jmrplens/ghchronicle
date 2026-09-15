package render

import (
	"fmt"
	"strings"
)

func drawSparklineHero(b *strings.Builder, c *Card, s *spec) {
	const height = 170.0
	inner := s.width - 2*pad
	tl := newTimeline(s.motion)
	var spark sparkMotion
	if s.has(fieldSparkline) {
		spark = sparkBeats(tl)
	}
	openDoc(b, &chronicleFamily, s, height, describe(c, s),
		".line{stroke-width:2.5}\n.area{fill-opacity:0.18}\n"+tl.css())
	cardBG(b, s.width, height)

	if s.has(fieldSparkline) {
		drawSparkline(b, c.Sparkline, 1, 66, s.width-2, 82, spark)
	}

	// Three numbers at most: the chart is the point and a fourth would sit
	// on the far edge of it.
	nums := s.nums
	if len(nums) > 3 {
		nums = nums[:3]
	}
	cell := inner / 3
	for i, m := range nums {
		x := pad + float64(i)*cell
		text(b, x, 40, "big", "start", fit(compact(m.value), 24, cell-10))
		text(b, x, 57, "l", "start", fit(m.label, 11, cell-10))
	}
	text(b, s.width-pad, 34, "s", "end", "@"+c.Login)

	caption := "CONTRIBUTIONS PER DAY"
	if n := len(c.Sparkline); n > 0 {
		caption += fmt.Sprintf(", LAST %d DAYS", n)
	}
	text(b, pad, height-10, "h", "start", caption)
	peak := 0
	for _, v := range c.Sparkline {
		if v > peak {
			peak = v
		}
	}
	if s.has(fieldSparkline) {
		text(b, s.width-pad, height-10, "s", "end", fmt.Sprintf("peak %d", peak))
	}
}
