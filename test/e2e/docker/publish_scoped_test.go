//go:build dockere2e

package docker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/racereport"
)

// Publishing with the token the documentation recommends: a service account
// that may publish dashboards and read datasources, and may make none. In a
// Grafana without Enterprise's custom roles that is an Editor. The unit tests
// hold the same cases against a stub; this holds them against the Grafana the
// suite already runs, whose refusals are what that stub imitates.
//
// The store's datasource is the provisioned one, named by uid, because an
// Editor cannot make one and the dashboard has nothing to read without it. The
// Loki datasource is the question: 2.4.0 and 2.5.0 abandoned the whole publish
// when it could not be created, or when one an Admin had made could not be
// rewritten, although it feeds one panel that has a note to fall back to.

// scopedDashboard is where the publish below lands: the Prometheus store's
// generated uid, since its sink needs no server of its own to publish.
const scopedDashboard = "ghchronicle-prometheus"

func TestAScopedTokenPublishesWithoutMakingADatasource(t *testing.T) {
	s := Start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	admin := grafana.Client{URL: s.GrafanaURL, User: grafanaUser, Password: grafanaPassword}
	token := mintEditorToken(ctx, t, admin)
	t.Cleanup(func() {
		// The dashboard and the account are this test's, and a reused stack
		// should not find them next time; what the provisioning made is not.
		// The test's own context is already done by the time this runs.
		_ = admin.Delete(context.WithoutCancel(ctx), grafana.DashboardPath(scopedDashboard), time.Minute)
	})

	dir, err := filepath.Abs(filepath.Join("out", "publish-scoped"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	binary, err := sqlStoresBuild(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		// loki is where the sink pushes. The publish never writes to it; the
		// address only decides which datasource the panel would read.
		loki   string
		tenant string
		// existing has an Admin make ghchronicle-loki before the Editor
		// publishes, as a first run with a wider token would have.
		existing bool
		panel    string
		source   string
		says     []string
	}{
		{
			// The host's published port is not the address Grafana reaches
			// Loki by, so the provisioned datasource is not this one and the
			// Editor may not make it.
			name: "a Loki datasource elsewhere is refused and every dashboard still goes",
			loki: s.LokiURL + "/loki/api/v1/push", panel: "text", source: "",
			says: []string{
				"warning:", "ghchronicle-loki", "datasources:create",
				"grafana.datasource.loki_uid", DatasourceLoki,
			},
		},
		{
			name: "the Loki datasource Grafana already reads is adopted",
			loki: "http://loki:3100/loki/api/v1/push", panel: "logs", source: DatasourceLoki,
			says: []string{DatasourceLoki + " (loki) adopted"},
		},
		{
			// A tenant is a secret Grafana never hands back, so the
			// datasource is rewritten on every start, and an Editor is
			// refused that. The datasource is still the one the panel read.
			name: "a ghchronicle-loki an Admin made is read as it is",
			loki: s.LokiURL + "/loki/api/v1/push", tenant: "tenant-one", existing: true,
			panel: "logs", source: "ghchronicle-loki",
			says: []string{
				"warning:", "ghchronicle-loki is used as Grafana has it", "datasources:write",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publishScoped(ctx, t, scopedRun{
				stack: s, admin: admin, binary: binary, dir: dir, token: token,
				loki: tc.loki, tenant: tc.tenant, existing: tc.existing,
				panel: tc.panel, source: tc.source, says: tc.says,
			})
		})
	}
}

// scopedRun is one publish with the Editor's token and what it should leave.
type scopedRun struct {
	stack              *Stack
	admin              grafana.Client
	binary, dir, token string
	loki, tenant       string
	existing           bool
	panel, source      string
	says               []string
}

func publishScoped(ctx context.Context, t *testing.T, run scopedRun) {
	t.Helper()
	tenant := ""
	if run.existing {
		// What an Admin run leaves behind: the datasource made from the sink,
		// with the tenant in the secret no later read can compare.
		made := grafana.Datasource{
			UID: "ghchronicle-loki", Name: "ghchronicle-loki", Type: "loki",
			URL:    strings.TrimSuffix(run.loki, "/loki/api/v1/push"),
			JSON:   map[string]any{"httpHeaderName1": "X-Scope-OrgID"},
			Secret: map[string]string{"httpHeaderValue1": run.tenant},
		}
		if _, err := run.admin.EnsureDatasource(ctx, made, time.Minute); err != nil {
			t.Fatalf("making ghchronicle-loki as an Admin: %v", err)
		}
		t.Cleanup(func() {
			_ = run.admin.Delete(context.WithoutCancel(ctx),
				grafana.DatasourcePath("ghchronicle-loki"), time.Minute)
		})
	}
	if run.tenant != "" {
		tenant = "\n    tenant_id: " + run.tenant
	}
	cfg := filepath.Join(run.dir, "config.yaml")
	body := fmt.Sprintf(`targets:
  user: acme
sinks:
  prometheus:
    listen: 127.0.0.1:0
  loki:
    url: %s%s
grafana:
  url: %s
  token: %s
  datasource:
    uid: %s
`, run.loki, tenant, run.stack.GrafanaURL, run.token, DatasourcePrometheus)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	said, err := publishWith(ctx, t, run.binary, cfg)
	if err != nil {
		t.Fatalf("the publish failed: %v\n%s", err, said)
	}
	for _, want := range run.says {
		if !strings.Contains(said, want) {
			t.Errorf("the run did not say %q:\n%s", want, said)
		}
	}
	kind, source := scopedLogPanel(ctx, t, run.admin)
	if kind != run.panel || source != run.source {
		t.Errorf("the failed output panel is a %q reading %q, want a %q reading %q",
			kind, source, run.panel, run.source)
	}
	there, err := run.admin.Exists(ctx, grafana.DatasourcePath("ghchronicle-loki"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case there && !run.existing:
		t.Error("a ghchronicle-loki datasource exists, which an Editor cannot have made")
	case !there && run.existing:
		t.Error("the ghchronicle-loki an Admin made is gone")
	}
}

// mintEditorToken makes a service account with the Editor role and a token
// for it, and removes the account when the test ends.
func mintEditorToken(ctx context.Context, t *testing.T, admin grafana.Client) string {
	t.Helper()
	name := "e2e-editor-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	account, err := admin.Do(ctx, http.MethodPost, "/api/serviceaccounts",
		map[string]any{"name": name, "role": "Editor", "isDisabled": false}, time.Minute)
	if err != nil || account.Status >= http.StatusBadRequest {
		t.Fatalf("creating the Editor service account: %v %v", err, account.Body)
	}
	id, _ := account.Body["id"].(float64)
	t.Cleanup(func() {
		_ = admin.Delete(context.WithoutCancel(ctx),
			"/api/serviceaccounts/"+strconv.Itoa(int(id)), time.Minute)
	})
	minted, err := admin.Do(ctx, http.MethodPost,
		"/api/serviceaccounts/"+strconv.Itoa(int(id))+"/tokens",
		map[string]any{"name": name}, time.Minute)
	if err != nil || minted.Status >= http.StatusBadRequest {
		t.Fatalf("creating its token: %v %v", err, minted.Status)
	}
	key, _ := minted.Body["key"].(string)
	if key == "" {
		t.Fatal("grafana returned an empty service account token")
	}
	return key
}

// publishWith runs -publish-dashboard and returns everything it printed.
func publishWith(ctx context.Context, tb testing.TB, binary, cfg string) (string, error) {
	tb.Helper()
	cmd := exec.CommandContext(ctx, binary, "-config", cfg, "-publish-dashboard")
	cmd.Env = collectorEnviron()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	racereport.Check(tb, out.String())
	return out.String(), err
}

// scopedLogPanel is the type of the published dashboard's failed output panel
// and the uid of the datasource it reads, "" for none.
func scopedLogPanel(ctx context.Context, t *testing.T, admin grafana.Client) (kind, source string) {
	t.Helper()
	res, err := admin.Do(ctx, http.MethodGet, grafana.DashboardPath(scopedDashboard), nil, time.Minute)
	if err != nil || res.Status != http.StatusOK {
		t.Fatalf("reading the published dashboard: %v %d", err, res.Status)
	}
	doc, _ := res.Body["dashboard"].(map[string]any)
	var walk func(any) map[string]any
	walk = func(panels any) map[string]any {
		list, _ := panels.([]any)
		for _, raw := range list {
			panel, _ := raw.(map[string]any)
			if panel["title"] == "Where failure output went" {
				return panel
			}
			if found := walk(panel["panels"]); found != nil {
				return found
			}
		}
		return nil
	}
	panel := walk(doc["panels"])
	if panel == nil {
		t.Fatal("the published dashboard has no failed output panel")
	}
	kind, _ = panel["type"].(string)
	from, _ := panel["datasource"].(map[string]any)
	source, _ = from["uid"].(string)
	return kind, source
}
