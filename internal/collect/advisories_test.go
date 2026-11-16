package collect

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

const advisoriesPath = "/repos/octocat/hello-world/security-advisories"

func collectAdvisories(t *testing.T, details bool) []sink.Point {
	t.Helper()
	f := newFixtureServer(t)
	f.file(advisoriesPath, "repo_advisories.json")
	points, err := Advisories{Details: details}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	return points
}

func advisoryRow(t *testing.T, points []sink.Point, ghsa string) sink.Point {
	t.Helper()
	return find(t, points, "gh_repo_advisory", map[string]string{"ghsa": ghsa})
}

func TestAdvisoriesRowPerAdvisory(t *testing.T) {
	t.Parallel()
	points := collectAdvisories(t, false)
	rows := only(t, points, "gh_repo_advisory")
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want one per advisory in every state", len(rows))
	}
	for _, p := range rows {
		keys := make([]string, 0, len(p.Tags))
		for k := range p.Tags {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if strings.Join(keys, ",") != "full_name,ghsa,owner,repo" {
			t.Errorf("%s: tags %v, want only the repository and the GHSA", p.Tags["ghsa"], keys)
		}
	}

	day := testNow.UTC().Truncate(24 * time.Hour)
	for ghsa, want := range map[string]time.Time{
		"GHSA-aaaa-bbbb-cccc": time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		"GHSA-jjjj-kkkk-llll": time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC),
		"GHSA-dddd-eeee-ffff": day,
		"GHSA-gggg-hhhh-iiii": day,
	} {
		if got := advisoryRow(t, points, ghsa).Time; !got.Equal(want) {
			t.Errorf("%s dated %v, want %v", ghsa, got, want)
		}
	}

	pub := advisoryRow(t, points, "GHSA-aaaa-bbbb-cccc")
	for k, want := range map[string]any{
		"advisory_state": "published", "severity": "high", "cvss": 7.5,
		"cve": "CVE-2026-0001", "cwes": "CWE-20,CWE-23", "ecosystems": "go,npm",
		"packages": 3, "credits": 1, "comments": 3, "submitted": 1, "reporter": "someone",
		"seconds_to_publish": int((4*24 + 2) * time.Hour / time.Second),
		"url":                "https://github.com/octocat/hello-world/security/advisories/GHSA-aaaa-bbbb-cccc",
	} {
		if got := pub.Fields[k]; got != want {
			t.Errorf("published %s = %v (%T), want %v", k, got, got, want)
		}
	}
	closed := advisoryRow(t, points, "GHSA-jjjj-kkkk-llll")
	if got := fieldInt(t, closed, "seconds_to_close"); got != int64(2*24*time.Hour/time.Second) {
		t.Errorf("seconds_to_close = %d", got)
	}
	// A maintainer's own draft is not a report, and names no reporter.
	draft := advisoryRow(t, points, "GHSA-gggg-hhhh-iiii")
	if draft.Fields["submitted"] != 0 || draft.Fields["reporter"] != nil {
		t.Errorf("draft: submitted %v reporter %v", draft.Fields["submitted"], draft.Fields["reporter"])
	}
}

func TestAdvisoryDraftOmitsWhatItHasNot(t *testing.T) {
	t.Parallel()
	draft := advisoryRow(t, collectAdvisories(t, true), "GHSA-gggg-hhhh-iiii")
	for _, k := range []string{"severity", "cvss", "cvss_v4", "seconds_to_publish", "seconds_to_close", "cve", "cwes", "ecosystems", "packages"} {
		if v, ok := draft.Fields[k]; ok {
			t.Errorf("a draft carries %s = %v, which it does not have", k, v)
		}
	}
}

// The detail fields of an advisory under embargo.
var advisoryDetails = []string{"summary", "cve", "cwes", "ecosystems", "packages"}

func TestAdvisoriesHideUnpublishedDetailsByDefault(t *testing.T) {
	t.Parallel()
	points := collectAdvisories(t, false)
	triage := advisoryRow(t, points, "GHSA-dddd-eeee-ffff")
	for _, k := range advisoryDetails {
		if v, ok := triage.Fields[k]; ok {
			t.Errorf("an unpublished report carries %s = %v without being asked", k, v)
		}
	}
	for _, k := range []string{"url", "severity", "advisory_state", "cvss"} {
		if _, ok := triage.Fields[k]; !ok {
			t.Errorf("an unpublished report lost %s, which says nothing about the flaw", k)
		}
	}
	pub := advisoryRow(t, points, "GHSA-aaaa-bbbb-cccc")
	for _, k := range advisoryDetails {
		if _, ok := pub.Fields[k]; !ok {
			t.Errorf("a published advisory lost %s", k)
		}
	}
}

func TestAdvisoriesKeepEveryDetailWhenAsked(t *testing.T) {
	t.Parallel()
	triage := advisoryRow(t, collectAdvisories(t, true), "GHSA-dddd-eeee-ffff")
	for k, want := range map[string]any{"summary": "Session tokens survive a password change", "cwes": "CWE-613", "ecosystems": "go", "packages": 1} {
		if got := triage.Fields[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
}

func TestAdvisoriesRefusedIsNothingHere(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status(advisoriesPath, http.StatusNotFound, "Not Found")
	points, err := Advisories{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Fatalf("got %d points and %v, want nothing and no error", len(points), err)
	}
}

func TestAdvisoriesFollowTheCursor(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	full := repeat(t, "repo_advisories.json", "", 100, func(i int, row map[string]any) {
		row["ghsa_id"] = fmt.Sprintf("GHSA-page-one-%04d", i)
	})
	f.handle(advisoriesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			w.Header().Set("Link", `<https://api.github.com`+advisoriesPath+`?after=Y3Vyc29y&per_page=100>; rel="next"`)
			_, _ = w.Write(full)
			return
		}
		f.write(w, "repo_advisories.json")
	})
	points, err := Advisories{Walk: Walk{Pages: -1}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(only(t, points, "gh_repo_advisory")); got != 104 {
		t.Fatalf("got %d rows, want both pages", got)
	}
	if calls := f.calls(advisoriesPath); len(calls) != 2 || calls[1].Query["after"] != "Y3Vyc29y" {
		t.Fatalf("calls %+v, want the second page by its cursor", calls)
	}
}
