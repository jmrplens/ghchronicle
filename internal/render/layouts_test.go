package render

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
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

func TestAnimatedLayoutsPlayOnceAndSettleToTheStaticCard(t *testing.T) {
	c := sample()
	for _, l := range Layouts() {
		doc := mustRender(t, c, &Options{Theme: "dark", Layout: l.Name})
		if !l.Animated {
			if strings.Contains(doc, "animation") || strings.Contains(doc, "<animate") {
				t.Errorf("%s: is not registered as animated but carries animation", l.Name)
			}
			continue
		}
		if strings.Contains(doc, "infinite") {
			t.Errorf("%s: an animation must be finite", l.Name)
		}
		if !strings.Contains(doc, "@media (prefers-reduced-motion:reduce)") || !strings.Contains(doc, "animation:none") {
			t.Errorf("%s: must switch its animation off under prefers-reduced-motion", l.Name)
		}
		for _, name := range animationsWithoutKeyframes(doc) {
			t.Errorf("%s: animation %q has no @keyframes", l.Name, name)
		}
		if strings.Contains(doc, "<polyline") {
			checkLineDrawSettles(t, l.Name, doc)
		}
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
// keyframes: the base style is a solid line, which is what a renderer without
// animation shows.
func checkLineDrawSettles(t *testing.T, layout, doc string) {
	t.Helper()
	if !strings.Contains(doc, "to{stroke-dasharray:1;stroke-dashoffset:0}") {
		t.Errorf("%s: the draw animation must end fully drawn", layout)
	}
	if strings.Contains(doc, ".draw{stroke-dasharray") {
		t.Errorf("%s: the dash must not be a base style", layout)
	}
}

func TestAnimatedCountersEndOnTheRealValues(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{Theme: "dark", Layout: "animated-counters"})
	for _, m := range metricsOf(c, layouts[9].Fields) {
		final := `class="big cu cuz" x="`
		if !strings.Contains(doc, final) {
			t.Fatal("no final frame")
		}
		if !strings.Contains(doc, `cuz" x="`) || !strings.Contains(doc, `>`+compact(m.value)+`</text>`) {
			t.Errorf("the final frame for %s must show %s", m.key, compact(m.value))
		}
	}
	// Every final frame carries the static value; intermediate frames rest
	// hidden and the final one rests visible, which is what a renderer that
	// ignores animation paints.
	finals := regexp.MustCompile(`class="big cu cuz"[^>]*>([^<]+)<`).FindAllStringSubmatch(doc, -1)
	if len(finals) != 6 {
		t.Fatalf("found %d final frames, want 6", len(finals))
	}
	want := []string{"283", "70", "41", "42", "6.2k", "1.8k"}
	for i, w := range want {
		if finals[i][1] != w {
			t.Errorf("final frame %d = %q, want %q", i, finals[i][1], w)
		}
	}
	for i := range counterFrames {
		if !strings.Contains(doc, ".cu"+strconv.Itoa(i)+"{opacity:0;") {
			t.Errorf("intermediate frame %d does not rest hidden", i)
		}
	}
	if strings.Contains(doc, ".cuz{opacity:0") || !strings.Contains(doc, "@keyframes kz{0%,99.99%{opacity:0}}") {
		t.Error("the final frame must rest visible and only hide while the count runs")
	}
	if counterValue(1000, 0, 16) != 0 || counterValue(1000, 16, 16) != 1000 {
		t.Error("a counter starts at zero and lands exactly on its value")
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
