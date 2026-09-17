package sink

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPushURLKeepsAGoodEndpointExactly(t *testing.T) {
	// The path is the contract for Loki and OTLP, so the check must hand back
	// what it was given rather than a normalized near-miss.
	for _, raw := range []string{
		"http://loki:3100/loki/api/v1/push",
		"https://collector.example:4318/v1/metrics",
		"http://127.0.0.1:8086",
	} {
		got, err := pushURL(raw)
		if err != nil {
			t.Errorf("pushURL(%q): %v", raw, err)
			continue
		}
		if got != raw {
			t.Errorf("pushURL(%q) = %q", raw, got)
		}
	}
}

func TestPushURLRefusesWhatIsNotAnHTTPEndpoint(t *testing.T) {
	for _, raw := range []string{
		"",                   // unset in the configuration
		"loki:3100/push",     // a host and port, no scheme
		"file:///etc/passwd", // a scheme these sinks do not speak
		"https://",           // a scheme and nothing to reach
	} {
		if got, err := pushURL(raw); err == nil {
			t.Errorf("pushURL(%q) = %q, want an error", raw, got)
		}
	}
}

func TestASinkNamesItselfAndTheEndpointItWasGiven(t *testing.T) {
	// Before the check, a misconfigured endpoint surfaced as net/http's
	// "unsupported protocol scheme", which names neither the sink nor the
	// setting the operator has to go and fix.
	point := []Point{{
		Measurement: "gh_star", Tags: map[string]string{"user": "a"},
		Fields: map[string]any{"starred": 1}, Time: time.Now(),
	}}
	_, err := NewLoki("loki:3100", "", nil, 0, 0, 0).Write(context.Background(), point)
	if err == nil {
		t.Fatal("a sink posted to an endpoint that is not an http URL")
	}
	if !strings.Contains(err.Error(), "loki push:") || !strings.Contains(err.Error(), `"loki:3100"`) {
		t.Errorf("err = %v, want it to name the sink and the value", err)
	}
}

func TestScalarReadsATimeAsUnixSecondsAndAnUnsetOneAsNothing(t *testing.T) {
	// A store that takes only numbers still gets a "created at", and a time
	// that was never set is not the year 1.
	if v, ok := scalar(time.Unix(1600000000, 0)); !ok || v != 1600000000 {
		t.Errorf("scalar(time) = %v, %v, want its Unix seconds", v, ok)
	}
	if _, ok := scalar(time.Time{}); ok {
		t.Error("scalar read an unset time as a number")
	}
	if v, ok := scalar(7); !ok || v != 7 {
		t.Errorf("scalar(7) = %v, %v", v, ok)
	}
}

func TestAnUndatedPointIsStampedNow(t *testing.T) {
	// A point with no time must not be written at the Unix epoch, where every
	// store would file it decades before anything else.
	before := time.Now()
	got := stampOf(Point{})
	if got.Before(before) {
		t.Errorf("stampOf(undated) = %v, want a time no earlier than %v", got, before)
	}
	at := time.Unix(5, 0)
	if dated := stampOf(Point{Time: at}); !dated.Equal(at) {
		t.Errorf("stampOf(dated) = %v, want %v", dated, at)
	}
}
