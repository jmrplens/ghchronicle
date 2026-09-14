//go:build dockere2e

package docker

import (
	"context"
	"errors"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a real Telegraf does with the line protocol the sink posts.
//
// This sink exists so that Telegraf's own outputs can carry the points
// onwards, which only works if Telegraf parses what the sink writes, keeps the
// timestamps it is given and keeps the field types. The capture suite in
// test/e2e proves the bytes leaving this process; nothing until now had put a
// real Telegraf on the other end of them.
//
// The stack's Telegraf writes what it accepted straight back out to a file in
// the same format, which makes this a round trip: the sink renders a point,
// Telegraf parses it, Telegraf renders it again, and the assertions compare
// that against the file sink's JSON for the same sweep.

// telegrafPoints is the sweep read back out of Telegraf's file output, keyed
// the way every store here keys a point.
//
// Two properties of that file decide the shape of this. Telegraf answers the
// POST as soon as it has parsed the body and writes on its own flush interval,
// a second here, so the file is polled rather than read once. And it is
// appended to rather than replaced, so a stack kept between runs holds earlier
// sweeps' lines too: the wait is for this sweep's own keys to appear, never
// for a line count, and the map keeps the last line of each key, which is the
// most recent write of it.
func telegrafPoints(ctx context.Context, t *testing.T, s *Stack, sweep *pushSweep) (map[string]influxPoint, []influxPoint) {
	t.Helper()
	want := telegrafWantKeys(t, sweep)
	var all []influxPoint
	byKey := map[string]influxPoint{}
	err := WaitUntil(ctx, "telegraf's file output", 2*time.Minute, func(context.Context) error {
		raw, err := os.ReadFile(s.TelegrafOutput)
		if err != nil {
			return err
		}
		all = all[:0]
		clear(byKey)
		for line := range strings.SplitSeq(string(raw), "\n") {
			if p, ok := influxParse(line); ok {
				all = append(all, p)
				byKey[p.key()] = p
			}
		}
		missing := 0
		for key := range want {
			if _, ok := byKey[key]; !ok {
				missing++
			}
		}
		if missing > 0 {
			return errors.New(strconv.Itoa(missing) + " of the sweep's points have not been flushed yet")
		}
		return nil
	})
	if err != nil {
		// Reported rather than fatal, because which points are missing is the
		// finding and the subtests below name them.
		t.Errorf("telegraf did not write the whole sweep back out: %v", err)
	}
	return byKey, all
}

// telegrafWantKeys is every point of the sweep the line protocol has anything
// to say about. A point whose fields are all empty is not a line, which is the
// same rule every other sink applies.
func telegrafWantKeys(t *testing.T, sweep *pushSweep) map[string]sqlStoresPoint {
	t.Helper()
	want := map[string]sqlStoresPoint{}
	for _, p := range pushPoints(t, sweep) {
		if esHasUsableField(p) {
			want[influxKeyOf(t, p)] = p
		}
	}
	return want
}

func TestTelegrafKeepsEveryPointTheSinkPosted(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	have, _ := telegrafPoints(ctx, t, s, sweep)
	want := telegrafWantKeys(t, sweep)

	t.Run("every point arrived", func(t *testing.T) {
		missing := 0
		for key := range want {
			if _, ok := have[key]; !ok {
				missing++
				if missing <= 5 {
					t.Errorf("telegraf never wrote %s back out", key)
				}
			}
		}
		if missing > 5 {
			t.Errorf("and %d more of the sweep's %d points", missing-5, len(want))
		}
	})

	t.Run("with the fields it was given", func(t *testing.T) {
		for key, p := range want {
			arrived, ok := have[key]
			if !ok {
				continue // reported above
			}
			telegrafWantFields(t, key, p, arrived)
		}
	})
}

func TestTelegrafKeepsTheDateAndTheTypeOfEachField(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	byKey, points := telegrafPoints(ctx, t, s, sweep)

	t.Run("a dated event keeps the nanosecond it happened", func(t *testing.T) {
		p := telegrafOne(t, byKey, "gh_star", starGivenAt, map[string]string{"user": "alice"})
		if p.at.Format(time.RFC3339) != starGivenAt {
			t.Errorf("the line is stamped %s, want %s", p.at.Format(time.RFC3339Nano), starGivenAt)
		}
		telegrafWantInt(t, p, "starred", 1)
		if url, _ := p.fields["url"].(string); url != "https://github.com/octocat/hello-world/stargazers" {
			t.Errorf("url = %#v, want the repository's stargazer list", p.fields["url"])
		}
		if url, _ := p.fields["user_url"].(string); url != "https://github.com/alice" {
			t.Errorf("user_url = %#v, want the stargazer's own page", p.fields["user_url"])
		}
	})

	t.Run("a daily snapshot keeps its own day and its integers", func(t *testing.T) {
		p := telegrafOne(t, byKey, "gh_traffic", trafficDayAt, map[string]string{"kind": "views"})
		// The integer suffix is the point of this one. A count that arrived
		// as 120 rather than 120i would be a float in every store Telegraf
		// forwards to, and the difference is invisible until someone sums a
		// column and gets 119.99999999999999.
		telegrafWantInt(t, p, "count", 120)
		telegrafWantInt(t, p, "uniques", 18)
	})

	t.Run("a run keeps its boolean", func(t *testing.T) {
		p := telegrafOne(t, byKey, "gh_workflow_run", runFinishedAt, map[string]string{"conclusion": "success"})
		if ok, is := p.fields["success"].(bool); !is || !ok {
			t.Errorf("success = %#v, want the boolean true", p.fields["success"])
		}
		telegrafWantInt(t, p, "duration_seconds", 220)
	})

	t.Run("a current-state gauge is stamped when the sweep looked", func(t *testing.T) {
		at := telegrafNewest(t, points, "gh_repo")
		if at.Before(sweep.Started) || at.After(sweep.Finished) {
			t.Errorf("the newest gh_repo line is stamped %s, outside the sweep (%s to %s)",
				at.Format(time.RFC3339Nano), sweep.Started.Format(time.RFC3339),
				sweep.Finished.Format(time.RFC3339))
		}
	})
}

// telegrafOne finds the line for one point, and says what it found instead
// when there is none.
func telegrafOne(t *testing.T, byKey map[string]influxPoint, measurement, stamp string, tags map[string]string) influxPoint {
	t.Helper()
	var seen []string
	for key, p := range byKey {
		if p.measurement != measurement || !hasTags(p.tags, tags) {
			continue
		}
		if p.at.Format(time.RFC3339) == stamp {
			return byKey[key]
		}
		seen = append(seen, p.at.Format(time.RFC3339))
	}
	sort.Strings(seen)
	t.Fatalf("telegraf holds no %s at %s for %v; it holds %v", measurement, stamp, tags, seen)
	return influxPoint{}
}

// telegrafNewest is the most recent moment one measurement was written at,
// which is how a gauge is read back: the file holds every earlier sweep's
// lines too, each stamped inside its own sweep.
func telegrafNewest(t *testing.T, points []influxPoint, measurement string) time.Time {
	t.Helper()
	var newest time.Time
	for _, p := range points {
		if p.measurement == measurement && p.at.After(newest) {
			newest = p.at
		}
	}
	if newest.IsZero() {
		t.Fatalf("telegraf holds no %s line at all", measurement)
	}
	return newest
}

func telegrafWantInt(t *testing.T, p influxPoint, field string, want int64) {
	t.Helper()
	got, ok := p.fields[field].(int64)
	if !ok {
		t.Errorf("%s.%s = %#v, want the integer %d", p.measurement, field, p.fields[field], want)
		return
	}
	if got != want {
		t.Errorf("%s.%s = %d, want %d", p.measurement, field, got, want)
	}
}

// telegrafWantFields compares one point's fields against the oracle's.
//
// The oracle is JSON, which has one number type and no time type, so the
// comparison is by value rather than by Go type: an integer field reads as a
// float there, and a time field reads as the string the sink would have
// written as Unix seconds.
func telegrafWantFields(t *testing.T, key string, want sqlStoresPoint, got influxPoint) {
	t.Helper()
	for name, raw := range want.Fields {
		if _, clash := want.Tags[name]; clash {
			continue // the tag wins, as in the line protocol
		}
		arrived, ok := got.fields[name]
		if !ok {
			if influxWritten(raw) {
				t.Errorf("%s: telegraf has no field %s", key, name)
			}
			continue
		}
		if !influxSameValue(raw, arrived) {
			t.Errorf("%s: %s = %#v, want %#v", key, name, arrived, raw)
		}
	}
}

// influxWritten reports whether the line protocol writes a field at all: an
// empty string and a nil are not fields.
func influxWritten(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	default:
		return true
	}
}

// influxSameValue compares an oracle value with what Telegraf wrote back.
func influxSameValue(want, got any) bool {
	switch w := want.(type) {
	case float64:
		return influxNumber(got) == w
	case bool:
		return got == w
	case string:
		// A time field is written as Unix seconds by the line protocol and as
		// RFC 3339 by the file sink's JSON, so the same value reads as a
		// number here and a string there.
		if at, err := time.Parse(time.RFC3339Nano, w); err == nil {
			if n, ok := got.(int64); ok {
				return n == at.Unix()
			}
		}
		text, ok := got.(string)
		return ok && text == influxOneLine(w)
	}
	return false
}

func influxNumber(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return math.NaN()
}

// influxOneLine is the sink's own normalization: the line protocol is a line,
// so a job log entry's newlines and tabs become spaces before it is quoted.
func influxOneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f' {
			return ' '
		}
		return r
	}, s)
}

// ── The line protocol, read back ────────────────────────────────────────────

// influxPoint is one parsed line: what Telegraf accepted, as Telegraf wrote it
// out again.
type influxPoint struct {
	measurement string
	tags        map[string]string
	fields      map[string]any
	at          time.Time
}

// key identifies a point the way every store here does: the measurement, the
// tags that are set, and the moment.
func (p influxPoint) key() string {
	return p.measurement + "|" + esTagKey(p.tags) + "|" + p.at.UTC().Format(time.RFC3339Nano)
}

// influxKeyOf is the same key, computed from the oracle.
func influxKeyOf(t *testing.T, p sqlStoresPoint) string {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, p.Time)
	if err != nil {
		t.Fatalf("the point's date %q: %v", p.Time, err)
	}
	return p.Measurement + "|" + esTagKey(p.Tags) + "|" + at.UTC().Format(time.RFC3339Nano)
}

// influxParse reads one line of line protocol.
//
// It is written out rather than taken from a library because the point is to
// read what arrived without the sink's own encoder in the loop: a parser that
// shared code with the writer would agree with it about a mistake.
func influxParse(line string) (influxPoint, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return influxPoint{}, false
	}
	key, fields, stamp, ok := influxSplit(line)
	if !ok {
		return influxPoint{}, false
	}
	nanos, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return influxPoint{}, false
	}
	parts := influxSplitEscaped(key, ',')
	p := influxPoint{
		measurement: influxUnescape(parts[0]),
		tags:        influxTags(parts[1:]),
		fields:      influxFields(fields),
		at:          time.Unix(0, nanos).UTC(),
	}
	return p, len(p.fields) > 0
}

// influxTags reads the key=value pairs that follow the measurement. A pair
// with no equals sign is left out rather than failing the line.
func influxTags(pairs []string) map[string]string {
	tags := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		if k, v, ok := influxPair(pair); ok {
			tags[k] = influxUnescape(v)
		}
	}
	return tags
}

// influxFields reads the field set, each value in the type its suffix or
// quoting gives it.
func influxFields(set string) map[string]any {
	fields := map[string]any{}
	for _, pair := range influxSplitEscaped(set, ',') {
		if k, v, ok := influxPair(pair); ok {
			fields[k] = influxValue(v)
		}
	}
	return fields
}

// influxSplit cuts a line into its three parts. The separators are the first
// and last spaces that are neither escaped nor inside a quoted field value,
// which is what a string field containing a space makes necessary.
func influxSplit(line string) (key, fields, stamp string, ok bool) {
	var spaces []int
	quoted, escaped := false, false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			spaces = append(spaces, i)
		}
	}
	if len(spaces) < 2 {
		return "", "", "", false
	}
	first, last := spaces[0], spaces[len(spaces)-1]
	return line[:first], line[first+1 : last], line[last+1:], true
}

// influxSplitEscaped splits on a separator that a backslash or a quoted string
// protects.
func influxSplitEscaped(s string, sep rune) []string {
	var out []string
	var current strings.Builder
	quoted, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			current.WriteRune(r)
			escaped = true
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == sep && !quoted:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	return append(out, current.String())
}

// influxPair splits one key=value on the first unescaped equals sign.
func influxPair(s string) (key, value string, ok bool) {
	escaped := false
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '=':
			return influxUnescape(s[:i]), s[i+1:], true
		}
	}
	return "", "", false
}

// influxValue reads a field value back into the type the line protocol gave
// it: an integer, a float, a boolean or a string.
func influxValue(raw string) any {
	switch {
	case strings.HasPrefix(raw, `"`):
		return influxUnquote(raw)
	case raw == "true" || raw == "t" || raw == "T":
		return true
	case raw == "false" || raw == "f" || raw == "F":
		return false
	case strings.HasSuffix(raw, "i"):
		if n, err := strconv.ParseInt(strings.TrimSuffix(raw, "i"), 10, 64); err == nil {
			return n
		}
	case strings.HasSuffix(raw, "u"):
		// The unsigned form, which nothing here writes and a Telegraf
		// processor could. Read as a signed integer: these are counts of
		// things GitHub reported, not numbers near the top of the range.
		if n, err := strconv.ParseInt(strings.TrimSuffix(raw, "u"), 10, 64); err == nil {
			return n
		}
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return raw
}

func influxUnquote(raw string) string {
	raw = strings.TrimPrefix(raw, `"`)
	raw = strings.TrimSuffix(raw, `"`)
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(raw)
}

func influxUnescape(s string) string {
	return strings.NewReplacer(`\,`, `,`, `\ `, ` `, `\=`, `=`, `\\`, `\`).Replace(s)
}
