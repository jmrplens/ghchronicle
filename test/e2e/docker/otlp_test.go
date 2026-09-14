//go:build dockere2e

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a real OpenTelemetry collector does with what the OTLP sink posts.
//
// The sink speaks OTLP's JSON encoding, hand-written rather than generated, so
// the first question is simply whether a collector accepts it: a receiver that
// could not parse a request answers 400, and a capture server never would. The
// second is the dating rule again. An OTLP data point carries an explicit
// timestamp, which is why the sink has a raw mode at all, and the sweep uses
// it: the default mode reduces to current values stamped now, and then there
// would be no date left to check.
//
// The pipeline in the stack is deliberately bare, no batching and no
// processors, and its file exporter writes what it received. So the file read
// back here is the collector's own understanding of the sink's bytes.

// otlpDataPoint is one reading, flattened out of the file exporter's document.
type otlpDataPoint struct {
	metric string
	attrs  map[string]string
	at     time.Time
	value  float64
}

// key identifies a reading the way every store in this suite keys a point.
func (d otlpDataPoint) key() string {
	return d.metric + "|" + esTagKey(d.attrs) + "|" + d.at.UTC().Format(time.RFC3339Nano)
}

func TestOTLPCollectorAcceptsWhatTheSinkPosts(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)

	t.Run("the collector refused nothing", func(t *testing.T) {
		// The receiver answers 400 on a request it cannot parse, and the sink
		// reports that as a failed write rather than swallowing it.
		if strings.Contains(sweep.Log, "otlp write") {
			t.Errorf("the sink reported an OTLP failure:\n%s", tail(sweep.Log))
		}
	})

	want := otlpExpected(t, sweep)
	have := otlpAwait(ctx, t, s, want)

	t.Run("every number of the sweep became a data point", func(t *testing.T) {
		missing := 0
		for key := range want {
			if _, ok := have[key]; !ok {
				missing++
				if missing <= 5 {
					t.Errorf("the collector never received %s", key)
				}
			}
		}
		if missing > 5 {
			t.Errorf("and %d more of the sweep's %d readings", missing-5, len(want))
		}
	})

	t.Run("with the value it was given", func(t *testing.T) {
		for key, value := range want {
			got, ok := have[key]
			if !ok {
				continue // reported above
			}
			if got.value != value {
				t.Errorf("%s = %v, want %v", key, got.value, value)
			}
		}
	})
}

func TestOTLPKeepsTheDateOfTheEvent(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	have := otlpAwait(ctx, t, s, otlpExpected(t, sweep))

	t.Run("a dated event keeps the nanosecond it happened", func(t *testing.T) {
		// The metric name is the sink's own convention: dots rather than the
		// underscores Prometheus wants, because a receiver exporting onward
		// to Prometheus converts them itself and going the other way it
		// cannot.
		d := otlpOne(t, have, "github.star.starred", starGivenAt, map[string]string{"user": "alice"})
		if d.value != 1 {
			t.Errorf("the star reads %v, want 1", d.value)
		}
	})

	t.Run("a daily snapshot keeps its own day", func(t *testing.T) {
		d := otlpOne(t, have, "github.traffic.count", trafficDayAt, map[string]string{"kind": "views"})
		if d.value != 120 {
			t.Errorf("the traffic day reads %v, want 120", d.value)
		}
	})

	t.Run("a run keeps the moment it finished", func(t *testing.T) {
		d := otlpOne(t, have, "github.workflow.run.duration.seconds", runFinishedAt,
			map[string]string{"conclusion": "success"})
		if d.value != 220 {
			t.Errorf("the run's duration reads %v, want 220", d.value)
		}
	})

	t.Run("nothing dated was restamped with the sweep's clock", func(t *testing.T) {
		// What raw mode is for. Without it the sink reduces to current values
		// and stamps them now, and every one of these would land inside the
		// sweep window instead of before it.
		for _, metric := range []string{"github.star.starred", "github.traffic.count"} {
			for _, d := range have {
				if d.metric != metric {
					continue
				}
				if !d.at.Before(sweep.Started) {
					t.Errorf("%s carries a reading stamped %s, inside the sweep that collected it",
						metric, d.at.Format(time.RFC3339Nano))
					break
				}
			}
		}
	})

	t.Run("a current-state gauge is stamped when the sweep looked", func(t *testing.T) {
		at := otlpNewest(t, have, "github.repo.stars")
		if at.Before(sweep.Started) || at.After(sweep.Finished) {
			t.Errorf("the newest github.repo.stars is stamped %s, outside the sweep (%s to %s)",
				at.Format(time.RFC3339Nano), sweep.Started.Format(time.RFC3339),
				sweep.Finished.Format(time.RFC3339))
		}
	})
}

// ── Reading the collector's output back ─────────────────────────────────────

// otlpAwait reads the collector's file exporter until it holds this sweep.
//
// The exporter flushes on its own interval, a second here, and appends rather
// than replaces, so a stack kept between runs holds earlier sweeps' documents
// too: the wait is for this sweep's own readings, and the map keeps the last
// of each key, which is the most recent write of it.
func otlpAwait(ctx context.Context, t *testing.T, s *Stack, want map[string]float64) map[string]otlpDataPoint {
	t.Helper()
	have := map[string]otlpDataPoint{}
	err := WaitUntil(ctx, "the collector's file exporter", 2*time.Minute, func(context.Context) error {
		raw, err := os.ReadFile(s.OTelOutput)
		if err != nil {
			return err
		}
		clear(have)
		for line := range strings.SplitSeq(string(raw), "\n") {
			for _, d := range otlpParse(line) {
				have[d.key()] = d
			}
		}
		missing := 0
		for key := range want {
			if _, ok := have[key]; !ok {
				missing++
			}
		}
		if missing > 0 {
			return errors.New(strconv.Itoa(missing) + " of the sweep's readings have not been flushed yet")
		}
		return nil
	})
	if err != nil {
		// Reported rather than fatal: which readings are missing is the
		// finding, and the subtests name them.
		t.Errorf("the collector did not write the whole sweep out: %v", err)
	}
	return have
}

// otlpParse flattens one document of the file exporter into its data points.
func otlpParse(line string) []otlpDataPoint {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var doc struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []otlpWireMetric `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return nil
	}
	var out []otlpDataPoint
	for _, rm := range doc.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out = append(out, m.points()...)
			}
		}
	}
	return out
}

// otlpWireMetric is one metric of a document as the file exporter's protojson
// renders it. The sink sends every metric as a gauge.
type otlpWireMetric struct {
	Name  string `json:"name"`
	Gauge struct {
		DataPoints []otlpWirePoint `json:"dataPoints"`
	} `json:"gauge"`
}

// points is every data point of the metric that has a timestamp to key it by.
func (m otlpWireMetric) points() []otlpDataPoint {
	var out []otlpDataPoint
	for _, wire := range m.Gauge.DataPoints {
		if d, ok := wire.point(m.Name); ok {
			out = append(out, d)
		}
	}
	return out
}

// otlpWirePoint is one gauge data point as the file exporter's protojson
// renders it.
type otlpWirePoint struct {
	Attributes []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	} `json:"attributes"`
	TimeUnixNano string  `json:"timeUnixNano"`
	AsDouble     float64 `json:"asDouble"`
}

// point is the reading this data point carries for the named metric, or false
// when its timestamp is not a number, which leaves nothing to key it by.
func (w otlpWirePoint) point(metric string) (otlpDataPoint, bool) {
	nanos, err := strconv.ParseInt(w.TimeUnixNano, 10, 64)
	if err != nil {
		return otlpDataPoint{}, false
	}
	// An attribute whose value is empty is dropped here, so that the key
	// matches the oracle's: the file sink's JSON drops an empty tag, as the
	// line protocol does.
	attrs := map[string]string{}
	for _, a := range w.Attributes {
		if a.Value.StringValue != "" {
			attrs[a.Key] = a.Value.StringValue
		}
	}
	// A missing asDouble is protojson's rendering of zero, which decodes to
	// zero here anyway.
	return otlpDataPoint{
		metric: metric, attrs: attrs,
		at: time.Unix(0, nanos).UTC(), value: w.AsDouble,
	}, true
}

// ── What the oracle says the collector should have received ─────────────────

// otlpExpected is every reading the sweep should have sent: one per point and
// numeric field.
//
// A string field is not a metric and is left out, and so is a time field: the
// sink converts a time to a number for the line protocol but not here, because
// numeric() is what OTLP asks and it does not take one.
func otlpExpected(t *testing.T, sweep *pushSweep) map[string]float64 {
	t.Helper()
	want := map[string]float64{}
	for _, p := range pushPoints(t, sweep) {
		at, err := time.Parse(time.RFC3339Nano, p.Time)
		if err != nil {
			t.Fatalf("the point's date %q: %v", p.Time, err)
		}
		for field, raw := range p.Fields {
			value, ok := otlpNumber(raw)
			if !ok {
				continue
			}
			key := otlpName(p.Measurement, field) + "|" + esTagKey(p.Tags) + "|" + at.UTC().Format(time.RFC3339Nano)
			want[key] = value
		}
	}
	return want
}

func otlpNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// otlpName is the sink's metric name: OpenTelemetry's dotted convention, with
// the gh_ prefix dropped and every underscore of the measurement and the field
// turned into a dot.
func otlpName(measurement, field string) string {
	m := strings.TrimPrefix(measurement, "gh_")
	return "github." + strings.ReplaceAll(m, "_", ".") + "." + strings.ReplaceAll(field, "_", ".")
}

// otlpOne finds one reading, and says which dates are there when it is not.
func otlpOne(t *testing.T, have map[string]otlpDataPoint, metric, stamp string, attrs map[string]string) otlpDataPoint {
	t.Helper()
	var seen []string
	for _, d := range have {
		if d.metric != metric || !hasTags(d.attrs, attrs) {
			continue
		}
		if d.at.Format(time.RFC3339) == stamp {
			return d
		}
		seen = append(seen, d.at.Format(time.RFC3339))
	}
	sort.Strings(seen)
	seen = slices.Compact(seen)
	t.Fatalf("the collector received no %s at %s for %v; it received readings at %v", metric, stamp, attrs, seen)
	return otlpDataPoint{}
}

// otlpNewest is the most recent moment a metric was written at.
func otlpNewest(t *testing.T, have map[string]otlpDataPoint, metric string) time.Time {
	t.Helper()
	var newest time.Time
	for _, d := range have {
		if d.metric == metric && d.at.After(newest) {
			newest = d.at
		}
	}
	if newest.IsZero() {
		t.Fatalf("the collector received no %s at all", metric)
	}
	return newest
}
