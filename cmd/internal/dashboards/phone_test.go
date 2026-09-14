package dashboards

import (
	"strings"
	"testing"
)

// What the phone review of 2026-09-14 measured, turned into checks on the
// specification. The dashboard is one column at 430 pixels, a table has 380
// of them to draw in and a legend eats the plot it belongs to.

// TestNoRepositoryColumnIsWiderThanItsName: fifteen tables put a 150 pixel
// Repository column in the middle of the row and pushed the column that
// names the row off the screen. At repoWidth the name is cut exactly where
// it was cut before and the difference goes to its neighbor.
func TestNoRepositoryColumnIsWiderThanItsName(t *testing.T) {
	t.Parallel()
	found := 0
	for _, p := range renderedPanels(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		w, ok := overrideProperty(p, "Repository", "custom.width").(int)
		if !ok {
			continue
		}
		found++
		if w > repoWidth {
			t.Errorf("%q draws Repository %d pixels wide, and a phone table is 380 "+
				"(repoColumn is %d)", p["title"], w, repoWidth)
		}
	}
	if found == 0 {
		t.Fatal("no table sets a Repository width, so this checked nothing")
	}
}

// TestTheCategoricalPanelsFoldTheirTail: a stacked chart, a donut or a
// legend with a series per value of an unbounded tag is unreadable long
// before it is wrong. Seven panels already fold to topSeriesKept and a
// remainder; these four did not, and the numbers are what each returned over
// two years once the backfill had filled the picker.
func TestTheCategoricalPanelsFoldTheirTail(t *testing.T) {
	t.Parallel()
	returned := map[string]int{
		"Code scanning runs":     14,
		"Stars gained over time": 17,
		"Forks gained over time": 7,
		"Events by type":         11,
	}
	seen := map[string]bool{}
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			n, ok := returned[p.Title]
			if !ok {
				continue
			}
			seen[p.Title] = true
			sql := p.Stores["influxdb"].Q[0].SQL
			if !strings.Contains(sql, "'other'") {
				t.Errorf("%q returned %d series and still names every one of them: %s",
					p.Title, n, sql)
			}
		}
	}
	for title := range returned {
		if !seen[title] {
			t.Errorf("no panel is called %q, so this checked nothing for it", title)
		}
	}
}

// TestLegendsHaveRoomForNineEntries: eight series and a remainder is nine
// legend entries. At h=7 a panel is 258 pixels, of which the legend takes 73
// and the plot 143, and nine entries did not fit: eleven legends hid
// something at 430 pixels and fifteen at 360. Their twins were already h=8,
// which is 38 pixels more and enough.
func TestLegendsHaveRoomForNineEntries(t *testing.T) {
	t.Parallel()
	checked := 0
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			if p.Kind != "timeseries" {
				continue
			}
			st := p.Stores["influxdb"]
			if st.Q == nil || !strings.Contains(st.Q[0].SQL, "'other'") {
				continue
			}
			checked++
			if p.H < 8 {
				t.Errorf("%s: %q folds to nine series into %d units, and nine legend "+
					"entries need 8", sec.Title, p.Title, p.H)
			}
		}
	}
	if checked < 8 {
		t.Fatalf("checked %d folded charts, and the specification has more", checked)
	}
}
