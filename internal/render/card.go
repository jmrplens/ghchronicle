// Package render draws one collected sweep as an SVG card.
//
// The card is meant to be produced by a workflow, committed to a repository
// and served from a README. That destination fixes two constraints that shape
// everything in this package.
//
// It has to be self-contained. GitHub serves README images through its camo
// proxy, which strips scripts and will not fetch anything the document points
// at, so there is no stylesheet, no webfont, no <image href> and no script
// here. Only inline markup and fonts the reader already has survive the trip.
//
// And it has to be byte-identical for identical input. A workflow that commits
// the file on every run would otherwise produce a diff a day out of nothing,
// so nothing here reads the clock, iterates a map or invents an identifier.
//
// There is more than one card. A layout is a way of arranging the same data,
// registered in layouts.go and chosen through Options.Layout; every layout
// belongs to one of two visual families, "chronicle" (this package's own look)
// and "github" (a box that passes for GitHub's own UI in a profile README).
package render

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Card is the data the renderer draws. The caller fills it from collected
// points; this package neither fetches nor computes anything.
type Card struct {
	Login string // the account the card is about, required
	Name  string // display name, falls back to Login

	Description string // the account bio, drawn under the title

	Stars     int // stars across all public repositories
	Forks     int
	Followers int
	Repos     int // public repository count

	Contributions int // contributions in the last year

	// Views, UniqueVisitors and Clones are totals over TrafficWindowDays,
	// which is GitHub's rolling window and the only period it will report.
	Views             int
	UniqueVisitors    int
	Clones            int
	TrafficWindowDays int

	// Activity in the last year, as GitHub's contribution collection counts
	// it: commits authored, pull requests opened, reviews given, issues opened.
	Commits      int
	PullRequests int
	Reviews      int
	Issues       int

	// Sparkline is one contribution count per day, oldest first, ending today.
	Sparkline []int

	// Languages is the byte count per language across the account's
	// repositories. Order does not matter; the renderer ranks it.
	Languages []Language

	TopRepos []TopRepo
}

// Language is one entry of the language share.
type Language struct {
	Name  string
	Bytes int64
	Color string // "#rrggbb"; empty falls back to the built-in Linguist table
}

// TopRepo is one row of the ranked list.
type TopRepo struct {
	Name     string
	Language string
	Stars    int
}

// Options control size and appearance.
type Options struct {
	Theme    string // "dark", "light" or "auto"; empty means "auto"
	Width    int    // layout-specific default; badge-row derives it from content
	Title    string // heading, defaults to the account name or login
	MaxRepos int    // rows in the ranked list, default 5

	// Layout names one of Layouts(); empty means "summary".
	Layout string
	// Fields is the ordered list of what the layout shows, from Fields();
	// empty means the layout's default set. A field the layout does not
	// support is skipped, an unknown one is an error.
	Fields []string
}

const (
	defaultWidth    = 495
	minWidth        = 300
	defaultMaxRepos = 5
	maxLanguages    = 8

	pad     = 25.0
	rowStep = 22.0
)

// Palettes. Held as constants rather than a table because a theme is a
// decision, not data: adding one means writing the drawing rules for it.
//
// The chronicle family is this package's own look. The github family is
// GitHub Primer, dark and light, taken from the CSS variables github.com
// ships (the same values the owner's existing profile panels use), so a card
// sits in a profile README as one more native box.
const (
	darkBG        = "#0d1117"
	darkPanel     = "#161b22"
	darkBorder    = "#30363d"
	darkTitle     = "#e6edf3"
	darkText      = "#c9d1d9"
	darkMuted     = "#8b949e"
	darkAccent    = "#e3b341"
	darkOnAccent  = "#0d1117"
	darkSparkline = "#58a6ff"
	darkSuccess   = "#3fb950"

	lightBG        = "#ffffff"
	lightPanel     = "#f6f8fa"
	lightBorder    = "#d0d7de"
	lightTitle     = "#1f2328"
	lightText      = "#24292f"
	lightMuted     = "#57606a"
	lightAccent    = "#9a6700"
	lightOnAccent  = "#ffffff"
	lightSparkline = "#0969da"
	lightSuccess   = "#1a7f37"

	ghDarkPanel  = "#151b23"
	ghDarkBorder = "#3d444d"
	ghDarkFG     = "#f0f6fc"
	ghDarkMuted  = "#9198a1"
	ghDarkAccent = "#4493f8"

	ghLightPanel  = "#f6f8fa"
	ghLightBorder = "#d1d9e0"
	ghLightFG     = "#1f2328"
	ghLightMuted  = "#59636e"
	ghLightAccent = "#0969da"
)

type palette struct {
	bg, panel, border, title, text, muted, accent, onAccent, spark, success string
	// heat is the contribution calendar ramp, empty first, so that the
	// heatmap is recognizable at a glance.
	heat [5]string
}

var (
	// The two contribution ramps are GitHub's own, read off github.com/jmrplens
	// on 2026-09-14 in a real browser: the computed background of a square at
	// each `data-level` 0 to 4, taken once with the page in the dark theme and
	// once in the light one (plan/2026-09-14-ramp/ghgrid2.mjs, a Playwright
	// context per colorScheme). GitHub repaints the calendar from time to time
	// and both ramps had moved since they were last copied here: dark level 1
	// in particular went from #0e4429 to a much darker #033a16, and the light
	// ramp from #ebedf0/#9be9a8/#40c463/#30a14e/#216e39. Remeasure rather than
	// remember, and keep cmd/internal/dashboards.calendarShades in step: it
	// carries the same four greens, over a lighter empty day on purpose, for
	// the reason given there.
	darkHeat  = [5]string{"#151b23", "#033a16", "#196c2e", "#2ea043", "#56d364"}
	lightHeat = [5]string{"#eff2f5", "#aceebb", "#4ac26b", "#2da44e", "#116329"}

	darkPalette = palette{
		darkBG, darkPanel, darkBorder, darkTitle, darkText, darkMuted,
		darkAccent, darkOnAccent, darkSparkline, darkSuccess, darkHeat,
	}
	lightPalette = palette{
		lightBG, lightPanel, lightBorder, lightTitle, lightText, lightMuted,
		lightAccent, lightOnAccent, lightSparkline, lightSuccess, lightHeat,
	}

	ghDarkPalette = palette{
		darkBG, ghDarkPanel, ghDarkBorder, ghDarkFG, ghDarkFG, ghDarkMuted,
		ghDarkAccent, darkOnAccent, ghDarkAccent, darkSuccess, darkHeat,
	}
	ghLightPalette = palette{
		lightBG, ghLightPanel, ghLightBorder, ghLightFG, ghLightFG, ghLightMuted,
		ghLightAccent, lightOnAccent, ghLightAccent, lightSuccess, lightHeat,
	}
)

// family is one visual language: its two palettes and its typography.
type family struct {
	name        string
	dark, light palette
	baseCSS     string
}

var (
	chronicleFamily = family{"chronicle", darkPalette, lightPalette, chronicleCSS}
	githubFamily    = family{"github", ghDarkPalette, ghLightPalette, githubCSS}
)

// ErrTheme is returned for a theme this package cannot draw.
var ErrTheme = errors.New("render: unknown theme")

// SVG renders the card. It returns a complete standalone <svg> document.
//
// Options is taken by pointer because it is too big to copy per call; a nil
// one means every default, so a caller with nothing to say passes nothing.
func SVG(c *Card, o *Options) ([]byte, error) {
	if o == nil {
		o = &Options{}
	}
	if strings.TrimSpace(c.Login) == "" {
		return nil, errors.New("render: card has no login")
	}
	theme := o.Theme
	if theme == "" {
		theme = "auto"
	}
	if theme != "auto" && theme != "dark" && theme != "light" {
		return nil, fmt.Errorf("%w: %q", ErrTheme, o.Theme)
	}
	def, err := findLayout(o.Layout)
	if err != nil {
		return nil, err
	}
	fields, err := resolveFields(def, o.Fields)
	if err != nil {
		return nil, err
	}
	width := float64(o.Width)
	if o.Width == 0 {
		width = float64(def.width)
	}
	if def.minWidth > 0 && width < float64(def.minWidth) {
		return nil, fmt.Errorf("render: width %d is below the %d minimum of layout %q", o.Width, def.minWidth, def.Name)
	}
	maxRepos := o.MaxRepos
	if maxRepos <= 0 {
		maxRepos = defaultMaxRepos
	}

	s := spec{
		theme:  theme,
		width:  width,
		title:  heading(c, o),
		titled: o.Title != "",
		fields: fields,
		nums:   metricsOf(c, fields),
		repos:  rank(c.TopRepos, maxRepos),
		langs:  rankLanguages(c.Languages, maxLanguages),
	}
	if !s.has(fieldTopRepos) {
		s.repos = nil
	}
	if !s.has(fieldLanguages) {
		s.langs = nil
	}
	var b strings.Builder
	b.Grow(8192)
	def.draw(&b, c, &s)
	b.WriteString("</svg>\n")
	return []byte(b.String()), nil
}

// spec is what a layout draws from: the options already validated and the
// data already ranked, so a layout is only geometry.
type spec struct {
	theme  string
	width  float64
	title  string
	titled bool // the caller set Options.Title; a github layout keeps its own otherwise
	fields []string
	nums   []metric
	repos  []TopRepo
	langs  []langShare
}

func (s *spec) has(field string) bool {
	return slices.Contains(s.fields, field)
}

// rank orders the list here rather than trusting the caller's order, because a
// caller that built it from a map would otherwise leak that iteration order
// into a file meant to be byte-identical between runs.
func rank(in []TopRepo, limit int) []TopRepo {
	out := make([]TopRepo, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Stars != out[j].Stars {
			return out[i].Stars > out[j].Stars
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// langShare is a language with its share of the total, in percent.
type langShare struct {
	Name  string
	Color string
	Bytes int64
	Share float64
}

// rankLanguages sorts by size and folds whatever does not fit into "Other",
// so the bar still adds up to the whole and the legend stays readable.
func rankLanguages(in []Language, limit int) []langShare {
	var total int64
	kept := make([]Language, 0, len(in))
	for _, l := range in {
		if l.Bytes <= 0 || strings.TrimSpace(l.Name) == "" {
			continue
		}
		total += l.Bytes
		kept = append(kept, l)
	}
	if total == 0 {
		return nil
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Bytes != kept[j].Bytes {
			return kept[i].Bytes > kept[j].Bytes
		}
		return kept[i].Name < kept[j].Name
	})
	out := make([]langShare, 0, limit+1)
	var rest int64
	for i, l := range kept {
		if i >= limit {
			rest += l.Bytes
			continue
		}
		out = append(out, langShare{l.Name, safeColor(l.Color, l.Name), l.Bytes, 100 * float64(l.Bytes) / float64(total)})
	}
	if rest > 0 {
		out = append(out, langShare{"Other", languageColor(""), rest, 100 * float64(rest) / float64(total)})
	}
	return out
}

// safeColor lets a caller-supplied color through only when it is exactly a
// hex triplet. Anything else is user input on its way into an attribute.
func safeColor(color, lang string) string {
	if len(color) == 7 && color[0] == '#' {
		ok := true
		for _, r := range color[1:] {
			hex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			ok = ok && hex
		}
		if ok {
			return strings.ToLower(color)
		}
	}
	return languageColor(lang)
}

// openDoc writes everything up to and including the stylesheet. The caller
// draws the background: the two families frame a card differently.
func openDoc(b *strings.Builder, fam *family, s *spec, height float64, desc, extraCSS string) {
	fmt.Fprintf(b, `<svg xmlns="http://www.w3.org/2000/svg" width="%s" height="%s" viewBox="0 0 %s %s" role="img" aria-labelledby="ghcTitle ghcDesc">`+"\n",
		num(s.width), num(height), num(s.width), num(height))

	// A chart tells a screen reader nothing, so the numbers are spelled out in
	// full here. They are not abbreviated the way the drawing is: "1.8k" is a
	// worse thing to hear than "1810".
	fmt.Fprintf(b, "<title id=\"ghcTitle\">%s</title>\n", esc(s.title))
	fmt.Fprintf(b, "<desc id=\"ghcDesc\">%s</desc>\n", esc(desc))

	b.WriteString("<style>\n")
	b.WriteString(themeCSS(fam, s.theme))
	b.WriteString(fam.baseCSS)
	b.WriteString(extraCSS)
	b.WriteString("</style>\n")
}

// cardBG is the rounded, bordered card both families start from.
func cardBG(b *strings.Builder, width, height float64) {
	fmt.Fprintf(b, `<rect class="bg" x="0.5" y="0.5" width="%s" height="%s" rx="6"/>`+"\n",
		num(width-1), num(height-1))
}

// themeCSS writes the palette out as literal colors, once per theme.
//
// Custom properties would be tidier, but a renderer that does not implement
// them drops the declaration and paints the card black on black, which is what
// librsvg does today. Literal colors with a media override degrade the other
// way, to the light card, which is still readable.
//
// "auto" is worth having because a README image is rendered by the reader's
// own browser and camo passes prefers-color-scheme through, so one committed
// file follows whoever is looking at it and there is no second URL to keep in
// step with the first.
func themeCSS(fam *family, theme string) string {
	switch theme {
	case "dark":
		return colorCSS(&fam.dark)
	case "light":
		return colorCSS(&fam.light)
	default:
		return colorCSS(&fam.light) +
			"@media (prefers-color-scheme: dark){" + colorCSS(&fam.dark) + "}\n"
	}
}

func colorCSS(p *palette) string {
	return ".bg{fill:" + p.bg + ";stroke:" + p.border + "}\n" +
		".panel{fill:" + p.panel + "}\n" +
		".t,.v,.big{fill:" + p.title + "}\n" +
		".d,.l,.h,.s{fill:" + p.muted + "}\n" +
		".n,.c{fill:" + p.text + "}\n" +
		".star,.pill-v{fill:" + p.accent + "}\n" +
		".on-accent{fill:" + p.onAccent + "}\n" +
		".line{stroke:" + p.spark + "}\n" +
		".area{fill:" + p.spark + "}\n" +
		".axis,.edge{stroke:" + p.border + "}\n" +
		".track{fill:" + p.border + "}\n" +
		".ok{fill:" + p.success + "}\n" +
		".fg{fill:" + p.title + "}\n" +
		".h0{fill:" + p.heat[0] + "}.h1{fill:" + p.heat[1] + "}.h2{fill:" + p.heat[2] + "}.h3{fill:" + p.heat[3] + "}.h4{fill:" + p.heat[4] + "}\n"
}

// Only fonts the reader already has. A webfont would be a second request, and
// camo does not make second requests.
const (
	fontSans = "-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif"
	fontMono = "ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,'Liberation Mono',monospace"
)

const chronicleCSS = `text{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif}
.mono{font-family:` + fontMono + `}
.t{font-size:18px;font-weight:600}
.d{font-size:11px}
.v{font-size:16px;font-weight:600}
.big{font-size:24px;font-weight:700}
.l{font-size:11px}
.h{font-size:10px;font-weight:600;letter-spacing:0.08em}
.n{font-size:12px}
.s{font-size:11px}
.c{font-size:12px}
.line{fill:none;stroke-width:2;stroke-linecap:round;stroke-linejoin:round}
.area{opacity:0.14;stroke:none}
.axis{stroke-width:1}
.edge{fill:none;stroke-width:1}
`

// The github family sets its numbers in a monospace stack, the readout look
// the owner's existing panels have; everything else is GitHub's own sans stack.
const githubCSS = `text{font-family:` + fontSans + `}
.mono{font-family:` + fontMono + `}
.t{font-size:15px;font-weight:600}
.d{font-size:11px}
.v{font-size:22px;font-weight:600;font-family:` + fontMono + `}
.big{font-size:20px;font-weight:600;font-family:` + fontMono + `}
.l{font-size:11px}
.h{font-size:14px;font-weight:600}
.n{font-size:13px}
.s{font-size:11px}
.c{font-size:13px;font-weight:500;font-family:` + fontMono + `}
.line{fill:none;stroke-width:2;stroke-linecap:round;stroke-linejoin:round}
.area{opacity:0.12;stroke:none}
.axis{stroke-width:1}
.edge{fill:none;stroke-width:1}
`

func heading(c *Card, o *Options) string {
	if o.Title != "" {
		return o.Title
	}
	if c.Name != "" {
		return c.Name
	}
	return c.Login
}

// describe spells the card out for a screen reader, in the order the card
// shows it and in full numbers.
func describe(c *Card, s *spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub summary for %s", c.Login)
	describeNumbers(&b, s.nums)
	describeLanguages(&b, s.langs)
	describeRepos(&b, s.repos)
	if s.has(fieldSparkline) {
		describeSparkline(&b, c.Sparkline)
	}
	return b.String()
}

// describeNumbers finishes the opening sentence with every number the card
// shows, and ends it even when there are none.
func describeNumbers(b *strings.Builder, nums []metric) {
	for i, m := range nums {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%d %s", m.value, m.spoken)
	}
	b.WriteString(".")
}

// describeLanguages is the sentence for the language shares, each rounded to
// a whole percent.
func describeLanguages(b *strings.Builder, langs []langShare) {
	if len(langs) == 0 {
		return
	}
	b.WriteString(" Languages by bytes: ")
	for i, l := range langs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%s %s%%", l.Name, num(math.Round(l.Share)))
	}
	b.WriteString(".")
}

// describeRepos is the sentence for the most starred repositories.
func describeRepos(b *strings.Builder, repos []TopRepo) {
	if len(repos) == 0 {
		return
	}
	b.WriteString(" Most starred repositories: ")
	for i, r := range repos {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%s with %d stars", r.Name, r.Stars)
		if r.Language != "" {
			fmt.Fprintf(b, " in %s", r.Language)
		}
	}
	b.WriteString(".")
}

// describeSparkline is the sentence for the contribution line: how many days
// it covers and its highest day.
func describeSparkline(b *strings.Builder, days []int) {
	if len(days) == 0 {
		return
	}
	peak := 0
	for _, v := range days {
		if v > peak {
			peak = v
		}
	}
	fmt.Fprintf(b, " Contributions per day over the last %d days, peaking at %d.", len(days), peak)
}

// drawSparkline draws the line and its area into the box. animated adds the
// draw-itself classes; the CSS behind them is the layout's business.
func drawSparkline(b *strings.Builder, values []int, x0, y0, w, h float64, animated bool) {
	pts := sparkPoints(values, x0, y0, w, h)
	var area, line strings.Builder
	fmt.Fprintf(&area, "M%s,%s", num(x0), num(y0+h))
	for i, p := range pts {
		if i > 0 {
			line.WriteString(" ")
		}
		line.WriteString(num(p.x) + "," + num(p.y))
		fmt.Fprintf(&area, " L%s,%s", num(p.x), num(p.y))
	}
	fmt.Fprintf(&area, " L%s,%sZ", num(x0+w), num(y0+h))

	if animated {
		fmt.Fprintf(b, `<path class="area fade" d="%s"/>`+"\n", area.String())
		// pathLength normalizes the dash to one unit so the CSS does not have
		// to know how long the line is.
		fmt.Fprintf(b, `<polyline class="line draw" pathLength="1" points="%s"/>`+"\n", line.String())
		return
	}
	fmt.Fprintf(b, `<path class="area" d="%s"/>`+"\n", area.String())
	fmt.Fprintf(b, `<polyline class="line" points="%s"/>`+"\n", line.String())
}

type point struct{ x, y float64 }

// sparkPoints scales the series into the box. The three degenerate series all
// have to draw something: an empty one and an all-zero one flatten onto the
// baseline, and a single day is stretched across the width rather than being a
// line of zero length that renderers drop.
func sparkPoints(values []int, x, y, w, h float64) []point {
	switch len(values) {
	case 0:
		values = []int{0, 0}
	case 1:
		values = []int{values[0], values[0]}
	}
	peak := 0
	for _, v := range values {
		if v > peak {
			peak = v
		}
	}
	if peak <= 0 {
		peak = 1 // a flat series sits on the baseline instead of dividing by zero
	}
	step := w / float64(len(values)-1)
	pts := make([]point, len(values))
	for i, v := range values {
		if v < 0 {
			v = 0
		}
		pts[i] = point{
			x: x + float64(i)*step,
			y: y + h - (float64(v)/float64(peak))*h,
		}
	}
	return pts
}

// starPath is a filled star on a 16 by 16 grid.
//
// A star drawn as a path rather than the U+2605 glyph: the font stack is
// whatever the reader has, and a missing glyph would show as tofu with no way
// to supply one from inside the document.
const starPath = "M8 .25a.75.75 0 0 1 .673.418l1.882 3.815 4.21.612a.75.75 0 0 1 .416 1.279l-3.046 2.97.719 4.192a.75.75 0 0 1-1.088.791L8 12.347l-3.766 1.98a.75.75 0 0 1-1.088-.79l.72-4.194L.818 6.374a.75.75 0 0 1 .416-1.28l4.21-.611L7.327.668A.75.75 0 0 1 8 .25Z"

func star(b *strings.Builder, x, y, scale float64) {
	fmt.Fprintf(b, `<path class="star" transform="translate(%s %s) scale(%s)" d="%s"/>`+"\n",
		num(x), num(y), num(scale), starPath)
}

// languageColor is a lookup, never a range: the iteration order of a map must
// not reach the file, so nothing ranges over linguistColors. The values are
// GitHub Linguist's; a language with no entry here is drawn in neutral gray.
func languageColor(lang string) string {
	if c, ok := linguistColors[lang]; ok {
		return c
	}
	return "#8b949e"
}

var linguistColors = map[string]string{
	"Go":         "#00add8",
	"Python":     "#3572a5",
	"JavaScript": "#f1e05a",
	"TypeScript": "#3178c6",
	"Shell":      "#89e051",
	"C":          "#555555",
	"C++":        "#f34b7d",
	"C#":         "#178600",
	"Rust":       "#dea584",
	"Java":       "#b07219",
	"Ruby":       "#701516",
	"PHP":        "#4f5d95",
	"Lua":        "#000080",
	"Swift":      "#f05138",
	"Kotlin":     "#a97bff",
	"TeX":        "#3d6117",
	"HTML":       "#e34c26",
	"CSS":        "#563d7c",
	"Astro":      "#ff5a03",
	"MDX":        "#fcb32c",
	"MATLAB":     "#e16737",
	"Dockerfile": "#384d54",
	"Makefile":   "#427819",
}

func text(b *strings.Builder, x, y float64, class, anchor, s string) {
	if s == "" {
		return
	}
	fmt.Fprintf(b, `<text class="%s" x="%s" y="%s" text-anchor="%s">%s</text>`+"\n",
		class, num(x), num(y), anchor, esc(s))
}

// esc escapes everything that reaches the document. A repository description
// is user input on its way into markup, so it goes through here or it does not
// go in at all.
func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // a bytes.Buffer cannot fail
	return b.String()
}

// compactUnits is the ladder compact climbs. The last rung is the last one
// there is: a count past a thousand billion keeps the B and grows the
// mantissa, which is honest and is a number no account will reach.
var compactUnits = [...]string{"", "k", "M", "B"}

// compact shortens a count to what fits under a label: 1810 as 1.8k, 1200000
// as 1.2M.
func compact(n int) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	// One rung at a time, and the comparison is against the rounded value
	// rather than the raw one: 999950 rounds to 1000.0k, which is a way of
	// writing 1M. The climb stops at the last rung it has a name for, so a
	// count past a thousand billion reads 1099.5B rather than being divided
	// again and labeled 1.1B, which is what it used to say.
	//
	// What climbs is the raw value: the rounding decides only whether to
	// climb, and the one that is shown happens once, at the end. Rounding at
	// every rung and carrying that forward is what made 1049950000 read
	// 1.1B, because 1049.95M was first rounded to 1050M and 1.05 then rounded
	// up again.
	v, rung := float64(n), 0
	for rung < len(compactUnits)-1 && round1(v) >= 1000 {
		v, rung = v/1000, rung+1
	}
	return sign + num(round1(v)) + compactUnits[rung]
}

// round1 rounds to the one decimal a compact count is shown at.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// grouped writes a count in full with thousands separators, the way GitHub's
// own UI does: the github family has room for it and the exact figure is the
// point of a readout.
func grouped(n int) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	head := len(s) % 3
	if head > 0 {
		b.WriteString(s[:head])
	}
	for i := head; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return sign + b.String()
}

// num formats a coordinate. Two decimals and no exponent, so the same input
// always writes the same bytes.
func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.HasSuffix(s, ".00") {
		return s[:len(s)-3]
	}
	return strings.TrimSuffix(s, "0") // 1.80 is 1.8, but 600.00 is not 60
}

// fit truncates to an ellipsis so a long name cannot run off the card.
// Truncation happens before escaping: cutting escaped text could split an
// entity in half.
func fit(s string, size, limit float64) string {
	if limit <= 0 {
		return ""
	}
	if textWidth(s, size) <= limit {
		return s
	}
	runes := []rune(s)
	ell := textWidth("…", size)
	for i := len(runes); i > 0; i-- {
		if textWidth(string(runes[:i]), size)+ell <= limit {
			return strings.TrimRight(string(runes[:i]), " ") + "…"
		}
	}
	return ""
}

// textWidth approximates a rendered width. There is no font here to measure
// against, and the alternative to an approximation is text that overlaps the
// column next to it.
func textWidth(s string, size float64) float64 {
	var w float64
	for _, r := range s {
		w += runeWidth(r) * size
	}
	return w
}

// monoWidth is textWidth for the monospace stack, where every glyph is the
// same advance.
func monoWidth(s string, size float64) float64 {
	return float64(len([]rune(s))) * 0.6 * size
}

func runeWidth(r rune) float64 {
	switch {
	case r == ' ':
		return 0.28
	case strings.ContainsRune("ijltfrI.,:;'!|[](){}", r):
		return 0.30
	case r == 'W' || r == 'M' || r == 'm' || r == '@':
		return 0.90
	case r >= 'A' && r <= 'Z':
		return 0.68
	case r >= '0' && r <= '9':
		return 0.56
	case r >= 0x2e80: // CJK and friends are square
		return 1.00
	}
	return 0.55
}
