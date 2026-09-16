package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	heatWeeks = 12
	heatCell  = 11.0
	heatPitch = 14.0

	// heatWave is how long one week's squares take to fade in, and heatStagger
	// is how far behind the week to its left a week starts. Twenty milliseconds
	// is the design's figure and it is what makes the grid fill from the left
	// rather than all at once; the fade is the free knob, and it sets how long
	// the card moves. At half a second the whole thing was over in 0.72 s,
	// which beside the ring's 1.88 s and the statistics box's 2.3 s read as a
	// flicker rather than as a card animating, so the fade carries the cycle
	// instead: 0.22 s of lead plus 1.6 s of fade is 1.82 s, the same range as
	// the two layouts it sits between.
	heatWave    = 1.6
	heatStagger = 0.02
)

// heatWeekBeats places the wave: one fade per week, left to right. One class
// per week and not one per cell, because eighty-four classes would be
// eighty-four keyframe blocks in a document that has to stay small enough to
// serve from a README, and the seven squares of a week have nothing to say to
// each other anyway. Under off every class comes back empty and no beat is
// placed.
func heatWeekBeats(tl *timeline) []string {
	weeks := make([]string, heatWeeks)
	for w := range weeks {
		weeks[w] = tl.add(effectFade, float64(w)*heatStagger, heatWave, "ease-out")
	}
	return weeks
}

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

// heatLevelClass is the class that paints one level of the calendar ramp.
func heatLevelClass(level int) string {
	return "h" + strconv.Itoa(level)
}

func drawActivityHeatmap(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, fmt.Sprintf("Contributions, last %d weeks", heatWeeks))
	const band = 44.0
	tl := newTimeline(s.motion)
	// The grid is the only thing that moves, so a card asked for no sparkline
	// places no beat: the wave has nothing to cross.
	weeks := make([]string, heatWeeks)
	if s.has(fieldSparkline) {
		weeks = heatWeekBeats(tl)
	}
	var body strings.Builder

	gridW := heatWeeks*heatPitch - (heatPitch - heatCell)
	gridH := 7*heatPitch - (heatPitch - heatCell)
	gx, gy := ghPad, band+18
	bottom := gy + gridH
	if s.has(fieldSparkline) {
		levels := heatLevels(c.Sparkline)
		for w := range heatWeeks {
			for d := range 7 {
				fmt.Fprintf(&body, `<rect class="%s" x="%s" y="%s" width="%s" height="%s" rx="2"/>`+"\n",
					classes(heatLevelClass(levels[w*7+d]), weeks[w]),
					num(gx+float64(w)*heatPitch), num(gy+float64(d)*heatPitch), num(heatCell), num(heatCell))
			}
		}
		ly := bottom + 16
		text(&body, gx, ly+9, "s", "start", "Less")
		lx := gx + 30
		// The key stands still. It is a legend for the ramp, not a week of the
		// calendar, and a key that faded in with the wave would read as data.
		for i := range 5 {
			fmt.Fprintf(&body, `<rect class="%s" x="%s" y="%s" width="%s" height="%s" rx="2"/>`+"\n",
				heatLevelClass(i), num(lx+float64(i)*heatPitch), num(ly), num(heatCell), num(heatCell))
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

	openDoc(b, &githubFamily, s, height, describe(c, s), tl.css())
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}
