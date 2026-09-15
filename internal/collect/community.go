package collect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Forks collects who forked a repository and when.
//
// The repository snapshot carries a fork count, which says how many but never
// when or by whom. The list carries a creation date per fork and, unlike the
// star list, it is short enough to walk in one page even after eight years.
type Forks struct {
	Walk Walk
}

// forkRow is one fork as the REST list reports it. The GraphQL batch in
// audience.go converts its nodes into this shape, so both paths render a fork
// through forkPoints and cannot drift apart.
type forkRow struct {
	FullName  string    `json:"full_name"`
	CreatedAt time.Time `json:"created_at"`
	PushedAt  time.Time `json:"pushed_at"`
	Stars     int       `json:"stargazers_count"`
	HTMLURL   string    `json:"html_url"`
	Owner     struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (f Forks) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	var points []sink.Point
	most := f.Walk.limit(5)
	for page := 1; page <= most; page++ {
		var batch []forkRow
		path := fmt.Sprintf("/repos/%s/forks?per_page=100&sort=oldest&page=%d", repo.FullName, page)
		if _, _, err := c.GetJSON(ctx, path, &batch, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return points, err
		}
		if len(batch) == 0 {
			break
		}
		points = append(points, forkPoints(batch, repo, now)...)
		if len(batch) < 100 {
			break
		}
	}
	return points, nil
}

// forkPoints stamps each fork at the moment it was created, so re-reading the
// list rewrites the same rows rather than adding to them.
func forkPoints(rows []forkRow, repo Repo, now time.Time) []sink.Point {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	points := make([]sink.Point, 0, len(rows))
	for i := range rows {
		f := &rows[i]
		fields := map[string]any{"forks": 1, "stars": f.Stars}
		// Whether the fork was ever pushed to separates a real derivative
		// from a bookmark, which is most of them.
		if !f.PushedAt.IsZero() {
			fields["days_since_push"] = int(now.Sub(f.PushedAt).Hours() / 24)
			fields["advanced"] = f.PushedAt.After(f.CreatedAt.Add(time.Minute))
		}
		points = append(points, sink.Point{
			Measurement: "gh_fork",
			Tags:        merge(base, map[string]string{"by": f.Owner.Login}),
			Fields:      withURL(fields, f.HTMLURL),
			Time:        f.CreatedAt,
		})
	}
	return points
}

// Planning collects labels and milestones.
//
// A milestone is the only place GitHub records an intention with a due date,
// and its completion percentage is computed server side. Labels say how the
// work is classified, which the per-issue label count cannot.
type Planning struct{}

const planningQuery = `
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    labels(first: 60) {
      nodes { name url issues { totalCount } pullRequests { totalCount } }
    }
    milestones(first: 30, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        title state url createdAt dueOn closedAt progressPercentage
        issues { totalCount }
        pullRequests { totalCount }
      }
    }
  }
}`

// repoLabel is one label of a repository, with the counts that say whether
// anybody classifies work with it.
type repoLabel struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	Issues       count  `json:"issues"`
	PullRequests count  `json:"pullRequests"`
}

// repoMilestone is one milestone, including the completion percentage GraphQL
// computes and nothing here has to.
type repoMilestone struct {
	Title              string     `json:"title"`
	State              string     `json:"state"`
	URL                string     `json:"url"`
	CreatedAt          time.Time  `json:"createdAt"`
	DueOn              *time.Time `json:"dueOn"`
	ClosedAt           *time.Time `json:"closedAt"`
	ProgressPercentage float64    `json:"progressPercentage"`
	Issues             count      `json:"issues"`
	PullRequests       count      `json:"pullRequests"`
}

// planningRepository is the half of a repository planningQuery asks for: how
// its work is classified, and what it is due under.
type planningRepository struct {
	Labels struct {
		Nodes []repoLabel `json:"nodes"`
	} `json:"labels"`
	Milestones struct {
		Nodes []repoMilestone `json:"nodes"`
	} `json:"milestones"`
}

func (Planning) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	var res struct {
		Repository planningRepository `json:"repository"`
	}
	vars := map[string]any{"owner": repo.Owner, "name": repo.Name}
	if err := c.GraphQL(ctx, planningQuery, vars, &res); err != nil {
		// A repository with no labels and no milestones answers NOT_FOUND on
		// the fields rather than returning empty connections, and that is not
		// a failure. Anything else is.
		if isSkippableGraphQL(err) {
			return nil, nil
		}
		return nil, err
	}
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	day := now.UTC().Truncate(24 * time.Hour)
	var points []sink.Point

	for _, l := range res.Repository.Labels.Nodes {
		if l.Issues.TotalCount+l.PullRequests.TotalCount == 0 {
			continue // a label nobody has used is noise, not a metric
		}
		points = append(points, sink.Point{
			Measurement: "gh_label",
			Tags:        merge(base, map[string]string{"label": l.Name}),
			Fields: map[string]any{
				"issues": l.Issues.TotalCount, "pull_requests": l.PullRequests.TotalCount,
				"used": l.Issues.TotalCount + l.PullRequests.TotalCount, "url": l.URL,
			},
			Time: day,
		})
	}
	for _, m := range res.Repository.Milestones.Nodes {
		fields := map[string]any{
			"progress": m.ProgressPercentage,
			"issues":   m.Issues.TotalCount, "pull_requests": m.PullRequests.TotalCount,
			"url": m.URL,
		}
		if m.DueOn != nil {
			fields["days_to_due"] = int(m.DueOn.Sub(now).Hours() / 24)
		}
		if m.ClosedAt != nil {
			fields["seconds_to_close"] = int(m.ClosedAt.Sub(m.CreatedAt).Seconds())
		}
		// Stamped at the day rather than at the sweep: a milestone is a
		// standing intention, and one row a day is enough to draw its progress.
		points = append(points, sink.Point{
			Measurement: "gh_milestone",
			Tags:        merge(base, map[string]string{"milestone": m.Title, "state": m.State}),
			Fields:      fields,
			Time:        day,
		})
	}
	return points, nil
}

// Outbound collects the stars the account gave to other people's repositories,
// and the work it did outside its own repositories.
//
// Everything else here measures what the account owns. This is the other half:
// what it reads and what it contributes to. All of it comes from GraphQL, one
// point a query, since 2026-09-11. It used to come from REST, which served the
// same rows wrapped in everything else it knows about a repository or an
// issue: the star list was 617 KB uncompressed for 93 stars against 13.5 KB
// here, and the five searches 387 KB against 21 KB, none of it with an ETag.
// Measured that day, row by row, against the REST answers of the same
// minute: the 93 stars came back in the same order with the same language and
// count, and the 69 items of the five searches were the same items with the
// same title, dates and url. One `comments` count differed, on an issue whose
// only comment had been deleted: REST still said 1 and GraphQL, which counts
// the comments that exist, said 0.
type Outbound struct {
	Login string
	Walk  Walk
}

// starredQuery is the stars the account gave, newest first, which is the
// order the REST listing served them in.
const starredQuery = `
query($first: Int!, $after: String) {
  viewer {
    starredRepositories(first: $first, after: $after, orderBy: {field: STARRED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      edges {
        starredAt
        node { nameWithOwner stargazerCount primaryLanguage { name } }
      }
    }
  }
}`

// outboundSearchQuery is one issue search, a page of a hundred, with the
// fields a contribution row is made of and nothing else.
const outboundSearchQuery = `
query($query: String!) {
  search(type: ISSUE, first: 100, query: $query) {
    nodes {
      ... on Issue {
        number title url createdAt closedAt
        comments { totalCount } repository { nameWithOwner }
      }
      ... on PullRequest {
        number title url createdAt closedAt mergedAt
        comments { totalCount } repository { nameWithOwner }
      }
    }
  }
}`

func (o Outbound) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	// Stars given, each dated when it was given.
	points, failed := o.starred(ctx, c)
	if failed != nil {
		return points, failed
	}

	// Work in repositories the account does not own. Nothing else sees it: it
	// is not in this account's repositories, and the event feed forgets it in
	// three days.
	//
	// Four searches rather than one, because the state is the interesting part
	// and search will not return it any other way: a pull request that was
	// merged, one that is still open, one that was closed unmerged and an
	// issue opened elsewhere are four different facts about the same person.
	for _, q := range []struct{ kind, state, query string }{
		{"pull_request", "merged", "is:pr is:merged"},
		{"pull_request", "open", "is:pr is:open"},
		{"pull_request", "closed", "is:pr is:closed is:unmerged"},
		{"issue", "open", "is:issue is:open"},
		{"issue", "closed", "is:issue is:closed"},
	} {
		pts, err := o.search(ctx, c, q.kind, q.state, q.query, now)
		if err != nil {
			if isSkippableGraphQL(err) {
				continue
			}
			return points, err
		}
		points = append(points, pts...)
	}

	// Comments and discussion answers, anywhere. Both come from `viewer`, one
	// GraphQL point each, and both see other people's repositories, which is
	// the half of the work that owning no repository there makes invisible.
	for _, collect := range []func(context.Context, *ghapi.Client, time.Time) ([]sink.Point, error){
		o.discussionComments, o.issueComments,
	} {
		pts, err := collect(ctx, c, now)
		if err != nil {
			if isSkippable(err) {
				continue
			}
			return points, err
		}
		points = append(points, pts...)
	}
	return points, nil
}

// discussionCommentNode is one comment on a discussion, wherever it was left.
// It is a named type because two collectors read the same shape: the walk over
// everything this account wrote anywhere, and the walk over everything anyone
// wrote in this account's own repositories.
type discussionCommentNode struct {
	CreatedAt   time.Time `json:"createdAt"`
	IsAnswer    bool      `json:"isAnswer"`
	UpvoteCount int       `json:"upvoteCount"`
	URL         string    `json:"url"`
	DatabaseID  int64     `json:"databaseId"`
	ReplyTo     *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"replyTo"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Discussion discussionThread `json:"discussion"`
}

// discussionThread is the discussion a comment hangs on, as both comment walks
// ask for it. It is read because the comment's own row carries the thread's
// state with it: see discussionContext.
type discussionThread struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	CreatedAt      time.Time  `json:"createdAt"`
	IsAnswered     bool       `json:"isAnswered"`
	AnswerChosenAt *time.Time `json:"answerChosenAt"`
	AnswerChosenBy *struct {
		Login string `json:"login"`
	} `json:"answerChosenBy"`
	Answer      *discussionAnswer `json:"answer"`
	Closed      bool              `json:"closed"`
	ClosedAt    *time.Time        `json:"closedAt"`
	StateReason string            `json:"stateReason"`
	Category    struct {
		Name string `json:"name"`
		// isAnswered is null, not false, for a discussion in a category
		// that cannot be answered. Go decodes that null to false, so
		// without this the nine comments this account has left on
		// announcements and general threads would read exactly like a
		// question nobody answered.
		IsAnswerable bool `json:"isAnswerable"`
	} `json:"category"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// discussionAnswer is the comment chosen as the answer, read only for who
// wrote it: answering one's own question and being answered by somebody else
// are different facts.
type discussionAnswer struct {
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

// context is what the thread says about itself, carried onto the comment.
func (n *discussionCommentNode) context() *discussionContext {
	c := &discussionContext{
		Created:     n.Discussion.CreatedAt,
		Answered:    n.Discussion.IsAnswered,
		AnsweredAt:  n.Discussion.AnswerChosenAt,
		Closed:      n.Discussion.Closed,
		ClosedAt:    n.Discussion.ClosedAt,
		StateReason: n.Discussion.StateReason,
		Category:    n.Discussion.Category.Name,
		Answerable:  n.Discussion.Category.IsAnswerable,
	}
	if a := n.Discussion.Answer; a != nil && a.Author != nil {
		c.AnsweredBy = a.Author.Login
	}
	if by := n.Discussion.AnswerChosenBy; by != nil {
		c.ChosenBy = by.Login
	}
	return c
}

// discussionContext is what a comment's own row cannot say: whether the
// question it sits under was ever answered, by whom, and whether the thread
// was closed.
//
// Without it an unanswered discussion and one somebody else answered are the
// same row, both carrying is_answer=false. Measured against the live API on
// 2026-09-10 over the 71 comments this account has left anywhere: 61 sit on
// discussions with no accepted answer, 5 are the accepted answer, and 5 sit on
// answered threads without being the answer, two of them on threads a
// different person won.
//
// It travels as fields, not as tags. A new tag changes the Graphite path depth
// of gh_discussion_comment, and both collectors that write that measurement
// would have to agree on a value for every row of it.
type discussionContext struct {
	Created    time.Time
	Answered   bool
	AnsweredAt *time.Time
	// AnsweredBy is the author of the accepted answer, which is the one that
	// says whether this account won the thread. ChosenBy is the actor who
	// marked it, usually the person who asked: measured on the 7 answered
	// threads here, the two are the same person exactly once.
	AnsweredBy string
	ChosenBy   string
	// Answerable is whether the category allows an answer at all. Measured
	// over the 71 comments: 9 sit in categories that cannot be answered, and
	// all four discussions in this account's own repositories are in one.
	// Answered is false on every one of them, which is not the same fact as a
	// question left unanswered.
	Answerable  bool
	Closed      bool
	ClosedAt    *time.Time
	StateReason string
	Category    string
}

// addTo writes the thread's context onto a comment's fields. A pointer
// receiver because the struct is a hundred and twenty bytes and every comment
// of every thread goes through here.
func (c *discussionContext) addTo(fields map[string]any) {
	fields["discussion_answered"] = c.Answered
	fields["discussion_answerable"] = c.Answerable
	fields["discussion_closed"] = c.Closed
	for name, value := range map[string]string{
		"answered_by":      c.AnsweredBy,
		"answer_chosen_by": c.ChosenBy,
		"state_reason":     c.StateReason,
		"category":         c.Category,
	} {
		if value != "" {
			fields[name] = value
		}
	}
	if c.Created.IsZero() {
		return
	}
	// The same definitions gh_discussion and gh_milestone use, so a thread in
	// somebody else's repository, which gh_discussion never sees, is measured
	// the way one's own is.
	if c.AnsweredAt != nil {
		fields["seconds_to_answer"] = int(c.AnsweredAt.Sub(c.Created).Seconds())
	}
	if c.ClosedAt != nil {
		fields["seconds_to_close"] = int(c.ClosedAt.Sub(c.Created).Seconds())
	}
}

// point renders one discussion comment. The identity is the comment's own id,
// so the same comment reached from either walk is the same row rather than two.
func (n *discussionCommentNode) point(login, repo string) sink.Point {
	author := login
	if n.Author != nil && n.Author.Login != "" {
		author = n.Author.Login
	}
	fields := map[string]any{
		"comments": 1, "upvotes": n.UpvoteCount,
		"title": n.Discussion.Title,
		// Answered, as a field of its own, so a panel can total accepted
		// answers without grouping by a tag.
		"answers": boolInt(n.IsAnswer),
		"url":     n.URL,
	}
	n.context().addTo(fields)
	return sink.Point{
		Measurement: "gh_discussion_comment",
		Tags: map[string]string{
			"user": login, "repo": repo,
			"own":       boolTag(isOwn(repo, login)),
			"is_answer": boolTag(n.IsAnswer),
			// A reply to a reply is a different thing from a comment on the
			// discussion itself, and only this tag separates them.
			"is_reply": boolTag(n.ReplyTo != nil),
			"author":   author,
			"comment":  strconv.FormatInt(n.DatabaseID, 10),
			"number":   strconv.Itoa(n.Discussion.Number),
		},
		Fields: fields,
		Time:   n.CreatedAt,
	}
}

const discussionCommentsQuery = `
query($last: Int!, $before: String) {
  viewer {
    repositoryDiscussionComments(last: $last, before: $before) {
      totalCount
      pageInfo { hasPreviousPage startCursor }
      nodes {
        createdAt isAnswer upvoteCount url databaseId
        replyTo { databaseId }
        author { login }
        discussion {
          number title createdAt
          isAnswered answerChosenAt answerChosenBy { login }
          answer { author { login } }
          closed closedAt stateReason
          category { name isAnswerable }
          repository { nameWithOwner }
        }
      }
    }
  }
}`

// backwardPageInfo is the page info of a connection walked from its end,
// which is how the two viewer comment connections are read: see
// discussionComments.
type backwardPageInfo struct {
	HasPreviousPage bool   `json:"hasPreviousPage"`
	StartCursor     string `json:"startCursor"`
}

// discussionCommentConnection is a page of
// viewer.repositoryDiscussionComments, read from its end like the walk that
// asks for it.
type discussionCommentConnection struct {
	TotalCount int                     `json:"totalCount"`
	PageInfo   backwardPageInfo        `json:"pageInfo"`
	Nodes      []discussionCommentNode `json:"nodes"`
}

// discussionComments records every comment left on a discussion, in any
// repository.
//
// gh_discussion sees only the discussions inside the account's own
// repositories, which on the account this was written against is three rows.
// The work happens in other people's: sixty six comments across twenty six
// repositories going back to 2021, four of them accepted as the answer, a
// number that existed nowhere.
//
// The connection is read from its end. Both viewer comment connections list
// oldest first, and repositoryDiscussionComments takes no orderBy at all,
// measured on 2026-09-12: first: 15 answered the comment of 2021 first, and
// the sweep, which reads one page, read the same oldest hundred on every
// pass. An answer given today reached the store only when a backfill walked
// the whole list, and a comment that became the accepted answer after that
// was never seen again. last: 100 with before is the newest hundred, and a
// backfill walks back from there until the API runs out or Since is passed.
func (o Outbound) discussionComments(ctx context.Context, c *ghapi.Client, _ time.Time) ([]sink.Point, error) {
	var points []sink.Point
	before := ""
	most := o.Walk.limit(1)
	for page := 1; page <= most; page++ {
		var res struct {
			Viewer struct {
				Comments discussionCommentConnection `json:"repositoryDiscussionComments"`
			} `json:"viewer"`
		}
		vars := map[string]any{"last": 100}
		if before != "" {
			vars["before"] = before
		}
		if err := c.GraphQL(ctx, discussionCommentsQuery, vars, &res); err != nil {
			return points, err
		}
		cs := res.Viewer.Comments
		for i := range cs.Nodes {
			n := &cs.Nodes[i]
			points = append(points, n.point(o.Login, n.Discussion.Repository.NameWithOwner))
		}
		// The page is oldest first, so its first node is the oldest seen.
		if !cs.PageInfo.HasPreviousPage || len(cs.Nodes) == 0 || o.Walk.past(cs.Nodes[0].CreatedAt) {
			break
		}
		before = cs.PageInfo.StartCursor
	}
	return points, nil
}

const issueCommentsQuery = `
query($last: Int!, $before: String) {
  viewer {
    issueComments(last: $last, before: $before) {
      totalCount
      pageInfo { hasPreviousPage startCursor }
      nodes {
        createdAt url
        issue { number repository { nameWithOwner } }
      }
    }
  }
}`

// issueCommentConnection is a page of viewer.issueComments, walked from its
// end, which is why it carries the backward page info.
type issueCommentConnection struct {
	TotalCount int                `json:"totalCount"`
	PageInfo   backwardPageInfo   `json:"pageInfo"`
	Nodes      []issueCommentNode `json:"nodes"`
}

// issueCommentNode is one comment the account left, anywhere.
type issueCommentNode struct {
	CreatedAt time.Time      `json:"createdAt"`
	URL       string         `json:"url"`
	Issue     commentedIssue `json:"issue"`
}

// commentedIssue is the issue or pull request a comment sits on, read only for
// where it is: the repository is what tells a comment left in the account's own
// work from one left in a stranger's.
type commentedIssue struct {
	Number     int `json:"number"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// issueComments records every comment the account left on an issue or a pull
// request, anywhere.
//
// The REST shape of this is one call per repository and sees only the ones
// swept. `viewer` is one call, costs one point, and includes the repositories
// the account does not own, which is where most of them are.
//
// Read from the end, for the reason discussionComments gives: the connection
// lists oldest first (measured on 2026-09-12, first: 3 of 706 answered the
// three of 2020), so a sweep reading its one page from the front wrote the
// same hundred comments of 2020 and 2021 on every pass and never the one
// left this morning.
func (o Outbound) issueComments(ctx context.Context, c *ghapi.Client, _ time.Time) ([]sink.Point, error) {
	var points []sink.Point
	before := ""
	most := o.Walk.limit(1)
	for page := 1; page <= most; page++ {
		var res struct {
			Viewer struct {
				Comments issueCommentConnection `json:"issueComments"`
			} `json:"viewer"`
		}
		vars := map[string]any{"last": 100}
		if before != "" {
			vars["before"] = before
		}
		if err := c.GraphQL(ctx, issueCommentsQuery, vars, &res); err != nil {
			return points, err
		}
		cs := res.Viewer.Comments
		for _, n := range cs.Nodes {
			repo := n.Issue.Repository.NameWithOwner
			points = append(points, sink.Point{
				Measurement: "gh_issue_comment",
				Tags: map[string]string{
					"user": o.Login, "repo": repo,
					"own":    boolTag(isOwn(repo, o.Login)),
					"number": strconv.Itoa(n.Issue.Number),
				},
				Fields: map[string]any{"comments": 1, "url": n.URL},
				Time:   n.CreatedAt,
			})
		}
		if !cs.PageInfo.HasPreviousPage || len(cs.Nodes) == 0 || o.Walk.past(cs.Nodes[0].CreatedAt) {
			break
		}
		before = cs.PageInfo.StartCursor
	}
	return points, nil
}

// isOwn reports whether a full name belongs to the account being collected.
// The comment left in one's own repository and the one left in a stranger's
// are different facts, and only the second is invisible everywhere else.
func isOwn(fullName, login string) bool {
	owner, _, ok := strings.Cut(fullName, "/")
	return ok && strings.EqualFold(owner, login)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// starredConnection is a page of viewer.starredRepositories. The star's own
// date hangs on the edge rather than on the repository, which is why this is
// read as edges.
type starredConnection struct {
	PageInfo pageInfo      `json:"pageInfo"`
	Edges    []starredEdge `json:"edges"`
}

// starredEdge is one star the account gave: when it was given, and to what.
type starredEdge struct {
	StarredAt time.Time         `json:"starredAt"`
	Node      starredRepository `json:"node"`
}

// starredRepository is as much of a starred repository as the row says: what
// it is called, how many stars it has, and what it is written in.
type starredRepository struct {
	NameWithOwner string `json:"nameWithOwner"`
	Stars         int    `json:"stargazerCount"`
	Language      *struct {
		Name string `json:"name"`
	} `json:"primaryLanguage"`
}

// starred reads the stars the account gave, a page of a hundred per point,
// as far as the walk allows: five pages on a sweep, everything on a backfill.
func (o Outbound) starred(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
	var points []sink.Point
	after := ""
	most := o.Walk.limit(5)
	for page := 1; page <= most; page++ {
		var res struct {
			Viewer struct {
				Starred starredConnection `json:"starredRepositories"`
			} `json:"viewer"`
		}
		vars := map[string]any{"first": 100}
		if after != "" {
			vars["after"] = after
		}
		if err := c.GraphQL(ctx, starredQuery, vars, &res); err != nil {
			if isSkippableGraphQL(err) {
				break
			}
			return points, err
		}
		st := res.Viewer.Starred
		for i := range st.Edges {
			s := &st.Edges[i]
			language := ""
			if s.Node.Language != nil {
				language = s.Node.Language.Name
			}
			points = append(points, sink.Point{
				Measurement: "gh_star_given",
				Tags: map[string]string{
					// A repository GitHub has detected no language in answers
					// null here, and an empty tag value is dropped: measured
					// on 2026-09-10, 9 of the 93 repositories this account has
					// starred. Those rows would sit in an InfluxDB series of
					// their own, carrying no language tag at all.
					"user": o.Login, "repo": s.Node.NameWithOwner, "language": orNone(language),
				},
				Fields: withURL(map[string]any{
					"stars": 1, "repo_stars": s.Node.Stars,
				}, githubPage(s.Node.NameWithOwner)),
				Time: s.StarredAt,
			})
		}
		if !st.PageInfo.HasNextPage || len(st.Edges) == 0 || o.Walk.past(st.Edges[len(st.Edges)-1].StarredAt) {
			break
		}
		after = st.PageInfo.EndCursor
	}
	return points, nil
}

// search runs one issue search scoped to other people's repositories.
//
// One GraphQL point a search, out of the same bucket as the rest of the
// sweep. The REST search API this came from has a budget of its own, thirty a
// minute, but a page of a hundred items there is a hundred bodies, users and
// label lists for the seven fields a row is made of, and never a 304: a
// contribution search was 100 KB uncompressed against 8 KB here.
func (o Outbound) search(ctx context.Context, c *ghapi.Client, kind, state, filter string, now time.Time) ([]sink.Point, error) {
	var res struct {
		Search struct {
			Nodes []struct {
				Number     int        `json:"number"`
				Title      string     `json:"title"`
				URL        string     `json:"url"`
				CreatedAt  time.Time  `json:"createdAt"`
				ClosedAt   *time.Time `json:"closedAt"`
				MergedAt   *time.Time `json:"mergedAt"`
				Comments   count      `json:"comments"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"nodes"`
		} `json:"search"`
	}
	vars := map[string]any{"query": fmt.Sprintf("%s author:%s -user:%s", filter, o.Login, o.Login)}
	if err := c.GraphQL(ctx, outboundSearchQuery, vars, &res); err != nil {
		return nil, err
	}
	var points []sink.Point
	for i := range res.Search.Nodes {
		it := &res.Search.Nodes[i]
		// Dated when it closed, or at the start of the day while it is still
		// open, so an open item rewrites one row a day instead of one an hour.
		stamp := now.UTC().Truncate(24 * time.Hour)
		if it.ClosedAt != nil {
			stamp = *it.ClosedAt
		}
		fields := withURL(map[string]any{
			"contributions": 1, "title": it.Title, "comments": it.Comments.TotalCount,
			"seconds_open": int(stamp.Sub(it.CreatedAt).Seconds()),
		}, it.URL)
		if it.MergedAt != nil {
			fields["merged"] = 1
			fields["seconds_to_merge"] = int(it.MergedAt.Sub(it.CreatedAt).Seconds())
		}
		points = append(points, sink.Point{
			Measurement: "gh_external_contribution",
			Tags: map[string]string{
				"user": o.Login, "repo": it.Repository.NameWithOwner,
				"number": strconv.Itoa(it.Number), "kind": kind, "state": state,
			},
			Fields: fields,
			Time:   stamp,
		})
	}
	return points, nil
}
