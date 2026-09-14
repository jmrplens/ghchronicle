package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OTLP writes metrics over OpenTelemetry's HTTP protocol.
//
// It speaks the JSON encoding rather than protobuf. That is a deliberate
// trade: JSON is larger on the wire, and it removes a code generator, a
// protobuf runtime and their transitive dependencies from a tool whose whole
// dependency list is one YAML parser. Every OTLP receiver worth using accepts
// `application/json` on the same endpoint.
//
// Whether the dated history survives depends entirely on the backend, and the
// difference is worth stating rather than discovering. OTLP data points carry
// an explicit timestamp, so a store that accepts them keeps GitHub's fourteen
// day traffic window as fourteen days. Prometheus's OTLP receiver does not: it
// rejects a sample much older than the scrape, measured as HTTP 400 for a point
// dated two days back. So the sink reduces to current values by default and
// sends the raw dated points only when asked.
type OTLP struct {
	// Endpoint is the full URL of the metrics path, for example
	// http://collector:4318/v1/metrics
	Endpoint string
	// Headers are sent with every request, for an API key or a tenant id.
	Headers map[string]string
	// Raw sends the dated points instead of the reduced current values. Use it
	// only with a backend that accepts old timestamps.
	Raw bool
	// Resource attributes describing who is reporting.
	Service string
	Batch   int
	// Repeat republishes the current state at this interval.
	//
	// Prometheus answers an instant query from the last sample within its
	// lookback window, five minutes by default. A gauge pushed once by a
	// family that runs every twelve hours is therefore invisible for eleven
	// hours and fifty-five minutes of every twelve. Pull-based scraping never
	// had this problem because the scrape is the repeat. Pushing has to
	// supply the repeat itself, which is what this is. Zero disables it,
	// which is right for a backend that keeps history on its own.
	Repeat time.Duration

	client  *http.Client
	reducer *Reducer
	// state is what Repeat republishes: the newest reduced point per series.
	mu    sync.Mutex
	state map[string]Point
	stop  chan struct{}
}

// NewOTLP returns a sink. A zero batch means 2000 data points per request.
func NewOTLP(endpoint, service string, headers map[string]string, raw bool, batch int, timeout time.Duration) *OTLP {
	if batch <= 0 {
		batch = 2000
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if service == "" {
		service = "ghchronicle"
	}
	return &OTLP{
		Endpoint: endpoint, Service: service, Headers: headers, Raw: raw,
		Batch: batch, client: &http.Client{Timeout: timeout},
		reducer: NewReducer(), state: map[string]Point{},
	}
}

func (o *OTLP) Name() string { return "otlp" }

// Start begins the republish loop, if one is configured.
func (o *OTLP) Start() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Repeat <= 0 || o.Raw || o.stop != nil {
		return
	}
	o.stop = make(chan struct{})
	stop := o.stop
	go func() {
		t := time.NewTicker(o.Repeat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				o.republish()
			}
		}
	}()
}

func (o *OTLP) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stop != nil {
		close(o.stop)
		o.stop = nil
	}
	return nil
}

// republish sends every known series again, stamped now.
func (o *OTLP) republish() {
	o.mu.Lock()
	points := make([]Point, 0, len(o.state))
	now := time.Now()
	for _, p := range o.state {
		p.Time = now
		points = append(points, p)
	}
	o.mu.Unlock()
	if len(points) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.client.Timeout)
	defer cancel()
	_ = o.send(ctx, points) // a missed heartbeat is not worth failing anything over
}

// The subset of the OTLP metrics schema this needs. Writing it out rather than
// generating it keeps the wire format visible: every field here appears in the
// JSON, and nothing else does.
type otlpRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type otlpMetric struct {
	Name  string    `json:"name"`
	Unit  string    `json:"unit,omitempty"`
	Gauge otlpGauge `json:"gauge"`
}

type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}

type otlpNumberDataPoint struct {
	Attributes   []otlpKeyValue `json:"attributes"`
	TimeUnixNano string         `json:"timeUnixNano"`
	AsDouble     float64        `json:"asDouble"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
}

func attr(k, v string) otlpKeyValue {
	return otlpKeyValue{Key: k, Value: otlpAnyValue{StringValue: &v}}
}

func (o *OTLP) Write(ctx context.Context, points []Point) error {
	if !o.Raw {
		points = o.reducer.Reduce(points)
		o.mu.Lock()
		for _, p := range points {
			o.state[p.Measurement+"|"+tagKey(p.Tags)] = p
		}
		o.mu.Unlock()
	}
	return o.send(ctx, points)
}

// send encodes and posts one batch.
func (o *OTLP) send(ctx context.Context, points []Point) error {
	// One metric per measurement and field, with the tags as data point
	// attributes. That is the shape every OTLP backend expects, and it maps
	// cleanly onto both Prometheus series and InfluxDB columns.
	byMetric := map[string][]otlpNumberDataPoint{}
	var order []string
	for _, p := range points {
		for _, field := range sortedKeys(p.Fields) {
			v, ok := numeric(p.Fields[field])
			if !ok {
				continue // a string has no place in a metric stream
			}
			name := otlpName(p.Measurement, field)
			if _, seen := byMetric[name]; !seen {
				order = append(order, name)
			}
			byMetric[name] = append(byMetric[name], otlpNumberDataPoint{
				Attributes:   otlpAttrs(p.Tags),
				TimeUnixNano: strconv.FormatInt(stampOf(p).UnixNano(), 10),
				AsDouble:     v,
			})
		}
	}
	sort.Strings(order)

	var metrics []otlpMetric
	sent := 0
	flush := func() error {
		if len(metrics) == 0 {
			return nil
		}
		err := o.post(ctx, otlpRequest{ResourceMetrics: []otlpResourceMetrics{{
			Resource: otlpResource{Attributes: []otlpKeyValue{
				attr("service.name", o.Service),
			}},
			ScopeMetrics: []otlpScopeMetrics{{
				Scope:   otlpScope{Name: "ghchronicle"},
				Metrics: metrics,
			}},
		}}})
		metrics, sent = nil, 0
		return err
	}
	for _, name := range order {
		dps := byMetric[name]
		metrics = append(metrics, otlpMetric{Name: name, Gauge: otlpGauge{DataPoints: dps}})
		sent += len(dps)
		if sent >= o.Batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (o *OTLP) post(ctx context.Context, body otlpRequest) error {
	endpoint, err := pushURL(o.Endpoint)
	if err != nil {
		return fmt.Errorf("otlp write: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("otlp write: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

// otlpName follows OpenTelemetry's convention of dots rather than the
// underscores Prometheus wants. A receiver exporting onward to Prometheus
// converts them itself; going the other way it cannot.
func otlpName(measurement, field string) string {
	m := strings.TrimPrefix(measurement, "gh_")
	return "github." + strings.ReplaceAll(m, "_", ".") + "." + strings.ReplaceAll(field, "_", ".")
}

func otlpAttrs(tags map[string]string) []otlpKeyValue {
	out := make([]otlpKeyValue, 0, len(tags))
	for _, k := range sortedKeys2(tags) {
		out = append(out, attr(k, tags[k]))
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
