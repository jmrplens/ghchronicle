package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/render"
	"github.com/jmrplens/ghchronicle/internal/sink"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// These tests drive the command the way a person or a unit file does: flags
// in, streams and an exit status out, against the fake GitHub both end to end
// suites collect from and stand-ins for the stores. The ones that run the
// command do not run in parallel, because each swaps exitProcess, and some
// notifyContext, which are package state by design (see their comments).

// fixtures is the directory the fake GitHub serves, relative to this package.
const fixtures = "../../test/e2e/testdata"

// notExited is the status of a run that returned without calling
// exitProcess, which is how the process ends with 0 after doing its work.
const notExited = -1

// syncBuffer is a stream more than one goroutine writes to: the logger is
// shared by the sweep's workers, and fatal writes from the caller's.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// outcome is how one run ended: the status it exited with, or notExited, and
// what it wrote to each stream.
type outcome struct {
	status         int
	stdout, stderr string
}

// runCommand runs the command with args and reports how it ended.
// exitProcess records the status instead of ending the test binary, and a
// second exit, which execute must never take, fails the test.
func runCommand(t *testing.T, args ...string) outcome {
	t.Helper()
	status := notExited
	previous := exitProcess
	exitProcess = func(code int) {
		if status != notExited {
			t.Errorf("exited twice, with %d and then with %d", status, code)
		}
		status = code
	}
	defer func() { exitProcess = previous }()
	var stdout, stderr syncBuffer
	execute(append([]string{"ghchronicle"}, args...), &stdout, &stderr)
	return outcome{status: status, stdout: stdout.String(), stderr: stderr.String()}
}

// signalsFrom stands ctx in for the signals the command listens for: when ctx
// ends, the run's context ends, the way SIGTERM ends it. A ctx that has
// already ended ends the run's context before the run sees it.
func signalsFrom(ctx context.Context, t *testing.T) {
	t.Helper()
	previous := notifyContext
	notifyContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		run, cancel := context.WithCancel(parent)
		stop := context.AfterFunc(ctx, cancel)
		if ctx.Err() != nil {
			cancel()
		}
		return run, func() { stop(); cancel() }
	}
	t.Cleanup(func() { notifyContext = previous })
}

// writeConfig writes a config that collects octocat from the GitHub at base,
// keeps its state in dir, and carries body verbatim after the targets. It
// answers the path to hand to -config.
func writeConfig(t *testing.T, dir, base, body string) string {
	t.Helper()
	cfg := fmt.Sprintf(`github:
  token: test-token
  base_url: %s
  timeout: 10s
targets:
  user: %s
state_file: %s
%s`, base, fakegh.Login, filepath.Join(dir, "state.json"), body)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// refusingGitHub is a GitHub that answers every request 401, which is what a
// revoked token gets. Discovery is the first request of every run, so this
// stops the run there.
func refusingGitHub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestExecutePrintOnlyFlagsWriteAndReturn covers the three flags that print
// and stop: each writes to standard output, nothing to standard error, and
// returns without an exit, which is a status of 0.
func TestExecutePrintOnlyFlagsWriteAndReturn(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want []string
	}{
		{"-version", []string{buildLine() + "\n"}},
		{"-card-layouts", append([]string{"fields: " + strings.Join(render.Fields(), ", ") + "\n"},
			layoutNames()...)},
		{"-groups", config.Groups()},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got := runCommand(t, tc.flag, "-config", filepath.Join(t.TempDir(), "absent.yaml"))
			if got.status != notExited || got.stderr != "" {
				t.Fatalf("%s = status %d, stderr %q, want a clean return", tc.flag, got.status, got.stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(got.stdout, w) {
					t.Errorf("%s printed %q, which lacks %q", tc.flag, got.stdout, w)
				}
			}
		})
	}
}

// layoutNames is the name of every card layout, each of which -card-layouts
// lists.
func layoutNames() []string {
	var out []string
	for _, l := range render.Layouts() {
		out = append(out, l.Name)
	}
	return out
}

// TestExecuteExitsOnFlagsTheWayTheFlagPackageDoes keeps the statuses a script
// reads: help asked for is 0 with the usage, and a flag the command does not
// know, or a value a flag cannot take, is 2 with the reason. Nothing is read
// from a config in any of them.
func TestExecuteExitsOnFlagsTheWayTheFlagPackageDoes(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		status int
		stderr []string
	}{
		{[]string{"-h"}, 0, []string{"Usage of ghchronicle:", "-config string"}},
		{[]string{"-help"}, 0, []string{"Usage of ghchronicle:", "-backfill-since string"}},
		{[]string{"-bogus"}, 2, []string{"flag provided but not defined: -bogus", "Usage of ghchronicle:"}},
		{[]string{"-once=maybe"}, 2, []string{`invalid boolean value "maybe" for -once`}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got := runCommand(t, tc.args...)
			if got.status != tc.status || got.stdout != "" {
				t.Errorf("status %d, stdout %q, want %d and nothing", got.status, got.stdout, tc.status)
			}
			for _, w := range tc.stderr {
				if !strings.Contains(got.stderr, w) {
					t.Errorf("stderr %q lacks %q", got.stderr, w)
				}
			}
		})
	}
}

// TestExecuteRefusesAConfigItCannotUse stops with 1 and the reason, before
// asking GitHub anything, for a file that is not there and for one that would
// collect into nowhere. -card-only waives the second, because the card is
// then the destination.
func TestExecuteRefusesAConfigItCannotUse(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.yaml")
	got := runCommand(t, "-config", missing, "-once")
	if got.status != 1 || got.stderr != "ghchronicle: open "+missing+": "+notFoundText(t, missing)+"\n" {
		t.Errorf("a missing config = %d, %q, want 1 and the path", got.status, got.stderr)
	}

	noSinks := writeConfig(t, dir, gh.URL(), "")
	got = runCommand(t, "-config", noSinks, "-once")
	if got.status != 1 || !strings.Contains(got.stderr, "sinks: enable at least one of") {
		t.Errorf("a config without sinks = %d, %q, want 1 and the rule", got.status, got.stderr)
	}
	if n := len(gh.Requests()); n != 0 {
		t.Errorf("%d requests reached GitHub from a run that could not start", n)
	}

	svg := filepath.Join(dir, "card.svg")
	got = runCommand(t, "-config", noSinks, "-card", svg, "-card-only")
	if got.status != notExited {
		t.Errorf("-card-only with no sinks = %d, %q, want the card drawn", got.status, got.stderr)
	}
}

// TestExecuteListsTheRepositories prints the repositories a sweep would
// collect, one per line, and collects nothing; when GitHub refuses the token
// it stops with 1 and GitHub's answer. The config narrows the groups, so the
// start-up line that names them is checked here as well.
func TestExecuteListsTheRepositories(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "groups: [account, repos]\nsinks:\n  stdout: true\n")

	got := runCommand(t, "-config", cfg, "-list")
	if got.status != notExited || got.stdout != fakegh.Login+"/hello-world\n" {
		t.Errorf("-list = %d, %q, want octocat/hello-world alone", got.status, got.stdout)
	}
	if !strings.Contains(got.stderr, `msg="metric groups selected" on=account,repos`) {
		t.Errorf("the log does not name the groups selected:\n%s", got.stderr)
	}
	for _, r := range gh.Requests() {
		if !strings.HasPrefix(r.Path, "/user") && !strings.HasPrefix(r.Path, "/users/") {
			t.Errorf("-list asked for %s, which is collection, not discovery", r.Path)
		}
	}

	refused := writeConfig(t, t.TempDir(), refusingGitHub(t), "sinks:\n  stdout: true\n")
	got = runCommand(t, "-config", refused, "-list")
	if got.status != 1 || got.stdout != "" || !strings.Contains(got.stderr, "ghchronicle: ") ||
		!strings.Contains(got.stderr, "401 Unauthorized") {
		t.Errorf("-list against a refusing GitHub = %d, %q, %q, want 1 and the 401",
			got.status, got.stdout, got.stderr)
	}
}

// TestExecuteOnceWritesTheSinksAndTheState runs one sweep into two file sinks
// and returns; the files hold what was collected and the state file remembers
// the sweep. The same sweep against a refusing GitHub stops with 1.
func TestExecuteOnceWritesTheSinksAndTheState(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	points := filepath.Join(dir, "points.jsonl")
	statements := filepath.Join(dir, "points.sql")
	logFile := filepath.Join(dir, "ghchronicle.log")
	cfg := writeConfig(t, dir, gh.URL(), fmt.Sprintf(`sinks:
  file:
    path: %s
    format: json
  sql:
    path: %s
log:
  format: json
  file: %s
`, points, statements, logFile))

	got := runCommand(t, "-config", cfg, "-once")
	if got.status != notExited {
		t.Fatalf("-once = %d, want a clean return:\n%s", got.status, got.stderr)
	}
	for _, path := range []string{points, statements, filepath.Join(dir, "state.json")} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Errorf("%s is missing or empty after a sweep: %v", path, err)
		}
	}
	if body, _ := os.ReadFile(points); !strings.Contains(string(body), `"measurement":"gh_repo"`) {
		t.Error("the file sink holds no gh_repo point after a sweep of a repository")
	}
	if !strings.Contains(got.stderr, `"msg":"repositories discovered"`) {
		t.Errorf("the log is not JSON as configured:\n%.400s", got.stderr)
	}
	// The run closed the log file on its way out, so everything it logged to
	// stderr is in the file as well.
	if file, err := os.ReadFile(logFile); err != nil || string(file) != got.stderr {
		t.Errorf("the log file does not hold what stderr got: %v", err)
	}

	refused := writeConfig(t, t.TempDir(), refusingGitHub(t), "sinks:\n  stdout: true\n")
	got = runCommand(t, "-config", refused, "-once")
	if got.status != 1 || !strings.Contains(got.stderr, "401 Unauthorized") {
		t.Errorf("-once against a refusing GitHub = %d, want 1 and the 401:\n%s", got.status, got.stderr)
	}
}

// TestExecuteCardBesideTheSinks draws the card from the same sweep the
// configured sinks are written, rather than in place of them.
func TestExecuteCardBesideTheSinks(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	points := filepath.Join(dir, "points")
	cfg := writeConfig(t, dir, gh.URL(), "sinks:\n  file:\n    path: "+points+"\n")
	svg := filepath.Join(dir, "card.svg")

	got := runCommand(t, "-config", cfg, "-card", svg)
	if got.status != notExited {
		t.Fatalf("-card = %d, want a clean return:\n%s", got.status, got.stderr)
	}
	for _, path := range []string{points, svg} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Errorf("%s is missing or empty after a sweep that drew the card: %v", path, err)
		}
	}
}

// TestExecuteCardDrawsTheSweep writes the card from one sweep, with the fields
// given trimmed of the spaces around their commas, and stops with 1 when the
// card cannot be drawn or cannot be written.
func TestExecuteCardDrawsTheSweep(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "")
	svg := filepath.Join(dir, "card.svg")
	unreachable := filepath.Join(dir, "absent", "card.svg")

	got := runCommand(t, "-config", cfg, "-card", svg, "-card-only",
		"-card-theme", "dark", "-card-fields", "stars, forks")
	if got.status != notExited {
		t.Fatalf("-card = %d, want a clean return:\n%s", got.status, got.stderr)
	}
	body, err := os.ReadFile(svg)
	if err != nil || !strings.HasPrefix(string(body), "<svg") {
		t.Fatalf("the card is not an SVG: %v, %.80q", err, body)
	}
	if _, err = os.Stat(svg + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temporary file was left beside the card: %v", err)
	}
	if !strings.Contains(got.stderr, "card written") {
		t.Errorf("the log does not say the card was written:\n%s", got.stderr)
	}

	for _, tc := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"a theme there is none of", []string{"-card", svg, "-card-theme", "neon"}, `"neon"`},
		{"a field there is none of", []string{"-card", svg, "-card-fields", "stars,karma"}, "karma"},
		{"a directory that is not there", []string{"-card", unreachable}, notFoundText(t, unreachable)},
		{"a motion there is none of", []string{"-card", svg, "-card-motion", "bounce"}, `"bounce"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := runCommand(t, append([]string{"-config", cfg, "-card-only"}, tc.args...)...)
			if failed.status != 1 || !strings.Contains(failed.stderr, "ghchronicle: ") ||
				!strings.Contains(failed.stderr, tc.stderr) {
				t.Errorf("status %d, want 1 and %q in:\n%s", failed.status, tc.stderr, failed.stderr)
			}
		})
	}
}

// TestExecuteBothThemesComeFromOneSweep writes the light and the dark card of
// one sweep, so the two pictures of a <picture> element can never show numbers
// from different moments, and passes the motion through to both.
func TestExecuteBothThemesComeFromOneSweep(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "")
	light := filepath.Join(dir, "card.svg")
	dark := filepath.Join(dir, "card_dark.svg")

	// A layout that loops, because the point here is that the motion reaches
	// both files: sparkline-hero only reveals, and loop draws it the card once
	// draws, so it could not tell the two motions apart.
	got := runCommand(t, "-config", cfg, "-card", light, "-card-only",
		"-card-layout", "terminal", "-card-theme", "both", "-card-motion", "loop")
	if got.status != notExited {
		t.Fatalf("-card-theme both = %d:\n%s", got.status, got.stderr)
	}
	lightSVG, err := os.ReadFile(light)
	if err != nil {
		t.Fatal(err)
	}
	darkSVG, err := os.ReadFile(dark)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lightSVG), "#ffffff") || !strings.Contains(string(darkSVG), "#0d1117") {
		t.Error("the light card must carry the light palette and the dark card the dark one")
	}
	for name, body := range map[string][]byte{light: lightSVG, dark: darkSVG} {
		if !strings.Contains(string(body), "infinite") {
			t.Errorf("%s does not loop", name)
		}
	}
	if strings.Count(got.stderr, "card written") != 2 {
		t.Errorf("the log must say each card was written:\n%s", got.stderr)
	}
}

// TestExecuteRefusesACardItCannotDrawBeforeItSweeps checks the options before
// the sweep spends any of the rate limit on a card that was never going to be
// written.
func TestExecuteRefusesACardItCannotDrawBeforeItSweeps(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "")
	got := runCommand(t, "-config", cfg, "-card", filepath.Join(dir, "card.svg"), "-card-only", "-card-motion", "bounce")
	if got.status != 1 || !strings.Contains(got.stderr, `"bounce"`) {
		t.Fatalf("status %d, want 1 naming the motion:\n%s", got.status, got.stderr)
	}
	if strings.Contains(got.stderr, "sweep") {
		t.Errorf("the run swept before it refused the card:\n%s", got.stderr)
	}
}

// collectorOnly narrows a backfill to the collector group, the rate limit
// alone. Against the fake a whole backfill walks the commit and deployment
// history the fake answers every page of, hundreds of thousands of points and
// most of a minute, and what is under test here is the bound, not the walk.
const collectorOnly = "groups: [collector]\n"

// TestExecuteBackfillHonoursItsBound covers the three bounds a backfill can
// have: the one on the command line, which wins over the config's, none at
// all, and one that is not a bound, which stops the run with 1 before GitHub
// is asked anything. A backfill that GitHub refuses stops with 1 too.
func TestExecuteBackfillHonoursItsBound(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	bounded := writeConfig(t, dir, gh.URL(), collectorOnly+
		"backfill:\n  since: 2y\nsinks:\n  file:\n    path: "+filepath.Join(dir, "points")+"\n")

	got := runCommand(t, "-config", bounded, "-backfill", "-backfill-since", "2024-01-01")
	if got.status != notExited {
		t.Fatalf("-backfill = %d, want a clean return:\n%s", got.status, got.stderr)
	}
	for _, line := range []string{"since=2024-01-01", "backfill finished"} {
		if !strings.Contains(got.stderr, line) {
			t.Errorf("the log lacks %q:\n%s", line, got.stderr)
		}
	}

	unbounded := writeConfig(t, t.TempDir(), gh.URL(), collectorOnly+
		"sinks:\n  file:\n    path: "+filepath.Join(dir, "more")+"\n")
	got = runCommand(t, "-config", unbounded, "-backfill")
	if got.status != notExited || !strings.Contains(got.stderr, "backfill has no lower bound") {
		t.Errorf("-backfill with no bound = %d, want the log to say so:\n%s", got.status, got.stderr)
	}

	before := len(gh.Requests())
	got = runCommand(t, "-config", bounded, "-backfill", "-backfill-since", "yesterday")
	if got.status != 1 || !strings.Contains(got.stderr, `ghchronicle: backfill.since: "yesterday" is not a date`) {
		t.Errorf("-backfill-since yesterday = %d, want 1 and the reason:\n%s", got.status, got.stderr)
	}
	if after := len(gh.Requests()); after != before {
		t.Errorf("%d requests reached GitHub from a backfill with no usable bound", after-before)
	}

	refused := writeConfig(t, t.TempDir(), refusingGitHub(t), "sinks:\n  stdout: true\n")
	got = runCommand(t, "-config", refused, "-backfill")
	if got.status != 1 || !strings.Contains(got.stderr, "401 Unauthorized") {
		t.Errorf("-backfill against a refusing GitHub = %d, want 1 and the 401:\n%s", got.status, got.stderr)
	}
}

// TestExecuteBackfillCutShortIsNotAFailure: a backfill whose sweep fails
// after a signal has ended its context returns, saying it finished, rather
// than exiting 1, because what it reached has been written. The GitHub here
// refuses the token, so the same backfill with its context alive exits 1,
// which the last test above holds.
func TestExecuteBackfillCutShortIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir, refusingGitHub(t), "sinks:\n  file:\n    path: "+filepath.Join(dir, "points")+"\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	signalsFrom(ctx, t)
	got := runCommand(t, "-config", cfg, "-backfill")
	if got.status != notExited || !strings.Contains(got.stderr, "backfill finished") {
		t.Errorf("a canceled backfill = %d, want a clean return:\n%s", got.status, got.stderr)
	}
}

// TestExecuteServesUntilTheContextEnds runs the long-lived path: it starts the
// exporter, sweeps, serves what it collected on /metrics, and returns without
// an exit when its context ends the way SIGTERM ends it.
func TestExecuteServesUntilTheContextEnds(t *testing.T) {
	gh := fakegh.New(t, fixtures)
	dir := t.TempDir()
	addr := freeAddr(t)
	cfg := writeConfig(t, dir, gh.URL(), fmt.Sprintf(`sinks:
  prometheus:
    listen: %s
log:
  level: debug
`, addr))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	signalsFrom(ctx, t)
	done := make(chan outcome, 1)
	go func() { done <- runCommand(t, "-config", cfg) }()

	var metrics string
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && !strings.Contains(metrics, "github_repo_stars") {
		metrics = scrape(t, "http://"+addr+"/metrics")
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(metrics, "github_repo_stars") {
		t.Errorf("the exporter never served the sweep; last scrape:\n%.400s", metrics)
	}
	cancel()

	select {
	case got := <-done:
		if got.status != notExited {
			t.Errorf("the serve loop ended with %d, want a clean return:\n%s", got.status, got.stderr)
		}
		if !strings.Contains(got.stderr, "ghchronicle running") {
			t.Errorf("the log does not say the loop started:\n%.400s", got.stderr)
		}
	case <-time.After(time.Minute):
		t.Fatal("the serve loop did not end when its context did")
	}
}

// TestExecuteRefusesAPortAlreadyHeld stops with 1 at start-up when the
// exporter's port is taken, rather than running a sweep nobody can scrape.
func TestExecuteRefusesAPortAlreadyHeld(t *testing.T) {
	held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	gh := fakegh.New(t, fixtures)
	cfg := writeConfig(t, t.TempDir(), gh.URL(),
		"sinks:\n  prometheus:\n    listen: "+held.Addr().String()+"\n")

	got := runCommand(t, "-config", cfg)
	if got.status != 1 || !strings.Contains(got.stderr, "ghchronicle: prometheus exporter: ") {
		t.Errorf("status %d, want 1 and the exporter named:\n%s", got.status, got.stderr)
	}
}

// TestExecuteOneShotRunsLeaveTheExportersPortAlone runs each of the three
// one-shot shapes with the exporter configured on a port a long-running
// instance already holds, which is the ordinary case of a scheduled -card
// beside a serving ghchronicle. Each is a one-shot on its own, not only in
// company, so each must finish without trying to take the port.
func TestExecuteOneShotRunsLeaveTheExportersPortAlone(t *testing.T) {
	held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	gh := fakegh.New(t, fixtures)

	for _, tc := range []struct {
		name string
		args func(dir string) []string
	}{
		{"-once", func(string) []string { return []string{"-once"} }},
		{"-backfill", func(string) []string { return []string{"-backfill"} }},
		{"-card", func(dir string) []string { return []string{"-card", filepath.Join(dir, "card.svg")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := writeConfig(t, dir, gh.URL(), collectorOnly+
				"sinks:\n  prometheus:\n    listen: "+held.Addr().String()+"\n")
			got := runCommand(t, append([]string{"-config", cfg}, tc.args(dir)...)...)
			if got.status != notExited || strings.Contains(got.stderr, "prometheus exporter") {
				t.Errorf("%s beside a held exporter port = %d, want a clean return:\n%s",
					tc.name, got.status, got.stderr)
			}
		})
	}
}

// freeAddr is a loopback address nothing listens on right now, for a sink
// that is configured with an address rather than handed a listener.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// scrape is the body at url, or nothing while the exporter is not up yet.
func scrape(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// stores is a stand-in for every store that is reached over HTTP. Influx
// refuses to parse whatever it is sent and Elasticsearch rejects every
// document, which are the two answers the command logs with its own words;
// everything else is accepted.
func stores(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v2/write"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"invalid","message":"parse failed: bad field"}`)
		case r.URL.Path == "/_bulk":
			_, _ = io.WriteString(w, `{"errors":true,"items":[{"index":{"_index":"ghchronicle-gh_repo",`+
				`"error":{"type":"mapper_parsing_exception","reason":"failed to parse"}}}]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// graphiteReceiver accepts plaintext connections and discards what they send.
func graphiteReceiver(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// TestBuildSinksBuildsEveryConfiguredSink configures all ten sinks and gets
// ten, the exporter left out of a one-shot run, and a point written through
// each HTTP store that refuses it logged in the command's own words.
func TestBuildSinksBuildsEveryConfiguredSink(t *testing.T) {
	dir := t.TempDir()
	url := stores(t)
	off := false
	cfg := &config.Config{
		GitHub:    config.GitHub{Timeout: "5s"},
		StateFile: filepath.Join(dir, "state.json"),
		Sinks: config.Sinks{
			Influx:        &config.InfluxSink{URL: url, Bucket: "github", Batch: 10, Exclude: []string{"gh_job_log"}},
			Prometheus:    &config.PrometheusSink{Listen: freeAddr(t), Path: "/metrics"},
			OTLP:          &config.OTLPSink{Endpoint: url + "/v1/metrics", Service: "ghchronicle", Batch: 10},
			Loki:          &config.LokiSink{URL: url + "/loki/api/v1/push", Batch: 10},
			File:          &config.FileSink{Path: filepath.Join(dir, "points"), Format: "json"},
			Stdout:        true,
			StdoutFormat:  "json",
			Telegraf:      &config.TelegrafSink{URL: url + "/telegraf", Batch: 10, Dedupe: &off},
			Graphite:      &config.GraphiteSink{Addr: graphiteReceiver(t), Prefix: "github", Batch: 10},
			SQL:           &config.SQLSink{Dialect: "postgres", Path: filepath.Join(dir, "points.sql")},
			Elasticsearch: &config.ElasticsearchSink{URL: url, Prefix: "ghchronicle", Batch: 10},
			DedupeFile:    filepath.Join(dir, "written.bin"),
		},
	}
	var logs syncBuffer
	logger, _ := newLogger(config.Log{Level: "debug"}, &logs)

	oneShot, err := buildSinks(cfg, logger, true)
	if err != nil {
		t.Fatal(err)
	}
	names := sinkNames(oneShot)
	if strings.Contains(names, "prometheus") || len(oneShot) != 9 {
		t.Errorf("a one-shot run built %s, want every sink but the exporter", names)
	}
	closeAll(t, oneShot)

	serving, err := buildSinks(cfg, logger, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(t, serving)
	if len(serving) != 10 {
		t.Errorf("a serving run built %s, want all ten", sinkNames(serving))
	}
	if !strings.Contains(logs.String(), "loaded the written-points ledger") {
		t.Errorf("a serving run did not load the ledger:\n%s", logs.String())
	}

	point := []sink.Point{{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "octocat/hello-world"},
		Fields: map[string]any{"stars": 1}, Time: time.Now(),
	}}
	for _, s := range serving {
		if s.Name() == "influxdb" || s.Name() == "elasticsearch" {
			// Both refuse the point, which is a partial success and returns
			// the count; the log lines are what is checked.
			_ = s.Write(t.Context(), point)
		}
	}
	for _, line := range []string{
		"influxdb rejected a line as unparseable",
		"elasticsearch rejected a document",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, logs.String())
		}
	}
}

// TestTheLokiSinkKeepsAnHourWhenTheConfigSaysNothing walks the whole path a
// `loki:` block with no max_age takes, because the defect was in the seam:
// the config layer answered seven days for the absent key, so the sink's own
// one hour could never be reached and the documented value was never the
// value a deployment got.
func TestTheLokiSinkKeepsAnHourWhenTheConfigSaysNothing(t *testing.T) {
	cfg := &config.Config{Sinks: config.Sinks{
		Loki:       &config.LokiSink{URL: "http://loki:3100/loki/api/v1/push"},
		DedupeFile: "off",
	}}
	logger, _ := newLogger(config.Log{Level: "error"}, io.Discard)
	built, err := buildSinks(cfg, logger, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(t, built)
	loki, ok := built[0].(*sink.Loki)
	if !ok {
		t.Fatalf("built %s, want the loki sink", sinkNames(built))
	}
	if loki.MaxAge != time.Hour {
		t.Errorf("max_age resolved to %v, want an hour: anything longer is outside Loki's out-of-order window", loki.MaxAge)
	}
}

// TestBuildSinksWithTheLedgerOff builds the sinks without the ledger, which
// is what dedupe_file: off asks for, and the plain stdout sink.
func TestBuildSinksWithTheLedgerOff(t *testing.T) {
	cfg := &config.Config{Sinks: config.Sinks{Stdout: true, DedupeFile: "off"}}
	var logs syncBuffer
	logger, _ := newLogger(config.Log{Level: "debug"}, &logs)
	got, err := buildSinks(cfg, logger, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(t, got)
	if len(got) != 1 || got[0].Name() != "stdout" {
		t.Errorf("built %s, want stdout alone", sinkNames(got))
	}
	if strings.Contains(logs.String(), "ledger") {
		t.Errorf("a ledger switched off was loaded:\n%s", logs.String())
	}
}

// TestBuildSinksWritesJSONToStdoutOnlyWhenAskedFor tells the two stdout sinks
// apart by what they are, because both are named "stdout": a count or a name
// passes whichever of the two was built, and a pipeline reading JSON lines
// that is handed line protocol breaks on the first point.
func TestBuildSinksWritesJSONToStdoutOnlyWhenAskedFor(t *testing.T) {
	logger, _ := newLogger(config.Log{Level: "error"}, io.Discard)
	for _, tc := range []struct {
		format string
		json   bool
	}{
		{"json", true},
		{"influx", false},
		{"", false},
	} {
		t.Run("stdout_format "+tc.format, func(t *testing.T) {
			cfg := &config.Config{Sinks: config.Sinks{Stdout: true, StdoutFormat: tc.format, DedupeFile: "off"}}
			built, err := buildSinks(cfg, logger, true)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAll(t, built)
			if len(built) != 1 {
				t.Fatalf("built %s, want stdout alone", sinkNames(built))
			}
			_, isJSON := built[0].(*sink.StdoutJSON)
			_, isLines := built[0].(*sink.Stdout)
			if isJSON != tc.json || isLines == tc.json {
				t.Errorf("stdout_format %q built %T, want JSON %v", tc.format, built[0], tc.json)
			}
		})
	}
}

// TestBuildSinksSkipsTheLedgerOnlyForASinkThatRefusesIt holds each store's
// own dedupe key to the ledger: absent and true both mean the ledger decides
// what is written again, and only false hands the store every point. A sink
// wrapped when it asked not to be would silently drop what it asked to get.
func TestBuildSinksSkipsTheLedgerOnlyForASinkThatRefusesIt(t *testing.T) {
	on, off := true, false
	logger, _ := newLogger(config.Log{Level: "error"}, io.Discard)
	url := stores(t)
	for _, tc := range []struct {
		name   string
		dedupe *bool
		ledger bool
	}{
		{"the key absent", nil, true},
		{"dedupe: true", &on, true},
		{"dedupe: false", &off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Sinks: config.Sinks{
				Telegraf:   &config.TelegrafSink{URL: url + "/telegraf", Batch: 10, Dedupe: tc.dedupe},
				DedupeFile: filepath.Join(t.TempDir(), "written.bin"),
			}}
			built, err := buildSinks(cfg, logger, false)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAll(t, built)
			if len(built) != 1 {
				t.Fatalf("built %s, want telegraf alone", sinkNames(built))
			}
			if _, wrapped := built[0].(*sink.Unchanged); wrapped != tc.ledger {
				t.Errorf("%s built %T, want it behind the ledger: %v", tc.name, built[0], tc.ledger)
			}
		})
	}
}

// sinkNames lists what was built, for a failure message.
func sinkNames(sinks []sink.Sink) string {
	names := make([]string, len(sinks))
	for i, s := range sinks {
		names[i] = s.Name()
	}
	return "[" + strings.Join(names, " ") + "]"
}

// closeAll closes what buildSinks opened.
func closeAll(t *testing.T, sinks []sink.Sink) {
	t.Helper()
	for _, s := range sinks {
		if err := s.Close(); err != nil {
			t.Errorf("closing %s: %v", s.Name(), err)
		}
	}
}

// TestNewLoggerTakesTheConfiguredLevelAndFormat checks each level lets
// through what it should and no more, the JSON format, and the log file that
// receives the same lines as standard error rather than instead of it.
func TestNewLoggerTakesTheConfiguredLevelAndFormat(t *testing.T) {
	for _, tc := range []struct {
		level    string
		dropped  string
		included string
	}{
		{"debug", "", "level=DEBUG"},
		{"", "level=DEBUG", "level=INFO"},
		{"warn", "level=INFO", "level=WARN"},
		{"error", "level=WARN", "level=ERROR"},
	} {
		var out syncBuffer
		logger, closer := newLogger(config.Log{Level: tc.level}, &out)
		if closer != nil {
			t.Errorf("level %q opened a file nobody asked for", tc.level)
		}
		logger.Debug("d")
		logger.Info("i")
		logger.Warn("w")
		logger.Error("e")
		if !strings.Contains(out.String(), tc.included) ||
			(tc.dropped != "" && strings.Contains(out.String(), tc.dropped)) {
			t.Errorf("level %q wrote:\n%s\nwant %s and not %q", tc.level, out.String(), tc.included, tc.dropped)
		}
	}

	path := filepath.Join(t.TempDir(), "ghchronicle.log")
	var out syncBuffer
	logger, closer := newLogger(config.Log{Format: "json", File: path}, &out)
	logger.Info("to both")
	if closer == nil {
		t.Fatal("a log file was configured and nothing is there to close it")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"stderr": out.String(), "the file": string(file)} {
		if !strings.Contains(got, `"msg":"to both"`) {
			t.Errorf("%s lacks the JSON line: %q", name, got)
		}
	}
}

// TestNewAPISaysWhenItWaits logs the wait the client announces when a
// backfill has spent the budget, rounded to the second.
func TestNewAPISaysWhenItWaits(t *testing.T) {
	var out syncBuffer
	logger, _ := newLogger(config.Log{}, &out)
	api := newAPI(&config.Config{GitHub: config.GitHub{Token: "t"}}, true, logger)
	api.OnWait("core", 90*time.Second+400*time.Millisecond)
	if want := `msg="budget spent, waiting for the window to reset" bucket=core wait=1m30s`; !strings.Contains(out.String(), want) {
		t.Errorf("the log is %q, want %q", out.String(), want)
	}
}

// TestReplaceFileRefusesWhatItCannotPlace fails, rather than half-writing,
// when the directory is missing or the path is a directory.
func TestReplaceFileRefusesWhatItCannotPlace(t *testing.T) {
	dir := t.TempDir()
	if err := replaceFile(filepath.Join(dir, "absent", "card.svg"), []byte("<svg/>")); err == nil {
		t.Error("replaceFile wrote into a directory that is not there")
	}
	if err := replaceFile(dir, []byte("<svg/>")); err == nil {
		t.Error("replaceFile replaced a directory with a file")
	}
}

// TestReplaceFileReadsThePathAsWritten writes through a `..` that follows a
// directory which is not there. Read as written the path is the card beside
// that directory, which is where the card lands; asked of the system as it
// stands, the path fails on Linux and macOS, where every step has to exist,
// and succeeds on Windows, which resolves `..` before it looks.
func TestReplaceFileReadsThePathAsWritten(t *testing.T) {
	dir := t.TempDir()
	sep := string(filepath.Separator)
	// Joined by hand: filepath.Join would read the `..` away before the
	// command saw it.
	given := dir + sep + "absent" + sep + ".." + sep + "card.svg"
	if err := replaceFile(given, []byte("<svg/>")); err != nil {
		t.Fatalf("replaceFile(%q) = %v, want the card written beside absent", given, err)
	}
	card := filepath.Join(dir, "card.svg")
	if body, err := os.ReadFile(card); err != nil || string(body) != "<svg/>" {
		t.Errorf("%s holds %q, %v, want the card", card, body, err)
	}
	if _, err := os.Stat(card + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temporary file was left beside the card: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Errorf("writing the card made the directory the path steps out of: %v", err)
	}
}

// notFoundText is how this platform words the failure to reach path, which
// the command passes on as it is: "no such file or directory" on Unix, and on
// Windows the system's own sentence, which for a file whose directory is
// missing is not the one for a missing file. So it is read off the same
// failure here rather than written out in Linux's words.
func notFoundText(t *testing.T, path string) string {
	t.Helper()
	_, err := os.Stat(path)
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("stat %s = %v, want it missing", path, err)
	}
	return pathErr.Err.Error()
}

// TestMainRunsTheProcessCommandLine is main itself: the process's arguments
// and standard output, handed to execute.
func TestMainRunsTheProcessCommandLine(t *testing.T) {
	args, stdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = args, stdout })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args, os.Stdout = []string{"ghchronicle", "-version"}, w

	main()

	os.Stdout = stdout
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	printed, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(printed) != buildLine()+"\n" {
		t.Errorf("main printed %q, want the build line", printed)
	}
}
