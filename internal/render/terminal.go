package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The window's measurements. termBar is the title bar, termPad the margin
// inside the window, termRowH one row of output and termFont the size every
// character in it is set at.
const (
	termBar   = 34.0
	termPad   = 16.0
	termRowH  = 17.0
	termFont  = 12.0
	termLabel = 11.0
	// termGap is the breath before a prompt that follows output, which is
	// where one run of the command ends and the next begins.
	termGap = 6.0

	// termType is how long one number takes to type itself in and termStagger
	// how far behind the line above it a line starts, in seconds. termBlink is
	// how long the cursor blinks once the last number has landed: two blinks
	// at blinkPeriod, which is enough to read as a cursor and short enough
	// that the card still settles.
	termType    = 0.3
	termStagger = 0.1
	termBlink   = 1.0

	// termSlop is how far past each end of a number the rectangle that types
	// it reaches, in user units. monoWidth is an approximation of an advance
	// this document has no font to measure, so the cover is given a little
	// more than the text it has to hide, and the last character cannot show
	// through on a reader whose monospace font is wider than the estimate.
	termSlop = 2.0
)

// termCSS is what the window needs beyond the family's own styles: one size
// for every character in the body, whatever class carries its color, and the
// weight that tells a number from the label beside it.
const termCSS = ".tm{font-size:12px}\n.tv{font-weight:600}\n"

// termMaskCSS takes the border off a rectangle painted in the card's own
// background color. The color has to come from a class, because the palette
// is written once per theme and a layout cannot name a literal that follows
// the reader's; .bg is the only class that carries it, and .bg is also the
// card's border. Written only on a card that has a number to type, so a still
// card is styled exactly as it was before this layout could move.
//
// Two classes in the selector, not one: .bg{stroke:...} and .mask{stroke:none}
// would tie on specificity and this one would win only because openDoc writes
// the palette before a layout's own rules. .bg.mask wins on specificity, so it
// no longer depends on the order the blocks are written in.
const termMaskCSS = ".bg.mask{stroke:none}\n"

// markRamp is the mark's opacity by the sum of a cell's row and column: one
// diagonal ridge across a five by five grid, which is what brand/mark-dark.svg
// draws at sixty-four pixels. Its green is the palette's success color in
// both themes, which .ok already carries, so the mark follows the theme
// without a literal of its own.
var markRamp = [9]float64{0.18, 0.18, 0.32, 0.55, 1, 0.55, 0.32, 0.18, 0.18}

// mark draws the project's mark, size wide and size tall, its top left corner
// at x, y.
func mark(b *strings.Builder, x, y, size float64) {
	pitch := size / 5
	cell := pitch * 0.79
	b.WriteString(`<g class="ok">` + "\n")
	for r := range 5 {
		for c := range 5 {
			fmt.Fprintf(b, `<rect x="%s" y="%s" width="%s" height="%s" rx="%s" opacity="%s"/>`+"\n",
				num(x+float64(c)*pitch), num(y+float64(r)*pitch),
				num(cell), num(cell), num(cell*0.3), num(markRamp[r+c]))
		}
	}
	b.WriteString("</g>\n")
}

// termLine is one row of the window: either a command the reader is meant to
// have run, drawn whole because it is the question and not the answer, or a
// line of output whose number types itself in.
type termLine struct {
	command      string
	label, value string
}

// termColumns is where a line's number starts and how much room it has.
type termColumns struct {
	valueX, valueRoom, labelRoom float64
}

// termLayout places the value column: a little past the middle of the window,
// with half of what is left of the line for the number and the other half for
// the rectangle that types it. That halving is the layout's one real
// constraint. A cover is slid off the text it hides, so the line needs as much
// clear space to the right of a number as the number itself takes, and a
// number wider than half of what is left would still be under its own cover
// when the beat ended.
func termLayout(width float64) termColumns {
	inner := width - 2*termPad
	valueX := termPad + math.Round(inner*0.52)
	return termColumns{
		valueX:    valueX,
		valueRoom: (width - termPad - valueX - 3*termSlop) / 2,
		labelRoom: valueX - termPad - 18,
	}
}

func drawTerminal(b *strings.Builder, c *Card, s *spec) {
	col := termLayout(s.width)
	tl := newTimeline(s.motion)
	var body strings.Builder

	lines := []termLine{{command: "ghchronicle --user " + c.Login}}
	for _, m := range s.nums {
		lines = append(lines, termLine{label: m.key, value: grouped(m.value)})
	}
	if len(s.repos) > 0 {
		lines = append(lines, termLine{command: "ghchronicle --top-repos"})
		for _, r := range s.repos {
			lines = append(lines, termLine{label: r.Name, value: grouped(r.Stars)})
		}
	}

	// The numbers type one after another, and the cursor blinks once the last
	// one has landed, so the beats are added in the order the card plays them
	// and the stylesheet reads the way the window fills.
	y := termBar + 22
	at, typed := 0.0, 0
	for i, ln := range lines {
		if i > 0 {
			y += termRowH
		}
		if ln.command != "" {
			if i > 0 {
				y += termGap
			}
			termCommand(&body, y, s.width, ln.command)
			continue
		}
		if termOutput(&body, tl, col, ln, y, at) {
			typed++
			at += termStagger
		}
	}
	// A card with nothing to type still has a cursor, and it still blinks:
	// the cursor is drawn whatever the fields say, so the beat always has an
	// element wearing its class. It starts when the last number lands, which
	// on a card with no numbers is the moment the card opens.
	end := 0.0
	if typed > 0 {
		end = at - termStagger + termType
	}
	blink := tl.add(effectBlink, end, termBlink, "linear")
	y += termRowH + termGap
	termCursor(&body, y, blink)
	height := math.Ceil(y + 13)

	extra := termCSS
	if typed > 0 {
		extra += termMaskCSS
	}
	openDoc(b, &chronicleFamily, s, height, describe(c, s), extra+tl.css())
	cardBG(b, s.width, height)
	topBand(b, s.width, termBar)
	mark(b, termPad, (termBar-16)/2, 16)
	text(b, termPad+26, termBar/2+4, "s", "start", fit(s.title, 11, s.width-termPad-26-8))
	b.WriteString(body.String())
}

// termCommand writes a line the reader is meant to have run: the prompt in the
// mark's own green, and the command after it.
func termCommand(b *strings.Builder, y, width float64, cmd string) {
	x := termPad + monoWidth("$ ", termFont)
	text(b, termPad, y, "c mono ok tm", "start", "$")
	text(b, x, y, "c mono tm", "start", monoFit(cmd, termFont, width-termPad-x))
}

// termOutput writes one line of output: a label, a leader of dots across to
// the value column, the number itself, and on a card that moves the rectangle
// that types the number in. It reports whether it placed that beat, which is
// what advances the clock for the line below.
//
// The rectangle is drawn after the number and over it, painted in the card's
// own background color, and it rests one slop past the end of the text where
// it covers nothing at all: that resting place is the base style, and a
// renderer that ignores animation, or a reader under prefers-reduced-motion,
// sees the finished line. The keyframes are what put it over the text.
func termOutput(b *strings.Builder, tl *timeline, col termColumns, ln termLine, y, at float64) bool {
	value := monoFit(ln.value, termFont, col.valueRoom)
	label := monoFit(ln.label, termLabel, col.labelRoom)
	text(b, termPad, y, "s mono tm", "start", label)
	if dots := termLeader(monoWidth(label, termLabel), col.valueX); dots != "" {
		text(b, termPad+monoWidth(label, termLabel)+6, y, "s mono tm", "start", dots)
	}
	text(b, col.valueX, y, "c mono tv tm", "start", value)

	runes := len([]rune(value))
	if !tl.moving() || runes == 0 {
		return false
	}
	width := monoWidth(value, termFont) + 2*termSlop
	// One step per character, so the number arrives a character at a time
	// rather than sliding out from under its cover.
	cls := tl.addShift(effectType, at, termType, "steps("+strconv.Itoa(runes)+")", width)
	fmt.Fprintf(b, `<rect class="%s" x="%s" y="%s" width="%s" height="%s"/>`+"\n",
		classes("bg", "mask", cls),
		num(col.valueX+monoWidth(value, termFont)+termSlop), num(y-9), num(width), num(13))
	return true
}

// termLeader is the run of dots between a label and the value column, or
// nothing when the two are already as close as the gaps either side of it.
func termLeader(labelWidth, valueX float64) string {
	ch := monoWidth(".", termFont)
	n := int((valueX - 6 - (termPad + labelWidth + 6)) / ch)
	if n < 1 {
		return ""
	}
	return strings.Repeat(".", n)
}

// termCursor writes the prompt the window rests at and the block cursor on it.
// The cursor is a filled block whatever the motion: blinking is a beat over a
// cursor that is there, not a cursor that only exists while the card moves, so
// the settled card is a window waiting for the next command.
func termCursor(b *strings.Builder, y float64, blink string) {
	text(b, termPad, y, "c mono ok tm", "start", "$")
	fmt.Fprintf(b, `<rect class="%s" x="%s" y="%s" width="%s" height="12"/>`+"\n",
		classes("ok", blink), num(termPad+monoWidth("$ ", termFont)), num(y-9), num(monoWidth("m", termFont)))
}
