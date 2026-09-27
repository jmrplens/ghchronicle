package collect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Security counts open alerts by severity and state.
//
// Every one of these endpoints answers 403 or 404 when the feature is switched
// off for a repository, which is normal rather than broken: of 52 repos, 45
// have Dependabot alerts off. The collector records that as "not enabled" and
// moves on, because failing the sweep on the first such repo would lose the
// other 51.
//
// The states a repository can be in are told apart from the first full page
// alone, and cost nothing extra. Measured against the live API on 2026-09-08
// over the 18 repositories this account sweeps:
//
//	403 feature off    Dependabot on 11 repos, "Dependabot alerts are
//	                   disabled for this repository"; code scanning on 6,
//	                   "Code scanning is not enabled for this repository"
//	404 nothing to serve  Code scanning only, 5 repos, "no analysis found":
//	                   the feature may well be configured, but it has never
//	                   produced an analysis, so there is no alert list
//	200 empty list     Feature on, nothing to report. Dependabot:
//	                   jmrplens/TFG-TFM_EPS. Code scanning: jmrplens/jmrplens,
//	                   NOT TFG-TFM_EPS, which answers 404 there
//	200 with rows      Feature on, alerts to read
//
// The 404 and the 403 are both recorded as not enabled, which is what
// `enabled` means here: not "somebody ticked a box" but "this repository will
// hand over this kind of alert". An earlier version repeated the call with
// per_page=1 to draw that same line; it fired whenever the list came back
// empty, which is 12 repositories for Dependabot and 12 for code scanning,
// 24 requests a sweep, and it learnt nothing the first response had not
// already said.
//
// Whether Dependabot also opens pull requests to fix an alert is a different
// switch, in the `security_and_analysis` block of GET /repos/{owner}/{repo},
// which belongs to RepoCore. Measured the same day: that block is null on all
// 8 private repositories of this account and present on the public ones, so
// its absence says "private", not "switched off" (jmrplens/kleidos is private,
// has the block null, and GET /automated-security-fixes answers enabled:true).
type Security struct {
	// Walk bounds the alert lists. Default one page each; a backfill walks
	// them all, which is what makes time-to-resolve computable on old alerts.
	// It bounds the rows and not the open counts: a walk that stops short of
	// the end has those read from the open alerts alone (see openAlerts).
	Walk Walk
	// Refusals remembers the repositories where a list is switched off, so
	// the 403 is paid once a day rather than once an hour. Nil asks every time.
	Refusals *Refusals
}

type dependabotRow struct {
	HTMLURL     string     `json:"html_url"`
	Number      int        `json:"number"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	FixedAt     *time.Time `json:"fixed_at"`
	DismissedAt *time.Time `json:"dismissed_at"`
	// AutoDismissedAt is the third way an alert closes, and the one that used
	// to be missed: GitHub dismisses a development-dependency alert by itself
	// and leaves both fixed_at and dismissed_at null, so an alert in state
	// auto_dismissed was recorded as still open, forever.
	AutoDismissedAt *time.Time `json:"auto_dismissed_at"`
	// Why a person closed it without fixing it, which is the part of a
	// dismissal a reader wants to audit later. All three are null unless the
	// state is dismissed.
	DismissedReason  string `json:"dismissed_reason"`
	DismissedComment string `json:"dismissed_comment"`
	DismissedBy      *struct {
		Login string `json:"login"`
	} `json:"dismissed_by"`
	SecurityAdvisory      securityAdvisory      `json:"security_advisory"`
	SecurityVulnerability securityVulnerability `json:"security_vulnerability"`
	Dependency            alertDependency       `json:"dependency"`
}

// securityAdvisory is the published advisory a Dependabot alert is raised
// from: what the flaw is, and the three scores that rank how much it matters.
type securityAdvisory struct {
	GHSAID   string `json:"ghsa_id"`
	CVEID    string `json:"cve_id"`
	Severity string `json:"severity"`
	// Summary is the advisory's title, the one line that says what the
	// alert is about. Without it a row says "vite" and a GHSA id.
	Summary string `json:"summary"`
	// PublishedAt is when the advisory became public. The gap to
	// created_at is how long GitHub took to notice it here, which nothing
	// else in the response says.
	PublishedAt time.Time `json:"published_at"`
	CVSS        cvssScore `json:"cvss"`
	// The top-level cvss is v3; v4 is only ever in cvss_severities.
	// GitHub always sends the key: an advisory with no v4 vector comes
	// as {"vector_string": null, "score": 0.0}, measured on 84 of the 138
	// alerts of jmrp.io, which is why the field below is written only
	// above zero rather than only when the key is there.
	CVSSSeverities struct {
		V4 cvssScore `json:"cvss_v4"`
	} `json:"cvss_severities"`
	// EPSS is the modeled probability of exploitation in the next thirty
	// days. It is the one number here about the outside world rather than
	// about this repository.
	EPSS struct {
		Percentage float64 `json:"percentage"`
		Percentile float64 `json:"percentile"`
	} `json:"epss"`
	CWEs []struct {
		ID string `json:"cwe_id"`
	} `json:"cwes"`
}

// cvssScore is one CVSS vector reduced to its score, which is the only part of
// it a row carries. Both versions of the vector come in this shape.
type cvssScore struct {
	Score float64 `json:"score"`
}

// securityVulnerability is the advisory narrowed to this dependency: which
// versions are affected, and which one ends it.
type securityVulnerability struct {
	// The range, next to first_patched, is the action to take.
	VulnerableVersionRange string `json:"vulnerable_version_range"`
	FirstPatched           *struct {
		Identifier string `json:"identifier"`
	} `json:"first_patched_version"`
}

// alertDependency is the package the alert is against, and how this repository
// came to have it.
type alertDependency struct {
	Package struct {
		Ecosystem string `json:"ecosystem"`
		Name      string `json:"name"`
	} `json:"package"`
	// A development dependency is not the same risk as a runtime one, and
	// a transitive alert cannot be fixed by editing your own manifest.
	ManifestPath string `json:"manifest_path"`
	Scope        string `json:"scope"`
	Relationship string `json:"relationship"`
}

type scanRow struct {
	Number      int        `json:"number"`
	HTMLURL     string     `json:"html_url"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	FixedAt     *time.Time `json:"fixed_at"`
	DismissedAt *time.Time `json:"dismissed_at"`
	// DismissedReason says whether a closed alert was fixed or waved through,
	// which is the difference between security work and paperwork.
	DismissedReason string `json:"dismissed_reason"`
	Rule            struct {
		ID               string `json:"id"`
		Severity         string `json:"severity"`
		SecuritySeverity string `json:"security_severity_level"`
		// Tags carry the CWE classification among housekeeping labels, as
		// "external/cwe/cwe-079".
		Tags []string `json:"tags"`
	} `json:"rule"`
	Tool struct {
		Name string `json:"name"`
	} `json:"tool"`
	// MostRecentInstance is where the alert actually is. The listing already
	// carries it and this collector used to throw it away, so the file, the
	// line and the commit cost no request at all. Measured on jmrp.io: 24 of
	// 34 alerts point at one file, which is invisible without this.
	MostRecentInstance scanInstance `json:"most_recent_instance"`
}

// scanInstance is where a code scanning alert actually is: the commit it was
// last seen on, and the file and line inside it.
type scanInstance struct {
	Ref       string `json:"ref"`
	CommitSHA string `json:"commit_sha"`
	Category  string `json:"category"`
	Location  struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
	} `json:"location"`
}

// alertCWEs renders every CWE an advisory names into one field value.
// Advisories name more than one often enough that keeping only the first
// would lie.
func alertCWEs(ids []string) string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return strings.Join(out, ",")
}

func (sc Security) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	points, err := sc.dependabot(ctx, c, repo, base, now)
	if err != nil {
		return points, err
	}
	scanning, err := sc.codeScanning(ctx, c, repo, base, now)
	// Dependabot answered and code scanning did not: the rows above are as
	// true as they were, so they travel with the error rather than being
	// thrown away with it.
	return append(points, scanning...), err
}

// dependabot renders the Dependabot alert list: a row per alert the walk
// read, the open counts, and the feature.
//
// The rows are every state, not only the open ones: an alert that was fixed is
// the only evidence of how long it took, and asking for state=open alone threw
// that away. `alerts` is the rows read and nothing more, so on a sweep of a
// repository past a hundred alerts it reads 100, meaning a hundred or more.
// The list pages by cursor and declares no last page, so unlike code scanning
// there is no one request that says its length, and the whole walk to recount
// alerts fixed years ago is what a backfill is for.
func (sc Security) dependabot(ctx context.Context, c *ghapi.Client, repo Repo,
	base map[string]string, now time.Time,
) ([]sink.Point, error) {
	path := fmt.Sprintf("/repos/%s/dependabot/alerts?per_page=100", repo.FullName)
	rows, enabled, more, err := dependabotPages(ctx, c, sc.Refusals, sc.Walk, path)
	if err != nil {
		return nil, err
	}
	points := dependabotItems(rows, base)
	open, err := openAlerts(ctx, c, path, rows, more, dependabotPages)
	if err != nil {
		// The rows are as true as they were; it is the count that could not
		// be completed, and a count from the page would be the one this read
		// exists to correct.
		return points, err
	}
	counts, n := dependabotCounts(open, base, repo, now)
	points = append(points, counts...)
	return append(points, securityFeaturePoint(base, "dependabot",
		githubPage(repo.FullName, "security"), enabled, n, len(rows), now)), nil
}

// codeScanning renders the code scanning alert list: a row per alert the walk
// read, the open counts, and the feature with the list's own total.
//
// Every state, for the same reason as Dependabot. `state=all` is not a valid
// value here (it answers 400), so the parameter is left off, which is the
// endpoint's own way of saying all of them.
func (sc Security) codeScanning(ctx context.Context, c *ghapi.Client, repo Repo,
	base map[string]string, now time.Time,
) ([]sink.Point, error) {
	path := fmt.Sprintf("/repos/%s/code-scanning/alerts?per_page=100", repo.FullName)
	rows, enabled, more, err := codeScanningPages(ctx, c, sc.Refusals, sc.Walk, path)
	if err != nil {
		return nil, err
	}
	points := codeScanningItems(rows, base)
	open, err := openAlerts(ctx, c, path, rows, more, codeScanningPages)
	if err != nil {
		return points, err
	}
	total := len(rows)
	if more {
		if total, err = codeScanningTotal(ctx, c, repo, total); err != nil {
			return points, err
		}
	}
	counts, n := codeScanningCounts(open, base, repo, now)
	points = append(points, counts...)
	return append(points, securityFeaturePoint(base, "code_scanning",
		githubPage(repo.FullName, "security", "code-scanning"), enabled, n, total, now)), nil
}

// securityFeaturePoint records whether the repository hands this kind of alert
// over at all, and how many there are.
//
// A zero is a fact worth recording; absence of a point is not the same as "no
// alerts", and a chart needs the difference.
func securityFeaturePoint(base map[string]string, feature, url string,
	enabled bool, open, total int, now time.Time,
) sink.Point {
	return sink.Point{
		Measurement: "gh_security_feature",
		Tags:        merge(base, map[string]string{"feature": feature}),
		Fields: withURL(map[string]any{
			"enabled": enabled, "open_alerts": open, "alerts": total,
		}, url),
		Time: now,
	}
}

// alertPages walks one alert list from path, newest first. answered is
// whether the repository hands the list over at all, and more whether the
// walk stopped with alerts still behind it.
type alertPages[T any] func(ctx context.Context, c *ghapi.Client, memory *Refusals, w Walk, path string) (rows []T, answered, more bool, err error)

// openAlerts is the list the open counts are taken from.
//
// When the walk read the whole list that is the rows it read. When it stopped
// with more behind it, which is every sweep of a repository past a hundred
// alerts, those rows are the newest hundred in every state, and an alert still
// open but older than them, a long-standing one behind a busy history of fixed
// ones, was simply not counted. So the list is read again with state=open,
// walked to its end: it is short where it matters, usually empty (measured on
// 2026-09-26, the two lists of this account past a hundred alerts answer an
// empty page), and asked conditionally like every other page, so a list that
// did not change is a free 304.
//
// The request goes past the refusal memory on purpose. The unfiltered list has
// just answered, so the feature is on, and a refusal here is about this walk,
// not the repository: it neither turns the feature off nor is remembered for a
// day. The counts fall back to the rows read, which is what they were before.
// A refusal deeper in the open walk keeps the pages it read, which are the
// newest open alerts and so already hold every open one of those rows.
func openAlerts[T any](ctx context.Context, c *ghapi.Client, path string, read []T, more bool, walk alertPages[T]) ([]T, error) {
	if !more {
		return read, nil
	}
	open, answered, _, err := walk(ctx, c, nil, Unbounded, path+"&state=open")
	if err != nil || !answered {
		return read, err
	}
	return open, nil
}

// codeScanningTotal is how many alerts the code scanning list holds, for a
// walk that stopped before its end; read is what it read, and stands when the
// list will not say.
//
// The list pages by number, so a page of one alert declares the total as its
// last page. Measured on 2026-09-26 on jmrplens/Cloudflare-DNS-Updater:
// per_page=1 answers page=1393 under rel="last", the 1,393 distinct alerts a
// backfill read, where a sweep had been writing 100. GitHub's 304 carries no
// Link at all, and the client replays the one its 200 came with, so a total
// that did not move is read for nothing.
func codeScanningTotal(ctx context.Context, c *ghapi.Client, repo Repo, read int) (int, error) {
	var one []scanRow
	link, _, err := c.GetJSON(ctx, fmt.Sprintf("/repos/%s/code-scanning/alerts?per_page=1", repo.FullName), &one, "")
	if err != nil {
		if isSkippable(err) {
			// Past the refusal memory for the reason openAlerts gives.
			return read, nil
		}
		return read, err
	}
	if m := lastPageRe.FindStringSubmatch(link); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil {
			return n, nil
		}
	}
	return read, nil
}

// dependabotPages walks a Dependabot alert list.
//
// The list refuses `page=` outright ("Pagination using the page parameter is
// not supported") and pages by cursor through the Link header, unlike every
// other alert endpoint, so it ends where the cursor does.
func dependabotPages(ctx context.Context, c *ghapi.Client, memory *Refusals, w Walk, path string) (rows []dependabotRow, answered, more bool, err error) {
	after := ""
	most := w.limit(1)
	for page := 1; page <= most; page++ {
		next := path
		if after != "" {
			next += "&after=" + after
		}
		var batch []dependabotRow
		link, _, e := memory.GetJSON(ctx, c, next, &batch, "")
		if e != nil {
			if isSkippable(e) {
				// Only the first page can mean "the feature is off". A refusal
				// deeper in the walk is about the walk, not the repository,
				// and the pages already read stay.
				return rows, page > 1, page > 1, nil
			}
			if isPaginationLimit(e) {
				return rows, true, false, nil
			}
			return nil, false, false, e
		}
		rows = append(rows, batch...)
		after = afterCursor(link)
		if after == "" || len(batch) < 100 {
			return rows, true, false, nil
		}
		if w.past(batch[len(batch)-1].CreatedAt) {
			break
		}
	}
	return rows, true, true, nil
}

// codeScanningPages walks a code scanning alert list.
//
// It is written out rather than handed to pages() because pages() folds a
// switched-off feature into an empty result, and that is exactly the
// distinction wanted here.
func codeScanningPages(ctx context.Context, c *ghapi.Client, memory *Refusals, w Walk, path string) (rows []scanRow, answered, more bool, err error) {
	most := w.limit(1)
	for page := 1; page <= most; page++ {
		var batch []scanRow
		if _, _, e := memory.GetJSON(ctx, c, fmt.Sprintf("%s&page=%d", path, page), &batch, ""); e != nil {
			if isSkippable(e) {
				return rows, page > 1, page > 1, nil
			}
			if isPaginationLimit(e) {
				return rows, true, false, nil
			}
			return nil, false, false, e
		}
		rows = append(rows, batch...)
		if len(batch) < 100 {
			return rows, true, false, nil
		}
		// The list is newest first, so a backfill bounded by date stops here
		// the way the Dependabot walk does, rather than paying for every page
		// back to the first scan and discarding what lies past the bound.
		if w.past(batch[len(batch)-1].CreatedAt) {
			break
		}
	}
	return rows, true, true, nil
}

// dependabotItems renders one point per alert, in whatever state.
func dependabotItems(rows []dependabotRow, base map[string]string) []sink.Point {
	points := make([]sink.Point, 0, len(rows))
	for i := range rows {
		a := &rows[i]
		fields := dependabotAlertFields(a)
		// The state is a field and not a tag: the row is dated when the
		// alert was raised and the state moves later, so as a tag an alert
		// that was open and is now fixed was two rows at the same instant for
		// ever, and every count of alerts drifted upwards. Under a new name so
		// a database that already holds the tag column keeps accepting
		// writes; the exporter reads it back as a label, where a gauge is
		// stamped at the sweep and nothing doubles.
		fields["alert_state"] = a.State
		points = append(points, sink.Point{
			Measurement: "gh_dependabot_alert_item",
			Tags: merge(base, map[string]string{
				"number":    strconv.Itoa(a.Number),
				"severity":  a.SecurityAdvisory.Severity,
				"ecosystem": a.Dependency.Package.Ecosystem,
				"package":   a.Dependency.Package.Name,
				// The series is already one per alert, so these group
				// existing rows rather than multiplying them.
				"ghsa":         orNone(a.SecurityAdvisory.GHSAID),
				"scope":        orNone(a.Dependency.Scope),
				"relationship": orNone(a.Dependency.Relationship),
				"manifest":     orNone(a.Dependency.ManifestPath),
			}),
			Fields: fields,
			Time:   a.CreatedAt,
		})
	}
	return points
}

// dependabotCounts renders the open alerts by severity and ecosystem, and
// returns how many are open. rows is either list openAlerts hands over, and
// only the open ones count in both.
func dependabotCounts(rows []dependabotRow, base map[string]string, repo Repo, now time.Time) (points []sink.Point, open int) {
	counts := map[[2]string]int{}
	for i := range rows {
		a := &rows[i]
		if a.State == "open" {
			counts[[2]string{a.SecurityAdvisory.Severity, a.Dependency.Package.Ecosystem}]++
			open++
		}
	}
	for k, n := range counts {
		points = append(points, sink.Point{
			Measurement: "gh_dependabot_alert",
			Tags:        merge(base, map[string]string{"severity": k[0], "ecosystem": k[1]}),
			Fields: withURL(map[string]any{"open": n},
				githubPage(repo.FullName, "security", "dependabot")),
			Time: now,
		})
	}
	return points, open
}

// dependabotAlertFields renders what one alert says about itself, as opposed
// to how it is grouped.
//
// The point is dated when the alert was raised and carries how long it stayed.
// created_at never moves, so a re-read rewrites the same row rather than
// adding one every sweep.
func dependabotAlertFields(a *dependabotRow) map[string]any {
	f := map[string]any{"alerts": 1, "url": a.HTMLURL}
	// Same guard for both scores. GitHub sends {"score": 0.0} for a vector
	// it does not have, and it has no v3 vector for an advisory published
	// with v4 only: measured on jmrp.io, 78 of 225 alerts carried cvss 0.0
	// beside a real cvss_v4, and "worst CVSS" over a severity group of those
	// read 0. A score of exactly zero is not a score the scale gives out.
	if v := a.SecurityAdvisory.CVSS.Score; v > 0 {
		f["cvss"] = v
	}
	if v := a.SecurityAdvisory.CVSSSeverities.V4.Score; v > 0 {
		f["cvss_v4"] = v
	}
	if v := a.SecurityAdvisory.EPSS.Percentage; v > 0 {
		f["epss"] = v
	}
	// The percentile is the one people read: "more likely to be exploited
	// than 20 per cent of known vulnerabilities" says more than 0.00278.
	if v := a.SecurityAdvisory.EPSS.Percentile; v > 0 {
		f["epss_percentile"] = v
	}
	if a.SecurityAdvisory.CVEID != "" {
		f["cve"] = a.SecurityAdvisory.CVEID
	}
	setNonEmpty(f, "summary", a.SecurityAdvisory.Summary)
	setNonEmpty(f, "vulnerable_range", a.SecurityVulnerability.VulnerableVersionRange)
	setNonEmpty(f, "dismissed_reason", a.DismissedReason)
	setNonEmpty(f, "dismissed_comment", a.DismissedComment)
	if a.DismissedBy != nil {
		setNonEmpty(f, "dismissed_by", a.DismissedBy.Login)
	}
	ids := make([]string, 0, len(a.SecurityAdvisory.CWEs))
	for _, cwe := range a.SecurityAdvisory.CWEs {
		ids = append(ids, cwe.ID)
	}
	if cwe := alertCWEs(ids); cwe != "" {
		f["cwe"] = cwe
	}
	if a.SecurityVulnerability.FirstPatched != nil {
		f["first_patched"] = a.SecurityVulnerability.FirstPatched.Identifier
	}
	if !a.SecurityAdvisory.PublishedAt.IsZero() {
		// How long GitHub took to raise this here after the advisory went
		// public. Negative when the alert came first, which happens when an
		// advisory is published after the fact.
		f["seconds_to_detect"] = int(a.CreatedAt.Sub(a.SecurityAdvisory.PublishedAt).Seconds())
	}
	// Three ways to close, not two. auto_dismissed_at is how GitHub closes a
	// development-dependency alert on its own, and it leaves the other two
	// null.
	var closed *time.Time
	switch {
	case a.FixedAt != nil:
		closed = a.FixedAt
	case a.DismissedAt != nil:
		closed = a.DismissedAt
	case a.AutoDismissedAt != nil:
		closed = a.AutoDismissedAt
	}
	addTimeToResolve(f, closed, a.CreatedAt)
	return f
}

// codeScanningItems renders one point per alert, in whatever state.
func codeScanningItems(rows []scanRow, base map[string]string) []sink.Point {
	points := make([]sink.Point, 0, len(rows))
	for i := range rows {
		a := &rows[i]
		points = append(points, sink.Point{
			Measurement: "gh_code_scanning_alert_item",
			Tags:        scanAlertTags(a, base, scanSeverity(a)),
			Fields:      scanAlertFields(a),
			Time:        a.CreatedAt,
		})
	}
	return points
}

// codeScanningCounts renders the open alerts by severity and tool, and
// returns how many are open, from either list openAlerts hands over.
func codeScanningCounts(rows []scanRow, base map[string]string, repo Repo, now time.Time) (points []sink.Point, open int) {
	counts := map[[2]string]int{}
	for i := range rows {
		if a := &rows[i]; a.State == "open" {
			counts[[2]string{scanSeverity(a), a.Tool.Name}]++
			open++
		}
	}
	for k, n := range counts {
		points = append(points, sink.Point{
			Measurement: "gh_code_scanning_alert",
			Tags:        merge(base, map[string]string{"severity": k[0], "tool": k[1]}),
			Fields: withURL(map[string]any{"open": n},
				githubPage(repo.FullName, "security", "code-scanning")),
			Time: now,
		})
	}
	return points, open
}

// scanSeverity is what an alert is grouped by, in its row and in the open
// counts alike, so an alert is never one severity in one and another in the
// other: the security severity, or the rule's own when there is none.
func scanSeverity(a *scanRow) string {
	if a.Rule.SecuritySeverity != "" {
		return a.Rule.SecuritySeverity
	}
	return a.Rule.Severity
}

// scanAlertTags names one alert and says where it is. The state and the
// reason it closed are fields, set by scanAlertFields, for the reason given
// on the Dependabot row: both move after the date the row carries.
func scanAlertTags(a *scanRow, base map[string]string, sev string) map[string]string {
	return merge(base, map[string]string{
		"number":   strconv.Itoa(a.Number),
		"severity": sev, "tool": a.Tool.Name, "rule": a.Rule.ID,
		// Where the alert is, which is what turns a count into something
		// actionable.
		"path":     orNone(a.MostRecentInstance.Location.Path),
		"category": orNone(a.MostRecentInstance.Category),
		"ref":      orNone(shortRef(a.MostRecentInstance.Ref)),
	})
}

// resolutionOf is why the alert closed, when it closed: "fixed" is GitHub's
// silence and a dismissal always names its reason. An open alert has neither
// and still gets the value, so that the column exists from the first row and
// a query naming it never fails on a database where no alert has closed yet.
func resolutionOf(a *scanRow) string {
	switch {
	case a.DismissedReason != "":
		return a.DismissedReason
	case a.FixedAt != nil:
		return "fixed"
	}
	return "open"
}

// scanAlertFields renders what one alert says about itself.
//
// The list was already being downloaded whole and these fields thrown away,
// so this costs no call at all. line is GitHub's own number, zero included: an
// alert about a whole file rather than about a line is served with
// "start_line": 0, measured against a real repository, so a zero here is the
// API's answer and not a reading this code failed to take.
func scanAlertFields(a *scanRow) map[string]any {
	f := map[string]any{
		"alerts": 1, "url": a.HTMLURL,
		"commit": a.MostRecentInstance.CommitSHA,
		"line":   a.MostRecentInstance.Location.StartLine,
		// Both were tags, and both change after the date the row carries,
		// which as tags doubled the row: see the Dependabot item.
		"alert_state": a.State,
		"resolution":  resolutionOf(a),
	}
	var cwes []string
	for _, tag := range a.Rule.Tags {
		// The rule's tags mix the classification with housekeeping labels
		// ("security", "maintainability"); only the CWEs say what class of
		// defect this is.
		if id, found := strings.CutPrefix(tag, "external/cwe/"); found {
			cwes = append(cwes, id)
		}
	}
	if cwe := alertCWEs(cwes); cwe != "" {
		f["cwe"] = cwe
	}
	closed := a.FixedAt
	if closed == nil {
		closed = a.DismissedAt
	}
	addTimeToResolve(f, closed, a.CreatedAt)
	return f
}

// addTimeToResolve writes how long the alert took to close. An alert still
// open gets no field at all: the row is dated when the alert was raised, so
// how long it has been open is now() less the row's own timestamp, and a
// panel computes that when it is asked rather than reading what a sweep
// believed.
//
// It used to write that figure here, as seconds_open. Two things were wrong
// with it. The value was only true at the instant of the sweep that wrote it,
// yet it sat on a row dated months earlier, so a reader querying last week
// got whatever the last sweep decided rather than what was true then. And
// every sweep changed the value, so every sweep rewrote the row, and InfluxDB
// 3 files a row by its own timestamp and never compacts: measured on
// 2026-09-17, gh_dependabot_alert_item held 1,059 parquet files for 1,419
// rows and gh_code_scanning_alert_item 673 for 10,670, about 234 files a day
// between them, growing with the clock whether or not GitHub had anything new
// to say.
func addTimeToResolve(f map[string]any, closed *time.Time, created time.Time) {
	if closed != nil {
		f["seconds_to_resolve"] = int(closed.Sub(created).Seconds())
	}
}

// Analyses collects the code scanning analyses themselves, not just the alerts.
//
// An alert says what is wrong now. An analysis says the scan ran, when, on
// which commit, with which version of the tool, and how many results it found.
// That is what answers "did the scan actually run on that release", and GitHub
// prunes them, so they have to be captured while they are there.
type Analyses struct {
	Walk Walk
	// Refusals remembers where code scanning is off, as in Security.
	Refusals *Refusals
}

func (a Analyses) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point
	most := a.Walk.limit(1)
	for page := 1; page <= most; page++ {
		var batch []struct {
			ID           int64     `json:"id"`
			Ref          string    `json:"ref"`
			CommitSHA    string    `json:"commit_sha"`
			CreatedAt    time.Time `json:"created_at"`
			ResultsCount int       `json:"results_count"`
			RulesCount   int       `json:"rules_count"`
			Category     string    `json:"category"`
			Environment  string    `json:"environment"`
			Tool         struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"tool"`
		}
		path := fmt.Sprintf("/repos/%s/code-scanning/analyses?per_page=100&page=%d", repo.FullName, page)
		if _, _, err := a.Refusals.GetJSON(ctx, c, path, &batch, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return points, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			an := &batch[i]
			points = append(points, sink.Point{
				Measurement: "gh_code_scanning_analysis",
				Tags: merge(base, map[string]string{
					"tool": an.Tool.Name, "version": an.Tool.Version,
					"ref": shortRef(an.Ref), "category": an.Category,
				}),
				Fields: map[string]any{
					"analyses": 1, "results": an.ResultsCount, "rules": an.RulesCount,
					"commit": an.CommitSHA,
				},
				Time: an.CreatedAt,
			})
		}
		if len(batch) < 100 || a.Walk.past(batch[len(batch)-1].CreatedAt) {
			break
		}
	}
	return points, nil
}
