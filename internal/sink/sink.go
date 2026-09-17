// Package sink writes measurements out.
//
// Two shapes, because the data has two natures. A Point carries its own
// timestamp and describes something that happened on a given day; it is what
// lets the traffic window be rewritten in full on every sweep, so a collector
// that was down for a day repairs itself on the next run. A Gauge describes
// what is true right now and is served over /metrics, where the scrape decides
// the timestamp.
//
// Prometheus cannot accept the first kind: measured against Prometheus 3.14
// with the OTLP receiver enabled and out_of_order_time_window at 30m, a point
// dated two days back is rejected with HTTP 400. That is the reason this
// package exists rather than everything being an exporter.
package sink

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Point is one dated observation.
type Point struct {
	Measurement string
	Tags        map[string]string
	Fields      map[string]any
	Time        time.Time
}

// Sink accepts dated points. Implementations must be safe for concurrent use.
type Sink interface {
	// Write sends one family's points. A *RejectedError or *DroppedError is
	// a partial success: every point the store would accept was written, and
	// the error counts the ones it would not; any other error is a failed
	// write.
	Write(ctx context.Context, points []Point) error
	// Name identifies the sink in log lines, so an operator can tell which
	// of several configured stores a warning is about.
	Name() string
	// Close flushes anything buffered and releases the connection, file or
	// listener behind the sink. It is called once, at shutdown.
	Close() error
}

// Filtering is a sink that writes only part of what it is offered and counts
// the rest. The count is cumulative over the sink's life, so a caller reads it
// before a write and again after, and the difference is what that write left
// out.
//
// It exists because the caller cannot see it otherwise. A sink filters inside
// its own Write, past the point where anything else can look, so a log line
// counting what was handed over reports points the store never received:
// production logged 440 points of gh_job_log delivered to InfluxDB, which
// excludes that measurement by default and has never held a row of it. A sink
// that writes everything it is given implements nothing and is counted whole.
type Filtering interface {
	// Filtered counts the points this sink has dropped rather than written,
	// since it was created.
	Filtered() uint64
}

// LineProtocol renders a point as InfluxDB line protocol.
//
// Tag values are escaped, never rewritten: a space in "Cloudflare, Inc." has to
// survive as a space or the series stops matching the ones written by anything
// else. Fields keep their Go type so integers do not silently become floats.
func LineProtocol(p Point) string {
	var b strings.Builder
	b.WriteString(escapeMeasurement(p.Measurement))

	keys := make([]string, 0, len(p.Tags))
	for k := range p.Tags {
		if p.Tags[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // sorted tags let InfluxDB skip a sort on ingest
	for _, k := range keys {
		b.WriteString("," + escapeTag(k) + "=" + escapeTag(p.Tags[k]))
	}

	b.WriteString(" ")
	fkeys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		fkeys = append(fkeys, k)
	}
	sort.Strings(fkeys)
	first := true
	for _, k := range fkeys {
		// A name cannot be both a tag and a field: InfluxDB rejects such a
		// line, and rejects the whole batch with it. The tag wins because it
		// is the one that can be grouped by, and a collector that produces
		// both meant the tag.
		if _, clash := p.Tags[k]; clash && p.Tags[k] != "" {
			continue
		}
		v, ok := formatField(p.Fields[k])
		if !ok {
			continue
		}
		if !first {
			b.WriteString(",")
		}
		b.WriteString(escapeTag(k) + "=" + v)
		first = false
	}
	if first {
		return "" // a point with no usable field is not a point
	}
	b.WriteString(" " + strconv.FormatInt(p.Time.UnixNano(), 10))
	return b.String()
}

func formatField(v any) (string, bool) {
	switch t := v.(type) {
	case int:
		return fmt.Sprintf("%di", t), true
	case int64:
		return fmt.Sprintf("%di", t), true
	case float64:
		return fmt.Sprintf("%g", t), true
	case bool:
		return strconv.FormatBool(t), true
	case string:
		if t == "" {
			return "", false
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(oneLine(t)) + `"`, true
	case time.Time:
		if t.IsZero() {
			return "", false
		}
		return fmt.Sprintf("%di", t.Unix()), true
	case nil:
		return "", false
	}
	return "", false
}

var (
	tagEscaper         = strings.NewReplacer(`\`, `\\`, ` `, `\ `, `,`, `\,`, `=`, `\=`)
	measurementEscaper = strings.NewReplacer(`\`, `\\`, ` `, `\ `, `,`, `\,`)
)

func escapeTag(s string) string         { return tagEscaper.Replace(oneLine(s)) }
func escapeMeasurement(s string) string { return measurementEscaper.Replace(oneLine(s)) }

// oneLine folds control characters into spaces.
//
// Line protocol is line oriented and has no escape for a newline inside a tag,
// so a value containing one splits the record in two and the second half is
// rejected as malformed, taking the whole batch with it. GitHub produces such
// values without asking: a step with no `name:` is named after its `run:`
// block, and a multi-line block becomes a multi-line name.
//
// Folding rather than dropping, because the words still identify the thing.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\n\r\t\v\f") {
		return s // the overwhelmingly common case, no allocation
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f' {
			return ' '
		}
		return r
	}, s)
}
