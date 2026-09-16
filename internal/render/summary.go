package render

import (
	"fmt"
	"math"
	"strings"
)

// gridStyle is how one family sets a number and its label.
type gridStyle struct {
	valueClass, labelClass string
	valueSize              float64
	labelDY, rowH          float64
	format                 func(int) string
	label                  func(metric) string
	// count is the classes the numbers count up with, from counterBeats. The
	// zero value writes each number once, which is every grid that stands
	// still; a layout that counts takes a copy of one of the styles below and
	// fills it in.
	count counterMotion
}

var (
	chronicleGrid = gridStyle{valueClass: "v", labelClass: "l", valueSize: 16, labelDY: 15, rowH: 42, format: compact, label: func(m metric) string { return m.label }}
	githubGrid    = gridStyle{valueClass: "v", labelClass: "l", valueSize: 22, labelDY: 18, rowH: 46, format: grouped, label: func(m metric) string { return m.long }}
	bigGrid       = gridStyle{valueClass: "big", labelClass: "l", valueSize: 20, labelDY: 16, rowH: 44, format: grouped, label: func(m metric) string { return m.long }}
)

// statGrid lays the numbers out in rows of perRow, the last row taking the
// full width in fewer, wider cells. baseline is the first row's value
// baseline; the return is the last row's label baseline.
func statGrid(b *strings.Builder, nums []metric, x, baseline, inner float64, perRow int, st gridStyle) float64 {
	y := baseline
	for start := 0; start < len(nums); start += perRow {
		end := min(start+perRow, len(nums))
		cell := inner / float64(end-start)
		for i, m := range nums[start:end] {
			cx := x + float64(i)*cell
			countUp(b, countedNumber{
				value: m.value, x: cx, y: y,
				class: st.valueClass, anchor: "start",
				size: st.valueSize, limit: cell - 6, format: st.format,
			}, st.count)
			text(b, cx, y+st.labelDY, st.labelClass, "start", fit(st.label(m), 11, cell-6))
		}
		if end < len(nums) {
			y += st.rowH
		}
	}
	return y + st.labelDY
}

func drawSummary(b *strings.Builder, c *Card, s *spec) {
	inner := s.width - 2*pad
	var body strings.Builder

	y := 34.0 // title baseline
	text(&body, pad, y, "t", "start", fit(s.title, 18, inner))
	if c.Description != "" {
		y += 18
		text(&body, pad, y, "d", "start", fit(c.Description, 11, inner))
	}
	if len(s.nums) > 0 {
		y = statGrid(&body, s.nums, pad, y+14+16, inner, 4, chronicleGrid)
	}
	for _, f := range s.fields {
		switch f {
		case fieldLanguages:
			if len(s.langs) > 0 {
				y = summaryLanguages(&body, s, y)
			}
		case fieldSparkline:
			top := y + 16
			text(&body, pad, top+8, "h", "start", "CONTRIBUTIONS PER DAY")
			// The baseline is drawn whatever the data does, so an account with
			// no contributions at all still gets a chart rather than a hole.
			fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
				num(pad), num(top+58), num(pad+inner), num(top+58))
			drawSparkline(&body, c.Sparkline, pad, top+14, inner, 44, sparkMotion{})
			y = top + 58
		case fieldTopRepos:
			if len(s.repos) > 0 {
				y = summaryRepos(&body, s, y)
			}
		}
	}
	height := math.Ceil(y + 16)

	openDoc(b, &chronicleFamily, s, height, describe(c, s), "")
	cardBG(b, s.width, height)
	b.WriteString(body.String())
}

// summaryLanguages is a thin share bar with the biggest names under it.
func summaryLanguages(b *strings.Builder, s *spec, y float64) float64 {
	inner := s.width - 2*pad
	top := y + 16
	text(b, pad, top+8, "h", "start", "LANGUAGES")
	langBar(b, s.langs, pad, top+14, inner, 8, "")
	x := pad
	ly := top + 36
	for _, l := range s.langs {
		label := l.Name + " " + num(math.Round(l.Share)) + "%"
		w := 14 + textWidth(label, 11)
		if x+w > pad+inner {
			break
		}
		fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="4" fill="%s"/>`+"\n", num(x+4), num(ly-4), l.Color)
		text(b, x+13, ly, "s", "start", label)
		x += w + 12
	}
	return ly
}

func summaryRepos(b *strings.Builder, s *spec, y float64) float64 {
	top := y + 24
	text(b, pad, top, "h", "start", "TOP REPOSITORIES BY STARS")

	starX := s.width - pad - 44
	langRight := starX - 14
	nameX := pad + 14
	nameMax := langRight - 78 - 6 - nameX

	y = top + 20
	for i, r := range s.repos {
		if i > 0 {
			y += rowStep
		}
		fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="4" fill="%s"/>`+"\n",
			num(pad+4), num(y-4), languageColor(r.Language))
		text(b, nameX, y, "n", "start", fit(r.Name, 12, nameMax))
		if r.Language != "" {
			text(b, langRight, y, "s", "end", fit(r.Language, 11, 78))
		}
		star(b, starX, y-9.5, 0.6)
		text(b, s.width-pad, y, "c", "end", compact(r.Stars))
	}
	return y + 2
}

// langBar is the segmented share bar, two pixel gaps between segments and a
// floor of one pixel so a tiny language is still a visible sliver.
//
// grow names the beat that grows the bar, or is empty on a bar that does not
// move. It goes on a group around the whole bar rather than on each segment:
// a segment scaled from its own left edge would take the gap to its right with
// it, and what the card means to show growing is one bar, not a race between
// one segment per language.
func langBar(b *strings.Builder, langs []langShare, x, y, w, h float64, grow string) {
	if grow != "" {
		fmt.Fprintf(b, `<g%s>`+"\n", classAttr(grow))
		defer b.WriteString("</g>\n")
	}
	if len(langs) == 0 {
		fmt.Fprintf(b, `<rect class="track" x="%s" y="%s" width="%s" height="%s" rx="3"/>`+"\n",
			num(x), num(y), num(w), num(h))
		return
	}
	pos := x
	for _, l := range langs {
		seg := l.Share / 100 * w
		fmt.Fprintf(b, `<rect x="%s" y="%s" width="%s" height="%s" rx="3" fill="%s"/>`+"\n",
			num(pos), num(y), num(math.Max(seg-2, 1)), num(h), l.Color)
		pos += seg
	}
}
