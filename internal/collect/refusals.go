package collect

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// Refusals remembers the endpoints that answered "not available", so that the
// next sweep does not pay to hear it again.
//
// A 403 or a 404 carries no ETag, so a feature that is switched off is charged
// in full on every sweep where a page that did not change costs nothing.
// Measured on 2026-09-11 over eighteen repositories: security paid 22 of its
// 36 requests an hour that way (seventeen 403, five 404), analyses 11 of 18
// every six hours, inventory 6 a day and the SBOM 11 a day from its own
// bucket, which is 589 core points a day, a fifth of the daily spend, for
// answers the collectors already turn into "nothing here".
//
// The memory lives in the process, like the ETag cache, not in the state
// file: a restart asks once more, and the day is the ceiling on how late a
// feature the author switches on is noticed. The runner keeps one per family,
// so what is remembered is keyed by family, repository and endpoint, the
// last two being what the path names.
type Refusals struct {
	// For is how long a refusal is remembered. Zero means a day.
	For time.Duration

	// now is the clock, replaceable by a test that wants the day to pass.
	now func() time.Time

	mu    sync.Mutex
	until map[string]refusal
}

// refusal is what the endpoint answered and until when that answer stands.
type refusal struct {
	until time.Time
	err   *ghapi.UnavailableError
}

// GetJSON is ghapi.Client.GetJSON with the memory in front of it: a path
// refused within the window answers the same refusal without a request, and
// a fresh refusal is remembered. A nil *Refusals remembers nothing and simply
// asks, which is what the probe and the tests that do not care get.
//
// Only an UnavailableError is kept. A 202 is GitHub still computing and is
// gone on the next call, and a spent budget is a 403 too but never reaches
// here as one: the client types it apart precisely so nobody reads it as a
// feature that is off.
func (m *Refusals) GetJSON(ctx context.Context, c *ghapi.Client, path string, out any, accept string) (link string, cached bool, err error) {
	if m == nil {
		return c.GetJSON(ctx, path, out, accept)
	}
	if remembered := m.lookup(path); remembered != nil {
		return "", false, remembered
	}
	link, cached, err = c.GetJSON(ctx, path, out, accept)
	if unavailable, ok := errors.AsType[*ghapi.UnavailableError](err); ok {
		m.remember(path, unavailable)
	}
	return link, cached, err
}

// lookup answers the refusal still standing for a path, or nil.
func (m *Refusals) lookup(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.until[path]
	if !ok {
		return nil
	}
	if !m.clock().Before(r.until) {
		delete(m.until, path)
		return nil
	}
	return r.err
}

func (m *Refusals) remember(path string, err *ghapi.UnavailableError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.until == nil {
		m.until = map[string]refusal{}
	}
	m.until[path] = refusal{until: m.clock().Add(m.window()), err: err}
}

func (m *Refusals) window() time.Duration {
	if m.For <= 0 {
		return 24 * time.Hour
	}
	return m.For
}

func (m *Refusals) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}
