package render

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Field identifiers. A layout shows what Options.Fields asks for, in that
// order, out of this vocabulary.
const (
	fieldStars         = "stars"
	fieldForks         = "forks"
	fieldFollowers     = "followers"
	fieldRepos         = "repos"
	fieldContributions = "contributions"
	fieldViews         = "views"
	fieldVisitors      = "visitors"
	fieldClones        = "clones"
	fieldCommits       = "commits"
	fieldPullRequests  = "pull_requests"
	fieldReviews       = "reviews"
	fieldIssues        = "issues"
	fieldLanguages     = "languages"
	fieldTopRepos      = "top_repos"
	fieldSparkline     = "sparkline"
)

// fieldVocabulary is every identifier Options.Fields accepts, in the order
// Fields() reports them. A slice, not a map: the error message lists it.
var fieldVocabulary = []string{
	fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions,
	fieldViews, fieldVisitors, fieldClones,
	fieldCommits, fieldPullRequests, fieldReviews, fieldIssues,
	fieldLanguages, fieldTopRepos, fieldSparkline,
}

// numericFields are the ones that are a single number. The other three are
// blocks (a bar, a list, a chart) and each layout places them its own way.
var numericFields = fieldVocabulary[:12]

// Fields returns the identifiers Options.Fields accepts.
func Fields() []string {
	out := make([]string, len(fieldVocabulary))
	copy(out, fieldVocabulary)
	return out
}

// Layout describes one registered layout.
type Layout struct {
	Name        string
	Family      string // "chronicle" or "github"
	Description string
	Animated    bool
	Fields      []string // the default set, in drawing order
	Supports    []string // every field the layout can show
}

type layoutDef struct {
	Layout
	width, minWidth int // minWidth 0 means Options.Width is ignored
	// draw writes the whole document. It is handed the spec by pointer and
	// may finish it: the github layouts supply their own heading when the
	// caller set none, and badge-row derives the width from its content.
	// SVG must not read the spec back afterwards, and does not.
	draw func(b *strings.Builder, c *Card, s *spec)
}

var (
	// ErrLayout is returned for a layout name that is not registered.
	ErrLayout = errors.New("render: unknown layout")
	// ErrField is returned for a field name outside Fields().
	ErrField = errors.New("render: unknown field")
)

var allNumeric = numericFields

// layouts is the registry, in the order Layouts() reports. Order matters for
// the same reason everything else here is a slice: the output must not depend
// on a map. In each entry the lines above the gap are the public Layout, and
// the line below it is how that layout is drawn.
var layouts = []layoutDef{
	{
		Name: "summary", Family: "chronicle",
		Description: "The original card: title, two rows of numbers, a sparkline and the most starred repositories.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions, fieldViews, fieldVisitors, fieldSparkline, fieldTopRepos},
		Supports:    join(allNumeric, fieldLanguages, fieldSparkline, fieldTopRepos),

		width: defaultWidth, minWidth: minWidth, draw: drawSummary,
	},
	{
		Name: "github-stats", Family: "github", Animated: true,
		Description: "GitHub's own box: a header band, rows of four monospace numbers that count up and a language share bar that grows in beside its legend.",
		Fields:      []string{fieldRepos, fieldStars, fieldForks, fieldFollowers, fieldCommits, fieldPullRequests, fieldViews, fieldClones, fieldLanguages},
		Supports:    join(allNumeric, fieldLanguages, fieldTopRepos, fieldSparkline),

		width: 800, minWidth: 600, draw: drawGithubStats,
	},
	{
		Name: "github-compact", Family: "github",
		Description: "One row of monospace numbers under a thin header band.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldCommits},
		Supports:    allNumeric,

		width: defaultWidth, minWidth: minWidth, draw: drawGithubCompact,
	},
	{
		Name: "badge-row", Family: "chronicle",
		Description: "A row of 20px pill badges, one per number, for a README line; the width follows the content.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions},
		Supports:    allNumeric,

		width: 0, minWidth: 0, draw: drawBadgeRow,
	},
	{
		Name: "wide-banner", Family: "chronicle", Animated: true,
		Description: "A full-width 60px banner: login on the left, numbers spread across, the sparkline drawing itself behind them.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldContributions, fieldSparkline},
		Supports:    join(allNumeric, fieldSparkline),

		width: 800, minWidth: 500, draw: drawWideBanner,
	},
	{
		Name: "sparkline-hero", Family: "chronicle", Animated: true,
		Description: "The contribution sparkline is the whole card, with up to three numbers overlaid; the line draws itself.",
		Fields:      []string{fieldContributions, fieldStars, fieldFollowers, fieldSparkline},
		Supports:    join(allNumeric, fieldSparkline),

		width: defaultWidth, minWidth: minWidth, draw: drawSparklineHero,
	},
	{
		Name: "language-ring", Family: "github", Animated: true,
		Description: "A donut of language shares with the legend beside it and a row of headline numbers; each slice draws itself and the legend follows.",
		Fields:      []string{fieldLanguages, fieldStars, fieldRepos},
		Supports:    join(allNumeric, fieldLanguages),

		width: defaultWidth, minWidth: 400, draw: drawLanguageRing,
	},
	{
		Name: "repo-list", Family: "github",
		Description: "The most starred repositories as the main content: language dot, stars and a bar per row, totals underneath.",
		Fields:      []string{fieldTopRepos, fieldStars, fieldForks, fieldRepos},
		Supports:    join(allNumeric, fieldTopRepos),

		width: defaultWidth, minWidth: minWidth, draw: drawRepoList,
	},
	{
		Name: "activity-heatmap", Family: "github", Animated: true,
		Description: "The last twelve weeks of the contribution calendar as GitHub's green squares, week by week from the left, with up to three numbers beside it.",
		Fields:      []string{fieldSparkline, fieldContributions, fieldCommits, fieldPullRequests},
		Supports:    join(allNumeric, fieldSparkline),

		width: defaultWidth, minWidth: 400, draw: drawActivityHeatmap,
	},
	{
		Name: "animated-counters", Family: "chronicle", Animated: true,
		Description: "Numbers that count up on load over a sparkline that draws itself; settles to the static card.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions, fieldViews, fieldSparkline},
		Supports:    join(allNumeric, fieldSparkline),

		width: defaultWidth, minWidth: minWidth, draw: drawAnimatedCounters,
	},
	{
		Name: "terminal", Family: "chronicle", Animated: true,
		Description: "A terminal window with the project's mark: one line of output per number, each number typed in, and a cursor blinking at the prompt.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions, fieldTopRepos},
		Supports:    join(allNumeric, fieldTopRepos),

		width: defaultWidth, minWidth: 360, draw: drawTerminal,
	},
	{
		Name: "ticker", Family: "chronicle", Animated: true,
		Description: "A band of pills, one per number and one per repository, scrolling from right to left without a seam, at a fixed speed, so a pass takes as long as the content is wide.",
		Fields:      []string{fieldStars, fieldForks, fieldFollowers, fieldRepos, fieldContributions, fieldCommits, fieldViews, fieldTopRepos},
		Supports:    join(allNumeric, fieldTopRepos),

		width: 800, minWidth: 400, draw: drawTicker,
	},
	{
		Name: "language-bars", Family: "github", Animated: true,
		Description: "One bar per language, each growing from its own left edge after the one above it, with the name and the share arriving behind it.",
		Fields:      []string{fieldLanguages},
		Supports:    join(allNumeric, fieldLanguages),

		width: defaultWidth, minWidth: 360, draw: drawLanguageBars,
	},
}

func join(base []string, more ...string) []string {
	out := make([]string, 0, len(base)+len(more))
	out = append(out, base...)
	return append(out, more...)
}

// Layouts returns the registered layouts in registry order. The slices are
// copies; a caller cannot reach the registry through them.
func Layouts() []Layout {
	out := make([]Layout, len(layouts))
	for i := range layouts {
		out[i] = layouts[i].Layout
		out[i].Fields = join(layouts[i].Fields)
		out[i].Supports = join(layouts[i].Supports)
	}
	return out
}

func findLayout(name string) (*layoutDef, error) {
	if name == "" {
		name = "summary"
	}
	for i := range layouts {
		if layouts[i].Name == name {
			return &layouts[i], nil
		}
	}
	names := make([]string, len(layouts))
	for i := range layouts {
		names[i] = layouts[i].Name
	}
	return nil, fmt.Errorf("%w: %q (valid: %s)", ErrLayout, name, strings.Join(names, ", "))
}

// resolveFields turns the request into what the layout will draw: the
// default set when empty, otherwise the request in its order with duplicates
// and unsupported fields dropped. An unknown name is an error rather than a
// silent skip, because a typo would otherwise remove a number and nobody
// would notice until the card was already committed.
func resolveFields(def *layoutDef, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return join(def.Fields), nil
	}
	out := make([]string, 0, len(requested))
	for _, f := range requested {
		if !slices.Contains(fieldVocabulary, f) {
			return nil, fmt.Errorf("%w: %q (valid: %s)", ErrField, f, strings.Join(fieldVocabulary, ", "))
		}
		if !slices.Contains(def.Supports, f) || slices.Contains(out, f) {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// metric is one numeric field ready to draw: its value and the three ways a
// layout may label it.
type metric struct {
	key    string
	value  int
	label  string // "Contributions (1y)", the chronicle family's label
	long   string // "Commits / yr", the github family's label
	short  string // "commits", for badges and banners
	spoken string // "commits in the last year", for the description
}

// metricsOf resolves the numeric fields, in order. The traffic labels carry
// the window, because "1810 views" means nothing without "over 14 days".
func metricsOf(c *Card, fields []string) []metric {
	window := "traffic"
	windowShort := "traffic"
	if c.TrafficWindowDays > 0 {
		window = strconv.Itoa(c.TrafficWindowDays) + "d"
		windowShort = window
	}
	spokenWindow := "over the traffic window"
	if c.TrafficWindowDays > 0 {
		spokenWindow = "in the last " + strconv.Itoa(c.TrafficWindowDays) + " days"
	}
	out := make([]metric, 0, len(fields))
	for _, f := range fields {
		var m metric
		switch f {
		case fieldStars:
			m = metric{f, c.Stars, "Stars", "Stars", "stars", "stars"}
		case fieldForks:
			m = metric{f, c.Forks, "Forks", "Forks", "forks", "forks"}
		case fieldFollowers:
			m = metric{f, c.Followers, "Followers", "Followers", "followers", "followers"}
		case fieldRepos:
			m = metric{f, c.Repos, "Repositories", "Repositories", "repos", "public repositories"}
		case fieldContributions:
			m = metric{f, c.Contributions, "Contributions (1y)", "Contributions / yr", "contribs", "contributions in the last year"}
		case fieldViews:
			m = metric{f, c.Views, "Views (" + window + ")", "Repo views / " + windowShort, "views", "repository views " + spokenWindow}
		case fieldVisitors:
			m = metric{f, c.UniqueVisitors, "Visitors (" + window + ")", "Visitors / " + windowShort, "visitors", "unique visitors " + spokenWindow}
		case fieldClones:
			m = metric{f, c.Clones, "Clones (" + window + ")", "Clones / " + windowShort, "clones", "clones " + spokenWindow}
		case fieldCommits:
			m = metric{f, c.Commits, "Commits (1y)", "Commits / yr", "commits", "commits in the last year"}
		case fieldPullRequests:
			m = metric{f, c.PullRequests, "Pull requests (1y)", "Pull requests / yr", "PRs", "pull requests in the last year"}
		case fieldReviews:
			m = metric{f, c.Reviews, "Reviews (1y)", "Reviews / yr", "reviews", "reviews in the last year"}
		case fieldIssues:
			m = metric{f, c.Issues, "Issues (1y)", "Issues / yr", "issues", "issues in the last year"}
		default:
			continue
		}
		out = append(out, m)
	}
	return out
}
