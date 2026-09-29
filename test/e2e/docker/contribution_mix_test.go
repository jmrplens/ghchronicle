//go:build dockere2e

package docker

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// contributionMixTitle is the panel TestTheContributionMixReadsTheNewestSnapshot
// asks.
const contributionMixTitle = "Contribution mix (last year)"

// TestTheContributionMixReadsTheNewestSnapshot writes the account's
// contributions snapshot into a namespace of every store that keeps rows,
// through the sinks, and asks "Contribution mix (last year)" about it through
// each dashboard's own query.
//
// The bars read the newest snapshot of the range, as the Contributions tile
// of Account beside them does, so a snapshot with none of the four kinds, the
// year of an account that did nothing for one, is no mix to draw even where
// an older snapshot in the range had one. The review of the 2.6.4 details
// wrote such a snapshot after the suite's own and found InfluxDB and Graphite
// drawing the older snapshot's mix beside a Contributions tile of 0, where
// Elasticsearch drew "not read": the SQL stores filtered out a row whose sum
// was 0, and Graphite's bars skipped the null its share of a sum of 0 is, for
// the last share that was not. Prometheus is not asked: it keeps no reading
// for the panel to fall back to, and its instant query divides the newest
// value of each gauge.
//
// The namespace with the older snapshot alone is the control: it has to draw
// that snapshot's mix, or the other's "not read" could be a query that reads
// nothing at all.
func TestTheContributionMixReadsTheNewestSnapshot(t *testing.T) {
	ctx := t.Context()
	s := Start(t)
	dir, err := sqlStoresWorkDir("contribution-mix")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	some := contributionsSnapshot(now.AddDate(0, 0, -10), 40, 5, 3, 2)
	// A day back rather than an hour: the dashboards' Graphite answer
	// averages two hours into a point over thirty days, and a point holding
	// the last share and the null after it reads the share.
	none := contributionsSnapshot(now.AddDate(0, 0, -1), 0, 0, 0, 0)
	for _, tc := range []struct {
		ns     string
		points []sink.Point
		want   map[string]any
	}{
		{"contributionmixsome", []sink.Point{some}, map[string]any{
			"Commits": 80.0, "Pull requests": 10.0, "Issues": 6.0, "Code review": 4.0,
		}},
		{"contributionmixnone", []sink.Point{some, none}, map[string]any{
			"Commits": notReadWords, "Pull requests": notReadWords, "Issues": notReadWords, "Code review": notReadWords,
		}},
	} {
		n := seedPanels(ctx, t, s, tc.ns, tc.points, dir)
		for _, store := range n.stores(false) {
			drawn := WaitUntil(ctx, store+" drawing the mix of "+tc.ns, 2*time.Minute, func(ctx context.Context) error {
				pic := n.drawnPanels(ctx, t, store, []string{contributionMixTitle}, "contributions_total")[contributionMixTitle]
				return mixDrawn(store, pic, tc.want)
			})
			if drawn != nil {
				t.Errorf("%s over %s: %v", store, tc.ns, drawn)
			}
		}
	}
}

// notReadWords is what a bar of the mix says for a share that is not there.
const notReadWords = "not read"

// contributionsSnapshot is one reading of the account's last year as the
// account family writes it, with the four kinds the mix is drawn from.
func contributionsSnapshot(at time.Time, commits, pulls, issues, reviews int) sink.Point {
	return sink.Point{
		Measurement: "gh_contributions_total",
		Tags:        map[string]string{"user": sqlStoresLogin},
		Fields: map[string]any{
			"commits": commits, "pull_requests": pulls, "issues": issues, "reviews": reviews,
			"calendar_total": commits + pulls + issues + reviews,
		},
		Time: at,
	}
}

// mixDrawn says how the bars drawn differ from want, the percentage of each
// or the words for none. Elasticsearch's bars are fractions, which its panel
// draws in percent.
func mixDrawn(store string, pic grafana.Picture, want map[string]any) error {
	got := map[string]any{}
	for _, tile := range pic.Tiles {
		got[tile.Name] = tile.Value
		if text, mapped := tile.Field.Mapped(nil); tile.Value == nil && mapped {
			got[tile.Name] = text
		} else if v, isNumber := tile.Value.(float64); isNumber && store == "elasticsearch" {
			got[tile.Name] = 100 * v
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("it draws %v, want %v", got, want)
	}
	for name, w := range want {
		v, isNumber := got[name].(float64)
		share, wantsShare := w.(float64)
		switch {
		case wantsShare && (!isNumber || math.Abs(v-share) > 1e-9):
			return fmt.Errorf("%s reads %v, want %v; it draws %v", name, got[name], w, got)
		case !wantsShare && got[name] != w:
			return fmt.Errorf("%s reads %v, want %q; it draws %v", name, got[name], w, got)
		}
	}
	return nil
}
