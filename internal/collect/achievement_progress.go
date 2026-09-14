package collect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// The progress half of the achievements family: how far the account is from
// the next tier of each badge that has tiers, as gh_achievement_progress.
//
// GitHub publishes neither the rule a badge is earned by nor the count it
// has reached; the profile page shows the tier and nothing more. What is
// known comes from the community list at
// github.com/Schweinepriester/github-profile-achievements, whose README
// records the thresholds people have observed, and from the owner's own
// measurement of one of them. So the count is recomputed here from the API,
// the tier that count implies is compared with the tier the page shows, and
// a row where the two disagree says so rather than drawing a bar from a rule
// the page contradicts.

// achievementRule is one badge with tiers: the count that decides it and the
// counts at which each tier begins.
type achievementRule struct {
	Slug string
	// Name is the display name the page gives the badge, used when the
	// badge is not on the page yet and there is no card to read it from.
	Name string
	// Thresholds are the counts at which tiers 1 to 4 begin: the badge
	// itself, then bronze (x2), silver (x3) and gold (x4).
	Thresholds [4]int
}

// achievementRules are the badges with tiers and their thresholds, in the
// order their rows are written.
//
// Source: the Tiers table of the README at
// https://github.com/Schweinepriester/github-profile-achievements, read on
// 2026-09-12. The first threshold of each is the Achievements table of the
// same README. Pair Extraordinaire was cross-checked the same day against
// the owner's own count over the public merged pull requests of the
// account: the owner's script read 27 with a co-authored commit, this walk
// 29 (the script's June to August range held 1,118 results and search stops
// at a thousand), and the page showed silver (x3), which is 24 to 47 in the
// list either way.
//
// The single-tier badges (YOLO, Quickdraw, Public Sponsor, the two archive
// contributor badges) and the two GitHub is still testing (Heart On Your
// Sleeve, Open Sourcerer) are not here: there is no next tier to measure
// against, or no rule the page has been seen to follow, so they are present
// on the shelf or not and nothing more.
var achievementRules = []achievementRule{
	{Slug: "pull-shark", Name: "Pull Shark", Thresholds: [4]int{2, 16, 128, 1024}},
	{Slug: "galaxy-brain", Name: "Galaxy Brain", Thresholds: [4]int{2, 8, 16, 32}},
	{Slug: "starstruck", Name: "Starstruck", Thresholds: [4]int{16, 128, 512, 4096}},
	{Slug: "pair-extraordinaire", Name: "Pair Extraordinaire", Thresholds: [4]int{1, 10, 24, 48}},
}

// tierOf is the tier a count implies under a rule, 0 below the first
// threshold, and the threshold of the next tier, 0 at the top.
func tierOf(count int, thresholds [4]int) (tier, next int) {
	for i, at := range thresholds {
		if count < at {
			return i, at
		}
		tier = i + 1
	}
	return tier, 0
}

// percentOf is how far a count is to the next threshold, 0 to 100 with one
// decimal, and 100 once there is no next tier.
func percentOf(count, next int) float64 {
	if next <= 0 {
		return 100
	}
	return math.Round(float64(count)*1000/float64(next)) / 10
}

// achievementCounts are the counts that decide the four tiered badges, as
// the API reports them today.
type achievementCounts struct {
	// PullsMerged is every merged pull request the account authored, in any
	// repository: the Pull Shark count. It is the same search the totals
	// family stores as gh_account_total.pulls_merged, asked again here for
	// one point rather than read back from a store the collector does not
	// have.
	PullsMerged int
	// Answers is the discussions whose accepted answer the account wrote,
	// anywhere: the Galaxy Brain count, one discussion search.
	Answers int
	// TopStars is the stars on the account's most starred repository of its
	// own, forks left out: the Starstruck count.
	TopStars int
	// Coauthored is the merged pull requests in public repositories with a
	// co-authored commit in them: the Pair Extraordinaire count, from the
	// walk below.
	Coauthored int
	// CreatedAt is when the account was created, the lower bound of that
	// walk.
	CreatedAt time.Time
}

// achievementCountsQuery is the three counts that search and one connection
// answer in one query, for one point. The query is named so a fake can tell
// it from the other searches.
const achievementCountsQuery = `
query achievementCounts($login: String!, $pulls: String!, $answers: String!) {
  pulls: search(type: ISSUE, query: $pulls) { issueCount }
  answers: search(type: DISCUSSION, query: $answers) { discussionCount }
  user(login: $login) {
    createdAt
    repositories(first: 1, ownerAffiliations: OWNER, isFork: false, orderBy: {field: STARGAZERS, direction: DESC}) {
      nodes { nameWithOwner stargazerCount }
    }
  }
}`

// counts asks for the three cheap counts and the account's creation date.
func (a Achievements) counts(ctx context.Context, c *ghapi.Client) (achievementCounts, error) {
	var res struct {
		Pulls struct {
			IssueCount int `json:"issueCount"`
		} `json:"pulls"`
		Answers struct {
			DiscussionCount int `json:"discussionCount"`
		} `json:"answers"`
		User *struct {
			CreatedAt    time.Time `json:"createdAt"`
			Repositories struct {
				Nodes []struct {
					Stars int `json:"stargazerCount"`
				} `json:"nodes"`
			} `json:"repositories"`
		} `json:"user"`
	}
	vars := map[string]any{
		"login":   a.Login,
		"pulls":   "type:pr author:" + a.Login + " is:merged",
		"answers": "answered-by:" + a.Login,
	}
	if err := c.GraphQL(ctx, achievementCountsQuery, vars, &res); err != nil {
		return achievementCounts{}, err
	}
	if res.User == nil {
		return achievementCounts{}, fmt.Errorf("achievement counts: no user %q in the answer", a.Login)
	}
	out := achievementCounts{
		PullsMerged: res.Pulls.IssueCount,
		Answers:     res.Answers.DiscussionCount,
		CreatedAt:   res.User.CreatedAt,
	}
	if len(res.User.Repositories.Nodes) > 0 {
		out.TopStars = res.User.Repositories.Nodes[0].Stars
	}
	return out, nil
}

// coauthoredPullsQuery is one page of the merged pull requests of the
// account in public repositories, with every commit message the badge could
// be reading: the commits of the pull request and the merge commit, which
// is where a squash merge keeps the trailers of the commits it folded.
//
// Measured on 2026-09-12 against the live API: a page of a hundred with
// their first hundred commits each costs one point, the same as a page of
// fifty, because the gateway prices a search by its own page and not by the
// commit connections under it.
const coauthoredPullsQuery = `
query coauthoredPulls($query: String!, $first: Int!, $after: String) {
  search(type: ISSUE, query: $query, first: $first, after: $after) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        mergeCommit { message }
        commits(first: 100) { totalCount nodes { commit { message } } }
      }
    }
  }
}`

// coauthoredBy is the trailer GitHub reads a co-author from, at the start of
// a line of the commit message, in any case.
var coauthoredBy = regexp.MustCompile(`(?im)^co-authored-by:`)

// searchCap is how many results one search will page through before GitHub
// refuses the next page, whatever issueCount says.
const searchCap = 1000

// coauthoredWalk counts the merged pull requests with a co-authored commit,
// the way the owner's own measurement does: public repositories only, since
// twenty-five such pull requests in a private repository moved nothing on
// 2026-09-12, and one pull request counts once however many of its commits
// carry the trailer.
//
// Search stops at a thousand results, so the walk is over ranges of merge
// dates: the whole life of the account first, and a range that holds more
// than a thousand is split in two by date and each half asked again. Each
// page and each split costs one point; measured on 2026-09-12 over the
// 1,767 public merged pull requests of an account created in 2017, the
// walk was 31 queries in 68 seconds, 32 points for the day with the counts
// query beside it.
type coauthoredWalk struct {
	login string
	// pulls is the count so far, and queries what the walk has cost.
	pulls, queries int
	// capped is set when one day alone held more than a thousand merged
	// pull requests, which no range can split further: the count is then a
	// floor.
	capped bool
	// truncated is how many pull requests had more commits than one page
	// carries and no trailer in the ones read, nor in their merge commit: a
	// trailer past the hundredth commit is not seen, and the count is a
	// floor by that many at most.
	truncated int
}

// oneDay is the granularity of a merged: range.
const oneDay = 24 * time.Hour

// coauthoredPage is one answer of the walk.
type coauthoredPage struct {
	Search struct {
		IssueCount int `json:"issueCount"`
		PageInfo   struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []struct {
			MergeCommit *struct {
				Message string `json:"message"`
			} `json:"mergeCommit"`
			Commits struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					Commit struct {
						Message string `json:"message"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"nodes"`
	} `json:"search"`
}

// walk counts the pull requests merged between from and to, both days
// included, splitting the range when the search will not page it whole.
func (w *coauthoredWalk) walk(ctx context.Context, c *ghapi.Client, from, to time.Time) error {
	first, after := 100, ""
	got := 0
	for {
		res, err := w.page(ctx, c, from, to, first, after)
		if err != nil {
			if _, tooLarge := errors.AsType[*ghapi.TooLargeError](err); tooLarge && first > 10 {
				// Same cursor, half the page: a hundred pull requests with
				// a hundred commits each is more than the gateway finishes
				// for a busy range.
				first /= 2
				continue
			}
			return err
		}
		if after == "" && res.Search.IssueCount > searchCap {
			if days := int(to.Sub(from) / oneDay); days >= 1 {
				mid := from.Add(time.Duration(days/2) * oneDay)
				if left := w.walk(ctx, c, from, mid); left != nil {
					return left
				}
				return w.walk(ctx, c, mid.Add(oneDay), to)
			}
			w.capped = true
		}
		w.count(res)
		got += len(res.Search.Nodes)
		if !res.Search.PageInfo.HasNextPage || got >= searchCap {
			return nil
		}
		after = res.Search.PageInfo.EndCursor
	}
}

// page asks for one page of the range.
func (w *coauthoredWalk) page(ctx context.Context, c *ghapi.Client, from, to time.Time, first int, after string) (*coauthoredPage, error) {
	vars := map[string]any{
		"query": fmt.Sprintf("is:pr is:merged is:public author:%s merged:%s..%s",
			w.login, from.Format("2006-01-02"), to.Format("2006-01-02")),
		"first": first,
	}
	if after != "" {
		vars["after"] = after
	}
	w.queries++
	var res coauthoredPage
	if err := c.GraphQL(ctx, coauthoredPullsQuery, vars, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// count reads a page: a pull request counts once when its merge commit or
// any commit read carries the trailer.
func (w *coauthoredWalk) count(res *coauthoredPage) {
	for i := range res.Search.Nodes {
		n := &res.Search.Nodes[i]
		found := n.MergeCommit != nil && coauthoredBy.MatchString(n.MergeCommit.Message)
		for j := range n.Commits.Nodes {
			if found {
				break
			}
			found = coauthoredBy.MatchString(n.Commits.Nodes[j].Commit.Message)
		}
		switch {
		case found:
			w.pulls++
		case n.Commits.TotalCount > len(n.Commits.Nodes):
			w.truncated++
		}
	}
}

// progressPoints is the row per tiered badge: the count, the tier it
// implies, beside the tier the page shows and whether the two agree, and
// when they do, the next threshold and how far along. Every rule gets a row whether or not the
// badge is on the page yet, since how far an account is from a badge it does
// not have is the same question. Stamped at the start of the day like the
// badges, because the counts are true today and not at any instant.
func (a Achievements) progressPoints(earned []achievement, counts achievementCounts, day time.Time) []sink.Point {
	onPage := map[string]achievement{}
	for _, e := range earned {
		onPage[e.Slug] = e
	}
	var points []sink.Point
	for _, rule := range achievementRules {
		count := rule.count(counts)
		tier, next := tierOf(count, rule.Thresholds)
		card, present := onPage[rule.Slug]
		name := rule.Name
		if present {
			name = card.Name
		}
		// tier_number rather than tier, for the reason gh_achievement gives:
		// tier is a string on the sponsorship measurements, and a field
		// name is mapped once across every index Elasticsearch searches.
		agrees := tier == card.Tier
		fields := map[string]any{
			"name": name, "count": count, "tier_number": tier,
			"page_tier": card.Tier, "agrees": boolInt(agrees),
		}
		// The target and the bar come from the rule, and a page that shows
		// another tier is a rule the page contradicts: the row then says the
		// count and both tiers and no more, rather than a percentage of a
		// threshold the badge is not following.
		if agrees {
			fields["next_threshold"] = next
			fields["percent"] = percentOf(count, next)
		}
		setNonEmpty(fields, "url", a.pageURL(rule.Slug))
		setNonEmpty(fields, "image", card.Image)
		points = append(points, sink.Point{
			Measurement: "gh_achievement_progress",
			Tags:        map[string]string{"user": a.Login, "achievement": rule.Slug},
			Fields:      fields,
			Time:        day,
		})
	}
	return points
}

// count picks the count a rule is decided by.
func (r achievementRule) count(c achievementCounts) int {
	switch r.Slug {
	case "pull-shark":
		return c.PullsMerged
	case "galaxy-brain":
		return c.Answers
	case "starstruck":
		return c.TopStars
	case "pair-extraordinaire":
		return c.Coauthored
	}
	return 0
}

// Disagreements are the rows whose implied tier is not the page's, as
// "slug: count N implies tier X, page shows Y", for the runner to log once.
func Disagreements(points []sink.Point) []string {
	var out []string
	for _, p := range points {
		if p.Measurement != "gh_achievement_progress" || p.Fields["agrees"] != 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s: count %v implies tier %v, page shows %v",
			p.Tags["achievement"], p.Fields["count"], p.Fields["tier_number"], p.Fields["page_tier"]))
	}
	return out
}
