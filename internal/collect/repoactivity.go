package collect

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// RepoActivity collects the shape of work in a repository: commits per week,
// the hour-of-week distribution, the workflows that exist and the discussions.
//
// The two `stats` endpoints used here are the ones that actually answer for
// this account. `stats/code_frequency` and `stats/contributors` return 202 with
// an empty body indefinitely and are deliberately not called.
type RepoActivity struct{}

func (RepoActivity) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	var points []sink.Point
	for _, read := range []func(context.Context, *ghapi.Client, Repo, map[string]string, time.Time) ([]sink.Point, error){
		weeklyCommitPoints, punchCardPoints, workflowPoints,
	} {
		pts, err := read(ctx, c, repo, base, now)
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

// weeklyCommitPoints reads the weekly commits for the last year, split into
// the owner's and everyone's. GitHub returns 52 numbers ending with the
// current week, so each is dated backwards from now and rewrites its own row
// on the next sweep.
func weeklyCommitPoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	var part struct {
		All   []int `json:"all"`
		Owner []int `json:"owner"`
	}
	if _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/stats/participation", &part, ""); err != nil {
		return nil, err
	}
	// Anchored to the Sunday that starts GitHub's current week, not to
	// "today minus N days": a sweep on Tuesday and one on Friday have to
	// land on the same row for the same week, or every re-read writes a
	// second copy of the year.
	sunday := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -int(now.UTC().Weekday()))
	n := len(part.All)
	points := make([]sink.Point, 0, n)
	for i := range n {
		week := sunday.AddDate(0, 0, -7*(n-1-i))
		f := map[string]any{"commits": part.All[i]}
		if i < len(part.Owner) {
			f["owner_commits"] = part.Owner[i]
		}
		points = append(points, sink.Point{
			Measurement: "gh_commits_week", Tags: base, Fields: f, Time: week,
		})
	}
	return points, nil
}

// punchCardPoints reads the punch card, which is a distribution, not a
// history: 168 buckets of day-of-week and hour. It is stamped now because it
// describes the whole life of the repository as of this moment.
func punchCardPoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	var punch [][3]int
	if _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/stats/punch_card", &punch, ""); err != nil {
		return nil, err
	}
	days := [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var points []sink.Point
	for _, row := range punch {
		if row[2] == 0 {
			continue // an empty hour is the absence of a fact, not a zero worth storing
		}
		if row[0] < 0 || row[0] > 6 {
			continue
		}
		points = append(points, sink.Point{
			Measurement: "gh_commit_punchcard",
			Tags: merge(base, map[string]string{
				"weekday": days[row[0]], "hour": twoDigits(row[1]),
			}),
			Fields: map[string]any{"commits": row[2]},
			Time:   now,
		})
	}
	return points, nil
}

// workflowPoints reads the workflows that exist, so a run count can be
// attributed to a workflow that is disabled rather than to one that simply
// did not fire.
func workflowPoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	var wf struct {
		Workflows []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			// The two dates arrive with a local offset (+01:00, +02:00) and a
			// fractional second rather than in Z like the rest of the API,
			// measured on all eleven workflows of one repository. RFC 3339
			// covers both, which is why this is still a time.Time.
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
			State     string    `json:"state"`
			HTMLURL   string    `json:"html_url"`
		} `json:"workflows"`
	}
	if _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/actions/workflows?per_page=100", &wf, ""); err != nil {
		return nil, err
	}
	points := make([]sink.Point, 0, len(wf.Workflows))
	for _, w := range wf.Workflows {
		fields := map[string]any{"active": w.State == "active", "url": w.HTMLURL}
		// When the definition was written and when it last changed. A run
		// whose duration jumps on the day days_since_change goes back to
		// zero is a workflow that was edited, which had no record here.
		if !w.CreatedAt.IsZero() {
			fields["age_days"] = int(now.Sub(w.CreatedAt).Hours() / 24)
		}
		if !w.UpdatedAt.IsZero() {
			fields["days_since_change"] = int(now.Sub(w.UpdatedAt).Hours() / 24)
		}
		points = append(points, sink.Point{
			Measurement: "gh_workflow",
			Tags: merge(base, map[string]string{
				"workflow": w.Name, "path": w.Path, "state": w.State,
			}),
			Fields: fields,
			Time:   now,
		})
	}
	return points, nil
}

func twoDigits(n int) string { return fmt.Sprintf("%02d", n) }

// Discussions collects the forum half of a repository.
//
// Discussions are invisible to every issue and pull request endpoint, and an
// answered question is a support cost that never appears in the issue numbers.
// One GraphQL query per repository covers them.
type Discussions struct {
	// First, Comments and Replies are the three page sizes, and together
	// they are what the query costs: GitHub charges for the nodes asked for,
	// not the nodes returned. Measured against the live API on 2026-09-11 on
	// a repository with no discussions at all, as first/comments/replies:
	//
	//	50/20/20  cost 11
	//	50/10/10  cost  6
	//	10/20/20  cost  2
	//	10/10/10  cost  1
	//
	// Zero means fifty, twenty and twenty, which is the backfill's shape and
	// the one the probe uses. A sweep asks for ten threads with the same
	// twenty comments and twenty replies each: the threads are newest-updated
	// first, so the ten most recently touched are the ones whose rows can
	// have moved, but comments and replies arrive oldest first, so a smaller
	// page there is not a fresher one, it is the eleventh comment of a busy
	// thread never being recorded by a sweep.
	First    int
	Comments int
	Replies  int
	Walk     Walk
	// Login is the account being collected, so a comment reached from here
	// carries the same identity as the same comment reached from the account's
	// own walk over everything it wrote anywhere.
	Login string
}

const discussionsQuery = `
query($owner: String!, $name: String!, $first: Int!, $comments: Int!, $replies: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    discussions(first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes {
        number title url createdAt updatedAt isAnswered upvoteCount
        answerChosenAt answerChosenBy { login }
        answer { author { login } }
        closed closedAt stateReason
        author { login }
        category { name isAnswerable }
        reactions { totalCount }
        comments(first: $comments) {
          totalCount
          nodes {
            databaseId createdAt url upvoteCount isAnswer
            author { login }
            replies(first: $replies) {
              totalCount
              nodes { databaseId createdAt url upvoteCount author { login } }
            }
          }
        }
      }
    }
  }
}`

type discussionNode struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	URL            string     `json:"url"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	IsAnswered     bool       `json:"isAnswered"`
	UpvoteCount    int        `json:"upvoteCount"`
	AnswerChosenAt *time.Time `json:"answerChosenAt"`
	AnswerChosenBy *struct {
		Login string `json:"login"`
	} `json:"answerChosenBy"`
	Answer *struct {
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"answer"`
	Closed      bool       `json:"closed"`
	ClosedAt    *time.Time `json:"closedAt"`
	StateReason string     `json:"stateReason"`
	Author      *struct {
		Login string `json:"login"`
	} `json:"author"`
	Category struct {
		Name         string `json:"name"`
		IsAnswerable bool   `json:"isAnswerable"`
	} `json:"category"`
	Reactions count `json:"reactions"`
	Comments  struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			DatabaseID  int64     `json:"databaseId"`
			CreatedAt   time.Time `json:"createdAt"`
			URL         string    `json:"url"`
			UpvoteCount int       `json:"upvoteCount"`
			IsAnswer    bool      `json:"isAnswer"`
			Author      *struct {
				Login string `json:"login"`
			} `json:"author"`
			Replies struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					DatabaseID  int64     `json:"databaseId"`
					CreatedAt   time.Time `json:"createdAt"`
					URL         string    `json:"url"`
					UpvoteCount int       `json:"upvoteCount"`
					Author      *struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"replies"`
		} `json:"nodes"`
	} `json:"comments"`
}

// context is the thread's own state, rendered onto every comment in it the
// same way the account-wide walk renders it, so the two converge on one row.
func (d *discussionNode) context() *discussionContext {
	c := &discussionContext{
		Created:     d.CreatedAt,
		Answered:    d.IsAnswered,
		AnsweredAt:  d.AnswerChosenAt,
		Closed:      d.Closed,
		ClosedAt:    d.ClosedAt,
		StateReason: d.StateReason,
		Category:    d.Category.Name,
		Answerable:  d.Category.IsAnswerable,
	}
	if a := d.Answer; a != nil && a.Author != nil {
		c.AnsweredBy = a.Author.Login
	}
	if by := d.AnswerChosenBy; by != nil {
		c.ChosenBy = by.Login
	}
	return c
}

func (d Discussions) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	first, comments, replies := d.First, d.Comments, d.Replies
	if first <= 0 {
		first = 50
	}
	if comments <= 0 {
		comments = 20
	}
	if replies <= 0 {
		replies = 20
	}
	var res struct {
		Repository struct {
			Discussions struct {
				TotalCount int              `json:"totalCount"`
				PageInfo   pageInfo         `json:"pageInfo"`
				Nodes      []discussionNode `json:"nodes"`
			} `json:"discussions"`
		} `json:"repository"`
	}
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	var points []sink.Point
	after := ""
	most := d.Walk.limit(1)
	for page := 1; page <= most; page++ {
		vars := map[string]any{
			"owner": repo.Owner, "name": repo.Name,
			"first": first, "comments": comments, "replies": replies,
		}
		if after != "" {
			vars["after"] = after
		}
		res.Repository.Discussions.Nodes = nil
		if err := c.GraphQL(ctx, discussionsQuery, vars, &res); err != nil {
			// Discussions can be switched off, which is not a failure. A
			// spent budget answers the same call and is.
			if isSkippableGraphQL(err) {
				return points, nil
			}
			return points, err
		}
		nodes := res.Repository.Discussions.Nodes
		points = append(points, discussionPoints(nodes, base, repo.FullName, d.Login)...)
		pi := res.Repository.Discussions.PageInfo
		if !pi.HasNextPage || len(nodes) == 0 || d.Walk.past(nodes[len(nodes)-1].UpdatedAt) {
			break
		}
		after = pi.EndCursor
	}
	return points, nil
}

func discussionPoints(nodes []discussionNode, base map[string]string, full, user string) []sink.Point {
	var points []sink.Point
	for i := range nodes {
		d := &nodes[i]
		replies := 0
		for _, cm := range d.Comments.Nodes {
			replies += cm.Replies.TotalCount
		}
		fields := map[string]any{
			// Comments are the answers to the discussion itself; replies are
			// the answers to those. GitHub's own count on the page is the two
			// added together, and keeping them apart is what says whether a
			// thread was a conversation or a queue of separate answers.
			"comments": d.Comments.TotalCount, "replies": replies,
			"reactions": d.Reactions.TotalCount,
			"upvotes":   d.UpvoteCount, "url": d.URL, "title": d.Title,
		}
		// How long a question waited for its answer is the number that says
		// whether the forum is working.
		if d.AnswerChosenAt != nil {
			fields["seconds_to_answer"] = int(d.AnswerChosenAt.Sub(d.CreatedAt).Seconds())
		}
		// Whether it has its answer is a field and not a tag: the row is
		// dated when the discussion was opened and the answer comes later,
		// so as the tag `answered` a question answered after the first sweep
		// was two rows at the same instant for ever. Under a new name so a
		// database that already holds the tag column keeps accepting writes.
		fields["has_answer"] = d.IsAnswered
		// Closing is a separate act from answering, and a thread closed as
		// OUTDATED or DUPLICATE is not the same fact as one closed RESOLVED.
		fields["closed"] = d.Closed
		if d.StateReason != "" {
			fields["state_reason"] = d.StateReason
		}
		if d.ClosedAt != nil {
			fields["seconds_to_close"] = int(d.ClosedAt.Sub(d.CreatedAt).Seconds())
		}
		points = append(points, sink.Point{
			Measurement: "gh_discussion",
			Tags: merge(base, map[string]string{
				"category": d.Category.Name,
				"author":   login(d.Author),
				// Whether the category even allows an answer. A question with
				// no answer in a category that cannot be answered is not an
				// unanswered question.
				"answerable": boolTag(d.Category.IsAnswerable),
				"number":     strconv.Itoa(d.Number),
			}),
			Fields: fields,
			Time:   d.CreatedAt,
		})

		// Every comment on the thread, and every reply to a comment. Until
		// this existed a discussion was a row with a comment count and the
		// answers themselves were nowhere: not the account's own replies, not
		// anyone else's, and not the reply to a reply, which is where most of
		// the back and forth in a thread happens.
		for _, cm := range d.Comments.Nodes {
			points = append(points, discussionComment(user, full, d, cm.DatabaseID, 0,
				login(cm.Author), cm.URL, cm.UpvoteCount, cm.IsAnswer, cm.CreatedAt))
			for _, rp := range cm.Replies.Nodes {
				points = append(points, discussionComment(user, full, d, rp.DatabaseID,
					cm.DatabaseID, login(rp.Author), rp.URL, rp.UpvoteCount, false, rp.CreatedAt))
			}
		}
	}
	return points
}

// discussionComment renders one comment or reply with the identity the
// account-wide walk gives the same thing, so the two converge on one row
// instead of writing two.
func discussionComment(user, full string, d *discussionNode, id, replyTo int64,
	by, url string, upvotes int, isAnswer bool, when time.Time,
) sink.Point {
	fields := map[string]any{
		"comments": 1, "upvotes": upvotes, "title": d.Title,
		"answers": boolInt(isAnswer), "url": url,
	}
	if replyTo != 0 {
		fields["reply_to"] = replyTo
	}
	// The same fields the account-wide walk writes. Leaving them out here
	// would give gh_discussion_comment two shapes, and half its rows would
	// answer "was this thread ever answered" with nothing at all.
	d.context().addTo(fields)
	return sink.Point{
		Measurement: "gh_discussion_comment",
		Tags: map[string]string{
			"user": user, "repo": full,
			"own":       boolTag(isOwn(full, user)),
			"is_answer": boolTag(isAnswer),
			"is_reply":  boolTag(replyTo != 0),
			"author":    by,
			"comment":   strconv.FormatInt(id, 10),
			"number":    strconv.Itoa(d.Number),
		},
		Fields: fields,
		Time:   when,
	}
}
