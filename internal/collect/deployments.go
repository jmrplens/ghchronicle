package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// defaultAliasBatch is how many repositories go into one alias query.
//
// Five, not ten. The gateway gives up on a query it cannot finish in about ten
// seconds, and these two collectors ask for connections rather than scalars:
// measured on 2026-09-10, ten repositories of deployments took 5.3 s and a
// comparable ten-alias query took 8.4 s. Halving on the way down costs an
// extra round trip; a batch of five leaves the headroom to not need it.
const defaultAliasBatch = 5

// aliasBatch asks GitHub about several repositories in one GraphQL query.
//
// One alias per repository against a shared fragment is what makes a whole
// sweep cost one point instead of one per repository. Every collector in this
// package that reads repositories in batches is that same shape (Deployments,
// PolicyFiles, Totals and RepoDetail), so the skeleton lives here once: build
// the query, ask for less when the gateway refuses the group, decode by alias.
// aliasRetry says which of the two refusals happened and what to do about it.
//
// emit is called per repository that answered, so a caller can accumulate
// points and cursors at once; a repository whose alias came back null is
// skipped without failing the rest.
func aliasBatch[T any](ctx context.Context, c *ghapi.Client, repos []Repo, size int, build func(batch []Repo) string, emit func(repo Repo, node T)) error {
	if size <= 0 {
		size = defaultAliasBatch
	}
	var failed error
	for start := 0; start < len(repos); start += size {
		end := min(start+size, len(repos))
		batch := repos[start:end]

		var res map[string]json.RawMessage
		err := c.GraphQL(ctx, build(batch), nil, &res)
		if err != nil {
			retryAt, recoverable := aliasRetry(err, len(batch))
			switch {
			case !recoverable:
				failed = errors.Join(failed, err)
			case retryAt > 0:
				failed = errors.Join(failed, aliasBatch(ctx, c, batch, retryAt, build, emit))
			}
			continue
		}
		decodeAliases(batch, res, emit)
	}
	return failed
}

// aliasRetry decides what to do with a batch the gateway refused: the size to
// ask again at, and whether the refusal is recoverable at all. A recoverable
// refusal with a size of zero is a batch to drop.
//
// Two different refusals reach here as one error and they want opposite
// things.
//
// A query the gateway gave up on is too large. Halving it is the only recovery
// there is, and one repository that is still too large is a real failure.
//
// A NOT_FOUND or FORBIDDEN is one repository of the batch that was renamed
// away or is no longer visible to this token, and it costs the rest of the
// batch.
// Measured on 2026-09-10 against a batch of three aliases with one made-up
// name: HTTP 200, cost 1, the data for the two that exist, r1 null, and an
// errors array naming r1. internal/ghapi returns that errors array as the
// error and drops the body with it, so the only way to keep the repositories
// that did answer is to ask for them one at a time. A batch that is already
// one repository is the one that is gone, and dropping it is what every other
// GraphQL collector in this package does with the same error. Asking one at a
// time costs one point per repository, and only for the sweep that discovers
// it: the alternative is losing the rest of the batch on every sweep for as
// long as that repository stays in the list.
func aliasRetry(err error, batch int) (int, bool) {
	if _, tooLarge := errors.AsType[*ghapi.TooLargeError](err); tooLarge {
		if batch > 1 {
			return batch / 2, true
		}
		return 0, false
	}
	// A spent budget and a canceled sweep arrive here too, and neither gets
	// smaller by being asked again: isSkippableGraphQL is what separates them
	// from a repository that is not there.
	if !isSkippableGraphQL(err) {
		return 0, false
	}
	if batch > 1 {
		return 1, true
	}
	return 0, true
}

// decodeAliases hands each repository the node its own alias came back under.
func decodeAliases[T any](batch []Repo, res map[string]json.RawMessage, emit func(repo Repo, node T)) {
	for i, repo := range batch {
		raw, ok := res["r"+strconv.Itoa(i)]
		if !ok || string(raw) == "null" {
			continue
		}
		var node T
		if err := json.Unmarshal(raw, &node); err != nil {
			continue
		}
		emit(repo, node)
	}
}

// aliasQuery writes `query { r0: repository(...) { <selection> } ... }` and
// appends the fragment the selections refer to.
func aliasQuery(batch []Repo, selection func(i int) string, fragment string) string {
	var b strings.Builder
	b.WriteString("query {")
	for i, repo := range batch {
		fmt.Fprintf(&b, " r%d: repository(owner: %q, name: %q) { %s }", i, repo.Owner, repo.Name, selection(i))
	}
	b.WriteString(" }")
	b.WriteString(fragment)
	return b.String()
}

// Deployments records every deployment GitHub has kept, dated when it was
// created, with the state of its latest status.
//
// This is the highest-volume dated fact the account has: 1,423 of them across
// ten repositories, against 24 releases. It is what turns deployment frequency
// from a guess based on releases into a measurement, and crossing commit with
// gh_commit gives the lead time from a commit to the deployment that carried
// it.
//
// REST cannot answer it. Measured: GET /repos/{r}/deployments returns objects
// whose keys are created_at, creator, description, environment, id, node_id,
// original_environment, payload, performed_via_github_app, ref,
// repository_url, sha, statuses_url, task, transient_environment, updated_at
// and url. There is no state anywhere in that object, so whether a deployment
// worked costs one more call per deployment: 433 on phonometry alone. In
// GraphQL latestStatus rides along, and ten repositories of a hundred
// deployments each cost one point.
//
// Two things about the shape of this data, both measured rather than assumed:
//
// Both measured on 2026-09-10 over the same reading: the ten repositories of
// this account that have deployments, newest hundred each, one query, HTTP
// 200, cost 1, nodeCount 1000, 5.0 s, 680 nodes.
//
//   - state mutates. A deployment that succeeded becomes INACTIVE when the
//     next one replaces it: 583 of those 680, and the status history says what
//     INACTIVE means (a superseded deployment carries SUCCESS at 10:44 and
//     INACTIVE at 12:56, when its replacement went live). INACTIVE means
//     "succeeded and was superseded", never failed, which is why the state tag
//     carries the outcome and the raw enum is a field: the outcome does not
//     move, so re-reading the same deployment rewrites its row instead of
//     adding a second one under a new tag value. A tag that moved would not
//     converge, it would open a second series at the same timestamp.
//   - a deployment needs its own identity tag. Two deployments of the same
//     environment can share a second: 14 of those 680 collide on environment,
//     task, outcome and createdAt, and without the id the second silently
//     replaces the first, exactly as a pull request would without its
//     number.
type Deployments struct {
	Repos []Repo
	// Batch is how many repositories go into one query. Zero means five.
	Batch int
	// First bounds the deployments asked for per repository per page. Zero
	// means a hundred, which is the API's own maximum.
	First int
	// Walk pages further back. A sweep wants the newest page; a backfill walks
	// until the deployments are older than its bound. The CREATED_AT ordering
	// makes that one cursor per repository.
	Walk Walk
}

// deploymentsFragment is on the connection rather than on Repository, so each
// repository in a batch can carry its own cursor.
const deploymentsFragment = `
fragment deploypage on DeploymentConnection {
  totalCount
  pageInfo { hasNextPage endCursor }
  nodes {
    databaseId createdAt environment state task
    latestStatus { state createdAt logUrl environmentUrl }
    creator { login } commit { oid } ref { name }
  }
}`

type deploymentPage struct {
	Deployments struct {
		TotalCount int              `json:"totalCount"`
		PageInfo   pageInfo         `json:"pageInfo"`
		Nodes      []deploymentNode `json:"nodes"`
	} `json:"deployments"`
}

type deploymentNode struct {
	DatabaseID  int64     `json:"databaseId"`
	CreatedAt   time.Time `json:"createdAt"`
	Environment string    `json:"environment"`
	State       string    `json:"state"`
	Task        string    `json:"task"`
	// LatestStatus is nullable in the schema: a deployment nobody ever
	// reported a status for carries none. None of the 680 read on 2026-09-10
	// was in that state, so the guard is the schema's word rather than a
	// measured case, and the fields it feeds are simply left out when it is
	// absent.
	LatestStatus *struct {
		State          string    `json:"state"`
		CreatedAt      time.Time `json:"createdAt"`
		LogURL         string    `json:"logUrl"`
		EnvironmentURL string    `json:"environmentUrl"`
	} `json:"latestStatus"`
	Creator *struct {
		Login string `json:"login"`
	} `json:"creator"`
	Commit *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	// Ref is null once the branch or tag it named is gone: 41 of the 680 nodes
	// read on 2026-09-10, spread across all three environments rather than
	// belonging to one kind of deployment.
	Ref *struct {
		Name string `json:"name"`
	} `json:"ref"`
}

func (d Deployments) Collect(ctx context.Context, c *ghapi.Client, _ time.Time) ([]sink.Point, error) {
	if len(d.Repos) == 0 {
		return nil, nil
	}
	first := d.First
	if first <= 0 {
		first = 100
	}
	// One cursor per repository, so a batch can carry repositories that are on
	// different pages of their own history.
	after := make(map[string]string, len(d.Repos))
	todo := d.Repos
	var points []sink.Point
	var failed error

	for page := 1; page <= d.Walk.limit(1) && len(todo) > 0; page++ {
		var next []Repo
		build := func(batch []Repo) string {
			return aliasQuery(batch, func(i int) string {
				return "deployments(" + d.args(first, after[batch[i].FullName]) + ") { ...deploypage }"
			}, deploymentsFragment)
		}
		err := aliasBatch(ctx, c, todo, d.Batch, build, func(repo Repo, node deploymentPage) {
			points = append(points, d.points(repo, node.Deployments.Nodes)...)
			nodes, info := node.Deployments.Nodes, node.Deployments.PageInfo
			if info.HasNextPage && len(nodes) > 0 && !d.Walk.past(nodes[len(nodes)-1].CreatedAt) {
				after[repo.FullName] = info.EndCursor
				next = append(next, repo)
			}
		})
		failed = errors.Join(failed, err)
		todo = next
	}

	// Only a total failure is a failure: a batch that lost one repository to a
	// rename must not mark the family as not having run.
	if len(points) == 0 && failed != nil {
		return nil, failed
	}
	return points, nil
}

// args writes the connection arguments, newest first so a walk can stop at a
// date rather than at a page.
func (Deployments) args(first int, cursor string) string {
	args := "first: " + strconv.Itoa(first) + ", orderBy: {field: CREATED_AT, direction: DESC}"
	if cursor != "" {
		args += ", after: " + strconv.Quote(cursor)
	}
	return args
}

func (Deployments) points(repo Repo, nodes []deploymentNode) []sink.Point {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	points := make([]sink.Point, 0, len(nodes))
	for i := range nodes {
		points = append(points, nodes[i].point(repo, base))
	}
	return points
}

func (n *deploymentNode) point(repo Repo, base map[string]string) sink.Point {
	// The deployment's own state, which is the authoritative one. The status
	// only speaks when the deployment carries no state of its own.
	state := n.State
	statusAt := time.Time{}
	fields := map[string]any{
		"deployments": 1,
		"creator":     login(n.Creator),
	}
	// The environment's own page, which is where a reader goes to see what is
	// live now.
	setNonEmpty(fields, "url", githubPage(repo.FullName, "deployments"))
	if s := n.LatestStatus; s != nil {
		statusAt = s.CreatedAt
		if state == "" {
			state = s.State
		}
		setNonEmpty(fields, "log_url", s.LogURL)
		setNonEmpty(fields, "environment_url", s.EnvironmentURL)
		if id := runID(s.LogURL); id > 0 {
			// The workflow run that performed the deployment, so a row here
			// joins gh_workflow_run without taking the URL apart in a panel.
			fields["run_id"] = id
		}
	}
	if n.Commit != nil {
		setNonEmpty(fields, "commit", n.Commit.OID)
	}
	if n.Ref != nil {
		setNonEmpty(fields, "ref", n.Ref.Name)
	}
	// The raw enum as a field, because it is the one thing here that moves:
	// ACTIVE becomes INACTIVE the moment the next deployment replaces this
	// one. Re-reading it rewrites this value in place rather than opening a
	// second series next to the first.
	fields["deployment_state"] = strings.ToLower(state)
	outcome := deploymentOutcome(state)
	// The outcome too: a deployment is pending before it is anything else,
	// and one held at an approval stays pending for weeks (seventy four days
	// on this account) before it succeeds, so as the tag `state` a sweep that
	// saw it pending and the next that saw it succeed were two rows at the
	// deployment's own instant for ever. Under a new name so a database that
	// already holds the tag column keeps accepting writes.
	fields["outcome"] = outcome
	fields["success"] = outcome == "success"
	fields["superseded"] = strings.EqualFold(state, "INACTIVE")
	n.durations(fields, state, statusAt)

	return sink.Point{
		Measurement: "gh_deployment",
		Tags: merge(base, map[string]string{
			// The identity of the deployment. Two of them can share a second
			// in the same environment, and without this the second overwrites
			// the first.
			"deployment":  strconv.FormatInt(n.DatabaseID, 10),
			"environment": orNone(n.Environment),
			"task":        orNone(n.Task),
		}),
		Fields: fields,
		// Dated when the deployment was created. The status moves for years
		// afterwards; the deployment happened once.
		Time: n.CreatedAt,
	}
}

// durations records how long the deployment took, and how long it stayed live.
//
// The two are the same subtraction against different meanings, which is the
// trap in this data: for a superseded deployment latestStatus.createdAt is the
// moment its replacement went live, not the moment it finished. Measured: a
// deployment created at 10:42:33 finished at 10:44:23 and was marked INACTIVE
// at 12:56:34. Writing those 8,041 seconds as "seconds to status" would
// overwrite the 110 s a sweep taken while it was still ACTIVE recorded,
// so each meaning gets its own field and neither is ever written over the
// other.
func (n *deploymentNode) durations(fields map[string]any, state string, statusAt time.Time) {
	if statusAt.IsZero() || n.CreatedAt.IsZero() {
		return
	}
	seconds := int(statusAt.Sub(n.CreatedAt).Seconds())
	if strings.EqualFold(state, "INACTIVE") {
		fields["seconds_live"] = seconds
		return
	}
	fields["seconds_to_status"] = seconds
}

// deploymentOutcome collapses GitHub's DeploymentState into the part of it
// that does not move, so the tag is stable across re-reads.
//
// ACTIVE and INACTIVE are the same outcome seen before and after a
// replacement, and a status of SUCCESS is the same fact reported from the
// status rather than from the deployment. A deployment caught in flight is the
// one case that still moves: its `pending` row stays behind when the next
// sweep writes the same deployment as `success`.
//
// Skipping an in-flight deployment, the way Actions skips a run that has not
// finished, would remove that stale row and was measured before being
// rejected. Across the 680 deployments of this account on 2026-09-10, none was
// in flight at read time and one has been QUEUED since 2026-06-28, 74 days:
// a deployment held at an approval never reaches a terminal state, so skipping
// would not defer that row, it would delete it. A stale `pending` row for the
// few deployments a sweep catches inside their own first minutes is the
// cheaper of the two errors, and it is visible for what it is.
func deploymentOutcome(state string) string {
	switch strings.ToUpper(state) {
	case "ACTIVE", "INACTIVE", "SUCCESS":
		return "success"
	case "FAILURE":
		return "failure"
	case "ERROR":
		return "error"
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING":
		return "pending"
	case "":
		return noneTag
	default:
		// DESTROYED and ABANDONED, and whatever GitHub adds next.
		return strings.ToLower(state)
	}
}

// runID pulls the workflow run out of a status log URL, which looks like
// https://github.com/{owner}/{repo}/actions/runs/{run}/job/{job}. A status
// posted by anything that is not Actions has no run in it, and says so by
// returning zero.
func runID(logURL string) int64 {
	const marker = "/actions/runs/"
	i := indexOf(logURL, marker)
	if i < 0 {
		return 0
	}
	rest := logURL[i+len(marker):]
	if j := indexOf(rest, "/"); j >= 0 {
		rest = rest[:j]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// setNonEmpty keeps a field out of the point rather than writing an empty
// string, so a store that types a column on first write is not taught that
// this one is sometimes blank.
func setNonEmpty(fields map[string]any, name, value string) {
	if value != "" {
		fields[name] = value
	}
}
