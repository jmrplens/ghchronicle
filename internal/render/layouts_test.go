package render

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestLayoutsRegistryHoldsTheThirteenConcepts(t *testing.T) {
	want := []string{
		"summary", "github-stats", "github-compact", "badge-row", "wide-banner",
		"sparkline-hero", "language-ring", "repo-list", "activity-heatmap", "animated-counters",
		"terminal", "ticker", "language-bars",
	}
	got := Layouts()
	if len(got) != len(want) {
		t.Fatalf("Layouts() has %d entries, want %d", len(got), len(want))
	}
	animated := map[string]bool{
		"github-stats": true, "wide-banner": true, "sparkline-hero": true,
		"language-ring": true, "activity-heatmap": true, "animated-counters": true,
		"terminal": true, "ticker": true, "language-bars": true,
	}
	// The two with something continuous to keep going: a cursor and a band.
	// Everything else only reveals, and a reveal is never replayed.
	loops := map[string]bool{"terminal": true, "ticker": true}
	for i, l := range got {
		if l.Name != want[i] {
			t.Errorf("layout %d = %q, want %q", i, l.Name, want[i])
		}
		checkRegisteredLayout(t, l, animated[l.Name], loops[l.Name])
	}
	// The registry must not be reachable through the copies.
	got[0].Fields[0] = "tampered"
	if Layouts()[0].Fields[0] == "tampered" {
		t.Error("Layouts() handed out the registry's own slice")
	}
}

// checkRegisteredLayout is what every entry of the registry owes, pulled out of
// the loop that walks it so that loop stays a list of names.
func checkRegisteredLayout(t *testing.T, l Layout, animated, loops bool) {
	t.Helper()
	if l.Family != "chronicle" && l.Family != "github" {
		t.Errorf("%s: family %q is neither chronicle nor github", l.Name, l.Family)
	}
	if l.Description == "" || strings.Contains(l.Description, "\n") {
		t.Errorf("%s: the description must be one line", l.Name)
	}
	if l.Animated != animated {
		t.Errorf("%s: animated = %v", l.Name, l.Animated)
	}
	if l.Loops != loops {
		t.Errorf("%s: loops = %v", l.Name, l.Loops)
	}
	if l.Loops && !l.Animated {
		t.Errorf("%s: a layout that loops has to move at all", l.Name)
	}
	if len(l.Fields) == 0 {
		t.Errorf("%s: no default fields", l.Name)
	}
	for _, f := range l.Fields {
		if !slices.Contains(l.Supports, f) {
			t.Errorf("%s: default field %q is not in its supported set", l.Name, f)
		}
	}
	for _, f := range l.Supports {
		if !slices.Contains(fieldVocabulary, f) {
			t.Errorf("%s: supports %q, which is not in the vocabulary", l.Name, f)
		}
	}
}

func TestFieldsVocabularyIsTheDocumentedFifteen(t *testing.T) {
	want := "stars forks followers repos contributions views visitors clones commits pull_requests reviews issues languages top_repos sparkline"
	if got := strings.Join(Fields(), " "); got != want {
		t.Fatalf("Fields() = %q", got)
	}
}

func TestFieldsResolveInOrderAndSkipWhatTheLayoutCannotShow(t *testing.T) {
	def, _ := findLayout("github-compact")
	got, err := resolveFields(def, []string{"forks", "languages", "stars", "forks", "top_repos"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "forks stars" {
		t.Fatalf("resolved %v: unsupported fields are skipped, duplicates dropped, order kept", got)
	}
	if defaults, _ := resolveFields(def, nil); strings.Join(defaults, " ") != strings.Join(def.Fields, " ") {
		t.Error("an empty request means the layout's default set")
	}
	_, err = resolveFields(def, []string{"stars", "starz"})
	if !errors.Is(err, ErrField) {
		t.Fatalf("an unknown field must be ErrField, got %v", err)
	}
	if !strings.Contains(err.Error(), `"starz"`) || !strings.Contains(err.Error(), "pull_requests") {
		t.Errorf("the error must name the offender and the valid set: %v", err)
	}
	if _, err = SVG(sample(), &Options{Fields: []string{"nope"}}); !errors.Is(err, ErrField) {
		t.Error("SVG must surface the field error")
	}
}

func TestUnknownLayoutIsAnErrorNamingTheValidOnes(t *testing.T) {
	_, err := SVG(sample(), &Options{Layout: "tarot"})
	if !errors.Is(err, ErrLayout) {
		t.Fatalf("got %v, want ErrLayout", err)
	}
	if !strings.Contains(err.Error(), "activity-heatmap") {
		t.Errorf("the error must list the registered layouts: %v", err)
	}
}

func TestSummaryHonorsFields(t *testing.T) {
	only := mustRender(t, sample(), &Options{Theme: "dark", Fields: []string{"stars", "languages"}})
	for _, gone := range []string{">Forks<", "<polyline", "TOP REPOSITORIES", "Followers"} {
		if strings.Contains(only, gone) {
			t.Errorf("%q drawn although it was not asked for", gone)
		}
	}
	if !strings.Contains(only, ">Stars<") || !strings.Contains(only, "LANGUAGES") {
		t.Error("the requested fields are missing")
	}
	if !strings.Contains(only, ">Go 46%<") {
		t.Error("the language legend is missing")
	}
}

func TestEveryLayoutDrawsEveryFieldItSupports(t *testing.T) {
	for _, l := range Layouts() {
		doc := mustRender(t, sample(), &Options{Theme: "dark", Layout: l.Name, Fields: l.Supports})
		if slices.Contains(l.Supports, fieldSparkline) && !strings.Contains(doc, "<polyline") && !strings.Contains(doc, `class="h`) {
			t.Errorf("%s: supports the sparkline but drew neither a line nor a grid", l.Name)
		}
		if slices.Contains(l.Supports, fieldTopRepos) && !strings.Contains(doc, "phonometry") {
			t.Errorf("%s: supports top_repos but drew no repository", l.Name)
		}
		if slices.Contains(l.Supports, fieldLanguages) && !strings.Contains(doc, languageColor("Go")) {
			t.Errorf("%s: supports languages but drew no language color", l.Name)
		}
		// The description spells out every number the card shows, so a
		// screen reader gets what the drawing gets.
		if !strings.Contains(doc, "283 stars") {
			t.Errorf("%s: the description lost the star count", l.Name)
		}
	}
}

var (
	keyframeName  = regexp.MustCompile(`@keyframes ([A-Za-z0-9_-]+)`)
	animationName = regexp.MustCompile(`animation(?:-name)?:([A-Za-z0-9_-]+)`)
)

// TestAnimatedLayoutsMoveOnlyAsTheyAreAsked renders every layout in the three
// motions. A layout not registered as animated carries no animation in any of
// them; an animated one plays once and settles, loops forever, or carries
// nothing, and in the two that move every animation has its keyframes and is
// switched off under reduced motion.
func TestAnimatedLayoutsMoveOnlyAsTheyAreAsked(t *testing.T) {
	c := sample()
	for _, l := range Layouts() {
		for _, motion := range []string{MotionOnce, MotionLoop, MotionOff} {
			doc := mustRender(t, c, &Options{Theme: "dark", Layout: l.Name, Motion: motion})
			checkLayoutMoves(t, l, motion, doc)
		}
	}
}

// checkLayoutMoves is what TestAnimatedLayoutsMoveOnlyAsTheyAreAsked asserts
// of one layout under one motion, pulled out of the loop so the loop itself
// stays readable: a layout not registered as animated carries no animation in
// any of them; an animated one plays once and settles, loops forever, or
// carries nothing, and in the two that move every animation has its
// keyframes and is switched off under reduced motion.
func checkLayoutMoves(t *testing.T, l Layout, motion, doc string) {
	t.Helper()
	moves := strings.Contains(doc, "animation") || strings.Contains(doc, "<animate")
	if !l.Animated || motion == MotionOff {
		if moves {
			t.Errorf("%s under %s carries animation", l.Name, motion)
		}
		return
	}
	if !moves {
		t.Errorf("%s under %s does not move", l.Name, motion)
		return
	}
	// Only a layout the registry marks Loops has anything that repeats, and
	// only under loop: everything else reveals content, and a reveal replayed
	// takes back what the reader was shown.
	if loops := strings.Contains(doc, "infinite"); loops != (motion == MotionLoop && l.Loops) {
		t.Errorf("%s under %s: infinite = %v, want %v", l.Name, motion, loops, motion == MotionLoop && l.Loops)
	}
	if !strings.Contains(doc, "@media (prefers-reduced-motion:reduce)") || !strings.Contains(doc, "animation:none") {
		t.Errorf("%s under %s must switch its animation off under prefers-reduced-motion", l.Name, motion)
	}
	if strings.Contains(doc, "animation-delay") {
		t.Errorf("%s under %s uses animation-delay, which a loop does not repeat", l.Name, motion)
	}
	for _, name := range animationsWithoutKeyframes(doc) {
		t.Errorf("%s under %s: animation %q has no @keyframes", l.Name, motion, name)
	}
	// Any stroke that draws itself, the sparkline's polyline and the ring's
	// arcs alike. The trigger is the markup and not the stylesheet it is about
	// to assert: a draw effect that stopped writing that last keyframe would
	// otherwise stop being checked instead of failing.
	if strings.Contains(doc, `pathLength="1"`) {
		checkLineDrawSettles(t, l.Name, doc)
	}
	checkReducedMotionNamesEveryClass(t, l.Name, motion, doc)
}

// checkReducedMotionNamesEveryClass is the promise the engine makes for every
// card: a class that animates is a class the prefers-reduced-motion block
// switches off. A layout cannot forget one, because it does not write the
// block; this is what proves the engine has not either.
func checkReducedMotionNamesEveryClass(t *testing.T, layout, motion, doc string) {
	t.Helper()
	m := reducedMotionBlock.FindStringSubmatch(doc)
	if m == nil {
		t.Errorf("%s under %s: no prefers-reduced-motion block", layout, motion)
		return
	}
	named := map[string]bool{}
	for cls := range strings.SplitSeq(m[1], ",") {
		named[strings.TrimPrefix(cls, ".")] = true
	}
	for _, am := range animatingClass.FindAllStringSubmatch(doc, -1) {
		if !named[am[1]] {
			t.Errorf("%s under %s: class %q animates but is not named in the reduced-motion block",
				layout, motion, am[1])
		}
	}
}

var (
	reducedMotionBlock = regexp.MustCompile(`@media \(prefers-reduced-motion:reduce\)\{([^}]+)\{animation:none\}\}`)
	animatingClass     = regexp.MustCompile(`\.(m\d+)\{animation:`)
)

// animationsWithoutKeyframes lists every animation the document names that
// no @keyframes in it defines.
func animationsWithoutKeyframes(doc string) []string {
	defined := map[string]bool{}
	for _, m := range keyframeName.FindAllStringSubmatch(doc, -1) {
		defined[m[1]] = true
	}
	var missing []string
	for _, m := range animationName.FindAllStringSubmatch(doc, -1) {
		if m[1] != "none" && !defined[m[1]] {
			missing = append(missing, m[1])
		}
	}
	return missing
}

// checkLineDrawSettles checks that every stroke that draws itself has its dash
// only inside the keyframes and that the last of them leaves it fully drawn:
// the base style is a solid stroke, which is what a renderer without animation
// shows. Every one of them, not one of them: the ring draws a stroke per
// language, and a check that passed on the first would have nothing to say
// about the other five.
func checkLineDrawSettles(t *testing.T, layout, doc string) {
	t.Helper()
	strokes := strings.Count(doc, `pathLength="1"`)
	settled := strings.Count(doc, "100%{stroke-dasharray:1;stroke-dashoffset:0}}")
	if settled != strokes {
		t.Errorf("%s: %d of the %d strokes that draw themselves end fully drawn",
			layout, settled, strokes)
	}
	if regexp.MustCompile(`\.m\d+\{stroke-dasharray`).MatchString(doc) {
		t.Errorf("%s: the dash must not be a base style", layout)
	}
}

// TestATranslucentAreaFadesInWithoutJumping guards the invariant the fade
// effect relies on: it ends on opacity 1, so an area that is only faintly
// filled has to get that from fill-opacity, or the last frame would flash it
// solid before the base style took over.
func TestATranslucentAreaFadesInWithoutJumping(t *testing.T) {
	for _, l := range Layouts() {
		doc := mustRender(t, sample(), &Options{Theme: "dark", Layout: l.Name})
		if regexp.MustCompile(`\.area\{([^}]*;)?opacity:`).MatchString(doc) {
			t.Errorf("%s: .area sets opacity; use fill-opacity", l.Name)
		}
	}
}

// TestAnimatedCardsAreByteIdenticalInEveryMotion is the determinism promise
// for the new option: a scheduled job commits nothing on a day with no change,
// whichever motion it renders.
func TestAnimatedCardsAreByteIdenticalInEveryMotion(t *testing.T) {
	for _, l := range Layouts() {
		for _, motion := range []string{MotionOnce, MotionLoop, MotionOff} {
			o := &Options{Theme: "light", Layout: l.Name, Motion: motion}
			first, second := mustRender(t, sample(), o), mustRender(t, sample(), o)
			if first != second {
				t.Errorf("%s under %s differs between two renders", l.Name, motion)
			}
		}
	}
}

// TestOnlyContinuousMotionRunsForEver is the feature's rule, read off every
// registered layout at once: a layout with nothing continuous draws the same
// bytes under loop as under once, and the two that have something continuous
// repeat exactly that one thing and nothing else.
//
// The author's decision, 2026-09-16: an animation that reveals content plays
// once and settles, because replaying it makes content a reader has already
// been shown disappear. Only motion that destroys nothing may run for ever.
func TestOnlyContinuousMotionRunsForEver(t *testing.T) {
	c := sample()
	for _, l := range Layouts() {
		once := mustRender(t, c, &Options{Theme: "dark", Layout: l.Name, Motion: MotionOnce})
		loop := mustRender(t, c, &Options{Theme: "dark", Layout: l.Name, Motion: MotionLoop})
		if !l.Loops {
			if loop != once {
				t.Errorf("%s has nothing to keep going, so loop must draw the card once draws: %s",
					l.Name, firstDifference(loop, once))
			}
			continue
		}
		// Exactly one class repeats, and everything else plays once, which is
		// the same "once" the other motion draws.
		forever := regexp.MustCompile(`\.(m\d+)\{animation:m\d+ [\d.]+s [^;}]* infinite`).FindAllStringSubmatch(loop, -1)
		if len(forever) != 1 {
			t.Errorf("%s under loop repeats %d classes, want the one thing it has that is continuous", l.Name, len(forever))
			continue
		}
		plays := len(regexp.MustCompile(`\.m\d+\{animation:m\d+ [\d.]+s [^;}]* 1[;}]`).FindAllString(loop, -1))
		// Every rule that names a keyframe block, which is one per beat; the
		// reduced-motion block says animation:none and names no keyframes.
		beats := len(regexp.MustCompile(`\.m\d+\{animation:m\d+ `).FindAllString(once, -1))
		if want := beats - 1; plays != want {
			t.Errorf("%s under loop plays %d beats once, want %d of its %d", l.Name, plays, want, beats)
		}
		if err := continuousClassIsTheRightOne(l.Name, loop, forever[0][1]); err != "" {
			t.Errorf("%s: %s", l.Name, err)
		}
		// And under reduced motion nothing moves, the perpetual class included.
		checkReducedMotionNamesEveryClass(t, l.Name, MotionLoop, loop)
		if !strings.Contains(loop, "."+forever[0][1]+",") && !strings.Contains(loop, ","+forever[0][1]+"{animation:none}") &&
			!strings.Contains(loop, "."+forever[0][1]+"{animation:none}") {
			t.Errorf("%s: the class that runs for ever must be switched off under prefers-reduced-motion", l.Name)
		}
	}
}

// continuousClassIsTheRightOne says whether the class a layout repeats is on
// the thing that is allowed to repeat: the terminal's cursor, which is the
// block at its prompt, and the ticker's band, which is the group holding the
// strip. Anything else wearing it would be content kept in motion.
func continuousClassIsTheRightOne(layout, doc, class string) string {
	switch layout {
	case "terminal":
		if !strings.Contains(doc, `<rect class="ok `+class+`"`) {
			return "the class that runs for ever is not the cursor's"
		}
		if strings.Count(doc, class+`"`) != 1 {
			return "the cursor's class is on more than the cursor"
		}
	case "ticker":
		if !strings.Contains(doc, `<g class="`+class+`">`) {
			return "the class that runs for ever is not the band's"
		}
	default:
		return "no layout but the terminal and the ticker has anything continuous"
	}
	return ""
}

// TestTheTerminalTypesOnceHoweverItIsAskedToMove is the rule at the layout
// that shows it best: the numbers type themselves in exactly once under loop,
// as under once, and what goes on for ever is the cursor and only the cursor.
func TestTheTerminalTypesOnceHoweverItIsAskedToMove(t *testing.T) {
	c := sample()
	once := mustRender(t, c, &Options{Theme: "dark", Layout: "terminal", Motion: MotionOnce})
	loop := mustRender(t, c, &Options{Theme: "dark", Layout: "terminal", Motion: MotionLoop})
	covers := regexp.MustCompile(`<rect class="bg mask m\d+"[^>]*>`).FindAllString(once, -1)
	for _, cover := range covers {
		if !strings.Contains(loop, cover) {
			t.Errorf("a cover moved between once and loop: %s", cover)
		}
	}
	// Every beat but the cursor's is written the same in both, and the two
	// documents differ only in the cursor's own two lines of stylesheet.
	blink := "m" + strconv.Itoa(len(covers))
	for line := range strings.SplitSeq(once, "\n") {
		if strings.HasPrefix(line, "."+blink+"{") || strings.HasPrefix(line, "@keyframes "+blink+"{") {
			continue
		}
		if !strings.Contains(loop, line) {
			t.Errorf("loop changed a line that has nothing to do with the cursor:\n%s", line)
		}
	}
	if !strings.Contains(loop, "."+blink+"{animation:"+blink+" "+num(blinkPeriod)+"s linear infinite}") {
		t.Errorf("the cursor must blink for ever under loop:\n%s", loop)
	}
	// It blinks on its own cadence, half lit and half dark, and the card it
	// settles to when it is not looping is the one with the cursor lit.
	if !strings.Contains(loop, "@keyframes "+blink+"{0%,49.99%{opacity:1}50%,100%{opacity:0}}") {
		t.Errorf("the perpetual blink is not one turn of lit and dark:\n%s", loop)
	}
	if !strings.Contains(once, "100%{opacity:1}}") {
		t.Error("played once, the cursor still settles lit")
	}
}

// TestAnimatedCountersEndOnTheRealValues reads the frames a renderer that
// ignores animation paints: the final frame of every number carries its real
// value and rests visible, the intermediate ones rest hidden, and under off
// the intermediate frames are not drawn at all.
func TestAnimatedCountersEndOnTheRealValues(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "animated-counters"})
	finals := regexp.MustCompile(`class="big cuz m\d+"[^>]*>([^<]+)<`).FindAllStringSubmatch(doc, -1)
	want := []string{"283", "70", "41", "42", "6.2k", "1.8k"}
	if len(finals) != len(want) {
		t.Fatalf("found %d final frames, want %d", len(finals), len(want))
	}
	for i, w := range want {
		if finals[i][1] != w {
			t.Errorf("final frame %d = %q, want %q", i, finals[i][1], w)
		}
	}
	if !strings.Contains(doc, ".cuf{opacity:0}") || strings.Contains(doc, ".cuz{opacity:0") {
		t.Error("intermediate frames must rest hidden and final frames visible")
	}
	if got := strings.Count(doc, `class="big cuf m`); got != counterFrames*len(want) {
		t.Errorf("drew %d intermediate frames, want %d", got, counterFrames*len(want))
	}

	still := mustRender(t, c, &Options{Theme: "dark", Layout: "animated-counters", Motion: MotionOff})
	if strings.Contains(still, "cuf") {
		t.Error("under off the intermediate frames are dead weight and must not be drawn")
	}
	// And neither is the marker on the number they would have sat above: cuz
	// says "this is the value a count lands on", and under off no count runs.
	// It used to be written whatever the motion, which is a class off promised
	// not to write.
	if strings.Contains(still, "cuz") {
		t.Error("under off there is no count to mark the end of")
	}
	if got := len(regexp.MustCompile(`class="big"[^>]*>`).FindAllString(still, -1)); got != len(want) {
		t.Errorf("under off drew %d numbers, want %d", got, len(want))
	}
	if counterValue(1000, 0, 16) != 0 || counterValue(1000, 16, 16) != 1000 {
		t.Error("a counter starts at zero and lands exactly on its value")
	}
}

// TestAnimatedCountersHonourReducedMotion checks the property the shared
// motionCSS used to guarantee for the counters before Task 2: every class
// that carries an animation is one the timeline named in its
// prefers-reduced-motion block, under both once and loop. Before this task
// the counters wrote their own CSS by hand and were not covered by that
// block at all.
func TestAnimatedCountersHonourReducedMotion(t *testing.T) {
	c := sample()
	for _, motion := range []string{MotionOnce, MotionLoop} {
		doc := mustRender(t, c, &Options{Theme: "dark", Layout: "animated-counters", Motion: motion})
		checkReducedMotionNamesEveryClass(t, "animated-counters", motion, doc)
	}
}

// TestTheHeatmapWaveIsOneClassPerWeek pins the shape of the wave: twelve
// beats and not eighty-four, the seven squares of a week sharing one, and the
// key beside the grid left out of it, because it is a legend for the ramp and
// not a week of the calendar.
func TestTheHeatmapWaveIsOneClassPerWeek(t *testing.T) {
	doc := mustRender(t, sample(), &Options{Theme: "dark", Layout: "activity-heatmap"})
	if got := strings.Count(doc, "@keyframes m"); got != heatWeeks {
		t.Errorf("the grid animates %d classes, want one per week, %d", got, heatWeeks)
	}
	for w := range heatWeeks {
		cls := "m" + strconv.Itoa(w)
		if got := len(regexp.MustCompile(`<rect class="h\d `+cls+`"`).FindAllString(doc, -1)); got != 7 {
			t.Errorf("week %d is on %d squares, want the seven days of a week", w, got)
		}
	}
	// The key is the five squares after the grid's eighty-four, and it stands
	// still.
	keys := regexp.MustCompile(`<rect class="h\d" `).FindAllString(doc, -1)
	if len(keys) != 5 {
		t.Errorf("%d squares carry a level and no week, want the five of the key", len(keys))
	}
	// Under off the squares are written exactly as they were before the wave
	// existed: a level and nothing else.
	still := mustRender(t, sample(), &Options{Theme: "dark", Layout: "activity-heatmap", Motion: MotionOff})
	if n := len(regexp.MustCompile(`<rect class="h\d" `).FindAllString(still, -1)); n != heatWeeks*7+5 {
		t.Errorf("under off %d squares carry a bare level, want %d", n, heatWeeks*7+5)
	}
}

// TestTheRingDrawsSliceBySliceAndSettlesWhole is the ring's half of the rule
// every animated card obeys: each slice is its own arc, so its base style is
// the finished slice, the dash that draws it exists only in the keyframes, and
// the slices take the clock in the order they are drawn with the legend last.
func TestTheRingDrawsSliceBySliceAndSettlesWhole(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "language-ring"})
	slices := regexp.MustCompile(`<path class="(m\d+)" pathLength="1"`).FindAllStringSubmatch(doc, -1)
	langs := rankLanguages(c.Languages, maxLanguages)
	if len(slices) != len(langs) {
		t.Fatalf("%d slices draw themselves, want one per language, %d", len(slices), len(langs))
	}
	for i, m := range slices {
		if want := "m" + strconv.Itoa(i); m[1] != want {
			t.Errorf("slice %d plays %q, want %q: the slices take the clock in order", i, m[1], want)
		}
	}
	// The legend follows the last slice, which is the last beat there is.
	legend := "m" + strconv.Itoa(len(langs))
	if !strings.Contains(doc, ` class="n `+legend+`"`) || !strings.Contains(doc, ` class="`+legend+`" cx=`) {
		t.Errorf("the legend's names and dots must all play %s:\n%s", legend, doc)
	}
	if strings.Contains(doc, "stroke-dasharray=") {
		t.Error("a slice's base style is the finished arc, with no dash in it")
	}
	// Under off a slice is an arc and nothing else: no class, and no
	// pathLength placed for an animation that is not there.
	still := mustRender(t, c, &Options{Theme: "dark", Layout: "language-ring", Motion: MotionOff})
	if strings.Contains(still, "pathLength") {
		t.Error("under off the ring carries no pathLength, placed for an animation that is not there")
	}
	// An arc is the only path here that starts with a move; the frame's band
	// is the other one, and it carries a class.
	if n := strings.Count(still, `<path d="M`); n != len(langs) {
		t.Errorf("under off drew %d arcs, want %d, each with no class of its own", n, len(langs))
	}
}

// TestTheShareBarGrowsAsOneAndSettlesAtItsFullWidth pins what the new effect
// is for: the whole bar scales from its own left edge, not each segment from
// its own, its legend arrives with it rather than standing beside a bar that
// is not there yet, and a card that does not move is written without the
// group.
func TestTheShareBarGrowsAsOneAndSettlesAtItsFullWidth(t *testing.T) {
	doc := mustRender(t, sample(), &Options{Theme: "dark", Layout: "github-stats"})
	grow := regexp.MustCompile(`<g class="(m\d+)">`).FindStringSubmatch(doc)
	if grow == nil {
		t.Fatalf("the share bar is not wrapped in a group that grows:\n%s", doc)
	}
	checkTheLegendWaitsForItsBar(t, doc, grow[1])
	for _, want := range []string{
		"." + grow[1] + "{animation:" + grow[1],
		"transform-box:fill-box;transform-origin:left",
		"@keyframes " + grow[1] + "{",
		"100%{transform:scaleX(1)}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the bar's growth lacks %q", want)
		}
	}
	if strings.Contains(doc, `<rect class="m`) {
		t.Error("the segments must not scale one by one: the gaps would grow with them")
	}
	still := mustRender(t, sample(), &Options{Theme: "dark", Layout: "github-stats", Motion: MotionOff})
	if strings.Contains(still, "<g ") || strings.Contains(still, "</g>") {
		t.Error("under off the bar needs no group to grow")
	}
	// The other layout that draws a share bar draws it as it always did.
	summary := mustRender(t, sample(), &Options{Theme: "dark", Layout: "summary"})
	if strings.Contains(summary, "<g ") {
		t.Error("summary does not move, and its bar carries no group")
	}
	// Asked for languages it has none of, the card draws the empty placeholder
	// track, which has no width to grow into. A beat for it would style a bar
	// nobody can see grow, and a legend of no entries.
	empty := mustRender(t, &Card{Login: "someone"}, &Options{Theme: "dark", Layout: "github-stats", Fields: []string{fieldLanguages}})
	if strings.Contains(empty, "<g ") || strings.Contains(empty, "animation") {
		t.Errorf("a card with no language to show must place no beat for its bar:\n%s", empty)
	}
}

// checkTheLegendWaitsForItsBar is the timing the two share one beat window
// for: the legend's dots, names and percentages fade in over exactly the
// stretch the bar grows across.
func checkTheLegendWaitsForItsBar(t *testing.T, doc, grow string) {
	t.Helper()
	legend := regexp.MustCompile(` class="n (m\d+)"`).FindStringSubmatch(doc)
	if legend == nil {
		t.Fatalf("the language legend does not fade in with the bar:\n%s", doc)
	}
	for _, want := range []string{
		` class="` + legend[1] + `" cx=`,
		` class="c ` + legend[1] + `"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the legend plays %s on its names but not on %q", legend[1], want)
		}
	}
	// Same stretch of the cycle: the two keyframe blocks open and close on the
	// same percentages, which is what "with the bar" means.
	stops := func(name string) string {
		m := regexp.MustCompile(`@keyframes ` + name + `\{([0-9.%,]+)\{[a-z-]+:[^}]*\}([0-9.%,]+)\{`).FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("no readable keyframes for %s in:\n%s", name, doc)
		}
		return m[1] + " -> " + m[2]
	}
	if got, want := stops(legend[1]), stops(grow); got != want {
		t.Errorf("the legend fades over %s, the bar grows over %s", got, want)
	}
}

// TestGithubStatsCountsItsNumbersWithTheCountersOwnFrames is the reuse the
// two counting layouts were meant to share: the same frame count, the same
// eased values and the same two marker classes, from one implementation.
func TestGithubStatsCountsItsNumbersWithTheCountersOwnFrames(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "github-stats"})
	nums := metricsOf(c, mustLayout(t, "github-stats").Fields)
	if got := strings.Count(doc, `class="v cuf m`); got != counterFrames*len(nums) {
		t.Errorf("drew %d intermediate frames, want %d", got, counterFrames*len(nums))
	}
	// The github family writes its numbers in full, so the frames do too.
	if !strings.Contains(doc, ".cuf{opacity:0}") {
		t.Error("the intermediate frames must rest hidden")
	}
	if !strings.Contains(doc, `class="v cuz m`) || !strings.Contains(doc, ">"+grouped(nums[0].value)+"<") {
		t.Error("the settled number is the real value, grouped as the github family writes it")
	}
	still := mustRender(t, c, &Options{Theme: "dark", Layout: "github-stats", Motion: MotionOff})
	if strings.Contains(still, "cuf") || strings.Contains(still, "cuz") {
		t.Error("under off there are no frames and nothing marks the end of a count")
	}
	if got := strings.Count(still, `class="v"`); got != len(nums) {
		t.Errorf("under off drew %d numbers, want %d", got, len(nums))
	}
}

// TestTheTerminalTypesEveryNumberAndSettlesOnACursor is the window's half of
// the rule every animated card obeys. The number is written once, whole, and
// what moves is the rectangle over it: a cover in the card's own background
// color, resting past the end of the text where it hides nothing, so the
// settled card is the finished line. The cursor is a block that is there
// whatever the motion, because a card has to settle on a cursor in a state
// somebody could point at, and blinking is a beat over it rather than the
// thing that draws it.
func TestTheTerminalTypesEveryNumberAndSettlesOnACursor(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "terminal"})
	nums := metricsOf(c, mustLayout(t, "terminal").Fields)
	covers := regexp.MustCompile(`<rect class="bg mask (m\d+)" `).FindAllStringSubmatch(doc, -1)
	if want := len(nums) + len(rank(c.TopRepos, defaultMaxRepos)); len(covers) != want {
		t.Fatalf("%d numbers type themselves in, want one per line of output, %d", len(covers), want)
	}
	for i, m := range covers {
		if got := "m" + strconv.Itoa(i); m[1] != got {
			t.Errorf("line %d types with %q, want %q: the lines take the clock in order", i, m[1], got)
		}
	}
	// The cover is a plain fill. .bg is the only class that carries the card's
	// own background color in both themes, and it is also the card's border,
	// so the layout takes the border off it.
	if !strings.Contains(doc, ".bg.mask{stroke:none}") {
		t.Error("a cover painted with .bg would draw the card's border across the line")
	}
	// Two classes in the selector, so the rule wins on specificity rather than
	// on openDoc writing the palette before a layout's own rules.
	if strings.Contains(doc, "\n.mask{") {
		t.Error("the rule must be .bg.mask, or it only wins by being written second")
	}
	// Every number is written once and in full: the cover moves, the text does
	// not, so a renderer that ignores animation reads the whole card.
	for _, m := range nums {
		if got := strings.Count(doc, ">"+grouped(m.value)+"<"); got != 1 {
			t.Errorf("%s is written %d times, want once", m.key, got)
		}
	}
	// The cursor blinks last of all, and its keyframes end lit.
	blink := "m" + strconv.Itoa(len(covers))
	if !strings.Contains(doc, `<rect class="ok `+blink+`"`) {
		t.Errorf("the cursor does not play %s, the last beat there is:\n%s", blink, doc)
	}
	if !strings.Contains(doc, "@keyframes "+blink+"{") || !strings.Contains(doc, "100%{opacity:1}}") {
		t.Error("the cursor must end its blink lit, which is the state the card settles in")
	}

	still := mustRender(t, c, &Options{Theme: "dark", Layout: "terminal", Motion: MotionOff})
	if strings.Contains(still, "mask") {
		t.Error("under off nothing types, so no cover and no rule for one is written")
	}
	if !strings.Contains(still, `<rect class="ok" `) {
		t.Error("a card that does not move still rests at a prompt with a cursor on it")
	}
}

// TestTheTerminalKeepsHalfOfEveryLineClearForTheCoverThatTypesIt pins the one
// geometric constraint the typing puts on the layout: a cover is slid off the
// text it hides, so a number needs as much clear room to its right as it takes
// itself, or it would still be covered when its beat ended. The card is drawn
// at its narrowest, where the constraint bites first.
func TestTheTerminalKeepsHalfOfEveryLineClearForTheCoverThatTypesIt(t *testing.T) {
	def, _ := findLayout("terminal")
	for _, width := range []float64{float64(def.minWidth), defaultWidth, 900} {
		col := termLayout(width)
		right := col.valueX + 2*col.valueRoom + 3*termSlop
		if right > width-termPad {
			t.Errorf("at %v the cover rests at %v, past the window's own margin at %v",
				width, right, width-termPad)
		}
		if col.valueRoom < monoWidth("000,000", termFont) {
			t.Errorf("at %v a number has room for %v, less than six digits and a separator",
				width, col.valueRoom)
		}
	}
	// And the cover really is wider than the text it has to hide.
	doc := mustRender(t, sample(), &Options{Theme: "dark", Layout: "terminal"})
	cover := regexp.MustCompile(`<rect class="bg mask m0" x="([\d.]+)" y="[\d.]+" width="([\d.]+)"`).FindStringSubmatch(doc)
	if cover == nil {
		t.Fatalf("no cover on the first line:\n%s", doc)
	}
	x, _ := strconv.ParseFloat(cover[1], 64)
	w, _ := strconv.ParseFloat(cover[2], 64)
	col := termLayout(defaultWidth)
	text := monoWidth(grouped(sample().Stars), termFont)
	// Where the keyframes put the cover at the start of the beat, one whole
	// width to the left of where it was drawn. It has to reach past both ends
	// of the number by the slop the layout allows for a font it cannot measure.
	const near = 0.01
	if start := x - w; start > col.valueX-termSlop+near || start+w < col.valueX+text+termSlop-near {
		t.Errorf("a cover %v wide starting at %v does not hide %v of text from %v",
			w, start, text, col.valueX)
	}
}

// TestTheTickerScrollsByExactlyOneCopyOfItsContent is what makes the band's
// loop have no seam: the strip holds the same pills over and over, and the
// beat shifts it by exactly one copy, so the picture at the end of a pass is
// the picture at its start. It also covers what a still card is left with,
// which is one copy and no group to move it.
func TestTheTickerScrollsByExactlyOneCopyOfItsContent(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "ticker"})
	scroll := regexp.MustCompile(`<g class="(m\d+)">`).FindStringSubmatch(doc)
	if scroll == nil {
		t.Fatalf("the strip is not wrapped in a group that scrolls:\n%s", doc)
	}
	shift := regexp.MustCompile(`100%\{transform:translateX\(-([\d.]+)px\)\}`).FindStringSubmatch(doc)
	if shift == nil {
		t.Fatalf("the band's keyframes do not end shifted:\n%s", doc)
	}
	by, _ := strconv.ParseFloat(shift[1], 64)
	// The pills of one copy, measured the way the layout measures them.
	var s spec
	s.width, s.fields = 800, mustLayout(t, "ticker").Fields
	s.nums, s.repos = metricsOf(c, s.fields), rank(c.TopRepos, defaultMaxRepos)
	strip := 0.0
	for _, p := range tickerPills(&s) {
		strip += p.width + tickGap
	}
	// A whole number of user units, which is what keeps the seam on the pixel
	// grid, and never less than the content it has to carry past the edge.
	if by != math.Ceil(strip) {
		t.Errorf("the band shifts by %v, one copy of its content rounds to %v: the seam would jump by the difference", by, math.Ceil(strip))
	}
	if by != math.Trunc(by) {
		t.Errorf("the band shifts by %v, which is not a whole pixel: a browser rasterizes the seam a pixel out", by)
	}
	strip = math.Ceil(strip)
	// Every pill of the first copy appears again exactly one copy further on,
	// which is what standing in for it at the seam means.
	first := regexp.MustCompile(`<rect class="track" x="0" `)
	if !first.MatchString(doc) {
		t.Errorf("the first pill does not start the strip:\n%s", doc)
	}
	if !strings.Contains(doc, `<rect class="track" x="`+num(strip)+`" `) {
		t.Errorf("no pill stands one copy on at %s, so the seam has a hole", num(strip))
	}
	// And one copy is never narrower than the band, whatever the content
	// measures, which is what keeps the copy behind it off the resting card.
	if by < tickerBand(800) {
		t.Errorf("one copy is %v and the band is %v", by, tickerBand(800))
	}
	// The strip is cut off by a viewport of its own rather than by a clip path,
	// which would be reached through url(), and that viewport is inset by the
	// family's padding, so the first pill rests under the title rather than
	// against the card's border.
	if !strings.Contains(doc, `<svg x="`+num(pad)+`" y="`+num(tickBandY)+`" width="`+num(tickerBand(800))+`"`) {
		t.Errorf("the band needs a viewport of its own, inset by the padding:\n%s", doc)
	}

	still := mustRender(t, c, &Options{Theme: "dark", Layout: "ticker", Motion: MotionOff})
	if strings.Contains(still, "<g ") {
		t.Error("under off there is no group, because there is nothing to move")
	}
	if got := strings.Count(still, `<rect class="track"`); got != len(tickerPills(&s)) {
		t.Errorf("under off the band draws %d pills, want one copy of %d: the rest would never scroll in",
			got, len(tickerPills(&s)))
	}
	// A band asked for nothing it has says so, and places no beat: there is
	// no content to scroll past the edge.
	empty := mustRender(t, &Card{Login: "someone"}, &Options{Theme: "dark", Layout: "ticker", Fields: []string{fieldTopRepos}})
	if strings.Contains(empty, "animation") || !strings.Contains(empty, "Nothing to show") {
		t.Errorf("an empty band must say so and place no beat:\n%s", empty)
	}
}

// TestATickerShorterThanItsBandStillRestsOnOneCopy is the rule the whole
// package is built on, for the one layout that can break it without anybody
// looking: the resting card, which is what a still renderer draws and what a
// reader under prefers-reduced-motion is left with, has to be the finished
// card and not the content listed twice.
//
// The copies exist to be scrolled into view, and they are laid out one strip
// apart. A strip narrower than the band therefore puts the second copy on
// screen while the group is untranslated, which is exactly what five metrics
// and no repositories used to do: fifteen pills under once, five under off.
// One copy is padded out to the band, so the copy behind it starts at the far
// edge or past it.
//
// A table because the property is general in the width and in how much content
// there is, and the bug had one shape. The rows bound it: the default width,
// the layout's narrowest, and a band with a single pill in it.
func TestATickerShorterThanItsBandStillRestsOnOneCopy(t *testing.T) {
	def, _ := findLayout("ticker")
	for _, tc := range []struct {
		name   string
		width  int
		fields []string
	}{
		{"five metrics at the default width", def.width, []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions}},
		{"five metrics at the narrowest", def.minWidth, []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions}},
		{"one pill at the default width", def.width, []string{fieldStars}},
		{"one pill at the narrowest", def.minWidth, []string{fieldStars}},
		{"repositories and no numbers", def.width, []string{fieldTopRepos}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := sample()
			o := func(motion string) *Options {
				return &Options{Theme: "dark", Layout: "ticker", Motion: motion, Fields: tc.fields, Width: tc.width}
			}
			moving, still := mustRender(t, c, o(MotionOnce)), mustRender(t, c, o(MotionOff))
			band := tickerBand(float64(tc.width))
			pills := tickerPills(&spec{
				width: float64(tc.width), fields: tc.fields,
				nums:  metricsOf(c, tc.fields),
				repos: repoBlock(c, tc.fields),
			})
			// How much one copy measures, and how many of its pills begin
			// inside the band, which is every one of them when the content is
			// the narrower and as many as fit when it is not.
			strip, fit := 0.0, 0
			for _, p := range pills {
				if strip < band {
					fit++
				}
				strip += p.width + tickGap
			}
			// Every pill the copies behind the first hold is off the band, so
			// what rests on screen is the one copy the still card draws.
			if got, want := pillsOnTheBand(moving, band), pillsOnTheBand(still, band); got != want {
				t.Errorf("the band rests on %d pills and the still card draws %d: a reader who sees no animation reads the content twice", got, want)
			}
			if got := pillsOnTheBand(moving, band); got != fit {
				t.Errorf("%d pills rest on the band, want the %d of the first copy that begin inside it", got, fit)
			}
			// Which is the same thing said about the shift: a copy is never
			// narrower than the band it has to clear.
			by := shiftOf(t, moving)
			if by < band {
				t.Errorf("one copy is %v and the band is %v: the copy behind it would rest on screen", by, band)
			}
			if by != math.Ceil(math.Max(strip, band)) {
				t.Errorf("the band shifts by %v, one copy padded to the band and rounded is %v", by, math.Ceil(math.Max(strip, band)))
			}
			if by != math.Trunc(by) {
				t.Errorf("the band shifts by %v, which is not a whole pixel: a browser rasterizes the seam a pixel out", by)
			}
		})
	}
}

// repoBlock is the ranked repositories a request would leave a ticker, so a
// test can build the same pills the layout does.
func repoBlock(c *Card, fields []string) []TopRepo {
	if !slices.Contains(fields, fieldTopRepos) {
		return nil
	}
	return rank(c.TopRepos, defaultMaxRepos)
}

// pillsOnTheBand counts the pills whose left edge is inside the viewport when
// the strip is untranslated, which is the card a reader who sees no animation
// is left with.
func pillsOnTheBand(doc string, band float64) int {
	n := 0
	for _, m := range regexp.MustCompile(`<rect class="track" x="([\d.]+)"`).FindAllStringSubmatch(doc, -1) {
		if x, _ := strconv.ParseFloat(m[1], 64); x < band {
			n++
		}
	}
	return n
}

// shiftOf is the distance the band's keyframes end on.
func shiftOf(t *testing.T, doc string) float64 {
	t.Helper()
	m := regexp.MustCompile(`100%\{transform:translateX\(-([\d.]+)px\)\}`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("the band does not scroll:\n%s", doc)
	}
	by, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return by
}

// TestLanguageBarsGrowOneAfterAnotherWithTheirLabelsBehind is the reuse of the
// growth effect this layout was added for: one bar per language, each on its
// own beat, each growing from its own left edge, and the name and the share
// arriving once their bar has stopped rather than standing over one that has
// not started.
func TestLanguageBarsGrowOneAfterAnotherWithTheirLabelsBehind(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "language-bars"})
	langs := rankLanguages(c.Languages, maxLanguages)
	bars := regexp.MustCompile(`<rect class="(m\d+)" x="22" y="[\d.]+" width=`).FindAllStringSubmatch(doc, -1)
	if len(bars) != len(langs) {
		t.Fatalf("%d bars grow, want one per language, %d", len(bars), len(langs))
	}
	for i, m := range bars {
		// A bar and its label take two beats, so the bars are every other one.
		want := "m" + strconv.Itoa(2*i)
		if m[1] != want {
			t.Errorf("bar %d plays %q, want %q: the bars take the clock in order", i, m[1], want)
		}
		label := "m" + strconv.Itoa(2*i+1)
		if !strings.Contains(doc, ` class="n `+label+`"`) || !strings.Contains(doc, ` class="c `+label+`"`) {
			t.Errorf("the name and the share of language %d do not both play %s", i, label)
		}
		if !strings.Contains(doc, "."+want+"{animation:"+want) ||
			!strings.Contains(doc, "transform-box:fill-box;transform-origin:left") {
			t.Errorf("bar %d does not grow from its own left edge", i)
		}
	}
	// The label waits for its own bar: its stretch of the cycle starts where
	// the bar's ends.
	for i := range langs {
		bar := keyframeStops(t, doc, "m"+strconv.Itoa(2*i))
		label := keyframeStops(t, doc, "m"+strconv.Itoa(2*i+1))
		if bar[1] != label[0] {
			t.Errorf("bar %d stops growing at %s and its label started at %s", i, bar[1], label[0])
		}
	}
	if strings.Contains(doc, "<g ") {
		t.Error("a bar is one rectangle placed with x and y, so it needs no group to grow from its own edge")
	}
	still := mustRender(t, c, &Options{Theme: "dark", Layout: "language-bars", Motion: MotionOff})
	if strings.Contains(still, "animation") || strings.Contains(still, `class="n m`) {
		t.Error("under off nothing grows and nothing waits for it")
	}
	// A card asked for languages it has none of draws the empty track and
	// places no beat: there is no width to grow into.
	empty := mustRender(t, &Card{Login: "someone"}, &Options{Theme: "dark", Layout: "language-bars", Fields: []string{fieldLanguages}})
	if strings.Contains(empty, "animation") {
		t.Errorf("a card with no language to show must place no beat:\n%s", empty)
	}
}

// keyframeStops is the percentage a class's effect starts and ends at, read
// off the two stops of its keyframe block.
func keyframeStops(t *testing.T, doc, name string) [2]string {
	t.Helper()
	m := regexp.MustCompile(`@keyframes ` + name + `\{[\d.]*%?,?([\d.]+)%\{[a-z-]+:[^}]*\}([\d.]+)%`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no readable keyframes for %s in:\n%s", name, doc)
	}
	return [2]string{m[1], m[2]}
}

// mustLayout is one registered layout, by name.
func mustLayout(t *testing.T, name string) Layout {
	t.Helper()
	for _, l := range Layouts() {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no layout %q", name)
	return Layout{}
}

func TestHeatLevelsEndTodayAndPadTheFront(t *testing.T) {
	levels := heatLevels([]int{4, 0, 1})
	if len(levels) != 84 {
		t.Fatalf("len = %d, want 84 days", len(levels))
	}
	if levels[81] != 4 || levels[82] != 0 || levels[83] != 1 {
		t.Errorf("the series must end at today's cell: %v", levels[80:])
	}
	for _, v := range levels[:81] {
		if v != 0 {
			t.Fatal("days before the series are empty")
		}
	}
	long := make([]int, 400)
	for i := range long {
		long[i] = i
	}
	if got := heatLevels(long); got[83] != 4 || got[0] != 4 {
		t.Errorf("a long series keeps its last twelve weeks: %v", got)
	}
	for _, v := range heatLevels([]int{0, 0, 0}) {
		if v != 0 {
			t.Fatal("a flat series has no level above zero")
		}
	}
	doc := mustRender(t, sample(), &Options{Theme: "dark", Layout: "activity-heatmap"})
	if n := strings.Count(doc, `<rect class="h`); n != 84+5 {
		t.Errorf("drew %d squares, want 84 plus the five of the legend", n)
	}
}

func TestGroupedWritesThousands(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 117000: "117,000", 1234567: "1,234,567", -3387: "-3,387"}
	for in, want := range cases {
		if got := grouped(in); got != want {
			t.Errorf("grouped(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRankLanguagesFoldsTheTailIntoOther(t *testing.T) {
	in := []Language{
		{Name: "b", Bytes: 10},
		{Name: "a", Bytes: 10},
		{Name: "big", Bytes: 70},
		{Name: "empty", Bytes: 0},
		{Name: "  ", Bytes: 5},
		{Name: "tail", Bytes: 10},
	}
	got := rankLanguages(in, 2)
	if len(got) != 3 || got[0].Name != "big" || got[1].Name != "a" || got[2].Name != "Other" {
		t.Fatalf("ranked %+v", got)
	}
	if got[2].Bytes != 20 || got[0].Share != 70 {
		t.Errorf("shares are of the total: %+v", got)
	}
	if rankLanguages(nil, 8) != nil || rankLanguages([]Language{{Name: "x"}}, 8) != nil {
		t.Error("nothing to rank is nil, not an empty bar")
	}
}

func TestSafeColorOnlyPassesHexTriplets(t *testing.T) {
	if safeColor("#00ADD8", "Go") != "#00add8" {
		t.Error("a valid color passes, lower-cased")
	}
	for _, bad := range []string{"", "red", "#00add", "#00add80", `"/><script>`, "#00gdd8"} {
		if got := safeColor(bad, "Python"); got != languageColor("Python") {
			t.Errorf("safeColor(%q) = %q, want the Linguist fallback", bad, got)
		}
	}
}

func TestBadgeRowWidthFollowsItsContent(t *testing.T) {
	two := mustRender(t, sample(), &Options{Layout: "badge-row", Fields: []string{"stars", "forks"}})
	five := mustRender(t, sample(), &Options{Layout: "badge-row"})
	if !strings.Contains(two, `height="20"`) || !strings.Contains(five, `height="20"`) {
		t.Error("a badge is twenty pixels tall, shields.io height")
	}
	w2, w5 := width(t, two), width(t, five)
	if w2 >= w5 {
		t.Errorf("two badges (%v) must be narrower than five (%v)", w2, w5)
	}
	none := mustRender(t, sample(), &Options{Layout: "badge-row", Fields: []string{"languages"}})
	if !strings.Contains(none, "jmrplens") {
		t.Error("a row with nothing to show still names the account")
	}
}

func width(t *testing.T, doc string) string {
	t.Helper()
	m := regexp.MustCompile(`<svg[^>]* width="([^"]+)"`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("no width")
	}
	return m[1]
}

func TestGithubFamilyCarriesGitHubsOwnPalette(t *testing.T) {
	dark := mustRender(t, sample(), &Options{Theme: "dark", Layout: "github-stats"})
	for _, want := range []string{ghDarkPanel, ghDarkBorder, ghDarkFG, ghDarkMuted, "ui-monospace", "'Noto Sans'", "GitHub Statistics", "3,387", "117,000", "Most Used Languages"} {
		if !strings.Contains(dark, want) {
			t.Errorf("github-stats dark lacks %q", want)
		}
	}
	light := mustRender(t, sample(), &Options{Theme: "light", Layout: "github-stats"})
	for _, want := range []string{ghLightPanel, ghLightBorder, ghLightFG, ghLightMuted} {
		if !strings.Contains(light, want) {
			t.Errorf("github-stats light lacks %q", want)
		}
	}
	titled := mustRender(t, sample(), &Options{Layout: "github-stats", Title: "Mine"})
	if !strings.Contains(titled, ">Mine<") || strings.Contains(titled, "GitHub Statistics") {
		t.Error("Options.Title must replace the band title")
	}
}

func TestOverlaysCapTheirNumbers(t *testing.T) {
	all := Fields()[:12]
	hero := mustRender(t, sample(), &Options{Layout: "sparkline-hero", Fields: append(all, "sparkline")})
	if n := strings.Count(hero, `class="big"`); n != 3 {
		t.Errorf("the hero overlays %d numbers, want three", n)
	}
	heat := mustRender(t, sample(), &Options{Layout: "activity-heatmap", Fields: append(all, "sparkline")})
	if n := strings.Count(heat, `class="big"`); n != 3 {
		t.Errorf("the heatmap stacks %d numbers, want three", n)
	}
}

func TestEachLayoutRefusesAWidthBelowItsMinimum(t *testing.T) {
	for _, def := range layouts {
		if def.minWidth == 0 {
			continue
		}
		if _, err := SVG(sample(), &Options{Layout: def.Name, Width: def.minWidth - 1}); err == nil {
			t.Errorf("%s accepted a width below %d", def.Name, def.minWidth)
		}
		if _, err := SVG(sample(), &Options{Layout: def.Name, Width: def.minWidth}); err != nil {
			t.Errorf("%s refused its own minimum: %v", def.Name, err)
		}
	}
}

// TestTheHeatRampIsGitHubsOwn pins both calendar ramps to the colors read off
// github.com/jmrplens on 2026-09-14, level 0 to 4. GitHub repainted the
// calendar and the card carried the previous ramp for a while, which is the
// kind of drift only a fixed list catches: the squares still looked green.
func TestTheHeatRampIsGitHubsOwn(t *testing.T) {
	for _, tc := range []struct {
		theme string
		want  [5]string
	}{
		{"dark", [5]string{"#151b23", "#033a16", "#196c2e", "#2ea043", "#56d364"}},
		{"light", [5]string{"#eff2f5", "#aceebb", "#4ac26b", "#2da44e", "#116329"}},
	} {
		doc := mustRender(t, sample(), &Options{Theme: tc.theme, Layout: "activity-heatmap"})
		for i, color := range tc.want {
			if rule := fmt.Sprintf(".h%d{fill:%s}", i, color); !strings.Contains(doc, rule) {
				t.Errorf("%s level %d: %q is not in the card", tc.theme, i, rule)
			}
		}
	}
}

// A traffic number without its window means nothing, and a card whose window
// is unknown says so in words rather than printing "0d".
func TestTrafficLabelsNameTheWindowOrAdmitItIsUnknown(t *testing.T) {
	fields := []string{fieldViews, fieldVisitors, fieldClones}
	for _, tc := range []struct {
		days                int
		label, long, spoken string
	}{
		{14, "Views (14d)", "Repo views / 14d", "repository views in the last 14 days"},
		{0, "Views (traffic)", "Repo views / traffic", "repository views over the traffic window"},
	} {
		m := metricsOf(&Card{TrafficWindowDays: tc.days}, fields)
		if m[0].label != tc.label || m[0].long != tc.long || m[0].spoken != tc.spoken {
			t.Errorf("window %d: views read %q, %q, %q", tc.days, m[0].label, m[0].long, m[0].spoken)
		}
		if tc.days == 0 && (m[1].label != "Visitors (traffic)" || m[2].spoken != "clones over the traffic window") {
			t.Errorf("window 0: visitors and clones read %+v and %+v", m[1], m[2])
		}
	}
}

// A negative day is no activity, not a level below the empty square, and a
// zero day stays empty however busy the rest of the calendar was.
func TestHeatLevelsTreatNegativeAndZeroDaysAsEmpty(t *testing.T) {
	levels := heatLevels([]int{-3, 0, 1, 2, 8})
	if got := levels[79:]; !slices.Equal(got, []int{0, 0, 1, 1, 4}) {
		t.Errorf("levels = %v, want 0 0 1 1 4", got)
	}
}

// A ring segment exactly six units of arc long sits right on the no-gap
// threshold: the gap must close rather than leave a sliver's dash shorter
// than its own gap.
func TestLanguageRingLeavesNoGapForAnArcExactlyAtTheThreshold(t *testing.T) {
	const r = 54.0
	circ := 2 * math.Pi * r
	share := 6.0 / circ * 100 // chosen so share/100*circ lands on exactly 6
	arc := share / 100 * circ
	if arc != 6 {
		t.Fatalf("test setup: arc = %v, want exactly 6", arc)
	}
	s := &spec{width: defaultWidth, motion: MotionOff, langs: []langShare{{Name: "X", Color: "#111111", Share: share}}}
	var b strings.Builder
	drawLanguageRing(&b, &Card{}, s)
	// No gap means the slice ends where its own arc ends, six units along the
	// ring from twelve o'clock, rather than three units short of it.
	const band, stroke = 44.0, 16.0
	x1, y1 := ringPoint(ghPad+r+stroke/2, band+18+r+stroke/2, r, arc)
	want := fmt.Sprintf(`1 %s,%s"`, num(x1), num(y1))
	if !strings.Contains(b.String(), want) {
		t.Errorf("an arc of exactly 6 must have no gap, want %q in:\n%s", want, b.String())
	}
}

// A language legend label that fits the row exactly, down to the last unit,
// must still be drawn: the cutoff is for what does not fit, not for what
// fits exactly.
func TestSummaryLanguagesLegendKeepsALabelThatFitsExactly(t *testing.T) {
	label := "A 50%"
	w := 14 + textWidth(label, 11)
	width := 2*pad + w
	inner := width - 2*pad
	if x := pad; x+w != pad+inner {
		t.Fatalf("test setup: x+w = %v, pad+inner = %v, want them exactly equal (the boundary moved)", x+w, pad+inner)
	}
	s := &spec{width: width, langs: []langShare{{Name: "A", Color: "#111111", Share: 50}}}
	var b strings.Builder
	summaryLanguages(&b, s, 0)
	if !strings.Contains(b.String(), label) {
		t.Errorf("a label that fits exactly must still be drawn:\n%s", b.String())
	}
}

// updateGolden rewrites the committed cards instead of comparing against
// them: go test ./internal/render -run TestEveryLayoutDrawsTheCommittedCards -update.
// A change to the geometry is then a diff of testdata/golden to be read,
// not a regeneration nobody looked at.
var updateGolden = flag.Bool("update", false, "rewrite testdata/golden from the renderer")

// goldenCase is one card drawn by every layout. fields nil means each
// layout's full supported set.
type goldenCase struct {
	name      string
	card      func() *Card
	fields    []string
	narrowest bool   // drawn at the layout's minimum width, where every column truncates
	title     string // Options.Title, empty for each layout's own heading
}

// goldenCases are chosen for the branches they reach rather than for looking
// like an account: the full sample, a card with nothing but a login, a
// request for only the three blocks so no layout has a number to draw, a
// request without the sparkline over repositories nobody starred, the sample
// squeezed to each layout's minimum width, the same squeeze over texts and
// numbers too long for any column, and a card of awkward values (a name equal to the login, a repository without
// stars or language, a sliver of a language, more languages than fit, names
// too long for their column and a negative day).
func goldenCases() []goldenCase {
	return []goldenCase{
		{name: "sample", card: sample},
		{name: "bare", card: func() *Card { return &Card{Login: "someone"} }},
		{name: "blocks-only", card: sample, fields: []string{fieldLanguages, fieldTopRepos, fieldSparkline}},
		{name: "awkward", card: awkward},
		{name: "unstarred", card: unstarred, fields: join(numericFields, fieldLanguages, fieldTopRepos)},
		{name: "narrowest", card: sample, narrowest: true},
		{
			name: "extremes", card: extremes, narrowest: true,
			title: strings.Repeat("A heading far too long for any band ", 4),
			// The traffic numbers first, because their labels carry the window
			// and so are the longest labels there are.
			fields: join([]string{fieldViews, fieldVisitors, fieldClones, fieldPullRequests}, fieldContributions,
				fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldCommits, fieldReviews, fieldIssues,
				fieldLanguages, fieldTopRepos, fieldSparkline),
		},
	}
}

// extremes is every text as long as it gets and every number as wide as it
// gets, drawn at the minimum width: nothing fits, so every column's limit
// decides where its text is cut, and a limit that moves by a few pixels moves
// the cut.
func extremes() *Card {
	const huge = -1099511627776
	long := func(s string) string { return strings.Repeat(s+" ", 8) }
	c := &Card{
		Login:       "an-account-login-long-enough-to-crowd-its-band",
		Name:        long("A display name"),
		Description: long("A biography"),
		Stars:       huge, Forks: huge, Followers: huge, Repos: huge, Contributions: huge,
		Views: huge, UniqueVisitors: huge, Clones: huge, Commits: huge, PullRequests: huge,
		Reviews: huge, Issues: huge,
		TrafficWindowDays: 12345678901234567,
		Sparkline:         []int{1, 5, 2},
	}
	for i, name := range []string{"Go", "Python", "TypeScript", "Shell"} {
		c.Languages = append(c.Languages, Language{Name: long(name), Bytes: int64(40 - 8*i)})
		c.TopRepos = append(c.TopRepos, TopRepo{Name: long("repository-" + name), Language: long(name), Stars: huge - i})
	}
	return c
}

// unstarred is the sample with no sparkline asked for and no repository
// holding a star, so the bars of repo-list have nothing to be relative to.
func unstarred() *Card {
	c := sample()
	for i := range c.TopRepos {
		c.TopRepos[i].Stars = 0
	}
	return c
}

func awkward() *Card {
	days := make([]int, 120)
	for i := range days {
		days[i] = (i * 7) % 11
	}
	days[40] = -4
	return &Card{
		Login:             "octo",
		Name:              "octo",
		Description:       "A description long enough that no layout has the room to print all of it on one line without cutting",
		Stars:             1234567,
		Forks:             0,
		Followers:         999950,
		Repos:             3,
		Contributions:     1049950000,
		Views:             -12,
		UniqueVisitors:    7,
		Clones:            1000,
		TrafficWindowDays: 0,
		Commits:           100,
		PullRequests:      10,
		Reviews:           1,
		Issues:            0,
		Sparkline:         days,
		Languages: []Language{
			{Name: "JavaScript with a very long qualifier", Bytes: 900000},
			{Name: "TypeScript also rather long", Bytes: 800000, Color: "#ABCDEF"},
			{Name: "Dockerfile", Bytes: 700000, Color: "not a color"},
			{Name: "Makefile and friends", Bytes: 600000},
			{Name: "Some language nobody has heard of", Bytes: 500000},
			{Name: "Kotlin", Bytes: 400000},
			{Name: "MATLAB", Bytes: 300000},
			{Name: "Assembly", Bytes: 200000},
			{Name: "Lua", Bytes: 1000},
			{Name: "Sliver", Bytes: 1},
		},
		TopRepos: []TopRepo{
			{Name: "a-repository-name-far-wider-than-any-column-it-could-be-given", Language: "Go", Stars: 50},
			{Name: "no-language", Stars: 50},
			{Name: "unstarred", Language: "Rust", Stars: 0},
			{Name: "unknown", Language: "Brainfuck", Stars: 3},
		},
	}
}

// TestEveryLayoutDrawsTheCommittedCards pins the geometry. The other tests
// here assert properties of a card; this one asserts the card, byte for
// byte, so an offset that moves a number onto its label or a height that
// clips the last row is a failure rather than a picture nobody opened.
func TestEveryLayoutDrawsTheCommittedCards(t *testing.T) {
	for _, gc := range goldenCases() {
		for _, l := range Layouts() {
			t.Run(gc.name+"/"+l.Name, func(t *testing.T) {
				fields := gc.fields
				if fields == nil {
					fields = l.Supports
				}
				o := &Options{Theme: "dark", Layout: l.Name, Fields: fields, Title: gc.title}
				if def, _ := findLayout(l.Name); gc.narrowest {
					o.Width = def.minWidth
				}
				got := mustRender(t, gc.card(), o)
				sameAsGolden(t, filepath.Join("testdata", "golden", gc.name, l.Name+".svg"), got)
			})
		}
	}
}

// sameAsGolden compares a card with the committed one at path, or writes it
// there under -update.
func sameAsGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the committed card: %v", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the committed card: %s", path, firstDifference(got, string(want)))
	}
}

// firstDifference names the first line that differs, which is where a
// geometry change shows up and all a reader needs to find it.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range min(len(g), len(w)) {
		if g[i] != w[i] {
			return fmt.Sprintf("line %d is\n%s\nwant\n%s", i+1, g[i], w[i])
		}
	}
	return fmt.Sprintf("%d lines, want %d", len(g), len(w))
}
