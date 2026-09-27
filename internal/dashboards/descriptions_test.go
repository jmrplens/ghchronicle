package dashboards

import (
	"slices"
	"strings"
	"testing"
)

// The documentation audit of 2.6.0 read every panel description against what
// the collector and the queries do, and found sentences that had been true of
// an earlier release or of one store and were read in all five. Each test here
// pins one of those corrections to the rendered dashboards, which is where a
// reader meets the sentence.

// descriptionOf is a rendered panel's description, the shared sentence and the
// store's own joined as the reader sees them.
func descriptionOf(p map[string]any) string {
	desc, _ := p["description"].(string)
	return desc
}

// TestOldestOpenAlertsSaysWhatItsRowsAre: the table said it was the two
// counts at the top of the section "as the rows they are made of". Since
// 2.6.0 the counts come from a read of the open alerts alone, while a sweep
// writes the rows of the newest hundred alerts in every state, so an open
// alert behind a hundred newer ones is counted and not listed, and one fixed
// behind them stays listed as open until a backfill reads the whole list.
func TestOldestOpenAlertsSaysWhatItsRowsAre(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, store := range AllStores() {
		p, ok := rendered(t, store.Name)["Oldest open alerts"]
		if !ok || p["type"] != "table" {
			continue // a store that cannot answer it says why in a note
		}
		checked++
		desc := descriptionOf(p)
		if strings.Contains(desc, "as the rows they are made of") ||
			!strings.Contains(desc, "newest hundred") || !strings.Contains(desc, "until a backfill reads the whole list") {
			t.Errorf("%s: Oldest open alerts does not say its rows are the newest hundred alerts "+
				"a sweep reads rather than what the counts are made of: %q", store.Name, desc)
		}
	}
	if checked == 0 {
		t.Fatal("no store answers Oldest open alerts, so this checks nothing")
	}
}

// TestTheOverviewSaysPrometheusTakesNoRange: the Repositories group said a
// range shorter than the totals cadence can leave the archived repositories
// out, which is true of the four stores that read the dashboard range and not
// of Prometheus, whose sums are instant queries.
func TestTheOverviewSaysPrometheusTakesNoRange(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		desc := descriptionOf(panelOf(t, store.Build(nil), "Repositories", "stat"))
		if strings.Contains(desc, "by default, so a range shorter than that can leave them out") ||
			!strings.Contains(desc, "where the sums are taken over the dashboard range") {
			t.Errorf("%s: the Overview says any range can leave the archived repositories out: %q",
				store.Name, desc)
		}
		if store.Name == "prometheus" && !strings.Contains(desc, "Prometheus takes no range here") {
			t.Errorf("prometheus: the Overview does not say its sums are instant: %q", desc)
		}
	}
}

// TestWorkElsewhereListsEachItemOnce: Elasticsearch listed the newest forty
// documents of gh_external_contribution, and an open item is a document for
// every day it was seen open, so one pull request filled a row per day and
// pushed the older items off the table. Each item is a bucket now, read from
// its newest document. The shared description said the event feed forgets
// these items in three days, where the feed keeps its last three hundred
// events of thirty; and Prometheus, which has no Seen or Opened column and
// reads the stars as they stand, inherited sentences about both.
func TestWorkElsewhereListsEachItemOnce(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Work elsewhere", "table")
		desc := descriptionOf(p)
		if strings.Contains(desc, "three days") || !strings.Contains(desc, "last three hundred events") {
			t.Errorf("%s: Work elsewhere says the event feed forgets an item in three days: %q", store.Name, desc)
		}
		switch store.Name {
		case "prometheus":
			if !strings.Contains(desc, "no Seen or Opened column") ||
				!strings.Contains(desc, "at the end of the range") {
				t.Errorf("prometheus: Work elsewhere does not say which of the columns described "+
					"it lacks and what its Stars is: %q", desc)
			}
		case "elasticsearch":
			targets := targetList(p)
			buckets, _ := targets[0].(map[string]any)["bucketAggs"].([]any)
			want := []any{
				"full_name.keyword", "kind.keyword", "number.keyword", "@timestamp",
				"state.keyword", "title.keyword", "url.keyword",
			}
			if fields := bucketFieldsOf(buckets); !slices.Equal(fields, want) {
				t.Fatalf("elasticsearch: Work elsewhere buckets by %v, want %v: each item, then "+
					"its newest document", fields, want)
			}
			settings, _ := buckets[3].(map[string]any)["settings"].(map[string]any)
			if settings["size"] != "1" || settings["orderBy"] != "_key" || settings["order"] != "desc" {
				t.Errorf("elasticsearch: the timestamp bucket of Work elsewhere keeps %v, want "+
					"the one newest", settings)
			}
		}
	}
}

// TestEveryArtifactFloorNamesBothCauses: the three panels that say the
// artifact size is a floor blamed the five page cap alone. Since 2.6.0 a walk
// cut short by a failed page writes the pages it read, with a Walked short of
// Declared, and that row is a floor for the other reason.
func TestEveryArtifactFloorNamesBothCauses(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "prometheus", "postgres", "graphite", "elasticsearch"} {
		named := 0
		for title, p := range rendered(t, store) {
			desc := descriptionOf(p)
			if !strings.Contains(desc, "floor") || !strings.Contains(strings.ToLower(title+desc), "artifact") {
				continue
			}
			if !strings.Contains(desc, "five hundred") {
				continue // a panel that says floor about something else
			}
			named++
			if !strings.Contains(desc, "fail") {
				t.Errorf("%s: %q calls the artifact size a floor for the page cap alone: %q", store, title, desc)
			}
		}
		if named == 0 {
			t.Errorf("%s: no panel calls the artifact size a floor, so this checks nothing", store)
		}
	}
}

// TestAchievementProgressSaysHowPairExtraordinaireIsCounted: the description
// said the co-authored count was walked every hour. Since 2.6.0 it is a tally
// the state file keeps, added to by each hourly pass and walked whole once a
// week, which is when a count that went down comes down.
func TestAchievementProgressSaysHowPairExtraordinaireIsCounted(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		desc := descriptionOf(panelOf(t, store.Build(nil), "Achievement progress", "table"))
		if strings.Contains(desc, "walked every hour") || !strings.Contains(desc, "state file") ||
			!strings.Contains(desc, "once a week") {
			t.Errorf("%s: Achievement progress does not say the co-authored count is a tally "+
				"walked whole once a week: %q", store.Name, desc)
		}
	}
}

// TestEveryBucketReadsTheExtremesOfTheRange: the SQL stores put the most any
// reading in the range had used and the least any had left under Most used
// and Lowest remaining, while Graphite and Elasticsearch put the newest
// reading there and Prometheus the value as it stands, so a bucket spent to
// its last request read as untouched in three stores once it had refilled.
func TestEveryBucketReadsTheExtremesOfTheRange(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := panelOf(t, store.Build(nil), "Every bucket", "table")
		targets := targetList(p)
		switch store.Name {
		case "influxdb", "postgres":
			sql := allSQL(p)
			if !strings.Contains(sql, `MAX(used) AS "Most used"`) ||
				!strings.Contains(sql, `MIN(remaining) AS "Lowest remaining"`) {
				t.Errorf("%s: Every bucket does not take the extremes of the range: %s", store.Name, sql)
			}
		case "graphite":
			expr, _ := targets[0].(map[string]any)["target"].(string)
			if got := graphiteReducers(p); !slices.Equal(got, []any{"min"}) ||
				!strings.Contains(expr, `consolidateBy(`+gp("gh_rate_limit", "remaining")+`, "min")`) {
				t.Errorf("graphite: Every bucket reduces the remaining count by %v over %s, want "+
					"the least of the range", got, expr)
			}
		case "elasticsearch":
			metrics, _ := targets[0].(map[string]any)["metrics"].([]any)
			var got []string
			for _, raw := range metrics {
				m, _ := raw.(map[string]any)
				field, _ := m["field"].(string)
				kind, _ := m["type"].(string)
				got = append(got, kind+" "+field)
			}
			if want := []string{"max limit", "min remaining", "max used"}; !slices.Equal(got, want) {
				t.Errorf("elasticsearch: Every bucket reads %v, want %v", got, want)
			}
		case "prometheus":
			exprs := asJSON(t, targets)
			for _, want := range []string{
				"min_over_time(github_rate_limit_remaining[$__range])",
				"max_over_time(github_rate_limit_used[$__range])",
			} {
				if !strings.Contains(exprs, want) {
					t.Errorf("prometheus: Every bucket does not ask %s: %s", want, exprs)
				}
			}
		}
	}
}
