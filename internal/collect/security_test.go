package collect

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

func TestSecurityAlertsOfEveryState(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/dependabot/alerts", "dependabot_alerts.json")
	f.file("/repos/octocat/hello-world/code-scanning/alerts", "code_scanning_alerts.json")

	points, err := Security{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_dependabot_alert_item", "gh_dependabot_alert", "gh_security_feature",
		"gh_code_scanning_alert", "gh_code_scanning_alert_item")

	checkAlertListsAreNotFilteredByState(t, f)
	checkDependabotAlertItems(t, points)
	checkClosedDependabotAlerts(t, points)
	checkDependabotGauges(t, points)
	checkCodeScanningGauges(t, points)
	checkCodeScanningAlertItems(t, points)
}

// checkAlertListsAreNotFilteredByState reads the requests: a fixed alert is
// the only record of how long the fix took, and state=open threw it away.
func checkAlertListsAreNotFilteredByState(t *testing.T, f *fixtureServer) {
	t.Helper()
	calls := f.calls("/repos/octocat/hello-world/dependabot/alerts")
	if len(calls) != 1 {
		t.Fatalf("%d dependabot calls", len(calls))
	}
	if _, filtered := calls[0].Query["state"]; filtered {
		t.Errorf("the dependabot list must not filter by state, got query %v", calls[0].Query)
	}
	scanCalls := f.calls("/repos/octocat/hello-world/code-scanning/alerts")
	if _, filtered := scanCalls[0].Query["state"]; filtered {
		t.Errorf("the code scanning list must not filter by state, got query %v", scanCalls[0].Query)
	}
}

// checkDependabotAlertItems reads the row of an alert that is still open:
// every field the listing carries and the collector used to drop.
func checkDependabotAlertItems(t *testing.T, points []sink.Point) {
	t.Helper()
	items := only(t, points, "gh_dependabot_alert_item")
	if len(items) != 4 {
		t.Fatalf("got %d alert items, want one per alert in every state", len(items))
	}
	open := find(t, points, "gh_dependabot_alert_item", map[string]string{"number": "3"})
	if open.Tags["severity"] != "high" || open.Tags["ecosystem"] != "go" || open.Tags["package"] != "golang.org/x/net" {
		t.Errorf("open alert tags = %v", open.Tags)
	}
	// The state moves after the date the row carries, so as a tag it opened
	// a second series at the same instant when the alert was fixed.
	if _, isTag := open.Tags["state"]; isTag {
		t.Error("the alert state must be a field, not a tag")
	}
	if hasField(open, "state") {
		t.Error("the demoted state must not reuse the old tag's column name")
	}
	if open.Fields["alert_state"] != "open" {
		t.Errorf("alert_state = %v, want open", open.Fields["alert_state"])
	}
	// An open alert carries no age at all. The row is dated when the alert
	// was raised, so how long it has been open is now() less that date and
	// belongs in the query; written here it would be a figure from the
	// sweep's clock sitting on a row dated months earlier, rewritten with a
	// new answer every sweep.
	if hasField(open, "seconds_to_resolve") || hasField(open, "seconds_open") {
		t.Errorf("an open alert carries neither age: %v", open.Fields)
	}
	if open.Fields["cvss"] != 7.5 || open.Fields["cvss_v4"] != 6.6 {
		t.Errorf("cvss = %v, cvss_v4 = %v", open.Fields["cvss"], open.Fields["cvss_v4"])
	}
	// The listing already carried all of this and the collector dropped it.
	if open.Tags["ghsa"] != "GHSA-aaaa-bbbb-cccc" || open.Tags["scope"] != "runtime" ||
		open.Tags["relationship"] != "direct" || open.Tags["manifest"] != "go.mod" {
		t.Errorf("open alert tags = %v", open.Tags)
	}
	checkAlertAdvisoryText(t, open)
	// Every CWE, not just the first: an advisory names more than one.
	if open.Fields["cwe"] != "CWE-400,CWE-770" {
		t.Errorf("cwe = %v", open.Fields["cwe"])
	}
	// created_at minus the advisory's published_at: how long GitHub took to
	// raise it here.
	if fieldInt(t, open, "seconds_to_detect") != 2*86400 {
		t.Errorf("seconds_to_detect = %v, want two days", open.Fields["seconds_to_detect"])
	}
	if want := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC); !open.Time.Equal(want) {
		t.Errorf("alert stamped %s, want created_at %s", open.Time, want)
	}
}

// checkAlertAdvisoryText reads what the row says beyond a package name and a
// GHSA id: the advisory's title and the range that first_patched is the way
// out of. Fields: a summary is prose and a range is one per advisory.
func checkAlertAdvisoryText(t *testing.T, open sink.Point) {
	t.Helper()
	if open.Fields["cve"] != "CVE-2026-11111" || open.Fields["first_patched"] != "0.7.5" {
		t.Errorf("cve/first_patched = %v", open.Fields)
	}
	if open.Fields["epss"] != 0.00278 || open.Fields["epss_percentile"] != 0.20421 {
		t.Errorf("epss = %v, percentile = %v", open.Fields["epss"], open.Fields["epss_percentile"])
	}
	if open.Fields["summary"] != "golang.org/x/net vulnerable to excessive resource consumption" ||
		open.Fields["vulnerable_range"] != ">= 0.7.0, < 0.7.5" {
		t.Errorf("summary/vulnerable_range = %v", open.Fields)
	}
	if hasField(open, "dismissed_reason") || hasField(open, "dismissed_by") {
		t.Errorf("an open alert was not dismissed: %v", open.Fields)
	}
	for _, k := range []string{"summary", "vulnerable_range", "dismissed_reason"} {
		if _, isTag := open.Tags[k]; isTag {
			t.Errorf("%s must be a field, not a tag", k)
		}
	}
}

// checkClosedDependabotAlerts reads the three ways an alert closes. The third
// is the one that used to be missed: GitHub dismisses a development-dependency
// alert by itself and leaves both fixed_at and dismissed_at null.
func checkClosedDependabotAlerts(t *testing.T, points []sink.Point) {
	t.Helper()
	fixed := find(t, points, "gh_dependabot_alert_item", map[string]string{"number": "2"})
	if fieldInt(t, fixed, "seconds_to_resolve") != 2*86400 {
		t.Errorf("fixed alert seconds_to_resolve = %v, want 2 days from fixed_at", fixed.Fields["seconds_to_resolve"])
	}
	dismissed := find(t, points, "gh_dependabot_alert_item", map[string]string{"number": "1"})
	if fieldInt(t, dismissed, "seconds_to_resolve") != 10*86400 {
		t.Errorf("dismissed alert seconds_to_resolve = %v, want 10 days from dismissed_at", dismissed.Fields["seconds_to_resolve"])
	}
	// An alert GitHub dismissed by itself has fixed_at and dismissed_at null
	// and the date in auto_dismissed_at, so it used to be recorded open
	// forever. It is closed, and closed two days after it was raised.
	auto := find(t, points, "gh_dependabot_alert_item", map[string]string{"number": "151"})
	if auto.Fields["alert_state"] != "auto_dismissed" {
		t.Fatalf("auto dismissed alert = %v", auto.Fields)
	}
	if hasField(auto, "seconds_open") {
		t.Errorf("an auto dismissed alert is closed, not open: %v", auto.Fields)
	}
	if fieldInt(t, auto, "seconds_to_resolve") != 2*86400 {
		t.Errorf("auto dismissed seconds_to_resolve = %v, want 2 days from auto_dismissed_at", auto.Fields["seconds_to_resolve"])
	}
	// "unknown" is a value GitHub itself sends in `relationship`, measured on
	// 4 of the 119 alerts this account has today, so it reaches the tag as it
	// came. It is also why the fallback beside it is not spelled that way:
	// this row and one GitHub said nothing about would read identically. An advisory with neither a CVE, an EPSS score nor a v4 vector
	// leaves those fields out rather than writing a zero that would read as a
	// measurement; the fixture carries GitHub's real shape for the missing v4
	// score, which is 0.0 and not null.
	if dismissed.Tags["relationship"] != "unknown" {
		t.Errorf("relationship = %q, want GitHub's own word, not the fallback",
			dismissed.Tags["relationship"])
	}
	if hasField(fixed, "cve") || hasField(fixed, "epss") || hasField(fixed, "cvss_v4") ||
		hasField(fixed, "cwe") || hasField(fixed, "first_patched") {
		t.Errorf("an advisory that names none of these writes none of them: %v", fixed.Fields)
	}
	// Why, who and with what words, on the one alert a person dismissed.
	if dismissed.Fields["dismissed_reason"] != "tolerable_risk" || dismissed.Fields["dismissed_by"] != "octocat" ||
		dismissed.Fields["dismissed_comment"] != "Not reachable from our code." {
		t.Errorf("dismissal fields = %v", dismissed.Fields)
	}
	// An advisory published with a v4 vector only comes with cvss.score 0.0,
	// GitHub's shape for "no v3 vector", and the collector used to store the
	// zero: 78 of the 225 alerts on jmrp.io, and "worst CVSS 0" on any
	// severity group made of them. The v3 score gets the guard v4 had.
	if hasField(auto, "cvss") {
		t.Errorf("cvss 0.0 is not a score: %v", auto.Fields["cvss"])
	}
	if auto.Fields["cvss_v4"] != 6.6 {
		t.Errorf("cvss_v4 = %v", auto.Fields["cvss_v4"])
	}
}

// checkDependabotGauges reads the by-severity counts and the feature row,
// which between them say how many alerts are open and how many exist.
func checkDependabotGauges(t *testing.T, points []sink.Point) {
	t.Helper()
	// Only the open alerts count towards the by-severity gauge.
	gauges := only(t, points, "gh_dependabot_alert")
	if len(gauges) != 1 {
		t.Errorf("got %d severity gauges, want 1 (one open alert)", len(gauges))
	}
	if g := gauges[0]; g.Tags["severity"] != "high" || g.Tags["ecosystem"] != "go" || fieldInt(t, g, "open") != 1 {
		t.Errorf("gauge = %v %v", g.Tags, g.Fields)
	}
	dep := find(t, points, "gh_security_feature", map[string]string{"feature": "dependabot"})
	if dep.Fields["enabled"] != true || fieldInt(t, dep, "open_alerts") != 1 || fieldInt(t, dep, "alerts") != 4 {
		t.Errorf("dependabot feature = %v", dep.Fields)
	}
}

// checkCodeScanningGauges reads the code scanning counts, which fall back to
// the rule severity when the alert has no security severity.
func checkCodeScanningGauges(t *testing.T, points []sink.Point) {
	t.Helper()
	scan := only(t, points, "gh_code_scanning_alert")
	if len(scan) != 2 {
		t.Fatalf("got %d code scanning gauges, want 2", len(scan))
	}
	high := find(t, points, "gh_code_scanning_alert", map[string]string{"severity": "high"})
	if high.Tags["tool"] != "CodeQL" || fieldInt(t, high, "open") != 1 {
		t.Errorf("high = %v %v", high.Tags, high.Fields)
	}
	find(t, points, "gh_code_scanning_alert", map[string]string{"severity": "warning", "tool": "CodeQL"})
	cs := find(t, points, "gh_security_feature", map[string]string{"feature": "code_scanning"})
	if cs.Fields["enabled"] != true || fieldInt(t, cs, "open_alerts") != 2 || fieldInt(t, cs, "alerts") != 4 {
		t.Errorf("code scanning feature = %v", cs.Fields)
	}
}

// checkCodeScanningAlertItems reads the per-alert rows: where the alert is,
// why it closed, and how long it took.
func checkCodeScanningAlertItems(t *testing.T, points []sink.Point) {
	t.Helper()
	// One point per alert, dated when it was raised, out of the list that was
	// already being downloaded.
	items := only(t, points, "gh_code_scanning_alert_item")
	if len(items) != 4 {
		t.Fatalf("got %d code scanning items, want one per alert in every state", len(items))
	}
	scanFixed := find(t, points, "gh_code_scanning_alert_item", map[string]string{"number": "3"})
	if scanFixed.Tags["severity"] != "critical" || scanFixed.Tags["rule"] != "js/bad-code-sanitization" {
		t.Errorf("fixed code scanning alert tags = %v", scanFixed.Tags)
	}
	checkScanAlertDemotedTags(t, scanFixed)
	// Where the alert is, out of most_recent_instance, which the listing
	// already carried. refs/heads/ and refs/tags/ are trimmed the same way
	// the analyses are.
	if scanFixed.Tags["path"] != ".github/workflows/ci.yml" || scanFixed.Tags["ref"] != "main" ||
		scanFixed.Tags["category"] != "/language:javascript-typescript" {
		t.Errorf("location tags = %v", scanFixed.Tags)
	}
	if fieldInt(t, scanFixed, "line") != 574 || scanFixed.Fields["commit"] != "61197070feab094c59abe7e26034a96b7ea598cd" {
		t.Errorf("location fields = %v", scanFixed.Fields)
	}
	// Only the CWEs out of rule.tags; "security" is housekeeping.
	if scanFixed.Fields["cwe"] != "cwe-079,cwe-094,cwe-116" {
		t.Errorf("cwe = %v", scanFixed.Fields["cwe"])
	}
	find(t, points, "gh_code_scanning_alert_item", map[string]string{"number": "2", "ref": "v1.2.0"})
	unclassified := find(t, points, "gh_code_scanning_alert_item", map[string]string{"number": "4"})
	if hasField(unclassified, "cwe") {
		t.Errorf("a rule with no CWE tag writes no cwe field: %v", unclassified.Fields)
	}
	if fieldInt(t, scanFixed, "seconds_to_resolve") != 86400 {
		t.Errorf("seconds_to_resolve = %v, want one day", scanFixed.Fields["seconds_to_resolve"])
	}
	if want := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC); !scanFixed.Time.Equal(want) {
		t.Errorf("alert stamped %s, want created_at %s", scanFixed.Time, want)
	}
	scanDismissed := find(t, points, "gh_code_scanning_alert_item", map[string]string{"number": "2"})
	if scanDismissed.Fields["resolution"] != "false positive" || scanDismissed.Fields["alert_state"] != "dismissed" {
		t.Errorf("a dismissal keeps its reason: %v", scanDismissed.Fields)
	}
	scanOpen := find(t, points, "gh_code_scanning_alert_item", map[string]string{"number": "5"})
	// An open alert has no reason to close, and still carries the field, so
	// the column exists before any alert has closed and a query naming it
	// does not fail.
	if scanOpen.Fields["resolution"] != "open" || scanOpen.Fields["alert_state"] != "open" {
		t.Errorf("an open alert reads open: %v", scanOpen.Fields)
	}
	if hasField(scanOpen, "seconds_to_resolve") || hasField(scanOpen, "seconds_open") {
		t.Errorf("an open alert carries neither age: %v", scanOpen.Fields)
	}
}

// checkScanAlertDemotedTags reads the state and the reason of a fixed alert:
// both move after the date the row carries, so as tags they doubled the row
// the day the alert was fixed or dismissed, and both are fields under new
// names so a database holding the old columns keeps accepting writes.
func checkScanAlertDemotedTags(t *testing.T, scanFixed sink.Point) {
	t.Helper()
	for _, tag := range []string{"state", "reason"} {
		if _, isTag := scanFixed.Tags[tag]; isTag {
			t.Errorf("%s must be a field, not a tag", tag)
		}
		if hasField(scanFixed, tag) {
			t.Errorf("the demoted %s must not reuse the old tag's column name", tag)
		}
	}
	if scanFixed.Fields["alert_state"] != "fixed" || scanFixed.Fields["resolution"] != "fixed" {
		t.Errorf("fixed code scanning alert fields = %v", scanFixed.Fields)
	}
}

func TestSecurityDisabledFeaturesAreRecordedNotFailed(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/dependabot/alerts", 403, "Dependabot alerts are disabled for this repository.")
	f.status("/repos/octocat/hello-world/code-scanning/alerts", 404, "no analysis found")

	points, err := Security{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("a disabled feature must not fail the sweep: %v", err)
	}
	checkPoints(t, points)
	features := only(t, points, "gh_security_feature")
	if len(features) != 2 || len(points) != 2 {
		t.Errorf("got %v, want only the two feature points", measurements(points))
	}
	for _, p := range features {
		if p.Fields["enabled"] != false || fieldInt(t, p, "open_alerts") != 0 {
			t.Errorf("%s feature = %v", p.Tags["feature"], p.Fields)
		}
	}
	// The refusal itself is the answer. Asking a second time with per_page=1
	// learnt nothing, and it fired on every repository whose list came back
	// empty: measured on 2026-09-08 over the 18 repositories this account
	// sweeps, 12 for Dependabot and 12 for code scanning, 24 requests a sweep.
	for _, path := range []string{
		"/repos/octocat/hello-world/dependabot/alerts",
		"/repos/octocat/hello-world/code-scanning/alerts",
	} {
		if n := len(f.calls(path)); n != 1 {
			t.Errorf("%s was called %d times, want 1", path, n)
		}
	}
}

func TestSecurityRealFailureIsReturned(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/dependabot/alerts", 502, "Bad Gateway")
	if _, err := (Security{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
		t.Fatal("a 502 must be returned")
	}
}

func TestAnalyses(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/code-scanning/analyses", "code_scanning_analyses.json")

	points, err := Analyses{Walk: Walk{Pages: 3}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if n := len(f.calls("/repos/octocat/hello-world/code-scanning/analyses")); n != 1 {
		t.Errorf("made %d requests, a short page ends the walk", n)
	}
	analyses := only(t, points, "gh_code_scanning_analysis")
	if len(analyses) != 2 {
		t.Fatalf("got %d analyses, want 2", len(analyses))
	}
	main := find(t, points, "gh_code_scanning_analysis", map[string]string{"ref": "main"})
	if main.Tags["tool"] != "CodeQL" || main.Tags["version"] != "2.19.0" || main.Tags["category"] == "" {
		t.Errorf("analysis tags = %v", main.Tags)
	}
	if fieldInt(t, main, "results") != 2 || fieldInt(t, main, "rules") != 137 || main.Fields["commit"] != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Errorf("analysis fields = %v", main.Fields)
	}
	if want := time.Date(2026, 9, 7, 4, 0, 0, 0, time.UTC); !main.Time.Equal(want) {
		t.Errorf("analysis stamped %s, want created_at %s", main.Time, want)
	}
	// refs/tags/ is trimmed the same way refs/heads/ is.
	find(t, points, "gh_code_scanning_analysis", map[string]string{"ref": "v1.2.0", "version": "2.18.4"})
}

func TestAnalysesDisabled(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/code-scanning/analyses", 403, "Advanced Security must be enabled for this repository to use code scanning.")
	points, err := Analyses{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("err=%v points=%d", err, len(points))
	}
}

func TestSecurityDependabotPagesByCursorDuringABackfill(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/dependabot/alerts", func(w http.ResponseWriter, r *http.Request) {
		if _, byNumber := r.URL.Query()["page"]; byNumber {
			// Dependabot refuses page= outright; only the cursor works.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Pagination using the page parameter is not supported"}`))
			return
		}
		if r.URL.Query().Get("after") == "" {
			w.Header().Set("Link", `<https://api.github.com/repos/octocat/hello-world/dependabot/alerts?per_page=100&after=cursor-2>; rel="next"`)
			_, _ = w.Write(repeat(t, "dependabot_alerts.json", "", 100, func(i int, row map[string]any) {
				row["number"] = 1000 + i
			}))
			return
		}
		f.write(w, "dependabot_alerts.json")
	})
	f.file("/repos/octocat/hello-world/code-scanning/alerts", "code_scanning_alerts.json")

	points, err := Security{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/repos/octocat/hello-world/dependabot/alerts")
	if len(calls) != 2 {
		t.Fatalf("made %d dependabot calls, want 2 pages", len(calls))
	}
	if calls[1].Query["after"] != "cursor-2" {
		t.Errorf("second page must carry the cursor from the Link header, got %v", calls[1].Query)
	}
	if len(only(t, points, "gh_dependabot_alert_item")) != 104 {
		t.Errorf("got %d alert items, want 104", len(only(t, points, "gh_dependabot_alert_item")))
	}
	dep := find(t, points, "gh_security_feature", map[string]string{"feature": "dependabot"})
	if fieldInt(t, dep, "alerts") != 104 || fieldInt(t, dep, "open_alerts") != 101 {
		t.Errorf("dependabot feature = %v", dep.Fields)
	}
}

// TestSecurityCodeScanningStopsAtTheBackfillBound serves a full page whose
// oldest alert predates the bound: the walk stops there, as the Dependabot
// one does, instead of paying for every page back to the first scan.
func TestSecurityCodeScanningStopsAtTheBackfillBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/dependabot/alerts", 403, "Dependabot alerts are disabled for this repository.")
	// Every page is full and the last alert of each is older than the one
	// before, so only the bound can end the walk.
	f.handle("/repos/octocat/hello-world/code-scanning/alerts", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_, _ = w.Write(repeat(t, "code_scanning_alerts.json", "", 100, func(i int, row map[string]any) {
			row["number"] = 10000 - 100*page - i
			row["created_at"] = testNow.AddDate(0, 0, -page*30-i).Format(time.RFC3339)
		}))
	})

	points, err := Security{Walk: Walk{Pages: -1, Since: testNow.AddDate(0, 0, -45)}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	// Page one runs from 30 to 129 days back and its last alert is past the
	// bound, so page two is never asked for.
	if n := len(f.calls("/repos/octocat/hello-world/code-scanning/alerts")); n != 1 {
		t.Errorf("made %d code scanning calls during a backfill bounded at 45 days, want 1", n)
	}
	if n := len(only(t, points, "gh_code_scanning_alert_item")); n != 100 {
		t.Errorf("got %d alert items, want the whole page that was read", n)
	}
}

func TestSecurityEmptyListIsEnabledWithoutAnExtraRequest(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// The feature is on and there is nothing to report, which GitHub answers
	// with 200 and an empty list rather than the 403 it uses for a feature
	// that is off. Measured on 2026-09-08: jmrplens/TFG-TFM_EPS for
	// Dependabot, jmrplens/jmrplens for code scanning. They are different
	// repositories on purpose, because TFG-TFM_EPS answers 404 "no analysis
	// found" to the code scanning list, which is the state the test below
	// records as not enabled.
	f.handle("/repos/octocat/hello-world/dependabot/alerts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	f.handle("/repos/octocat/hello-world/code-scanning/alerts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	points, err := Security{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	for _, feature := range []string{"dependabot", "code_scanning"} {
		p := find(t, points, "gh_security_feature", map[string]string{"feature": feature})
		if p.Fields["enabled"] != true || fieldInt(t, p, "open_alerts") != 0 || fieldInt(t, p, "alerts") != 0 {
			t.Errorf("%s = %v, want enabled with zero alerts", feature, p.Fields)
		}
	}
	// One request each. The per_page=1 probe that used to draw this line is
	// gone: the full first page already draws it.
	for _, path := range []string{
		"/repos/octocat/hello-world/dependabot/alerts",
		"/repos/octocat/hello-world/code-scanning/alerts",
	} {
		calls := f.calls(path)
		if len(calls) != 1 {
			t.Fatalf("%s was called %d times, want 1", path, len(calls))
		}
		if calls[0].Query["per_page"] != "100" {
			t.Errorf("%s asked for %q per page, want a full page", path, calls[0].Query["per_page"])
		}
	}
}

// A feature switched off deeper into a backfill is about the walk, not about
// the repository: the pages already read stay, and the feature stays enabled.
func TestSecurityRefusalAfterTheFirstPageKeepsWhatWasRead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/code-scanning/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(repeat(t, "code_scanning_alerts.json", "", 100, func(i int, row map[string]any) {
				row["number"] = 1000 + i
			}))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Code scanning is not enabled for this repository."}`))
	})
	// The same thing on the other walk, which pages by cursor rather than by
	// number and so has its own copy of the decision.
	f.handle("/repos/octocat/hello-world/dependabot/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Dependabot alerts are disabled for this repository."}`))
			return
		}
		w.Header().Set("Link", `<https://api.github.com/repos/octocat/hello-world/dependabot/alerts?per_page=100&after=cursor-2>; rel="next"`)
		_, _ = w.Write(repeat(t, "dependabot_alerts.json", "", 100, func(i int, row map[string]any) {
			row["number"] = 1000 + i
		}))
	})

	points, err := Security{Walk: Unbounded}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	cs := find(t, points, "gh_security_feature", map[string]string{"feature": "code_scanning"})
	if cs.Fields["enabled"] != true || fieldInt(t, cs, "alerts") != 100 {
		t.Errorf("code scanning feature = %v, want enabled with the 100 rows page one gave", cs.Fields)
	}
	dep := find(t, points, "gh_security_feature", map[string]string{"feature": "dependabot"})
	if dep.Fields["enabled"] != true || fieldInt(t, dep, "alerts") != 100 {
		t.Errorf("dependabot feature = %v, want enabled with the 100 rows page one gave", dep.Fields)
	}
}

// The fallback that keeps an alert tag from ever being empty, which no
// fixture exercises because no live alert does: of 257 read on 2026-09-08,
// every one named its scope, its relationship and its manifest. It still has
// to hold, because sink.LineProtocol drops an empty tag value, which would
// move that one point into a series of its own.
func TestAlertTagIsNeverEmpty(t *testing.T) {
	t.Parallel()
	if got := orNone(""); got != noneTag {
		t.Errorf("orNone(%q) = %q, want the one fallback this package writes", "", got)
	}
	if got := orNone("development"); got != "development" {
		t.Errorf("orNone(%q) = %q, a value GitHub sent must survive", "development", got)
	}
	p := sink.Point{
		Measurement: "gh_dependabot_alert_item",
		Tags:        map[string]string{"scope": orNone("")},
		Fields:      map[string]any{"alerts": 1},
		Time:        testNow,
	}
	if !strings.Contains(sink.LineProtocol(p), "scope="+noneTag) {
		t.Errorf("the tag must reach the line protocol: %s", sink.LineProtocol(p))
	}
}

// TestAPastDatedRowSaysTheSameThingWheneverItIsCollected is the gate on the
// whole class of defect this closes: a field derived from the clock, written
// on to a row dated when the thing happened.
//
// Such a field is only true at the instant of the sweep that wrote it, and it
// makes every later sweep rewrite a row dated in the past, which in InfluxDB 3
// files another parquet file into that old partition and never compacts it.
// Measured on 2026-09-17, the two alert families and gh_fork were writing
// about 260 files a day between them for some 2,300 rows, growing with the
// clock rather than with anything GitHub had to say.
//
// So the same fixture collected three days apart has to produce byte for byte
// the same rows. Anything a panel wants from the clock is now() less the row's
// own timestamp, which is right when it is asked rather than when it was
// written.
func TestAPastDatedRowSaysTheSameThingWheneverItIsCollected(t *testing.T) {
	t.Parallel()
	dated := []string{"gh_dependabot_alert_item", "gh_code_scanning_alert_item", "gh_fork"}
	collect := func(now time.Time) []string {
		f := newFixtureServer(t)
		f.file("/repos/octocat/hello-world/dependabot/alerts", "dependabot_alerts.json")
		f.file("/repos/octocat/hello-world/code-scanning/alerts", "code_scanning_alerts.json")
		f.file("/repos/octocat/hello-world/forks", "forks.json")
		points, err := Security{}.Collect(ctx(t), f.Client, testRepo, now)
		if err != nil {
			t.Fatal(err)
		}
		forks, err := (Forks{}).Collect(ctx(t), f.Client, testRepo, now)
		if err != nil {
			t.Fatal(err)
		}
		return lines(append(points, forks...), dated...)
	}
	first, later := collect(testNow), collect(testNow.AddDate(0, 0, 3))
	if len(first) == 0 {
		t.Fatal("the fixtures produced no past-dated row to compare")
	}
	if len(first) != len(later) {
		t.Fatalf("%d rows collected now against %d three days later", len(first), len(later))
	}
	for i := range first {
		if first[i] != later[i] {
			t.Errorf("a row dated in the past moved with the clock:\n at the time %s\nthree days later %s",
				first[i], later[i])
		}
	}
}
