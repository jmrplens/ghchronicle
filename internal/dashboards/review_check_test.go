package dashboards

import (
	"strings"
	"testing"
)

// The second reading of the 2026-09-12 review, over the rendered dashboards
// after the first round of repairs. Each test pins one thing that reading
// found still drawn other than as the panel says.

// TestBarCellsAreOneColor: once each gauge cell scaled by its own column, the
// default threshold showed through and every count over eighty drew red.
// A bar cell carries its own single-step thresholds, so a count is never
// colored as too high; the cache table, which does have a ceiling, keeps
// its own steps.
func TestBarCellsAreOneColor(t *testing.T) {
	t.Parallel()
	n := 0
	for title, p := range rendered(t, "influxdb") {
		if p["type"] != "table" {
			continue
		}
		fc, _ := p["fieldConfig"].(map[string]any)
		overrides, _ := fc["overrides"].([]any)
		for _, raw := range overrides {
			o, _ := raw.(map[string]any)
			if !gaugeCell(o) {
				continue
			}
			n++
			steps := thresholdSteps(o)
			if title == "Cache against the ceiling" {
				if len(steps) < 2 {
					t.Errorf("%s lost the ceiling it colors by", title)
				}
				continue
			}
			if len(steps) != 1 {
				t.Errorf("%s: the bar cell %v has %d threshold steps, so a count colors by size",
					title, matcherName(o), len(steps))
			}
		}
	}
	if n < 30 {
		t.Fatalf("found %d bar cells, the dashboard has more", n)
	}
}

func gaugeCell(o map[string]any) bool {
	props, _ := o["properties"].([]any)
	for _, raw := range props {
		p, _ := raw.(map[string]any)
		if p["id"] != "custom.cellOptions" {
			continue
		}
		v, _ := p["value"].(map[string]any)
		if v["type"] == "gauge" {
			return true
		}
	}
	return false
}

func thresholdSteps(o map[string]any) []any {
	props, _ := o["properties"].([]any)
	for _, raw := range props {
		p, _ := raw.(map[string]any)
		if p["id"] != "thresholds" {
			continue
		}
		v, _ := p["value"].(map[string]any)
		steps, _ := v["steps"].([]any)
		return steps
	}
	return nil
}

// TestContributionTotalsHeaderCarriesNoIndex: Grafana names the value column
// of a transposed numeric frame after its row, "Last year 1", and the
// organize that follows the transpose takes the index off in every store
// that transposes.
func TestContributionTotalsHeaderCarriesNoIndex(t *testing.T) {
	t.Parallel()
	for _, store := range Names() {
		p := mustPanel(t, rendered(t, store), "Contribution totals")
		raw := asJSON(t, p["transformations"])
		if !strings.Contains(raw, `"id":"transpose"`) {
			continue
		}
		after := raw[strings.Index(raw, `"id":"transpose"`):]
		if !strings.Contains(after, `"renameByName":{"Last year 1":"Last year"}`) {
			t.Errorf("%s: the header keeps Grafana's row index: %s", store, after)
		}
	}
}

// TestSnapshotCurvesAreNotSummedInElasticsearch: gh_repo is written every
// hour, and a sum of it over a day's histogram counted every repository
// twenty four times. The curve is one series per repository, its largest
// reading of the day, stacked.
func TestSnapshotCurvesAreNotSummedInElasticsearch(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "elasticsearch")
	for _, title := range []string{"Stars over time", "Forks over time"} {
		p := mustPanel(t, panels, title)
		raw := asJSON(t, p["targets"])
		if strings.Contains(raw, `"type":"sum"`) {
			t.Errorf("%s sums the sweeps of a day: %s", title, raw)
		}
		if !strings.Contains(raw, `"field":"repo.keyword"`) || !strings.Contains(raw, `"type":"max"`) {
			t.Errorf("%s is not the largest reading per repository: %s", title, raw)
		}
		if st, _ := customOf(t, p)["stacking"].(map[string]any); st["mode"] != "normal" {
			t.Errorf("%s does not stack the repositories, so the top is one of them, not the total", title)
		}
		if desc, _ := p["description"].(string); !strings.Contains(desc, "stacked") {
			t.Errorf("%s does not say how the curve is made: %q", title, desc)
		}
	}
}

// TestStoresThatListOpenRowsAsWrittenSaySo: the InfluxDB table reads each
// pull request or issue from its newest row; Graphite and Elasticsearch
// cannot, and a merged one keeps showing there. Their description says so
// rather than letting the same title promise the same list.
func TestStoresThatListOpenRowsAsWrittenSaySo(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"graphite", "elasticsearch"} {
		panels := rendered(t, store)
		for _, title := range []string{"Open the longest", "Open issues the longest"} {
			desc, _ := mustPanel(t, panels, title)["description"].(string)
			if !strings.Contains(desc, "stays listed as open") {
				t.Errorf("%s %s does not say a closed item stays listed: %q", store, title, desc)
			}
		}
	}
}

// TestArchivedListTakesNoRepositoryFilter: the $repo variable is built from
// gh_repo, which only the collected repositories reach, and the archived
// table lists the ones the filter set aside. Filtered by the variable it
// answered nothing on an account with seventeen archived repositories, in
// every store that has the filter.
func TestArchivedListTakesNoRepositoryFilter(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres", "graphite", "elasticsearch"} {
		p := mustPanel(t, rendered(t, store), "Repositories archived")
		if raw := asJSON(t, p["targets"]); strings.Contains(raw, "$repo") || strings.Contains(raw, "${repo") {
			t.Errorf("%s Repositories archived filters by the repository variable, which never "+
				"names a repository the sweep set aside: %s", store, raw)
		}
		if desc, _ := p["description"].(string); !strings.Contains(desc, "no repository filter") {
			t.Errorf("%s Repositories archived does not say it ignores the filter: %q", store, desc)
		}
	}
}

// TestProfileFlagsHaveNoColumnOnlyOneRowFills: the flags table read age_days
// as a Set column, and the collector writes that field on the availability
// status alone, so the column was empty on seven rows of eight and, on live
// data, read 1529 days beside a flag that was off, the age of the status
// message rather than of the flag. The column is gone in every store, the
// panel says why, and it names the eight flags the collector writes.
func TestProfileFlagsHaveNoColumnOnlyOneRowFills(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres", "graphite", "elasticsearch", "prometheus"} {
		p := mustPanel(t, rendered(t, store), "Profile flags")
		raw := asJSON(t, p)
		for _, gone := range []string{`"Set"`, "age_days"} {
			if strings.Contains(raw, gone) {
				t.Errorf("%s Profile flags still reads %s, a column one row of eight fills", store, gone)
			}
		}
		desc, _ := p["description"].(string)
		for _, said := range []string{"eight flags", "Sponsors listing", "shown nowhere here"} {
			if !strings.Contains(desc, said) {
				t.Errorf("%s Profile flags does not say %q: %q", store, said, desc)
			}
		}
	}
}

// TestClosedListsShowEveryRow: a table whose row count is known in advance
// is tall enough to show every row without a scroll inside the table. The
// eighth profile flag, sponsors_listing, was added on a panel seven units
// high, and seven units were measured at 1920 and at 430 to show five rows,
// so the new flag was reached only by a scroll. The fit below is the model
// those measurements gave: a unit is thirty pixels plus an eight pixel
// gutter, the panel chrome takes forty, a row of the default cell height is
// thirty-six and the header is one more. Ten units showed eight rows, nine
// showed seven, eight showed six and seven showed five, all measured.
func TestClosedListsShowEveryRow(t *testing.T) {
	t.Parallel()
	panels := rendered(t, "influxdb")
	for title, rows := range map[string]int{
		"Profile flags": 8, // the closed list the collector writes
		"Pinned items":  6, // GitHub's ceiling on pinned items
		"Achievements":  8, // the profile it was written against
	} {
		g, _ := mustPanel(t, panels, title)["gridPos"].(map[string]any)
		h, _ := g["h"].(int)
		if fit := (h*38-48)/36 - 1; fit < rows {
			t.Errorf("%s is %d units high, which shows %d rows of %d", title, h, fit, rows)
		}
	}
}
