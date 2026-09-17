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

// The ends and the middle of Options.Speed, which is how fast an animated
// layout plays.
//
// SpeedDefault is the pace every card had before there was a choice, and it
// is exactly that pace: the engine multiplies its own durations by one there
// and writes the same bytes it always wrote. Below it a card is slower, above
// it faster, and every layout scales together rather than each keeping a knob
// of its own, so the relationships this engine argues for hold at every
// speed.
//
// SpeedSlowest is the slowest animation, not a still card. A reader meeting a
// range that starts at zero will assume the opposite, so every place this is
// documented says it, and the refusal below says it too: MotionOff is what
// draws a card that does not move.
//
// The author's decision, 2026-09-17, on being shown the ticker: a speed the
// reader sets, where the current pace is the middle of the range.
const (
	SpeedSlowest = 0.0
	SpeedDefault = 0.5
	SpeedFastest = 1.0
)

// ErrSpeed is returned for an Options.Speed outside SpeedSlowest to
// SpeedFastest. Like ErrWidth it names both ends and what it got, because the
// number is typed at a command line and in a workflow's `with:` block.
var ErrSpeed = errors.New("render: speed out of range")

// speedReach is how far each end of the range goes: at SpeedSlowest a card's
// motion takes speedReach times as long as at the default, and at SpeedFastest
// it takes a speedReach-th of it.
//
// Two, and not more, because both ends bind at about the same place and they
// were measured rather than guessed. At the fast end the binding motion is the
// terminal's cursor, whose blink is the shortest cycle on any card at half a
// second and the one thing that runs for ever under MotionLoop: halved it is
// a cursor at four blinks a second, and anything beyond that is a strobe on a
// card nobody can pause. At the slow end it is the ticker, whose pass over the
// gallery's own content is 11.44 s, by far the longest here because a pass is
// as long as the band is wide: doubled it is the 22.87 s the card writes, and
// the next step out would put a band on a README that takes more than half a
// minute to come round. Everything else sits between 1.6 s and 2.3 s and has
// room either way, so it is these two that fix the range. A card still settles
// in 4.6 s at the slowest.
//
// CHANGING IT IS NOT FREE, AND NOT EVERY VALUE IS ALLOWED. The reach has to be
// a value for which speedPivot and speedPivot minus one are both exactly
// representable in binary, or motionScale stops returning exactly one at
// SpeedDefault and every animated card is drawn with different bytes. 2, 3,
// 1.5 and 4 qualify. 2.5 does not: it makes motionScale(0.5) return
// 1.0000000000000002. Four thirds written as a decimal does not either, at
// 0.9999999999999999. Neither is visible on a card and both move the whole
// gallery, which is why this paragraph is here and not in a commit message.
//
// A violation is caught at once and in two places, so nothing wrong can ship:
// TestTheDefaultSpeedIsExactlyTheEngineOwnDurations compares that value to one
// exactly, and `make check-gallery` finds all thirty committed cards changed.
// A reach that is allowed still fails seven pinned durations, six in
// TestSpeedChangesTheDurationAndNothingElse and one in
// TestTheSlowestSpeedStillAnimates, which are arithmetic to update and are
// deliberately spelled out rather than derived: a test that recomputed the
// expected seconds the way the engine does would agree with any arithmetic the
// engine had, including wrong arithmetic.
//
// The prose that states this number is gated too, by
// TestTheReachOfTheSpeedRangeIsWrittenAsTheEngineSetsIt in documented_test.go,
// which names the four pages that write it out in words.
const speedReach = 2.0

// speedPivot is where the curve below is centered so that SpeedDefault lands
// on exactly one. It is a constant expression, so what motionScale evaluates
// is (2 - speed) / (1 + speed), and both of those halves are 1.5 at the
// default. See the paragraph above for which reaches keep that true.
const speedPivot = speedReach / (speedReach - 1)

// motionScale is the multiple of its own durations the engine draws a card at.
// It is applied to one thing and one thing only, the seconds an animation is
// given, which is what makes a speed a speed: the keyframes inside it are
// percentages of the cycle and say the same thing however long the cycle runs.
//
// The curve is a ratio of two straight lines rather than the power of a ratio
// every other speed control is written as, for three reasons and in this
// order.
//
// It is exactly one at the default. (2 - 0.5) and (1 + 0.5) are both 1.5, both
// exact in binary, and a number divided by itself is one with no rounding to
// hope for. That is the promise this whole change hangs on and it is
// structural here rather than arithmetical: see scaled, which does not
// multiply at all when the scale is one.
//
// Read that as a property of these two particular numbers and not of the
// curve's shape. A ratio of two straight lines is what makes the identity
// reachable; it is speedPivot and speedPivot minus one both being exact that
// makes it hold. Another reach can lose it while the shape is unchanged, and
// speedReach says which ones do.
//
// It reaches the same distance either way. motionScale(s) * motionScale(1-s)
// is exactly one, so a quarter below the default is as much slower as a
// quarter above it is faster, which is what a reader sliding a number between
// two ends expects and what a power curve also gives.
//
// And it is arithmetic a machine cannot disagree about. There is no exponent
// and no logarithm, so nothing depends on a library routine that is assembly
// on one architecture and Go on another, and there is no multiply next to an
// add for a compiler to fuse: this repository has already been bitten by an
// arm64 fused multiply-add giving a different last digit from the same
// expression evaluated in two steps, and a card that renders differently on
// two runners is a card that produces a diff out of nothing.
func motionScale(speed float64) float64 {
	return (speedPivot - speed) / (speedPivot - 1 + speed)
}

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
	// scale is what the reader's speed works out to, from motionScale: the
	// multiple of its own durations this clock runs at. It is held rather
	// than the speed it came from because it is settled once and asked for
	// once per beat, and because one is the value scaled has to recognize.
	scale float64
	beats []beat
}

// newTimeline starts a card's clock. The classes it hands out, m0, m1 and on,
// are unique within one document only, so a card uses exactly one timeline:
// two in the same SVG would each start at m0 and restyle each other's beats.
//
// speed is Options.Speed as SVG settled it, already inside the range.
func newTimeline(motion string, speed float64) *timeline {
	return &timeline{motion: motion, scale: motionScale(speed)}
}

// scaled is a duration in seconds as the stylesheet writes it: what the beat
// asked for, at the speed the card was drawn at.
//
// The default returns the duration untouched, and that is the point of the
// branch rather than an optimization. A card at the default has to be the card
// this renderer drew before speed existed, to the byte, on every layout, in
// every motion and on every architecture; multiplying by a one that is exactly
// one would also do it, but only because IEEE says so about that particular
// value, and the guarantee is worth more when nothing is multiplied at all.
// Every other speed is one multiplication, on a value num then writes to two
// decimals.
func (t *timeline) scaled(d float64) float64 {
	if t.scale == 1 {
		return d
	}
	return d * t.scale
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
//
// In the engine's own seconds, which is what every beat here is placed in. The
// reader's speed is applied once, where the stylesheet writes the duration out;
// see scaled.
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
			fmt.Fprintf(&b, ".%s{animation:%s %ss %s infinite%s}\n", name, name, num(t.scaled(bt.period())), bt.ease, anchorCSS(bt.effect))
			fmt.Fprintf(&b, "@keyframes %s{%s}\n", name, perpetual(bt))
			continue
		}
		fmt.Fprintf(&b, ".%s{animation:%s %ss %s 1%s}\n", name, name, num(t.scaled(cycle)), bt.ease, anchorCSS(bt.effect))
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
