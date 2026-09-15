package render

import (
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func sample() *Card {
	return &Card{
		Login:             "jmrplens",
		Name:              "José M. Requena Plens",
		Description:       "Embedded firmware and acoustics",
		Stars:             283,
		Forks:             70,
		Followers:         41,
		Repos:             42,
		Contributions:     6210,
		Views:             1810,
		UniqueVisitors:    878,
		Clones:            117000,
		TrafficWindowDays: 14,
		Commits:           3387,
		PullRequests:      1179,
		Reviews:           389,
		Issues:            64,
		Sparkline:         []int{0, 3, 12, 7, 0, 1, 20, 15, 4, 9},
		Languages: []Language{
			{Name: "Go", Bytes: 35100000},
			{Name: "Python", Bytes: 21300000},
			{Name: "C++", Bytes: 19900000},
		},
		TopRepos: []TopRepo{
			{Name: "phonometry", Language: "Python", Stars: 112},
			{Name: "TFG-TFM_EPS", Language: "TeX", Stars: 84},
			{Name: "gitlab-mcp-server", Language: "Go", Stars: 33},
		},
	}
}

// everyLayout ranges over every registered layout crossed with every theme.
// The constraints its callers assert are the reason this package exists, so
// none of them may hold for the summary only.
//
// A sequence rather than a callback taking *testing.T: the assertions then sit
// in the caller's own t.Run body, so a failure reports the line that failed
// instead of the one line inside here that runs every subtest.
func everyLayout(yield func(layout, theme string) bool) {
	for _, l := range Layouts() {
		for _, theme := range []string{"dark", "light", "auto"} {
			if !yield(l.Name, theme) {
				return
			}
		}
	}
}

func mustRender(t *testing.T, c *Card, o *Options) string {
	t.Helper()
	out, err := SVG(c, o)
	if err != nil {
		t.Fatalf("SVG: %v", err)
	}
	return string(out)
}

// parseXML fails the test if the document is not well formed. A card that a
// parser rejects is a card the browser draws as nothing.
func parseXML(t *testing.T, doc string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("document does not parse: %v", err)
		}
	}
}

func TestSVGIsByteIdenticalBetweenRuns(t *testing.T) {
	for layout, theme := range everyLayout {
		t.Run(layout+"/"+theme, func(t *testing.T) {
			c := sample()
			o := &Options{Theme: theme, Layout: layout, Fields: Fields()}
			first := mustRender(t, c, o)
			for range 20 {
				if mustRender(t, c, o) != first {
					t.Fatal("the same card rendered twice must produce the same bytes, or a workflow that commits it writes a diff a day out of nothing")
				}
			}
		})
	}
}

func TestTopReposAreRankedIndependentlyOfInputOrder(t *testing.T) {
	c := sample()
	shuffled := sample()
	shuffled.TopRepos = []TopRepo{
		{Name: "gitlab-mcp-server", Language: "Go", Stars: 33},
		{Name: "phonometry", Language: "Python", Stars: 112},
		{Name: "TFG-TFM_EPS", Language: "TeX", Stars: 84},
	}
	if mustRender(t, c, &Options{}) != mustRender(t, shuffled, &Options{}) {
		t.Fatal("the caller's order must not reach the file: the list is ranked here")
	}
}

func TestTopReposAreCappedAndTiesBrokenByName(t *testing.T) {
	in := []TopRepo{{Name: "b", Stars: 5}, {Name: "a", Stars: 5}, {Name: "c", Stars: 9}}
	got := rank(in, 2)
	if len(got) != 2 || got[0].Name != "c" || got[1].Name != "a" {
		t.Fatalf("rank = %+v, want c then a", got)
	}
}

func TestSVGEscapesHostileRepositoryText(t *testing.T) {
	for layout, theme := range everyLayout {
		t.Run(layout+"/"+theme, func(t *testing.T) {
			c := sample()
			c.Login = `<script>login</script>`
			c.Description = `<script>alert("xss")</script> & 'quotes'`
			c.Name = `<b>José</b>`
			c.TopRepos = []TopRepo{
				{Name: `<script>alert(1)</script>`, Language: `C++ & <b>`, Stars: 7},
			}
			c.Languages = []Language{
				{Name: `<script>lang</script>`, Bytes: 10, Color: `"/><script>x</script>`},
				{Name: "Go", Bytes: 5, Color: "#00ADD8"},
			}
			doc := mustRender(t, c, &Options{Theme: theme, Layout: layout, Fields: Fields(), Title: `Title <script>`})

			for _, forbidden := range []string{"<script", "</script", "<b>", "alert(1)<", `"/>` + "<"} {
				if strings.Contains(doc, forbidden) {
					t.Errorf("%q survived as markup; every string must go through esc", forbidden)
				}
			}
			if !strings.Contains(doc, "&lt;script&gt;") {
				t.Error("the hostile text should still be visible, escaped, rather than dropped")
			}
			if strings.Contains(doc, "x</script>") {
				t.Error("a caller-supplied color reached an attribute unchecked")
			}
			parseXML(t, doc)
		})
	}
}

func TestEscapeCoversEveryDangerousCharacter(t *testing.T) {
	got := esc(`&<>"'`)
	for _, raw := range []string{"&<", "<", ">", `"`, "'"} {
		if strings.Contains(strings.ReplaceAll(got, "&amp;", ""), raw) {
			t.Fatalf("esc(%q) = %q, %q survived", `&<>"'`, got, raw)
		}
	}
}

func TestSparklineHandlesDegenerateSeries(t *testing.T) {
	cases := []struct {
		name   string
		values []int
		want   int // points drawn
	}{
		{"no data at all", nil, 2},
		{"all zeros", []int{0, 0, 0, 0}, 4},
		{"a single day", []int{9}, 2},
		{"a normal week", []int{1, 4, 0, 7, 2, 2, 5}, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pts := sparkPoints(tc.values, 10, 20, 100, 40)
			if len(pts) != tc.want {
				t.Fatalf("points = %d, want %d", len(pts), tc.want)
			}
			for _, p := range pts {
				if math.IsNaN(p.x) || math.IsNaN(p.y) || math.IsInf(p.x, 0) || math.IsInf(p.y, 0) {
					t.Fatalf("degenerate series produced %v; the scale must not divide by zero", p)
				}
				if p.y < 20 || p.y > 60 {
					t.Fatalf("point %v escaped the 20..60 box", p)
				}
			}
			c := sample()
			c.Sparkline = tc.values
			doc := mustRender(t, c, &Options{})
			if strings.Contains(doc, "NaN") || strings.Contains(doc, "Inf") {
				t.Fatal("the document carries a non-finite coordinate")
			}
			if !strings.Contains(doc, "<polyline") {
				t.Fatal("every series draws a line, even a flat one")
			}
			parseXML(t, doc)
		})
	}
}

func TestSparklineFlattensAnAllZeroSeriesOntoTheBaseline(t *testing.T) {
	for _, p := range sparkPoints([]int{0, 0, 0}, 0, 0, 100, 40) {
		if p.y != 40 {
			t.Fatalf("y = %v, want the baseline at 40", p.y)
		}
	}
}

func TestCompactShortensCounts(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1k"},
		{1810, "1.8k"},
		{1850, "1.9k"},
		{12400, "12.4k"},
		{999499, "999.5k"},
		{999999, "1M"},
		{1200000, "1.2M"},
		{1000000000, "1B"},
		{-1810, "-1.8k"},
		// Past the last rung the mantissa grows rather than the unit. A
		// trillion is 1000 billion, not one.
		{1000000000000, "1000B"},
		{1099511627776, "1099.5B"},
		// Rounded once, at the end. 1049.95M is 1.04995B, which is 1.0B to
		// the one decimal shown; rounding at every rung on the way up made
		// this read 1.1B, a whole tenth of a billion that was not there.
		{1049950000, "1B"},
		{1049999999, "1B"},
		{1050000000, "1.1B"},
	}
	for _, tc := range cases {
		if got := compact(tc.in); got != tc.want {
			t.Errorf("compact(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFitTruncatesWithAnEllipsis(t *testing.T) {
	const size, column = 12, 60.0

	short := "go"
	if got := fit(short, size, column); got != short {
		t.Errorf("fit did not leave a short string alone: %q", got)
	}

	long := "a-repository-name-far-wider-than-its-column"
	got := fit(long, size, column)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("fit(%q) = %q, want an ellipsis", long, got)
	}
	if got == long || len([]rune(got)) >= len([]rune(long)) {
		t.Errorf("fit(%q) = %q, nothing was cut", long, got)
	}
	if w := textWidth(got, size); w > column {
		t.Errorf("fit produced %q at width %v, over the %v column", got, w, column)
	}
	if fit(long, size, 0) != "" {
		t.Error("a column with no room holds no text")
	}
}

func TestSVGParsesInEveryTheme(t *testing.T) {
	for layout, theme := range everyLayout {
		t.Run(layout+"/"+theme, func(t *testing.T) {
			for _, fields := range [][]string{nil, Fields()} {
				doc := mustRender(t, sample(), &Options{Theme: theme, Layout: layout, Fields: fields})
				parseXML(t, doc)
				if !strings.HasPrefix(doc, "<svg xmlns=") || !strings.HasSuffix(doc, "</svg>\n") {
					t.Errorf("theme %q did not produce a standalone document", theme)
				}
				if !strings.Contains(doc, `role="img"`) || !strings.Contains(doc, "<title id=\"ghcTitle\"") ||
					!strings.Contains(doc, "<desc id=\"ghcDesc\"") {
					t.Errorf("theme %q lost the accessible name or description", theme)
				}
				if strings.Contains(doc, "prefers-color-scheme") != (theme == "auto") {
					t.Errorf("theme %q: prefers-color-scheme presence is wrong", theme)
				}
			}
		})
	}
	// The empty theme is auto, and the empty layout is the summary.
	if mustRender(t, sample(), &Options{}) != mustRender(t, sample(), &Options{Theme: "auto", Layout: "summary"}) {
		t.Error("empty options must mean the auto summary")
	}
	// And no options at all means the same as empty ones.
	if mustRender(t, sample(), nil) != mustRender(t, sample(), &Options{}) {
		t.Error("a nil Options must mean every default, not a panic")
	}
}

func TestSVGStaysSelfContained(t *testing.T) {
	for layout, theme := range everyLayout {
		t.Run(layout+"/"+theme, func(t *testing.T) {
			doc := mustRender(t, sample(), &Options{Theme: theme, Layout: layout, Fields: Fields()})
			for _, forbidden := range []string{"<script", "https://www.w3.org/1999/xlink", "@import", "<image", "url(", "@font-face", "<foreignObject", "onload"} {
				if strings.Contains(doc, forbidden) {
					t.Errorf("%q would be fetched or stripped by the camo proxy", forbidden)
				}
			}
			if n := strings.Count(doc, "http"); n != 1 {
				t.Errorf("the only URL in the document is the SVG namespace, found %d", n)
			}
			// A renderer that does not implement custom properties drops the whole
			// declaration and paints the card black on black. librsvg still does.
			if strings.Contains(doc, "var(--") {
				t.Error("colors must be literal, not custom properties")
			}
		})
	}
}

func TestSVGFixedThemesCarryTheirPalette(t *testing.T) {
	for _, l := range Layouts() {
		dark := mustRender(t, sample(), &Options{Theme: "dark", Layout: l.Name})
		light := mustRender(t, sample(), &Options{Theme: "light", Layout: l.Name})
		if !strings.Contains(dark, darkBG) || strings.Contains(dark, lightBG) {
			t.Errorf("%s: the dark card must carry only the dark palette", l.Name)
		}
		if !strings.Contains(light, lightBG) || strings.Contains(light, darkBG) {
			t.Errorf("%s: the light card must carry only the light palette", l.Name)
		}
	}
}

func TestSVGGrowsWithItsContent(t *testing.T) {
	few := sample()
	few.TopRepos = few.TopRepos[:1]
	many := sample()
	if height(t, mustRender(t, many, &Options{})) <= height(t, mustRender(t, few, &Options{})) {
		t.Fatal("more repository rows must make a taller card, not overflow it")
	}
	none := sample()
	none.TopRepos = nil
	doc := mustRender(t, none, &Options{})
	if strings.Contains(doc, "TOP REPOSITORIES") {
		t.Error("an empty list must not leave its heading behind")
	}
	parseXML(t, doc)
}

func height(t *testing.T, doc string) string {
	t.Helper()
	var root struct {
		Height string `xml:"height,attr"`
	}
	if err := xml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatal(err)
	}
	return root.Height
}

func TestSVGRejectsWhatItCannotDraw(t *testing.T) {
	if _, err := SVG(&Card{}, &Options{}); err == nil {
		t.Error("a card with no login has nothing to be about")
	}
	if _, err := SVG(sample(), &Options{Theme: "solarized"}); err == nil {
		t.Error("an unknown theme must be an error, not a silent fallback")
	}
	if _, err := SVG(sample(), &Options{Width: 120}); err == nil {
		t.Error("a card narrower than its columns must be refused")
	}
}

func TestOptionsWidthReachesTheDocument(t *testing.T) {
	doc := mustRender(t, sample(), &Options{Width: 600})
	if !strings.Contains(doc, `width="600"`) || !strings.Contains(doc, `viewBox="0 0 600 `) {
		t.Error("Width must set both the attribute and the viewBox")
	}
}

func TestMaxReposLimitsTheList(t *testing.T) {
	c := sample()
	doc := mustRender(t, c, &Options{MaxRepos: 1})
	if strings.Contains(doc, "TFG-TFM_EPS") {
		t.Error("MaxRepos did not cap the list")
	}
	if !strings.Contains(doc, "phonometry") {
		t.Error("the most starred repository must survive the cap")
	}
}

func TestNumKeepsIntegerZeros(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{{600, "600"}, {100, "100"}, {1.8, "1.8"}, {12.34, "12.34"}, {0, "0"}, {40.5, "40.5"}}
	for _, tc := range cases {
		if got := num(tc.in); got != tc.want {
			t.Errorf("num(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every character next to a hex digit range is one an attacker could use,
// so the boundaries of all three ranges are pinned, and so is the leading #.
func TestSafeColorAcceptsEveryHexDigitAndNothingBesideThem(t *testing.T) {
	for _, good := range []string{"#09afAF", "#000000", "#FFFFFF", "#a0f9A0"} {
		if got := safeColor(good, "Python"); got != strings.ToLower(good) {
			t.Errorf("safeColor(%q) = %q, want it through, lower-cased", good, got)
		}
	}
	for _, bad := range []string{"#/00000", "#:00000", "#`00000", "#g00000", "#@00000", "#G00000", "000add8", "#00add/"} {
		if got := safeColor(bad, "Python"); got != languageColor("Python") {
			t.Errorf("safeColor(%q) = %q, want the Linguist fallback", bad, got)
		}
	}
}

// A language with no bytes is not a slice of the bar, and when everything
// fits there is no "Other" at all. When there is one, its share is of the
// same total as the others, so the bar still adds up to the whole.
func TestRankLanguagesKeepsOnlyWhatHasBytes(t *testing.T) {
	got := rankLanguages([]Language{{Name: "Go", Bytes: 30}, {Name: "Empty", Bytes: 0}, {Name: "C", Bytes: 10}}, 8)
	if len(got) != 2 || got[0].Name != "Go" || got[1].Name != "C" {
		t.Fatalf("ranked %+v, want Go and C and nothing else", got)
	}
	if got[0].Share != 75 || got[1].Share != 25 {
		t.Errorf("shares = %v and %v, want 75 and 25", got[0].Share, got[1].Share)
	}
	folded := rankLanguages([]Language{{Name: "a", Bytes: 60}, {Name: "b", Bytes: 30}, {Name: "c", Bytes: 10}}, 1)
	if len(folded) != 2 || folded[1].Name != "Other" || folded[1].Share != 40 {
		t.Errorf("folded %+v, want a then Other at 40%%", folded)
	}
}

// A limit of zero folds every language into "Other": there is no room for a
// kept entry, so the whole total rests on the fold.
func TestRankLanguagesWithZeroLimitFoldsEverythingIntoOther(t *testing.T) {
	got := rankLanguages([]Language{{Name: "Go", Bytes: 10}, {Name: "Rust", Bytes: 5}}, 0)
	if len(got) != 1 || got[0].Name != "Other" || got[0].Bytes != 15 {
		t.Fatalf("rankLanguages with limit 0 = %+v, want everything folded into Other", got)
	}
}

// Two entries the ranking cannot tell apart keep the order they came in: the
// sort is stable, and a comparison that called equals "less" would swap them
// on every run.
func TestRankingKeepsTheOrderOfEntriesItCannotTellApart(t *testing.T) {
	repos := rank([]TopRepo{{Name: "x", Language: "Go", Stars: 3}, {Name: "x", Language: "C", Stars: 3}}, 5)
	if repos[0].Language != "Go" || repos[1].Language != "C" {
		t.Errorf("rank = %+v, want the caller's order for identical keys", repos)
	}
	langs := rankLanguages([]Language{{Name: "Go", Bytes: 5, Color: "#111111"}, {Name: "Go", Bytes: 5, Color: "#222222"}}, 8)
	if langs[0].Color != "#111111" || langs[1].Color != "#222222" {
		t.Errorf("rankLanguages = %+v, want the caller's order for identical keys", langs)
	}
}

func TestHeadingPrefersTheTitleThenTheNameThenTheLogin(t *testing.T) {
	c := &Card{Login: "octo", Name: "Octo Cat"}
	if got := heading(c, &Options{Title: "Mine"}); got != "Mine" {
		t.Errorf("heading = %q, want the title", got)
	}
	if got := heading(c, &Options{}); got != "Octo Cat" {
		t.Errorf("heading = %q, want the name", got)
	}
	c.Name = ""
	if got := heading(c, &Options{}); got != "octo" {
		t.Errorf("heading = %q, want the login", got)
	}
}

// The description is what a screen reader says instead of the drawing, so
// its punctuation is part of what it says. Pinned whole, with a repository
// that has no language and a sparkline whose highest day is not its last.
func TestDescribeSpellsTheCardOutSentenceBySentence(t *testing.T) {
	c := &Card{
		Login: "octo", Stars: 1810, Forks: 3,
		Sparkline: []int{3, 9, 2},
		Languages: []Language{{Name: "Go", Bytes: 2}, {Name: "C", Bytes: 1}},
		TopRepos:  []TopRepo{{Name: "one", Language: "Go", Stars: 9}, {Name: "two", Stars: 4}},
	}
	fields := []string{fieldStars, fieldForks, fieldLanguages, fieldTopRepos, fieldSparkline}
	s := &spec{
		fields: fields, nums: metricsOf(c, fields),
		repos: rank(c.TopRepos, 5), langs: rankLanguages(c.Languages, maxLanguages),
	}
	want := "GitHub summary for octo: 1810 stars, 3 forks." +
		" Languages by bytes: Go 67%, C 33%." +
		" Most starred repositories: one with 9 stars in Go, two with 4 stars." +
		" Contributions per day over the last 3 days, peaking at 9."
	if got := describe(c, s); got != want {
		t.Errorf("describe =\n%q\nwant\n%q", got, want)
	}

	if got := describe(&Card{Login: "octo"}, &spec{fields: []string{fieldSparkline}}); got != "GitHub summary for octo." {
		t.Errorf("an empty card is described as %q", got)
	}
}

// The area is closed along the baseline at both ends of the box, and the
// line visits the same points in order, separated by single spaces.
func TestDrawSparklineClosesTheAreaAlongTheBaseline(t *testing.T) {
	var b strings.Builder
	drawSparkline(&b, []int{0, 2, 1}, 10, 20, 100, 40, sparkMotion{})
	want := `<path class="area" d="M10,60 L10,60 L60,20 L110,40 L110,60Z"/>` + "\n" +
		`<polyline class="line" points="10,60 60,20 110,40"/>` + "\n"
	if b.String() != want {
		t.Errorf("drawSparkline =\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	drawSparkline(&b, []int{0, 2, 1}, 10, 20, 100, 40, sparkMotion{area: "m0", line: "m1"})
	want = `<path class="area m0" d="M10,60 L10,60 L60,20 L110,40 L110,60Z"/>` + "\n" +
		`<polyline class="line m1" pathLength="1" points="10,60 60,20 110,40"/>` + "\n"
	if b.String() != want {
		t.Errorf("animated drawSparkline =\n%s\nwant\n%s", b.String(), want)
	}
}

// The points are spread evenly across the width and scaled against the
// highest day; a negative count, which no calendar has but a caller can
// pass, sits on the baseline instead of below the box.
func TestSparkPointsScaleAgainstThePeakAndClampBelowZero(t *testing.T) {
	got := sparkPoints([]int{-5, 4, 2}, 10, 20, 100, 40)
	want := []point{{10, 60}, {60, 20}, {110, 40}}
	if len(got) != len(want) {
		t.Fatalf("points = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("point %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestCardBackgroundSitsHalfAPixelInsideTheDocument(t *testing.T) {
	var b strings.Builder
	cardBG(&b, 100, 50)
	if want := `<rect class="bg" x="0.5" y="0.5" width="99" height="49" rx="6"/>` + "\n"; b.String() != want {
		t.Errorf("cardBG = %q, want %q", b.String(), want)
	}
}

// The three edges of fit: a text exactly as wide as its column is left
// whole, a cut keeps the longest prefix that fits together with its
// ellipsis even when that is an exact fit, and a column with room for the
// ellipsis but not for one letter holds nothing rather than a lone "…".
func TestFitHandlesExactFitsAndColumnsTooNarrowForALetter(t *testing.T) {
	const size = 10.0
	if got := fit("abc", size, textWidth("abc", size)); got != "abc" {
		t.Errorf("an exact fit was cut: %q", got)
	}
	if got := fit("abcd", size, textWidth("ab", size)+textWidth("…", size)); got != "ab…" {
		t.Errorf("fit = %q, want ab…", got)
	}
	if got := fit("WWW", size, textWidth("…", size)+1); got != "" {
		t.Errorf("fit = %q, want nothing", got)
	}
}

func TestMonoWidthIsSixTenthsOfTheSizePerGlyph(t *testing.T) {
	if got := monoWidth("héllo", 10); got != 30 {
		t.Errorf("monoWidth = %v, want 30: five glyphs, not six bytes", got)
	}
}

// runeWidth is the whole of the text measurement, and every class boundary
// is a character that sits on it.
func TestRuneWidthClassifiesEveryBoundaryCharacter(t *testing.T) {
	cases := []struct {
		want  float64
		runes string
	}{
		{0.28, " "},
		{0.30, "i.}:["},
		{0.90, "WMm@"},
		{0.68, "AZQ"},
		{0.56, "09"},
		{0.55, "/\\az?\u2e7f"},
		{1.00, "\u2e80漢"},
	}
	for _, tc := range cases {
		for _, r := range tc.runes {
			if got := runeWidth(r); got != tc.want {
				t.Errorf("runeWidth(%q) = %v, want %v", r, got, tc.want)
			}
		}
	}
}

// badge-row sizes itself from its content, so it has no minimum to enforce
// and whatever Width says, even a negative one, changes nothing.
func TestBadgeRowIgnoresTheWidthItWasGiven(t *testing.T) {
	want := mustRender(t, sample(), &Options{Layout: "badge-row"})
	for _, w := range []int{-1, 1, 5000} {
		got, err := SVG(sample(), &Options{Layout: "badge-row", Width: w})
		if err != nil {
			t.Fatalf("width %d: %v", w, err)
		}
		if string(got) != want {
			t.Errorf("width %d changed the badge row", w)
		}
	}
}
