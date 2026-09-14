// Package e2e drives the built binary against a fake GitHub.
//
// The unit tests in internal/ prove each collector against its own fixtures.
// These prove the wiring: the config file reaches the client, every family is
// scheduled, the sink writes what the collectors produced, and the state file
// records the sweep.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
	"github.com/jmrplens/ghchronicle/test/e2e/racereport"
)

// binary is the path of the ghchronicle built by TestMain.
var binary string

func TestMain(m *testing.M) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: go not on PATH, nothing to build")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "ghchronicle-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "ghchronicle"+exeSuffix)
	// The arguments and the bound come from the race seam, so a `go test -race`
	// run builds an instrumented collector rather than driving an
	// uninstrumented one (harness_race_test.go). TestMain has no test to take a
	// context from, so the bound is the seam's own rather than a test's.
	ctx, cancel := context.WithTimeout(context.Background(), collectorBuildTimeout)
	build := exec.CommandContext(ctx, goTool, collectorBuildArgs(binary)...)
	build.Dir = moduleRoot()
	out, buildErr := build.CombinedOutput()
	cancel()
	if buildErr != nil {
		fmt.Fprintf(os.Stderr, "e2e: go build failed: %v\n%s", buildErr, out)
		removeBuildDir(dir)
		os.Exit(1)
	}
	code := m.Run()
	removeBuildDir(dir)
	os.Exit(code)
}

// removeBuildDir takes the built binary away again, and says so when it
// cannot: a removal that fails quietly leaves one binary per run behind.
func removeBuildDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %s was left behind: %v\n", dir, err)
	}
}

// moduleRoot is two directories up from test/e2e.
func moduleRoot() string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic(err)
	}
	return root
}

// families is every collector family the suites switch on and
// expectedMeasurements at least one measurement per family, both from
// test/e2e/fakegh so that this suite and the containerized one cannot come to
// disagree about which families exist.
var (
	families             = fakegh.Families()
	expectedMeasurements = fakegh.Measurements
)

// point is one line of the file sink's JSON format.
type point struct {
	Time        string            `json:"time"`
	Measurement string            `json:"measurement"`
	Tags        map[string]string `json:"tags"`
	Fields      map[string]any    `json:"fields"`
}

// writeConfig writes a config that points the binary at base and every
// family at a one minute cadence, so a fresh state file runs them all.
func writeConfig(t *testing.T, dir, base, token, user, extra string) string {
	t.Helper()
	return writeConfigWithCadence(t, dir, base, token, user, "1m", extra)
}

// writeConfigWithCadence is writeConfig with every family at the given
// cadence, for a test that needs a second sweep to fall due while it watches.
func writeConfigWithCadence(t *testing.T, dir, base, token, user, cadence, extra string) string {
	t.Helper()
	var every strings.Builder
	for _, f := range families {
		fmt.Fprintf(&every, "    %s: %s\n", f, cadence)
	}
	cfg := fmt.Sprintf(`github:
  token: %s
  base_url: %s
  timeout: 20s
targets:
  user: %s
sinks:
  file:
    path: %s
    format: json
every:
  families:
%s
state_file: %s
log:
  level: debug
%s`, token, base, user, filepath.Join(dir, "points.jsonl"), every.String(), filepath.Join(dir, "state.json"), extra)
	return writeConfigFile(t, dir, cfg)
}

// writeConfigFile puts body in dir under the one name the tests use. The
// callers differ in what goes in the file, never in where it lands.
func writeConfigFile(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// run executes the binary and returns its combined output. A race report in
// that output fails the test here, whatever the caller goes on to assert.
func run(t *testing.T, timeout time.Duration, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = collectorEnviron()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	racereport.Check(t, out.String())
	return out.Bytes(), err
}

func readPoints(t *testing.T, path string) []point {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the file sink wrote nothing: %v", err)
	}
	defer f.Close()
	var points []point
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var p point
		if decodeErr := json.Unmarshal(line, &p); decodeErr != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", len(points)+1, decodeErr, line)
		}
		points = append(points, p)
	}
	if scanErr := sc.Err(); scanErr != nil {
		t.Fatal(scanErr)
	}
	return points
}

type state struct {
	LastRun  map[string]time.Time `json:"last_run"`
	FirstSaw map[string]time.Time `json:"first_saw"`
}

func readState(t *testing.T, path string) state {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var s state
	if decodeErr := json.Unmarshal(b, &s); decodeErr != nil {
		t.Fatalf("state file is not JSON: %v\n%s", decodeErr, b)
	}
	return s
}

func TestOnceAgainstFakeGitHub(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "e2e-token", login, "")

	out, err := run(t, 2*time.Minute, "-config", cfg, "-once")
	if err != nil {
		t.Fatalf("ghchronicle -once failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("collector failed")) || bytes.Contains(out, []byte("sink write failed")) {
		t.Errorf("the sweep logged a failure:\n%s", out)
	}

	assertRequestsCarriedTheToken(t, gh.Requests())

	points := readPoints(t, filepath.Join(dir, "points.jsonl"))
	if len(points) < 100 {
		t.Fatalf("only %d points written", len(points))
	}
	assertEveryFamilyIsRepresented(t, points)
	assertTheForkWasFiltered(t, points)
	assertDatingRulesSurvived(t, points)
	assertAchievementProgressAgreesWithThePage(t, points, out)
	assertStateRecordsTheSweep(t, readState(t, filepath.Join(dir, "state.json")))

	// A second sweep straight after is an increment: nothing is due at a
	// one minute cadence, so nothing new is written and nothing fails.
	before := len(points)
	if out, err = run(t, time.Minute, "-config", cfg, "-once"); err != nil {
		t.Fatalf("second -once failed: %v\n%s", err, out)
	}
	if after := len(readPoints(t, filepath.Join(dir, "points.jsonl"))); after != before {
		t.Errorf("a sweep with nothing due wrote %d more points", after-before)
	}
}

// TestTheSecondSweepIsPricedByTheCache is the companion of the family
// coverage assertion above: that one proves every family wrote something,
// this one proves what a second sweep of the same process pays for it.
//
// The fake answers a repeat that presents a fixture's validator with a 304 and
// charges nothing for it, so a repeated URL that arrives without the
// validator, or is answered 200 with it, is a request the cache paid for
// twice. The URLs a second sweep is charged for are exactly the ones it did
// not ask for the first time: the moving windows in a query string, and the
// GraphQL queries, which carry no validator and are priced by the block every
// answer now carries. Both figures are read from the answers, never by
// subtracting two readings, which is the rule the client itself follows.
func TestTheSecondSweepIsPricedByTheCache(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	// Cadences shorter than the tick, so every tick is a sweep with every
	// family due, in the one process whose cache the first sweep filled:
	// -once twice would be two processes and two empty caches.
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, "1s", "heartbeat: 2s\n")

	p := serveInBackground(t, cfg)
	finished := func(n int) func() bool {
		return func() bool { return strings.Count(p.Output(), "sweep finished") >= n || p.Exited() }
	}
	// The fake's log is cut where each sweep ended: nothing is asked between
	// one sweep's last answer and the next tick, so the count at "sweep
	// finished" is the boundary.
	if !waitFor(time.Minute, finished(1)) || p.Exited() {
		t.Fatalf("the first sweep never finished:\n%s", p.Output())
	}
	afterFirst := len(gh.Requests())
	if !waitFor(time.Minute, finished(2)) || p.Exited() {
		t.Fatalf("the second sweep never finished:\n%s", p.Output())
	}
	afterSecond := len(gh.Requests())
	p.Stop()

	all := gh.Requests()
	first, second := all[:afterFirst], all[afterFirst:afterSecond]
	if len(second) == 0 {
		t.Fatalf("the second sweep asked the fake nothing; the first asked %d", len(first))
	}
	assertRepeatsWereRevalidated(t, first, second)
	core1, gql1 := sweepCost(first)
	core2, gql2 := sweepCost(second)
	t.Logf("first sweep: %d core, %d graphql; second sweep: %d core, %d graphql", core1, gql1, core2, gql2)
	if core2*2 > core1 {
		t.Errorf("the second sweep charged %d core requests against the first sweep's %d: fewer than half of the first sweep's URLs came back 304", core2, core1)
	}
	assertOwnSpendWasPriced(t, readPoints(t, filepath.Join(dir, "points.jsonl")))
}

// assertRepeatsWereRevalidated: every GET the first sweep was answered 200
// for came back 304 when the second sweep asked again, and some did. A 404
// carries no validator, so a feature this account has switched off is
// charged every time it is asked about, exactly as api.github.com charges
// it, and is left out. So is everything off the API: the job log, read as
// text from object storage, and the profile page the achievements family
// reads anonymously, neither validated nor cached.
func assertRepeatsWereRevalidated(t *testing.T, first, second []recorded) {
	t.Helper()
	answered := map[string]int{}
	for _, r := range first {
		answered[r.Method+" "+r.Path+"?"+r.Query] = r.Status
	}
	var free, repeats int
	charged := map[string]int{}
	for _, r := range second {
		url := r.Method + " " + r.Path + "?" + r.Query
		if r.Method == http.MethodGet && answered[url] == http.StatusOK && !fakegh.OffAPI(r.Path) {
			repeats++
			if r.Status != http.StatusNotModified {
				t.Errorf("%s was answered 200 in the first sweep and %d in the second: the client did not present the validator it stored", url, r.Status)
			}
		}
		if r.Cost == 0 {
			free++
			continue
		}
		charged[r.Method+" "+r.Path]++
	}
	if repeats == 0 || free == 0 {
		t.Fatalf("the second sweep repeated %d URLs and %d answers were free; the first sweep asked %d, the second %d", repeats, free, len(first), len(second))
	}
	for path, n := range charged {
		t.Logf("charged in the second sweep: %d x %s", n, path)
	}
}

// sweepCost is what a sweep's answers said they charged, per bucket the
// suites care about.
func sweepCost(reqs []recorded) (core, graphql int) {
	for _, r := range reqs {
		if r.Path == "/graphql" {
			graphql += r.Cost
		} else {
			core += r.Cost
		}
	}
	return core, graphql
}

// assertOwnSpendWasPriced: the GraphQL spend the ratelimit family reports is
// this process's own, priced by the block each answer carried. With every
// query priced at one it is the number of charged queries made before the
// family ran, and it cannot exceed what the fake says the whole token spent.
func assertOwnSpendWasPriced(t *testing.T, points []point) {
	t.Helper()
	var own int
	for _, pt := range points {
		if pt.Measurement != "gh_rate_limit" || pt.Tags["resource"] != "graphql" {
			continue
		}
		own++
		queries, _ := pt.Fields["own_queries"].(float64)
		spent, _ := pt.Fields["own_cost"].(float64)
		if queries == 0 || spent != queries {
			t.Errorf("gh_rate_limit graphql at %s reports own_cost=%v own_queries=%v, want both equal and above zero", pt.Time, spent, queries)
		}
		if used, _ := pt.Fields["used"].(float64); used < spent {
			t.Errorf("gh_rate_limit graphql reports used=%v below this process's own spend of %v", used, spent)
		}
	}
	if own == 0 {
		t.Fatal("no gh_rate_limit point for graphql was written")
	}
}

// assertRequestsCarriedTheToken is the config reaching the client: every
// request went to the fake with the configured token, and nothing went
// anywhere else.
func assertRequestsCarriedTheToken(t *testing.T, reqs []recorded) {
	t.Helper()
	if len(reqs) < 30 {
		t.Errorf("only %d requests reached the fake GitHub", len(reqs))
	}
	for _, r := range reqs {
		if r.Auth != "Bearer e2e-token" && !fakegh.OffAPI(r.Path) {
			t.Errorf("%s %s carried Authorization %q, want the configured token", r.Method, r.Path, r.Auth)
		}
		if fakegh.OffAPI(r.Path) && r.Auth != "" {
			t.Errorf("%s is off the API and carried the token", r.Path)
		}
	}
}

// assertEveryFamilyIsRepresented checks each point over, and then that every
// family this fixture set has an answer for left at least one measurement
// behind, so an unscheduled family, or one whose routes the fake does not
// answer, cannot hide behind the others.
func assertEveryFamilyIsRepresented(t *testing.T, points []point) {
	t.Helper()
	byName := map[string]int{}
	for i, p := range points {
		byName[p.Measurement]++
		assertPointIsWellFormed(t, i, p)
	}
	for _, family := range fakegh.Answering() {
		if m := expectedMeasurements[family]; byName[m] == 0 {
			t.Errorf("family %s produced no %s points; measurements seen: %v", family, m, keys(byName))
		}
	}
	// Two-sided, so a family that starts answering stops being excused rather
	// than staying quietly on the list.
	for family, why := range fakegh.Unanswered {
		if m := expectedMeasurements[family]; byName[m] > 0 {
			t.Errorf("family %s is listed as answering nothing (%s) and wrote %d %s points",
				family, why, byName[m], m)
		}
	}
}

// assertPointIsWellFormed checks what every point has to carry whichever
// collector produced it.
func assertPointIsWellFormed(t *testing.T, i int, p point) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339Nano, p.Time); err != nil {
		t.Errorf("point %d (%s): time %q does not parse: %v", i, p.Measurement, p.Time, err)
	}
	if !strings.HasPrefix(p.Measurement, "gh_") {
		t.Errorf("point %d: measurement %q", i, p.Measurement)
	}
	if len(p.Fields) == 0 {
		t.Errorf("point %d (%s) has no field", i, p.Measurement)
	}
	for k, v := range p.Tags {
		if v == "" {
			t.Errorf("point %d (%s): tag %q is empty", i, p.Measurement, k)
		}
	}
}

// assertAchievementProgressAgreesWithThePage: the progress half of the
// achievements family wrote its row per tiered badge, and the fixtures are
// consistent with the profile page they sit beside, so every row agrees with
// the page and the sweep had no disagreement to report. The page shows a
// gold Starstruck and a gold Pair Extraordinaire and neither Pull Shark nor
// Galaxy Brain, and the counts say 4,321 stars on the top repository, 48
// co-authored pull requests, one merged pull request and one accepted
// answer, so a rule that drifted from the page would fail here.
func assertAchievementProgressAgreesWithThePage(t *testing.T, points []point, out []byte) {
	t.Helper()
	rows := map[string]point{}
	for _, p := range points {
		if p.Measurement == "gh_achievement_progress" {
			rows[p.Tags["achievement"]] = p
		}
	}
	if len(rows) != 4 {
		t.Errorf("gh_achievement_progress has %d rows, want one per tiered badge: %v", len(rows), slices.Sorted(maps.Keys(rows)))
	}
	for slug, want := range map[string]float64{"pull-shark": 0, "starstruck": 4, "pair-extraordinaire": 4, "galaxy-brain": 0} {
		p, ok := rows[slug]
		if !ok {
			continue
		}
		if p.Fields["tier_number"] != want || p.Fields["page_tier"] != want || p.Fields["agrees"] != float64(1) {
			t.Errorf("%s progress = %v, want tier %v, the page's tier, and agreement", slug, p.Fields, want)
		}
		if !strings.HasSuffix(p.Time, "T00:00:00Z") {
			t.Errorf("%s progress stamped %s, want the start of the UTC day", slug, p.Time)
		}
	}
	if bytes.Contains(out, []byte("disagrees with the profile page")) {
		t.Errorf("the sweep reported a disagreement the fixtures do not have:\n%s", out)
	}
}

// assertTheForkWasFiltered: the fork in the repository list is left out by
// default, so nothing it owns should have reached a sink.
func assertTheForkWasFiltered(t *testing.T, points []point) {
	t.Helper()
	for _, p := range points {
		if p.Tags["repo"] == "linguist" || p.Tags["full_name"] == "octocat/linguist" {
			t.Errorf("a fork was collected: %+v", p)
			return
		}
	}
}

// assertDatingRulesSurvived spot checks the three datings that are decided in
// a collector and have to survive the whole pipeline unchanged.
func assertDatingRulesSurvived(t *testing.T, points []point) {
	t.Helper()
	for _, p := range points {
		switch p.Measurement {
		case "gh_traffic":
			if !strings.HasSuffix(p.Time, "T00:00:00Z") {
				t.Errorf("traffic day stamped %s, want GitHub's own midnight", p.Time)
			}
		case "gh_commits_week":
			at, _ := time.Parse(time.RFC3339Nano, p.Time)
			if at.Weekday() != time.Sunday {
				t.Errorf("weekly row stamped on a %s", at.Weekday())
			}
		case "gh_job_log":
			if s, _ := p.Fields["line"].(string); strings.ContainsRune(s, 0x1b) {
				t.Errorf("job log line kept an escape code: %q", s)
			}
		}
	}
}

// assertStateRecordsTheSweep: the state file remembers when each family last
// ran and when the repository was first seen, which is what makes the next
// sweep an increment.
func assertStateRecordsTheSweep(t *testing.T, st state) {
	t.Helper()
	for _, f := range families {
		if _, ok := st.LastRun[f]; !ok {
			t.Errorf("state has no last_run for %s: %v", f, keys2(st.LastRun))
		}
	}
	if _, ok := st.FirstSaw["octocat/hello-world"]; !ok {
		t.Errorf("state does not remember first sight of the repository: %v", st.FirstSaw)
	}
}

func TestListPrintsTheDiscoveredRepositories(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "e2e-token", login, "")
	// Split rather than combined: the log goes to standard error, and this
	// harness runs every family at a minute, which the config package now
	// warns about family by family.
	out, logs, err := runSplit(t, time.Minute, "-config", cfg, "-list")
	if err != nil {
		t.Fatalf("-list failed: %v\n%s\n%s", err, out, logs)
	}
	if got := strings.TrimSpace(out); got != "octocat/hello-world" {
		t.Errorf("-list printed %q, want the one non-fork repository", got)
	}
}

func TestCardOnlyWritesValidSVG(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeConfig(t, dir, gh.URL(), "e2e-token", login, "")
	card := filepath.Join(dir, "card.svg")

	out, err := run(t, 2*time.Minute, "-config", cfg, "-card", card, "-card-only")
	if err != nil {
		t.Fatalf("-card-only failed: %v\n%s", err, out)
	}
	svg, err := os.ReadFile(card)
	if err != nil {
		t.Fatalf("card not written: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(card + ".tmp"); statErr == nil {
		t.Error("the temporary file was left behind")
	}
	// Card only: the file sink must not have been written.
	if _, statErr := os.Stat(filepath.Join(dir, "points.jsonl")); statErr == nil {
		t.Error("-card-only wrote to the file sink")
	}

	dec := xml.NewDecoder(bytes.NewReader(svg))
	var root string
	var elements int
	for {
		tok, tokenErr := dec.Token()
		if tokenErr != nil {
			if errors.Is(tokenErr, io.EOF) {
				break
			}
			t.Fatalf("card is not well-formed XML: %v\n%s", tokenErr, truncate(svg))
		}
		if se, ok := tok.(xml.StartElement); ok {
			elements++
			if root == "" {
				root = se.Name.Local
				if se.Name.Space != "http://www.w3.org/2000/svg" {
					t.Errorf("root namespace = %q", se.Name.Space)
				}
			}
		}
	}
	if root != "svg" {
		t.Errorf("root element = %q, want svg", root)
	}
	if elements < 5 {
		t.Errorf("card has only %d elements", elements)
	}
	if !bytes.Contains(svg, []byte(login)) {
		t.Errorf("card does not name the account:\n%s", truncate(svg))
	}
	// The numbers on the card come from the same points the sinks get.
	if !bytes.Contains(svg, []byte("1,200")) && !bytes.Contains(svg, []byte("1.2k")) && !bytes.Contains(svg, []byte("1200")) {
		t.Errorf("the follower count from the account fixture is not on the card:\n%s", truncate(svg))
	}
}

// TestLiveAPI runs the binary against api.github.com. It is gated so it
// never runs in CI by accident: set GHC_E2E_LIVE=1 and GITHUB_TOKEN, and
// optionally GHC_E2E_USER for an account other than the module's owner.
func TestLiveAPI(t *testing.T) {
	user := requireLiveAPI(t)
	dir := t.TempDir()
	// The default cadences and families: history and job logs stay off, so a
	// live run costs what a normal first sweep costs.
	body := fmt.Sprintf(`github:
  token: ${GITHUB_TOKEN}
  timeout: 60s
targets:
  user: %s
sinks:
  file:
    path: %s
    format: json
state_file: %s
`, user, filepath.Join(dir, "points.jsonl"), filepath.Join(dir, "state.json"))
	cfg := writeConfigFile(t, dir, body)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-config", cfg, "-once")
	// The whole environment, token included, since the config asks for it by
	// name; only the detector's settings are added.
	cmd.Env = append(os.Environ(), raceEnviron()...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	racereport.Check(t, out.String())
	if err != nil {
		t.Fatalf("live run failed: %v\n%s", err, out.String())
	}
	points := readPoints(t, filepath.Join(dir, "points.jsonl"))
	if len(points) < 100 {
		t.Errorf("live run wrote %d points, want at least 100\n%s", len(points), out.String())
	}
}

// requireLiveAPI skips unless the live test is switched on and a token is
// there to run it with, and answers with the account to sweep. The token
// itself stays in the environment: the config asks for it by name.
func requireLiveAPI(t *testing.T) string {
	t.Helper()
	if os.Getenv("GHC_E2E_LIVE") != "1" {
		t.Skip("set GHC_E2E_LIVE=1 to run against the real API")
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		t.Skip("GITHUB_TOKEN is not set")
	}
	if user := os.Getenv("GHC_E2E_USER"); user != "" {
		return user
	}
	return "jmrplens"
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keys2(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncate(b []byte) string {
	if len(b) > 2000 {
		return string(b[:2000]) + "..."
	}
	return string(b)
}
