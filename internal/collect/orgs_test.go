package collect

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// orgFixture answers two organizations: acme, which the token owns and where
// every part answers, and guild, where it is a member and refused what an
// owner alone may read, with the codes conda-forge answered on 2026-11-21.
func orgFixture(t *testing.T) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.handle("/orgs/acme", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"acme","html_url":"https://github.com/acme","public_repos":4,` +
			`"total_private_repos":4,"followers":1,"two_factor_requirement_enabled":true,` +
			`"plan":{"name":"free","seats":0,"filled_seats":1}}`))
	})
	f.handle("/orgs/guild", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"guild","html_url":"https://github.com/guild","public_repos":30214,` +
			`"followers":9,"plan":{"name":"team","seats":0,"filled_seats":0}}`))
	})
	list := func(n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			size := n
			if n > 100 {
				size = 100
				if page == "2" {
					size = n - 100
				}
			}
			_, _ = w.Write([]byte("[" + strings.TrimSuffix(strings.Repeat("{},", size), ",") + "]"))
		}
	}
	total := func(n int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"total_count":%d}`, n)
		}
	}
	refuse := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"refused"}`))
		}
	}
	// members answers role=admin with one and the whole list with 130, two
	// pages, so the walk past a full page is exercised.
	f.handle("/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("role") == "admin" {
			list(1)(w, r)
			return
		}
		list(130)(w, r)
	})
	for path, h := range map[string]http.HandlerFunc{
		"/orgs/acme/teams": list(2), "/orgs/acme/outside_collaborators": list(0),
		"/orgs/acme/invitations": list(1), "/orgs/acme/projectsV2": list(0),
		"/orgs/acme/properties/schema": list(0), "/orgs/acme/hooks": list(0),
		"/orgs/acme/installations": total(1), "/orgs/acme/actions/runners": total(0),
		"/orgs/acme/actions/secrets": total(3), "/orgs/acme/actions/variables": total(2),
		"/orgs/guild/members": list(30), "/orgs/guild/teams": list(30),
		"/orgs/guild/projectsV2": list(2), "/orgs/guild/properties/schema": list(0),
		"/orgs/guild/outside_collaborators": refuse(403), "/orgs/guild/invitations": refuse(403),
		"/orgs/guild/actions/runners": refuse(403), "/orgs/guild/actions/secrets": refuse(403),
		"/orgs/guild/actions/variables": refuse(403), "/orgs/guild/actions/cache/usage": refuse(403),
		"/orgs/guild/hooks": refuse(404), "/orgs/guild/installations": refuse(404),
		"/orgs/guild/copilot/billing": refuse(404),
	} {
		f.handle(path, h)
	}
	f.handle("/orgs/acme/copilot/billing", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"business","seat_breakdown":{"total":2,"active_this_cycle":1}}`))
	})
	f.handle("/orgs/acme/actions/cache/usage", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_active_caches_count":5,"total_active_caches_size_in_bytes":1048576}`))
	})
	return f
}

// TestAnOrganizationIsReadWhole: an owner's organization writes every count,
// and a member's leaves out what it was refused rather than writing a zero
// that would read as none.
func TestAnOrganizationIsReadWhole(t *testing.T) {
	t.Parallel()
	f := orgFixture(t)
	points, err := Orgs{Orgs: []string{"acme", "guild"}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 2 {
		t.Fatalf("got %d rows, want one per organization", len(points))
	}
	acme := find(t, points, "gh_org", map[string]string{"org": "acme"})
	if !acme.Time.Equal(testNow) {
		t.Errorf("stamped %s, want now: an organization is a current state", acme.Time)
	}
	for field, want := range map[string]int64{
		"members": 130, "admins": 1, "teams": 2, "outside_collaborators": 0,
		"pending_invitations": 1, "projects": 0, "hooks": 0, "installations": 1,
		"runners": 0, "secrets": 3, "variables": 2, "copilot_seats": 2, "copilot_active": 1,
		"public_repos": 4, "private_repos": 4, "followers": 1, "two_factor_required": 1,
		"actions_caches": 5, "actions_cache_bytes": 1048576,
	} {
		if got := fieldInt(t, acme, field); got != want {
			t.Errorf("acme %s = %d, want %d", field, got, want)
		}
	}
	if acme.Fields["plan"] != "free" || acme.Fields["url"] != "https://github.com/acme" {
		t.Errorf("acme plan %v url %v", acme.Fields["plan"], acme.Fields["url"])
	}
	guild := find(t, points, "gh_org", map[string]string{"org": "guild"})
	if fieldInt(t, guild, "members") != 30 || fieldInt(t, guild, "teams") != 30 {
		t.Errorf("guild = %v, want what a member may read", guild.Fields)
	}
	for _, refused := range []string{
		"outside_collaborators", "pending_invitations", "runners", "secrets", "variables",
		"hooks", "installations", "copilot_seats", "copilot_active", "actions_caches",
		"private_repos", "two_factor_required",
	} {
		if v, ok := guild.Fields[refused]; ok {
			t.Errorf("guild writes %s = %v, a part it was refused", refused, v)
		}
	}
}

// TestAnOrganizationThatDoesNotAnswerWritesNothing: an organization renamed
// or gone is a 404 on its own page, nothing to write and not a failure that
// would stop the next one.
func TestAnOrganizationThatDoesNotAnswerWritesNothing(t *testing.T) {
	t.Parallel()
	f := orgFixture(t)
	f.handle("/orgs/gone", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	points, err := Orgs{Orgs: []string{"gone", "acme"}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Tags["org"] != "acme" {
		t.Errorf("got %v, want acme's row alone", points)
	}
}
