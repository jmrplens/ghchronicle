package collect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
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

// achievementUser is the account half of achievementCountsQuery: when it was
// created, which bounds the walk, and the star counts the Starstruck tiers are
// read from.
type achievementUser struct {
	CreatedAt    time.Time `json:"createdAt"`
	Repositories struct {
		Nodes []struct {
			Stars int `json:"stargazerCount"`
		} `json:"nodes"`
	} `json:"repositories"`
}

// counts asks for the three cheap counts and the account's creation date.
func (a Achievements) counts(ctx context.Context, c *ghapi.Client) (achievementCounts, error) {
	var res struct {
		Pulls struct {
			IssueCount int `json:"issueCount"`
		} `json:"pulls"`
		Answers struct {
			DiscussionCount int `json:"discussionCount"`
		} `json:"answers"`
		User *achievementUser `json:"user"`
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
// is where a squash merge keeps the trailers of the commits it folded, and
// when it merged, which is what tells the days a pass settles from the one
// still being merged into.
//
// Measured on 2026-09-12 against the live API: a page of a hundred with
// their first hundred commits each costs one point, the same as a page of
// fifty, because the gateway prices a search by its own page and not by the
// commit connections under it. The messages are most of the bytes, and the
// other way to see a co-author costs more where it matters: asking each
// commit for the count of its authors, which lists co-authors, answered the
// same page of a hundred in 27 KB where the messages took 968 KB, and cost
// 102 points where they cost one (measured on 2026-09-27). Nor does asking
// for gzip help: the same answer came back uncompressed.
const coauthoredPullsQuery = `
query coauthoredPulls($query: String!, $first: Int!, $after: String) {
  search(type: ISSUE, query: $query, first: $first, after: $after) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        id
        mergedAt
        mergeCommit { message }
        commits(first: 100) { totalCount pageInfo { hasNextPage endCursor } nodes { commit { message } } }
      }
    }
  }
}`

// coauthoredCommitsField is the next hundred commits of one pull request the
// search page left unread, by its node id, aliased so that one query reads
// the next page of several.
const coauthoredCommitsField = `
  p%[1]d: node(id: $id%[1]d) { ... on PullRequest { commits(first: 100, after: $after%[1]d) { pageInfo { hasNextPage endCursor } nodes { commit { message } } } } }`

// coauthoredFollowBatch is how many pull requests one query reads the next
// hundred commits of. A search page of a hundred pull requests with a hundred
// commits each is what the gateway can refuse, which the walk answers by
// halving it; ten is a tenth of that at most, and a refused query leaves only
// its ten as floors.
const coauthoredFollowBatch = 10

// coauthoredRule is the version of the rule the Pair Extraordinaire count is
// made by: which pull requests the walk asks for and what in them it reads
// as a co-author. A tally kept under another version counted something else,
// and adding this rule's days to it would mix the two, so it is walked again
// whole. Raise it with any change to the search, the trailer or the commits
// read. 2 reads every commit of a pull request, where 1 read the first
// hundred and kept a tally whose floor 2 can settle.
const coauthoredRule = 2

// coauthoredWholeEvery is how many days a tally is added to before the whole
// history is walked again. Adding only ever raises the count, and it can go
// down: a repository made private or deleted takes its pull requests out of
// is:public, and only a walk over the whole range sees them gone. A week is
// how long a count that fell is shown too high, against a whole walk that
// transfers what about seventy passes adding one day do, or two days of the
// hourly passes that walk the day in progress (measured below).
const coauthoredWholeEvery = 7

// CoauthoredTally is the Pair Extraordinaire count as far as it is settled,
// kept in the state file so that the next pass walks the pull requests merged
// since instead of the account's whole history.
//
// The whole history, walked every day, was 34 or 35 queries and 18 to 24 MB
// on each daily pass from 2026-09-20 to 2026-09-27, measured on the production
// proxy's log for an account with 2,315 public merged pull requests: 127 of
// the 410 MB the service transferred from the 20th to the 26th, to count one
// number, and growing with each pull request merged. Run live on 2026-09-27
// over the last one, two and four days, a pass was the counts query and one
// page of the walk, 54 KB, 333 KB and 874 KB in two to three seconds, where
// the whole walk was 35 queries, 23.7 MB and 94 seconds. The one-day figure
// is from before 09:05 UTC: the hourly passes the production proxy logged
// later the same day, from 12:44 to 18:43 UTC, walked 468 to 513 KB each,
// since a pass walks the day in progress and the day had merged more.
type CoauthoredTally struct {
	// Count is the co-authored pull requests merged on or before Through.
	Count int `json:"count"`
	// Through is the last UTC day Count covers: the day before the pass that
	// settled it. The day a pass is made on is still being merged into, so
	// its pull requests are in that day's row and not here, and the next
	// pass walks the day again.
	Through time.Time `json:"through"`
	// Truncated and Capped are what of Count is a floor, as coauthoredWalk
	// says it, kept so a pass that adds to a floor still says it is one.
	Truncated int  `json:"truncated,omitempty"`
	Capped    bool `json:"capped,omitempty"`
	// Rule is the coauthoredRule Count was made by.
	Rule int `json:"rule"`
	// WalkedWhole is the UTC day the whole history was last walked.
	WalkedWhole time.Time `json:"walked_whole"`
}

// coauthoredBase is where a pass starts from: the tally an earlier pass left
// and the day after the last it covers, or nothing and the account's first
// day when the whole history is due.
func (a Achievements) coauthoredBase(created, day time.Time) (base CoauthoredTally, from time.Time) {
	if a.Coauthored != nil && !a.Whole {
		t := *a.Coauthored
		// A Through on or after today is a clock that went back; the tally
		// cannot say which of its days are still to come.
		if t.Rule == coauthoredRule && !t.Through.IsZero() && t.Through.Before(day) &&
			day.Before(t.WalkedWhole.AddDate(0, 0, coauthoredWholeEvery)) {
			return t, t.Through.AddDate(0, 0, 1)
		}
	}
	return CoauthoredTally{Rule: coauthoredRule, WalkedWhole: day}, created.UTC().Truncate(oneDay)
}

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
// dates: the range asked first, and a range that holds more than a thousand
// is split in two by date and each half asked again. Each page and each split
// costs one point; measured on 2026-09-12 over the 1,767 public merged pull
// requests of an account created in 2017, the whole life of the account was
// 31 queries in 68 seconds, 32 points for the day with the counts query
// beside it. A merged: range is of UTC days, which is what lets a pass
// settle a day by the clock: measured on 2026-09-27 over the pull requests
// merged from the 24th to the 26th, one merged at 00:05Z is in its own date
// and not in the day before, and one merged at 23:18Z in its own and not in
// the day after, which leaves no time zone standing but UTC's.
type coauthoredWalk struct {
	login string
	// today is the start of the UTC day the pass is made on. What merged
	// before it is settled, and what merged on it is counted for today's row
	// and walked again by the next pass.
	today time.Time
	// pulls is the count so far, settled the part of it merged before today,
	// and queries what the walk has cost.
	pulls, settled, queries int
	// capped is set when one day alone held more than a thousand merged
	// pull requests, which no range can split further: the count is then a
	// floor.
	capped bool
	// truncated is how many pull requests had more commits than the search
	// page carries, no trailer in the ones read nor in their merge commit,
	// and the rest of their commits not read, because the query that asked
	// for them failed: a trailer among those is not seen, and the count is a
	// floor by that many at most. settledTruncated is the part of it merged
	// before today.
	truncated, settledTruncated int
}

// unreadPull is a pull request whose commits run past what has been read,
// with no trailer in them nor in its merge commit: where the rest begin, and
// whether it merged before today.
type unreadPull struct {
	id, after string
	settled   bool
}

// coauthoredCommits is one page of a pull request's commits.
type coauthoredCommits struct {
	TotalCount int      `json:"totalCount"`
	PageInfo   pageInfo `json:"pageInfo"`
	Nodes      []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	} `json:"nodes"`
}

// coauthored is whether a trailer is in any of the page's commits.
func (c *coauthoredCommits) coauthored() bool {
	for i := range c.Nodes {
		if coauthoredBy.MatchString(c.Nodes[i].Commit.Message) {
			return true
		}
	}
	return false
}

// oneDay is the granularity of a merged: range.
const oneDay = 24 * time.Hour

// coauthoredPage is one answer of the walk.
type coauthoredPage struct {
	Search struct {
		IssueCount int      `json:"issueCount"`
		PageInfo   pageInfo `json:"pageInfo"`
		Nodes      []struct {
			ID          string    `json:"id"`
			MergedAt    time.Time `json:"mergedAt"`
			MergeCommit *struct {
				Message string `json:"message"`
			} `json:"mergeCommit"`
			Commits coauthoredCommits `json:"commits"`
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
		if unreadErr := w.follow(ctx, c, w.count(res)); unreadErr != nil {
			return unreadErr
		}
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
// any commit read carries the trailer. It hands back the ones whose commits
// run past the page with no trailer so far, for follow to read on.
func (w *coauthoredWalk) count(res *coauthoredPage) (unread []unreadPull) {
	for i := range res.Search.Nodes {
		n := &res.Search.Nodes[i]
		settled := n.MergedAt.Before(w.today)
		switch {
		case n.MergeCommit != nil && coauthoredBy.MatchString(n.MergeCommit.Message), n.Commits.coauthored():
			w.found(settled)
		case n.Commits.TotalCount <= len(n.Commits.Nodes):
		case n.ID != "" && n.Commits.PageInfo.HasNextPage && n.Commits.PageInfo.EndCursor != "":
			unread = append(unread, unreadPull{id: n.ID, after: n.Commits.PageInfo.EndCursor, settled: settled})
		default:
			w.floor(settled)
		}
	}
	return unread
}

// follow reads the commits of each unread pull request a hundred at a time,
// several pull requests to a query, until it finds a trailer or the commits
// run out. Measured on 2026-09-28 on the production account, whose three pull
// requests over a hundred commits had made its count a floor on every pass:
// two queries of one point each, 77 KB and 44 KB, read the rest of their
// commits, and found no trailer in any. A query that fails leaves its pull
// requests as floors, as they were when nothing past the first page was
// asked for; one cut short by the pass itself fails the walk.
func (w *coauthoredWalk) follow(ctx context.Context, c *ghapi.Client, unread []unreadPull) error {
	for len(unread) > 0 {
		batch := unread[:min(len(unread), coauthoredFollowBatch)]
		unread = unread[len(batch):]
		pages, err := w.commits(ctx, c, batch)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for _, u := range batch {
				w.floor(u.settled)
			}
			continue
		}
		for i, u := range batch {
			switch page := pages[i]; {
			case page == nil:
				w.floor(u.settled)
			case page.coauthored():
				w.found(u.settled)
			case page.PageInfo.HasNextPage && page.PageInfo.EndCursor != "":
				unread = append(unread, unreadPull{id: u.id, after: page.PageInfo.EndCursor, settled: u.settled})
			}
		}
	}
	return nil
}

// commits asks for the next hundred commits of each pull request of the
// batch, in one query, and hands back each page in the batch's order, nil
// for a pull request the answer does not carry.
func (w *coauthoredWalk) commits(ctx context.Context, c *ghapi.Client, batch []unreadPull) ([]*coauthoredCommits, error) {
	var params, fields strings.Builder
	vars := map[string]any{}
	for i, u := range batch {
		if i > 0 {
			params.WriteString(", ")
		}
		fmt.Fprintf(&params, "$id%[1]d: ID!, $after%[1]d: String!", i)
		fmt.Fprintf(&fields, coauthoredCommitsField, i)
		vars[fmt.Sprintf("id%d", i)] = u.id
		vars[fmt.Sprintf("after%d", i)] = u.after
	}
	w.queries++
	var res map[string]*struct {
		Commits *coauthoredCommits `json:"commits"`
	}
	query := "query coauthoredCommits(" + params.String() + ") {" + fields.String() + "\n}"
	if err := c.GraphQL(ctx, query, vars, &res); err != nil {
		return nil, err
	}
	pages := make([]*coauthoredCommits, len(batch))
	for i := range batch {
		if node := res[fmt.Sprintf("p%d", i)]; node != nil {
			pages[i] = node.Commits
		}
	}
	return pages, nil
}

// found counts a co-authored pull request, and settles it when it merged
// before today.
func (w *coauthoredWalk) found(settled bool) {
	w.pulls++
	if settled {
		w.settled++
	}
}

// floor counts a pull request the walk could not read to the end.
func (w *coauthoredWalk) floor(settled bool) {
	w.truncated++
	if settled {
		w.settledTruncated++
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
