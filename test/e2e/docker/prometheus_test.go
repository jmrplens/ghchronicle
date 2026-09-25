//go:build dockere2e

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What Prometheus makes of the exporter, and what the exporter makes of a
// sweep.
//
// This is the one sink that is scraped rather than pushed to, and the one that
// deliberately throws the dates away: Prometheus stamps a sample at scrape
// time and refuses anything meaningfully older, so the exporter can only ever
// say what is true now. That is not a gap in this suite, it is the property
// under test. GitHub's fourteen-day traffic window has to arrive as one
// number, a star history as a count, and every sample has to be undated on the
// page so that the scrape is what dates it.
//
// The scrape itself is the half this machine cannot always run. The exporter
// listens on the host, so Prometheus has to reach back out of its container,
// which a machine that firewalls its docker bridge refuses. The harness
// reports that as ErrExporterUnreachable and the scrape assertions skip on it,
// with the reason printed, rather than failing for something that is not the
// exporter's fault.

func TestExporterReducesTheSweepToWhatIsTrueNow(t *testing.T) {
	// No store is involved in this one. The exporter serves from inside the
	// collector process, on the host, and the page it serves is the whole of
	// what this asks about; the stack is what the test below needs.
	run := startExporterSweep(t)
	page := promScrape(t, run)
	points := promOraclePoints(t, run)

	t.Run("a snapshot is the newest reading", func(t *testing.T) {
		promWant(t, page, "github_repo_stars", map[string]string{"repo": "hello-world"}, 80)
		promWant(t, page, "github_repo_forks", map[string]string{"repo": "hello-world"}, 9)
	})

	t.Run("a window is its sum", func(t *testing.T) {
		// The whole point of the sum rule: GitHub reports traffic as a day
		// per row over a fourteen-day window, and "views over the window" is
		// a single number. The expectation is added up from the sweep's own
		// points rather than copied out of the fixture.
		want := promSum(points, "gh_traffic", "count", map[string]string{"kind": "views"})
		promWant(t, page, "github_traffic_count", map[string]string{"kind": "views"}, want)
	})

	t.Run("dated items are a count", func(t *testing.T) {
		// A series per star would never change again, so the stars become
		// how many there were, under a name that says so.
		want := float64(len(promSelect(points, "gh_star", nil)))
		promWant(t, page, "github_stars_gained_count", map[string]string{"repo": "hello-world"}, want)
	})

	t.Run("no sample carries a timestamp", func(t *testing.T) {
		// The exposition format allows a sample to carry its own millisecond
		// timestamp, and this exporter deliberately does not: Prometheus
		// refuses a sample much older than the scrape, and half of what this
		// tool collects is years old. A page that grew timestamps would be
		// rejected sample by sample by the very server it is written for.
		for line := range strings.SplitSeq(page, "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if promHasTimestamp(line) {
				t.Errorf("this sample carries a timestamp: %s", line)
			}
		}
	})

	t.Run("nothing that has no honest current value is on the page", func(t *testing.T) {
		// The contribution calendar, the weekly commit series and the daily
		// star history are history: there is no "now" for them, and the
		// reduction skips them rather than guessing. A name appearing here
		// means a rule changed. The star history would be the costliest to
		// get wrong: counted, its first read would put years of stars into
		// one increase(), and an unstar lowering a past day would read as a
		// counter reset. The current count is github_repo_stars already.
		for _, name := range []string{"github_contribution_day_", "github_commits_week_", "github_star_day_"} {
			if strings.Contains(page, "\n"+name) {
				t.Errorf("%s is on the page, though its measurement is skipped by the reduction", name)
			}
		}
		// Absent only counts if the sweep had some to leave out.
		if len(promSelect(points, "gh_star_day", nil)) == 0 {
			t.Error("the exporter's sweep collected no gh_star_day, so its absence from the page proves nothing")
		}
	})
}

func TestPrometheusScrapesTheExporter(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	run := startExporterSweep(t)

	if err := s.SetPrometheusTarget(ctx, run.Port); err != nil {
		if errors.Is(err, ErrExporterUnreachable) {
			// Not a failure of anything this suite is testing: the exporter
			// answers on loopback, which the subtests above prove, and this
			// machine will not let a container reach it.
			t.Skipf("this machine cannot scrape the exporter: %v", err)
		}
		t.Fatalf("pointing Prometheus at the exporter: %v", err)
	}

	t.Run("the series arrive", func(t *testing.T) {
		samples := promQuery(ctx, t, s, `github_repo_stars{repo="hello-world"}`)
		if len(samples) != 1 {
			t.Fatalf("Prometheus holds %d series for the repository's stars, want one", len(samples))
		}
		if samples[0].value != 80 {
			t.Errorf("github_repo_stars = %v, want 80", samples[0].value)
		}
	})

	t.Run("the star history is not a series", func(t *testing.T) {
		// Asked after the subtest above has seen a scrape land, so an empty
		// answer means the exporter left it out rather than that Prometheus
		// had not scraped yet.
		if samples := promQuery(ctx, t, s, `{__name__=~"github_star_day_.*"}`); len(samples) != 0 {
			t.Errorf("Prometheus holds %d series of the daily star history, which the "+
				"reduction skips: %v", len(samples), samples)
		}
	})

	t.Run("stamped when it scraped, not when the thing happened", func(t *testing.T) {
		// The other half of the dating rule, and the reason the InfluxDB sink
		// exists beside this one. A star from 2024 reaches Prometheus as part
		// of a count that is true now, and the sample is stamped now.
		samples := promQuery(ctx, t, s, `github_stars_gained_count{repo="hello-world"}`)
		if len(samples) != 1 {
			t.Fatalf("Prometheus holds %d series for the stars gained, want one", len(samples))
		}
		if age := time.Since(samples[0].at); age > 5*time.Minute {
			t.Errorf("the sample is %s old, so it was not stamped at the scrape", age.Round(time.Second))
		}
	})
}

// ── The exposition page ─────────────────────────────────────────────────────

// promScrape reads the exporter's page from the host, which is where it always
// answers whatever the container network allows.
func promScrape(t *testing.T, run *exporterSweep) string {
	t.Helper()
	page, err := storeText(t.Context(), run.URL)
	if err != nil {
		t.Fatalf("scraping the exporter: %v", err)
	}
	if strings.TrimSpace(page) == "" {
		t.Fatalf("the exporter served an empty page after its first sweep:\n%s", tail(run.Log))
	}
	return page
}

// promWant fails unless the page carries one sample of a metric with these
// labels, and it reads this value.
func promWant(t *testing.T, page, name string, labels map[string]string, want float64) {
	t.Helper()
	var seen []string
	for line := range strings.SplitSeq(page, "\n") {
		if !strings.HasPrefix(line, name+"{") && line != name {
			continue
		}
		got, value, ok := promSample(line)
		if !ok || !hasTags(got, labels) {
			continue
		}
		if value != want {
			t.Errorf("%s%v = %v, want %v", name, labels, value, want)
		}
		return
	}
	for line := range strings.SplitSeq(page, "\n") {
		if strings.HasPrefix(line, name) {
			seen = append(seen, line)
		}
	}
	t.Errorf("no %s with %v on the page; it carries %v", name, labels, seen)
}

// promHasTimestamp reports whether a sample line carries a timestamp after
// its value. The value is what follows the label set, and a label value may
// itself contain spaces ("Actions Linux", "Alice Example"), so the count is
// taken after the closing brace rather than over the whole line.
func promHasTimestamp(line string) bool {
	rest := line
	if brace := strings.LastIndexByte(line, '}'); brace >= 0 {
		rest = line[brace+1:]
	} else if _, after, found := strings.Cut(line, " "); found {
		rest = after
	}
	return len(strings.Fields(rest)) > 1
}

// promSample reads one exposition line into its labels and its value.
func promSample(line string) (labels map[string]string, value float64, ok bool) {
	cut := strings.LastIndexByte(line, ' ')
	if cut < 0 {
		return nil, 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line[cut+1:]), 64)
	if err != nil {
		return nil, 0, false
	}
	labels = map[string]string{}
	_, set, braced := strings.Cut(line[:cut], "{")
	if !braced {
		return labels, value, true
	}
	for pair := range strings.SplitSeq(strings.TrimSuffix(set, "}"), ",") {
		k, v, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		labels[k] = promLabelValue(v)
	}
	return labels, value, true
}

// promLabelValue is a label value without its quotes, or the value as written
// when it is not a quoted string.
func promLabelValue(v string) string {
	if unquoted, err := strconv.Unquote(v); err == nil {
		return unquoted
	}
	return v
}

// ── What the exporter's own sweep collected ─────────────────────────────────

// promOraclePoints is the file sink's JSON from the exporter's sweep. The
// exporter has nothing to read back once a process has exited, so the file
// sink runs beside it and is what the reductions are checked against.
func promOraclePoints(t *testing.T, run *exporterSweep) []sqlStoresPoint {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(run.Dir, "points.jsonl"))
	if err != nil {
		t.Fatalf("the exporter sweep wrote no points file: %v", err)
	}
	var points []sqlStoresPoint
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p sqlStoresPoint
		if decodeErr := json.Unmarshal([]byte(line), &p); decodeErr != nil {
			t.Fatalf("point %d is not JSON: %v", len(points)+1, decodeErr)
		}
		points = append(points, p)
	}
	if len(points) == 0 {
		t.Fatal("the exporter's sweep emitted no points at all")
	}
	return points
}

func promSelect(points []sqlStoresPoint, measurement string, tags map[string]string) []sqlStoresPoint {
	var out []sqlStoresPoint
	for _, p := range points {
		if p.Measurement == measurement && hasTags(p.Tags, tags) {
			out = append(out, p)
		}
	}
	return out
}

// promSum adds one field over the points a rule reduces together.
func promSum(points []sqlStoresPoint, measurement, field string, tags map[string]string) float64 {
	total := 0.0
	for _, p := range promSelect(points, measurement, tags) {
		if v, ok := p.Fields[field].(float64); ok {
			total += v
		}
	}
	return total
}

// ── Prometheus itself ───────────────────────────────────────────────────────

// promSampleAt is one instant-query result: the value and the moment
// Prometheus stamped it.
type promSampleAt struct {
	at    time.Time
	value float64
}

func promQuery(ctx context.Context, t *testing.T, s *Stack, expr string) []promSampleAt {
	t.Helper()
	var out struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	where := s.PrometheusURL + "/api/v1/query?query=" + url.QueryEscape(expr)
	if err := storeJSON(ctx, http.MethodGet, where, nil, &out); err != nil {
		t.Fatalf("querying %s: %v", expr, err)
	}
	samples := make([]promSampleAt, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		seconds, _ := r.Value[0].(float64)
		text, _ := r.Value[1].(string)
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			continue
		}
		samples = append(samples, promSampleAt{
			at:    time.Unix(int64(seconds), 0).UTC(),
			value: value,
		})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].value < samples[j].value })
	return samples
}
