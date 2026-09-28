package grafana

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A dashboard's automatic interval variables, as a render computes them.
//
// A variable of the interval type set to auto is a length of time Grafana works
// out of the range when the page is drawn: the range over the variable's step
// count, rounded by the table Grafana rounds every interval by, and never under
// the variable's minimum. /api/ds/query is handed a target with the variable in
// it and substitutes nothing, so a checker has to, the way it substitutes the
// repository variable. The Graphite dashboard bins every chart over time by
// one (see grBin in internal/dashboards).

// AutoIntervals is what every automatic interval variable of a dashboard
// becomes over a range of that length, by name: rangeutil.calculateInterval
// of Grafana 13.2.1, read out of its frontend.
func AutoIntervals(dashboard map[string]any, span time.Duration) (map[string]string, error) {
	templating, _ := dashboard["templating"].(map[string]any)
	out := map[string]string{}
	for _, raw := range list(templating["list"]) {
		v, _ := raw.(map[string]any)
		if v["type"] != "interval" {
			continue
		}
		if auto, _ := v["auto"].(bool); !auto {
			continue
		}
		name, _ := v["name"].(string)
		steps, err := stepCount(v["auto_count"])
		if err != nil {
			return nil, fmt.Errorf("the interval variable %s: %w", name, err)
		}
		floor := time.Millisecond
		if minimum, _ := v["auto_min"].(string); minimum != "" {
			if floor, err = intervalLength(minimum); err != nil {
				return nil, fmt.Errorf("the interval variable %s: %w", name, err)
			}
		}
		out[name] = hms(max(floor, roundInterval(span/time.Duration(steps))))
	}
	return out, nil
}

func stepCount(v any) (int, error) {
	switch n := v.(type) {
	case int:
		if n > 0 {
			return n, nil
		}
	case float64:
		if n >= 1 {
			return int(n), nil
		}
	}
	return 0, fmt.Errorf("a step count of %v", v)
}

// roundedIntervals is Grafana's roundInterval: an interval below each bound in
// milliseconds is rounded to the length beside it, and one past the last to a
// year.
var roundedIntervals = [][2]int64{
	{10, 1},
	{15, 10},
	{35, 20},
	{75, 50},
	{150, 100},
	{350, 200},
	{750, 500},
	{1500, 1000},
	{3500, 2000},
	{7500, 5000},
	{12500, 10000},
	{17500, 15000},
	{25000, 20000},
	{45000, 30000},
	{90000, 60000},
	{210000, 120000},
	{450000, 300000},
	{750000, 600000},
	{1050000, 900000},
	{1500000, 1200000},
	{2700000, 1800000},
	{5400000, 3600000},
	{9000000, 7200000},
	{16200000, 10800000},
	{32400000, 21600000},
	{86400000, 43200000},
	{604800000, 86400000},
	{1814400000, 604800000},
	{3628800000, 2592000000},
}

func roundInterval(d time.Duration) time.Duration {
	ms := float64(d) / float64(time.Millisecond)
	for _, r := range roundedIntervals {
		if ms < float64(r[0]) {
			return time.Duration(r[1]) * time.Millisecond
		}
	}
	return 365 * 24 * time.Hour
}

// hms is Grafana's secondsToHms: the largest whole unit an interval holds,
// and nothing below it.
func hms(d time.Duration) string {
	rest := int64(d / time.Second)
	for _, u := range []struct {
		name    string
		seconds int64
	}{{"y", 31536000}, {"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}} {
		if n := rest / u.seconds; n > 0 {
			return strconv.FormatInt(n, 10) + u.name
		}
		rest %= u.seconds
	}
	if ms := d.Milliseconds(); ms > 0 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	return "less than a millisecond"
}

// intervalUnits is how long each unit of Grafana's interval strings is, a month
// and a year at the lengths Grafana's own describeInterval gives them.
var intervalUnits = map[string]time.Duration{
	"y": 365 * 24 * time.Hour, "M": 30 * 24 * time.Hour, "w": 7 * 24 * time.Hour,
	"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second, "ms": time.Millisecond,
}

var intervalString = regexp.MustCompile(`^(\d+)(ms|[yMwdhms])$`)

func intervalLength(s string) (time.Duration, error) {
	m := intervalString.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("an interval of %q", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("an interval of %q: %w", s, err)
	}
	return time.Duration(n) * intervalUnits[m[2]], nil
}

// RangeSpan is how long a range from and to Grafana's relative times is,
// "now-30d" to "now" being thirty days, which is what an interval variable
// is computed from. An epoch in milliseconds is read as one too.
func RangeSpan(from, to string, now time.Time) (time.Duration, error) {
	start, err := relativeInstant(from, now)
	if err != nil {
		return 0, err
	}
	end, err := relativeInstant(to, now)
	if err != nil {
		return 0, err
	}
	if !end.After(start) {
		return 0, fmt.Errorf("a range from %s to %s holds no time", from, to)
	}
	return end.Sub(start), nil
}

func relativeInstant(s string, now time.Time) (time.Time, error) {
	if s == "now" {
		return now, nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	if back, ok := strings.CutPrefix(s, "now-"); ok {
		d, err := intervalLength(back)
		if err != nil {
			return time.Time{}, fmt.Errorf("the time %q: %w", s, err)
		}
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("the time %q is neither now, now less an interval, nor an epoch", s)
}
