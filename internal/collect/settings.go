package collect

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Settings collects the configuration that changes over time, and how well the
// parts of it that talk to the outside world are working.
//
// Most of a repository's settings are a snapshot nobody needs charted. Webhook
// deliveries are not: a real time series with a status code and a latency,
// and they fail silently: on the account this was built against, one hook had
// been answering 403 for seventy-eight of its last hundred deliveries and
// nothing anywhere said so. The other changelog a repository keeps, the
// versions of its rulesets, is RulesetHistory's.
type Settings struct {
	// Deliveries is how many recent webhook deliveries to read per hook.
	Deliveries int
}

func (s Settings) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point
	for _, read := range []func(context.Context, *ghapi.Client, Repo, map[string]string, time.Time) ([]sink.Point, error){
		s.webhookPoints, environmentPoints, deployKeyPoints,
	} {
		pts, err := read(ctx, c, repo, base, now)
		// Kept before the error is looked at: the webhook read hands back
		// the hooks and deliveries it had when a later delivery list failed.
		points = append(points, pts...)
		if err != nil && !isSkippable(err) {
			return points, err
		}
	}
	return points, nil
}

// webhookPoints reads the webhooks, and how their deliveries are going.
func (s Settings) webhookPoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	deliveries := s.Deliveries
	if deliveries <= 0 {
		deliveries = 30
	}
	day := now.UTC().Truncate(24 * time.Hour)
	var points []sink.Point
	var hooks []struct {
		ID     int64    `json:"id"`
		Name   string   `json:"name"`
		Active bool     `json:"active"`
		Events []string `json:"events"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/hooks?per_page=100", &hooks, ""); err != nil {
		if !isSkippable(err) {
			return points, err
		}
	}
	for _, h := range hooks {
		// The id, not the name: `name` is "web" on every webhook GitHub has,
		// measured on 28 hooks and 340 deliveries, so as the tag it told two
		// hooks of one repository apart only by their host, and two aimed at
		// the same host were one series.
		hook := strconv.FormatInt(h.ID, 10)
		points = append(points, sink.Point{
			Measurement: "gh_webhook",
			Tags: merge(base, map[string]string{
				"hook": hook, "active": boolTag(h.Active),
				"host": hostOf(h.Config.URL),
			}),
			Fields: map[string]any{"events": len(h.Events), "hooks": 1},
			Time:   day,
		})
		var del []struct {
			ID          int64     `json:"id"`
			DeliveredAt time.Time `json:"delivered_at"`
			Redelivery  bool      `json:"redelivery"`
			Duration    float64   `json:"duration"`
			Status      string    `json:"status"`
			StatusCode  int       `json:"status_code"`
			Event       string    `json:"event"`
			Action      string    `json:"action"`
		}
		dp := fmt.Sprintf("/repos/%s/hooks/%d/deliveries?per_page=%d", repo.FullName, h.ID, deliveries)
		if _, _, err := c.GetJSON(ctx, dp, &del, ""); err != nil {
			if isSkippable(err) {
				continue
			}
			return points, err
		}
		for _, d := range del {
			points = append(points, sink.Point{
				Measurement: "gh_webhook_delivery",
				Tags: merge(base, map[string]string{
					"hook": hook, "host": hostOf(h.Config.URL),
					"event": d.Event, "status": d.Status,
					"code": strconv.Itoa(d.StatusCode),
					"ok":   boolTag(d.StatusCode >= 200 && d.StatusCode < 300),
				}),
				Fields: map[string]any{
					"deliveries": 1, "duration_seconds": d.Duration,
					"redelivery": d.Redelivery,
				},
				Time: d.DeliveredAt,
			})
		}
	}
	return points, nil
}

// environmentPoints reads the environments, which are a standing grant of
// access and worth seeing go stale.
func environmentPoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	var envs struct {
		TotalCount   int `json:"total_count"`
		Environments []struct {
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
			HTMLURL   string    `json:"html_url"`
			// An admin can deploy past every rule below unless this is off.
			CanAdminsBypass bool `json:"can_admins_bypass"`
			// Reviewers, wait timers and branch policies all arrive here.
			// The count is what says whether an environment guards anything
			// at all; the dedicated subendpoints repeat it for two requests
			// per environment, which is why they are not called.
			ProtectionRules []struct {
				Type string `json:"type"`
			} `json:"protection_rules"`
			BranchPolicy *struct {
				ProtectedBranches    bool `json:"protected_branches"`
				CustomBranchPolicies bool `json:"custom_branch_policies"`
			} `json:"deployment_branch_policy"`
		} `json:"environments"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/environments", &envs, ""); err != nil {
		return nil, err
	}
	day := now.UTC().Truncate(24 * time.Hour)
	points := make([]sink.Point, 0, len(envs.Environments))
	for _, e := range envs.Environments {
		// A null deployment_branch_policy means no branch restriction at
		// all, which is a different fact from one that restricts to
		// protected branches. Both of its booleans are false in that
		// case, so has_branch_policy is what tells them apart.
		protected, custom := false, false
		if e.BranchPolicy != nil {
			protected, custom = e.BranchPolicy.ProtectedBranches, e.BranchPolicy.CustomBranchPolicies
		}
		points = append(points, sink.Point{
			Measurement: "gh_environment",
			Tags:        merge(base, map[string]string{"environment": e.Name}),
			Fields: map[string]any{
				"environments":      1,
				"days_since_change": int(now.Sub(e.UpdatedAt).Hours() / 24),
				"age_days":          int(now.Sub(e.CreatedAt).Hours() / 24),
				"url":               e.HTMLURL,
				// An environment holding a token, with no protection rule
				// and no branch policy, is a secret store any branch can
				// deploy to. These five fields are what say so.
				"protection_rules":       len(e.ProtectionRules),
				"has_branch_policy":      e.BranchPolicy != nil,
				"protected_branches":     protected,
				"custom_branch_policies": custom,
				"can_admins_bypass":      e.CanAdminsBypass,
			},
			Time: day,
		})
	}
	return points, nil
}

// deployKeyPoints reads the deploy keys, the other standing grant of access.
func deployKeyPoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	var keys []struct {
		Title    string     `json:"title"`
		ReadOnly bool       `json:"read_only"`
		LastUsed *time.Time `json:"last_used"`
		AddedBy  string     `json:"added_by"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/keys?per_page=100", &keys, ""); err != nil {
		return nil, err
	}
	day := now.UTC().Truncate(24 * time.Hour)
	points := make([]sink.Point, 0, len(keys))
	for _, k := range keys {
		// `read_only` is a tag, so it must not also be a field: InfluxDB
		// rejects a line that uses one name for both, and rejects the
		// whole batch with it.
		f := map[string]any{"keys": 1}
		// A write key nobody has used in a year is a credential to remove,
		// and this is the only place that says so.
		if k.LastUsed != nil {
			f["days_since_use"] = int(now.Sub(*k.LastUsed).Hours() / 24)
		}
		points = append(points, sink.Point{
			Measurement: "gh_deploy_key",
			Tags:        merge(base, map[string]string{"key": k.Title, "read_only": boolTag(k.ReadOnly)}),
			Fields:      f,
			Time:        day,
		})
	}
	return points, nil
}

// hostOf keeps the host of a webhook URL and drops the rest, which usually
// carries a secret in the path.
func hostOf(raw string) string {
	s := raw
	for _, prefix := range []string{"https://", "http://"} {
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			s = s[len(prefix):]
			break
		}
	}
	if i := indexOf(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}
