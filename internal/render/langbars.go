package render

import (
	"fmt"
	"math"
	"strings"
)

// One row of the list: the name and the share above, the bar under them.
const (
	barRowH  = 34.0
	barTrack = 8.0
	// barDrop is how far under the name and the share their bar sits.
	barDrop = 10.0

	// barGrow is how long one bar takes to reach its share, barStagger how far
	// behind the bar above it a bar starts, and barLabel how long the name and
	// the percentage take to arrive once their own bar has stopped growing.
	// Eight languages is the most there can be, so the last label lands at
	// 7*0.16 + 0.5 + 0.3, a second and nine tenths after the card opens.
	barGrow    = 0.5
	barStagger = 0.16
	barLabel   = 0.3
)

func drawLanguageBars(b *strings.Builder, c *Card, s *spec) {
	ghTitled(s, "Most used languages")
	const band = 44.0
	inner := s.width - 2*ghPad
	tl := newTimeline(s.motion, s.speed)
	var body strings.Builder

	// One beat per bar and one per label, placed before anything is drawn so
	// the stylesheet runs in the order the card plays: a bar, then what says
	// how much of the account it is, then the bar under it.
	grow := make([]string, len(s.langs))
	label := make([]string, len(s.langs))
	for i := range s.langs {
		at := float64(i) * barStagger
		grow[i] = tl.add(effectGrowX, at, barGrow, "ease-out")
		label[i] = tl.add(effectFade, at+barGrow, barLabel, "ease-out")
	}

	y := band + 30
	text(&body, ghPad, y, "d", "start", "Share of bytes across public repositories")
	y += 22
	if len(s.langs) == 0 {
		// The empty track is the card's way of saying there is nothing to
		// share out. It has no width to grow into, so no beat is placed for
		// it and no label waits on one.
		fmt.Fprintf(&body, `<rect class="track" x="%s" y="%s" width="%s" height="%s" rx="%s"/>`+"\n",
			num(ghPad), num(y), num(inner), num(barTrack), num(barTrack/2))
		y += barTrack
	}
	for i, l := range s.langs {
		langBarRow(&body, s, l, y+float64(i)*barRowH, [2]string{grow[i], label[i]})
	}
	if n := len(s.langs); n > 0 {
		// The card ends at the last bar, not at where the row after it would
		// have begun.
		y += float64(n-1)*barRowH + barDrop + barTrack
	}

	if len(s.nums) > 0 {
		fmt.Fprintf(&body, `<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n",
			num(ghPad), num(y+10.5), num(ghPad+inner), num(y+10.5))
		y = statGrid(&body, s.nums, ghPad, y+44, inner, 4, bigGrid)
	}
	height := math.Ceil(y + 18)

	openDoc(b, &githubFamily, s, height, describe(c, s), tl.css())
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}

// langBarRow is one language: its name on the left, its share on the right and
// its own bar under the two, over a track the width of the whole row. motion
// is the class the bar grows with and the class the name and the share fade in
// with, both empty on a card that does not move.
//
// The class goes on the filled rectangle itself, which is placed with x and y
// and carries no transform of its own, so the growth has the whole transform
// property to itself and the bar grows from its own left edge.
//
// The name and the share arrive once the bar has stopped, rather than standing
// over a bar that is not there yet: what describes a moving thing waits for it.
func langBarRow(b *strings.Builder, s *spec, l langShare, y float64, motion [2]string) {
	inner := s.width - 2*ghPad
	share := num(math.Round(l.Share)) + "%"
	text(b, ghPad, y, classes("n", motion[1]), "start", fit(l.Name, 13, inner-textWidth(share, 13)-16))
	text(b, s.width-ghPad, y, classes("c", motion[1]), "end", share)
	fmt.Fprintf(b, `<rect class="track" x="%s" y="%s" width="%s" height="%s" rx="%s"/>`+"\n",
		num(ghPad), num(y+barDrop), num(inner), num(barTrack), num(barTrack/2))
	fmt.Fprintf(b, `<rect%s x="%s" y="%s" width="%s" height="%s" rx="%s" fill="%s"/>`+"\n",
		classAttr(motion[0]), num(ghPad), num(y+barDrop), num(math.Max(l.Share/100*inner, barTrack)),
		num(barTrack), num(barTrack/2), l.Color)
}
