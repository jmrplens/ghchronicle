package render

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestLayoutsRegistryHoldsTheTenConcepts(t *testing.T) {
	want := []string{
		"summary", "github-stats", "github-compact", "badge-row", "wide-banner",
		"sparkline-hero", "language-ring", "repo-list", "activity-heatmap", "animated-counters",
	}
	got := Layouts()
	if len(got) != len(want) {
		t.Fatalf("Layouts() has %d entries, want %d", len(got), len(want))
	}
	animated := map[string]bool{"wide-banner": true, "sparkline-hero": true, "animated-counters": true}
	for i, l := range got {
		if l.Name != want[i] {
			t.Errorf("layout %d = %q, want %q", i, l.Name, want[i])
		}
		if l.Family != "chronicle" && l.Family != "github" {
			t.Errorf("%s: family %q is neither chronicle nor github", l.Name, l.Family)
		}
		if l.Description == "" || strings.Contains(l.Description, "\n") {
			t.Errorf("%s: the description must be one line", l.Name)
		}
		if l.Animated != animated[l.Name] {
			t.Errorf("%s: animated = %v", l.Name, l.Animated)
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
	// The registry must not be reachable through the copies.
	got[0].Fields[0] = "tampered"
	if Layouts()[0].Fields[0] == "tampered" {
		t.Error("Layouts() handed out the registry's own slice")
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
	if loops := strings.Contains(doc, "infinite"); loops != (motion == MotionLoop) {
		t.Errorf("%s under %s: infinite = %v", l.Name, motion, loops)
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
	if strings.Contains(doc, "<polyline") {
		checkLineDrawSettles(t, l.Name, doc)
	}
}

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

// checkLineDrawSettles checks that the line's dash exists only inside the
// keyframes and that the last of them leaves it fully drawn: the base style is
// a solid line, which is what a renderer without animation shows.
func checkLineDrawSettles(t *testing.T, layout, doc string) {
	t.Helper()
	if !strings.Contains(doc, "100%{stroke-dasharray:1;stroke-dashoffset:0}}") {
		t.Errorf("%s: the draw animation must end fully drawn", layout)
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
	if got := len(regexp.MustCompile(`class="big cuz"[^>]*>`).FindAllString(still, -1)); got != len(want) {
		t.Errorf("under off drew %d final frames, want %d", got, len(want))
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
		m := regexp.MustCompile(`@media \(prefers-reduced-motion:reduce\)\{([^}]+)\{animation:none\}\}`).FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("under %s: no prefers-reduced-motion block", motion)
		}
		named := map[string]bool{}
		for cls := range strings.SplitSeq(m[1], ",") {
			named[strings.TrimPrefix(cls, ".")] = true
		}
		for _, am := range regexp.MustCompile(`\.(m\d+)\{animation:`).FindAllStringSubmatch(doc, -1) {
			if !named[am[1]] {
				t.Errorf("under %s: class %q animates but is not named in the reduced-motion block", motion, am[1])
			}
		}
	}
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
