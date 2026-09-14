package collect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// RepoActivityLog collects a repository's own activity log.
//
// It is the only place a force push is recorded. The public event feed does
// not distinguish one, and nothing else says a branch was created or deleted
// or that a merge was a squash rather than a rebase.
//
// It is also as perishable as traffic: a hundred entries covered twenty-six
// hours on the busiest repository here, and unlike the account feed it pages
// by cursor rather than refusing past three hundred.
type RepoActivityLog struct {
	Walk Walk
}

// activityRow is one entry of the activity log as GitHub sends it.
type activityRow struct {
	ID           int64     `json:"id"`
	Ref          string    `json:"ref"`
	Timestamp    time.Time `json:"timestamp"`
	ActivityType string    `json:"activity_type"`
	Actor        *struct {
		Login string `json:"login"`
	} `json:"actor"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func (r RepoActivityLog) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	var rows []activityRow
	after := ""
	most := r.Walk.limit(2)
	for page := 1; page <= most; page++ {
		path := fmt.Sprintf("/repos/%s/activity?per_page=100", repo.FullName)
		if after != "" {
			path += "&after=" + after
		}
		var batch []activityRow
		link, _, err := c.GetJSON(ctx, path, &batch, "")
		if err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return activityPoints(rows, base), err
		}
		if len(batch) == 0 {
			break
		}
		rows = append(rows, batch...)
		after = afterCursor(link)
		if after == "" || len(batch) < 100 || r.Walk.past(batch[len(batch)-1].Timestamp) {
			break
		}
	}
	return activityPoints(rows, base), nil
}

// activityPoints renders the log as one row per activity type, actor and
// second, in the order GitHub listed them.
//
// The branch is a field, not a tag: every pull request and every Dependabot
// bump mints a name that never comes back, 427 of them in 2,554 rows measured
// on this account, which is the series-per-item the same value was demoted
// from gh_workflow_run for. Under a new name so a database that already holds
// the tag column keeps accepting writes.
//
// Without the branch in the key, the activities of one push are one row:
// every store keys a point by its tags and its time, and a push of a stack
// moves every branch in it in the same second. Measured on this account, 84
// entries in 32 such seconds, eight force pushes at 07:23:54 among them, and
// as separate points seven of the eight were overwritten by the last one
// written. So the entries that share a key are folded into one point that
// counts them, `events` being the count, `ref_name` naming every branch in
// the order GitHub listed them, and `id` the newest entry's. A sum of events
// is then right in every store, and the Force pushes table reads one row for
// the one push.
func activityPoints(rows []activityRow, base map[string]string) []sink.Point {
	type key struct {
		activity, actor string
		at              time.Time
	}
	type group struct {
		first activityRow
		actor string
		refs  []string
	}
	index := map[key]int{}
	var groups []*group
	for _, a := range rows {
		actor := "(ghost)"
		if a.Actor != nil {
			actor = a.Actor.Login
		}
		k := key{a.ActivityType, actor, a.Timestamp}
		i, seen := index[k]
		if !seen {
			i = len(groups)
			index[k] = i
			groups = append(groups, &group{first: a, actor: actor})
		}
		groups[i].refs = append(groups[i].refs, shortRef(a.Ref))
	}
	points := make([]sink.Point, 0, len(groups))
	for _, g := range groups {
		points = append(points, sink.Point{
			Measurement: "gh_repo_activity",
			Tags:        merge(base, map[string]string{"activity": g.first.ActivityType, "actor": g.actor}),
			Fields: map[string]any{
				"events": len(g.refs), "id": g.first.ID,
				"ref_name": strings.Join(g.refs, ","),
			},
			Time: g.first.Timestamp,
		})
	}
	return points
}

// shortRef trims refs/heads/ so the tag reads as a branch name.
func shortRef(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if len(ref) > len(p) && ref[:len(p)] == p {
			return ref[len(p):]
		}
	}
	return ref
}

// afterCursor pulls the `after` value out of a Link header. This endpoint pages
// by cursor rather than by page number, so the usual page counter does not
// advance it.
func afterCursor(link string) string {
	for _, part := range splitLinks(link) {
		if part.rel != "next" {
			continue
		}
		if i := indexOf(part.url, "after="); i >= 0 {
			v := part.url[i+len("after="):]
			if j := indexOf(v, "&"); j >= 0 {
				v = v[:j]
			}
			return v
		}
	}
	return ""
}

type linkPart struct{ url, rel string }

func splitLinks(link string) []linkPart {
	var out []linkPart
	for _, chunk := range splitAny(link, ",") {
		var p linkPart
		for _, bit := range splitAny(chunk, ";") {
			bit = trimSpace(bit)
			switch {
			case len(bit) > 2 && bit[0] == '<' && bit[len(bit)-1] == '>':
				p.url = bit[1 : len(bit)-1]
			case len(bit) > 5 && bit[:4] == "rel=":
				p.rel = trimQuotes(bit[4:])
			}
		}
		if p.url != "" {
			out = append(out, p)
		}
	}
	return out
}
