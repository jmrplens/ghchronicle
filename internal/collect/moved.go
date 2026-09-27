package collect

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// Movements asks every repository, in one aliased query per batch, when it
// last moved in the two ways three families can be told about in advance:
// the newest commit of its default branch, and the newest update to any of
// its issues or pull requests.
//
// Those three, commits, issueevents and the incremental pass of issues, read
// only what moved since a window of their own, one query per repository, and
// GitHub charges a query for the page it asks for, not for what comes back.
// Measured from the proxy log of the production service over 27 passes of
// each on 37 repositories, 2026-09-25 12:58Z to 2026-09-26 19:50Z: 961 of the
// 999 commits answers held no commit, and 468 of the 968 incremental issues
// answers and 486 of the 1,005 issueevents answers held no item at all. That
// is 2,077 points for nothing, 43 per cent of every GraphQL point the service
// spent in the window. This query says which repositories those were before
// any of them is asked, for one point per batch.
//
// It writes nothing. What it answers is only ever a reason to skip a read,
// and a repository it has no answer for is read as if it had not been asked.
type Movements struct {
	Repos []Repo
	// Batch is how many repositories go into one query. Zero means
	// movedBatch.
	Batch int
}

// movedBatch is how many repositories go into one query.
//
// A point holds fifty: each repository is two nodes, the newest issue and
// the newest pull request, and GitHub charges a point per hundred. It is not
// the point that bounds the batch but the gateway's ten seconds, and every
// field here costs time on a busy repository. Measured on 2026-09-27 against
// the hundred most recently updated repositories with more than twenty
// thousand stars: fifty at once took 5.2 to 7.8 seconds, forty 4.7 to 8.4
// and twenty five 2.9 to 4.8, each at cost 1, and a hundred cost 2 and came
// back with RESOURCE_LIMITS_EXCEEDED for every alias past the sixty-fifth. The account's own thirty seven answered in 3.3 to 3.6
// seconds at once, and in 2.0 to 2.7 and 1.5 as twenty five and twelve. One
// point more per twenty five repositories is nothing beside a query the
// gateway gives up on after ten seconds, which aliasBatch retries halved.
const movedBatch = 25

// movedFragment spells the connections the way the collectors they stand in
// for order them, so the first node is the one those collectors would read
// first.
const movedFragment = `
fragment moved on Repository {
  defaultBranchRef { target { ... on Commit { committedDate } } }
  issues(first: 1, orderBy: {field: UPDATED_AT, direction: DESC}) { nodes { updatedAt } }
  pullRequests(first: 1, orderBy: {field: UPDATED_AT, direction: DESC}) { nodes { updatedAt } }
}`

// Movement is when one repository last moved, in the terms the families it
// lets skip a read are bounded by.
type Movement struct {
	// Head is when the newest commit of the default branch was committed,
	// and zero for a repository with no default branch, which is an empty
	// one: the commits query finds no history there either.
	Head time.Time
	// Items is when the most recently updated issue or pull request was
	// updated, and zero for a repository that has neither.
	Items time.Time
}

// CommitsSince reports whether the default branch holds a commit committed at
// or after since.
//
// This is the condition under which history(since:) answers anything, and not
// an approximation of it. Measured on 2026-09-27 against golang/go, whose head
// was committed at 22:27:19Z and authored three days earlier: since that
// second the history held the head, one second later it held nothing, and
// since the day before it held the commits committed that day whatever their
// author date. The walk stops at the first commit older than since, so a head
// older than since is an empty history.
func (m Movement) CommitsSince(since time.Time) bool { return !m.Head.Before(since) }

// ItemsSince reports whether an issue or a pull request was updated at or
// after since.
//
// Both families it stands in for list the items newest updated first, so
// when the newest of each list is older than since, so is every item. The
// issueevents pass finds nothing then, because it skips an item last updated
// before since, an event moving its item's updatedAt; the incremental issues
// pass would rewrite the newest page of items nobody touched, which is the
// daily whole-page read's job and not something that moved.
func (m Movement) ItemsSince(since time.Time) bool { return !m.Items.Before(since) }

// movedNode is one repository's answer.
type movedNode struct {
	DefaultBranchRef *struct {
		Target struct {
			CommittedDate time.Time `json:"committedDate"`
		} `json:"target"`
	} `json:"defaultBranchRef"`
	Issues       newestUpdate `json:"issues"`
	PullRequests newestUpdate `json:"pullRequests"`
}

// newestUpdate is a connection of one node, the most recently updated.
type newestUpdate struct {
	Nodes []struct {
		UpdatedAt time.Time `json:"updatedAt"`
	} `json:"nodes"`
}

func (n newestUpdate) at() time.Time {
	if len(n.Nodes) == 0 {
		return time.Time{}
	}
	return n.Nodes[0].UpdatedAt
}

func (n *movedNode) movement() Movement {
	var m Movement
	if n.DefaultBranchRef != nil {
		m.Head = n.DefaultBranchRef.Target.CommittedDate
	}
	m.Items = n.Issues.at()
	if pr := n.PullRequests.at(); pr.After(m.Items) {
		m.Items = pr
	}
	return m
}

// Read answers, per full name, for every repository whose batch answered.
//
// A batch that failed is left out of the map along with its error, and the
// rest of the map is still true: the caller reads every repository the map
// does not name.
func (ms Movements) Read(ctx context.Context, c *ghapi.Client) (map[string]Movement, error) {
	size := ms.Batch
	if size <= 0 {
		size = movedBatch
	}
	build := func(batch []Repo) string {
		return aliasQuery(batch, func(int) string { return "...moved" }, movedFragment)
	}
	out := make(map[string]Movement, len(ms.Repos))
	err := aliasBatch(ctx, c, ms.Repos, size, build, func(repo Repo, n movedNode) {
		out[repo.FullName] = n.movement()
	})
	return out, err
}
