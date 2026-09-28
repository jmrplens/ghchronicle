package grafana

import (
	"testing"
	"time"
)

// TestAutoIntervalsAreGrafanas holds the bucket variables a checker
// substitutes to the values a render gives them: Grafana's calculateInterval,
// the range over the step count rounded by its table and never under the
// floor, written as secondsToHms writes it. The SQL twins bin by the same
// computation through $__dateBin, so these are also their buckets.
func TestAutoIntervalsAreGrafanas(t *testing.T) {
	t.Parallel()
	variable := func(name, floor string) map[string]any {
		return map[string]any{
			"type": "interval", "name": name, "auto": true, "auto_count": float64(100), "auto_min": floor,
		}
	}
	doc := map[string]any{"templating": map[string]any{"list": []any{
		map[string]any{"type": "query", "name": "repo"},
		variable("bucket_1d", "1d"), variable("bucket_1h", "1h"), variable("bucket_5m", "5m"),
		map[string]any{"type": "interval", "name": "picked", "auto": false},
	}}}
	day := 24 * time.Hour
	for _, tc := range []struct {
		span time.Duration
		want map[string]string
	}{
		// Seven hours and twelve minutes a hundredth, rounded down to six.
		{30 * day, map[string]string{"bucket_1d": "1d", "bucket_1h": "6h", "bucket_5m": "6h"}},
		{7 * day, map[string]string{"bucket_1d": "1d", "bucket_1h": "2h", "bucket_5m": "2h"}},
		{90 * day, map[string]string{"bucket_1d": "1d", "bucket_1h": "12h", "bucket_5m": "12h"}},
		// Anything from a day to a week a hundredth is a day.
		{365 * day, map[string]string{"bucket_1d": "1d", "bucket_1h": "1d", "bucket_5m": "1d"}},
		{2 * 365 * day, map[string]string{"bucket_1d": "7d", "bucket_1h": "7d", "bucket_5m": "7d"}},
		// Fourteen minutes and a half, rounded to fifteen.
		{day, map[string]string{"bucket_1d": "1d", "bucket_1h": "1h", "bucket_5m": "15m"}},
		{6 * time.Hour, map[string]string{"bucket_1d": "1d", "bucket_1h": "1h", "bucket_5m": "5m"}},
	} {
		got, err := AutoIntervals(doc, tc.span)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tc.want) {
			t.Errorf("over %v: %v, want %v", tc.span, got, tc.want)
		}
		for name, want := range tc.want {
			if got[name] != want {
				t.Errorf("over %v %s is %q, want %q", tc.span, name, got[name], want)
			}
		}
	}
}

func TestRangeSpanReadsGrafanasRelativeTimes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 28, 15, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		from, to string
		want     time.Duration
	}{
		{"now-30d", "now", 30 * 24 * time.Hour},
		{"now-6h", "now-1h", 5 * time.Hour},
		{"1790000000000", "1790003600000", time.Hour},
	} {
		got, err := RangeSpan(tc.from, tc.to, now)
		if err != nil || got != tc.want {
			t.Errorf("%s to %s is %v (%v), want %v", tc.from, tc.to, got, err, tc.want)
		}
	}
	for _, bad := range [][2]string{{"yesterday", "now"}, {"now", "now-1d"}, {"now-3x", "now"}} {
		if _, err := RangeSpan(bad[0], bad[1], now); err == nil {
			t.Errorf("%s to %s has a length", bad[0], bad[1])
		}
	}
}

// TestAnIntervalVariableIsSubstitutedLikeTheRepository is the other half: a
// Graphite target that bins by a bucket variable reaches the datasource with
// the bucket in it, which /api/ds/query would otherwise hand Graphite as the
// variable's name, and Graphite refuses as an offset with no unit.
func TestAnIntervalVariableIsSubstitutedLikeTheRepository(t *testing.T) {
	t.Parallel()
	v := Vars{Intervals: map[string]string{"bucket_1h": "6h", "bucket_1": "wrong"}}
	got := v.Apply(map[string]any{"target": `summarize(x, '$bucket_1h', "sum")`, "other": "${bucket_1h}"})
	if got["target"] != `summarize(x, '6h', "sum")` || got["other"] != "6h" {
		t.Errorf("the bucket variable came out as %v", got)
	}
}
