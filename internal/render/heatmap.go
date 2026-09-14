package render

import (
	"fmt"
	"math"
	"strings"
)

const (
	heatWeeks = 12
	heatCell  = 11.0
	heatPitch = 14.0
)

// heatLevels buckets the last twelve weeks of daily counts into GitHub's
// five levels. The sparkline is taken as daily counts ending today, so the
// last cell is today and a series shorter than twelve weeks is padded with
// empty days at the front. There is no clock here to name the weekdays, so
// the rows are days relative to today rather than Monday to Sunday.
func heatLevels(values []int) []int {
	n := heatWeeks * 7
	days := make([]int, n)
	from := max(len(values)-n, 0)
	copy(days[n-(len(values)-from):], values[from:])
	peak := 0
	for _, v := range days {
		if v > peak {
			peak = v
		}
	}
	levels := make([]int, n)
	for i, v := range days {
		switch {
		case v <= 0 || peak <= 0:
			levels[i] = 0
		default:
			levels[i] = int(math.Ceil(4 * float64(v) / float64(peak)))
		}
	}
	return levels
}

func drawActivityHeatmap(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, fmt.Sprintf("Contributions, last %d weeks", heatWeeks))
	const band = 44.0
	var body strings.Builder

	gridW := heatWeeks*heatPitch - (heatPitch - heatCell)
	gridH := 7*heatPitch - (heatPitch - heatCell)
	gx, gy := ghPad, band+18
	bottom := gy + gridH
	if s.has(fieldSparkline) {
		levels := heatLevels(c.Sparkline)
		for w := range heatWeeks {
			for d := range 7 {
				fmt.Fprintf(&body, `<rect class="h%d" x="%s" y="%s" width="%s" height="%s" rx="2"/>`+"\n",
					levels[w*7+d], num(gx+float64(w)*heatPitch), num(gy+float64(d)*heatPitch), num(heatCell), num(heatCell))
			}
		}
		ly := bottom + 16
		text(&body, gx, ly+9, "s", "start", "Less")
		lx := gx + 30
		for i := range 5 {
			fmt.Fprintf(&body, `<rect class="h%d" x="%s" y="%s" width="%s" height="%s" rx="2"/>`+"\n",
				i, num(lx+float64(i)*heatPitch), num(ly), num(heatCell), num(heatCell))
		}
		text(&body, lx+5*heatPitch+2, ly+9, "s", "start", "More")
		bottom = ly + heatCell
	} else {
		bottom = gy
	}

	// Up to three numbers stacked to the right of the grid, which is all the
	// height the grid gives them.
	nums := s.nums
	if len(nums) > 3 {
		nums = nums[:3]
	}
	nx := gx + gridW + 34
	ny := gy + 18
	for i, m := range nums {
		y := ny + float64(i)*44
		text(&body, nx, y, "big", "start", fit(grouped(m.value), 20, s.width-ghPad-nx))
		text(&body, nx, y+16, "l", "start", fit(m.long, 11, s.width-ghPad-nx))
		bottom = math.Max(bottom, y+16)
	}
	height := math.Ceil(bottom + 20)

	openDoc(b, &githubFamily, s, height, describe(c, s), "")
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}
