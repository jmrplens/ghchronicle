package collect

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Pulls collects pull requests and issues one by one, not as counts.
//
// A count of open pull requests says nothing about how the work actually goes.
// The interesting numbers are durations: how long until someone reviewed it,
// how long until it merged, how big the diff was, how many review rounds it
// took. All of them are dated facts, stamped when the pull request closed, so
// "how fast were we merging in July" stays answerable forever.
//
// One GraphQL query per repository covers both pull requests and issues.
type Pulls struct {
	// First bounds how many of each are fetched per page. The newest are the
	// ones that change; older ones are already written and immutable.
	First int
	// Threads bounds the review threads fetched per pull request. It is its
	// own knob because it is what the query costs, and the gateway gives up
	// at ten seconds whatever the point budget says.
	//
	// Measured against the live API on 2026-09-10, jmrplens/gitlab-mcp-server
	// at First=50, both connections in one query:
	//
	//	today's selection            cost  2, 1,650 nodes, 6.1 s
	//	the fields below, no threads cost  3, 1,750 nodes, 8.0 s
	//	Threads=5                    cost  6, 2,250 nodes, 8.5 s
	//	Threads=10                   cost  8, 2,750 nodes, 7.6 to 9.5 s
	//	Threads=50                   cost 28, 6,750 nodes, 9.8 s
	//
	// So ten, not the fifty the audit asked for: fifty runs against the
	// ceiling and costs fourteen times the query it replaces. Ten captures
	// 653 of the 804 threads on the newest fifty pull requests of the nine
	// busiest repositories (81 per cent; 23 of those 370 pull requests carry
	// more than ten, the worst 37), and gh_pull_request.review_threads
	// carries the true total so the truncation is visible rather than
	// silent.
	Threads int
	// Walk pages further back. A sweep wants one page; a backfill walks until
	// the items are older than its bound, which the UPDATED_AT ordering makes
	// a single cursor walk per connection.
	Walk Walk
}

const pullsQuery = `
query($owner: String!, $name: String!, $first: Int!, $threads: Int!, $prAfter: String, $issueAfter: String, $withPRs: Boolean!, $withIssues: Boolean!) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first, after: $prAfter, orderBy: {field: UPDATED_AT, direction: DESC}) @include(if: $withPRs) {
      pageInfo { hasNextPage endCursor }
      nodes {
        updatedAt url
        number title state isDraft createdAt closedAt mergedAt
        additions deletions changedFiles
        author { login __typename }
        authorAssociation
        labels(first: 10) { totalCount nodes { name } }
        reviews(first: 20) {
          totalCount
          nodes { author { login __typename } state submittedAt url }
        }
        comments { totalCount }
        commits { totalCount }
        reviewDecision
        timelineItems(first: 1, itemTypes: [PULL_REQUEST_REVIEW]) {
          nodes { ... on PullRequestReview { createdAt } }
        }
        totalCommentsCount
        mergeable mergeStateStatus
        baseRefName headRefName
        mergedBy { login }
        mergeCommit { oid }
        reviewRequests { totalCount }
        stackEntry { position stack { number size } }
        reviewThreads(first: $threads) {
          totalCount
          nodes {
            isResolved isOutdated path subjectType
            resolvedBy { login }
            comments(first: 1) {
              totalCount
              nodes { databaseId createdAt author { login __typename } }
            }
          }
        }
      }
    }
    issues(first: $first, after: $issueAfter, orderBy: {field: UPDATED_AT, direction: DESC}) @include(if: $withIssues) {
      pageInfo { hasNextPage endCursor }
      nodes {
        updatedAt url
        number state createdAt closedAt
        author { login __typename }
        comments { totalCount }
        reactions { totalCount }
        labels(first: 10) { nodes { name } }
        stateReason
        parent { number }
        subIssuesSummary { total completed }
        assignees(first: 1) { nodes { login } }
        milestone { title }
        closedByPullRequestsReferences(first: 1, includeClosedPrs: true) {
          totalCount
          nodes { number }
        }
      }
    }
  }
}`

type pullNode struct {
	UpdatedAt      time.Time  `json:"updatedAt"`
	URL            string     `json:"url"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	IsDraft        bool       `json:"isDraft"`
	CreatedAt      time.Time  `json:"createdAt"`
	ClosedAt       *time.Time `json:"closedAt"`
	MergedAt       *time.Time `json:"mergedAt"`
	Additions      int        `json:"additions"`
	Deletions      int        `json:"deletions"`
	ChangedFiles   int        `json:"changedFiles"`
	ReviewDecision string     `json:"reviewDecision"`
	Author         *actor     `json:"author"`
	// AuthorAssociation is what separates an outside contribution from the
	// owner's own work: OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR,
	// FIRST_TIME_CONTRIBUTOR or NONE.
	AuthorAssociation string `json:"authorAssociation"`
	Labels            struct {
		TotalCount int         `json:"totalCount"`
		Nodes      []labelNode `json:"nodes"`
	} `json:"labels"`
	Reviews struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			// The kind of author matters here: on this account the first
			// review of nearly every pull request is a bot's, submitted
			// seconds after it opened, and a time-to-first-review that
			// counts those measures the bot rather than the wait.
			Author      *actor     `json:"author"`
			State       string     `json:"state"`
			SubmittedAt *time.Time `json:"submittedAt"`
			URL         string     `json:"url"`
		} `json:"nodes"`
	} `json:"reviews"`
	Comments      count `json:"comments"`
	Commits       count `json:"commits"`
	TimelineItems struct {
		Nodes []struct {
			CreatedAt *time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"timelineItems"`
	// TotalCommentsCount is the whole conversation, review comments included.
	// Comments.TotalCount is only the issue-style thread on the pull request:
	// measured on jmrplens/gitlab-mcp-server PR 631, 2 against 10.
	TotalCommentsCount int    `json:"totalCommentsCount"`
	Mergeable          string `json:"mergeable"`
	MergeStateStatus   string `json:"mergeStateStatus"`
	BaseRefName        string `json:"baseRefName"`
	HeadRefName        string `json:"headRefName"`
	MergedBy           *struct {
		Login string `json:"login"`
	} `json:"mergedBy"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	ReviewRequests count `json:"reviewRequests"`
	// StackEntry is this pull request's own place in its stack. The audit
	// reached for PullRequestStack.entries, which is a node per sibling per
	// pull request; stackEntry answers the same question for one point and
	// fifty nodes over a page of fifty.
	StackEntry *struct {
		Position int       `json:"position"`
		Stack    pullStack `json:"stack"`
	} `json:"stackEntry"`
	ReviewThreads struct {
		TotalCount int          `json:"totalCount"`
		Nodes      []threadNode `json:"nodes"`
	} `json:"reviewThreads"`
}

// pullStack is the stack a pull request belongs to: which stack it is, and how
// many pull requests are queued in it.
type pullStack struct {
	Number int `json:"number"`
	Size   int `json:"size"`
}

// threadNode is one review thread: a conversation anchored to a line or a
// file, which GitHub keeps open until someone resolves it.
type threadNode struct {
	IsResolved  bool   `json:"isResolved"`
	IsOutdated  bool   `json:"isOutdated"`
	Path        string `json:"path"`
	SubjectType string `json:"subjectType"`
	ResolvedBy  *struct {
		Login string `json:"login"`
	} `json:"resolvedBy"`
	Comments struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			// DatabaseID identifies the thread, because nothing else does.
			// Measured on pull request 691 of jmrplens/gitlab-mcp-server:
			// two threads, same file, same author, same second, both
			// unresolved and not outdated, differing only in the line. Tagged
			// by the audit's five tags they are one series at one timestamp,
			// and InfluxDB keeps whichever arrives last.
			DatabaseID int64     `json:"databaseId"`
			CreatedAt  time.Time `json:"createdAt"`
			Author     *actor    `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

// actor is whoever wrote something, and whether it is a GitHub App. Nearly
// every review thread on this account is opened by one: measured on
// jmrplens/Cloudflare-DNS-Updater, every thread author in a page of fifteen
// pull requests had __typename Bot.
type actor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

type issueNode struct {
	UpdatedAt time.Time  `json:"updatedAt"`
	URL       string     `json:"url"`
	Number    int        `json:"number"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	ClosedAt  *time.Time `json:"closedAt"`
	Author    *actor     `json:"author"`
	Comments  count      `json:"comments"`
	Reactions count      `json:"reactions"`
	Labels    struct {
		Nodes []labelNode `json:"nodes"`
	} `json:"labels"`
	// StateReason separates an issue closed because the work was done from
	// one closed because it was dropped. Without it every time-to-close
	// number mixes the two.
	StateReason string `json:"stateReason"`
	Parent      *struct {
		Number int `json:"number"`
	} `json:"parent"`
	SubIssuesSummary struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
	} `json:"subIssuesSummary"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	// ClosedByPullRequestsReferences is asked of the issue because that is
	// where the row is written, and it is one field on a node already being
	// fetched. Not for the reason the audit gives: it says the pull request
	// side answers 0 where the issue side answers 1, and that does not
	// reproduce. Checked on 2026-09-10 on all five pairs the page offers
	// (593/583, 590/574, 587/570, 684/683, 669/644), both directions answer
	// 1 every time.
	ClosedBy struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"closedByPullRequestsReferences"`
}

// pullConnection is one page of a repository's pull requests, and
// issueConnection one of its issues. Two types rather than one because the
// nodes differ: an issue has no review of any kind.
type pullConnection struct {
	PageInfo pageInfo   `json:"pageInfo"`
	Nodes    []pullNode `json:"nodes"`
}

type issueConnection struct {
	PageInfo pageInfo    `json:"pageInfo"`
	Nodes    []issueNode `json:"nodes"`
}

func (p Pulls) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	first := p.First
	if first <= 0 {
		first = 50
	}
	threads := p.Threads
	if threads <= 0 {
		threads = 10
	}
	var res struct {
		Repository struct {
			PullRequests pullConnection  `json:"pullRequests"`
			Issues       issueConnection `json:"issues"`
		} `json:"repository"`
	}
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point

	// Two cursors, one per connection, walked in the same query while both
	// have pages left and then one at a time. Each stops when its items are
	// older than the bound or when the API says there is no next page.
	prAfter, issueAfter := "", ""
	withPRs, withIssues := true, true
	most := p.Walk.limit(1)
	for page := 1; page <= most && (withPRs || withIssues); page++ {
		vars := map[string]any{
			"owner": repo.Owner, "name": repo.Name, "first": first,
			"threads": threads,
			"withPRs": withPRs, "withIssues": withIssues,
		}
		if prAfter != "" {
			vars["prAfter"] = prAfter
		}
		if issueAfter != "" {
			vars["issueAfter"] = issueAfter
		}
		if err := c.GraphQL(ctx, pullsQuery, vars, &res); err != nil {
			var tooLarge *ghapi.TooLargeError
			if errors.As(err, &tooLarge) && first > 10 {
				// Same cursor, half the page. A pull request carries its
				// reviews, its timeline and now its review threads, so a
				// hundred of them is more than the gateway will finish for a
				// busy repository. Measured on 2026-09-10, three attempts out
				// of three: First=100 with Threads=10 on
				// jmrplens/gitlab-mcp-server is an HTML 502 after 10.8 s,
				// where the same query at 50 answers in 7.6 to 9.5 s. The
				// backfill therefore asks for 50 as well, and this path is
				// the safety net for a repository busier than the one it was
				// measured on rather than the normal way a backfill runs.
				first /= 2
				vars["first"] = first
				page--
				continue
			}
			return points, err
		}
		if withPRs {
			points = append(points, p.pullPoints(res.Repository.PullRequests.Nodes, base, now)...)
			pi, nodes := res.Repository.PullRequests.PageInfo, res.Repository.PullRequests.Nodes
			withPRs = pi.HasNextPage && len(nodes) > 0 && !p.Walk.past(nodes[len(nodes)-1].UpdatedAt)
			prAfter = pi.EndCursor
		}
		if withIssues {
			points = append(points, p.issuePoints(res.Repository.Issues.Nodes, base, now)...)
			pi, nodes := res.Repository.Issues.PageInfo, res.Repository.Issues.Nodes
			withIssues = pi.HasNextPage && len(nodes) > 0 && !p.Walk.past(nodes[len(nodes)-1].UpdatedAt)
			issueAfter = pi.EndCursor
		}
		res.Repository.PullRequests.Nodes = nil
		res.Repository.Issues.Nodes = nil
	}
	return points, nil
}

// pullPoints renders one page of pull requests and their reviews.
func (p Pulls) pullPoints(nodes []pullNode, base map[string]string, now time.Time) []sink.Point {
	var points []sink.Point
	for i := range nodes {
		pr := &nodes[i]
		fields := map[string]any{
			"additions": pr.Additions, "deletions": pr.Deletions,
			"changed_files": pr.ChangedFiles, "churn": pr.Additions + pr.Deletions,
			"reviews": pr.Reviews.TotalCount, "comments": pr.Comments.TotalCount,
			"commits": pr.Commits.TotalCount,
			// The page on GitHub. A dashboard row that names an item and
			// cannot open it makes the reader search for it by hand.
			"url": pr.URL,
			// The whole conversation, review comments included, which is what
			// "comments" above is not.
			"total_comments":  pr.TotalCommentsCount,
			"review_requests": pr.ReviewRequests.TotalCount,
			// The true number of threads, which is not the number of rows
			// written below: a pull request carrying more than Threads of
			// them would otherwise be indistinguishable from one carrying
			// exactly that many.
			"review_threads": pr.ReviewThreads.TotalCount,
			// Branch names are unbounded, so they are fields. Measured on
			// 2026-09-10: 27 of the newest 50 pull requests of
			// jmrplens/gitlab-mcp-server have a base other than main, which
			// is what a stack looks like from the outside.
			"base_ref": pr.BaseRefName,
			"head_ref": pr.HeadRefName,
			// The count the way gh_issue carries it, and the names beside
			// it: a table wants to say "dependencies, breaking", not "2".
			"labels": pr.Labels.TotalCount,
		}
		// What a reader knows the pull request by. Without it a table of
		// pull requests is a number and a link, and the number is only
		// meaningful once the link has been opened.
		setNonEmpty(fields, "title", pr.Title)
		setNonEmpty(fields, "author_association", pr.AuthorAssociation)
		setNonEmpty(fields, "label_names", labelNames(pr.Labels.Nodes))
		if pr.MergedBy != nil {
			fields["merged_by"] = pr.MergedBy.Login
		}
		if pr.MergeCommit != nil {
			// The join to gh_commit. Nothing else ties a pull request to the
			// commit that actually shipped.
			fields["merge_commit"] = pr.MergeCommit.OID
		}
		if pr.State == "OPEN" {
			// Current state, true only while the pull request is open.
			// Measured: merged pull request 689 of
			// jmrplens/gitlab-mcp-server still answers CONFLICTING and DIRTY,
			// which written into a row dated the day it merged would be a
			// lie that never expires.
			fields["mergeable"] = pr.Mergeable
			fields["merge_state"] = pr.MergeStateStatus
		}
		if pr.StackEntry != nil {
			// A stack of five is one delivery, not five. Measured on
			// 2026-09-10: 38 of the newest 50 pull requests of
			// jmrplens/gitlab-mcp-server belong to a stack, so a pace panel
			// counting rows overcounts by more than two to one. stack is the
			// stack's own number and not one of its members: stack 693 holds
			// pull requests 691 and 692. That is what makes it the thing to
			// count distinct values of, which is the only recipe that holds
			// when a stack is abandoned half merged; position 1 is the one
			// based on the default branch and position stack_size the top.
			fields["stack"] = pr.StackEntry.Stack.Number
			fields["stack_size"] = pr.StackEntry.Stack.Size
			fields["stack_position"] = pr.StackEntry.Position
		}
		// Time to first review is the one that says whether anyone is waiting.
		if n := pr.TimelineItems.Nodes; len(n) > 0 && n[0].CreatedAt != nil {
			fields["seconds_to_first_review"] = int(n[0].CreatedAt.Sub(pr.CreatedAt).Seconds())
		}
		// And the same wait counting other people only. Measured on this
		// account over 140 pull requests: the median of the field above is
		// five seconds, because 132 of them were first reviewed by a bot
		// within a minute of opening, and the earliest non-bot review on
		// every one that had any was the author's own reply to the bot, two
		// hours later. The first field answers "did anything look at it";
		// this one answers "did anyone else", and is absent when nobody has.
		if first, ok := firstHumanReview(pr); ok {
			fields["seconds_to_first_human_review"] = int(first.Sub(pr.CreatedAt).Seconds())
		}
		if pr.MergedAt != nil {
			fields["seconds_to_merge"] = int(pr.MergedAt.Sub(pr.CreatedAt).Seconds())
		}
		if pr.ClosedAt != nil {
			fields["seconds_open"] = int(pr.ClosedAt.Sub(pr.CreatedAt).Seconds())
		} else {
			fields["seconds_open"] = int(now.Sub(pr.CreatedAt).Seconds())
		}
		// A closed pull request is stamped when it closed. An open one has no
		// such date, so it is stamped at the start of the UTC day: stamping it
		// at the instant of the sweep wrote a fresh copy every hour, and any
		// panel counting rows reported twenty-four open pull requests where
		// there was one.
		stamp := now.UTC().Truncate(24 * time.Hour)
		if pr.ClosedAt != nil {
			stamp = *pr.ClosedAt
		}
		// Whether it is a draft and what the reviewers decided are fields,
		// not tags: both move while the row's date stays, since an open pull
		// request is rewritten at the same start of day until it closes, so
		// as tags a draft marked ready for review was two rows at the same
		// instant for ever. `state` stays a tag because its date moves with
		// it: the open row is the day's and the closed row is stamped when
		// it closed. Both are under new names so a database that already
		// holds the tag columns keeps accepting writes.
		fields["is_draft"] = pr.IsDraft
		// GitHub answers null here for a pull request nobody has reviewed and
		// none is required of, which is almost all of them: measured on
		// 2026-09-10 over 35 repositories, 555 of 558. Written always, with
		// the tag fallback where GitHub says nothing, so the column exists
		// from the first row and a query naming it does not fail on a
		// database where no pull request has been reviewed yet.
		fields["decision"] = orNone(pr.ReviewDecision)
		points = append(points, sink.Point{
			Measurement: "gh_pull_request",
			Tags: merge(base, map[string]string{
				// The number is what makes this a per-item measurement. Without
				// it two pull requests by the same author in the same state
				// share a series and the second silently replaces the first.
				"number": strconv.Itoa(pr.Number),
				"state":  pr.State, "author": actorLogin(pr.Author),
			}),
			Fields: fields,
			Time:   stamp,
		})

		points = reviewPoints(pr, base, points)
		points = threadPoints(pr, base, points)
	}

	return points
}

// reviewPoints renders the reviews of one pull request, each as its own fact
// dated when it was submitted. Nothing else records who reviews: the count
// alone cannot answer whether the work is spread across people or resting on
// one.
func reviewPoints(pr *pullNode, base map[string]string, points []sink.Point) []sink.Point {
	for _, rv := range pr.Reviews.Nodes {
		if rv.SubmittedAt == nil {
			continue
		}
		points = append(points, sink.Point{
			Measurement: "gh_pull_request_review",
			Tags: merge(base, map[string]string{
				"number": strconv.Itoa(pr.Number),
				"author": actorLogin(pr.Author), "reviewer": actorLogin(rv.Author),
				// The same distinction gh_review_thread draws, so a reviewers
				// table can leave the bots out or show them apart: a bot
				// answering in seconds next to a person answering in hours is
				// two different questions in one median.
				"bot": boolTag(isBot(rv.Author)),
				// And whether the "reviewer" is the pull request's own
				// author, which is not a review either: GitHub refuses an
				// approval or a request for changes from the author, and
				// the COMMENTED reviews it records under their name are
				// their replies in the threads. Measured on this account,
				// 436 of the 1,460 review rows were the owner answering the
				// bots on his own pull requests, more than any reviewer
				// but one.
				"self": boolTag(isSelf(pr, rv.Author)),
			}),
			Fields: map[string]any{
				"reviews":           1,
				"seconds_to_review": int(rv.SubmittedAt.Sub(pr.CreatedAt).Seconds()),
				"url":               rv.URL,
				// The state is a field: an approval dismissed later reads
				// DISMISSED under the same submittedAt, so as a tag the
				// dismissal was a second row at the review's own instant.
				// Under a new name so a database that already holds the tag
				// column keeps accepting writes.
				"review_state": rv.State,
			},
			Time: *rv.SubmittedAt,
		})
	}
	return points
}

// threadPoints renders the review threads of one pull request.
//
// A thread is dated at its first comment, which is when the reviewer raised
// the point. That date never moves again, which is what makes re-collection
// converge: resolving a thread rewrites its one row instead of adding a
// second one dated at the resolution, which would count one objection twice.
// The connection returns comments oldest first, so comments(first: 1) is the
// opening one: checked against the live API on 2026-09-10 over every
// multi-comment thread of the newest twenty pull requests of
// jmrplens/gitlab-mcp-server, 10 of 10 ascending.
//
// Convergence is the reason isResolved and isOutdated are fields and not the
// tags the audit asked for. They are current state on a row whose timestamp
// is frozen at the thread's birth, so as tags a resolution starts a second
// series at that same timestamp and the unresolved row survives beside it
// forever. Measured on the newest fifty pull requests of
// jmrplens/gitlab-mcp-server: 44 of the 46 threads on the page are already
// resolved and 24 are outdated, so tagging them would leave 44 permanent
// unresolved ghosts on one repository, and "unresolved review threads" would
// only ever count upwards.
//
// This is the review debt, and nothing in the collector saw it before.
// Measured across the newest fifty pull requests of nine repositories: 804
// threads, of which 238 were still unresolved.
func threadPoints(pr *pullNode, base map[string]string, points []sink.Point) []sink.Point {
	for i := range pr.ReviewThreads.Nodes {
		th := &pr.ReviewThreads.Nodes[i]
		if len(th.Comments.Nodes) == 0 {
			// No first comment is no date to carry, and a point dated now
			// would be dated wrong. GitHub does not make such a thread; a
			// truncated response could.
			continue
		}
		opened := th.Comments.Nodes[0]
		fields := map[string]any{
			// A tag here would be one series per file of every repository.
			"path":     th.Path,
			"comments": th.Comments.TotalCount,
			// Fields, not tags: see the convergence note above. As integers
			// rather than booleans because the exporter reduces a per-item
			// measurement to a count plus the mean of its numbers, and the
			// mean of these two is the share of the debt that is paid off
			// and the share that no longer applies to the current code.
			"resolved": boolInt(th.IsResolved),
			"outdated": boolInt(th.IsOutdated),
		}
		if th.SubjectType != "" {
			fields["subject_type"] = th.SubjectType
		}
		if th.ResolvedBy != nil {
			fields["resolved_by"] = th.ResolvedBy.Login
		}
		points = append(points, sink.Point{
			Measurement: "gh_review_thread",
			Tags: merge(base, map[string]string{
				// What makes this a per-item measurement, the way number does
				// for gh_pull_request. The identity is the id of the comment
				// that opened the thread, which never changes; a thread of
				// its own has no numeric id. Without it the audit's tag set
				// does not identify a thread: measured on the newest fifty
				// pull requests of jmrplens/gitlab-mcp-server, 9 groups of
				// threads agree on every other tag and on the second they
				// were opened, the worst of them five threads deep, so
				// InfluxDB would keep one of the five. Zero is not a value
				// GitHub returns, so the fallback is unreachable in
				// practice, and costs a collision rather than a dropped row
				// if it is not.
				// Every tag here is immutable, which is what lets the row
				// converge: the thread, the pull request it sits on and who
				// opened it cannot change after the fact.
				"thread": threadTag(opened.DatabaseID),
				"number": strconv.Itoa(pr.Number),
				"author": actorLogin(opened.Author),
				// Who is asking matters more than usual here: on this account
				// the reviewers are review bots, and a backlog of bot threads
				// reads differently from a backlog of human ones.
				"bot": boolTag(isBot(opened.Author)),
			}),
			Fields: fields,
			Time:   opened.CreatedAt,
		})
	}
	return points
}

// issuePoints renders one page of issues.
func (p Pulls) issuePoints(nodes []issueNode, base map[string]string, now time.Time) []sink.Point {
	var points []sink.Point
	for i := range nodes {
		is := &nodes[i]
		fields := map[string]any{
			"comments": is.Comments.TotalCount, "reactions": is.Reactions.TotalCount,
			"labels": len(is.Labels.Nodes), "url": is.URL,
			// How far an epic has got. Measured: issue 365 of
			// jmrplens/gitlab-mcp-server has 100 children, 94 of them closed.
			"sub_issues_total":     is.SubIssuesSummary.Total,
			"sub_issues_completed": is.SubIssuesSummary.Completed,
			// The pull request that closed this issue, 0 when none did,
			// asked of the issue side for the reason on ClosedBy above.
			"pull_request": closedByPR(is),
		}
		setNonEmpty(fields, "label_names", labelNames(is.Labels.Nodes))
		if is.ClosedAt != nil {
			fields["seconds_to_close"] = int(is.ClosedAt.Sub(is.CreatedAt).Seconds())
		} else {
			fields["seconds_open"] = int(now.Sub(is.CreatedAt).Seconds())
		}
		is.planning(fields)
		stamp := now.UTC().Truncate(24 * time.Hour)
		if is.ClosedAt != nil {
			stamp = *is.ClosedAt
		}
		points = append(points, sink.Point{
			Measurement: "gh_issue",
			Tags:        merge(base, is.tags()),
			Fields:      fields,
			Time:        stamp,
		})
	}
	return points
}

// ItemCounts is how many pull requests and issues a repository has ever had,
// as the totals family last reported them.
type ItemCounts struct {
	Pulls  int
	Issues int
}

// Most is the larger of the two, which is the page both connections share.
func (c ItemCounts) Most() int { return max(c.Pulls, c.Issues) }

// ReadItemCounts reads the lifetime counts out of the points the totals
// family emitted, keyed by full name, and writes them over into.
//
// The page below is sized from these rather than from a query of its own
// because the numbers are already on their way to the sinks twice a day, and
// gh_repo_total is a wire format a dashboard reads, so its field names are
// the ones here that cannot drift.
func ReadItemCounts(points []sink.Point, into map[string]ItemCounts) {
	for i := range points {
		p := &points[i]
		if p.Measurement != "gh_repo_total" {
			continue
		}
		full := p.Tags["full_name"]
		if full == "" {
			continue
		}
		into[full] = ItemCounts{
			Pulls:  fieldSum(p.Fields, "pulls_open", "pulls_merged", "pulls_closed"),
			Issues: fieldSum(p.Fields, "issues_open", "issues_closed"),
		}
	}
}

// fieldSum adds the named integer fields of a point, reading a missing one
// as zero.
func fieldSum(fields map[string]any, names ...string) int {
	total := 0
	for _, name := range names {
		if n, ok := fields[name].(int); ok {
			total += n
		}
	}
	return total
}

// PageFor is the page worth asking for when a repository is known to hold
// total items: the smallest of the sizes the gateway prices differently that
// still covers them all, so the response is exactly what a page of fifty
// would have been.
//
// GitHub charges a query for the nodes it could return, not the nodes it
// does. Measured against the live API on 2026-09-11 with the pull request
// query at Threads=10, first 50 costs 8, 20 costs 3, 10 costs 2 and 5 costs
// 1, on a repository with no pull requests at all as much as on the busiest.
// Fifty is also the page the backfill uses and the one this walk halves
// from, so nothing is ever asked for beyond it.
func PageFor(total int) int {
	for _, size := range [...]int{5, 10, 20} {
		if total <= size {
			return size
		}
	}
	return 50
}

// pageInfo is GraphQL's cursor, shared by every connection walked here.
type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// login handles a deleted account, where GitHub returns a null author.
func login(a *struct {
	Login string `json:"login"`
},
) string {
	if a == nil {
		return "(ghost)"
	}
	return a.Login
}

// tags are the identities one issue carries: the number, who opened it and
// the state, whose date moves with it (the open row is the day's, the closed
// row is stamped when it closed), so it never doubles a row.
func (is *issueNode) tags() map[string]string {
	return map[string]string{
		"number": strconv.Itoa(is.Number),
		"state":  is.State, "author": actorLogin(is.Author),
	}
}

// planning is what can change about an issue after the date its row carries:
// why it closed, who holds it, which milestone and which parent it sits
// under. Each was a tag, and each moved the row into a second series at the
// same instant when it changed, since a closed issue's date is fixed at its
// closing and an open one's at the start of the day. Fields converge instead.
// Every one is written, with the tag fallback where GitHub says nothing and
// zero for no parent, so the columns exist from the first row and a query
// naming one does not fail on a database where no issue has been assigned
// yet. Under new names so a database that already holds the tag columns
// keeps accepting writes.
//
// resolution is the one that changes what the other measurements mean:
// without it an issue closed because the work was done and one closed because
// it was dropped are the same row, and every time-to-close average mixes
// them. Measured over 50 issues of jmrplens/gitlab-mcp-server: COMPLETED 27,
// NOT_PLANNED 1, and 22 open ones with no reason at all.
func (is *issueNode) planning(fields map[string]any) {
	fields["resolution"] = orNone(is.StateReason)
	parent := 0
	if is.Parent != nil {
		parent = is.Parent.Number
	}
	fields["parent_issue"] = parent
	assignee := ""
	if n := is.Assignees.Nodes; len(n) > 0 {
		assignee = n[0].Login
	}
	fields["assigned_to"] = orNone(assignee)
	milestone := ""
	if is.Milestone != nil {
		milestone = is.Milestone.Title
	}
	fields["milestone_title"] = orNone(milestone)
}

// closedByPR is the number of the pull request that closed an issue, or 0.
func closedByPR(is *issueNode) int {
	if n := is.ClosedBy.Nodes; len(n) > 0 {
		return n[0].Number
	}
	return 0
}

// actorLogin handles a deleted account, where GitHub returns a null author,
// and spells an app the way REST does, so one bot is one author everywhere.
func actorLogin(a *actor) string {
	if a == nil {
		return "(ghost)"
	}
	return appLogin(a.Login, a.Typename == "Bot")
}

// threadTag renders a review thread's identity, with a fallback because the
// sink drops a point's tag when its value is empty.
func threadTag(id int64) string {
	if id == 0 {
		return noneTag
	}
	return strconv.FormatInt(id, 10)
}

// isBot says whether an author is an automated reviewer: a GitHub App
// (__typename Bot, which is what sourcery-ai and coderabbitai are) or a
// login carrying the "[bot]" suffix REST gives the same accounts. A deleted
// author is not a bot: GitHub retires people, not apps, into ghost.
func isBot(a *actor) bool {
	if a == nil {
		return false
	}
	return a.Typename == "Bot" || strings.HasSuffix(a.Login, "[bot]")
}

// isSelf says whether a review's author is the pull request's own author.
// Two deleted accounts are not one person: a ghost is never self.
func isSelf(pr *pullNode, reviewer *actor) bool {
	return pr.Author != nil && reviewer != nil && pr.Author.Login == reviewer.Login
}

// firstHumanReview is when somebody else first reviewed the pull request,
// over the reviews the query fetched. Bots are left out, and so is the
// author: measured on this account, every one of the 91 pull requests with a
// non-bot review had the author's own reply to a bot as the earliest, so a
// wait that counted those measured how fast the owner answers sourcery-ai,
// not how long the work waited for a reviewer.
//
// GitHub serves the connection oldest first, so the first person in it is
// the first person there was, unless the page ended before one appeared: a
// pull request whose first twenty reviews are all bots and self-replies
// reports no human wait rather than a wrong one, and its reviews count says
// why.
func firstHumanReview(pr *pullNode) (time.Time, bool) {
	var first time.Time
	found := false
	for _, rv := range pr.Reviews.Nodes {
		if rv.SubmittedAt == nil || isBot(rv.Author) || isSelf(pr, rv.Author) {
			continue
		}
		if !found || rv.SubmittedAt.Before(first) {
			first, found = *rv.SubmittedAt, true
		}
	}
	return first, found
}

// labelNode is one label on a pull request or an issue.
type labelNode struct {
	Name string `json:"name"`
}

// labelNames joins the label names an item carries, in the order GitHub
// lists them, empty when it carries none so the caller writes no field at
// all. A field and not a tag: nine labels on one pull request is one row,
// not nine series.
func labelNames(nodes []labelNode) string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Name != "" {
			names = append(names, n.Name)
		}
	}
	return strings.Join(names, ",")
}
