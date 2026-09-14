package collect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// IssueEvents collects the transitions of issues and pull requests.
//
// gh_issue and gh_pull_request keep the state a thing ended in. gh_repo_activity
// keeps what happened to branches. Neither keeps the moment something changed:
// when a label went on, when an issue was closed, when it was reopened, when a
// review was asked for, when a branch was force pushed under a pull request.
// A reopened issue exists in no series at all today.
//
// The repository-level list, GET /repos/{r}/issues/events, is the reference
// for what an event is and how it is named, and it is what this collector
// read for a year. It embeds the whole issue in every event, description
// included: measured on 2026-09-11, a page of a hundred events is a megabyte
// and the collector keeps three per cent of it, and the first sweep of an
// installation downloaded fifty three megabytes of it. So an ordinary sweep
// now asks GraphQL for the timeline of the issues and pull requests updated
// in its window, which answers the same events without the issue around each
// of them, and a backfill walks the per-issue endpoint, GET
// /repos/{r}/issues/{n}/events, which is the same rows without the issue
// either. Both are rendered through the one function the repository list
// was, so the point is the same whichever road it came by.
//
// Measured on 2026-09-11 over a week of jmrplens/gitlab-mcp-server (2,217
// events) and jmrplens/jmrp.io (48): the timeline agrees with the list on
// every event of every type it can name, field for field, once the page is
// ten items per connection. At twenty and above the gateway silently returns
// empty timelines for some of the items, without an error; at fifty it
// returned none at all for a window of two hours. What the timeline cannot
// name is added_to_stack, an event of the stacked pull requests feature that
// has an enum value but no type in the schema, and an event on an item whose
// updatedAt did not move, which is a commit referencing an old issue: three
// of 2,217. The first is covered by reading the per-issue endpoint for a pull
// request that is in a stack; the second is the one thing the list saw that
// this does not, and it is noted in docs/rate-limits.md.
type IssueEvents struct {
	// Since is the start of an ordinary sweep's window: the items updated
	// after it are asked for the events dated after it. Zero means thirty
	// days, which is what a first sweep wants so that a fresh install does
	// not chart a history that starts an hour ago.
	Since time.Time
	// Walk, when it asks for pages, turns the sweep into the historical walk:
	// every issue and pull request updated since its bound, each read through
	// the per-issue endpoint, every page of it.
	Walk Walk
}

// issueEventUser is the shape GitHub uses for the two people a review
// request names.
type issueEventUser struct {
	Login string `json:"login"`
}

// issueEventActor is who did it. Type is "Bot" for an app.
type issueEventActor struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// issueEventIssue is the item an event happened to, as the repository list
// embeds it. The per-issue endpoint and the timeline do not carry it, and
// fill it in from the item they were asked about.
type issueEventIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	HTMLURL     string `json:"html_url"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

// issueEventRow is one event as REST lists it, and the shape every road
// through this collector ends in before it becomes a point.
type issueEventRow struct {
	Event     string           `json:"event"`
	CreatedAt time.Time        `json:"created_at"`
	Actor     *issueEventActor `json:"actor"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	// Who was asked and who asked. Present on review_requested and
	// review_request_removed, absent everywhere else.
	RequestedReviewer *issueEventUser `json:"requested_reviewer"`
	ReviewRequester   *issueEventUser `json:"review_requester"`
	Rename            *struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"rename"`
	// The commit a referenced, merged or closed-by-commit event points at.
	// Measured over 100 events of one repository: 20 carry it, 80 send an
	// explicit null, which decodes to the empty string.
	CommitID string           `json:"commit_id"`
	Issue    *issueEventIssue `json:"issue"`
}

// timelineWithEvents is the page size when every item carries its timeline.
//
// Ten and not more, measured: at twenty the gateway answered a week of
// jmrplens/gitlab-mcp-server with 17 events missing and no error, at
// twenty-five 109, at fifty 808, and a two hour window at fifty came back
// with no events at all where the list had two. At ten and at fifteen every
// event was there, three runs out of three. The cost is one point a page
// either way.
const timelineWithEvents = 10

// timelineListing is the page size when only the items are wanted, which is
// the backfill finding what to walk.
const timelineListing = 50

// timelineEvent is one kind of timeline item: whether it exists on pull
// requests alone, so its fragment is never spread on an issue's timeline,
// and what it carries beyond the actor and the date. The name /issues/events
// gives the same event is derived from the type name by restEventName.
type timelineEvent struct {
	pullOnly  bool
	selection string
}

// timelineEvents is every timeline item type that is an event, which is the
// union of the two timelines less the seven things in them that are not:
// comments, commits, reviews, review threads, the revision marker and the
// cross reference, none of which the repository list has ever emitted.
//
// Measured on 2026-09-11 against the list, on the 24 kinds a week of two
// repositories produced: the name is the type's without Event in snake case,
// with one exception, RenamedTitleEvent, which the list calls renamed. The
// kinds not seen that week follow the same rule, which is GitHub's own.
var timelineEvents = map[string]timelineEvent{
	"AddedToMergeQueueEvent":            {pullOnly: true},
	"AddedToProjectEvent":               {},
	"AddedToProjectV2Event":             {},
	"AssignedEvent":                     {},
	"AutoMergeDisabledEvent":            {pullOnly: true},
	"AutoMergeEnabledEvent":             {pullOnly: true},
	"AutoRebaseEnabledEvent":            {pullOnly: true},
	"AutoSquashEnabledEvent":            {pullOnly: true},
	"AutomaticBaseChangeFailedEvent":    {pullOnly: true},
	"AutomaticBaseChangeSucceededEvent": {pullOnly: true},
	"BaseRefChangedEvent":               {pullOnly: true},
	"BaseRefDeletedEvent":               {pullOnly: true},
	"BaseRefForcePushedEvent":           {pullOnly: true},
	"BlockedByAddedEvent":               {},
	"BlockedByRemovedEvent":             {},
	"BlockingAddedEvent":                {},
	"BlockingRemovedEvent":              {},
	// closer is a commit or a pull request; the list carries a commit_id
	// only when it was a commit, and so does this.
	"ClosedEvent":                       {selection: "closer { ... on Commit { oid } }"},
	"CommentDeletedEvent":               {},
	"ConnectedEvent":                    {},
	"ConvertToDraftEvent":               {pullOnly: true},
	"ConvertedFromDraftEvent":           {},
	"ConvertedNoteToIssueEvent":         {},
	"ConvertedToDiscussionEvent":        {},
	"DemilestonedEvent":                 {selection: "milestoneTitle"},
	"DeployedEvent":                     {pullOnly: true},
	"DeploymentEnvironmentChangedEvent": {pullOnly: true},
	"DisconnectedEvent":                 {},
	"HeadRefDeletedEvent":               {pullOnly: true},
	// The list's commit_id on a force push is the commit pushed, measured
	// on 256 of them.
	"HeadRefForcePushedEvent":         {pullOnly: true, selection: "afterCommit { oid }"},
	"HeadRefRestoredEvent":            {pullOnly: true},
	"IssueCommentPinnedEvent":         {},
	"IssueCommentUnpinnedEvent":       {},
	"IssueFieldAddedEvent":            {},
	"IssueFieldChangedEvent":          {},
	"IssueFieldRemovedEvent":          {},
	"IssueTypeAddedEvent":             {},
	"IssueTypeChangedEvent":           {},
	"IssueTypeRemovedEvent":           {},
	"LabeledEvent":                    {selection: "label { name }"},
	"LockedEvent":                     {},
	"MarkedAsDuplicateEvent":          {},
	"MentionedEvent":                  {},
	"MergedEvent":                     {pullOnly: true, selection: "commit { oid }"},
	"MilestonedEvent":                 {selection: "milestoneTitle"},
	"MovedColumnsInProjectEvent":      {},
	"ParentIssueAddedEvent":           {},
	"ParentIssueRemovedEvent":         {},
	"PinnedEvent":                     {},
	"ProjectV2ItemStatusChangedEvent": {},
	"ReadyForReviewEvent":             {pullOnly: true},
	"ReferencedEvent":                 {selection: "commit { oid }"},
	"RemovedFromMergeQueueEvent":      {pullOnly: true},
	"RemovedFromProjectEvent":         {},
	"RemovedFromProjectV2Event":       {},
	"RenamedTitleEvent":               {selection: "previousTitle currentTitle"},
	"ReopenedEvent":                   {},
	"ReviewDismissedEvent":            {pullOnly: true},
	// A team asked for review is requested_team on the list, which this
	// collector has never read, so only a person or an app is named.
	"ReviewRequestRemovedEvent": {pullOnly: true, selection: reviewerSelection},
	"ReviewRequestedEvent":      {pullOnly: true, selection: reviewerSelection},
	"SubIssueAddedEvent":        {},
	"SubIssueRemovedEvent":      {},
	"SubscribedEvent":           {},
	"TransferredEvent":          {},
	"UnassignedEvent":           {},
	"UnlabeledEvent":            {selection: "label { name }"},
	"UnlockedEvent":             {},
	"UnmarkedAsDuplicateEvent":  {},
	"UnpinnedEvent":             {},
	"UnsubscribedEvent":         {},
	"UserBlockedEvent":          {},
}

const reviewerSelection = "requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } }"

// restEventName is what /issues/events calls a timeline item type: the type
// name without its Event suffix, in snake case, a digit staying with the
// letter before it so AddedToProjectV2Event is added_to_project_v2.
func restEventName(typename string) string {
	if typename == "RenamedTitleEvent" {
		return "renamed"
	}
	return snakeCase(strings.TrimSuffix(typename, "Event"))
}

// timelineItemType is the enum value that names a type in itemTypes, which
// is the whole type name in upper snake case: RenamedTitleEvent is
// RENAMED_TITLE_EVENT there and renamed on the list.
func timelineItemType(typename string) string {
	return strings.ToUpper(snakeCase(typename))
}

// snakeCase splits a CamelCase name at each capital.
func snakeCase(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// issueTimelineQuery is built once from timelineEvents: one inline fragment
// per event type, and the itemTypes list that keeps the seven non-events
// out of the answer at the source rather than in the decoder.
var issueTimelineQuery = buildIssueTimelineQuery()

func buildIssueTimelineQuery() string {
	names := make([]string, 0, len(timelineEvents))
	for name := range timelineEvents {
		names = append(names, name)
	}
	sort.Strings(names)
	var issueTypes, pullTypes, issueFragments, pullFragments []string
	for _, name := range names {
		ev := timelineEvents[name]
		fragment := "... on " + name + " { createdAt actor { login __typename } " + ev.selection + " }"
		pullTypes = append(pullTypes, timelineItemType(name))
		pullFragments = append(pullFragments, fragment)
		if !ev.pullOnly {
			issueTypes = append(issueTypes, timelineItemType(name))
			issueFragments = append(issueFragments, fragment)
		}
	}
	return fmt.Sprintf(`
query($owner: String!, $name: String!, $first: Int!, $since: DateTime!, $issueAfter: String, $prAfter: String, $withIssues: Boolean!, $withPRs: Boolean!, $withTimeline: Boolean!) {
  repository(owner: $owner, name: $name) {
    issues(first: $first, after: $issueAfter, orderBy: {field: UPDATED_AT, direction: DESC}) @include(if: $withIssues) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title url updatedAt
        timelineItems(since: $since, first: 100, itemTypes: [%s]) @include(if: $withTimeline) {
          pageInfo { hasNextPage }
          nodes { __typename %s }
        }
      }
    }
    pullRequests(first: $first, after: $prAfter, orderBy: {field: UPDATED_AT, direction: DESC}) @include(if: $withPRs) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title url updatedAt
        stackEntry { position }
        timelineItems(since: $since, first: 100, itemTypes: [%s]) @include(if: $withTimeline) {
          pageInfo { hasNextPage }
          nodes { __typename %s }
        }
      }
    }
  }
}`, strings.Join(issueTypes, ", "), strings.Join(issueFragments, " "),
		strings.Join(pullTypes, ", "), strings.Join(pullFragments, " "))
}

// timelineActor is who did it, as GraphQL names it: __typename is Bot for an
// app, whose login the REST list suffixes with [bot] and GraphQL does not.
type timelineActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// restLogin is the login as the REST list writes it, which for an app carries
// the [bot] suffix GraphQL leaves off: dependabot is dependabot[bot] there,
// measured on every bot of the week compared.
func (a *timelineActor) restLogin() string {
	if a == nil {
		return ""
	}
	return appLogin(a.Login, a.Typename == "Bot")
}

// timelineItem is one node of a timeline, every field any event type carries
// laid flat, since GraphQL answers only the ones the type has.
type timelineItem struct {
	Typename  string         `json:"__typename"`
	CreatedAt time.Time      `json:"createdAt"`
	Actor     *timelineActor `json:"actor"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
	MilestoneTitle string `json:"milestoneTitle"`
	PreviousTitle  string `json:"previousTitle"`
	CurrentTitle   string `json:"currentTitle"`
	Commit         *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	AfterCommit *struct {
		OID string `json:"oid"`
	} `json:"afterCommit"`
	Closer *struct {
		OID string `json:"oid"`
	} `json:"closer"`
	RequestedReviewer *timelineActor `json:"requestedReviewer"`
}

// timelineNode is one issue or pull request with its timeline.
type timelineNode struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
	// StackEntry is set when a pull request is in a stack, which is the one
	// case the timeline cannot answer for: added_to_stack has no type.
	StackEntry *struct {
		Position int `json:"position"`
	} `json:"stackEntry"`
	Timeline struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []timelineItem `json:"nodes"`
	} `json:"timelineItems"`
}

// item is the node as the REST rows embed it, for the two roads that carry
// no issue of their own.
func (n *timelineNode) item(kind string) *issueEventIssue {
	it := &issueEventIssue{Number: n.Number, Title: n.Title, HTMLURL: n.URL}
	if kind == "pull_request" {
		it.PullRequest = &struct {
			URL string `json:"url"`
		}{URL: n.URL}
	}
	return it
}

// row renders a timeline item as the list would have listed it, or nil for a
// type the list never emits.
func (it *timelineItem) row(issue *issueEventIssue) *issueEventRow {
	if _, known := timelineEvents[it.Typename]; !known {
		return nil
	}
	r := &issueEventRow{Event: restEventName(it.Typename), CreatedAt: it.CreatedAt, Issue: issue}
	if it.Actor != nil {
		r.Actor = &issueEventActor{Login: it.Actor.restLogin()}
		if it.Actor.Typename == "Bot" {
			r.Actor.Type = "Bot"
		}
	}
	if it.Label != nil {
		r.Label = &struct {
			Name string `json:"name"`
		}{Name: it.Label.Name}
	}
	if it.MilestoneTitle != "" {
		r.Milestone = &struct {
			Title string `json:"title"`
		}{Title: it.MilestoneTitle}
	}
	if it.Typename == "RenamedTitleEvent" {
		r.Rename = &struct {
			From string `json:"from"`
			To   string `json:"to"`
		}{From: it.PreviousTitle, To: it.CurrentTitle}
	}
	if it.Typename == "ReviewRequestedEvent" || it.Typename == "ReviewRequestRemovedEvent" {
		// The list names the requester twice, as the actor and as
		// review_requester: measured equal on every such event.
		if login := it.RequestedReviewer.restLogin(); login != "" {
			r.RequestedReviewer = &issueEventUser{Login: login}
		}
		if r.Actor != nil {
			r.ReviewRequester = &issueEventUser{Login: r.Actor.Login}
		}
	}
	switch {
	case it.Commit != nil:
		r.CommitID = it.Commit.OID
	case it.AfterCommit != nil:
		r.CommitID = it.AfterCommit.OID
	case it.Closer != nil:
		r.CommitID = it.Closer.OID
	}
	return r
}

func (ie IssueEvents) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	if ie.Walk.Pages != 0 {
		return ie.history(ctx, c, repo, base)
	}
	since := ie.Since
	if since.IsZero() {
		since = now.AddDate(0, 0, -30)
	}
	return ie.recent(ctx, c, repo, base, since)
}

// recent is the ordinary sweep: the timeline of every item updated in the
// window, and the per-issue list for the two items the timeline cannot
// answer whole, a pull request in a stack and an item with more than a
// hundred events in the window.
func (ie IssueEvents) recent(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, since time.Time) ([]sink.Point, error) {
	var points []sink.Point
	err := ie.listItems(ctx, c, repo, since, true, func(n *timelineNode, kind string) error {
		if n.Timeline.PageInfo.HasNextPage || n.StackEntry != nil {
			pts, err := itemEvents(ctx, c, repo, base, n.item(kind), Walk{Since: since})
			points = append(points, pts...)
			return err
		}
		issue := n.item(kind)
		for i := range n.Timeline.Nodes {
			if r := n.Timeline.Nodes[i].row(issue); r != nil {
				points = append(points, issueEventPoint(base, r))
			}
		}
		return nil
	})
	return points, err
}

// history is the backfill: every item updated since the bound, each walked
// through the per-issue list, every page of it.
func (ie IssueEvents) history(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string) ([]sink.Point, error) {
	var points []sink.Point
	err := ie.listItems(ctx, c, repo, ie.Walk.Since, false, func(n *timelineNode, kind string) error {
		pts, err := itemEvents(ctx, c, repo, base, n.item(kind), ie.Walk)
		points = append(points, pts...)
		return err
	})
	return points, err
}

// timelineConnection is one page of issues or of pull requests.
type timelineConnection struct {
	PageInfo pageInfo       `json:"pageInfo"`
	Nodes    []timelineNode `json:"nodes"`
}

// more reports whether the next page is worth asking for: there is one, and
// the last item of this one was not updated before the bound.
func (tc *timelineConnection) more(bound Walk) bool {
	return tc.PageInfo.HasNextPage && len(tc.Nodes) > 0 && !bound.past(tc.Nodes[len(tc.Nodes)-1].UpdatedAt)
}

// listItems pages the issues and the pull requests newest-updated first, one
// cursor per connection in the same query while both have pages left, and
// hands each node to visit. A connection stops when its last node was
// updated before since, or when GitHub says there is no next page.
func (ie IssueEvents) listItems(ctx context.Context, c *ghapi.Client, repo Repo, since time.Time, withTimeline bool,
	visit func(n *timelineNode, kind string) error,
) error {
	first := timelineListing
	if withTimeline {
		first = timelineWithEvents
	}
	var res struct {
		Repository struct {
			Issues       timelineConnection `json:"issues"`
			PullRequests timelineConnection `json:"pullRequests"`
		} `json:"repository"`
	}
	bound := Walk{Since: since}
	// The timeline's since is required, and a walk with no bound has to
	// send something: the epoch is before any event GitHub has.
	stamp := since
	if stamp.IsZero() {
		stamp = time.Unix(0, 0)
	}
	vars := map[string]any{
		"owner": repo.Owner, "name": repo.Name, "first": first,
		"since": stamp.UTC().Format(time.RFC3339), "withTimeline": withTimeline,
		"withIssues": true, "withPRs": true,
	}
	most := ie.Walk.limit(100)
	for page := 1; page <= most && (vars["withIssues"] == true || vars["withPRs"] == true); page++ {
		if err := c.GraphQL(ctx, issueTimelineQuery, vars, &res); err != nil {
			if isSkippableGraphQL(err) {
				return nil
			}
			return err
		}
		issues, pulls := &res.Repository.Issues, &res.Repository.PullRequests
		if vars["withIssues"] == true {
			if err := visitEach(issues.Nodes, "issue", bound, visit); err != nil {
				return err
			}
			vars["withIssues"] = issues.cursor(vars, "issueAfter", bound)
		}
		if vars["withPRs"] == true {
			if err := visitEach(pulls.Nodes, "pull_request", bound, visit); err != nil {
				return err
			}
			vars["withPRs"] = pulls.cursor(vars, "prAfter", bound)
		}
		issues.Nodes, pulls.Nodes = nil, nil
	}
	return nil
}

// cursor sets the connection's cursor variable for the next page and reports
// whether there is one to ask for. The variable is left out rather than sent
// empty when the walk is over, since an empty cursor is not a cursor.
func (tc *timelineConnection) cursor(vars map[string]any, name string, bound Walk) bool {
	if !tc.more(bound) || tc.PageInfo.EndCursor == "" {
		delete(vars, name)
		return false
	}
	vars[name] = tc.PageInfo.EndCursor
	return true
}

// visitEach hands the items of one page to visit, skipping those updated
// before the bound: an event moves its item's updatedAt, so an item last
// updated before the window has nothing in it, and asking would cost a
// per-issue list for every old stacked pull request on the page.
func visitEach(nodes []timelineNode, kind string, bound Walk, visit func(n *timelineNode, kind string) error) error {
	for i := range nodes {
		if bound.past(nodes[i].UpdatedAt) {
			continue
		}
		if err := visit(&nodes[i], kind); err != nil {
			return err
		}
	}
	return nil
}

// itemEvents reads one item's events from the per-issue list and keeps the
// ones inside the walk. The list is oldest first, so unlike every other list
// this collector reads it cannot stop at the first row past the bound: every
// page is read and the rows are filtered. An item with more than a hundred
// events is rare enough that this costs a second page a handful of times.
//
// A refusal is the item being gone, moved or hidden, and is not a failure of
// the repository.
func itemEvents(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, issue *issueEventIssue, w Walk) ([]sink.Point, error) {
	var points []sink.Point
	err := pages(ctx, c, Unbounded, 1, func(page int) string {
		return fmt.Sprintf("/repos/%s/issues/%d/events?per_page=100&page=%d", repo.FullName, issue.Number, page)
	}, func(rows []issueEventRow) bool {
		for i := range rows {
			if w.past(rows[i].CreatedAt) {
				continue
			}
			rows[i].Issue = issue
			points = append(points, issueEventPoint(base, &rows[i]))
		}
		return true
	})
	return points, err
}

// issueEventPoint turns one event into its point. Split out of the walk
// because the tags each need their own fallback and the fields are each
// optional, which is a lot of branching to carry inside a page loop.
func issueEventPoint(base map[string]string, e *issueEventRow) sink.Point {
	actor, bot := "", false
	if e.Actor != nil {
		bot = e.Actor.Type == "Bot"
		actor = appLogin(e.Actor.Login, bot)
	}
	// GitHub files a mention and a subscription with the person it happened
	// to as the actor, and says nothing about who wrote the comment. Kept as
	// the actor, the account named in "@coderabbitai" landed in the same
	// column as the app that reviews, under a third spelling, and the `@id`,
	// `@graph` and `@context` of a JSON-LD block in a pull request body
	// became the actors id, graph and context. They are the subject of the
	// event, so they go where the subject of a labeling or a review request
	// goes: in a tag of their own, with the actor left unnamed.
	mentioned := ""
	if e.Event == "mentioned" || e.Event == "subscribed" {
		mentioned, actor, bot = actor, "", false
	}
	kind := "issue"
	number, title, url := 0, "", ""
	if e.Issue != nil {
		number, title, url = e.Issue.Number, e.Issue.Title, e.Issue.HTMLURL
		if e.Issue.PullRequest != nil {
			kind = "pull_request"
		}
	}
	label, milestone := "", ""
	if e.Label != nil {
		label = e.Label.Name
	}
	if e.Milestone != nil {
		milestone = e.Milestone.Title
	}
	tags := merge(base, map[string]string{
		// GitHub declares the actor nullable and this collector has guarded
		// against it since it was written, but the guard left the tag empty,
		// which is the one shape it must not have. The fallback costs nothing
		// on the events that do name an actor.
		"event": e.Event, "actor": orNone(actor), "kind": kind,
		// A bot labeling a dependency bump and a person closing an issue
		// are the same event type and different work.
		"bot": boolTag(bot),
		// Each of the next four is the subject of one event type and nothing
		// at all on the rest. They are written even so: a tag present on some
		// points of a measurement and absent on others gives that measurement
		// two Graphite path depths, and the panels index their nodes from one
		// fixed table, so the per-day panel was silently missing every
		// labeled and milestoned event until this was fixed once already.
		"label":              orNone(label),
		"milestone":          orNone(milestone),
		"requested_reviewer": orNone(loginOf(e.RequestedReviewer)),
		"review_requester":   orNone(loginOf(e.ReviewRequester)),
		"mentioned":          orNone(mentioned),
	})
	fields := map[string]any{"events": 1, "number": number}
	if e.CommitID != "" {
		// The only link an event carries back to the code. Measured over 100
		// events, 20 have one and 80 send an explicit null, so it is written
		// where there is one rather than empty on every point.
		fields["commit_id"] = e.CommitID
	}
	if e.Rename != nil {
		// The event said something was renamed and not from what to what,
		// which is the half a reader wants.
		fields["rename_from"] = e.Rename.From
		fields["rename_to"] = e.Rename.To
	}
	if title != "" {
		fields["title"] = title
	}
	if url != "" {
		// The event has no page; the item it happened to does.
		fields["url"] = url
	}
	return sink.Point{
		Measurement: "gh_issue_event",
		Tags:        tags,
		Fields:      fields,
		Time:        e.CreatedAt,
	}
}

func loginOf(u *issueEventUser) string {
	if u == nil {
		return ""
	}
	return u.Login
}
