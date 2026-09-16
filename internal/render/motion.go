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
// animated layout did before there was a choice. off writes no animation at
// all.
//
// loop does not replay anything. An animation here reveals content, and
// replaying a reveal takes content that a reader has already been shown and
// hides it again, which is the one thing a card in a README must not do. So
// loop means: the reveal still plays once and settles, and whatever the card
// has that is continuous and destroys nothing keeps going for ever. Two
// layouts have such a thing, the terminal's cursor and the ticker's band, and
// they are the two the registry marks Loops. On every other layout loop draws
// the same card as once, to the byte.
//
// The author's decision, 2026-09-16, after seeing the layouts move.
const (
	MotionOnce = "once"
	MotionLoop = "loop"
	MotionOff  = "off"
)

// ErrMotion is returned for a motion this package does not know.
var ErrMotion = errors.New("render: unknown motion")

// effect is what a beat does to the elements that carry its class. Each one
// animates a property whose base value it knows, which is what lets a beat
// write its last keyframe explicitly and still end on the element's own style.
//
// effectSlide is the exception and says so again below: it ends translated by
// a whole copy of the content it moves, not on the element's own style, and it
// is the layout's job to make that the same picture. A layout that scrolls
// something which is not repeated end to end would settle somewhere the base
// style does not describe.
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
	//
	// It is the one effect here that does not end on the element's own style,
	// and the only one whose promise the engine cannot keep on its own: the
	// content has to repeat at exactly the beat's distance, and enough of it
	// has to be there that the untranslated state shows one copy and no more.
	// The layout owes both; see drawTicker.
	effectSlide
	// effectBlink is the terminal cursor: lit for half of each blinkPeriod,
	// dark for the other half, for as long as the beat lasts, and lit again
	// when it ends. Base: lit, which is what a still card shows and the one
	// state a cursor can settle in.
	effectBlink
)

// continuous reports whether an effect is motion the card can keep up for ever
// rather than a reveal that has to land. The line between the two is what
// loop means: a reveal shows content, so replaying it takes back something the
// reader has already been given, while a cursor that blinks and a band that
// scrolls put nothing on the card and take nothing off it. Only these two may
// run for ever, and a layout is marked Loops in the registry exactly when it
// has one of them.
func (e effect) continuous() bool {
	return e == effectSlide || e == effectBlink
}

// beat is one effect placed on the card's clock, in seconds. shift is how far
// the two effects that translate move, in user units; every other effect
// leaves it at zero.
type beat struct {
	effect     effect
	start, dur float64
	ease       string
	shift      float64
}

// period is how long one turn of a continuous beat takes, which is what it
// repeats on under loop. A band's turn is the pass it was given; a cursor's is
// the cadence it blinks at, and not the beat's own length, because dur is how
// long it blinks before a card that plays once settles, which a card that
// never settles has no use for.
//
// The beat's start is not used here and is not used by perpetual either: an
// animation that repeats for ever cannot wait once before the first turn
// unless it is given an animation-delay, and this engine writes none. A
// continuous beat placed at a non-zero start therefore begins at once under
// loop, and waits for its start only under once. Today's two are the terminal's
// cursor, which is meant to blink from the moment the window is drawn, and the
// ticker's band, which starts at zero anyway. A third one that has to wait
// needs the delay, and the rule against delays relaxed for continuous beats
// alone, where a delay means exactly what it says.
func (b beat) period() float64 {
	if b.effect == effectBlink {
		return blinkPeriod
	}
	return b.dur
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

// cycle is how long the card takes to tell its story: the end of its last
// beat, whatever the motion. It used to grow by a rest under loop, for the
// stillness between one replay and the next; nothing replays any more, so
// there is nothing to rest between and once and loop measure the same.
func (t *timeline) cycle() float64 {
	end := 0.0
	for _, b := range t.beats {
		end = max(end, b.start+b.dur)
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
	var b strings.Builder
	names := make([]string, len(t.beats))
	for i, bt := range t.beats {
		name := "m" + strconv.Itoa(i)
		names[i] = "." + name
		// A continuous beat under loop is the only thing that repeats, and it
		// repeats on a clock of its own: it is not part of the sequence the
		// card reveals itself in, it has no stagger to keep in step with, and
		// its period is the one its own effect has. Everything else plays once
		// over the cycle, under loop exactly as under once.
		if t.motion == MotionLoop && bt.effect.continuous() {
			fmt.Fprintf(&b, ".%s{animation:%s %ss %s infinite%s}\n", name, name, num(bt.period()), bt.ease, anchorCSS(bt.effect))
			fmt.Fprintf(&b, "@keyframes %s{%s}\n", name, perpetual(bt))
			continue
		}
		fmt.Fprintf(&b, ".%s{animation:%s %ss %s 1%s}\n", name, name, num(cycle), bt.ease, anchorCSS(bt.effect))
		fmt.Fprintf(&b, "@keyframes %s{%s}\n", name, keyframes(bt, cycle))
	}
	fmt.Fprintf(&b, "@media (prefers-reduced-motion:reduce){%s{animation:none}}\n", strings.Join(names, ","))
	return b.String()
}

// What a keyframe says, at the two stops every effect writes. A keyframe block
// is assembled here as a string, so a fragment spelled out wherever it is
// wanted is one chance per spelling to get it wrong, and a brace or a colon out
// of place is a rule the browser drops and an animation nobody ever sees.
//
// They read as the state an element is left in, because that is what a keyframe
// is: hidden and shown are the two ends of every effect that works on opacity,
// drawing and drawn the two ends of the one that dashes a stroke.
const (
	hidden  = "{opacity:0}"
	shown   = "{opacity:1}"
	drawing = "{stroke-dasharray:1;stroke-dashoffset:1}"
	drawn   = "{stroke-dasharray:1;stroke-dashoffset:0}"
)

// transformed is the state an effect that moves an element leaves it in. It
// sets the whole transform property, which is why an element that carries a
// transform of its own cannot wear one of these classes: see effectGrowX.
func transformed(to string) string { return "{transform:" + to + "}" }

// keyframes places one beat inside the cycle. A step effect ends its hidden
// stretch a hundredth of a percent before the visible one starts, so the
// change is a cut rather than a fade between two stops.
func keyframes(bt beat, cycle float64) string {
	from := 100 * bt.start / cycle
	to := 100 * (bt.start + bt.dur) / cycle
	switch bt.effect {
	case effectDraw:
		return stops(0, from) + drawing + stops(to, 100) + drawn
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
		return stops(0, from) + transformed("scaleX(0)") + stops(to, 100) + transformed("scaleX(1)")
	case effectType:
		return stops(0, from) + transformed(shiftedBy(-bt.shift)) +
			stops(to, 100) + transformed(shiftedBy(0))
	case effectSlide:
		return stops(0, from) + transformed(shiftedBy(0)) +
			stops(to, 100) + transformed(shiftedBy(-bt.shift))
	case effectBlink:
		return blinkFrames(from, to, bt.dur)
	default: // effectFade
		return stops(0, from) + hidden + stops(to, 100) + shown
	}
}

// perpetual is the keyframes of a continuous beat under loop: one turn, which
// the browser repeats for ever. There is no waiting stretch before it and no
// settled one after it, because there is no cycle for it to sit inside; it is
// the whole animation, and the beat's start is no part of it. See period.
func perpetual(bt beat) string {
	if bt.effect == effectBlink {
		// Lit for the first half of the turn and dark for the second, wrapping
		// straight back to lit. The base style is the lit cursor either way,
		// so a still renderer and a reader under prefers-reduced-motion get
		// the cursor this one is blinking.
		return stops(0, 50-0.01) + shown + stops(50, 100) + hidden
	}
	return "0%" + transformed(shiftedBy(0)) + "100%" + transformed(shiftedBy(-bt.shift))
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
	blinks := max(int(math.Round(dur/blinkPeriod)), 1)
	period := (to - from) / float64(blinks)
	var b strings.Builder
	b.WriteString(stops(0, from+period/2-0.01) + shown)
	for i := range blinks {
		off := from + float64(i)*period + period/2
		on := off + period/2
		end := on + period/2 - 0.01
		if i == blinks-1 {
			end = 100
		}
		b.WriteString(stops(off, on-0.01) + hidden)
		b.WriteString(stops(on, end) + shown)
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
