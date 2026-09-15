package render

import (
	"errors"
	"fmt"
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
)

// beat is one effect placed on the card's clock, in seconds.
type beat struct {
	effect     effect
	start, dur float64
	ease       string
}

// timeline is every beat a card plays, on one clock. All of them share the
// cycle's duration and differ only in where their keyframes sit inside it,
// because animation-delay applies to the first iteration alone: a stagger
// written with it comes apart on the second pass of a loop.
type timeline struct {
	motion string
	beats  []beat
}

func newTimeline(motion string) *timeline {
	return &timeline{motion: motion}
}

// add places a beat and returns the class to put on the elements it moves.
// The class is "" when the card does not move, so a layout can pass it
// through classes() without asking.
func (t *timeline) add(e effect, start, dur float64, ease string) string {
	if !t.moving() {
		return ""
	}
	t.beats = append(t.beats, beat{effect: e, start: start, dur: dur, ease: ease})
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
		fmt.Fprintf(&b, ".%s{animation:%s %ss %s %s}\n", name, name, num(cycle), bt.ease, count)
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
	default: // effectFade
		return stops(0, from) + hidden + stops(to, 100) + shown
	}
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
