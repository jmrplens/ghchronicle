package e2e

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// influxConfig is the sinks block for a run that writes to rec and nothing
// else. exclude is spliced in verbatim so a test can pin the default as well
// as an explicit list.
func influxConfig(rec *capture, exclude string) string {
	cfg := `  influxdb:
    url: ` + rec.URL() + `
    token: influx-token
    org: acme
    bucket: github`
	if exclude != "" {
		cfg += "\n    exclude: " + exclude
	}
	return cfg
}

func TestInfluxSinkWritesLineProtocol(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), influxConfig(rec, ""))

	sweepOnce(t, cfg)

	reqs := rec.Requests()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the InfluxDB receiver")
	}
	for _, r := range reqs {
		assertInfluxWrite(t, &r)
	}

	points := parseLineProtocol(t, rec.Body())
	if len(points) < 100 {
		t.Fatalf("only %d lines arrived", len(points))
	}
	byName := measurementsOf(points)
	for _, want := range []string{"gh_traffic", "gh_repo", "gh_workflow_run", "gh_star", "gh_account"} {
		if byName[want] == 0 {
			t.Errorf("no %s line arrived; measurements seen: %v", want, sortedNames(byName))
		}
	}

	assertNanosecondStamps(t, points)

	traffic := trafficDay(points)
	if traffic == nil {
		t.Fatal("the traffic day from the fixture did not arrive on its own date")
	}
	if traffic.Tags["repo"] != "hello-world" || traffic.Tags["full_name"] != "octocat/hello-world" {
		t.Errorf("traffic tags = %v", traffic.Tags)
	}
	// The wire type matters: an integer written as a float changes the column
	// type in InfluxDB 3, which then refuses every later write.
	if got, ok := traffic.Fields["count"].(int64); !ok || got != 120 {
		t.Errorf("count field = %#v, want the integer 120 from the fixture", traffic.Fields["count"])
	}

	// The default exclude keeps the job log out of the metrics database.
	if byName["gh_job_log"] != 0 {
		t.Errorf("%d gh_job_log lines were written to InfluxDB despite the default exclude", byName["gh_job_log"])
	}
}

// TestTheSweepLogCountsWhatInfluxDBTookNotWhatItWasOffered: the excluded
// measurement is collected and offered like any other, and the line that
// reports the write used to count it as written. Production read
// "sink=influxdb family=joblogs points=440 unchanged=0" for a measurement the
// database has never held a row of, and a review spent an afternoon on it.
func TestTheSweepLogCountsWhatInfluxDBTookNotWhatItWasOffered(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), influxConfig(rec, ""))

	out := sweepOnce(t, cfg)

	if byName := measurementsOf(parseLineProtocol(t, rec.Body())); byName["gh_job_log"] != 0 {
		t.Fatalf("%d gh_job_log lines reached InfluxDB, so this proves nothing", byName["gh_job_log"])
	}
	line := regexp.MustCompile(`sink=influxdb family=joblogs points=(\d+) unchanged=\d+ filtered=(\d+)`)
	m := line.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the sweep log does not report what the InfluxDB sink filtered:\n%s", out)
	}
	if m[1] != "0" {
		t.Errorf("the log credits InfluxDB with writing %s job log points: %s", m[1], m[0])
	}
	if m[2] == "0" {
		t.Errorf("the log reports nothing filtered, so nothing was offered: %s", m[0])
	}
}

// assertInfluxWrite checks one write request: where it went, the org and
// bucket the query names, and the credentials and encoding it carried.
func assertInfluxWrite(t *testing.T, r *capturedRequest) {
	t.Helper()
	if r.Method != http.MethodPost || r.Path != "/api/v2/write" {
		t.Errorf("%s %s, want POST /api/v2/write", r.Method, r.Path)
	}
	q, err := url.ParseQuery(r.RawQuery)
	if err != nil {
		t.Fatalf("query %q: %v", r.RawQuery, err)
	}
	if q.Get("org") != "acme" || q.Get("bucket") != "github" {
		t.Errorf("query %q does not carry the configured org and bucket", r.RawQuery)
	}
	// Nanoseconds, because the traffic points are whole days and the workflow
	// ones are seconds, and one precision has to cover both.
	if q.Get("precision") != "ns" {
		t.Errorf("precision = %q, want ns", q.Get("precision"))
	}
	if got := r.Header.Get("Authorization"); got != "Token influx-token" {
		t.Errorf("Authorization = %q, want the configured token", got)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// trafficDay finds the view count the fixture dates to 30 August 2026, which
// is the one point whose whole journey the assertions below follow.
func trafficDay(points []lpPoint) *lpPoint {
	day := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC).UnixNano()
	for i, p := range points {
		if p.Measurement == "gh_traffic" && p.Tags["kind"] == "views" && p.Time == day {
			return &points[i]
		}
	}
	return nil
}

// TestInfluxExcludeIsWhatKeepsTheJobLogOut proves the previous test's absence
// is the exclude doing its job and not the collector failing to produce one.
func TestInfluxExcludeIsWhatKeepsTheJobLogOut(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), influxConfig(rec, "[]"))

	sweepOnce(t, cfg)

	byName := measurementsOf(parseLineProtocol(t, rec.Body()))
	if byName["gh_job_log"] == 0 {
		t.Errorf("with an empty exclude the job log should reach InfluxDB; measurements seen: %v",
			sortedNames(byName))
	}
}

// TestInfluxBisectsAroundAnUnparseableLine pins the documented behavior: one
// line the server will not parse must not cost the several thousand good ones
// it was batched with. The receiver refuses any batch containing a punch card
// line with the message InfluxDB uses, and the sink halves the batch until the
// offenders are alone.
func TestInfluxBisectsAroundAnUnparseableLine(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, func(body []byte) (int, string) {
		if strings.Contains(string(body), "gh_commit_punchcard,") {
			return 400, `{"code":"invalid","message":"unable to parse points: parse failed"}`
		}
		return 0, ""
	})
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), influxConfig(rec, ""))

	out := sweepOnce(t, cfg)

	if !strings.Contains(out, "influxdb rejected a line as unparseable") {
		t.Errorf("the rejected lines were not reported by content:\n%s", out)
	}
	// Five punch card points in the fixture, and every one of them is isolated
	// rather than the whole stats batch being lost.
	if !strings.Contains(out, "sink rejected some lines") || !strings.Contains(out, "rejected=5") {
		t.Errorf("the sweep did not report five rejected lines:\n%s", out)
	}

	accepted := parseLineProtocol(t, rec.Body())
	byName := measurementsOf(accepted)
	if byName["gh_commit_punchcard"] != 0 {
		t.Errorf("a line the server refused was counted as written")
	}
	// The good lines that shared the batch with the offenders still arrived.
	if byName["gh_commits_week"] == 0 {
		t.Errorf("the rest of the stats batch was lost with the bad lines; seen: %v", sortedNames(byName))
	}
	batches := 0
	for _, r := range rec.Requests() {
		if strings.Contains(string(r.Body), "gh_commits_week,") {
			batches++
		}
	}
	if batches < 3 {
		t.Errorf("the batch was sent %d times, so it was not bisected", batches)
	}
}
