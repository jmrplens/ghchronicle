package render

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The values Options.Motion takes.
//
// once plays the animation a single time and settles, which is what every
// animated layout did before there was a choice. loop plays it, holds the
// finished card still for loopRest and plays it again, so a README that shows
// the card moves for a few seconds at a time rather than all the time. off
// writes no animation at all.
const (
	MotionOnce = "once"
	MotionLoop = "loop"
	MotionOff  = "off"
)

// ErrMotion is returned for a motion this package does not know.
var ErrMotion = errors.New("render: unknown motion")

// loopRest is how long a looping card holds its finished state before it
// plays again, in seconds.
const loopRest = 7.0

// effect is what a beat does to the elements that carry its class. Each one
// animates a property whose base value it knows, which is what lets a beat
// write its last keyframe explicitly and still end on the element's own style.
type effect int

const (
	// effectFade takes an element from transparent to opacity 1. Its base
	// opacity must be 1: a translucent fill uses fill-opacity instead.
	effectFade effect = iota
	// effectDraw draws a stroke that carries pathLength="1". The dash exists
	// only inside the keyframes; the base style is the solid line.
	effectDraw
	// effectReveal hides an element until the beat starts and then shows it
	// at once. Base: visible.
	effectReveal
	// effectFlash shows an element only while its beat lasts. Base: hidden.
	effectFlash
	// effectGrowX scales an element up from nothing along its own x axis,
	// with its left edge fixed. Its base is the element untransformed, which
	// is what the last keyframe leaves it on; the scaling exists only inside
	// the keyframes. The class carries the two properties that give the
	// transform a box and an edge to work from, see anchorCSS.
	//
	// The element it goes on must carry no transform of its own: the
	// keyframes set the whole transform property, so a translate that placed
	// the element would be dropped for as long as the beat lasts and the
	// element would grow from the wrong place. An element that has to be
	// placed goes inside a group, and the class goes on the group.
	effectGrowX
	// effectType reveals text left to right, the way a terminal types it. It
	// goes not on the text but on a rectangle painted in the color of
	// whatever is behind it, drawn just past the end of the text and covering
	// nothing at all: that is the base state, the finished line. The
	// keyframes start the rectangle one shift to the left, where it covers
	// the text whole, and walk it back to where it was drawn.
	//
	// A rectangle that moves rather than a clipPath that grows, because the
	// width of a clipPath is not animatable by CSS in every engine, and
	// because a clip-path is referenced by url() and this document
	// references nothing. The steps are the beat's easing, steps(n), so the
	// text arrives a character at a time instead of sliding out.
	effectType
	// effectSlide moves an element left by the distance the beat carries and
	// leaves it there. A band whose content is repeated end to end uses it to
	// scroll: shifted by exactly one copy, the next copy stands where the
	// last one did, so the end of the beat and its start are the same
	// picture and the base state, untranslated, is one of them.
	effectSlide
	// effectBlink is the terminal cursor: lit for half of each blinkPeriod,
	// dark for the other half, for as long as the beat lasts, and lit again
	// when it ends. Base: lit, which is what a still card shows and the one
	// state a cursor can settle in.
	effectBlink
)

// beat is one effect placed on the card's clock, in seconds. shift is how far
// the two effects that translate move, in user units; every other effect
// leaves it at zero.
type beat struct {
	effect     effect
	start, dur float64
	ease       string
	shift      float64
}

// timeline is every beat a card plays, on one clock. All of them share the
// cycle's duration and differ only in where their keyframes sit inside it,
// because animation-delay applies to the first iteration alone: a stagger
// written with it comes apart on the second pass of a loop.
type timeline struct {
	motion string
	beats  []beat
}

// newTimeline starts a card's clock. The classes it hands out, m0, m1 and on,
// are unique within one document only, so a card uses exactly one timeline:
// two in the same SVG would each start at m0 and restyle each other's beats.
func newTimeline(motion string) *timeline {
	return &timeline{motion: motion}
}

// add places a beat and returns the class to put on the elements it moves.
// The class is "" when the card does not move, so a layout can pass it
// through classes() without asking.
func (t *timeline) add(e effect, start, dur float64, ease string) string {
	return t.addShift(e, start, dur, ease, 0)
}

// addShift is add for the two effects that translate, which need a distance
// the engine cannot work out for itself: how wide the number a rectangle
// types is, how wide one copy of a band that repeats. In user units, and
// always leftwards, which is the only direction either effect moves.
//
// A second constructor rather than a fifth argument on add, so the twelve
// calls that move nothing are not all made to say so.
func (t *timeline) addShift(e effect, start, dur float64, ease string, shift float64) string {
	if !t.moving() {
		return ""
	}
	t.beats = append(t.beats, beat{effect: e, start: start, dur: dur, ease: ease, shift: shift})
	return "m" + strconv.Itoa(len(t.beats)-1)
}

// moving reports whether the card animates at all. A layout with frames that
// only exist to be animated (a counter's intermediate values) skips drawing
// them when it does not.
func (t *timeline) moving() bool {
	return t != nil && t.motion != MotionOff
}

func (t *timeline) cycle() float64 {
	end := 0.0
	for _, b := range t.beats {
		end = max(end, b.start+b.dur)
	}
	if t.motion == MotionLoop {
		end += loopRest
	}
	return end
}

// css is the stylesheet for every beat added so far, and the reduced-motion
// block that names each of them, so no layout can forget one.
func (t *timeline) css() string {
	if !t.moving() || len(t.beats) == 0 {
		return ""
	}
	cycle := t.cycle()
	count := "1"
	if t.motion == MotionLoop {
		count = "infinite"
	}
	var b strings.Builder
	names := make([]string, len(t.beats))
	for i, bt := range t.beats {
		name := "m" + strconv.Itoa(i)
		names[i] = "." + name
		fmt.Fprintf(&b, ".%s{animation:%s %ss %s %s%s}\n", name, name, num(cycle), bt.ease, count, anchorCSS(bt.effect))
		fmt.Fprintf(&b, "@keyframes %s{%s}\n", name, keyframes(bt, cycle))
	}
	fmt.Fprintf(&b, "@media (prefers-reduced-motion:reduce){%s{animation:none}}\n", strings.Join(names, ","))
	return b.String()
}

// keyframes places one beat inside the cycle. A step effect ends its hidden
// stretch a hundredth of a percent before the visible one starts, so the
// change is a cut rather than a fade between two stops.
func keyframes(bt beat, cycle float64) string {
	from := 100 * bt.start / cycle
	to := 100 * (bt.start + bt.dur) / cycle
	const hidden, shown = "{opacity:0}", "{opacity:1}"
	switch bt.effect {
	case effectDraw:
		return stops(0, from) + "{stroke-dasharray:1;stroke-dashoffset:1}" +
			stops(to, 100) + "{stroke-dasharray:1;stroke-dashoffset:0}"
	case effectReveal:
		if from == 0 {
			return stops(0, 100) + shown
		}
		return stops(0, from-0.01) + hidden + stops(from, 100) + shown
	case effectFlash:
		var s string
		if from > 0 {
			s = stops(0, from-0.01) + hidden
		}
		return s + stops(from, to-0.01) + shown + stops(to, 100) + hidden
	case effectGrowX:
		return stops(0, from) + "{transform:scaleX(0)}" + stops(to, 100) + "{transform:scaleX(1)}"
	case effectType:
		return stops(0, from) + "{transform:" + shiftedBy(-bt.shift) + "}" +
			stops(to, 100) + "{transform:" + shiftedBy(0) + "}"
	case effectSlide:
		return stops(0, from) + "{transform:" + shiftedBy(0) + "}" +
			stops(to, 100) + "{transform:" + shiftedBy(-bt.shift) + "}"
	case effectBlink:
		return blinkFrames(from, to, bt.dur)
	default: // effectFade
		return stops(0, from) + hidden + stops(to, 100) + shown
	}
}

// shiftedBy writes one horizontal translation. The unit is px because a CSS
// transform takes no bare number, and px on an SVG element is one user unit,
// which is what every other coordinate in this document is written in.
func shiftedBy(by float64) string {
	return "translateX(" + num(by) + "px)"
}

// blinkPeriod is one blink of the cursor, in seconds: lit for the first half
// of it and dark for the second, which is the cadence a terminal has.
const blinkPeriod = 0.5

// blinkFrames is the square wave between two percentages of the cycle: the
// cursor lit before the beat, blinking through the beat's dur seconds and lit
// from its end to the end of the cycle, so the card settles on a cursor that
// is there. A beat too short for one whole blink still gets one.
//
// Written stop by stop rather than with a steps() easing, because the effect
// has to end lit whatever the beat's length works out to, and an easing is
// one function over the whole animation, including the stretches where the
// cursor only waits.
func blinkFrames(from, to, dur float64) string {
	const dark, lit = "{opacity:0}", "{opacity:1}"
	blinks := max(int(math.Round(dur/blinkPeriod)), 1)
	period := (to - from) / float64(blinks)
	var b strings.Builder
	b.WriteString(stops(0, from+period/2-0.01) + lit)
	for i := range blinks {
		off := from + float64(i)*period + period/2
		on := off + period/2
		end := on + period/2 - 0.01
		if i == blinks-1 {
			end = 100
		}
		b.WriteString(stops(off, on-0.01) + dark)
		b.WriteString(stops(on, end) + lit)
	}
	return b.String()
}

// anchorCSS is what a class needs beyond its animation before its effect has
// anything to work from. Only effectGrowX has one: a transform on an SVG
// element is measured against the user space origin unless transform-box says
// otherwise, so without these two a bar would grow from the left edge of the
// document rather than from its own. They are static declarations and not
// keyframes on purpose: they say where a transform applies, not that there is
// one, so they leave an element with no transform exactly where it was and
// cost nothing under prefers-reduced-motion.
func anchorCSS(e effect) string {
	if e == effectGrowX {
		return ";transform-box:fill-box;transform-origin:left"
	}
	return ""
}

// stops is the selector for a stretch of keyframes: "18.6%,100%", or a single
// stop when both ends are the same.
func stops(a, b float64) string {
	if num(a) == num(b) {
		return num(a) + "%"
	}
	return num(a) + "%," + num(b) + "%"
}

// classes joins the non-empty class names, so a class that motion did not
// produce leaves no stray space in the attribute.
func classes(parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// classAttr is a class attribute holding the non-empty parts, or nothing at
// all when there are none, so an element a card does not move carries no empty
// attribute it had no reason to grow.
func classAttr(parts ...string) string {
	c := classes(parts...)
	if c == "" {
		return ""
	}
	return ` class="` + c + `"`
}
