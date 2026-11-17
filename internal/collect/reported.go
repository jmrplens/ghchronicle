package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Reported finds the security advisories the account reported, authored, is
// credited in or collaborates on, in repositories that are not its own.
//
// No endpoint lists them. Measured on 2026-11-16 against the whole GraphQL
// schema and REST's OpenAPI description 1.1.4: GraphQL has no repository
// advisories and no credits, REST lists advisories only per repository, per
// organization and in the global database, and the global listing ignores a
// `credit=` filter. What a reporter can do is read a repository's list, which
// shows them their own reports in every state. So the collector builds the
// set of repositories worth asking from four sources, each of which found
// something the others did not or would not:
//
//   - the inbox: a RepositoryAdvisory notification arrives when a report
//     changes state or is commented on, in a repository the account never
//     touched otherwise, and the inbox keeps it for months only;
//   - the issues and pull requests that involve the account elsewhere, and
//   - the repositories GitHub says the account contributed to: a report
//     still in triage has notified nothing yet, and these two held every
//     repository with a report on the day this was measured;
//   - the reviewed global database, read by update: an advisory there that
//     credits the account names its repository even when the account did
//     nothing else on GitHub.
//
// Every repository the inbox or the database named, and every one that
// turned out to hold an advisory, is handed back for the state file to keep,
// since the notification goes and the search may not find it again.
type Reported struct {
	Login string
	// Own is the owners whose repositories are the account's, itself and the
	// organizations configured, and Collected the repositories the
	// advisories family reads, named ones of other owners included. The
	// advisories of either are received, not reported, and reading them here
	// would write a report the account sent to a repository it collects twice.
	Own       []string
	Collected []string
	// Known is the repositories an earlier run remembered.
	Known []string
	// Notified reads the inbox since then; zero reads all of it.
	Notified time.Time
	// Scanned reads the global database back to then; zero walks all of it.
	Scanned time.Time
	// Walk bounds each repository's advisory list.
	Walk     Walk
	Details  bool
	Refusals *Refusals
}

// ReportedRun is what one run found.
type ReportedRun struct {
	Points []sink.Point
	// Remember is the repositories to add to the state file.
	Remember []string
	// Scanned says the global walk reached its bound or the end, so the
	// next run may start from this one.
	Scanned bool
	// InboxRead says the inbox was read to its end, so the next run may
	// read only what changed after this one started.
	InboxRead bool
}

const reportedSearchQuery = `query($query: String!, $after: String, $first: Int!) {
  search(query: $query, type: ISSUE, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on Issue { repository { nameWithOwner } }
      ... on PullRequest { repository { nameWithOwner } }
    }
  }
}`

const reportedContributedQuery = `query($after: String, $first: Int!) {
  viewer {
    repositoriesContributedTo(first: $first, after: $after, includeUserRepositories: false,
      contributionTypes: [COMMIT, ISSUE, PULL_REQUEST, PULL_REQUEST_REVIEW, REPOSITORY]) {
      pageInfo { hasNextPage endCursor }
      nodes { nameWithOwner }
    }
  }
}`

// reportedRepository is a repository as either GraphQL source names it.
type reportedRepository struct {
	NameWithOwner string `json:"nameWithOwner"`
}

// reportedConnection is a page of repositories GraphQL lists.
type reportedConnection struct {
	PageInfo reportedPageInfo     `json:"pageInfo"`
	Nodes    []reportedRepository `json:"nodes"`
}

type reportedPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// candidates is the set of repositories to ask, keyed without case as GitHub
// matches names, keeping the first spelling met.
type candidates struct {
	own       []string
	collected map[string]bool
	names     map[string]string
	remember  map[string]bool
}

func (c *candidates) add(fullName string, remember bool) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return
	}
	key := strings.ToLower(fullName)
	if c.collected[key] || slices.ContainsFunc(c.own, func(o string) bool { return strings.EqualFold(o, owner) }) {
		return
	}
	if _, seen := c.names[key]; !seen {
		c.names[key] = fullName
	}
	if remember {
		c.remember[key] = true
	}
}

// Collect gathers the candidates, reads each one's advisories and keeps those
// that name the account. A source that fails is reported with everything the
// others found, so one bad answer costs that source and not the run.
func (r Reported) Collect(ctx context.Context, c *ghapi.Client, now time.Time) (ReportedRun, error) {
	set := &candidates{own: r.Own, collected: map[string]bool{}, names: map[string]string{}, remember: map[string]bool{}}
	for _, full := range r.Collected {
		set.collected[strings.ToLower(full)] = true
	}
	var errs []error
	inboxErr := r.inbox(ctx, c, set)
	if inboxErr != nil {
		errs = append(errs, fmt.Errorf("inbox: %w", inboxErr))
	}
	if err := r.involved(ctx, c, set); err != nil {
		errs = append(errs, fmt.Errorf("search: %w", err))
	}
	if err := r.contributed(ctx, c, set); err != nil {
		errs = append(errs, fmt.Errorf("contributions: %w", err))
	}
	scanned, err := r.credited(ctx, c, set)
	if err != nil {
		errs = append(errs, fmt.Errorf("global advisories: %w", err))
	}
	for _, k := range r.Known {
		set.add(k, true)
	}

	out := ReportedRun{Scanned: scanned, InboxRead: inboxErr == nil}
	keys := make([]string, 0, len(set.names))
	for k := range set.names {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	// A GHSA id is unique across GitHub, and one repository can be asked
	// under two names: a transferred one answers its old name through the
	// redirect, and a name typed by hand in the global database may differ
	// in case from the one a search returns. Each advisory is one point, its
	// tags taken from the page GitHub gives it rather than from the name it
	// was asked by.
	seen := map[string]bool{}
	kept := map[string]bool{}
	keep := func(full string) {
		if k := strings.ToLower(full); !kept[k] {
			kept[k] = true
			out.Remember = append(out.Remember, full)
		}
	}
	for _, k := range keys {
		full := set.names[k]
		rows, readErr := advisoryPages(ctx, c, r.Refusals, r.Walk, full)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", full, readErr))
		}
		for i := range rows {
			p, ok := r.point(&rows[i], full, now)
			if !ok || seen[rows[i].GHSAID] {
				continue
			}
			seen[rows[i].GHSAID] = true
			out.Points = append(out.Points, p)
			keep(p.Tags["full_name"])
		}
		if set.remember[k] {
			keep(full)
		}
	}
	return out, errors.Join(errs...)
}

// advisoryHome is the repository an advisory's own page belongs to, which
// is its current name whatever it was asked by, or fallback when the page is
// not a repository advisory's.
func advisoryHome(htmlURL, fallback string) string {
	rest, ok := strings.CutPrefix(htmlURL, "https://github.com/")
	if !ok {
		return fallback
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] != "security" || parts[3] != "advisories" {
		return fallback
	}
	return parts[0] + "/" + parts[1]
}

// inbox reads the notifications for the repositories of advisory threads.
//
// all=true is what keeps a thread already read, and it keeps one marked done
// as well: measured on 2026-11-16, two threads marked done through the API,
// one of September and one of that morning, were still listed with all=true
// minutes later and only left the list without it. The inbox still forgets
// a thread after months, which is why the found repositories are kept.
func (r Reported) inbox(ctx context.Context, c *ghapi.Client, set *candidates) error {
	since := ""
	if !r.Notified.IsZero() {
		since = "&since=" + r.Notified.UTC().Format(time.RFC3339)
	}
	// The whole inbox measured 98 pages of fifty on 2026-11-16, three months
	// of an active account; four hundred is a ceiling, not an estimate.
	for page := 1; page <= 400; page++ {
		var batch []struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			Subject struct {
				Type string `json:"type"`
			} `json:"subject"`
		}
		path := fmt.Sprintf("/notifications?all=true&per_page=50&page=%d%s", page, since)
		if _, _, err := c.GetJSON(ctx, path, &batch, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				return nil
			}
			return err
		}
		for _, n := range batch {
			if n.Subject.Type == "RepositoryAdvisory" {
				set.add(n.Repository.FullName, true)
			}
		}
		if len(batch) < 50 {
			return nil
		}
	}
	return nil
}

// involved reads the repositories of every issue and pull request elsewhere
// that involves the account: authored, commented, assigned or mentioned.
func (r Reported) involved(ctx context.Context, c *ghapi.Client, set *candidates) error {
	vars := map[string]any{"query": fmt.Sprintf("involves:%s -user:%s", r.Login, r.Login), "first": 100}
	// Search serves a thousand results of any query, ten pages of a hundred.
	for page := 1; page <= 10; page++ {
		var res struct {
			Search struct {
				PageInfo reportedPageInfo `json:"pageInfo"`
				Nodes    []struct {
					Repository reportedRepository `json:"repository"`
				} `json:"nodes"`
			} `json:"search"`
		}
		if err := c.GraphQL(ctx, reportedSearchQuery, vars, &res); err != nil {
			if n := smallerPage(err, pageSize(vars)); n > 0 {
				vars["first"] = n
				page--
				continue
			}
			return err
		}
		for _, n := range res.Search.Nodes {
			set.add(n.Repository.NameWithOwner, false)
		}
		if !res.Search.PageInfo.HasNextPage || len(res.Search.Nodes) == 0 {
			return nil
		}
		vars["after"] = res.Search.PageInfo.EndCursor
	}
	return nil
}

// contributed reads the repositories GitHub counts the viewer as having
// contributed to, which it keeps for about a year.
func (r Reported) contributed(ctx context.Context, c *ghapi.Client, set *candidates) error {
	vars := map[string]any{"first": 100}
	for page := 1; page <= 20; page++ {
		var res struct {
			Viewer struct {
				Repos reportedConnection `json:"repositoriesContributedTo"`
			} `json:"viewer"`
		}
		if err := c.GraphQL(ctx, reportedContributedQuery, vars, &res); err != nil {
			if n := smallerPage(err, pageSize(vars)); n > 0 {
				vars["first"] = n
				page--
				continue
			}
			return err
		}
		for _, n := range res.Viewer.Repos.Nodes {
			set.add(n.NameWithOwner, false)
		}
		if !res.Viewer.Repos.PageInfo.HasNextPage || len(res.Viewer.Repos.Nodes) == 0 {
			return nil
		}
		vars["after"] = res.Viewer.Repos.PageInfo.EndCursor
	}
	return nil
}

// globalAdvisory is the part of a global database entry that says whom it
// credits and where it came from.
type globalAdvisory struct {
	UpdatedAt             time.Time        `json:"updated_at"`
	RepositoryAdvisoryURL string           `json:"repository_advisory_url"`
	SourceCodeLocation    string           `json:"source_code_location"`
	Credits               []advisoryCredit `json:"credits"`
}

// credited walks the reviewed global database by update, newest first, back
// to Scanned, and adds the repository of every advisory crediting the login.
//
// Reviewed only: of the 360 unreviewed advisories updated in the 24 hours
// before 2026-11-16 none carried a credit, and of the 130 reviewed ones 69
// did. The listing pages by cursor; `page=` is ignored and served page one
// again. done is true when the walk reached Scanned or the end.
func (r Reported) credited(ctx context.Context, c *ghapi.Client, set *candidates) (done bool, err error) {
	path := "/advisories?type=reviewed&sort=updated&direction=desc&per_page=100"
	after := ""
	for page := 1; page <= Unbounded.limit(0); page++ {
		next := path
		if after != "" {
			next += "&after=" + after
		}
		var batch []globalAdvisory
		link, _, getErr := c.GetJSON(ctx, next, &batch, "")
		if getErr != nil {
			// A server with no global database, as a GitHub Enterprise
			// Server may be, has nothing here to read: it is not a failure.
			if isPaginationLimit(getErr) || isSkippable(getErr) {
				return true, nil
			}
			return false, getErr
		}
		for i := range batch {
			a := &batch[i]
			if slices.ContainsFunc(a.Credits, func(cr advisoryCredit) bool { return strings.EqualFold(cr.User.Login, r.Login) }) {
				set.add(advisoryRepository(a), true)
			}
		}
		after = afterCursor(link)
		if after == "" || len(batch) < 100 {
			return true, nil
		}
		if !r.Scanned.IsZero() && batch[len(batch)-1].UpdatedAt.Before(r.Scanned) {
			return true, nil
		}
	}
	return true, nil
}

// advisoryRepository is the repository a global advisory came from: the one
// its repository advisory lives in, or else the GitHub repository its source
// code location names. Anything else names no repository to ask.
func advisoryRepository(a *globalAdvisory) string {
	if _, rest, ok := strings.Cut(a.RepositoryAdvisoryURL, "/repos/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	if rest, ok := strings.CutPrefix(a.SourceCodeLocation, "https://github.com/"); ok {
		parts := strings.Split(strings.TrimSuffix(rest, ".git"), "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	return ""
}

// point renders one advisory of a candidate repository, if it names the
// login as author, in a credit or as a collaborator on the private fork.
func (r Reported) point(a *repoAdvisory, fullName string, now time.Time) (sink.Point, bool) {
	is := func(login string) bool { return strings.EqualFold(login, r.Login) }
	authored := a.Author != nil && is(a.Author.Login)
	var types []string
	state := ""
	for _, cr := range a.Credits {
		if is(cr.User.Login) {
			types = append(types, cr.Type)
			state = cr.State
		}
	}
	collaborator := slices.ContainsFunc(a.Collaborators, func(u advisoryUser) bool { return is(u.Login) })
	if a.GHSAID == "" || (!authored && len(types) == 0 && !collaborator) {
		return sink.Point{}, false
	}
	fields := advisoryFields(a, r.Details, now)
	fields["authored"] = boolInt(authored)
	slices.Sort(types)
	// Written for a collaborator the advisory does not credit too, so the
	// column exists for the panel that names it whatever the account's
	// advisories are.
	fields["credit"] = orNone(strings.Join(types, ","))
	setNonEmpty(fields, "credit_state", state)
	return sink.Point{
		Measurement: "gh_advisory_reported",
		Tags: merge(fullNameTags(advisoryHome(a.HTMLURL, fullName)), map[string]string{
			"ghsa": a.GHSAID, "user": r.Login,
		}),
		Fields: fields,
		Time:   advisoryStamp(a, now),
	}, true
}
