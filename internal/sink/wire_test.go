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
	err := NewLoki("loki:3100", "", nil, 0, 0, 0).Write(context.Background(), point)
	if err == nil {
		t.Fatal("a sink posted to an endpoint that is not an http URL")
	}
	if !strings.Contains(err.Error(), "loki push:") || !strings.Contains(err.Error(), `"loki:3100"`) {
		t.Errorf("err = %v, want it to name the sink and the value", err)
	}
}
