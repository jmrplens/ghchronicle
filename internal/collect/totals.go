package collect

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Totals records the numbers that are true since the beginning of the account,
// as numbers rather than as a history to be added up.
//
// Every other collector here writes one row per thing that happened, which is
// the right shape for "how many in July" and the wrong shape for "how many
// ever". Answering the second from the first means scanning the whole table on
// every refresh: slow in any store, and refused outright by InfluxDB 3 Core,
// which caps a query at ten thousand Parquet files and counts them before it
// aggregates anything. A tile that wants a lifetime number should read one row.
//
// So GitHub is asked to do the counting, which it does for free:
//
//   - Search reports a count for any query, so "pull requests merged ever"
//     is one alias of one query whether the answer is ten or ten thousand.
//   - GraphQL reports totalCount on every connection, so a repository's whole
//     lifetime, commits included, arrives in a batch that costs one point for
//     several repositories at once.
//
// The result is available on the first sweep of a fresh install, which a
// running total accumulated by this tool would not be.
type Totals struct {
	Login string
	Repos []Repo
	// Archived is the archived repositories the filter set aside, which get
	// one row and one query between them: the date each was archived, as
	// gh_repo_archived. Nothing else is asked about them, because nothing
	// else about an archived repository moves. The listing that set them
	// aside cannot supply the date itself: REST carries no archived_at, and
	// its updated_at is not it either, measured on 2026-09-12 against the
	// GraphQL archivedAt of the same repositories, two seconds to eight
	// minutes later on five of them. The query is asked on every totals
	// sweep, one point at the family's cadence, because the rows are the
	// same rows each time and the exporters keep only what is rewritten.
	Archived []Repo
	// Batch is how many repositories go into one GraphQL query. Zero means
	// ten. The gateway gives up on a query it cannot finish in about ten
	// seconds, and a batch that hits that is halved rather than lost.
	Batch int
}

// searchCounts are the lifetime counts search answers, keyed by the field a
// dashboard reads, which is the part that must not drift. Ten of the eleven
// come from one GraphQL query with an alias per count: issueCount is what
// REST's total_count is, and measured on 2026-09-11 all ten matched the REST
// answers of the same minute exactly, for 406 bytes at cost 1 where REST is
// ten requests out of the search bucket of thirty a minute, each a page of
// one item it then throws away. Only the commit count stays in REST, below,
// because search has no COMMIT type in GraphQL.
func (t Totals) searchCounts() []searchCount {
	login := t.Login
	issues := func(field, q string) searchCount {
		return searchCount{field: field, kind: "ISSUE", query: q}
	}
	return []searchCount{
		issues("pulls_opened", "type:pr author:"+login),
		issues("pulls_merged", "type:pr author:"+login+" is:merged"),
		issues("pulls_open_now", "type:pr author:"+login+" is:open"),
		// Merged somewhere that is not this account: the work that a sweep
		// over one's own repositories cannot see at all.
		issues("pulls_merged_elsewhere", "type:pr author:"+login+" is:merged -user:"+login),
		issues("pulls_reviewed", "type:pr reviewed-by:"+login),
		issues("issues_opened", "type:issue author:"+login),
		issues("issues_closed", "type:issue author:"+login+" is:closed"),
		issues("issues_elsewhere", "type:issue author:"+login+" -user:"+login),
		issues("commented_elsewhere", "commenter:"+login+" -author:"+login),
		{field: "repositories", kind: "REPOSITORY", query: "user:" + login},
	}
}

// searchCount is one lifetime count: the field it is written as, which is
// also its alias in the query, the search type and the query.
type searchCount struct {
	field string
	kind  string
	query string
}

// countsQuery writes the ten counts as one query, one alias each. The alias
// is the field name, so decoding the answer is reading it by field.
func countsQuery(counts []searchCount) string {
	var b strings.Builder
	b.WriteString("query {")
	for _, c := range counts {
		field := "issueCount"
		if c.kind == "REPOSITORY" {
			field = "repositoryCount"
		}
		fmt.Fprintf(&b, " %s: search(type: %s, query: %q) { %s }", c.field, c.kind, c.query, field)
	}
	b.WriteString(" }")
	return b.String()
}

// commitsSearch is the one count GraphQL cannot answer, still a REST search
// of a page of one.
const commitsSearch = "/search/commits?per_page=1&q="

const totalsFragment = `
fragment totals on Repository {
  nameWithOwner url databaseId createdAt pushedAt isFork isArchived archivedAt isPrivate
  diskUsage stargazerCount forkCount
  watchers { totalCount }
  issuesOpen: issues(states: OPEN) { totalCount }
  issuesClosed: issues(states: CLOSED) { totalCount }
  pullsOpen: pullRequests(states: OPEN) { totalCount }
  pullsMerged: pullRequests(states: MERGED) { totalCount }
  pullsClosed: pullRequests(states: CLOSED) { totalCount }
  releases { totalCount }
  discussions { totalCount }
  labels { totalCount }
  milestones { totalCount }
  branches: refs(refPrefix: "refs/heads/") { totalCount }
  tags: refs(refPrefix: "refs/tags/") { totalCount }
  defaultBranchRef { target { ... on Commit { history { totalCount } } } }
  isSecurityPolicyEnabled hasVulnerabilityAlertsEnabled
  forkingAllowed hasDiscussionsEnabled hasIssuesEnabled
  hasWikiEnabled hasSponsorshipsEnabled isBlankIssuesEnabled
  autoMergeAllowed deleteBranchOnMerge
  mergeCommitAllowed rebaseMergeAllowed squashMergeAllowed
  fundingLinks { platform }
  issueTemplates { name }
  issueForms: object(expression: "HEAD:.github/ISSUE_TEMPLATE") { ... on Tree { entries { name } } }
  pullRequestTemplates { filename }
  branchProtectionRules { totalCount }
  codeowners { errors { kind } }
}`

// repoTotals is one repository's lifetime, as GraphQL reports it.
type repoTotals struct {
	NameWithOwner string    `json:"nameWithOwner"`
	URL           string    `json:"url"`
	DatabaseID    int64     `json:"databaseId"`
	CreatedAt     time.Time `json:"createdAt"`
	PushedAt      time.Time `json:"pushedAt"`
	IsFork        bool      `json:"isFork"`
	IsArchived    bool      `json:"isArchived"`
	// ArchivedAt is the instant the repository was archived, which REST does
	// not report at all. Verified on 2026-09-10 by printing the keys of
	// GET /repos/{r}: an archived repository and a live one both carry
	// `archived` and neither carries `archived_at`. A pointer because it is
	// null on every repository that is not archived.
	ArchivedAt    *time.Time `json:"archivedAt"`
	IsPrivate     bool       `json:"isPrivate"`
	DiskUsage     int        `json:"diskUsage"`
	Stars         int        `json:"stargazerCount"`
	Forks         int        `json:"forkCount"`
	Watchers      count      `json:"watchers"`
	IssuesOpen    count      `json:"issuesOpen"`
	IssuesClosed  count      `json:"issuesClosed"`
	PullsOpen     count      `json:"pullsOpen"`
	PullsMerged   count      `json:"pullsMerged"`
	PullsClosed   count      `json:"pullsClosed"`
	Releases      count      `json:"releases"`
	Discussions   count      `json:"discussions"`
	Labels        count      `json:"labels"`
	Milestones    count      `json:"milestones"`
	Branches      count      `json:"branches"`
	Tags          count      `json:"tags"`
	DefaultBranch *struct {
		Target struct {
			History count `json:"history"`
		} `json:"target"`
	} `json:"defaultBranchRef"`

	// The settings, which a previous pass had costed at eleven REST endpoints
	// and a hundred and ninety eight calls. Here they are part of a batch that
	// already costs one point.
	SecurityPolicy bool `json:"isSecurityPolicyEnabled"`
	// VulnerabilityAlerts is whether this repository will hand over Dependabot
	// alerts at all. Verified on 2026-09-08 against the REST equivalent
	// GET /repos/{r}/vulnerability-alerts over all 52 repositories of the
	// account: 204 on 7 and 404 on 45, the same 7 the schema reports true.
	// It rides free in this batch, and it is the second, independent source
	// for the `enabled` that Security writes per feature.
	VulnerabilityAlerts bool                        `json:"hasVulnerabilityAlertsEnabled"`
	ForkingAllowed      bool                        `json:"forkingAllowed"`
	HasDiscussions      bool                        `json:"hasDiscussionsEnabled"`
	HasIssues           bool                        `json:"hasIssuesEnabled"`
	HasWiki             bool                        `json:"hasWikiEnabled"`
	HasSponsorships     bool                        `json:"hasSponsorshipsEnabled"`
	BlankIssues         bool                        `json:"isBlankIssuesEnabled"`
	AutoMerge           bool                        `json:"autoMergeAllowed"`
	DeleteOnMerge       bool                        `json:"deleteBranchOnMerge"`
	MergeCommit         bool                        `json:"mergeCommitAllowed"`
	RebaseMerge         bool                        `json:"rebaseMergeAllowed"`
	SquashMerge         bool                        `json:"squashMergeAllowed"`
	FundingLinks        []struct{ Platform string } `json:"fundingLinks"`
	// IssueTemplates is what GraphQL calls the repository's issue templates,
	// and it lists the Markdown ones only. Verified on 2026-09-12 against
	// jmrplens/gitlab-mcp-server: the field answers [] while
	// .github/ISSUE_TEMPLATE holds four YAML issue forms and a config.yml.
	// IssueForms is that directory read as a tree, which lists both kinds,
	// so a repository that moved to forms stops reading as having none. It
	// rides in the same batch: measured on the same day, a batch of ten with
	// and without the object costs one point either way. Nil when the
	// directory does not exist, which is when the Markdown list is all
	// there is (a lone .github/ISSUE_TEMPLATE.md is listed there and lives
	// outside the directory).
	IssueTemplates []struct{ Name string } `json:"issueTemplates"`
	IssueForms     *struct {
		Entries []struct{ Name string } `json:"entries"`
	} `json:"issueForms"`
	PullRequestTmpls []struct{ Filename string } `json:"pullRequestTemplates"`
	BranchProtection count                       `json:"branchProtectionRules"`
	Codeowners       *struct {
		Errors []struct{ Kind string } `json:"errors"`
	} `json:"codeowners"`
}

func (t Totals) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	var points []sink.Point
	var failed error

	if t.Login != "" {
		fields, err := t.accountCounts(ctx, c)
		failed = errors.Join(failed, err)
		if len(fields) > 0 {
			setNonEmpty(fields, "url", githubPage(t.Login))
			points = append(points, sink.Point{
				Measurement: "gh_account_total",
				Tags:        map[string]string{"user": t.Login},
				Fields:      fields,
				Time:        now,
			})
		}
	}

	repoPoints, err := t.repoTotals(ctx, c, t.Repos, now)
	points = append(points, repoPoints...)
	failed = errors.Join(failed, err)

	archivedPoints, err := archivedDates(ctx, c, t.Archived)
	points = append(points, archivedPoints...)
	failed = errors.Join(failed, err)

	// Only a total failure is a failure. A family that returns nothing at all
	// should say why; one that returned most of its rows should not be marked
	// as not having run.
	if len(points) == 0 && failed != nil {
		return nil, failed
	}
	return points, nil
}

// accountCounts reads the eleven lifetime counts: ten from one GraphQL query,
// the commit count from REST. One missing number is not a reason to throw
// away the ones that answered, so each source fails on its own and the row
// carries whatever came back.
func (t Totals) accountCounts(ctx context.Context, c *ghapi.Client) (map[string]any, error) {
	fields := map[string]any{}
	var failed error

	counts := t.searchCounts()
	if err := t.readCounts(ctx, c, counts, fields); err != nil {
		failed = errors.Join(failed, fmt.Errorf("search counts: %w", err))
		// GraphQL answers one errors array for the whole query when a single
		// alias fails, and the client drops the data with it, so the nine
		// that answered are asked again on their own: one refused search
		// loses one number, as it did when the eleven were REST. Ten points,
		// spent only by the sweep that needed them, and never when the
		// budget is what refused.
		if retryOneByOne(ctx, err) {
			for _, sc := range counts {
				failed = errors.Join(failed, t.readCounts(ctx, c, []searchCount{sc}, fields))
			}
		}
	}

	var commits struct {
		TotalCount int `json:"total_count"`
	}
	if _, _, err := c.GetJSON(ctx, commitsSearch+url.QueryEscape("author:"+t.Login), &commits, ""); err != nil {
		// Search has its own bucket of thirty a minute, and it is the first
		// one to run out.
		failed = errors.Join(failed, fmt.Errorf("commits: %w", err))
	} else {
		fields["commits"] = commits.TotalCount
	}
	return fields, failed
}

// readCounts asks one query for the counts given and writes the ones that
// answered into fields under their own names.
func (Totals) readCounts(ctx context.Context, c *ghapi.Client, counts []searchCount, fields map[string]any) error {
	var res map[string]struct {
		IssueCount      int `json:"issueCount"`
		RepositoryCount int `json:"repositoryCount"`
	}
	if err := c.GraphQL(ctx, countsQuery(counts), nil, &res); err != nil {
		return err
	}
	for _, sc := range counts {
		n, ok := res[sc.field]
		if !ok {
			continue
		}
		if sc.kind == "REPOSITORY" {
			fields[sc.field] = n.RepositoryCount
			continue
		}
		fields[sc.field] = n.IssueCount
	}
	return nil
}

// retryOneByOne says whether a refused counts query is worth ten smaller
// ones: not once the sweep is canceled, and not when the budget is what
// refused, which ten more queries would only spend further. The GraphQL
// bucket reports that as an errors array of type RATE_LIMITED, which the
// client hands back as text, and a REST-shaped 403 as RateLimitedError.
func retryOneByOne(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if _, limited := errors.AsType[*ghapi.RateLimitedError](err); limited {
		return false
	}
	return !strings.Contains(err.Error(), "graphql: RATE_LIMITED:")
}

// repoTotals asks for several repositories in one query. aliasBatch owns both
// refusals: it halves a batch the gateway gave up on, and asks one repository
// at a time when one of them was renamed away or hidden, so neither costs the
// rest of the group.
func (t Totals) repoTotals(ctx context.Context, c *ghapi.Client, repos []Repo, now time.Time) ([]sink.Point, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	size := t.Batch
	if size <= 0 {
		size = 10
	}
	build := func(batch []Repo) string {
		return aliasQuery(batch, func(int) string { return "...totals" }, totalsFragment)
	}
	var points []sink.Point
	failed := aliasBatch(ctx, c, repos, size, build, func(repo Repo, rt repoTotals) {
		points = append(points, rt.point(repo, now), rt.policy(repo, now))
		if archived, isArchived := rt.archived(repo); isArchived {
			points = append(points, archived)
		}
	})
	if len(points) == 0 && failed != nil {
		return nil, failed
	}
	return points, nil
}

func (rt *repoTotals) point(repo Repo, now time.Time) sink.Point {
	commits := 0
	if rt.DefaultBranch != nil {
		commits = rt.DefaultBranch.Target.History.TotalCount
	}
	return sink.Point{
		Measurement: "gh_repo_total",
		Tags: map[string]string{
			"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName,
			"fork": boolTag(rt.IsFork), "archived": boolTag(rt.IsArchived),
			"visibility": visibility(rt.IsPrivate),
		},
		Fields: map[string]any{
			"commits": commits,
			"stars":   rt.Stars, "forks": rt.Forks, "watchers": rt.Watchers.TotalCount,
			"issues_open": rt.IssuesOpen.TotalCount, "issues_closed": rt.IssuesClosed.TotalCount,
			"issues":     rt.IssuesOpen.TotalCount + rt.IssuesClosed.TotalCount,
			"pulls_open": rt.PullsOpen.TotalCount, "pulls_merged": rt.PullsMerged.TotalCount,
			"pulls_closed": rt.PullsClosed.TotalCount,
			"pulls":        rt.PullsOpen.TotalCount + rt.PullsMerged.TotalCount + rt.PullsClosed.TotalCount,
			"releases":     rt.Releases.TotalCount, "discussions": rt.Discussions.TotalCount,
			"labels": rt.Labels.TotalCount, "milestones": rt.Milestones.TotalCount,
			"branches": rt.Branches.TotalCount, "tags": rt.Tags.TotalCount,
			"size_kb": rt.DiskUsage, "repo_id": rt.DatabaseID,
			"age_days":        int(now.Sub(rt.CreatedAt).Hours() / 24),
			"days_since_push": int(now.Sub(rt.PushedAt).Hours() / 24),
			"url":             rt.URL,
		},
		Time: now,
	}
}

// policy is what the repository allows, as a row of booleans.
//
// A previous pass priced this at eleven REST endpoints and a hundred and
// ninety eight calls per sweep, and shelved it. In the batch above it is free.
// Two of these read better than their REST equivalents: branchProtectionRules
// counts the classic protections without having to read a 404 as "none", and
// codeowners.errors is the only thing that would ever say a CODEOWNERS file
// has stopped working.
func (rt *repoTotals) policy(repo Repo, now time.Time) sink.Point {
	codeownerErrors, hasCodeowners := 0, false
	if rt.Codeowners != nil {
		hasCodeowners, codeownerErrors = true, len(rt.Codeowners.Errors)
	}
	return sink.Point{
		Measurement: "gh_repo_policy",
		Tags: map[string]string{
			"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName,
		},
		Fields: withURL(map[string]any{
			"security_policy": rt.SecurityPolicy, "forking_allowed": rt.ForkingAllowed,
			"vulnerability_alerts": rt.VulnerabilityAlerts,
			"discussions":          rt.HasDiscussions, "issues": rt.HasIssues,
			"wiki": rt.HasWiki, "sponsorships": rt.HasSponsorships,
			"blank_issues": rt.BlankIssues, "auto_merge": rt.AutoMerge,
			"delete_branch_on_merge": rt.DeleteOnMerge,
			"merge_commit":           rt.MergeCommit, "rebase_merge": rt.RebaseMerge,
			"squash_merge":            rt.SquashMerge,
			"funding_links":           len(rt.FundingLinks),
			"issue_templates":         rt.issueTemplateCount(),
			"pull_request_templates":  len(rt.PullRequestTmpls),
			"branch_protection_rules": rt.BranchProtection.TotalCount,
			"codeowners":              hasCodeowners,
			"codeowners_errors":       codeownerErrors,
		}, pageURL(rt.URL, "settings")),
		Time: now,
	}
}

// issueTemplateCount is how many issue templates the repository offers, of
// either kind. The directory is counted when it exists, leaving out
// config.yml (the chooser's settings, not a template) and anything that is
// not a template file, and the Markdown list is the floor: GraphQL lists a
// Markdown template kept outside the directory that the tree cannot see.
func (rt *repoTotals) issueTemplateCount() int {
	n := 0
	if rt.IssueForms != nil {
		for _, e := range rt.IssueForms.Entries {
			if isIssueTemplateFile(e.Name) {
				n++
			}
		}
	}
	return max(n, len(rt.IssueTemplates))
}

// isIssueTemplateFile is whether a name in .github/ISSUE_TEMPLATE is a
// template: a YAML issue form or a Markdown template, and not the chooser's
// config.yml, which GitHub documents under the same directory.
func isIssueTemplateFile(name string) bool {
	lower := strings.ToLower(name)
	if lower == "config.yml" || lower == "config.yaml" {
		return false
	}
	return strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".md")
}

// archivedFragment is the whole of what an archived repository set aside by
// the filter is asked: enough for its gh_repo_archived row and not a
// connection more. Four scalars per alias is why one query can carry every
// archived repository of an account at once where totalsFragment takes ten.
const archivedFragment = `
fragment archived on Repository { nameWithOwner url createdAt archivedAt }`

// archivedBatch is how many of them go into that query. The gateway's ten
// second ceiling is what caps totalsFragment at ten aliases, and fifty of
// these scalars are a fraction of one of those; aliasBatch still halves on a
// refusal, so a bigger batch costs a retry and never a repository.
const archivedBatch = 50

// archivedDates writes the archive row of repositories the sweep does not
// otherwise collect. Every alias decodes into repoTotals, of which the query
// fills the four fields archived reads, so the row is the same row a
// collected repository gets from its own totals batch.
func archivedDates(ctx context.Context, c *ghapi.Client, repos []Repo) ([]sink.Point, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	build := func(batch []Repo) string {
		return aliasQuery(batch, func(int) string { return "...archived" }, archivedFragment)
	}
	var points []sink.Point
	failed := aliasBatch(ctx, c, repos, archivedBatch, build, func(repo Repo, rt repoTotals) {
		if point, isArchived := rt.archived(repo); isArchived {
			points = append(points, point)
		}
	})
	if len(points) == 0 && failed != nil {
		return nil, failed
	}
	return points, nil
}

// archived is the one row that carries a date rather than a state: the instant
// this repository stopped taking work.
//
// gh_repo_total tags `archived` as a boolean stamped now, so eighteen archived
// repositories read as one undifferentiated fact. They were not: measured on
// 2026-09-10 across all 52 repositories of the account, the eighteen were
// archived in batches, eight of them within eight seconds of each other on
// 2026-07-11 and three more on 2026-08-29. Dated at archivedAt, that is a
// visible clear-out; dated now it is a number that never moves.
func (rt *repoTotals) archived(repo Repo) (sink.Point, bool) {
	if rt.ArchivedAt == nil || rt.ArchivedAt.IsZero() {
		return sink.Point{}, false
	}
	fields := map[string]any{"archived": 1, "url": rt.URL}
	// How long the repository lived before it was closed. Absent rather than
	// wrong when there is no creation date to subtract from.
	if !rt.CreatedAt.IsZero() {
		fields["age_days_at_archive"] = int(rt.ArchivedAt.Sub(rt.CreatedAt).Hours() / 24)
	}
	return sink.Point{
		Measurement: "gh_repo_archived",
		Tags: map[string]string{
			"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName,
		},
		Fields: fields,
		Time:   *rt.ArchivedAt,
	}, true
}
