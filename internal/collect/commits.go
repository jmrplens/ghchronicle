package collect

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Commits collects the commit history with its size and its signature.
//
// This is what replaces `stats/code_frequency`, which answers 202 with an
// empty body forever on a personal account. GraphQL gives lines added and
// removed per commit, which is the same number aggregated, and gives it
// attributed to an author and dated to the commit rather than to a week.
//
// The signature comes with it. Nothing else in the project records whether the
// history is signed, and it is one query either way.
//
// The REST list is not an alternative: it carries `verification` but not
// `stats`, so the sizes would cost one request per commit.
type Commits struct {
	// Since bounds the walk. A zero value means the last 100 commits.
	Since time.Time
	// First is how many commits to ask for, capped by GitHub at 100.
	First int
	// Walk pages further back through the history, a hundred commits at a
	// time. One page is the increment a sweep needs; a backfill walks it all,
	// because without that the lines-changed series begins on install day.
	Walk Walk
}

const commitsQuery = `
query($owner: String!, $name: String!, $first: Int!, $since: GitTimestamp, $after: String) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef {
      name
      target {
        ... on Commit {
          history(first: $first, since: $since, after: $after) {
            totalCount
            pageInfo { hasNextPage endCursor }
            nodes {
              oid url committedDate additions deletions changedFilesIfAvailable
              messageHeadline
              author { name user { login } }
              signature { isValid state }
              associatedPullRequests(first: 1) { nodes { number } }
              statusCheckRollup {
                state
                contexts(first: 100) {
                  totalCount
                  nodes {
                    __typename
                    ... on CheckRun { name conclusion completedAt detailsUrl checkSuite { app { slug } } }
                    ... on StatusContext { context state createdAt targetUrl }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// checkRollup is the gate state of one commit and the contexts behind it.
type checkRollup struct {
	State    string `json:"state"`
	Contexts struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Typename    string     `json:"__typename"`
			Name        string     `json:"name"`
			Conclusion  string     `json:"conclusion"`
			CompletedAt *time.Time `json:"completedAt"`
			DetailsURL  string     `json:"detailsUrl"`
			TargetURL   string     `json:"targetUrl"`
			CheckSuite  checkSuite `json:"checkSuite"`
			Context     string     `json:"context"`
			State       string     `json:"state"`
			CreatedAt   *time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"contexts"`
}

// checkSuite is the suite a check run belongs to, read only for the app that
// ran it: nothing else in a context names the provider behind the check.
type checkSuite struct {
	App *struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

type commitRef struct {
	Name   string `json:"name"`
	Target struct {
		History commitHistory `json:"history"`
	} `json:"target"`
}

// commitHistory is one page of a branch's history, newest first.
type commitHistory struct {
	TotalCount int      `json:"totalCount"`
	PageInfo   pageInfo `json:"pageInfo"`
	Nodes      []struct {
		OID             string       `json:"oid"`
		URL             string       `json:"url"`
		CommittedDate   time.Time    `json:"committedDate"`
		Additions       int          `json:"additions"`
		Deletions       int          `json:"deletions"`
		ChangedFiles    int          `json:"changedFilesIfAvailable"`
		MessageHeadline string       `json:"messageHeadline"`
		Author          commitAuthor `json:"author"`
		Signature       *struct {
			IsValid bool   `json:"isValid"`
			State   string `json:"state"`
		} `json:"signature"`
		AssociatedPullRequests struct {
			Nodes []struct {
				Number int `json:"number"`
			} `json:"nodes"`
		} `json:"associatedPullRequests"`
		StatusCheckRollup *checkRollup `json:"statusCheckRollup"`
	} `json:"nodes"`
}

// commitAuthor is who wrote a commit: the name git recorded, and the GitHub
// account GraphQL matched it to, which is absent for an email it knows nobody
// by.
type commitAuthor struct {
	Name string `json:"name"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (cm Commits) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	first := cm.First
	if first <= 0 || first > 100 {
		first = 100
	}
	// Each commit carries up to a hundred check contexts, which is what makes
	// the gate state free of extra requests and the page expensive. Measured:
	// fifty commits of a busy repository is 5,050 nodes and 3.8 seconds, and a
	// hundred would sit on the gateway's ten second ceiling.
	if first > 50 {
		first = 50
	}
	var res struct {
		Repository struct {
			DefaultBranchRef commitRef `json:"defaultBranchRef"`
		} `json:"repository"`
	}

	w := cm.Walk
	if w.Since.IsZero() {
		w.Since = cm.Since
	}
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point
	after := ""
	most := w.limit(1)
	for range most {
		vars := map[string]any{"owner": repo.Owner, "name": repo.Name, "first": first}
		if !cm.Since.IsZero() {
			vars["since"] = cm.Since.UTC().Format(time.RFC3339)
		}
		if after != "" {
			vars["after"] = after
		}
		if err := c.GraphQL(ctx, commitsQuery, vars, &res); err != nil {
			// An empty repository has no default branch, which is not a
			// failure. Neither is a page that times out on a large history.
			// A spent budget or a canceled sweep is, and returning the
			// pages walked so far with no error would hide it.
			if isSkippableGraphQL(err) {
				return points, nil
			}
			return points, err
		}
		pts, next := commitPoints(res.Repository.DefaultBranchRef, base)
		points = append(points, pts...)
		// The since argument already bounds the server side; this stops the
		// walk once a page has clearly gone past it, which saves the request.
		if next == "" || (len(pts) > 0 && w.past(pts[len(pts)-1].Time)) {
			break
		}
		after = next
	}
	return points, nil
}

// commitPoints renders one page and returns the cursor of the next, or empty
// when the history ends.
func commitPoints(ref commitRef, base map[string]string) (points []sink.Point, next string) {
	branch := ref.Name
	for i := range ref.Target.History.Nodes {
		cmt := &ref.Target.History.Nodes[i]
		author := cmt.Author.Name
		if cmt.Author.User != nil && cmt.Author.User.Login != "" {
			author = cmt.Author.User.Login
		}
		// An unsigned commit reports no signature at all, which is a different
		// fact from a signature that failed to verify.
		sigState := "unsigned"
		valid := false
		if cmt.Signature != nil {
			sigState, valid = cmt.Signature.State, cmt.Signature.IsValid
		}
		fields := map[string]any{
			"additions": cmt.Additions, "deletions": cmt.Deletions,
			"churn": cmt.Additions + cmt.Deletions, "changed_files": cmt.ChangedFiles,
			"commits": 1, "signed": valid, "oid": cmt.OID,
			"headline": cmt.MessageHeadline, "url": cmt.URL,
		}
		if n := cmt.AssociatedPullRequests.Nodes; len(n) > 0 {
			fields["pull_request"] = n[0].Number
		}
		// Whether the commit itself came out green. A workflow run that failed
		// says one job failed; this says the gate did, which is the number a
		// reader means by "how often is main red". A field and not a tag: the
		// row is dated when the commit was made and the gate finishes later,
		// so as the tag `checks` a commit seen PENDING by one sweep and
		// FAILURE by the next was two rows at the same instant for ever.
		// Under a new name so a database that already holds the tag column
		// keeps accepting writes.
		gate := "none"
		if r := cmt.StatusCheckRollup; r != nil {
			gate = r.State
			fields["checks_total"] = r.Contexts.TotalCount
			fields["checks_failed"] = boolInt(r.State == "FAILURE")
		}
		fields["gate"] = gate
		sha := cmt.OID[:min(12, len(cmt.OID))]
		points = append(points, sink.Point{
			Measurement: "gh_commit",
			Tags: merge(base, map[string]string{
				// The hash is the identity of the fact. Two commits in the same
				// second by the same author would otherwise share a row.
				"sha":    sha,
				"author": author, "branch": branch, "signature": sigState,
			}),
			Fields: fields,
			Time:   cmt.CommittedDate,
		})
		points = append(points, commitCheckPoints(cmt.StatusCheckRollup, base, sha, cmt.CommittedDate)...)
	}
	if ref.Target.History.PageInfo.HasNextPage {
		return points, ref.Target.History.PageInfo.EndCursor
	}
	return points, ""
}

// checkPoints records the checks that are not GitHub Actions.
//
// Everything Actions runs is already in gh_workflow_run and gh_workflow_job in
// far more detail. What is invisible today is everything else that gates a
// commit: on the account this was written against, SonarQubeCloud accounts for
// thirty seven of the sixteen hundred contexts on fifty commits, and Dependabot
// for one. Those are the ones worth a row, and they cost nothing extra: they
// arrive in the same query as the commit.
func commitCheckPoints(rollup *checkRollup, base map[string]string, sha string, committed time.Time) []sink.Point {
	if rollup == nil {
		return nil
	}
	var points []sink.Point
	for i := range rollup.Contexts.Nodes {
		ctx := &rollup.Contexts.Nodes[i]
		app, name, conclusion, stamp := "", ctx.Name, ctx.Conclusion, ctx.CompletedAt
		link := ctx.DetailsURL
		if ctx.Typename == "StatusContext" {
			link = ctx.TargetURL
			// A commit status rather than a check run: an older mechanism, and
			// the one integrations that predate the Checks API still use.
			app, name, conclusion, stamp = "status", ctx.Context, ctx.State, ctx.CreatedAt
		} else if ctx.CheckSuite.App != nil {
			app = ctx.CheckSuite.App.Slug
		}
		if app == "github-actions" || app == "" {
			continue // already collected, in more detail, by the actions family
		}
		when := committed
		if stamp != nil {
			when = *stamp
		}
		points = append(points, sink.Point{
			Measurement: "gh_commit_check",
			Tags: merge(base, map[string]string{
				"sha": sha, "app": app, "check": name, "conclusion": conclusion,
			}),
			Fields: map[string]any{
				"checks": 1, "failed": boolInt(conclusion == "FAILURE" || conclusion == "ERROR"),
				"url": link,
			},
			Time: when,
		})
	}
	return points
}
