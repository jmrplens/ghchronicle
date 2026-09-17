package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	heatCell  = 11.0
	heatPitch = 14.0

	// heatWeeksMax is a year of the contribution calendar and the most a card
	// ever draws. accumulate keeps the last year of daily counts and no more,
	// so a fifty-third week would be padding sold as data.
	heatWeeksMax = 52

	// heatNumsGap is the channel between the grid and the numbers beside it.
	heatNumsGap = 34.0

	// heatWave is how long one week's squares take to fade in, and heatSweep
	// is how long the wave takes to cross the grid, from the first week
	// starting to the last one starting. The sweep is what a reader sees,
	// which is why it and not a step per week is the figure held fixed: the
	// grid fills from the left over the same fifth of a second whether it is
	// sixteen weeks wide or fifty-two, and the step between neighbors is
	// whatever that works out to (0.02 s at twelve weeks, which is the design's
	// original figure, 0.01 s at twenty-three, 0.004 s at a year).
	//
	// The fade carries the cycle. At half a second the whole thing was over in
	// 0.72 s, which beside the ring's 1.88 s and the statistics box's 2.3 s
	// read as a flicker rather than as a card animating, so 0.22 s of sweep
	// plus 1.6 s of fade is 1.82 s, the same range as the two layouts it sits
	// between. Holding the sweep rather than the step is also what keeps that
	// 1.82 s true at every width: a fixed 0.02 s step would have stretched the
	// cycle to 2.62 s at a year of squares, well outside the range the figure
	// was chosen from.
	heatWave  = 1.6
	heatSweep = 0.22
)

// heatMaxNums is how many numbers fit beside the grid, which is all the height
// the grid gives them. Both the drawing and heatFullWidth cut to it, so the
// width the layout accepts is measured from the numbers the card actually
// draws rather than from every one it was handed.
const heatMaxNums = 3

// heatFields is what this layout draws when nothing is asked for. It is named
// here rather than written into the registry entry because heatFullWidth is
// measured from the labels of its numbers, and the registry entry is what
// carries that width.
var heatFields = []string{fieldSparkline, fieldContributions, fieldCommits, fieldPullRequests}

// heatFullWidth is the width at which the grid holds the whole year, and so
// the widest this layout has anything to draw at: the card's two paddings, a
// year of squares, the channel, and the column the default card's numbers ask
// for. It is the layout's MaxWidth.
//
// Every other layout takes maxWidth, which only catches a typo, because every
// other layout spreads the same content over whatever it is given. This one
// runs out: the collector keeps a year of daily counts, heatGridWeeks stops at
// fifty-two, and a card wider than this drew the year and then the empty
// quarter that this layout was rewritten to get rid of. Three hundred and nine
// units of it at twelve hundred, measured.
//
// Computed rather than typed, from the same labels the card sets, so a metric
// renamed to something longer moves this with it instead of leaving a figure
// that used to be right. An account whose numbers are wider than those labels
// needs a wider column, gets one, and draws a week or two fewer: the grid takes
// what is left, which is the rule everywhere else in this file.
//
// What it does NOT know is the field set. A card asked for fewer numbers, or
// for numbers with shorter labels, has a narrower column and so reaches the
// year before this width: -card-fields sparkline has no column at all, draws
// the year at 769, and by 891 has a hundred and twenty-two units of nothing on
// its right, measured. The bound is static because the registry is static, and
// making it follow the fields would mean a width this layout accepts by default
// being refused once a reader narrows them, which is a worse thing to hand him
// than some empty space at the end of a range he had to make two deliberate
// choices to reach. TestTheHeatmapGridTakesEveryWeekItHasRoomAndDataFor is the
// invariant that does hold over every field set: the grid is never smaller than
// the room allows unless it has already drawn the year.
var heatFullWidth = int(math.Ceil(2*ghPad + heatNumsGap +
	float64(heatWeeksMax)*heatPitch - (heatPitch - heatCell) +
	heatNumsWidth(heatNumbers(metricsOf(&Card{}, heatFields)))))

// heatNumbers is the numbers the card draws of the ones it was handed: the
// first heatMaxNums, because that is all the height beside the grid there is.
// One function for the drawing and for heatFullWidth, so the width the layout
// accepts cannot come to be measured from a number the card leaves out.
func heatNumbers(nums []metric) []metric {
	if len(nums) > heatMaxNums {
		return nums[:heatMaxNums]
	}
	return nums
}

// heatNumsWidth is the room the numbers beside the grid need: the widest of
// them measured against its own label, which on an ordinary account is the
// wider of the two. Measured rather than fixed, because everything this column
// does not take is the grid's, and a column sized for the longest label any
// card could carry is empty space on every card that does not carry it.
//
// The value is measured at the monospace advance and the label at the
// proportional one, because that is how each is drawn: the github family sets
// .big in the mono stack and .l in the sans one. Measuring a mono number the
// proportional way makes it up to a sixth narrower than it comes out, and here
// that sixth would be handed to the grid and then run off the card, which is
// what the minimum-width card of huge numbers used to do.
func heatNumsWidth(nums []metric) float64 {
	w := 0.0
	for _, m := range nums {
		w = math.Max(w, monoWidth(grouped(m.value), 20))
		w = math.Max(w, textWidth(m.long, 11))
	}
	return w
}

// heatGridWeeks is how many weeks of the calendar fit in what the width leaves
// once the numbers have their column, so the grid ends where the card does
// instead of stopping at a number somebody picked. The width is therefore the
// knob, and -card-width on the binary and card-width on the Action are how a
// reader turns it: sixteen weeks at the layout's MinWidth, twenty-three at the
// width it declares, and the whole year at heatFullWidth, which is its
// MaxWidth and the widest it accepts precisely because the year lands there.
//
// The last week spends a cell and not a whole pitch, because the gap that
// follows every other week is the card's right padding after the last one. The
// floor of one is arithmetic rather than design: no width this layout accepts
// comes near it, and the stagger divides by the count.
func heatGridWeeks(width, numsW float64) int {
	room := width - 2*ghPad
	if numsW > 0 {
		room -= heatNumsGap + numsW
	}
	weeks := int((room + heatPitch - heatCell) / heatPitch)
	return min(max(weeks, 1), heatWeeksMax)
}

// heatWeekBeats places the wave: one fade per week, left to right, the whole
// sweep taking heatSweep seconds however many weeks there are. One class per
// week and not one per cell, because a class per cell would be a keyframe
// block per cell in a document that has to stay small enough to serve from a
// README, and the seven squares of a week have nothing to say to each other
// anyway. Under off every class comes back empty and no beat is placed.
func heatWeekBeats(tl *timeline, weeks int) []string {
	out := make([]string, weeks)
	stagger := 0.0
	if weeks > 1 {
		stagger = heatSweep / float64(weeks-1)
	}
	for w := range out {
		out[w] = tl.add(effectFade, float64(w)*stagger, heatWave, "ease-out")
	}
	return out
}

// heatLevels buckets as many weeks of daily counts as the grid holds into
// GitHub's five levels. The sparkline is taken as daily counts ending today,
// so the last cell is today and a series shorter than the grid is padded with
// empty days at the front, which is what a young account or a short backfill
// draws. There is no clock here to name the weekdays, so the rows are days
// relative to today rather than Monday to Sunday.
func heatLevels(values []int, weeks int) []int {
	n := weeks * 7
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
	const band = 44.0
	// The numbers stacked to the right of the grid, cut to the height the grid
	// gives them. They are measured before anything is drawn because their
	// column is what decides how wide the grid may be.
	nums := heatNumbers(s.nums)
	numsW := heatNumsWidth(nums)
	weeks := heatGridWeeks(s.width, numsW)
	// The heading names the period the grid drew, so a card that draws no grid
	// names no period: the week count is a fact about the calendar on the card,
	// and there is no calendar on a card that was not asked for the sparkline.
	if s.has(fieldSparkline) {
		ghTitled(s, fmt.Sprintf("Contributions, last %d weeks", weeks))
	} else {
		ghTitled(s, "Contributions")
	}
	tl := newTimeline(s.motion)
	// The grid is the only thing that moves, so a card asked for no sparkline
	// places no beat: the wave has nothing to cross.
	beats := make([]string, weeks)
	if s.has(fieldSparkline) {
		beats = heatWeekBeats(tl, weeks)
	}
	var body strings.Builder

	gridW := float64(weeks)*heatPitch - (heatPitch - heatCell)
	gridH := 7*heatPitch - (heatPitch - heatCell)
	gx, gy := ghPad, band+18
	bottom := gy + gridH
	if s.has(fieldSparkline) {
		levels := heatLevels(c.Sparkline, weeks)
		for w := range weeks {
			for d := range 7 {
				fmt.Fprintf(&body, `<rect class="%s" x="%s" y="%s" width="%s" height="%s" rx="2"/>`+"\n",
					classes(heatLevelClass(levels[w*7+d]), beats[w]),
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

	// The numbers start where the grid ends, or at the card's own padding when
	// there is no grid: an indent the width of a grid nobody asked for would
	// leave the card empty on the side the grid was supposed to fill.
	nx := gx
	if s.has(fieldSparkline) {
		nx = gx + gridW + heatNumsGap
	}
	ny := gy + 18
	for i, m := range nums {
		y := ny + float64(i)*44
		text(&body, nx, y, "big", "start", monoFit(grouped(m.value), 20, s.width-ghPad-nx))
		text(&body, nx, y+16, "l", "start", fit(m.long, 11, s.width-ghPad-nx))
		bottom = math.Max(bottom, y+16)
	}
	height := math.Ceil(bottom + 20)

	openDoc(b, &githubFamily, s, height, describe(c, s), tl.css())
	githubFrame(b, s.width, height, band)
	githubHeader(b, s, c.Login, band, ghPad)
	b.WriteString(body.String())
}
