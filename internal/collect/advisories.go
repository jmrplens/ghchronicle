package collect

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Advisories reads the security advisories of one repository: the private
// reports others sent it, and the drafts and publications of its maintainers.
//
// GET /repos/{owner}/{repo}/security-advisories lists every state to an
// administrator, triage and closed reports included, and only the published
// ones to anyone else. It is the one place a report sent to the repository is
// visible at all: no other endpoint, REST or GraphQL, lists them.
type Advisories struct {
	// Walk bounds the list. The default is advisoryPages' own, which reads
	// every page there is in practice: a report still in triage has to be
	// read every day for its daily row, however long ago it last moved.
	Walk Walk
	// Refusals remembers a repository whose list is refused, a fork above
	// all, so the refusal is paid once a day rather than once a sweep.
	Refusals *Refusals
	// Details writes what an unpublished advisory says about the flaw. Off,
	// its row carries the state, the severity, the dates and the page, and
	// nothing that describes the vulnerability: a report under embargo has no
	// business in a store other people may read.
	Details bool
}

// repoAdvisory is one repository security advisory as the listing returns it.
type repoAdvisory struct {
	GHSAID   string `json:"ghsa_id"`
	CVEID    string `json:"cve_id"`
	HTMLURL  string `json:"html_url"`
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
	State    string `json:"state"`
	// Author is null to anyone but the author and the repository's
	// maintainers, measured on 2026-11-16: an unauthenticated read of a
	// published advisory names nobody.
	Author      *advisoryUser `json:"author"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	PublishedAt *time.Time    `json:"published_at"`
	ClosedAt    *time.Time    `json:"closed_at"`
	WithdrawnAt *time.Time    `json:"withdrawn_at"`
	// Submission is set on a report sent through private vulnerability
	// reporting and null on an advisory a maintainer drafted.
	Submission *struct {
		Accepted bool `json:"accepted"`
	} `json:"submission"`
	Vulnerabilities []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
	} `json:"vulnerabilities"`
	// A draft has no score at all: both keys arrive with a null score.
	CVSSSeverities struct {
		V3 struct {
			Score *float64 `json:"score"`
		} `json:"cvss_v3"`
		V4 struct {
			Score *float64 `json:"score"`
		} `json:"cvss_v4"`
	} `json:"cvss_severities"`
	CWEIDs        []string         `json:"cwe_ids"`
	Credits       []advisoryCredit `json:"credits_detailed"`
	Collaborators []advisoryUser   `json:"collaborating_users"`
	Comments      int              `json:"comments"`
}

// advisoryUser is a person an advisory names.
type advisoryUser struct {
	Login string `json:"login"`
}

// advisoryCredit is one credit: who, in what role, and whether they accepted.
type advisoryCredit struct {
	User  advisoryUser `json:"user"`
	Type  string       `json:"type"`
	State string       `json:"state"`
}

// public reports whether the advisory has been published, which is what
// decides whether its description belongs in a row without asking.
func (a *repoAdvisory) public() bool {
	return a.State == "published" || a.State == "withdrawn"
}

// advisoryPages walks one repository's advisory list, newest update first.
//
// It pages by cursor through the Link header, as the Dependabot list does. A
// refusal is "nothing here" for a repository, a fork's above all, and not a
// failure.
func advisoryPages(ctx context.Context, c *ghapi.Client, memory *Refusals, w Walk, fullName string) ([]repoAdvisory, error) {
	path := fmt.Sprintf("/repos/%s/security-advisories?per_page=100&sort=updated&direction=desc", fullName)
	var rows []repoAdvisory
	after := ""
	// Ten pages is a thousand advisories, more than any repository measured
	// holds (getarcaneapp/arcane, 18; opencloud-eu/opencloud, 5), so a sweep
	// reads every one and an advisory still in triage keeps its daily row.
	most := w.limit(10)
	for page := 1; page <= most; page++ {
		next := path
		if after != "" {
			next += "&after=" + after
		}
		var batch []repoAdvisory
		link, _, e := memory.GetJSON(ctx, c, next, &batch, "")
		if e != nil {
			if isSkippable(e) {
				return rows, nil
			}
			return rows, e
		}
		rows = append(rows, batch...)
		after = afterCursor(link)
		if after == "" || len(batch) < 100 {
			break
		}
		if w.past(batch[len(batch)-1].UpdatedAt) {
			break
		}
	}
	return rows, nil
}

// advisoryStamp dates an advisory's row.
//
// A published advisory is dated when it was published, and a withdrawn one
// too, since withdrawing is a later state of the same publication. A report
// closed without publication is dated when it closed. Anything still open is
// a daily snapshot, stamped at the start of the UTC day and rewritten through
// it, so a report in triage for a month leaves a row a day and not one a sweep.
func advisoryStamp(a *repoAdvisory, now time.Time) time.Time {
	switch {
	case a.PublishedAt != nil && (a.State == "published" || a.State == "withdrawn"):
		return *a.PublishedAt
	case a.State == "withdrawn" && a.WithdrawnAt != nil:
		return *a.WithdrawnAt
	case a.State == "closed" && a.ClosedAt != nil:
		return *a.ClosedAt
	}
	return now.UTC().Truncate(24 * time.Hour)
}

// advisoryFields renders what both advisory measurements carry.
//
// Every value is a field: the state, the severity and the scores all change
// after the row's own date, and a tag that changes leaves the stale row behind
// for ever. Whatever describes the flaw (summary, CVE, CWEs, packages) is
// written for a published advisory always and for any other only when the
// operator asked for it.
func advisoryFields(a *repoAdvisory, details bool, now time.Time) map[string]any {
	stamp := advisoryStamp(a, now)
	f := withURL(map[string]any{
		"advisory_state": a.State,
		"comments":       a.Comments,
		"credits":        len(a.Credits),
		"seconds_open":   int(stamp.Sub(a.CreatedAt).Seconds()),
	}, a.HTMLURL)
	setNonEmpty(f, "severity", a.Severity)
	if v := a.CVSSSeverities.V3.Score; v != nil && *v > 0 {
		f["cvss"] = *v
	}
	if v := a.CVSSSeverities.V4.Score; v != nil && *v > 0 {
		f["cvss_v4"] = *v
	}
	if a.PublishedAt != nil {
		f["seconds_to_publish"] = int(a.PublishedAt.Sub(a.CreatedAt).Seconds())
	}
	if a.ClosedAt != nil {
		f["seconds_to_close"] = int(a.ClosedAt.Sub(a.CreatedAt).Seconds())
	}
	if !details && !a.public() {
		return f
	}
	setNonEmpty(f, "summary", a.Summary)
	setNonEmpty(f, "cve", a.CVEID)
	cwes := slices.Clone(a.CWEIDs)
	sort.Strings(cwes)
	setNonEmpty(f, "cwes", strings.Join(cwes, ","))
	var ecosystems []string
	for _, v := range a.Vulnerabilities {
		if e := v.Package.Ecosystem; e != "" && !slices.Contains(ecosystems, e) {
			ecosystems = append(ecosystems, e)
		}
	}
	sort.Strings(ecosystems)
	setNonEmpty(f, "ecosystems", strings.Join(ecosystems, ","))
	if len(a.Vulnerabilities) > 0 {
		f["packages"] = len(a.Vulnerabilities)
	}
	return f
}

// Collect renders a row per advisory of the repository.
func (ad Advisories) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	rows, err := advisoryPages(ctx, c, ad.Refusals, ad.Walk, repo.FullName)
	base := repoTags(repo.Owner, repo.Name)
	points := make([]sink.Point, 0, len(rows))
	for i := range rows {
		a := &rows[i]
		if a.GHSAID == "" {
			continue
		}
		fields := advisoryFields(a, ad.Details, now)
		// Who reported it is said only of a report: an advisory a maintainer
		// drafted names its author, who is that maintainer.
		submitted := a.Submission != nil
		fields["submitted"] = boolInt(submitted)
		if submitted && a.Author != nil {
			setNonEmpty(fields, "reporter", a.Author.Login)
		}
		points = append(points, sink.Point{
			Measurement: "gh_repo_advisory",
			Tags:        merge(base, map[string]string{"ghsa": a.GHSAID}),
			Fields:      fields,
			Time:        advisoryStamp(a, now),
		})
	}
	return points, err
}
