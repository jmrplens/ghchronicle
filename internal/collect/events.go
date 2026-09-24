package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Events collects the account's activity feed.
//
// This is the most perishable surface GitHub has: the feed keeps the last 300
// events, and none older than thirty days, which GitHub documents and a quiet
// account shows: its feed is empty. Nothing else records that a repository
// was starred, forked, watched or pushed to at a given minute, so a sweep that
// misses the window loses the fact for good.
// Every event becomes one point stamped at its own creation time, which makes
// re-collection idempotent: the same event rewrites the same row.
type Events struct {
	// Login is whose feed to read. Empty means the authenticated user.
	Login string
	// Pages bounds the walk. Measured against this account, the feed serves
	// three pages of 100 and answers the fourth with 422 "pagination is
	// limited for this resource", so 300 events is the real ceiling.
	Pages int
	// After is the id of the newest event the previous sweep saw. The walk
	// stops at the page that carries it: everything older is already written.
	// Empty reads the whole feed, which is what a first sweep and a backfill
	// want.
	//
	// The page is emitted whole rather than cut at the id, and on purpose. An
	// event is never rewritten, so re-emitting a page costs nothing but the
	// sink's own comparison, and the feed has been seen to deliver an event
	// a little late, behind ones already served: cutting at the id would
	// drop it, emitting the page catches it.
	//
	// Measured on 2026-09-11: three pages of 100 every thirty minutes, never
	// a 304 because the feed moves with every event, for eleven events an
	// hour. One page a sweep is two requests of the three saved, ninety six
	// core points a day.
	After string
	// Newest is set by Collect to the id of the first event on the feed,
	// which is what the next sweep hands back as After.
	Newest string
}

// eventRow is one entry of the activity feed. The payload is left raw because
// its shape differs per event type.
type eventRow struct {
	// GitHub sends the id as a string of digits, and only equality is ever
	// asked of it.
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Public    bool                   `json:"public"`
	CreatedAt time.Time              `json:"created_at"`
	Actor     struct{ Login string } `json:"actor"`
	Repo      struct{ Name string }  `json:"repo"`
	Payload   json.RawMessage        `json:"payload"`
}

func (e *Events) Collect(ctx context.Context, c *ghapi.Client, _ time.Time) ([]sink.Point, error) {
	pages := e.Pages
	if pages <= 0 || pages > 3 {
		pages = 3
	}
	var points []sink.Point
	for page := 1; page <= pages; page++ {
		path := fmt.Sprintf("/users/%s/events?per_page=100&page=%d", e.Login, page)
		var batch []eventRow
		if _, _, err := c.GetJSON(ctx, path, &batch, ""); err != nil {
			// A 422 here is GitHub refusing to paginate further, which is the
			// end of the feed rather than a failure.
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return points, err
		}
		if len(batch) == 0 {
			break
		}
		if page == 1 {
			e.Newest = batch[0].ID
		}
		seen := false
		for i := range batch {
			points = append(points, eventPoint(&batch[i]))
			seen = seen || (e.After != "" && batch[i].ID == e.After)
		}
		if len(batch) < 100 || seen {
			break
		}
	}
	return points, nil
}

// eventPoint renders one feed entry, stamped at its own creation time so that
// re-collecting the feed rewrites the same row.
//
// The payload shape differs per event type. Only the parts that carry a number
// or a name worth charting are read; the rest is left alone rather than
// flattened into noise.
//
// The actor is not a tag. The feed is /users/{login}/events, the events the
// account itself performed, so the actor was the login on every row and cost
// a node on every Graphite path for nothing. Whether the event was public is
// a field: nobody groups by it, and a field is where a value that identifies
// nothing goes.
func eventPoint(ev *eventRow) sink.Point {
	tags := merge(fullNameTags(ev.Repo.Name), map[string]string{"type": ev.Type})
	fields := map[string]any{"events": 1, "public": ev.Public}
	// An event has no page of its own. The repository it happened in does, and
	// it is the one place a reader can go and see it.
	setNonEmpty(fields, "url", githubPage(ev.Repo.Name))
	var p struct {
		Action  string `json:"action"`
		Ref     string `json:"ref"`
		RefType string `json:"ref_type"`
		Size    int    `json:"size"`
		Commits []struct {
			Message string `json:"message"`
		} `json:"commits"`
	}
	// Only some event types carry an action or a ref type, and both are
	// written on every row all the same, with the one fallback the rest of
	// the collectors use. Written only where the payload had one, they were
	// NULL on every push, the only NULL tags in the whole database: a panel
	// grouping by action left out 218 of 380 rows, and the Graphite path
	// had three depths, which is why the events measurement had to be
	// addressed from the end of its path rather than from the tag table.
	if json.Unmarshal(ev.Payload, &p) == nil {
		tags["action"] = orNone(p.Action)
		tags["ref_type"] = orNone(p.RefType)
		if p.Size > 0 {
			fields["commits"] = p.Size
		} else if n := len(p.Commits); n > 0 {
			fields["commits"] = n
		}
	}
	return sink.Point{
		Measurement: "gh_event", Tags: tags, Fields: fields, Time: ev.CreatedAt,
	}
}

// Notifications collects the inbox.
//
// Like the event feed this is a window, not a history: GitHub keeps inbox
// notifications for three months unless they are saved, and `per_page` is
// silently capped at 50 whatever is asked for. Each notification is stamped at
// its own update time, so what gets charted is when the thread last moved.
type Notifications struct {
	// All includes read notifications, not only the unread ones.
	All bool
	// Pages bounds the walk. Each page is 50 whatever per_page asks for.
	Pages int
	// Walk widens the walk during a backfill.
	Walk Walk
	// Since asks only for the threads that moved after it, by their
	// updated_at, which is the timestamp of the point. Zero asks for the
	// whole inbox.
	//
	// Measured on 2026-09-11 against an inbox of a thousand unread threads:
	// twenty pages of fifty every thirty minutes, every one of them charged,
	// because a new thread shifts all twenty and the ETag never matches, for
	// six or seven rows that were new. `since` two hours back answered seven
	// rows in one page. The runner sets it to the newest updated_at it has
	// seen minus two cadences, and clears it once a day, which bounds to a
	// day whatever GitHub's filter left out of the windowed reads.
	Since time.Time
	// Newest is set by Collect to the latest updated_at it saw, which is
	// where the next sweep's window starts from.
	Newest time.Time
}

func (n *Notifications) Collect(ctx context.Context, c *ghapi.Client, _ time.Time) ([]sink.Point, error) {
	pages := n.Pages
	if pages <= 0 {
		pages = 20
	}
	if n.Walk.Pages != 0 {
		pages = n.Walk.limit(20)
	}
	since := ""
	if !n.Since.IsZero() {
		since = "&since=" + n.Since.UTC().Format(time.RFC3339)
	}
	var points []sink.Point
	for page := 1; page <= pages; page++ {
		path := fmt.Sprintf("/notifications?all=%t&per_page=50&page=%d%s", n.All, page, since)
		var batch []struct {
			Reason     string    `json:"reason"`
			Unread     bool      `json:"unread"`
			UpdatedAt  time.Time `json:"updated_at"`
			Repository struct {
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
			} `json:"repository"`
			Subject struct {
				Title string `json:"title"`
				Type  string `json:"type"`
				URL   string `json:"url"`
				// Measured over 500 notifications: present on 222 of them and
				// null on every CheckSuite, and it points at the exchange that
				// last moved the thread rather than at the thread.
				LatestCommentURL string `json:"latest_comment_url"`
			} `json:"subject"`
		}
		if _, _, err := c.GetJSON(ctx, path, &batch, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return points, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			it := &batch[i]
			if it.UpdatedAt.After(n.Newest) {
				n.Newest = it.UpdatedAt
			}
			fields := notificationFields(it.Subject.Title,
				notificationURL(it.Subject.URL, it.Subject.LatestCommentURL,
					it.Subject.Type, it.Repository.FullName))
			// Whether it is still unread is a field, not a tag: the row is
			// dated at the thread's last update and reading it does not
			// move that date, so the daily read with all=true, the one
			// that lists a thread read without a reply, has to rewrite the
			// same row rather than open a second one beside it. As the tag
			// `unread` it did the latter, and the inbox counted every
			// thread it had ever seen unread for ever. Under a new name so
			// a database that already holds the tag column keeps accepting
			// writes.
			fields["is_unread"] = it.Unread
			points = append(points, sink.Point{
				Measurement: "gh_notification",
				Tags: merge(fullNameTags(it.Repository.FullName), map[string]string{
					"reason": it.Reason, "private": boolTag(it.Repository.Private),
					"subject_type": it.Subject.Type,
				}),
				Fields: fields,
				Time:   it.UpdatedAt,
			})
		}
		if len(batch) < 50 || n.Walk.past(batch[len(batch)-1].UpdatedAt) {
			break
		}
	}
	return points, nil
}

// notificationFields keeps a link out of the point rather than writing it
// empty, so a row either carries a page or says nothing about one.
func notificationFields(title, url string) map[string]any {
	fields := map[string]any{"notifications": 1, "title": title}
	if url != "" {
		fields["url"] = url
	}
	return fields
}

// notificationURL picks the page a notification should open.
//
// Measured against this account's inbox on 2026-09-10, over 500 notifications
// read with `all=true`: there are four subject types, and 266 of them are
// CheckSuite, whose `subject.url` is null. Mapping more shapes cannot help
// those rows, because there is no address to map. What they do carry is the
// repository, and a CheckSuite notification is about a workflow run, so those
// land on that repository's Actions page.
//
// The other lever is `latest_comment_url`, present on 222 rows and pointing at
// the comment rather than at the item, which is the more useful destination.
// Thirteen of those are of the form /repos/o/n/issues/comments/<id>, an
// address with no page of its own: GitHub's own html_url for that comment is
// the item's page plus a #issuecomment-<id> anchor, verified by reading three
// of them back from the API. Building the anchor is why the comment address is
// not simply pushed through htmlURLOf, which would turn it into a dead
// github.com/o/n/issues/comments/<id>.
func notificationURL(subjectURL, commentURL, subjectType, fullName string) string {
	subject := htmlURLOf(subjectURL, "")
	if anchor, isComment := commentAnchor(commentURL); isComment {
		// A comment is an anchor on its item's page, so it is only the better
		// link when that page is known and the shape is one we can anchor.
		if anchor != "" && subject != "" {
			return subject + anchor
		}
	} else if comment := htmlURLOf(commentURL, ""); comment != "" {
		return comment
	}
	if subject != "" {
		return subject
	}
	return repositoryFallback(subjectType, fullName)
}

// repositoryFallback is where a notification with no address of its own sends
// the reader.
//
// Only CheckSuite earns the Actions page: it is the one type measured without
// a `subject.url`, and a workflow run is what it is about. Any other type
// arriving without an address is a shape nobody here has seen, and guessing
// Actions for it would be a confident lie the moment GitHub sends, say, a
// vulnerability alert, whose page is not Actions. The repository itself is
// the answer that cannot be wrong.
func repositoryFallback(subjectType, fullName string) string {
	if subjectType == "CheckSuite" {
		return githubPage(fullName, "actions")
	}
	return githubPage(fullName)
}

// commentAnchor reports whether an address names a comment, and gives the
// fragment GitHub itself uses for it.
//
// Only the issue-comment shape is recognized, because it is the only one this
// inbox produces. Any other comment address is reported as a comment with no
// anchor, which sends the caller to the item's own page rather than to an
// invented fragment.
func commentAnchor(api string) (string, bool) {
	if _, id, found := strings.Cut(api, "/issues/comments/"); found {
		return "#issuecomment-" + id, true
	}
	return "", strings.Contains(api, "/comments/")
}

// htmlURLOf turns the API address of a notification's subject into the page a
// person can open, and answers fallback when there is nothing to turn.
//
// A notification carries `subject.url`, which is the REST resource:
// https://api.github.com/repos/o/n/pulls/42. Nobody wants to read that. The
// mapping is mechanical, and a shape this does not recognize falls back rather
// than being guessed at.
func htmlURLOf(api, fallback string) string {
	rest, ok := strings.CutPrefix(api, "https://api.github.com/repos/")
	if !ok {
		return fallback
	}
	for from, to := range map[string]string{
		"/pulls/": "/pull/", "/issues/": "/issues/",
		"/releases/": "/releases/", "/commits/": "/commit/", "/discussions/": "/discussions/",
	} {
		if before, after, found := strings.Cut(rest, from); found {
			return "https://github.com/" + before + to + after
		}
	}
	return fallback
}
