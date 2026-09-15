package render

import (
	"fmt"
	"math"
	"strings"
)

// The badge is the shields.io flat style: twenty pixels tall, eleven pixel
// Verdana, a gray label half and a colored value half. The size and the
// font are what a README line of badges already uses, so this row lines up
// with the ones next to it.
const (
	badgeH    = 20.0
	badgeFont = 11.0
	badgeGap  = 6.0
	badgeCSS  = "text{font-family:Verdana,Geneva,'DejaVu Sans',sans-serif;font-size:11px}\n.pl{font-size:11px}\n"
)

func drawBadgeRow(b *strings.Builder, c *Card, s *spec) {
	type pill struct {
		label, value string
		lw, vw       float64
	}
	pills := make([]pill, 0, len(s.nums))
	total := 0.0
	for _, m := range s.nums {
		p := pill{label: m.short, value: compact(m.value)}
		p.lw = math.Ceil(textWidth(p.label, badgeFont) + 12)
		p.vw = math.Max(math.Ceil(textWidth(p.value, badgeFont)+12), 30)
		pills = append(pills, p)
		total += p.lw + p.vw
	}
	if len(pills) == 0 {
		// An empty row still has to be a document. One pill with the login
		// says whose card this was going to be.
		p := pill{label: "github", value: c.Login}
		p.lw = math.Ceil(textWidth(p.label, badgeFont) + 12)
		p.vw = math.Ceil(textWidth(p.value, badgeFont) + 12)
		pills = append(pills, p)
		total = p.lw + p.vw
	}
	s.width = total + badgeGap*float64(len(pills)-1)

	openDoc(b, &chronicleFamily, s, badgeH, describe(c, s), badgeCSS)
	x := 0.0
	for _, p := range pills {
		// Four rectangles per pill: two rounded for the outer corners, two
		// square so the seam between the halves is a straight line.
		fmt.Fprintf(b, `<rect class="track" x="%s" y="0" width="%s" height="%s" rx="3"/>`+"\n", num(x), num(p.lw+3), num(badgeH))
		fmt.Fprintf(b, `<rect class="track" x="%s" y="0" width="%s" height="%s"/>`+"\n", num(x+3), num(p.lw), num(badgeH))
		fmt.Fprintf(b, `<rect class="pill-v" x="%s" y="0" width="%s" height="%s" rx="3"/>`+"\n", num(x+p.lw), num(p.vw), num(badgeH))
		fmt.Fprintf(b, `<rect class="pill-v" x="%s" y="0" width="%s" height="%s"/>`+"\n", num(x+p.lw), num(p.vw-3), num(badgeH))
		text(b, x+p.lw/2, 14, "pl t", "middle", p.label)
		text(b, x+p.lw+p.vw/2, 14, "pl on-accent", "middle", p.value)
		x += p.lw + p.vw + badgeGap
	}
}

func drawWideBanner(b *strings.Builder, c *Card, s *spec) {
	const height = 60.0
	tl := newTimeline(s.motion)
	var spark sparkMotion
	if s.has(fieldSparkline) {
		spark = sparkBeats(tl)
	}
	openDoc(b, &chronicleFamily, s, height, describe(c, s),
		".line{stroke-width:1.25;opacity:0.35}\n.area{fill-opacity:0.07}\n.t2{font-size:15px;font-weight:600}\n"+tl.css())
	cardBG(b, s.width, height)

	if s.has(fieldSparkline) {
		drawSparkline(b, c.Sparkline, 1, 20, s.width-2, 38, spark)
	}

	left := 20.0
	nameW := 180.0
	if c.Name != "" && c.Name != c.Login {
		text(b, left, 27, "t2 t", "start", fit(c.Login, 15, nameW))
		text(b, left, 44, "d", "start", fit(c.Name, 11, nameW))
	} else {
		text(b, left, 36, "t2 t", "start", fit(c.Login, 15, nameW))
	}

	if len(s.nums) == 0 {
		return
	}
	x0 := left + nameW + 20
	cell := (s.width - 20 - x0) / float64(len(s.nums))
	for i, m := range s.nums {
		x := x0 + float64(i)*cell
		text(b, x, 30, "v", "start", fit(compact(m.value), 16, cell-8))
		text(b, x, 45, "l", "start", fit(m.short, 11, cell-8))
	}
}
