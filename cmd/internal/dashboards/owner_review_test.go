package dashboards

import (
	"regexp"
	"strings"
	"testing"
)

// The owner's review of the published dashboard on 2026-09-12 asked for
// panels the first review had not: the profile's calendar and radar drawn
// the way GitHub draws them, the discussions one by one, the badge shelf,
// and the brand header as a masthead. Each test here pins one of those to
// the rendered JSON.

// TestContributionCalendarIsTheGitHubGrid: the grid the profile draws,
// measured on the profile page on 2026-09-14 and pinned here. A status
// history of one field per weekday and one row per week, binned from a
// Sunday; the shade a function of the day's own count through the fifths of
// the busiest day of the year, which is the rule that reproduced all 366 of
// GitHub's own squares, and not the quartile among the days that had
// anything, which painted 54 days the darkest green where the profile paints
// 14 and gave one count two shades; GitHub's dark palette over a gray day;
// three rows named and four blank, as the profile labels them; the cell
// fractions and the ten by five place that make the cell ten pixels square;
// and the year of its own the profile always draws. The three stores that
// cannot pivot a day into a week and a weekday say so in a note.
func TestContributionCalendarIsTheGitHubGrid(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		p := mustPanel(t, rendered(t, store), "Contribution calendar")
		if p["type"] != "status-history" {
			t.Errorf("%s draws the calendar as a %v", store, p["type"])
		}
		calendarQuery(t, store, sqlOf(t, p))
		calendarShape(t, store, p)
	}
	if sql := sqlOf(t, mustPanel(t, rendered(t, "postgres"), "Contribution calendar")); !strings.Contains(sql, "date_bin('7 days', time, TIMESTAMP '1970-01-04')") {
		t.Errorf("the PostgreSQL twin keeps the DataFusion spelling of the week: %s", sql)
	}
	for _, store := range []string{"prometheus", "graphite", "elasticsearch"} {
		if p := mustPanel(t, rendered(t, store), "Contribution calendar"); p["type"] != "text" {
			t.Errorf("%s claims to draw the calendar as a %v", store, p["type"])
		}
	}
}

// calendarQuery is the half of the grid that is the statement: the rows in
// order, the three the profile names, the Sunday the week is binned from and
// the bands the shade is, which are GitHub's and not a quartile of our own.
func calendarQuery(t *testing.T, store, sql string) {
	t.Helper()
	for i, day := range calendarRows {
		if !strings.Contains(sql, `AS "`+day+`"`) {
			t.Errorf("%s: no %q column", store, day)
		}
		if i > 0 && strings.Index(sql, `AS "`+calendarRows[i-1]+`"`) > strings.Index(sql, `AS "`+day+`"`) {
			t.Errorf("%s: %q comes before %q, and the rows read top to bottom", store, day, calendarRows[i-1])
		}
	}
	if named := []string{"Mon", "Wed", "Fri"}; !equalStrings(namedRows(calendarRows), named) {
		t.Errorf("%s: the rows named are %v, and the profile names %v", store, namedRows(calendarRows), named)
	}
	if !strings.Contains(sql, "1970-01-04") {
		t.Errorf("%s: the week is not binned from a Sunday: %s", store, sql)
	}
	if strings.Contains(sql, "NTILE") {
		t.Errorf("%s: the shade is still a quartile of the days that had anything: %s", store, sql)
	}
	for _, band := range []string{
		"WHEN contributions = 0 THEN 0",
		"WHEN contributions * 5 >= 3 * MAX(contributions) OVER () THEN 4",
		"WHEN contributions * 5 >= 2 * MAX(contributions) OVER () THEN 3",
		"WHEN contributions * 5 >= MAX(contributions) OVER () THEN 2",
	} {
		if !strings.Contains(sql, band) {
			t.Errorf("%s: the shade is not GitHub's band %q: %s", store, band, sql)
		}
	}
}

// calendarShape is the other half: the palette, the cell, the place and the
// year of its own, which are what make it read as the profile's grid.
func calendarShape(t *testing.T, store string, p map[string]any) {
	t.Helper()
	defaults := defaultsOf(t, p)
	thresholds, _ := defaults["thresholds"].(map[string]any)
	steps, _ := thresholds["steps"].([]any)
	if len(steps) != 5 {
		t.Fatalf("%s: %d shades, and GitHub draws five", store, len(steps))
	}
	// The four greens as github.com draws them now, read off the profile page
	// twice on 2026-09-14: the computed background of a data-level 1 to 4
	// square, and the same four counted among the pixels of a screenshot of
	// that grid. The ramp this panel carried before (#0e4429, #006d32,
	// #26a641, #39d353) is the one GitHub drew before it repainted the
	// calendar.
	for i, want := range []string{"#21262d", "#033a16", "#196c2e", "#2ea043", "#56d364"} {
		step, _ := steps[i].(map[string]any)
		if step["color"] != want {
			t.Errorf("%s: shade %d is %v, and GitHub's dark theme draws %s", store, i, step["color"], want)
		}
	}
	if raw := asJSON(t, defaults["mappings"]); strings.Contains(raw, "quartile") {
		t.Errorf("%s: a cell still reads as a quartile, which the bands are not: %s", store, raw)
	}
	options, _ := p["options"].(map[string]any)
	if options["showValue"] != "never" {
		t.Errorf("%s: the cells carry their number, which at fifty-three columns is noise", store)
	}
	if options["colWidth"] != 0.77 || options["rowHeight"] != 0.66 {
		t.Errorf("%s: the cell is %v by %v of its slot, and GitHub's square is 0.77 by 0.66 here",
			store, options["colWidth"], options["rowHeight"])
	}
	if p["timeFrom"] != calendarRange {
		t.Errorf("%s: the grid follows the dashboard range (timeFrom %v), and the profile's is always a year",
			store, p["timeFrom"])
	}
	g, _ := p["gridPos"].(map[string]any)
	if g["w"] != 10 || g["h"] != 5 {
		t.Errorf("%s: the grid is %v by %v units, and the cell is only square at 10 by 5", store, g["w"], g["h"])
	}
}

// TestTheCalendarAxisNamesTheWeekAndNotTheMonth: the owner's second look at
// the grid, 2026-09-14. Maximized at 1920 the axis read 2025-10, 2025-10,
// 2025-11, 2025-12, 2025-12 and on, five of the twelve months printed twice.
// A status history builds its own x ticks, one every Nth column, so every
// tick lands on a week and the stride is a function of the pixel width: at
// three weeks a month gets two ticks and a month name is written twice. The
// panel cannot move a tick. It can say what one is written as, and the Sunday
// a column starts on can never repeat, so the format has to carry the day.
func TestTheCalendarAxisNamesTheWeekAndNotTheMonth(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		p := mustPanel(t, rendered(t, store), "Contribution calendar")
		unit, _ := overrideProperty(p, calendarXField, "unit").(string)
		format, ok := strings.CutPrefix(unit, "time:")
		if !ok {
			t.Errorf("%s: the ticks are left to Grafana's interval format (unit %q), which writes a month it cannot place on that month's first week",
				store, unit)
			continue
		}
		if !strings.Contains(format, "D") {
			t.Errorf("%s: ticks are written %q, and without the day of the month two ticks in one month read the same", store, format)
		}
		// The rightmost tick is centered on the last column and cut off at
		// the panel's edge. Measured at 430 on 2026-09-14, where that column
		// is the week of 13 September: written "MMM D" the axis drew
		// "Sep 13" and showed "Sep 1", a whole date twelve days off, so the
		// day has to come first and leave "13 Se" behind instead.
		if strings.HasSuffix(format, "D") {
			t.Errorf("%s: ticks are written %q, and the day is the half the panel edge cuts off, which leaves a date that is wrong rather than one that is plainly unfinished", store, format)
		}
	}
}

// TestTheCalendarSaysThatMaximizingStretchesIt: a status history sizes a cell
// as a fraction of the band its row gets, so the ten pixel square is square
// at the size the panel has on the dashboard and nowhere else. Maximized in a
// 1920 by 900 window the same cells measured 27 by 67 pixels, tall bars, and
// 27 by 84 in a taller one, since the height is the panel's. There is no option
// that caps a cell, so the description is where the reader is told.
func TestTheCalendarSaysThatMaximizingStretchesIt(t *testing.T) {
	t.Parallel()
	desc, _ := mustPanel(t, rendered(t, "influxdb"), "Contribution calendar")["description"].(string)
	for _, want := range []string{"maximized", "stretch"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description does not say what maximizing does to the cells (no %q): %s", want, desc)
		}
	}
}

// namedRows is the rows of the calendar that carry a name: the blanks are
// spaces, which is how a status history draws a row with no label.
func namedRows(rows []string) []string {
	var out []string
	for _, r := range rows {
		if strings.TrimSpace(r) != "" {
			out = append(out, r)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEveryDashboardReadsInUTC: every bucket a query makes is cut in UTC, so
// the axis and the tooltip that name those buckets are read there too. In the
// browser's zone the calendar's tooltip read 02:00 over a row stamped at
// midnight, and a day bin was labeled with the neighboring day for two hours
// of it.
func TestEveryDashboardReadsInUTC(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		if tz := store.Build(nil)["timezone"]; tz != "utc" {
			t.Errorf("%s reads its times in %v", store.Name, tz)
		}
	}
}

// TestStatusHistoryCellFractionsAreOptional: the two fractions are how the
// calendar shapes its cell, and every other status history keeps the default.
func TestStatusHistoryCellFractionsAreOptional(t *testing.T) {
	t.Parallel()
	plain, _ := statusHistory(1, "t", nil, nil, 24, 7, 0, 0, "", nil)["options"].(map[string]any)
	if plain["colWidth"] != 0.8 || plain["rowHeight"] != 0.8 {
		t.Errorf("the default cell is %v by %v of its slot, want 0.8 by 0.8", plain["colWidth"], plain["rowHeight"])
	}
	sized, _ := statusHistory(1, "t", nil, nil, 10, 5, 0, 0, "",
		Opts{"col_width": 0.77, "row_height": 0.66})["options"].(map[string]any)
	if sized["colWidth"] != 0.77 || sized["rowHeight"] != 0.66 {
		t.Errorf("the fractions given were drawn as %v by %v", sized["colWidth"], sized["rowHeight"])
	}
}

// TestContributionMixIsFourSharesOfTheTotal: the radar GitHub draws is four
// percentages of one sum, and every store hands the panel the percentages
// under the four names, computed in its own query or, for Elasticsearch, by
// the panel's own arithmetic over the four counts.
func TestContributionMixIsFourSharesOfTheTotal(t *testing.T) {
	t.Parallel()
	b := &builder{}
	var spec *Panel
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			if p.Title == "Contribution mix (last year)" {
				spec = &p
			}
		}
	}
	if spec == nil {
		t.Fatal("no Contribution mix panel")
	}
	if spec.Kind != "bargauge" {
		t.Errorf("the mix is drawn as a %s", spec.Kind)
	}
	for name, st := range spec.Stores {
		if st.Q == nil {
			t.Errorf("%s has no answer for the mix, and every store holds the four totals", name)
			continue
		}
		if n := namedValues(name, st, spec); n < 4 {
			t.Errorf("%s names %d of the four parts", name, n)
		}
	}
	sql := sqlOf(t, mustPanel(t, rendered(t, "influxdb"), "Contribution mix (last year)"))
	for _, part := range mixParts {
		if !strings.Contains(sql, "100.0 * "+part.From+" / "+mixTotal+` AS "`+part.To+`"`) {
			t.Errorf("%s is not a percentage of the four summed: %s", part.To, sql)
		}
	}
	es := mustPanel(t, rendered(t, "elasticsearch"), "Contribution mix (last year)")
	if unit := defaultsOf(t, es)["unit"]; unit != "percentunit" {
		t.Errorf("Elasticsearch divides the counts itself and shows the fraction as %v", unit)
	}
	raw := asJSON(t, es["transformations"])
	if !strings.Contains(raw, `"reducer":"sum"`) || strings.Count(raw, `"operator":"/"`) != len(mixParts) {
		t.Errorf("the Elasticsearch shares are not each count over the row's sum: %s", raw)
	}
}

// TestDiscussionsAreListedOneByOne: beside the count by category, which read
// "Ideas" and nothing else at a month, the discussions themselves, and the
// comments left in other people's discussions with whether each was
// accepted; both whatever the range, since an account has a handful, and
// every row links.
func TestDiscussionsAreListedOneByOne(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for title, want := range map[string][]string{
		"Latest discussions": {
			`title AS "Title"`, `category AS "Category"`, `comments AS "Comments"`,
			`url AS "Link"`, "INTERVAL '30 years'", "PARTITION BY repo, number ORDER BY time DESC, comments DESC",
			"WHEN answerable = 'false' THEN NULL WHEN has_answer THEN 1 ELSE 0 END",
		},
		// One row per comment, whichever of its rows says accepted: is_answer
		// is a tag, so a comment seen before it was accepted and again after
		// is two rows at one instant, and without the partition it read twice.
		"Answers elsewhere": {
			`title AS "Title"`, `repo AS "Repository"`, `url AS "Link"`, `answers AS "Accepted"`,
			"INTERVAL '30 years'", "own = 'false'", "PARTITION BY comment ORDER BY answers DESC",
		},
	} {
		p := mustPanel(t, panels, title)
		sql := sqlOf(t, p)
		for _, w := range want {
			if !strings.Contains(sql, w) {
				t.Errorf("%s lacks %s: %s", title, w, sql)
			}
		}
		if strings.Contains(sql, "$__timeFilter") {
			t.Errorf("%s takes the dashboard range, and a month hid all but one: %s", title, sql)
		}
		if titles := rowLinkTitles(p); len(titles) != 1 || titles[0] != "Open on GitHub" {
			t.Errorf("%s links %v", title, titles)
		}
	}
	// The third state of Answered: a category that takes no answer.
	mappings := asJSON(t, overrideProperty(mustPanel(t, panels, "Latest discussions"), "Answered", "mappings"))
	if !strings.Contains(mappings, `"match":"null"`) {
		t.Errorf("an unanswerable discussion reads as an empty cell: %s", mappings)
	}
	if p := mustPanel(t, panels, "Discussions"); p["gridPos"].(map[string]any)["w"] != 8 {
		t.Errorf("the count by category still takes the whole row")
	}
}

// TestAchievementsShelfIsListed: gh_achievement as the profile shows it, one
// row per badge with its tier and level, newest per badge, each linking to
// the badge's page.
func TestAchievementsShelfIsListed(t *testing.T) {
	t.Parallel()
	p := mustPanel(t, rendered(t, "influxdb"), "Achievements")
	sql := sqlOf(t, p)
	for _, want := range []string{
		`name AS "Achievement"`, `tier_number AS "Tier"`, `tier_name AS "Level"`, `url AS "Link"`,
		"PARTITION BY achievement ORDER BY time DESC", "FROM gh_achievement",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("Achievements lacks %s: %s", want, sql)
		}
	}
	if titles := rowLinkTitles(p); len(titles) != 1 {
		t.Errorf("Achievements links %v", titles)
	}
	for _, store := range Names() {
		if twin := mustPanel(t, rendered(t, store), "Achievements"); twin["type"] != "table" {
			t.Errorf("%s draws the shelf as a %v, and every store keeps the tier per badge", store, twin["type"])
		}
	}
}

// TestOverviewSitsUnderTheHeader: the header grew from two grid units to
// brandHeight, and the first line of numbers starts where it ends.
func TestOverviewSitsUnderTheHeader(t *testing.T) {
	t.Parallel()
	b := &builder{}
	panels := Sections[0].Build(b)
	if panels[0].H != brandHeight {
		t.Fatalf("the header is %d units tall, brandHeight says %d", panels[0].H, brandHeight)
	}
	below := regexp.MustCompile(`^(Repositories|Traffic in range|Community)$`)
	for _, p := range panels[1:] {
		if below.MatchString(p.Title) && p.Y != brandHeight {
			t.Errorf("%q starts at %d, the header ends at %d", p.Title, p.Y, brandHeight)
		}
	}
}

// TestAchievementProgressIsABarPerBadge: gh_achievement_progress as the
// owner asked for it, one row per tiered badge with its image, its name,
// the tier the count implies, the count against the next threshold, a bar
// from zero to a hundred per cent whatever the other rows read, the page's
// agreement, and a link to the badge's page from the name. A table in every
// store, drawn at the row height a badge image needs.
func TestAchievementProgressIsABarPerBadge(t *testing.T) {
	t.Parallel()
	p := mustPanel(t, rendered(t, "influxdb"), "Achievement progress")
	sql := sqlOf(t, p)
	for _, want := range []string{
		`image AS "Badge"`, `name AS "Achievement"`, `tier_number AS "Tier"`, `count AS "Count"`,
		`next_threshold AS "Next tier at"`, `percent AS "Progress"`, `agrees AS "Page agrees"`,
		`url AS "Link"`, "PARTITION BY achievement ORDER BY time DESC", "FROM gh_achievement_progress",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("Achievement progress lacks %s: %s", want, sql)
		}
	}
	if titles := rowLinkTitles(p); len(titles) != 1 {
		t.Errorf("Achievement progress links %v", titles)
	}
	if cell, _ := overrideProperty(p, "Badge", "custom.cellOptions").(map[string]any); cell["type"] != "image" {
		t.Errorf("the badge column is drawn as %v, want the image", cell)
	}
	if cell, _ := overrideProperty(p, "Progress", "custom.cellOptions").(map[string]any); cell["type"] != "gauge" {
		t.Errorf("the progress column is drawn as %v, want a gauge", cell)
	}
	if overrideProperty(p, "Progress", "min") != 0 || overrideProperty(p, "Progress", "max") != 100 {
		t.Errorf("the progress bar runs %v to %v, want 0 to 100 so a tenth is a tenth of the cell",
			overrideProperty(p, "Progress", "min"), overrideProperty(p, "Progress", "max"))
	}
	if overrideProperty(p, "Page agrees", "mappings") == nil {
		t.Error("a disagreement is a number, not a word in red")
	}
	options, _ := p["options"].(map[string]any)
	if options["cellHeight"] != "lg" {
		t.Errorf("rows are %v, want lg so the badge image is a badge", options["cellHeight"])
	}
	for _, store := range Names() {
		if twin := mustPanel(t, rendered(t, store), "Achievement progress"); twin["type"] != "table" {
			t.Errorf("%s draws the progress as a %v, and every store keeps the count per badge", store, twin["type"])
		}
	}
	// The one table that asks for the larger row: every other keeps the
	// small one, so the option did not leak.
	for _, other := range renderedPanels(t, "influxdb") {
		theirs, _ := other["options"].(map[string]any)
		if other["type"] == "table" && other["title"] != "Achievement progress" && theirs["cellHeight"] != "sm" {
			t.Errorf("%q has cellHeight %v", other["title"], theirs["cellHeight"])
		}
	}
}
