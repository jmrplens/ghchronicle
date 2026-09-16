package render

import (
	"fmt"
	"math"
	"strings"
)

// The github family's frame: GitHub's Box component, a bordered card with a
// tinted header band. Twenty-two pixels of padding and a fifty pixel band
// are the owner's existing panel, kept so the two sit side by side.
const (
	ghPad  = 22.0
	ghBand = 50.0
)

// githubFrame paints the card, the band and the border. The band is a path
// with rounded top corners rather than a clipped rectangle: a clip-path is
// referenced by url(), and this document references nothing.
func githubFrame(b *strings.Builder, width, height, band float64) {
	cardBG(b, width, height)
	fmt.Fprintf(b, `<path class="panel" d="M0.5,%s V6.5 A6,6 0 0 1 6.5,0.5 H%s A6,6 0 0 1 %s,6.5 V%s Z"/>`+"\n",
		num(band), num(width-6.5), num(width-0.5), num(band))
	fmt.Fprintf(b, `<line class="axis" x1="0.5" y1="%s" x2="%s" y2="%s"/>`+"\n",
		num(band+0.5), num(width-0.5), num(band+0.5))
	fmt.Fprintf(b, `<rect class="edge" x="0.5" y="0.5" width="%s" height="%s" rx="6"/>`+"\n",
		num(width-1), num(height-1))
}

// githubHeader writes the band's title on the left and the login on the
// right, in the monospace stack so it reads as a handle.
func githubHeader(b *strings.Builder, s *spec, login string, band, pad float64) {
	baseline := band/2 + 6
	right := "@" + login
	rightW := monoWidth(right, 12)
	text(b, pad, baseline, "t", "start", fit(s.title, 15, s.width-2*pad-rightW-12))
	text(b, s.width-pad, baseline, "d mono", "end", right)
}

// ghTitled returns the layout's own heading unless the caller set one.
func ghTitled(s *spec, def string) {
	if !s.titled {
		s.title = def
	}
}

// langBarGrow is how long the share bar takes to reach its full width, in
// seconds. Its legend fades in over the same stretch: a legend already painted
// beside a bar that has not started growing reads as a bar that failed to
// draw, rather than as one that is about to.
const langBarGrow = 0.9

func drawGithubStats(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, "GitHub Statistics")
	inner := s.width - 2*ghPad
	tl := newTimeline(s.motion)

	// The count opens the card and the bar grows once the numbers have landed.
	// The counters are placed first because a beat is named as it is added, so
	// this is also what keeps the counter classes at the front of the
	// stylesheet, where the layout they are shared with has them. A card with
	// no numbers has nothing to wait for and starts the bar at once rather
	// than after a second and a half of a card standing still, and a card
	// asked for neither places no beat at all.
	grid := githubGrid
	start := 0.0
	if len(s.nums) > 0 {
		grid.count = counterBeats(tl)
		start = counterDuration
	}
	// Gated on there being a language to draw and not on the field being
	// asked for: a card asked for languages it has none of draws the empty
	// placeholder track, which has no width to grow into, and a beat for it
	// would be a keyframe block styling a bar nobody can see grow.
	var bar, barLegend string
	if len(s.langs) > 0 {
		bar = tl.add(effectGrowX, start, langBarGrow, "ease-out")
		barLegend = tl.add(effectFade, start, langBarGrow, "ease-out")
	}
	var body strings.Builder

	y := ghBand + 14
	if len(s.nums) > 0 {
		y = statGrid(&body, s.nums, ghPad, ghBand+32, inner, 4, grid)
	}
	for _, f := range s.fields {
		switch f {
		case fieldLanguages:
			y = githubLanguages(&body, s, y, bar, barLegend)
		case fieldTopRepos:
			if len(s.repos) > 0 {
				y = githubRepos(&body, s, y)
			}
		case fieldSparkline:
			text(&body, ghPad, y+36, "h fg", "start", "Contributions per day")
			text(&body, ghPad, y+53, "d", "start", fmt.Sprintf("Last %d days", len(c.Sparkline)))
			fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
				num(ghPad), num(y+118), num(ghPad+inner), num(y+118))
			drawSparkline(&body, c.Sparkline, ghPad, y+66, inner, 52, sparkMotion{})
			y += 122
		}
	}
	height := math.Ceil(y + 22)

	openDoc(b, &githubFamily, s, height, describe(c, s), counterCSS(grid.count)+tl.css())
	githubFrame(b, s.width, height, ghBand)
	githubHeader(b, s, c.Login, ghBand, ghPad)
	b.WriteString(body.String())
}

// githubLanguages is the "Most Used Languages" section: title, subtitle, the
// full-width bar and a two column legend. grow is the beat that grows the bar
// and legend the one that fades its legend in beside it, both empty on a card
// that does not move.
func githubLanguages(b *strings.Builder, s *spec, y float64, grow, legend string) float64 {
	inner := s.width - 2*ghPad
	titleY := y + 36
	text(b, ghPad, titleY, "h fg", "start", "Most Used Languages")
	text(b, ghPad, titleY+17, "d", "start", "Share of bytes across public repositories")
	barY := titleY + 34
	langBar(b, s.langs, ghPad, barY, inner, 14, grow)
	if len(s.langs) == 0 {
		return barY + 14
	}
	top := barY + 14 + 26
	col := inner / 2
	half := (len(s.langs) + 1) / 2
	for i, l := range s.langs {
		cx, row := 0.0, i
		if i >= half {
			cx, row = col, i-half
		}
		ly := top + float64(row)*25
		fmt.Fprintf(b, `<circle%s cx="%s" cy="%s" r="6" fill="%s"/>`+"\n", classAttr(legend), num(ghPad+cx+6), num(ly-4), l.Color)
		text(b, ghPad+cx+20, ly, classes("n", legend), "start", fit(l.Name, 13, col-80))
		text(b, ghPad+cx+col-20, ly, classes("c", legend), "end", num(math.Round(l.Share))+"%")
	}
	return top + float64(half-1)*25 + 6
}

func githubRepos(b *strings.Builder, s *spec, y float64) float64 {
	text(b, ghPad, y+36, "h fg", "start", "Top repositories")
	text(b, ghPad, y+53, "d", "start", "By stars")
	starX := s.width - ghPad - 56
	langRight := starX - 14
	ry := y + 78
	for i, r := range s.repos {
		if i > 0 {
			ry += 24
		}
		fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="5" fill="%s"/>`+"\n",
			num(ghPad+5), num(ry-4), languageColor(r.Language))
		text(b, ghPad+18, ry, "n", "start", fit(r.Name, 13, langRight-90-ghPad-18))
		text(b, langRight, ry, "s", "end", fit(r.Language, 11, 84))
		star(b, starX, ry-10, 0.7)
		text(b, s.width-ghPad, ry, "c", "end", grouped(r.Stars))
	}
	return ry + 6
}

func drawGithubCompact(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, "GitHub")
	const cpad, band = 16.0, 36.0
	inner := s.width - 2*cpad
	var body strings.Builder

	y := band + 10
	if len(s.nums) > 0 {
		y = statGrid(&body, s.nums, cpad, band+30, inner, 6, bigGrid)
	}
	height := math.Ceil(y + 14)

	openDoc(b, &githubFamily, s, height, describe(c, s), ".t{font-size:13px}\n")
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, cpad)
	b.WriteString(body.String())
}
