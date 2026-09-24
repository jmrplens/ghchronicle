package collect

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// inventoryRoutes serves the four calls of one repository, from responses
// captured on 2026-09-10: the workflow policy of phonometry (write, and so
// able to approve a pull request) and the secrets and code scanning setup of
// jmrp.io.
func inventoryRoutes(f *fixtureServer) {
	f.file("/repos/octocat/hello-world/actions/permissions/workflow", "actions_permissions_workflow.json")
	f.file("/repos/octocat/hello-world/actions/secrets", "actions_secrets.json")
	f.file("/repos/octocat/hello-world/dependabot/secrets", "dependabot_secrets.json")
	f.file("/repos/octocat/hello-world/code-scanning/default-setup", "code_scanning_setup.json")
}

func TestRepoInventory(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	inventoryRoutes(f)

	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_actions_policy", "gh_secret", "gh_code_scanning_setup")
	day := startOfDay(testNow)
	for _, p := range points {
		// All three are settings, not events. A re-run has to land on the row
		// it already wrote instead of adding a second one.
		if !p.Time.Equal(day) {
			t.Errorf("%s stamped %s, want the start of the UTC day %s", p.Measurement, p.Time, day)
		}
	}
	checkActionsPolicy(t, points)
	checkSecrets(t, points)
	checkCodeScanningConfigured(t, points)
}

func checkActionsPolicy(t *testing.T, points []sink.Point) {
	t.Helper()
	policy := only(t, points, "gh_actions_policy")
	if len(policy) != 1 {
		t.Fatalf("got %d policy rows, want one per repository", len(policy))
	}
	if policy[0].Tags["permissions"] != "write" {
		t.Errorf("permissions = %v", policy[0].Tags)
	}
	if policy[0].Fields["can_approve_pr"] != true || fieldInt(t, policy[0], "policies") != 1 {
		t.Errorf("policy fields = %v", policy[0].Fields)
	}
	if hasField(policy[0], "permissions") {
		t.Error("permissions is a tag and must not also be a field")
	}
}

// checkSecrets reads the age of a stored credential, which is the number the
// measurement exists for, and the kind tag that separates the two stores.
func checkSecrets(t *testing.T, points []sink.Point) {
	t.Helper()
	secrets := only(t, points, "gh_secret")
	if len(secrets) != 12 {
		t.Fatalf("got %d secrets, want 8 actions plus 4 dependabot", len(secrets))
	}
	// Every secret keeps its own row: the name is the identity, so two
	// secrets of the same repository must not collapse into one series.
	seen := map[string]bool{}
	for _, p := range secrets {
		key := p.Tags["kind"] + "/" + p.Tags["secret"]
		if seen[key] {
			t.Errorf("two rows share the series %q", key)
		}
		seen[key] = true
		if p.Tags["secret"] == "" {
			t.Error("the secret name is a tag and is never allowed to be empty")
		}
	}
	// SONAR_TOKEN exists in both stores with different dates, which is what
	// makes kind a tag rather than a field.
	act := find(t, points, "gh_secret", map[string]string{"kind": "actions", "secret": "SONAR_TOKEN"})
	created := time.Date(2025, 12, 19, 15, 4, 41, 0, time.UTC)
	if want := int(testNow.Sub(created).Hours() / 24); fieldInt(t, act, "age_days") != int64(want) {
		t.Errorf("age_days = %v, want %d", act.Fields["age_days"], want)
	}
	// None of jmrp.io's secrets has ever been rotated, so the two ages
	// agree here. TestRepoInventoryRotatedSecret covers the other case.
	if act.Fields["age_days"] != act.Fields["days_since_rotation"] {
		t.Errorf("an unrotated secret has one age: %v", act.Fields)
	}
	dep := find(t, points, "gh_secret", map[string]string{"kind": "dependabot", "secret": "SONAR_TOKEN"})
	if fieldInt(t, dep, "age_days") == fieldInt(t, act, "age_days") {
		t.Error("the two stores hold the same name with different dates")
	}
	if fieldInt(t, dep, "secrets") != 1 {
		t.Errorf("dependabot secret fields = %v", dep.Fields)
	}
}

func checkCodeScanningConfigured(t *testing.T, points []sink.Point) {
	t.Helper()
	setup := only(t, points, "gh_code_scanning_setup")
	if len(setup) != 1 {
		t.Fatalf("got %d setup rows, want one per repository", len(setup))
	}
	p := setup[0]
	if p.Tags["state"] != "configured" || p.Tags["query_suite"] != "default" || p.Tags["schedule"] != "weekly" {
		t.Errorf("setup tags = %v", p.Tags)
	}
	if fieldInt(t, p, "languages") != 5 {
		t.Errorf("languages = %v, want the five of the captured response", p.Fields["languages"])
	}
	changed := time.Date(2026, 7, 26, 2, 55, 11, 0, time.UTC)
	if want := int(testNow.Sub(changed).Hours() / 24); fieldInt(t, p, "days_since_change") != int64(want) {
		t.Errorf("days_since_change = %v, want %d", p.Fields["days_since_change"], want)
	}
}

// TestRepoInventoryRotatedSecret is why age_days and days_since_rotation are
// two fields rather than one. Measured on 2026-09-10: WINGET_TOKEN was created
// 64 days ago and replaced 14 days ago, and ADMIN_TOKEN 133 and 101. Keeping
// only created_at would report a credential as four times more stale than it
// is; keeping only updated_at would lose that it has been in place since April.
func TestRepoInventoryRotatedSecret(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	inventoryRoutes(f)
	f.file("/repos/octocat/hello-world/actions/secrets", "actions_secrets_rotated.json")

	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	rotated := find(t, points, "gh_secret", map[string]string{"kind": "actions", "secret": "WINGET_TOKEN"})
	created := time.Date(2026, 7, 8, 0, 20, 36, 0, time.UTC)
	changed := time.Date(2026, 8, 27, 12, 58, 11, 0, time.UTC)
	if want := int64(testNow.Sub(created).Hours() / 24); fieldInt(t, rotated, "age_days") != want {
		t.Errorf("age_days = %v, want %d from created_at", rotated.Fields["age_days"], want)
	}
	if want := int64(testNow.Sub(changed).Hours() / 24); fieldInt(t, rotated, "days_since_rotation") != want {
		t.Errorf("days_since_rotation = %v, want %d from updated_at", rotated.Fields["days_since_rotation"], want)
	}
	if fieldInt(t, rotated, "days_since_rotation") >= fieldInt(t, rotated, "age_days") {
		t.Error("a rotated secret is younger than it is old")
	}
	never := find(t, points, "gh_secret", map[string]string{"kind": "actions", "secret": "PYPI_TOKEN"})
	if never.Fields["age_days"] != never.Fields["days_since_rotation"] {
		t.Errorf("an untouched secret in the same list keeps one age: %v", never.Fields)
	}
}

// TestRepoInventoryNotConfigured is the trap of this collector: updated_at is
// null when the state is not-configured, and an age computed from a zero time
// reads as about twenty thousand days.
func TestRepoInventoryNotConfigured(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	inventoryRoutes(f)
	f.file("/repos/octocat/hello-world/code-scanning/default-setup", "code_scanning_setup_not_configured.json")

	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	p := only(t, points, "gh_code_scanning_setup")[0]
	if p.Tags["state"] != "not-configured" {
		t.Errorf("state = %v", p.Tags)
	}
	if hasField(p, "days_since_change") {
		t.Errorf("updated_at is null here, so there is no age to publish: %v", p.Fields)
	}
	// schedule is null too, and a tag written on some rows and not others
	// gives one measurement two shapes.
	if p.Tags["schedule"] != noneTag {
		t.Errorf("schedule = %q, want the fallback", p.Tags["schedule"])
	}
	// The languages of a not-configured repository are the ones GitHub
	// detected, not the ones anything analyzed. Still counted, because the
	// count is what gh_code_scanning_analysis is read against.
	if fieldInt(t, p, "languages") != 7 {
		t.Errorf("languages = %v", p.Fields["languages"])
	}
}

// TestRepoInventoryCodeScanningOff pins the state that 24 of the 52
// repositories of this account are in. A 403 here is an answer, not a failure,
// and writing no row would make those repositories look unswept.
func TestRepoInventoryCodeScanningOff(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	inventoryRoutes(f)
	f.status("/repos/octocat/hello-world/code-scanning/default-setup", 403,
		"Code scanning is not enabled for this repository. Please enable code scanning in the repository settings.")

	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("a 403 here is a state, not a failure: %v", err)
	}
	checkPoints(t, points)
	p := only(t, points, "gh_code_scanning_setup")[0]
	if p.Tags["state"] != "unavailable" {
		t.Errorf("state = %v", p.Tags)
	}
	// Every tag is written on every row, whatever the state, or the
	// measurement has two Graphite depths.
	for _, tag := range []string{"owner", "repo", "full_name", "state", "query_suite", "schedule"} {
		if p.Tags[tag] == "" {
			t.Errorf("tag %q is missing on an unavailable row: %v", tag, p.Tags)
		}
	}
	if hasField(p, "languages") || hasField(p, "days_since_change") {
		t.Errorf("there is no body to read these from: %v", p.Fields)
	}
	if fieldInt(t, p, "setups") != 1 {
		t.Errorf("an unavailable row still needs a field: %v", p.Fields)
	}
}

// TestRepoInventoryWithoutAdminAccess covers a repository the token only
// reads: all three surfaces need admin and answer 404.
func TestRepoInventoryWithoutAdminAccess(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("404 must be skipped: %v", err)
	}
	checkPoints(t, points)
	// The code scanning row survives as unavailable; the other two do not
	// exist at all, because there is nothing to say about them.
	if got := byMeasurement(points); len(got["gh_actions_policy"]) != 0 || len(got["gh_secret"]) != 0 {
		t.Errorf("got %v", measurements(points))
	}
	if only(t, points, "gh_code_scanning_setup")[0].Tags["state"] != "unavailable" {
		t.Error("a repository the token cannot read is unavailable, not configured")
	}
	if got := len(f.calls("/repos/octocat/hello-world/actions/secrets")); got != 1 {
		t.Errorf("%d calls to actions/secrets, want one per sweep", got)
	}
}

// TestRepoInventoryAsksBothSecretStores keeps the call count honest: this is
// the family's whole cost, four requests per repository per day.
func TestRepoInventoryAsksBothSecretStores(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	inventoryRoutes(f)
	if _, err := (RepoInventory{}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/repos/octocat/hello-world/actions/permissions/workflow",
		"/repos/octocat/hello-world/actions/secrets",
		"/repos/octocat/hello-world/dependabot/secrets",
		"/repos/octocat/hello-world/code-scanning/default-setup",
	} {
		if got := len(f.calls(path)); got != 1 {
			t.Errorf("%d calls to %s, want exactly one", got, path)
		}
	}
	// The five sibling endpoints under /actions/permissions were measured
	// constant or 422 across the account, so none of them is asked for.
	if got := len(f.calls("/repos/octocat/hello-world/actions/permissions")); got != 0 {
		t.Errorf("%d calls to the constant endpoint", got)
	}
	if q := f.calls("/repos/octocat/hello-world/actions/secrets")[0].Query["per_page"]; q != "100" {
		t.Errorf("per_page = %q; a repository can hold more than the default page", q)
	}
}

// TestRepoInventorySpentBudgetIsNotAState is the other half of the 403 above,
// and the more dangerous half: GitHub answers a spent budget with the same
// status a switched-off feature does, and the headers are the only difference.
// Reading one as the other would file all 52 repositories as `unavailable` on
// the day the budget ran out, which is a lie that looks exactly like a fact.
func TestRepoInventorySpentBudgetIsNotAState(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	inventoryRoutes(f)
	f.handle("/repos/octocat/hello-world/code-scanning/default-setup",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", strconv.FormatInt(testNow.Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for user ID 1."}`))
		})

	points, err := RepoInventory{}.Collect(ctx(t), f.Client, testRepo, testNow)
	// The type, not merely an error: this has to be the budget the runner
	// knows how to stop a family for, and not some other failure that happens
	// to arrive from the same handler.
	if _, ok := errors.AsType[*ghapi.RateLimitedError](err); !ok {
		t.Fatalf("a spent budget must be reported as one, got %T %v", err, err)
	}
	for _, p := range points {
		if p.Measurement == "gh_code_scanning_setup" {
			t.Errorf("a budget that ran out produced a setup row: %v", p.Tags)
		}
	}
}
