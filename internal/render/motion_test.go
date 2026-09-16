package render

import (
	"errors"
	"strings"
	"testing"
)

// TestATimelineThatPlaysOnceEndsWhenItsLastBeatDoes pins the whole stylesheet
// of the simplest case, because every layout's once mode is this output: one
// cycle as long as the last beat, played one time.
func TestATimelineThatPlaysOnceEndsWhenItsLastBeatDoes(t *testing.T) {
	tl := newTimeline(MotionOnce)
	area := tl.add(effectFade, 0, 1.6, "ease-out")
	line := tl.add(effectDraw, 0, 1.6, "ease-out")
	if area != "m0" || line != "m1" {
		t.Fatalf("classes = %q, %q, want m0 and m1 in the order they were added", area, line)
	}
	want := ".m0{animation:m0 1.6s ease-out 1}\n" +
		"@keyframes m0{0%{opacity:0}100%{opacity:1}}\n" +
		".m1{animation:m1 1.6s ease-out 1}\n" +
		"@keyframes m1{0%{stroke-dasharray:1;stroke-dashoffset:1}100%{stroke-dasharray:1;stroke-dashoffset:0}}\n" +
		"@media (prefers-reduced-motion:reduce){.m0,.m1{animation:none}}\n"
	if got := tl.css(); got != want {
		t.Errorf("css =\n%s\nwant\n%s", got, want)
	}
}

// TestALoopingTimelineRestsBeforeItPlaysAgain checks the two things a loop
// adds: a cycle longer than the animation by loopRest, and percentages that
// squeeze the beat into the front of it so the card holds still after.
func TestALoopingTimelineRestsBeforeItPlaysAgain(t *testing.T) {
	tl := newTimeline(MotionLoop)
	tl.add(effectFade, 0, 1.6, "ease-out")
	css := tl.css()
	for _, want := range []string{
		".m0{animation:m0 8.6s ease-out infinite}",
		"@keyframes m0{0%{opacity:0}18.6%,100%{opacity:1}}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q:\n%s", want, css)
		}
	}
}

// TestAStaggeredBeatWaitsInsideItsKeyframes is the reason the engine exists:
// animation-delay applies to the first iteration only, so a stagger written
// with it falls apart on the second pass of a loop.
func TestAStaggeredBeatWaitsInsideItsKeyframes(t *testing.T) {
	tl := newTimeline(MotionLoop)
	tl.add(effectFade, 0, 1, "ease-out")
	tl.add(effectFade, 1, 1, "ease-out")
	css := tl.css()
	if strings.Contains(css, "animation-delay") {
		t.Errorf("a stagger must not use animation-delay:\n%s", css)
	}
	if !strings.Contains(css, "@keyframes m1{0%,11.11%{opacity:0}22.22%,100%{opacity:1}}") {
		t.Errorf("the second beat does not wait for its start inside the cycle:\n%s", css)
	}
}

// TestRevealAndFlashAreTheTwoHalvesOfACounter covers the step effects a
// counter is made of: an intermediate frame shows only during its beat and
// rests hidden, the final one rests visible and hides only until its start.
func TestRevealAndFlashAreTheTwoHalvesOfACounter(t *testing.T) {
	tl := newTimeline(MotionOnce)
	tl.add(effectFlash, 0, 0.5, "linear")
	tl.add(effectFlash, 0.5, 0.5, "linear")
	tl.add(effectReveal, 1, 1, "linear")
	css := tl.css()
	for _, want := range []string{
		"@keyframes m0{0%,24.99%{opacity:1}25%,100%{opacity:0}}",
		"@keyframes m1{0%,24.99%{opacity:0}25%,49.99%{opacity:1}50%,100%{opacity:0}}",
		"@keyframes m2{0%,49.99%{opacity:0}50%,100%{opacity:1}}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q:\n%s", want, css)
		}
	}
}

// TestARevealAtTheStartOfTheCycleWritesACleanFirstStop covers the from == 0
// branch of keyframes: a reveal beat that starts exactly when the cycle does
// must not subtract 0.01% from zero and emit a negative percent stop.
func TestARevealAtTheStartOfTheCycleWritesACleanFirstStop(t *testing.T) {
	tl := newTimeline(MotionOnce)
	tl.add(effectReveal, 0, 1, "linear")
	css := tl.css()
	if strings.Contains(css, "-0.01%") {
		t.Errorf("a reveal starting at 0 must not write a negative stop:\n%s", css)
	}
	if !strings.Contains(css, "@keyframes m0{0%,100%{opacity:1}}") {
		t.Errorf("css lacks the two-stop reveal at 0:\n%s", css)
	}
}

// TestANilTimelineDoesNotMove exercises the nil receiver moving() checks
// before dereferencing: a layout that never got a timeline must still be
// askable whether it moves, and the answer is no.
func TestANilTimelineDoesNotMove(t *testing.T) {
	var tl *timeline
	if tl.moving() {
		t.Error("a nil timeline must not report moving")
	}
}

// TestACardThatDoesNotMoveCarriesNoMotion keeps off honest: no class on any
// element and not one byte of stylesheet.
func TestACardThatDoesNotMoveCarriesNoMotion(t *testing.T) {
	tl := newTimeline(MotionOff)
	if got := tl.add(effectFade, 0, 1, "ease-out"); got != "" {
		t.Errorf("add under off = %q, want no class", got)
	}
	if tl.moving() || tl.css() != "" {
		t.Errorf("off must not move and must write no css, got %q", tl.css())
	}
	if got := newTimeline(MotionOnce).css(); got != "" {
		t.Errorf("a timeline with no beats wrote %q", got)
	}
}

// TestABarGrowsFromNothingAndSettlesUntransformed covers the effect nothing
// else here uses: a grow places the box and the edge it scales from on the
// class, as static declarations, and leaves the element untransformed at the
// end of the cycle, which is the element's own style.
func TestABarGrowsFromNothingAndSettlesUntransformed(t *testing.T) {
	tl := newTimeline(MotionOnce)
	tl.add(effectFade, 0, 1, "linear")
	tl.add(effectGrowX, 1, 1, "ease-out")
	css := tl.css()
	for _, want := range []string{
		".m1{animation:m1 2s ease-out 1;transform-box:fill-box;transform-origin:left}",
		"@keyframes m1{0%,50%{transform:scaleX(0)}100%{transform:scaleX(1)}}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q:\n%s", want, css)
		}
	}
	// Only the effect that needs them carries them. A fade that anchored a
	// transform would be saying something about a property it never touches.
	if strings.Contains(css, ".m0{animation:m0 2s linear 1;transform-box") {
		t.Errorf("a fade must not carry a transform box:\n%s", css)
	}
}

func TestClassesSkipsTheEmptyParts(t *testing.T) {
	if got := classes("big", "", "cuz", ""); got != "big cuz" {
		t.Errorf("classes = %q, want %q", got, "big cuz")
	}
}

// TestAClassAttributeIsWrittenOnlyWhenThereIsAClass keeps a card that does not
// move free of the attributes a card that does needs: an element the motion
// named nothing on is written exactly as it was before there was motion.
func TestAClassAttributeIsWrittenOnlyWhenThereIsAClass(t *testing.T) {
	if got := classAttr("n", "m3"); got != ` class="n m3"` {
		t.Errorf("classAttr = %q", got)
	}
	if got := classAttr("", ""); got != "" {
		t.Errorf("classAttr with nothing to say = %q, want no attribute", got)
	}
}

// TestAnUnknownMotionIsAnErrorThatListsTheValidOnes matches how a theme, a
// layout and a field are refused, and an empty value is the default.
func TestAnUnknownMotionIsAnErrorThatListsTheValidOnes(t *testing.T) {
	c := sample()
	_, err := SVG(c, &Options{Layout: "sparkline-hero", Motion: "bounce"})
	if !errors.Is(err, ErrMotion) || !strings.Contains(err.Error(), "once, loop, off") {
		t.Errorf("err = %v, want ErrMotion listing once, loop, off", err)
	}
	for _, m := range []string{"", MotionOnce, MotionLoop, MotionOff} {
		if _, err = SVG(c, &Options{Layout: "summary", Motion: m}); err != nil {
			t.Errorf("motion %q on a layout that does not move: %v", m, err)
		}
	}
}
