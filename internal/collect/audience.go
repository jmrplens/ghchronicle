package collect

import (
	"context"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Audience reads the newest stars and forks of every repository in one
// GraphQL batch, which is what an ordinary sweep of the stars and forks
// families does once a repository has been walked in full.
//
// The REST paths in stars.go and community.go stay for the walks GraphQL is
// not asked to do: the first sight of a repository, when every page of its
// star list is read, and a backfill. After that only the newest page of
// either list can change, and GraphQL serves that page for every repository
// at once. Measured on 2026-09-11 over the eighteen repositories of the
// account: one query of eighteen aliases asking both connections answered
// 33,838 bytes at cost 1 with 274 stars and 67 forks, where REST is thirty
// seven requests, thirty six of them 304 after the first sweep and each of
// them a round trip. The same rows came back in the same order: `last: 100`
// ascending by starred date is the last hundred rows of the REST walk, and
// the fork nodes matched the REST list field for field. Two fork rows of the
// sixty nine REST lists are not in GraphQL, and both belong to accounts that
// no longer exist: the fork and its owner answer 404, and the repository's
// own forks_count says 3 where the REST list says 4. GraphQL agrees with the
// count; REST keeps the ghost.
//
// One query for all eighteen took 7.7 to 8.9 s, too close to the ten seconds
// the gateway allows, so the batch defaults to ten: measured at 2.0 s for the
// stars of ten repositories and 1.3 s for their forks.
type Audience struct {
	Repos []Repo
	// Stars and Forks say which connection to ask for. The runner sets one
	// per family, because the two families run on different cadences; a
	// query carrying both costs the same one point.
	Stars bool
	Forks bool
	// Batch is how many repositories go into one query. Zero means ten.
	Batch int
	// Overflow, when set, is told of every repository whose fork list holds
	// more than the hundred the batch reads. A fork row carries the fork's
	// own stars and how long since it was pushed, and the REST walk
	// refreshed those on up to five hundred forks a sweep; the batch cannot,
	// so the caller walks that repository the old way as well.
	Overflow func(repo Repo)
}

// audienceBatch is the batch size measured above.
const audienceBatch = 10

// audienceStars is the newest hundred stars, oldest first, the order the REST
// list serves them in, so the two paths agree on what "the last page" holds.
const audienceStars = `
  stargazers(last: 100, orderBy: {field: STARRED_AT, direction: ASC}) {
    edges { starredAt node { login } }
  }`

// audienceForks is the newest hundred forks, oldest first, which is the order
// the REST list is asked for with sort=oldest. totalCount says whether those
// hundred are the whole list, which is what Overflow reports on.
const audienceForks = `
  forks(last: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
    totalCount
    nodes { nameWithOwner createdAt pushedAt stargazerCount url owner { login } }
  }`

// audiencePage is what one connection of the batch reads, and so what a
// list has to exceed for Overflow to be told.
const audiencePage = 100

// fragment writes the selection the flags ask for.
func (a Audience) fragment() string {
	var b strings.Builder
	b.WriteString("fragment audience on Repository {")
	if a.Stars {
		b.WriteString(audienceStars)
	}
	if a.Forks {
		b.WriteString(audienceForks)
	}
	b.WriteString("\n}")
	return b.String()
}

// audienceNode is one repository's answer. Both connections are optional,
// because the query only asks for the ones the flags name.
type audienceNode struct {
	Stargazers struct {
		Edges []struct {
			StarredAt time.Time `json:"starredAt"`
			Node      struct {
				Login string `json:"login"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"stargazers"`
	Forks struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			NameWithOwner string    `json:"nameWithOwner"`
			CreatedAt     time.Time `json:"createdAt"`
			PushedAt      time.Time `json:"pushedAt"`
			Stars         int       `json:"stargazerCount"`
			URL           string    `json:"url"`
			Owner         struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"nodes"`
	} `json:"forks"`
}

// stars converts the edges into the rows the REST path decodes, so the same
// starPoints renders both.
func (n *audienceNode) stars() []starRow {
	rows := make([]starRow, 0, len(n.Stargazers.Edges))
	for _, e := range n.Stargazers.Edges {
		var row starRow
		row.StarredAt, row.User.Login = e.StarredAt, e.Node.Login
		rows = append(rows, row)
	}
	return rows
}

// forks converts the nodes into the rows the REST path decodes, so the same
// forkPoints renders both.
func (n *audienceNode) forks() []forkRow {
	rows := make([]forkRow, 0, len(n.Forks.Nodes))
	for i := range n.Forks.Nodes {
		f := &n.Forks.Nodes[i]
		var row forkRow
		row.FullName, row.CreatedAt, row.PushedAt = f.NameWithOwner, f.CreatedAt, f.PushedAt
		row.Stars, row.HTMLURL, row.Owner.Login = f.Stars, f.URL, f.Owner.Login
		rows = append(rows, row)
	}
	return rows
}

func (a Audience) Collect(ctx context.Context, c *ghapi.Client, _ time.Time) ([]sink.Point, error) {
	if len(a.Repos) == 0 || (!a.Stars && !a.Forks) {
		return nil, nil
	}
	size := a.Batch
	if size <= 0 {
		size = audienceBatch
	}
	fragment := a.fragment()
	build := func(batch []Repo) string {
		return aliasQuery(batch, func(int) string { return "...audience" }, fragment)
	}
	var points []sink.Point
	failed := aliasBatch(ctx, c, a.Repos, size, build, func(repo Repo, node audienceNode) {
		if a.Stars {
			base := repoTags(repo.Owner, repo.Name)
			points = append(points, starPoints([][]starRow{node.stars()}, base,
				githubPage(repo.FullName, "stargazers"))...)
		}
		if a.Forks {
			points = append(points, forkPoints(node.forks(), repo)...)
			if a.Overflow != nil && node.Forks.TotalCount > audiencePage {
				a.Overflow(repo)
			}
		}
	})
	// Both: aliasBatch already kept every repository that answered, and the
	// error goes back with them rather than being dropped because some of
	// them did. Whether a family that half failed is marked as having run is
	// the runner's decision and is made there; here it is a fact, and a fact
	// nobody was told cost five repositories their whole history once.
	return points, failed
}
