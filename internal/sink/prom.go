package sink

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prom serves the collected points as a Prometheus exposition page.
//
// It deliberately drops each point's timestamp. Prometheus stamps a sample at
// scrape time and refuses anything meaningfully older: measured against
// Prometheus 3.14 with the OTLP receiver enabled and a 30-minute out-of-order
// window, a point dated two days back comes back as HTTP 400. So the exporter
// can only ever say what is true now.
//
// The consequence is worth stating plainly rather than hiding: through this
// sink, GitHub's 14-day traffic window collapses to its most recent day, and
// the star history to the current total. The InfluxDB sink is the one that
// keeps the dated history. Both are supported because most people already run
// Prometheus, and today's number is still the number they want to alert on.
type Prom struct {
	Listen string
	Path   string

	mu      sync.RWMutex
	samples map[string]sample
	reducer *Reducer
	srv     *http.Server
	// Stale drops a series that has not been rewritten for this long, so a
	// repository that leaves the sweep stops being reported as if it were
	// still there.
	Stale time.Duration
}

type sample struct {
	name   string
	labels map[string]string
	value  float64
	seen   time.Time
}

// NewProm returns an exporter listening on addr.
func NewProm(listen, path string) *Prom {
	if path == "" {
		path = "/metrics"
	}
	return &Prom{
		Listen: listen, Path: path, samples: map[string]sample{}, Stale: 24 * time.Hour,
		reducer: NewReducer(),
	}
}

func (p *Prom) Name() string { return "prometheus" }

// Start begins serving. It returns as soon as the listener is up.
func (p *Prom) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc(p.Path, p.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p.Path, http.StatusFound)
	})
	p.srv = &http.Server{Addr: p.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := listen(p.Listen)
	if err != nil {
		return err
	}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

func (p *Prom) Close() error {
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

func (p *Prom) Write(_ context.Context, points []Point) error {
	// Reduced first: the collectors emit one row per fact, and Prometheus
	// wants one series per thing that has a current value. Summarize is where
	// that difference is decided, measurement by measurement.
	points = p.reducer.Reduce(points)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pt := range points {
		for field, raw := range pt.Fields {
			v, ok := numeric(raw)
			if !ok {
				continue // a string field has no place in an exposition page
			}
			name := metricName(pt.Measurement, field)
			key := name + "|" + labelKey(pt.Tags)
			p.samples[key] = sample{name: name, labels: pt.Tags, value: v, seen: now}
		}
	}
	// Expire quietly rather than growing without bound.
	for key, s := range p.samples {
		if now.Sub(s.seen) > p.Stale {
			delete(p.samples, key)
		}
	}
	return nil
}

func (p *Prom) handle(w http.ResponseWriter, _ *http.Request) {
	p.mu.RLock()
	list := make([]sample, 0, len(p.samples))
	for _, s := range p.samples {
		list = append(list, s)
	}
	p.mu.RUnlock()

	sort.Slice(list, func(i, j int) bool {
		if list[i].name != list[j].name {
			return list[i].name < list[j].name
		}
		return labelKey(list[i].labels) < labelKey(list[j].labels)
	})

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	lastName := ""
	for _, s := range list {
		if s.name != lastName {
			fmt.Fprintf(&b, "# TYPE %s gauge\n", s.name)
			lastName = s.name
		}
		b.WriteString(s.name)
		if len(s.labels) > 0 {
			b.WriteString("{")
			keys := make([]string, 0, len(s.labels))
			for k := range s.labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				if i > 0 {
					b.WriteString(",")
				}
				// Quoted by hand: escapeLabel already writes the three
				// escapes the exposition format has, and %q would escape
				// each of them a second time, in Go's syntax rather than
				// Prometheus's.
				b.WriteString(k + `="` + escapeLabel(s.labels[k]) + `"`)
			}
			b.WriteString("}")
		}
		fmt.Fprintf(&b, " %g\n", s.value)
	}
	_, _ = w.Write([]byte(b.String()))
}

// metricName turns gh_traffic + views into github_traffic_views. The gh_
// prefix becomes github_ because that is what a Prometheus user expects to
// type, and the measurement name is what InfluxDB users already read.
func metricName(measurement, field string) string {
	m := strings.TrimPrefix(measurement, "gh_")
	return "github_" + sanitize(m) + "_" + sanitize(field)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func labelKey(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(tags[k])
		b.WriteString(",")
	}
	return b.String()
}

func numeric(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
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
