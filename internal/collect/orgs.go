package collect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Orgs reads what each organization in targets.orgs says about itself: its
// people, its plan and the machinery its owners set up, which no repository
// family sees. One row per organization, gh_org, stamped now, since all of it
// is a current state.
//
// Much of it answers an owner alone. A member is refused the outside
// collaborators, the invitations, the runners, the secrets, the variables and
// the cache usage (403), and Copilot, the hooks and the installations (404),
// measured on conda-forge on 2026-11-21, where this account is a member,
// while the members, the teams, the projects and the custom properties
// answered; on kleidos-dev, which it owns, every part answered. A refused part leaves its fields out
// rather than writing a zero that would read as none. Every part answered 304
// to If-None-Match there, so a pass over an organization that has not moved
// costs nothing.
type Orgs struct {
	Orgs []string
}

// orgProfile is the organization as GET /orgs/{org} returns it.
type orgProfile struct {
	Login             string `json:"login"`
	HTMLURL           string `json:"html_url"`
	PublicRepos       int    `json:"public_repos"`
	TotalPrivateRepos *int   `json:"total_private_repos"`
	Followers         int    `json:"followers"`
	TwoFactorRequired *bool  `json:"two_factor_requirement_enabled"`
	// Plan is a member's to read too (conda-forge answered "team"), and
	// null to an outsider.
	Plan *struct {
		Name string `json:"name"`
	} `json:"plan"`
}

// orgCount is one count of an organization's: the field it is written as and
// the list or object that answers it.
type orgCount struct {
	field string
	path  string
	// total names the object's count field where the answer is an object
	// carrying one, as the Actions lists do; empty means a plain list, counted
	// by walking it.
	total string
}

// orgCounts are the counts every organization is asked for, in the order the
// fields are documented.
func orgCounts(org string) []orgCount {
	base := "/orgs/" + org
	return []orgCount{
		{field: "members", path: base + "/members"},
		{field: "admins", path: base + "/members?role=admin"},
		{field: "teams", path: base + "/teams"},
		{field: "outside_collaborators", path: base + "/outside_collaborators"},
		{field: "pending_invitations", path: base + "/invitations"},
		{field: "projects", path: base + "/projectsV2"},
		{field: "custom_properties", path: base + "/properties/schema"},
		{field: "hooks", path: base + "/hooks"},
		{field: "installations", path: base + "/installations", total: "total_count"},
		{field: "runners", path: base + "/actions/runners", total: "total_count"},
		{field: "secrets", path: base + "/actions/secrets", total: "total_count"},
		{field: "variables", path: base + "/actions/variables", total: "total_count"},
	}
}

// Collect renders one row per organization.
func (o Orgs) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	points := make([]sink.Point, 0, len(o.Orgs))
	for _, org := range o.Orgs {
		p, found, err := orgPoint(ctx, c, org, now)
		if err != nil {
			return points, fmt.Errorf("organization %s: %w", org, err)
		}
		if found {
			points = append(points, p)
		}
	}
	return points, nil
}

// orgPoint is one organization's row, and whether the organization answered
// at all.
func orgPoint(ctx context.Context, c *ghapi.Client, org string, now time.Time) (sink.Point, bool, error) {
	var prof orgProfile
	if _, _, err := c.GetJSON(ctx, "/orgs/"+org, &prof, ""); err != nil {
		if isSkippable(err) {
			return sink.Point{}, false, nil
		}
		return sink.Point{}, false, err
	}
	fields := withURL(map[string]any{
		"public_repos": prof.PublicRepos,
		"followers":    prof.Followers,
	}, prof.HTMLURL)
	if prof.TotalPrivateRepos != nil {
		fields["private_repos"] = *prof.TotalPrivateRepos
	}
	if prof.TwoFactorRequired != nil {
		fields["two_factor_required"] = boolInt(*prof.TwoFactorRequired)
	}
	if prof.Plan != nil {
		setNonEmpty(fields, "plan", prof.Plan.Name)
	}
	for _, oc := range orgCounts(org) {
		n, ok, err := orgCountOf(ctx, c, oc)
		if err != nil {
			return sink.Point{}, false, err
		}
		if ok {
			fields[oc.field] = n
		}
	}
	if err := orgCopilot(ctx, c, org, fields); err != nil {
		return sink.Point{}, false, err
	}
	if err := orgCache(ctx, c, org, fields); err != nil {
		return sink.Point{}, false, err
	}
	return sink.Point{
		Measurement: "gh_org",
		Tags:        map[string]string{"org": org},
		Fields:      fields,
		Time:        now,
	}, true, nil
}

// orgCountOf answers one count, and whether the organization let the token
// read it at all.
func orgCountOf(ctx context.Context, c *ghapi.Client, oc orgCount) (n int, ok bool, err error) {
	sep := "?"
	if strings.Contains(oc.path, "?") {
		sep = "&"
	}
	if oc.total != "" {
		var res map[string]any
		if _, _, getErr := c.GetJSON(ctx, oc.path+sep+"per_page=1", &res, ""); getErr != nil {
			if isSkippable(getErr) {
				return 0, false, nil
			}
			return 0, false, getErr
		}
		v, _ := res[oc.total].(float64)
		return int(v), true, nil
	}
	// A list has no count of its own, so it is walked, a page of a hundred
	// at a time. An organization the size where that is slow belongs in
	// targets.repos by name, not in targets.orgs.
	for page := 1; ; page++ {
		var rows []struct{}
		path := fmt.Sprintf("%s%sper_page=100&page=%d", oc.path, sep, page)
		if _, _, getErr := c.GetJSON(ctx, path, &rows, ""); getErr != nil {
			if isSkippable(getErr) {
				return 0, false, nil
			}
			return 0, false, getErr
		}
		n += len(rows)
		if len(rows) < 100 {
			return n, true, nil
		}
	}
}

// orgCopilot writes the organization's Copilot seats: how many it holds and
// how many were used this cycle. Business and Enterprise plans have a seat
// breakdown; an organization without Copilot answers 404, and a member 403.
func orgCopilot(ctx context.Context, c *ghapi.Client, org string, fields map[string]any) error {
	var res struct {
		PlanType      string `json:"plan_type"`
		SeatBreakdown *struct {
			Total             int `json:"total"`
			ActiveThisCycle   int `json:"active_this_cycle"`
			PendingInvitation int `json:"pending_invitation"`
		} `json:"seat_breakdown"`
	}
	if _, _, err := c.GetJSON(ctx, "/orgs/"+org+"/copilot/billing", &res, ""); err != nil {
		if isSkippable(err) {
			return nil
		}
		return err
	}
	if res.SeatBreakdown != nil {
		fields["copilot_seats"] = res.SeatBreakdown.Total
		fields["copilot_active"] = res.SeatBreakdown.ActiveThisCycle
	}
	return nil
}

// orgCache writes what the organization's repositories hold in the Actions
// cache, all of them together: the per-repository rows are gh_actions_cache.
func orgCache(ctx context.Context, c *ghapi.Client, org string, fields map[string]any) error {
	var res struct {
		Count int64 `json:"total_active_caches_count"`
		Bytes int64 `json:"total_active_caches_size_in_bytes"`
	}
	if _, _, err := c.GetJSON(ctx, "/orgs/"+org+"/actions/cache/usage", &res, ""); err != nil {
		if isSkippable(err) {
			return nil
		}
		return err
	}
	fields["actions_caches"] = res.Count
	fields["actions_cache_bytes"] = res.Bytes
	return nil
}
