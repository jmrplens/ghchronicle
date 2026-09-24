package collect

import (
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

func settingsRoutes(f *fixtureServer) {
	f.file("/repos/octocat/hello-world/hooks", "hooks.json")
	f.file("/repos/octocat/hello-world/hooks/12345678/deliveries", "hook_deliveries.json")
	f.file("/repos/octocat/hello-world/rulesets", "rulesets.json")
	f.file("/repos/octocat/hello-world/environments", "environments.json")
	f.file("/repos/octocat/hello-world/keys", "deploy_keys.json")
}

func TestSettings(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	settingsRoutes(f)

	points, err := Settings{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	// Rulesets moved to RepoDetail, which asks for ten repositories at once.
	wantMeasurements(t, points, "gh_webhook", "gh_webhook_delivery", "gh_environment", "gh_deploy_key")
	day := startOfDay(testNow)

	checkWebhookRow(t, points, day)
	checkNoTagOrFieldLeaksTheWebhookPath(t, points)
	checkWebhookDeliveries(t, points, f)
	checkEnvironmentRow(t, points, day)
	checkDeployKeyRows(t, points)
}

// checkWebhookRow reads the hook itself: only the host of its URL is kept,
// because the path carries a secret.
func checkWebhookRow(t *testing.T, points []sink.Point, day time.Time) {
	t.Helper()
	// Only the host of the webhook URL is kept; the path carries a secret.
	hook := only(t, points, "gh_webhook")[0]
	// The id and not the name: GitHub names every webhook "web", so the name
	// identified nothing and two hooks at one host were one series.
	if hook.Tags["host"] != "hooks.example.com" || hook.Tags["hook"] != "12345678" || hook.Tags["active"] != "true" {
		t.Errorf("webhook tags = %v", hook.Tags)
	}
	if fieldInt(t, hook, "events") != 3 || !hook.Time.Equal(day) {
		t.Errorf("webhook = %v at %s", hook.Fields, hook.Time)
	}
}

// checkNoTagOrFieldLeaksTheWebhookPath sweeps every point produced, not only
// the webhook's, because a leak anywhere is the same leak.
func checkNoTagOrFieldLeaksTheWebhookPath(t *testing.T, points []sink.Point) {
	t.Helper()
	for _, p := range points {
		for k, v := range p.Tags {
			if strings.Contains(v, "secret-path") || strings.Contains(v, "/github/") {
				t.Errorf("%s tag %s leaks the webhook path: %q", p.Measurement, k, v)
			}
		}
		for k, v := range p.Fields {
			if s, ok := v.(string); ok && strings.Contains(s, "secret-path") {
				t.Errorf("%s field %s leaks the webhook path: %q", p.Measurement, k, s)
			}
		}
	}
}

// checkWebhookDeliveries reads what each attempt cost and whether it was a
// redelivery, which is the difference between a flaky hook and a dead one.
func checkWebhookDeliveries(t *testing.T, points []sink.Point, f *fixtureServer) {
	t.Helper()
	deliveries := f.calls("/repos/octocat/hello-world/hooks/12345678/deliveries")
	if len(deliveries) != 1 || deliveries[0].Query["per_page"] != "30" {
		t.Errorf("deliveries calls = %d, query %v, want the default of 30", len(deliveries), deliveries[0].Query)
	}
	ok := find(t, points, "gh_webhook_delivery", map[string]string{"code": "200"})
	if ok.Tags["ok"] != "true" || ok.Tags["status"] != "OK" || ok.Tags["event"] != "push" || ok.Tags["host"] != "hooks.example.com" {
		t.Errorf("delivery tags = %v", ok.Tags)
	}
	if ok.Fields["duration_seconds"] != 0.27 || ok.Fields["redelivery"] != false || fieldInt(t, ok, "deliveries") != 1 {
		t.Errorf("delivery fields = %v", ok.Fields)
	}
	if want := time.Date(2026, 9, 7, 9, 15, 1, 0, time.UTC); !ok.Time.Equal(want) {
		t.Errorf("delivery stamped %s, want delivered_at %s", ok.Time, want)
	}
	failed := find(t, points, "gh_webhook_delivery", map[string]string{"code": "403"})
	if failed.Tags["ok"] != "false" || failed.Fields["redelivery"] != true {
		t.Errorf("failed delivery = %v %v", failed.Tags, failed.Fields)
	}
}

// checkEnvironmentRow reads the deployment environment, which is inventory and
// so is stamped at the start of the day.
func checkEnvironmentRow(t *testing.T, points []sink.Point, day time.Time) {
	t.Helper()
	env := find(t, points, "gh_environment", map[string]string{"environment": "production"})
	if fieldInt(t, env, "environments") != 1 || !env.Time.Equal(day) {
		t.Errorf("environment = %v %v", env.Tags, env.Fields)
	}
	// Two rules and a custom branch policy: a guarded environment.
	if fieldInt(t, env, "protection_rules") != 2 || env.Fields["has_branch_policy"] != true {
		t.Errorf("guarded environment = %v", env.Fields)
	}
	if env.Fields["custom_branch_policies"] != true || env.Fields["protected_branches"] != false {
		t.Errorf("branch policy = %v", env.Fields)
	}
	if env.Fields["can_admins_bypass"] != false {
		t.Errorf("can_admins_bypass = %v", env.Fields["can_admins_bypass"])
	}
	// created_at and updated_at are different clocks: this one was made in
	// January 2025 and last changed in June 2026.
	if fieldInt(t, env, "age_days") < 600 || fieldInt(t, env, "days_since_change") != 99 {
		t.Errorf("environment ages = %v", env.Fields)
	}

	// The reading the whole entry exists for: an environment named after a
	// token, with no rule, no branch policy and an admin bypass, is a secret
	// store any branch can deploy to. Both environments on jmrplens/jmrp.io
	// look exactly like this today.
	open := find(t, points, "gh_environment", map[string]string{"environment": "PYPI_TOKEN"})
	if fieldInt(t, open, "protection_rules") != 0 || open.Fields["has_branch_policy"] != false {
		t.Errorf("unguarded environment = %v", open.Fields)
	}
	// A null deployment_branch_policy is not "restricted to protected
	// branches": both booleans are false and has_branch_policy separates them.
	if open.Fields["protected_branches"] != false || open.Fields["custom_branch_policies"] != false {
		t.Errorf("a null branch policy = %v", open.Fields)
	}
	if open.Fields["can_admins_bypass"] != true {
		t.Errorf("can_admins_bypass = %v", open.Fields["can_admins_bypass"])
	}

	pages := find(t, points, "gh_environment", map[string]string{"environment": "github-pages"})
	if pages.Fields["protected_branches"] != true || pages.Fields["custom_branch_policies"] != false {
		t.Errorf("a policy restricted to protected branches = %v", pages.Fields)
	}
}

// checkDeployKeyRows reads the keys, including the one that has never been
// used at all, which is the reading nothing else offers.
func checkDeployKeyRows(t *testing.T, points []sink.Point) {
	t.Helper()
	keys := only(t, points, "gh_deploy_key")
	if len(keys) != 2 {
		t.Fatalf("got %d deploy keys", len(keys))
	}
	ro := find(t, points, "gh_deploy_key", map[string]string{"key": "deploy-bot"})
	// read_only is a tag and must not also be a field: InfluxDB rejects a
	// line that uses one name for both, and the whole batch with it.
	if ro.Tags["read_only"] != "true" {
		t.Errorf("read_only tag = %q", ro.Tags["read_only"])
	}
	if hasField(ro, "read_only") {
		t.Error("read_only must not be a field")
	}
	if fieldInt(t, ro, "days_since_use") != 7 || fieldInt(t, ro, "keys") != 1 {
		t.Errorf("read-only key fields = %v", ro.Fields)
	}
	rw := find(t, points, "gh_deploy_key", map[string]string{"key": "ci-writer"})
	if rw.Tags["read_only"] != "false" || hasField(rw, "days_since_use") {
		t.Errorf("never-used write key = %v %v", rw.Tags, rw.Fields)
	}
}

func TestSettingsDeliveriesBound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	settingsRoutes(f)
	if _, err := (Settings{Deliveries: 100}).Collect(ctx(t), f.Client, testRepo, testNow); err != nil {
		t.Fatal(err)
	}
	calls := f.calls("/repos/octocat/hello-world/hooks/12345678/deliveries")
	if len(calls) != 1 || calls[0].Query["per_page"] != "100" {
		t.Errorf("deliveries query = %v", calls[0].Query)
	}
}

func TestSettingsWithoutAdminAccess(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// Hooks, rulesets, environments and keys all need admin; a 404 on each
	// is the normal answer for a repository the token only reads.
	f.status("/repos/octocat/hello-world/hooks", 404, "Not Found")
	points, err := Settings{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("404 must be skipped: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points", len(points))
	}
}

// TestSettingsKeepsTheHooksReadBeforeADeliveryListFails pins what a failure
// halfway through the hooks hands back: the hook already read, and no request
// after it, because the environments and the deploy keys would meet the same
// spent budget or failing server.
func TestSettingsKeepsTheHooksReadBeforeADeliveryListFails(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	settingsRoutes(f)
	f.status("/repos/octocat/hello-world/hooks/12345678/deliveries", 500, "boom")
	points, err := Settings{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err == nil {
		t.Fatal("a 500 on the delivery list must be returned")
	}
	if len(points) != 1 || points[0].Measurement != "gh_webhook" {
		t.Errorf("got %d points (%v), want the one hook read before the failure", len(points), points)
	}
	for _, path := range []string{"/repos/octocat/hello-world/environments", "/repos/octocat/hello-world/keys"} {
		if calls := f.calls(path); len(calls) != 0 {
			t.Errorf("%s read %d times after the failure", path, len(calls))
		}
	}
}

func TestHostOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://hooks.example.com/github/8f3a2b1c-secret": "hooks.example.com",
		"http://10.0.0.5:8080/hook":                        "10.0.0.5:8080",
		"https://example.com":                              "example.com",
		"example.com/path":                                 "example.com",
		"":                                                 "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
