package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Branches collects the live branch list of every repository, one row per
// branch carrying the age of its tip.
//
// Nothing else answers "which branches were abandoned". GitHub's own branch
// list sorts by name and forgets, so a branch whose last commit is four months
// old looks exactly like one pushed this morning. Measured on this account on
// 2026-09-10: eighteen repositories, forty-nine branches, one of them
// (Cloudflare-DNS-Updater fix/prefer-local-ipv6-fallback) four months stale.
//
// Deliberately without `associatedPullRequests`. Asking each ref which pull
// requests point at it is the obvious way to separate an abandoned branch from
// one waiting for review, and it is exactly what makes the query expensive:
// measured, fourteen repositories cost 1 point without that connection and 14
// with it. The join belongs in the panel instead. It needs a head branch on
// gh_pull_request, which that measurement does not carry today; adding it
// there is one field on a query that is already being made.
type Branches struct {
	Repos []Repo
	// Batch is how many repositories go into one query. Zero means fourteen,
	// measured at cost 1 and under two seconds for the whole sweep.
	Batch int
}

// branchFragment asks for the refs unordered on purpose.
//
// `refs(orderBy: {field: TAG_COMMIT_DATE, direction: DESC})` answers 200 and
// does not order heads: measured, it returned main (2026-09-06) ahead of
// fix/csp-discard-yandex-metrika (2026-09-07). Sorting, when a reader wants
// it, happens in the panel over days_since_commit.
//
// first: 100 is the cap, and because the refs come back unordered the hundred
// that arrive are an arbitrary hundred: a repository with more heads than that
// can lose its default branch along with the rest, leaving it with no
// is_default=true row at all. gh_repo_total carries a `branches` field read
// from the same refPrefix, so the truncation is visible rather than silent.
// The largest repository on this account has five heads, measured 2026-09-10.
const branchFragment = `
fragment branchInventory on Repository {
  defaultBranchRef { name }
  refs(refPrefix: "refs/heads/", first: 100) {
    totalCount
    nodes { name target { ... on Commit { oid committedDate } } }
  }
}`

type branchInventory struct {
	// A repository with no commits at all has no default branch, so this is a
	// pointer: reading .Name off a value would report every branch as the
	// default when the field is absent.
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
	Refs struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Name   string `json:"name"`
			Target struct {
				OID           string    `json:"oid"`
				CommittedDate time.Time `json:"committedDate"`
			} `json:"target"`
		} `json:"nodes"`
	} `json:"refs"`
}

func (b Branches) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	if len(b.Repos) == 0 {
		return nil, nil
	}
	size := b.Batch
	if size <= 0 {
		size = 14
	}
	day := now.UTC().Truncate(24 * time.Hour)
	var points []sink.Point
	var failed error

	for start := 0; start < len(b.Repos); start += size {
		end := min(start+size, len(b.Repos))
		batch := b.Repos[start:end]

		var q strings.Builder
		q.WriteString("query {")
		for i, repo := range batch {
			fmt.Fprintf(&q, " r%d: repository(owner: %q, name: %q) { ...branchInventory }", i, repo.Owner, repo.Name)
		}
		q.WriteString(" }")
		q.WriteString(branchFragment)

		var res map[string]json.RawMessage
		if graphQLErr := c.GraphQL(ctx, q.String(), nil, &res); graphQLErr != nil {
			if !isSkippableGraphQL(graphQLErr) {
				failed = errors.Join(failed, graphQLErr)
				continue
			}
			// One repository renamed away takes the whole batch with it: the
			// gateway answers 200 with the data of the other thirteen and a
			// NOT_FOUND in the errors array, and the client reports the error
			// rather than the partial data. Halving isolates the one that
			// went and keeps the rest, at one point per extra query.
			if len(batch) > 1 {
				half, err := Branches{Repos: batch, Batch: len(batch) / 2}.Collect(ctx, c, now)
				points = append(points, half...)
				failed = errors.Join(failed, err)
			}
			continue
		}
		for i, repo := range batch {
			raw, ok := res[fmt.Sprintf("r%d", i)]
			if !ok || string(raw) == "null" {
				continue
			}
			var inv branchInventory
			if err := json.Unmarshal(raw, &inv); err != nil {
				continue
			}
			points = append(points, inv.points(repo, now, day)...)
		}
	}
	if len(points) == 0 && failed != nil {
		return nil, failed
	}
	return points, nil
}

// points renders one row per branch, stamped at the start of the UTC day.
//
// This is an inventory and not an event: the tip of a branch moves, and the
// branch itself is deleted when its pull request merges. Dating a row at the
// commit it happens to point at today would scatter the same branch across
// the year and make "how many branches are stale right now" unanswerable.
func (inv *branchInventory) points(repo Repo, now, day time.Time) []sink.Point {
	base := repoTags(repo.Owner, repo.Name)
	var def string
	if inv.DefaultBranchRef != nil {
		def = inv.DefaultBranchRef.Name
	}
	var points []sink.Point
	for _, ref := range inv.Refs.Nodes {
		if ref.Name == "" {
			continue
		}
		// The name is what makes this a per-item measurement, the same reason
		// gh_pull_request tags its number: with the name in a field, every
		// branch of a repository shares one series and one timestamp, and the
		// four branches of jmrp.io would be stored as one row that the last
		// one read overwrites. The audit asked for a field to keep the
		// cardinality of dependabot/* branches down, which is a real cost,
		// but it is the cost of storing the thing at all.
		fields := map[string]any{"branches": 1}
		if ref.Target.OID != "" {
			fields["oid"] = ref.Target.OID
		}
		if !ref.Target.CommittedDate.IsZero() {
			fields["days_since_commit"] = int(now.Sub(ref.Target.CommittedDate).Hours() / 24)
		}
		points = append(points, sink.Point{
			Measurement: "gh_branch",
			Tags: merge(base, map[string]string{
				"branch":     ref.Name,
				"is_default": boolTag(def != "" && ref.Name == def),
			}),
			Fields: fields,
			Time:   day,
		})
	}
	return points
}
