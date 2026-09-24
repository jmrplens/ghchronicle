package collect

import (
	"context"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// RepoDetail collects the parts of a repository that GraphQL will hand over in
// a batch, so they cost one point for ten repositories instead of one REST
// call each.
//
// Three surfaces were a call per repository and are now a field in a shared
// query: the language byte counts, the topics, and the rulesets. Measured on
// eighteen repositories: five REST calls per repository became three, and the
// batch itself is one GraphQL point for ten of them, 1,450 nodes and two and a
// half seconds.
//
// Asking what the protections enforce is nearly free, but only at the right
// page size. Measured on 2026-09-10, ten repositories of this account in one
// query, rateLimit.cost:
//
//	the fragment as it was, rulesets(first: 25)                      1
//	+ branchProtectionRules(first: 10) and its fifteen switches      1
//	+ rules, conditions and bypassActors, rulesets(first: 25)        5
//	+ rules, conditions and bypassActors, rulesets(first: 10)        2
//	+ rules, conditions and bypassActors, rulesets(first: 5)         1
//
// The multiplier is the ruleset page, not what hangs off it: the same nested
// selection costs 5, 2 or 1 as that page goes 25, 10 or 5. The audit read
// cost 1 because it measured two and six repositories at a time, where every
// shape still rounds down to 1. Classic branch protection is genuinely free.
// The page is now ten, which is +1 per batch and so +6 an hour for the 52
// repositories of this account against a budget of 5000: the audit's own
// "0 to +2" for this entry. Ten is five times the most any repository here
// carries (two), and nothing above it would be reported at all.
//
// What did not move stayed for a reason. `GET /repos/{r}` carries
// `network_count`, the size of the whole fork tree, which GraphQL does not
// expose at all; `/community/profile` carries the health percentage, which
// exists nowhere else; and the release list carries a download count per asset
// that GraphQL will only return for a handful of releases before refusing the
// query on resource limits.
type RepoDetail struct {
	Repos []Repo
	// Batch is how many repositories go into one query. Zero means ten.
	Batch int
}

const detailFragment = `
fragment detail on Repository {
  nameWithOwner url
  languages(first: 25, orderBy: {field: SIZE, direction: DESC}) {
    edges { size node { name } }
  }
  repositoryTopics(first: 40) { nodes { url topic { name } } }
  branchProtectionRules(first: 10) {
    nodes {
      pattern
      allowsDeletions allowsForcePushes blocksCreations
      dismissesStaleReviews isAdminEnforced
      requiresApprovingReviews requiredApprovingReviewCount
      requiresCodeOwnerReviews requiresCommitSignatures
      requiresConversationResolution requiresLinearHistory
      requiresStatusChecks requiresStrictStatusChecks requiresDeployments
      restrictsPushes restrictsReviewDismissals
      requiredStatusChecks { context }
    }
  }
  rulesets(first: 10) {
    nodes {
      name target enforcement updatedAt
      rules(first: 20) { nodes { type } }
      conditions { refName { include exclude } }
      bypassActors(first: 5) { totalCount nodes { bypassMode } }
    }
  }
}`

// branchProtectionRule is one classic branch protection with every switch it
// carries, which is the half of "what is enforced here" that rulesets cannot
// answer.
//
// RequiredReviews is a pointer because GraphQL answers null, not zero, when
// requiresApprovingReviews is false. Measured on 2026-09-10 against
// jmrplens/phonometry's pyoctaveband-v2 rule, where the audit's own transcript
// prints it as 0. Writing a zero there would read as "zero reviewers
// required", which is a different and real state: four repositories on this
// account do have requiresApprovingReviews true with a count of 0.
type branchProtectionRule struct {
	Pattern                        string `json:"pattern"`
	AllowsDeletions                bool   `json:"allowsDeletions"`
	AllowsForcePushes              bool   `json:"allowsForcePushes"`
	BlocksCreations                bool   `json:"blocksCreations"`
	DismissesStaleReviews          bool   `json:"dismissesStaleReviews"`
	IsAdminEnforced                bool   `json:"isAdminEnforced"`
	RequiresApprovingReviews       bool   `json:"requiresApprovingReviews"`
	RequiresCodeOwnerReviews       bool   `json:"requiresCodeOwnerReviews"`
	RequiresCommitSignatures       bool   `json:"requiresCommitSignatures"`
	RequiresConversationResolution bool   `json:"requiresConversationResolution"`
	RequiresLinearHistory          bool   `json:"requiresLinearHistory"`
	RequiresStatusChecks           bool   `json:"requiresStatusChecks"`
	RequiresStrictStatusChecks     bool   `json:"requiresStrictStatusChecks"`
	RequiresDeployments            bool   `json:"requiresDeployments"`
	RestrictsPushes                bool   `json:"restrictsPushes"`
	RestrictsReviewDismissals      bool   `json:"restrictsReviewDismissals"`

	RequiredReviews      *int                       `json:"requiredApprovingReviewCount"`
	RequiredStatusChecks []struct{ Context string } `json:"requiredStatusChecks"`
}

// ruleset is one ruleset and what it actually enforces: its rules, the refs it
// applies to, and who is allowed to walk past it.
type ruleset struct {
	Name        string    `json:"name"`
	Target      string    `json:"target"`
	Enforcement string    `json:"enforcement"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Rules       struct {
		Nodes []struct {
			Type string `json:"type"`
		} `json:"nodes"`
	} `json:"rules"`
	Conditions   *rulesetConditions `json:"conditions"`
	BypassActors struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			BypassMode string `json:"bypassMode"`
		} `json:"nodes"`
	} `json:"bypassActors"`
}

// rulesetConditions is the refs a ruleset applies to. A ruleset with no
// condition at all applies to every ref, which is why the pointer is kept.
type rulesetConditions struct {
	RefName *struct {
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
	} `json:"refName"`
}

type repoDetail struct {
	NameWithOwner string `json:"nameWithOwner"`
	URL           string `json:"url"`
	Languages     struct {
		Edges []struct {
			Size int `json:"size"`
			Node struct {
				Name string `json:"name"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"languages"`
	RepositoryTopics struct {
		Nodes []struct {
			URL   string `json:"url"`
			Topic struct {
				Name string `json:"name"`
			} `json:"topic"`
		} `json:"nodes"`
	} `json:"repositoryTopics"`
	BranchProtectionRules struct {
		Nodes []branchProtectionRule `json:"nodes"`
	} `json:"branchProtectionRules"`
	Rulesets struct {
		Nodes []ruleset `json:"nodes"`
	} `json:"rulesets"`
}

func (rd RepoDetail) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	if len(rd.Repos) == 0 {
		return nil, nil
	}
	size := rd.Batch
	if size <= 0 {
		size = 10
	}
	day := now.UTC().Truncate(24 * time.Hour)
	build := func(batch []Repo) string {
		return aliasQuery(batch, func(int) string { return "...detail" }, detailFragment)
	}
	// aliasBatch halves a batch the gateway gave up on and asks one repository
	// at a time when one of them was renamed away or hidden, the same two
	// recoveries every batched read of a repository in this package gets.
	var points []sink.Point
	failed := aliasBatch(ctx, c, rd.Repos, size, build, func(repo Repo, d repoDetail) {
		points = append(points, d.points(repo, now, day)...)
	})
	return points, failed
}

func (d *repoDetail) points(repo Repo, now, day time.Time) []sink.Point {
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point

	// Bytes per language. No competing tool records this: they keep only the
	// dominant language as a label, which cannot show a repository shifting
	// from one language to another over time.
	for _, e := range d.Languages.Edges {
		points = append(points, sink.Point{
			Measurement: "gh_repo_language",
			Tags:        merge(base, map[string]string{"language": e.Node.Name}),
			Fields:      map[string]any{"bytes": e.Size},
			Time:        now,
		})
	}

	// Topics, one point each, so both "how many" and "which" are answerable.
	// The URL is GitHub's own topic page, which is where a reader goes next.
	for _, t := range d.RepositoryTopics.Nodes {
		points = append(points, sink.Point{
			Measurement: "gh_repo_topic",
			Tags:        merge(base, map[string]string{"topic": t.Topic.Name}),
			Fields:      map[string]any{"present": 1, "url": t.URL},
			Time:        now,
		})
	}

	// What each classic protection enforces. gh_repo_policy counts these and
	// stops there, so a protection that is switched on and asks for nothing
	// looks the same as one that blocks force pushes and demands three checks.
	//
	// This is only half the answer and has to stay separate from the other
	// half. defaultBranchRef.refUpdateRule looks like the effective protection
	// and is not: over a sweep of 35 repositories it is non-null exactly where
	// there is a classic rule, and null on gitlab-mcp-server despite two
	// ACTIVE rulesets on refs/heads/main. Re-measured on 2026-09-10: that
	// repository still reports branchProtectionRules.totalCount 0 with both
	// rulesets in force. The join belongs in the panel, not here.
	// Indexed rather than ranged by value: both node types are wide enough
	// that copying one per iteration is what the linter objects to.
	for i := range d.BranchProtectionRules.Nodes {
		r := &d.BranchProtectionRules.Nodes[i]
		points = append(points, sink.Point{
			Measurement: "gh_branch_protection",
			Tags:        merge(base, map[string]string{"pattern": orNone(r.Pattern)}),
			Fields:      r.fields(d.URL),
			Time:        day,
		})
	}

	// Rulesets, and when each was last changed. A 404 on branch protection
	// does not mean unprotected: a repository can be governed entirely by
	// rulesets, which that endpoint knows nothing about.
	for i := range d.Rulesets.Nodes {
		r := &d.Rulesets.Nodes[i]
		points = append(points, sink.Point{
			Measurement: "gh_ruleset",
			Tags: merge(base, map[string]string{
				"ruleset": orNone(r.Name), "target": strings.ToLower(r.Target),
				"enforcement": strings.ToLower(r.Enforcement),
			}),
			Fields: withURL(map[string]any{
				"rulesets": 1, "active": strings.EqualFold(r.Enforcement, "active"),
				"days_since_change": int(now.Sub(r.UpdatedAt).Hours() / 24),
			}, pageURL(d.URL, "settings", "rules")),
			Time: day,
		})
		points = append(points, r.rulePoints(base, day)...)
	}
	return points
}

// fields is what one classic protection enforces, as a row.
func (r *branchProtectionRule) fields(repoURL string) map[string]any {
	f := map[string]any{
		"rules":                            1,
		"allows_deletions":                 r.AllowsDeletions,
		"allows_force_pushes":              r.AllowsForcePushes,
		"blocks_creations":                 r.BlocksCreations,
		"dismisses_stale_reviews":          r.DismissesStaleReviews,
		"admin_enforced":                   r.IsAdminEnforced,
		"requires_approving_reviews":       r.RequiresApprovingReviews,
		"requires_code_owner_reviews":      r.RequiresCodeOwnerReviews,
		"requires_commit_signatures":       r.RequiresCommitSignatures,
		"requires_conversation_resolution": r.RequiresConversationResolution,
		"requires_linear_history":          r.RequiresLinearHistory,
		"requires_status_checks":           r.RequiresStatusChecks,
		"requires_strict_status_checks":    r.RequiresStrictStatusChecks,
		"requires_deployments":             r.RequiresDeployments,
		"restricts_pushes":                 r.RestrictsPushes,
		"restricts_review_dismissals":      r.RestrictsReviewDismissals,
		"required_checks":                  len(r.RequiredStatusChecks),
	}
	setNonEmpty(f, "url", pageURL(repoURL, "settings", "branches"))
	// Absent rather than zero when GitHub says null: see the type's comment.
	if r.RequiredReviews != nil {
		f["required_reviews"] = *r.RequiredReviews
	}
	return f
}

// rulePoints is one row per rule a ruleset carries, which is the only place
// that says what the ruleset does rather than that it exists.
//
// bypass_always is the number of actors that may walk past it unconditionally.
// A ruleset reported ACTIVE with every actor on ALWAYS is a protection that
// protects nobody, and neither the ruleset row nor the branch protection row
// would ever say so.
//
// It is counted over a page of five while bypass_actors is the exact total, so
// bypass_sampled says how many were actually read. Comparing bypass_always
// with bypass_actors is only sound while the two agree; the largest on this
// account is two, but nothing stops a repository having more than five.
func (r *ruleset) rulePoints(base map[string]string, day time.Time) []sink.Point {
	always := 0
	for _, a := range r.BypassActors.Nodes {
		if strings.EqualFold(a.BypassMode, "always") {
			always++
		}
	}
	// The refs a ruleset applies to are the difference between guarding one
	// branch and guarding everything, so the patterns are kept rather than
	// counted: "~ALL" and "refs/heads/main" are both a list of one. The
	// exclusions come with them because "~ALL except refs/heads/main" is the
	// one shape that reads as total coverage and is not. A space joins them
	// safely: a git ref cannot contain one.
	include, exclude := "", ""
	if r.Conditions != nil && r.Conditions.RefName != nil {
		include = strings.Join(r.Conditions.RefName.Include, " ")
		exclude = strings.Join(r.Conditions.RefName.Exclude, " ")
	}
	points := make([]sink.Point, 0, len(r.Rules.Nodes))
	for _, rule := range r.Rules.Nodes {
		points = append(points, sink.Point{
			Measurement: "gh_ruleset_rule",
			Tags: merge(base, map[string]string{
				"ruleset": orNone(r.Name),
				// Lowercased like the target and enforcement tags on the
				// gh_ruleset row beside it, so one dashboard reads both.
				"rule": strings.ToLower(orNone(rule.Type)),
			}),
			Fields: map[string]any{
				"rules":          1,
				"bypass_actors":  r.BypassActors.TotalCount,
				"bypass_always":  always,
				"bypass_sampled": len(r.BypassActors.Nodes),
				"ref_include":    include,
				"ref_exclude":    exclude,
			},
			Time: day,
		})
	}
	return points
}
