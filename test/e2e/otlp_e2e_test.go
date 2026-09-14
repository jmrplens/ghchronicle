package e2e

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The subset of the OTLP JSON encoding these assertions read back.
type otlpKV struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string `json:"stringValue"`
	} `json:"value"`
}

type otlpDataPoint struct {
	Attributes   []otlpKV `json:"attributes"`
	TimeUnixNano string   `json:"timeUnixNano"`
	AsDouble     float64  `json:"asDouble"`
}

type otlpMetric struct {
	Name  string `json:"name"`
	Gauge struct {
		DataPoints []otlpDataPoint `json:"dataPoints"`
	} `json:"gauge"`
}

type otlpResourceMetrics struct {
	Resource struct {
		Attributes []otlpKV `json:"attributes"`
	} `json:"resource"`
	ScopeMetrics []struct {
		Scope struct {
			Name string `json:"name"`
		} `json:"scope"`
		Metrics []otlpMetric `json:"metrics"`
	} `json:"scopeMetrics"`
}

type otlpBody struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

var otlpMetricName = regexp.MustCompile(`^github\.[a-z0-9]+(\.[a-z0-9]+)*$`)

func decodeOTLP(t *testing.T, body []byte) otlpBody {
	t.Helper()
	var out otlpBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the OTLP body is not JSON: %v\n%s", err, truncate(body))
	}
	return out
}

func TestOTLPSinkPushesJSONMetrics(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	rec := newCapture(t, nil)
	dir := t.TempDir()
	cfg := writeSinkConfig(t, dir, gh.URL(), `  otlp:
    endpoint: `+rec.URL()+`/v1/metrics
    service: ghchronicle-e2e
    repeat: 200ms
    headers:
      X-Scope-OrgID: e2e-tenant`)

	// Serve mode, because repeat is a ticker: a one-shot run exits before it
	// has a chance to fire.
	proc := serveInBackground(t, cfg)
	awaitAccountRepublish(t, proc, rec)
	if out := proc.Stop(); strings.Contains(out, "sink write failed") {
		t.Errorf("the OTLP sink failed while serving:\n%s", out)
	}

	reqs := rec.Accepted()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the OTLP receiver")
	}
	names := map[string]int{}
	for _, r := range reqs {
		assertOTLPEnvelope(t, &r)
		for _, m := range metricsOfOnePush(t, decodeOTLP(t, r.Body)) {
			names[m.Name]++
			assertOTLPMetric(t, m)
		}
	}

	for _, want := range []string{"github.workflow.runs.count", "github.repo.stars", "github.account.followers"} {
		if names[want] == 0 {
			t.Errorf("%s was never pushed; metrics seen: %v", want, sortedNames(names))
		}
	}
	// gh_workflow_run_total is a different measurement from gh_workflow_run: a
	// per-repository aggregate the rules keep whole, whose dotted name sits
	// under the per-item measurement's prefix. Both halves are pinned, because
	// the prefix below cannot tell them apart.
	const runTotal = "github.workflow.run.total.runs"
	if names[runTotal] == 0 {
		t.Errorf("%s was never pushed, so the aggregate the rules keep went with the per-run series", runTotal)
	}

	// raw is off by default, so the dated per-item rows were reduced first.
	for name := range names {
		if name == runTotal {
			continue
		}
		if strings.HasPrefix(name, "github.workflow.run.") || strings.HasPrefix(name, "github.job.log.") {
			t.Errorf("%q was pushed, but the default reduction should have removed it", name)
		}
	}
}

// awaitAccountRepublish waits until the account gauges have been pushed twice.
// The account family writes once per sweep, so a second request carrying its
// metric can only be the repeat ticker firing.
func awaitAccountRepublish(t *testing.T, proc *bgProcess, rec *capture) {
	t.Helper()
	pushes := func() int {
		n := 0
		for _, r := range rec.Accepted() {
			if strings.Contains(string(r.Body), `"github.account.followers"`) {
				n++
			}
		}
		return n
	}
	awaitSweep(t, proc, 60*time.Second)
	if !waitFor(60*time.Second, func() bool { return pushes() >= 2 || proc.Exited() }) || pushes() < 2 {
		t.Fatalf("the repeat never republished the account gauges (%d pushes carried them):\n%s",
			pushes(), proc.Output())
	}
}

// assertOTLPEnvelope checks what every push carries whatever is in it: where it
// went, how it was encoded, and the header the config asked for.
func assertOTLPEnvelope(t *testing.T, r *capturedRequest) {
	t.Helper()
	if r.Method != http.MethodPost || r.Path != "/v1/metrics" {
		t.Errorf("%s %s, want POST /v1/metrics", r.Method, r.Path)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := r.Header.Get("X-Scope-OrgID"); got != "e2e-tenant" {
		t.Errorf("the configured header did not reach the request: %q", got)
	}
}

// metricsOfOnePush checks the shape around the metrics -- one resource, named
// after the configured service -- and returns every metric underneath it.
func metricsOfOnePush(t *testing.T, body otlpBody) []otlpMetric {
	t.Helper()
	if len(body.ResourceMetrics) != 1 {
		t.Fatalf("resourceMetrics has %d entries", len(body.ResourceMetrics))
	}
	rm := body.ResourceMetrics[0]
	service := ""
	for _, a := range rm.Resource.Attributes {
		if a.Key == "service.name" && a.Value.StringValue != nil {
			service = *a.Value.StringValue
		}
	}
	if service != "ghchronicle-e2e" {
		t.Errorf("resource attribute service.name = %q, want the configured service", service)
	}
	var out []otlpMetric
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

// assertOTLPMetric checks one metric's name and every data point under it.
func assertOTLPMetric(t *testing.T, m otlpMetric) {
	t.Helper()
	// Dots, following the OpenTelemetry convention. A receiver exporting
	// onward to Prometheus converts them; the other direction is not
	// recoverable.
	if !otlpMetricName.MatchString(m.Name) {
		t.Errorf("metric name %q is not dotted github.<measurement>.<field>", m.Name)
	}
	if len(m.Gauge.DataPoints) == 0 {
		t.Errorf("metric %q carries no data point", m.Name)
	}
	for _, dp := range m.Gauge.DataPoints {
		if _, err := strconv.ParseInt(dp.TimeUnixNano, 10, 64); err != nil {
			t.Errorf("metric %q: timeUnixNano %q does not parse: %v", m.Name, dp.TimeUnixNano, err)
		}
		for _, a := range dp.Attributes {
			if a.Value.StringValue == nil || *a.Value.StringValue == "" {
				t.Errorf("metric %q: attribute %q has no string value", m.Name, a.Key)
			}
		}
	}
}
