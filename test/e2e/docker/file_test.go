//go:build dockere2e

package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The file sink, which is this suite's oracle and therefore the one thing in
// it that has to be checked on its own terms.
//
// Every other assertion here compares a store against the JSON this sink
// wrote in the same sweep, rather than against a table copied by hand. That is
// only worth anything if the JSON is what the collectors produced, and if the
// two formats this sink writes describe the same points: the line protocol is
// what the Telegraf and InfluxDB sinks send, the JSON is what a log shipper
// tails, and a difference between them would make one of the two suites here
// quietly meaningless.

func TestFileSinkWritesTheDateOfTheEvent(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	points := pushPointsByMeasurement(t, sweep)

	t.Run("a dated event keeps the day it happened", func(t *testing.T) {
		p := pushPointAt(t, points["gh_star"], "gh_star", starGivenAt, map[string]string{"user": "alice"})
		if p.Fields["starred"] != float64(1) {
			t.Errorf("starred = %#v, want 1", p.Fields["starred"])
		}
	})

	t.Run("a daily snapshot keeps its own day", func(t *testing.T) {
		p := pushPointAt(t, points["gh_traffic"], "gh_traffic", trafficDayAt, map[string]string{"kind": "views"})
		if p.Fields["count"] != float64(120) || p.Fields["uniques"] != float64(18) {
			t.Errorf("the traffic day reads %v, want 120 views from 18 uniques", p.Fields)
		}
	})

	t.Run("a run keeps the moment it finished", func(t *testing.T) {
		p := pushPointAt(t, points["gh_workflow_run"], "gh_workflow_run", runFinishedAt,
			map[string]string{"conclusion": "success"})
		if p.Fields["duration_seconds"] != float64(220) {
			t.Errorf("duration_seconds = %#v, want 220", p.Fields["duration_seconds"])
		}
	})

	t.Run("a current-state gauge is stamped when the sweep looked", func(t *testing.T) {
		for _, p := range points["gh_repo"] {
			at := lokiWhen(t, p.Time)
			if at.Before(sweep.Started) || at.After(sweep.Finished) {
				t.Errorf("gh_repo is stamped %s, outside the sweep (%s to %s)",
					p.Time, sweep.Started.Format(time.RFC3339), sweep.Finished.Format(time.RFC3339))
			}
		}
	})

	t.Run("the file is readable by its owner and nobody else", func(t *testing.T) {
		// Deliberate, and documented as such: a sweep of a private account
		// puts repository names, Dependabot severities and whole job log
		// lines in this file. A shipper running as another user still reads
		// it once an operator grants that, which is a decision worth making.
		info, err := os.Stat(sweep.Points)
		if err != nil {
			t.Fatalf("the points file: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("the points file is mode %o, want 600", mode)
		}
	})
}

func TestFileSinkWritesTheSamePointsInBothFormats(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)
	twin := fileInfluxTwin(ctx, t)

	want := fileDatedPoints(t, sweep)
	if len(want) < 20 {
		t.Fatalf("only %d dated points in the sweep, which cannot be right", len(want))
	}

	t.Run("every dated point of the JSON is a line of the line protocol", func(t *testing.T) {
		for key, p := range want {
			line, ok := twin[key]
			if !ok {
				t.Errorf("the line protocol has no line for %s", key)
				continue
			}
			fileWantSameFields(t, key, p, line)
		}
	})

	t.Run("with the values the fixtures pin", func(t *testing.T) {
		// The fields fileWantSameFields cannot compare by value are the ones
		// computed against the wall clock; these three are not. They come
		// from the fixture, so the two renderings have to agree on them
		// exactly.
		fileWantValue(t, twin, "gh_star", starGivenAt, map[string]string{"user": "alice"}, "starred", int64(1))
		fileWantValue(t, twin, "gh_traffic", trafficDayAt, map[string]string{"kind": "views"}, "count", int64(120))
		fileWantValue(t, twin, "gh_workflow_run", runFinishedAt,
			map[string]string{"conclusion": "success"}, "duration_seconds", int64(220))
	})

	t.Run("and the same measurements are in both", func(t *testing.T) {
		fileWantSameMeasurements(t, fileIdentitiesByMeasurement(t, sweep), twin)
	})
}

// fileWantSameMeasurements compares the two formats measurement by
// measurement: each one has as many lines in the line protocol as it has
// distinct points in the JSON, and neither format has a measurement the other
// lacks.
func fileWantSameMeasurements(t *testing.T, asJSON map[string]map[string]bool, twin map[string]influxPoint) {
	t.Helper()
	asLines := map[string]int{}
	for _, p := range twin {
		asLines[p.measurement]++
	}
	for measurement, keys := range asJSON {
		if asLines[measurement] != len(keys) {
			t.Errorf("%s: %d points in the JSON, %d lines in the line protocol",
				measurement, len(keys), asLines[measurement])
		}
	}
	for measurement := range asLines {
		if _, ok := asJSON[measurement]; !ok {
			t.Errorf("%s is in the line protocol and not in the JSON", measurement)
		}
	}
}

// fileDatedPoints is every point of the sweep's JSON that carries a date of its
// own, keyed as the line protocol keys it.
//
// The dated points are the ones the two sweeps can be compared on directly: a
// gauge is stamped when its sweep looked, and these two swept at different
// moments. Everything with a date of its own is the same point in both.
func fileDatedPoints(t *testing.T, sweep *pushSweep) map[string]sqlStoresPoint {
	t.Helper()
	out := map[string]sqlStoresPoint{}
	for _, p := range pushPoints(t, sweep) {
		at := lokiWhen(t, p.Time)
		if at.Before(sweep.Started) && esHasUsableField(p) {
			out[influxKeyOf(t, p)] = p
		}
	}
	return out
}

// fileIdentitiesByMeasurement is the set of distinct points the sweep's JSON
// holds for each measurement.
//
// Counted by identity rather than by line: a sweep emits the same point twice
// for a few measurements, once per family that reports it, and both files
// carry both copies.
func fileIdentitiesByMeasurement(t *testing.T, sweep *pushSweep) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, p := range pushPoints(t, sweep) {
		if !esHasUsableField(p) {
			continue
		}
		if out[p.Measurement] == nil {
			out[p.Measurement] = map[string]bool{}
		}
		out[p.Measurement][influxKeyOf(t, p)] = true
	}
	return out
}

// fileWantSameFields compares the field names of one point in both formats.
//
// The names rather than the values, and that is not a weaker test by choice:
// the two formats come from two sweeps, and several fields are computed
// against the wall clock when the collector looks. `seconds_open` on an issue
// that is still open is the age of the issue now, and two sweeps eight seconds
// apart honestly disagree about it by eight. The values that come from the
// fixture instead are compared exactly, by the subtest above and by every
// other store in this suite.
func fileWantSameFields(t *testing.T, key string, want sqlStoresPoint, got influxPoint) {
	t.Helper()
	for name, raw := range want.Fields {
		if _, clash := want.Tags[name]; clash {
			continue // the tag wins, as in the line protocol
		}
		if _, ok := got.fields[name]; !ok && influxWritten(raw) {
			t.Errorf("%s: the line protocol has no field %s", key, name)
		}
	}
	for name := range got.fields {
		if _, ok := want.Fields[name]; !ok {
			t.Errorf("%s: the line protocol carries a field %s that the JSON does not", key, name)
		}
	}
}

// fileWantValue fails unless one field of one point reads the same in the
// line protocol as the fixture says.
func fileWantValue(t *testing.T, twin map[string]influxPoint, measurement, stamp string, tags map[string]string, field string, want any) {
	t.Helper()
	for _, p := range twin {
		if p.measurement != measurement || !hasTags(p.tags, tags) {
			continue
		}
		if p.at.Format(time.RFC3339) != stamp {
			continue
		}
		if p.fields[field] != want {
			t.Errorf("%s.%s at %s = %#v, want %#v", measurement, field, stamp, p.fields[field], want)
		}
		return
	}
	t.Errorf("the line protocol holds no %s at %s for %v", measurement, stamp, tags)
}

// fileInfluxTwin runs a second sweep whose only sink is the file sink in its
// other format, and returns what it wrote, keyed as every store here keys a
// point.
//
// A second sweep rather than a second sink on the first, because the sink
// takes one format: the same fixtures, the same collectors, the other
// renderer.
func fileInfluxTwin(ctx context.Context, t *testing.T) map[string]influxPoint {
	t.Helper()
	dir, err := pushWorkDir("file-influx")
	if err != nil {
		t.Fatalf("preparing the second sweep: %v", err)
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		t.Fatalf("building the collector: %v", err)
	}
	path := filepath.Join(dir, "points.influx")
	gh := newSQLStoresGitHub(t)
	cfg, err := fileInfluxConfig(dir, gh.URL(), path)
	if err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	log, err := sqlStoresExec(ctx, t, binary, cfg)
	if writeErr := os.WriteFile(filepath.Join(dir, "sweep.log"), []byte(log), 0o600); writeErr != nil {
		t.Errorf("keeping the sweep's log: %v", writeErr)
	}
	if err != nil {
		t.Fatalf("the second sweep did not finish: %v\n%s", err, tail(log))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the line protocol the second sweep wrote: %v", err)
	}
	out := map[string]influxPoint{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if p, ok := influxParse(line); ok {
			out[p.key()] = p
		}
	}
	if len(out) == 0 {
		t.Fatal("the second sweep wrote no line protocol at all")
	}
	return out
}

// fileInfluxConfig configures the file sink and nothing else: what the stores
// hold is the shared sweep's business, and a second writer into them would
// only make those assertions harder to read.
func fileInfluxConfig(dir, githubURL, path string) (string, error) {
	// Under every.families, the layer that names one family: the flat map this
	// used to write is refused by the loader.
	var every strings.Builder
	every.WriteString("  families:\n")
	for _, f := range sqlStoresFamilies {
		fmt.Fprintf(&every, "    %s: %s\n", f, pushSweepEvery)
	}
	body := fmt.Sprintf(`github:
  token: e2e-token
  base_url: %s
  timeout: 30s
targets:
  user: %s
sinks:
  file:
    path: %s
    format: influx
  dedupe_file: "off"
every:
%sstate_file: %s
log:
  level: debug
`, githubURL, sqlStoresLogin, path, every.String(), filepath.Join(dir, "state.json"))

	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		return "", err
	}
	return cfg, nil
}
