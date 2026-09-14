package collect

import (
	"net/http"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

// A 403 has no ETag, so without the memory every sweep pays to be told again
// that Dependabot is off. With it the refusal is paid once a day, and the
// points are the same either way: the collector still writes the feature as
// off.
func TestRefusalsAnswerFromMemoryForADay(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/dependabot/alerts", http.StatusForbidden, "Dependabot alerts are disabled for this repository.")
	f.status("/repos/octocat/hello-world/code-scanning/alerts", http.StatusNotFound, "no analysis found")

	clock := testNow
	m := &Refusals{For: 24 * time.Hour, now: func() time.Time { return clock }}
	sc := Security{Refusals: m}
	first, err := sc.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sc.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/repos/octocat/hello-world/dependabot/alerts", "/repos/octocat/hello-world/code-scanning/alerts"} {
		if n := len(f.calls(path)); n != 1 {
			t.Errorf("%s asked %d times in two sweeps, want once: the refusal is remembered", path, n)
		}
	}
	if !samePoints(first, second) {
		t.Errorf("a remembered refusal changed the points:\n%v\n%v", first, second)
	}
	for _, feature := range []string{"dependabot", "code_scanning"} {
		p := find(t, second, "gh_security_feature", map[string]string{"feature": feature})
		if on, _ := p.Fields["enabled"].(bool); on {
			t.Errorf("%s = %v, want recorded as off from memory as it was from the 403", feature, p.Fields)
		}
	}

	// A day later the question is asked again, which is how a feature the
	// author switches on is noticed.
	clock = clock.Add(24 * time.Hour)
	if _, err = sc.Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/dependabot/alerts")); n != 2 {
		t.Errorf("after a day the endpoint was asked %d times in total, want 2", n)
	}
}

// Only "not available" is remembered. A 202 is GitHub still computing and is
// gone on the next call; a page that answers is never in the memory at all;
// and a nil memory asks every time, which is what the probe gets.
func TestRefusalsRememberOnlyWhatIsUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/computing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	f.handle("/fine", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	f.status("/gone", http.StatusNotFound, "Not Found")

	m := &Refusals{}
	for range 2 {
		for _, path := range []string{"/computing", "/fine", "/gone"} {
			var out map[string]any
			_, _, _ = m.GetJSON(ctx(t), f.Client, path, &out, "")
		}
	}
	for path, want := range map[string]int{"/computing": 2, "/fine": 2, "/gone": 1} {
		if n := len(f.calls(path)); n != want {
			t.Errorf("%s asked %d times, want %d", path, n, want)
		}
	}
	var none *Refusals
	for range 2 {
		var out map[string]any
		_, _, _ = none.GetJSON(ctx(t), f.Client, "/gone", &out, "")
	}
	if n := len(f.calls("/gone")); n != 3 {
		t.Errorf("a nil memory must ask every time, /gone asked %d times", n)
	}
}

// samePoints compares two collections field by field, ignoring order.
func samePoints(a, b []sink.Point) bool {
	if len(a) != len(b) {
		return false
	}
	lines := map[string]int{}
	for _, p := range a {
		lines[sink.LineProtocol(p)]++
	}
	for _, p := range b {
		lines[sink.LineProtocol(p)]--
	}
	for _, n := range lines {
		if n != 0 {
			return false
		}
	}
	return true
}
