package render

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
)

// TestATimelineThatPlaysOnceEndsWhenItsLastBeatDoes pins the whole stylesheet
// of the simplest case, because every layout's once mode is this output: one
// cycle as long as the last beat, played one time.
func TestATimelineThatPlaysOnceEndsWhenItsLastBeatDoes(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedDefault)
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

// TestALoopNeverReplaysAReveal is the rule the whole feature now turns on. A
// reveal shows the reader something; replaying it takes that something away
// again, and a card in a README must not do that. So a timeline of reveals
// writes the same stylesheet under loop as under once, down to the byte, and
// the card a reader gets is the same card.
//
// The author's decision, 2026-09-16, after seeing the layouts move. It is also
// why the cycle no longer grows by a rest under loop: there is no second play
// to hold the card still before.
func TestALoopNeverReplaysAReveal(t *testing.T) {
	reveal := func(motion string) string {
		tl := newTimeline(motion, SpeedDefault)
		tl.add(effectFade, 0, 1.6, "ease-out")
		tl.add(effectDraw, 0.4, 1.2, "linear")
		tl.add(effectGrowX, 1.6, 0.9, "ease-out")
		return tl.css()
	}
	once, loop := reveal(MotionOnce), reveal(MotionLoop)
	if once != loop {
		t.Errorf("loop wrote a different stylesheet from once:\n%s\nwant\n%s", loop, once)
	}
	if strings.Contains(loop, "infinite") {
		t.Errorf("nothing a reveal does may repeat:\n%s", loop)
	}
	if !strings.Contains(once, ".m0{animation:m0 2.5s ease-out 1}") {
		t.Errorf("the cycle must end with the last beat:\n%s", once)
	}
}

// TestAStaggeredBeatWaitsInsideItsKeyframes is the reason the engine exists:
// animation-delay applies to the first iteration only, so a stagger written
// with it falls apart on the second pass of a loop.
func TestAStaggeredBeatWaitsInsideItsKeyframes(t *testing.T) {
	tl := newTimeline(MotionLoop, SpeedDefault)
	tl.add(effectFade, 0, 1, "ease-out")
	tl.add(effectFade, 1, 1, "ease-out")
	css := tl.css()
	if strings.Contains(css, "animation-delay") {
		t.Errorf("a stagger must not use animation-delay:\n%s", css)
	}
	if !strings.Contains(css, "@keyframes m1{0%,50%{opacity:0}100%{opacity:1}}") {
		t.Errorf("the second beat does not wait for its start inside the cycle:\n%s", css)
	}
}

// TestRevealAndFlashAreTheTwoHalvesOfACounter covers the step effects a
// counter is made of: an intermediate frame shows only during its beat and
// rests hidden, the final one rests visible and hides only until its start.
func TestRevealAndFlashAreTheTwoHalvesOfACounter(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedDefault)
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
	tl := newTimeline(MotionOnce, SpeedDefault)
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
	tl := newTimeline(MotionOff, SpeedDefault)
	if got := tl.add(effectFade, 0, 1, "ease-out"); got != "" {
		t.Errorf("add under off = %q, want no class", got)
	}
	if tl.moving() || tl.css() != "" {
		t.Errorf("off must not move and must write no css, got %q", tl.css())
	}
	if got := newTimeline(MotionOnce, SpeedDefault).css(); got != "" {
		t.Errorf("a timeline with no beats wrote %q", got)
	}
}

// TestABarGrowsFromNothingAndSettlesUntransformed covers the effect nothing
// else here uses: a grow places the box and the edge it scales from on the
// class, as static declarations, and leaves the element untransformed at the
// end of the cycle, which is the element's own style.
func TestABarGrowsFromNothingAndSettlesUntransformed(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedDefault)
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

// TestAGrowAtTheStartOfTheCycleWritesACleanFirstStop is the grow's half of the
// from == 0 case the reveal has its own test for: a beat that starts exactly
// when the cycle does writes one stop and not a range from zero to zero.
func TestAGrowAtTheStartOfTheCycleWritesACleanFirstStop(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedDefault)
	tl.add(effectGrowX, 0, 1, "ease-out")
	css := tl.css()
	if !strings.Contains(css, "@keyframes m0{0%{transform:scaleX(0)}100%{transform:scaleX(1)}}") {
		t.Errorf("css lacks the two-stop grow at 0:\n%s", css)
	}
	if strings.Contains(css, "0%,0%") {
		t.Errorf("a grow starting at 0 must not write a range of no length:\n%s", css)
	}
}

// TestAGrowingBarStaysGrownWhateverTheMotion is what a bar owes a reader: it
// reaches its width and stays there. It is a reveal, so loop asks nothing of
// it that once does not, and a bar that snapped back to nothing every few
// seconds is exactly the card the rule forbids.
func TestAGrowingBarStaysGrownWhateverTheMotion(t *testing.T) {
	for _, motion := range []string{MotionOnce, MotionLoop} {
		tl := newTimeline(motion, SpeedDefault)
		tl.add(effectGrowX, 0, 1.6, "ease-out")
		css := tl.css()
		for _, want := range []string{
			".m0{animation:m0 1.6s ease-out 1;transform-box:fill-box;transform-origin:left}",
			"@keyframes m0{0%{transform:scaleX(0)}100%{transform:scaleX(1)}}",
		} {
			if !strings.Contains(css, want) {
				t.Errorf("under %s the css lacks %q:\n%s", motion, want, css)
			}
		}
	}
}

// TestACoverTypesTextInAndSettlesWhereItWasDrawn covers the effect the
// terminal window is built on: the cover starts one width to the left, over
// the text, and ends untranslated, where the layout drew it and where it hides
// nothing. The steps are the beat's easing, one per character, which is what
// makes the text arrive a character at a time.
func TestACoverTypesTextInAndSettlesWhereItWasDrawn(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedDefault)
	tl.addShift(effectType, 0, 0.5, "steps(3)", 21.6)
	css := tl.css()
	for _, want := range []string{
		".m0{animation:m0 0.5s steps(3) 1}",
		"@keyframes m0{0%{transform:translateX(-21.6px)}100%{transform:translateX(0px)}}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q:\n%s", want, css)
		}
	}
	// It translates, so it needs neither a box to measure against nor an edge
	// to work from: a px on an SVG element is a user unit wherever it sits.
	if strings.Contains(css, "transform-box") {
		t.Errorf("a translation needs no transform box:\n%s", css)
	}
}

// TestACoverNeverGoesBackOverANumberItHasTyped is the rule read off the one
// effect that would break it most plainly. A cover put back over a number is a
// number taken away from a reader who has already read it, so typing never
// repeats: under loop the cover is written exactly as it is under once.
func TestACoverNeverGoesBackOverANumberItHasTyped(t *testing.T) {
	typed := func(motion string) string {
		tl := newTimeline(motion, SpeedDefault)
		tl.addShift(effectType, 0, 0.5, "steps(3)", 21.6)
		return tl.css()
	}
	css := typed(MotionLoop)
	if css != typed(MotionOnce) {
		t.Errorf("loop wrote a different cover from once:\n%s", css)
	}
	for _, want := range []string{
		".m0{animation:m0 0.5s steps(3) 1}",
		"@keyframes m0{0%{transform:translateX(-21.6px)}100%{transform:translateX(0px)}}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q:\n%s", want, css)
		}
	}
	// And a cover that starts part way into the cycle waits over its text
	// until its own beat, rather than uncovering with the one above it.
	staggered := newTimeline(MotionOnce, SpeedDefault)
	staggered.addShift(effectType, 0, 0.5, "steps(2)", 10)
	staggered.addShift(effectType, 0.5, 0.5, "steps(2)", 10)
	if want := "@keyframes m1{0%,50%{transform:translateX(-10px)}100%{transform:translateX(0px)}}"; !strings.Contains(staggered.css(), want) {
		t.Errorf("css lacks %q:\n%s", want, staggered.css())
	}
}

// TestABandSlidesByOneCopyAndHoldsThere is the effect the ticker scrolls with,
// and the half of it a loop needs: the band holds the shift it reached for the
// rest of the cycle instead of snapping back and waiting at the start.
func TestABandSlidesByOneCopyAndHoldsThere(t *testing.T) {
	once := newTimeline(MotionOnce, SpeedDefault)
	once.addShift(effectSlide, 0, 4, "linear", 500)
	if want := "@keyframes m0{0%{transform:translateX(0px)}100%{transform:translateX(-500px)}}"; !strings.Contains(once.css(), want) {
		t.Errorf("css lacks %q:\n%s", want, once.css())
	}
	// A band is continuous, so under loop it is the one thing that repeats,
	// and it repeats on its own turn with nothing added and nothing held: one
	// pass, then the next, which is what scrolling is.
	loop := newTimeline(MotionLoop, SpeedDefault)
	loop.addShift(effectSlide, 0, 4, "linear", 500)
	for _, want := range []string{
		".m0{animation:m0 4s linear infinite}",
		"@keyframes m0{0%{transform:translateX(0px)}100%{transform:translateX(-500px)}}",
	} {
		if !strings.Contains(loop.css(), want) {
			t.Errorf("css lacks %q:\n%s", want, loop.css())
		}
	}
}

// TestACursorBlinksThroughItsBeatAndEndsLit is what makes the terminal a card
// that settles: the one effect here with no natural end blinks a whole number
// of times inside its beat, is lit before it and is lit from its end to the
// end of the cycle, so the still card has a cursor in a state a reader could
// point at rather than half a one.
func TestACursorBlinksThroughItsBeatAndEndsLit(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedDefault)
	tl.add(effectBlink, 0, 1, "linear")
	css := tl.css()
	// One second is two blinks at blinkPeriod: lit, dark, lit, dark, lit.
	want := "@keyframes m0{0%,24.99%{opacity:1}25%,49.99%{opacity:0}50%,74.99%{opacity:1}75%,99.99%{opacity:0}100%{opacity:1}}"
	if !strings.Contains(css, want) {
		t.Errorf("css lacks %q:\n%s", want, css)
	}
	if !strings.HasSuffix(strings.TrimSpace(strings.Split(css, "@media")[0]), "100%{opacity:1}}") {
		t.Errorf("a blink must end lit:\n%s", css)
	}
	// A beat shorter than one blink still blinks once rather than not at all.
	short := newTimeline(MotionOnce, SpeedDefault)
	short.add(effectBlink, 0, 0.1, "linear")
	if got := strings.Count(short.css(), "{opacity:0}"); got != 1 {
		t.Errorf("a beat too short for a whole blink went dark %d times, want once:\n%s", got, short.css())
	}
	// Under a loop the cursor is the one thing that keeps going, and it keeps
	// going on its own cadence rather than inside the card's cycle: one turn,
	// lit for half of it and dark for the other half, repeated for ever. The
	// beat's own length is how long it blinks before a card that settles
	// settles, which a card that never settles has no use for.
	forever := newTimeline(MotionLoop, SpeedDefault)
	forever.add(effectBlink, 1.4, 1, "linear")
	for _, want := range []string{
		".m0{animation:m0 0.5s linear infinite}",
		"@keyframes m0{0%,49.99%{opacity:1}50%,100%{opacity:0}}",
	} {
		if !strings.Contains(forever.css(), want) {
			t.Errorf("css lacks %q:\n%s", want, forever.css())
		}
	}
	// The beat above was placed at 1.4 seconds and the one below at zero, and
	// they write the same thing: a beat that repeats for ever begins at once,
	// because waiting once before the first turn would take an
	// animation-delay and this engine writes none. A layout that needs a
	// continuous beat to wait needs that rule relaxed first; see period.
	fromTheStart := newTimeline(MotionLoop, SpeedDefault)
	fromTheStart.add(effectBlink, 0, 1, "linear")
	if forever.css() != fromTheStart.css() {
		t.Errorf("a continuous beat's start reached its keyframes:\n%s\nagainst\n%s", forever.css(), fromTheStart.css())
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

// TestTheDefaultSpeedIsExactlyTheEngineOwnDurations is the promise the whole
// option hangs on, at the one place it can be made structurally: the scale a
// card is drawn at is exactly one at the default, so scaled hands every beat
// back the duration it asked for and nothing is multiplied at all.
//
// Checked as an exact float comparison on purpose. A scale that came out at
// 0.9999999999999999 would draw cards that look the same and write different
// bytes, which is the failure this is here to catch.
func TestTheDefaultSpeedIsExactlyTheEngineOwnDurations(t *testing.T) {
	if got := motionScale(SpeedDefault); got != 1 {
		t.Errorf("motionScale(%v) = %v, want exactly 1", SpeedDefault, got)
	}
	tl := newTimeline(MotionOnce, SpeedDefault)
	for _, d := range []float64{0.05, 0.22, 1.4, 1.82, 11.44} {
		if got := tl.scaled(d); got != d {
			t.Errorf("scaled(%v) = %v at the default, want the duration untouched", d, got)
		}
	}
}

// TestSpeedReachesTheSameDistanceEitherWayOfTheDefault holds the curve to what
// a reader sliding a number between two ends expects: the ends are the reach
// the engine declares, a quarter below the default is as much slower as a
// quarter above it is faster, and nothing in between goes backwards.
func TestSpeedReachesTheSameDistanceEitherWayOfTheDefault(t *testing.T) {
	if got := motionScale(SpeedSlowest); got != speedReach {
		t.Errorf("motionScale(%v) = %v, want %v", SpeedSlowest, got, speedReach)
	}
	if got := motionScale(SpeedFastest); got != 1/speedReach {
		t.Errorf("motionScale(%v) = %v, want %v", SpeedFastest, got, 1/speedReach)
	}
	for i := range 101 {
		speed := float64(i) / 100
		if product := motionScale(speed) * motionScale(1-speed); math.Abs(product-1) > 1e-12 {
			t.Errorf("motionScale(%v) * motionScale(%v) = %v, want 1: the two halves of the "+
				"range must reach the same distance", speed, 1-speed, product)
		}
		if i == 0 {
			continue
		}
		if motionScale(speed) >= motionScale(speed-0.01) {
			t.Fatalf("motionScale(%v) is not shorter than motionScale(%v): a higher speed "+
				"has to be a faster card", speed, speed-0.01)
		}
	}
}

// TestSpeedChangesTheDurationAndNothingElse is what makes a speed a speed.
//
// A beat's keyframes are percentages of the cycle, so they say the same thing
// however long the cycle runs, and the stagger between two beats is inside
// them. If a speed touched them, a card at one end of the range would not be
// the same card played faster: it would be a different card.
func TestSpeedChangesTheDurationAndNothingElse(t *testing.T) {
	plays := func(speed float64) string {
		tl := newTimeline(MotionLoop, speed)
		tl.add(effectFade, 0, 1.6, "ease-out")
		tl.add(effectDraw, 0.4, 1.2, "linear")
		tl.addShift(effectSlide, 0, 8, "linear", 1120)
		tl.add(effectBlink, 2.2, 1, "linear")
		return tl.css()
	}
	duration := regexp.MustCompile(`animation:m\d+ [\d.]+s`)
	want := duration.ReplaceAllString(plays(SpeedDefault), "")
	for _, speed := range []float64{SpeedSlowest, 0.25, 0.75, SpeedFastest} {
		if got := duration.ReplaceAllString(plays(speed), ""); got != want {
			t.Errorf("speed %v rewrote more than the durations:\n%s\nwant\n%s", speed, got, want)
		}
	}
	// And the durations themselves move, in the direction the name promises:
	// the cycle the reveals share, the band's pass and the cursor's blink, the
	// last two on clocks of their own under loop.
	slow, fast := plays(SpeedSlowest), plays(SpeedFastest)
	for _, want := range []string{
		"animation:m0 16s ease-out 1", "animation:m2 16s linear infinite",
		"animation:m3 1s linear infinite",
	} {
		if !strings.Contains(slow, want) {
			t.Errorf("the slowest speed does not say %q:\n%s", want, slow)
		}
	}
	for _, want := range []string{
		"animation:m0 4s ease-out 1", "animation:m2 4s linear infinite",
		"animation:m3 0.25s linear infinite",
	} {
		if !strings.Contains(fast, want) {
			t.Errorf("the fastest speed does not say %q:\n%s", want, fast)
		}
	}
}

// TestTheSlowestSpeedStillAnimates is the misreading the option invites. Zero
// is the slow end of a range, not an off switch, and a card drawn there still
// carries every animation it has; MotionOff is the one that carries none.
func TestTheSlowestSpeedStillAnimates(t *testing.T) {
	tl := newTimeline(MotionOnce, SpeedSlowest)
	tl.add(effectFade, 0, 1.6, "ease-out")
	css := tl.css()
	if !strings.Contains(css, "animation:m0 3.2s ease-out 1") {
		t.Errorf("the slowest speed drew no animation:\n%s", css)
	}
	if still := newTimeline(MotionOff, SpeedSlowest); still.css() != "" {
		t.Errorf("motion off is what draws no animation:\n%s", still.css())
	}
}
