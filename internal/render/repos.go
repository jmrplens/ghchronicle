package render

import (
	"fmt"
	"math"
	"strings"
)

func drawRepoList(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, "Top repositories")
	const band, rowH = 44.0, 36.0
	inner := s.width - 2*ghPad
	var body strings.Builder

	starX := s.width - ghPad - 56
	langRight := starX - 14
	nameX := ghPad + 16
	peak := 0
	for _, r := range s.repos {
		if r.Stars > peak {
			peak = r.Stars
		}
	}
	y := band + 26
	if len(s.repos) == 0 {
		text(&body, ghPad, y, "d", "start", "No repositories to show")
	}
	for i, r := range s.repos {
		if i > 0 {
			y += rowH
		}
		color := languageColor(r.Language)
		fmt.Fprintf(&body, `<circle cx="%s" cy="%s" r="5" fill="%s"/>`+"\n", num(ghPad+5), num(y-4), color)
		text(&body, nameX, y, "n", "start", fit(r.Name, 13, langRight-90-nameX))
		text(&body, langRight, y, "s", "end", fit(r.Language, 11, 84))
		star(&body, starX, y-10, 0.7)
		text(&body, s.width-ghPad, y, "c", "end", grouped(r.Stars))
		// The bar is stars relative to the top repository, which is the only
		// scale that makes five rows comparable at a glance.
		fmt.Fprintf(&body, `<rect class="track" x="%s" y="%s" width="%s" height="4" rx="2"/>`+"\n",
			num(ghPad), num(y+8), num(inner))
		if peak > 0 && r.Stars > 0 {
			fmt.Fprintf(&body, `<rect x="%s" y="%s" width="%s" height="4" rx="2" fill="%s"/>`+"\n",
				num(ghPad), num(y+8), num(math.Max(inner*float64(r.Stars)/float64(peak), 4)), color)
		}
	}
	y += 12
	if len(s.nums) > 0 {
		parts := make([]string, len(s.nums))
		for i, m := range s.nums {
			parts[i] = grouped(m.value) + " " + m.short
		}
		y += 24
		text(&body, ghPad, y, "d mono", "start", fit(strings.Join(parts, "  ·  "), 11, inner/0.6*0.55))
	}
	height := math.Ceil(y + 16)

	openDoc(b, &githubFamily, s, height, describe(c, s), "")
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}
