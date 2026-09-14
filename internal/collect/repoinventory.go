package collect

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// RepoInventory collects the three per-repository policy surfaces that are one
// REST call each and change on a scale of months: what the GITHUB_TOKEN of a
// workflow is allowed to do, how old every stored secret is, and whether code
// scanning is switched on by GitHub rather than by a workflow of its own.
//
// All three are settings, not events. They are stamped at the start of the UTC
// day so a re-run converges on the row it already wrote, and so a change shows
// as the day the value moved.
//
// Universe. This runs over the repositories config.yaml sweeps, and that is a
// narrower set than the account. Measured on 2026-09-10: the two oldest
// secrets of the account, both API_TOKEN_GITHUB at 1,971 days, live in
// getANSIfrequencies and FFT2octave, which the sweep does not touch. The
// oldest secret this collector can ever see is therefore not the oldest secret
// there is. Widening the universe is a decision for the owner, not for this
// file.
type RepoInventory struct {
	// Refusals remembers the endpoints a repository refused, which for this
	// family is a 403 on the secret stores of a repository the token does not
	// administer, paid once a day rather than on every sweep. Nil asks every
	// time.
	Refusals *Refusals
}

func (inv RepoInventory) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	day := now.UTC().Truncate(24 * time.Hour)
	var points []sink.Point

	// What a workflow's own token may do by default. Of the six endpoints
	// under /actions/permissions this is the only one that separates
	// repositories at all: the other five were measured constant across the
	// 52 repositories of the account, or answer 422 for a public repository,
	// or 422 for a private one. Read means a workflow cannot push; write
	// means a compromised action can, and the same repositories are the ones
	// whose token can approve a pull request.
	var policy struct {
		DefaultWorkflowPermissions string `json:"default_workflow_permissions"`
		CanApprovePullRequests     bool   `json:"can_approve_pull_request_reviews"`
	}
	if _, _, err := inv.Refusals.GetJSON(ctx, c, "/repos/"+repo.FullName+"/actions/permissions/workflow", &policy, ""); err == nil {
		points = append(points, sink.Point{
			Measurement: "gh_actions_policy",
			Tags:        merge(base, map[string]string{"permissions": orNone(policy.DefaultWorkflowPermissions)}),
			Fields:      map[string]any{"policies": 1, "can_approve_pr": policy.CanApprovePullRequests},
			Time:        day,
		})
	} else if !isSkippable(err) {
		return points, err
	}

	for _, kind := range []string{"actions", "dependabot"} {
		secrets, err := secretsOf(ctx, c, inv.Refusals, repo, kind)
		if err != nil {
			return points, err
		}
		for _, s := range secrets {
			if s.Name == "" {
				continue
			}
			points = append(points, sink.Point{
				Measurement: "gh_secret",
				// The name is public already: it is written in the workflow
				// file that reads it. The value is not, and the API never
				// returns it. Bounded too, at 96 names across the account.
				Tags: merge(base, map[string]string{"kind": kind, "secret": s.Name}),
				Fields: map[string]any{
					"secrets":  1,
					"age_days": int(now.Sub(s.CreatedAt).Hours() / 24),
					// Equal to age_days until somebody rotates the secret,
					// which is the point: the gap between the two is the only
					// evidence a credential was ever replaced.
					"days_since_rotation": int(now.Sub(s.UpdatedAt).Hours() / 24),
				},
				Time: day,
			})
		}
	}

	setup, err := codeScanningSetup(ctx, c, inv.Refusals, repo, base, now, day)
	if err != nil {
		return points, err
	}
	return append(points, setup...), nil
}

type repoSecret struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// secretsOf reads one of the two secret stores of a repository.
//
// Both are asked for on every repository. Pruning dependabot to the
// repositories with dependency updates switched on would halve the calls,
// measured at 51 of 52 repositories holding none, but the flag that says so is
// not on Repo, and a repository that has secrets with the feature off is
// exactly the leftover worth seeing.
//
// One page, and no walk behind it: the longest secret list on this account is
// twelve names (gitlab-mcp-server, measured 2026-09-10), so a hundred is a
// ceiling nothing is near. A repository that did outgrow it would be silently
// short here.
func secretsOf(ctx context.Context, c *ghapi.Client, m *Refusals, repo Repo, kind string) ([]repoSecret, error) {
	var body struct {
		Secrets []repoSecret `json:"secrets"`
	}
	path := "/repos/" + repo.FullName + "/" + kind + "/secrets?per_page=100"
	if _, _, err := m.GetJSON(ctx, c, path, &body, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	return body.Secrets, nil
}

// codeScanningSetup reads whether GitHub's own default code scanning is
// configured, and records the answer even when there is none.
//
// not-configured does not mean "no code scanning". gitlab-mcp-server answers
// not-configured and runs CodeQL from a workflow it wrote itself, which
// gh_code_scanning_analysis sees and this does not. In that state the
// languages list is what GitHub detected in the repository, not what anything
// analyzed, so the field counts a possibility rather than a fact. The two
// measurements only mean something read together.
//
// A 403 is a state as well: measured on 24 of the 52 repositories, with the
// literal message "Code scanning is not enabled for this repository". Writing
// no row for those would make them indistinguishable from a repository the
// sweep never reached.
func codeScanningSetup(ctx context.Context, c *ghapi.Client, m *Refusals, repo Repo, base map[string]string, now, day time.Time) ([]sink.Point, error) {
	var setup struct {
		State      string     `json:"state"`
		Languages  []string   `json:"languages"`
		QuerySuite string     `json:"query_suite"`
		Schedule   string     `json:"schedule"`
		UpdatedAt  *time.Time `json:"updated_at"`
	}
	fields := map[string]any{"setups": 1}
	state := "unavailable"
	if _, _, err := m.GetJSON(ctx, c, "/repos/"+repo.FullName+"/code-scanning/default-setup", &setup, ""); err == nil {
		state = setup.State
		fields["languages"] = len(setup.Languages)
		// updated_at is null whenever the state is not-configured, measured on
		// gitlab-mcp-server and mcp.jmrp.io. Reading it as a zero time would
		// publish an age of about twenty thousand days, which is worse than
		// publishing nothing: it is a number a panel will happily chart.
		if setup.UpdatedAt != nil && !setup.UpdatedAt.IsZero() {
			fields["days_since_change"] = int(now.Sub(*setup.UpdatedAt).Hours() / 24)
		}
	} else if !isSkippable(err) {
		// A rate limit or a broken token arrives here and must not be filed
		// as "this repository has no code scanning".
		return nil, err
	}
	return []sink.Point{{
		Measurement: "gh_code_scanning_setup",
		Tags: merge(base, map[string]string{
			"state": orNone(state),
			// Both are absent on a 403 and schedule is null on
			// not-configured, so both carry a fallback. A tag written on some
			// rows and not others gives one measurement two Graphite depths.
			"query_suite": orNone(setup.QuerySuite),
			"schedule":    orNone(setup.Schedule),
		}),
		Fields: fields,
		Time:   day,
	}}, nil
}
