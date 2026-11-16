package collect

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// reportedServer answers every source of Reported, and every candidate's
// advisory list with the same synthetic fixture unless a test overrides one.
func reportedServer(t *testing.T, candidates ...string) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.file("/notifications", "reported_notifications.json")
	f.file("/advisories", "global_advisories.json")
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		switch {
		case strings.Contains(query, "repositoriesContributedTo"):
			f.write(w, "reported_contributed.json")
		case strings.Contains(query, "search("):
			f.write(w, "reported_search.json")
		default:
			t.Errorf("unexpected query %s", query)
		}
	})
	for _, c := range candidates {
		serveAdvisoriesOf(t, f, c, c)
	}
	return f
}

// serveAdvisoriesOf answers asked's advisory list with the synthetic
// fixture, its pages on home and its GHSA ids made home's own, so two
// repositories never share an advisory unless a test means them to.
func serveAdvisoriesOf(t *testing.T, f *fixtureServer, asked, home string) {
	t.Helper()
	body := strings.ReplaceAll(string(fixture(t, "reported_repo_advisories.json")), "x/y", home)
	body = strings.ReplaceAll(body, "-aaaa-", "-"+strings.Repeat(home[:1], 4)+"-")
	f.handle("/repos/"+asked+"/security-advisories", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

func reportedOf() Reported {
	return Reported{Login: "octocat", Own: []string{"octocat", "some-org"}}
}

func runReported(t *testing.T, f *fixtureServer, r Reported) ReportedRun {
	t.Helper()
	run, err := r.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, run.Points)
	return run
}

func reposOf(points []sink.Point) []string {
	var out []string
	for _, p := range points {
		if !slices.Contains(out, p.Tags["full_name"]) {
			out = append(out, p.Tags["full_name"])
		}
	}
	slices.Sort(out)
	return out
}

func TestReportedFindsTheRepositoriesWithoutAList(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited")
	run := runReported(t, f, reportedOf())

	want := []string{"a/notified", "b/searched", "c/contributed", "d/credited"}
	if got := reposOf(run.Points); !slices.Equal(got, want) {
		t.Fatalf("advisories found in %v, want one from each source %v", got, want)
	}
	if got := slices.Sorted(slices.Values(run.Remember)); !slices.Equal(got, want) {
		t.Errorf("remember %v, want every repository that held one %v", got, want)
	}
	if !run.Scanned || !run.InboxRead {
		t.Errorf("scanned %v, inbox read %v: both walks reached their end", run.Scanned, run.InboxRead)
	}
	// e/someone-elses credits someone else, and z/pulls-only notified about a
	// pull request: neither is a candidate.
	for _, p := range []string{"/repos/e/someone-elses/security-advisories", "/repos/z/pulls-only/security-advisories"} {
		if n := len(f.calls(p)); n != 0 {
			t.Errorf("%s asked %d times", p, n)
		}
	}
	if calls := f.calls("/notifications"); calls[0].Query["all"] != "true" {
		t.Errorf("the inbox is read without all=true, losing every thread already read: %v", calls[0].Query)
	}
}

func TestReportedMatchesTheLoginInAnyRole(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited")
	run := runReported(t, f, reportedOf())
	rows := byMeasurement(run.Points)["gh_advisory_reported"]
	if len(rows) != 12 {
		t.Fatalf("got %d rows, want three of four advisories in each of four repositories", len(rows))
	}
	tags := map[string]string{"full_name": "b/searched"}
	get := func(ghsa string) sink.Point {
		tags["ghsa"] = ghsa
		return find(t, run.Points, "gh_advisory_reported", tags)
	}
	authored, credited, collaborating := get("GHSA-mine-bbbb-0001"), get("GHSA-mine-bbbb-0002"), get("GHSA-mine-bbbb-0003")
	if authored.Fields["authored"] != 1 || authored.Fields["credit"] != "reporter" || authored.Fields["credit_state"] != "accepted" {
		t.Errorf("authored row: %v", authored.Fields)
	}
	if credited.Fields["authored"] != 0 || credited.Fields["credit"] != "analyst,finder" {
		t.Errorf("credited row: %v", credited.Fields)
	}
	if collaborating.Fields["credit"] != noneTag || collaborating.Fields["authored"] != 0 {
		t.Errorf("collaborator row: %v", collaborating.Fields)
	}
	if credited.Tags["user"] != "octocat" || credited.Tags["owner"] != "b" || credited.Tags["repo"] != "searched" {
		t.Errorf("tags %v", credited.Tags)
	}
	if got, want := credited.Time, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("published row dated %v, want %v", got, want)
	}
}

func TestReportedLeavesTheAccountsOwnRepositories(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited")
	runReported(t, f, reportedOf())
	for _, p := range []string{"/repos/octocat/hello-world/security-advisories", "/repos/Some-Org/x/security-advisories"} {
		if n := len(f.calls(p)); n != 0 {
			t.Errorf("%s asked %d times: the advisories family reads it as received", p, n)
		}
	}
}

func TestReportedSkipsARepositoryThatRefuses(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "b/searched", "c/contributed", "d/credited")
	f.status("/repos/a/notified/security-advisories", http.StatusNotFound, "Not Found")
	run := runReported(t, f, reportedOf())
	if got := reposOf(run.Points); !slices.Equal(got, []string{"b/searched", "c/contributed", "d/credited"}) {
		t.Fatalf("found in %v", got)
	}
	// The inbox named it, so it is kept for when it answers again.
	if !slices.Contains(run.Remember, "a/notified") {
		t.Errorf("remember %v lost a repository the inbox named", run.Remember)
	}
}

func TestReportedScanCutShortIsNotMarked(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed")
	page := repeat(t, "global_advisories.json", "", 100, func(_ int, row map[string]any) {
		row["credits"] = []any{}
	})
	f.handle("/advisories", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			w.Header().Set("Link", `<https://api.github.com/advisories?after=bmV4dA&per_page=100>; rel="next"`)
			_, _ = w.Write(page)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	run, err := reportedOf().Collect(ctx(t), f.Client, testNow)
	if err == nil || !strings.Contains(err.Error(), "global advisories") {
		t.Fatalf("err %v, want the global walk's failure", err)
	}
	if run.Scanned {
		t.Error("a walk cut short is marked scanned, so what it did not read is never read")
	}
	if got := reposOf(run.Points); len(got) != 3 {
		t.Errorf("the other sources' findings were lost with the failure: %v", got)
	}
}

func TestReportedGlobalWalkStopsAtScanned(t *testing.T) {
	t.Parallel()
	f := reportedServer(t)
	page := repeat(t, "global_advisories.json", "", 100, func(_ int, row map[string]any) {
		row["credits"] = []any{}
	})
	f.handle("/advisories", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/advisories?after=bmV4dA&per_page=100>; rel="next"`)
		_, _ = w.Write(page)
	})
	r := reportedOf()
	r.Scanned = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r.Notified = time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	run, _ := r.Collect(ctx(t), f.Client, testNow)
	if n := len(f.calls("/advisories")); n != 1 || !run.Scanned {
		t.Errorf("%d pages read and scanned %v, want one page reaching the bound", n, run.Scanned)
	}
	if got := f.calls("/notifications")[0].Query["since"]; got != "2026-09-07T13:00:00Z" {
		t.Errorf("inbox since %q", got)
	}
}

func TestReportedReadsEachCandidateOnce(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited")
	r := reportedOf()
	r.Known = []string{"B/Searched", "d/credited"}
	runReported(t, f, r)
	for _, p := range []string{"/repos/b/searched/security-advisories", "/repos/d/credited/security-advisories"} {
		if n := len(f.calls(p)); n != 1 {
			t.Errorf("%s asked %d times", p, n)
		}
	}
	if n := len(f.calls("/repos/B/Searched/security-advisories")); n != 0 {
		t.Errorf("a second spelling of one repository was asked as another")
	}
}

func TestReportedHidesUnpublishedDetailsByDefault(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited")
	run := runReported(t, f, reportedOf())
	tags := map[string]string{"full_name": "b/searched", "ghsa": "GHSA-mine-bbbb-0001"}
	if v := find(t, run.Points, "gh_advisory_reported", tags).Fields["summary"]; v != withheldSummary {
		t.Errorf("a report in triage carries its summary %q without being asked", v)
	}
	r := reportedOf()
	r.Details = true
	run = runReported(t, reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited"), r)
	if v := find(t, run.Points, "gh_advisory_reported", tags).Fields["summary"]; v == withheldSummary {
		t.Error("asked for every detail, a report in triage still lacks its summary")
	}
}

// TestReportedWritesAMovedRepositoryOnce: a repository remembered under the
// name it had and found again under the one it has now answers both, the old
// one through GitHub's redirect, and each advisory is one point named by its
// own page, whichever name came first.
func TestReportedWritesAMovedRepositoryOnce(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "c/contributed", "d/credited")
	serveAdvisoriesOf(t, f, "b/searched", "b/searched")
	serveAdvisoriesOf(t, f, "b/old-name", "b/searched")
	r := reportedOf()
	r.Known = []string{"b/old-name"}
	run := runReported(t, f, r)
	n := 0
	for _, p := range run.Points {
		if strings.HasPrefix(p.Tags["ghsa"], "GHSA-mine-bbbb-") {
			n++
			if p.Tags["full_name"] != "b/searched" {
				t.Errorf("%s tagged %s, want the name its page carries", p.Tags["ghsa"], p.Tags["full_name"])
			}
		}
	}
	if n != 3 {
		t.Errorf("%d points for the moved repository's three advisories", n)
	}
	if !slices.Contains(run.Remember, "b/searched") {
		t.Errorf("remember %v lacks the repository's current name", run.Remember)
	}
}

// TestReportedLeavesTheCollectedRepositories: a repository of another owner
// named in targets.repos is read by the advisories family, so a report there
// is received, not reported.
func TestReportedLeavesTheCollectedRepositories(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed", "d/credited")
	r := reportedOf()
	r.Collected = []string{"B/Searched"}
	run := runReported(t, f, r)
	if n := len(f.calls("/repos/b/searched/security-advisories")); n != 0 {
		t.Errorf("a collected repository asked %d times", n)
	}
	if got := reposOf(run.Points); slices.Contains(got, "b/searched") {
		t.Errorf("found in %v", got)
	}
}

// TestReportedReadsNoGlobalDatabaseAsNothingThere: a server without one, as
// GitHub Enterprise Server may be, is not a failure every hour.
func TestReportedReadsNoGlobalDatabaseAsNothingThere(t *testing.T) {
	t.Parallel()
	f := reportedServer(t, "a/notified", "b/searched", "c/contributed")
	f.status("/advisories", http.StatusNotFound, "Not Found")
	run := runReported(t, f, reportedOf())
	if !run.Scanned {
		t.Error("an absent database is not marked read")
	}
}
