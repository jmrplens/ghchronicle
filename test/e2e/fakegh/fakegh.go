// Package fakegh is the fake GitHub both end-to-end suites collect from.
//
// It serves the fixtures under test/e2e/testdata on the paths the collectors
// ask for, for one account (octocat) with one repository (hello-world). Every
// fixture is a real response shape; the only liberty is the tokens below,
// every one of them a date counted from the fake's own clock.
//
//   - "@NOW@" becomes the current time, so a family that only looks at the last
//     few minutes still finds something: job logs, for one, are fetched for runs
//     updated within twice their own cadence.
//   - "@TODAY@" becomes the start of the current UTC day, for a point whose date
//     is part of its identity, so that two sweeps of one test read the same date
//     rather than two timestamps seconds apart.
//   - "@SOON@" and "@SOON_EPOCH@" become an hour from now, as RFC 3339 and as
//     Unix seconds. A rate limit window that has already closed is not a window,
//     and a fixed one would publish a reset years in the past and a negative
//     countdown with it.
//   - "@DAYS_AGO_n@" and "@DAYS_AHEAD_n@" become the date n days either side of
//     today, so a fixture stays the same distance from the present; see
//     daysAgoMarker.
//   - "@WEEK_EPOCH_n@" becomes the Unix second of Sunday 00:00 UTC n weeks
//     before the current week's, which is how the daily star history labels
//     its weeks; see weekEpochMarker.
//
// It prices what it answers the way api.github.com does, so a suite can say
// what a sweep cost and hold the number: every REST fixture carries one ETag,
// a request that presents it is answered 304 and charged nothing, every other
// answer on the API charges its bucket one, the object storage a job log
// redirects to charges nothing and carries no headers at all, the
// x-ratelimit-* headers carry the running totals, and every GraphQL answer
// that asked for the budget block gets one.
//
// It is a package rather than a helper inside one suite because it used to be
// two. The original lived in a _test.go file of package e2e, which no other
// package can import, so test/e2e/docker carried a second copy of the route
// table; the audit's first group added three GraphQL fragments and four REST
// paths to the original and the copy never learned them, and every family
// behind those routes collected nothing in the containerized suite while
// passing in the other. One route table, imported twice, is what stops that
// happening again.
package fakegh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// The account the fixtures describe, and its one repository. Login is what a
// config points targets.user at; repoPath is only ever a prefix in the table
// below.
const (
	Login    = "octocat"
	repoPath = "/repos/octocat/hello-world"
)

// The two answers that are not a fixture. emptyPage is a page past the first:
// an empty JSON array, which is how the real API ends a paginated walk.
// graphQLUnknown is a query no marker matches, which GraphQL reports as a
// two-hundred carrying an errors array rather than as a status.
const (
	emptyPage      = "@empty"
	graphQLUnknown = "@graphql-unknown"
)

// graphQLNoFixture is the body graphQLUnknown writes.
const graphQLNoFixture = `{"errors":[{"type":"UNKNOWN","message":"no fixture for this query"}]}`

// rest maps a request path to the fixture that answers it.
var rest = map[string]string{
	"/user/repos":                                   "user_repos.json",
	repoPath:                                        "repo.json",
	repoPath + "/traffic/views":                     "traffic_views.json",
	repoPath + "/traffic/clones":                    "traffic_clones.json",
	repoPath + "/traffic/popular/referrers":         "traffic_referrers.json",
	repoPath + "/traffic/popular/paths":             "traffic_paths.json",
	repoPath + "/languages":                         "repo_languages.json",
	repoPath + "/community/profile":                 "repo_community.json",
	repoPath + "/topics":                            "repo_topics.json",
	repoPath + "/releases":                          "releases.json",
	repoPath + "/stargazers":                        "stargazers_page1.json",
	repoPath + "/stargazers/history":                "stargazers_history.json",
	repoPath + "/actions/runs/1000163135/jobs":      "actions_jobs.json",
	repoPath + "/actions/runs/1000163134/jobs":      "actions_jobs_failed.json",
	repoPath + "/actions/cache/usage":               "actions_cache.json",
	repoPath + "/actions/artifacts":                 "artifacts.json",
	repoPath + "/actions/workflows":                 "workflows.json",
	repoPath + "/dependabot/alerts":                 "dependabot_alerts.json",
	repoPath + "/code-scanning/alerts":              "code_scanning_alerts.json",
	repoPath + "/code-scanning/analyses":            "code_scanning_analyses.json",
	repoPath + "/stats/participation":               "stats_participation.json",
	repoPath + "/stats/punch_card":                  "stats_punch_card.json",
	repoPath + "/activity":                          "repo_activity.json",
	repoPath + "/forks":                             "forks.json",
	repoPath + "/hooks":                             "hooks.json",
	repoPath + "/hooks/12345678/deliveries":         "hook_deliveries.json",
	repoPath + "/rulesets":                          "rulesets.json",
	repoPath + "/rulesets/21/history":               "ruleset_history.json",
	repoPath + "/rulesets/22/history":               "ruleset_history_single.json",
	repoPath + "/actions/permissions/workflow":      "actions_permissions_workflow.json",
	repoPath + "/actions/secrets":                   "actions_secrets.json",
	repoPath + "/dependabot/secrets":                "dependabot_secrets.json",
	repoPath + "/code-scanning/default-setup":       "code_scanning_setup.json",
	repoPath + "/environments":                      "environments.json",
	repoPath + "/keys":                              "deploy_keys.json",
	"/users/octocat/events":                         "events.json",
	"/notifications":                                "notifications.json",
	"/users/octocat/settings/billing/usage":         "billing_usage.json",
	"/user/packages/container/ghchronicle/versions": "package_versions.json",
	"/gists":                         "gists.json",
	"/users/octocat/social_accounts": "social_accounts.json",
	"/user/starred":                  "user_starred.json",
	"/search/issues":                 "search_issues.json",
	"/search/commits":                "search_commits.json",
	"/storage/2000000011.txt":        "job_log.txt",
	// The profile page beside the API: the one family no API answers reads
	// its badges off it. Off every bucket, like storage.
	profilePage: "achievements_page.html",
}

// profilePage is the account's public profile, which the achievements family
// reads with ?tab=achievements and no token.
const profilePage = "/" + Login

// jobLogPath is the one request the real API answers with a redirect to object
// storage, which the collector following it is part of what the suites prove.
const jobLogPath = repoPath + "/actions/jobs/2000000011/logs"

// graphQL picks a fixture from the query text, first marker wins.
//
// Order matters: the history query also mentions the calendar, the creation
// probe is a subset of the account query, and the totals fragment mentions
// pullRequests( as well.
//
// The three fragment names below are matched first because a fragment name
// cannot appear in any other query, so they can neither be shadowed nor shadow
// anything.
var graphQL = []struct{ marker, fixture string }{
	{"fragment branchInventory on Repository", "graphql_branches.json"},
	{"fragment deploypage on DeploymentConnection", "graphql_deployments.json"},
	{"fragment policyfiles on Repository", "graphql_policy_files.json"},
	{"fragment audience on Repository", "graphql_audience.json"},
	{"repositoryDiscussionComments", "graphql_discussion_comments.json"},
	// The two queries of the achievements family are named, so the name is
	// the marker: the counts also search ISSUE and the walk also pages a
	// search, and neither may fall through to those.
	{"query achievementCounts(", "graphql_achievement_counts.json"},
	{"query coauthoredPulls(", "graphql_coauthored_pulls.json"},
	{"issueComments(", "graphql_issue_comments.json"},
	// The account query mentions starredRepositories too, as a count; the
	// argument list is what only the starred walk carries. Likewise the
	// totals counters search ISSUE as well, with no page: the page size is
	// what only an outbound search carries.
	{"starredRepositories(first:", "graphql_starred.json"},
	{"search(type: ISSUE, first: 100", "graphql_search_issues.json"},
	{"search(type: REPOSITORY", "graphql_search_counts.json"},
	{"fragment totals on Repository", "graphql_repo_totals.json"},
	{"fragment detail on Repository", "graphql_repo_detail.json"},
	{"contributionsCollection(from:", "graphql_history.json"},
	{"user(login: $login) { createdAt } }", "graphql_created_at.json"},
	{"contributionCalendar", "graphql_account.json"},
	{"pullRequests(", "graphql_pulls.json"},
	{"discussions(", "graphql_discussions.json"},
	{"history(", "graphql_commits_page1.json"},
	{"milestones(", "graphql_planning.json"},
}

// Request is one call the fake was asked to answer, and what the answer cost.
type Request struct {
	Method, Path, Query, Auth string
	// Status is what was answered. 304 is the one that costs nothing: the
	// request carried the validator of the fixture it would have been served,
	// which is how a second sweep of the same account is mostly free.
	Status int
	// Cost is what the answer charged the bucket it came from, priced as
	// GitHub prices them: one for an answer that carried a body, nothing for
	// a 304 and nothing for the budget probe.
	Cost int
	// GraphQL is the query text when the call was one, so a test can tell
	// the batches of one family from another: every one of them is a POST
	// to the same path.
	GraphQL string
}

// Server is the running fake.
type Server struct {
	tb  testing.TB
	srv *httptest.Server
	// bodies is the fixture directory, read once at start-up and keyed by file
	// name. Nothing here opens a file named by a request: the names come from
	// the directory, and a request only ever looks one up.
	bodies map[string][]byte
	// perRepo answers for repositories other than hello-world; New sets it
	// only when it is given an overlay.
	perRepo bool
	// now is what the fixtures' relative dates are resolved against. It is the
	// real clock unless a suite froze it; see FreezeAt.
	now func() time.Time

	mu       sync.Mutex
	requests []Request
	// failing is the paths a test has asked the fake to break, and the status
	// each answers with.
	failing map[string]int
	// used is what each bucket has been charged since the fake started. It is
	// what the x-ratelimit-used header and the budget block report, so a
	// sweep's cost can be read the way it is read against api.github.com:
	// from the answers, never by subtracting two readings.
	used map[string]int
}

// budgets is the limit of each bucket the fake prices. Search has its own
// small budget, and the collector scales its reserve to it.
var budgets = map[string]int{"core": 5000, "graphql": 5000, "search": 30}

// New starts a fake serving the fixtures in dir, which is testdata relative to
// the calling suite's own directory. It stops when the test ends.
//
// Each overlay is a directory whose fixtures replace the ones of the same name
// in dir, and nothing else. It exists for the pictures of the card: they want
// an account with a year of contributions and a repository in several
// languages, and every other suite here asserts on the smaller account the
// base fixtures describe, so the richer one cannot simply replace it.
//
// With an overlay, the fake also answers for repositories other than
// hello-world. A request under /repos/octocat/<name>/ is routed as if it named
// hello-world, and answered with the fixture "<name>~<fixture>" when the
// overlay has one, which is how each repository of the gallery gets its own
// stars and language, and with hello-world's otherwise. Without an overlay
// such a request is a 404, exactly as before, so no base suite can come to
// depend on a repository it never listed.
func New(tb testing.TB, dir string, overlays ...string) *Server {
	tb.Helper()
	bodies := readFixtures(tb, dir)
	for _, overlay := range overlays {
		maps.Copy(bodies, readFixtures(tb, overlay))
	}
	s := &Server{
		tb: tb, bodies: bodies, used: map[string]int{},
		perRepo: len(overlays) > 0, failing: map[string]int{},
		now: func() time.Time { return time.Now().UTC() },
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(s.srv.Close)
	return s
}

// readFixtures reads the fixture directory into memory.
func readFixtures(tb testing.TB, dir string) map[string][]byte {
	tb.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("the fixtures the suites drive themselves with: %v", err)
	}
	bodies := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			tb.Fatalf("fixture %s: %v", e.Name(), readErr)
		}
		bodies[e.Name()] = body
	}
	return bodies
}

// URL is the base URL to point github.base_url at.
func (s *Server) URL() string { return s.srv.URL }

// Fail makes one path answer with a status instead of with its fixture, and
// keeps answering that way until Fail is called again for the same path.
//
// It exists for one shape of test and it is worth naming it: on the author's
// own account, one 502 from /repos/<repo>/actions/runs/<id>/jobs, once per
// repository, cost five repositories every workflow run and job they had.
// Nothing here can be made to answer that by arranging fixtures, because the
// fixtures are what a working GitHub says. A status of zero restores the
// fixture, so a test can break a path for one sweep and mend it for the next.
func (s *Server) Fail(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status == 0 {
		delete(s.failing, path)
		return
	}
	s.failing[path] = status
}

// Requests returns a copy of everything served so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	bucket := bucketOf(r.URL.Path)
	a := s.answer(r, body)
	// A validator that still names the fixture the request would be served
	// is answered 304 with no body, as GitHub does, and is charged nothing.
	// The ETag is per fixture and not per rendering, so the tokens a fixture
	// carries do not turn every repeat into a 200.
	if a.etag != "" && r.Header.Get("If-None-Match") == a.etag {
		a = answer{status: http.StatusNotModified, etag: a.etag, cost: 0}
	}

	s.mu.Lock()
	s.used[bucket] += a.cost
	used := s.used[bucket]
	s.requests = append(s.requests, Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
		Status: a.status, Cost: a.cost, GraphQL: a.graphQL,
	})
	s.mu.Unlock()

	soon := time.Now().UTC().Add(time.Hour)
	if a.graphQL != "" && strings.Contains(a.graphQL, ghapi.RateLimitAlias) {
		a.body = withBudget(a.body, used, soon)
	}
	h := w.Header()
	if bucket != "" {
		// The collector reads the bucket the response declares to decide
		// what it has left, so search has to say search, with search's own
		// small budget. Object storage is off the API and says nothing.
		h.Set("x-ratelimit-resource", bucket)
		h.Set("x-ratelimit-limit", strconv.Itoa(budgets[bucket]))
		h.Set("x-ratelimit-used", strconv.Itoa(used))
		h.Set("x-ratelimit-remaining", strconv.Itoa(budgets[bucket]-used))
		h.Set("x-ratelimit-reset", strconv.FormatInt(soon.Unix(), 10))
	}
	if a.etag != "" {
		h.Set("ETag", a.etag)
	}
	if a.location != "" {
		h.Set("Location", a.location)
	}
	if a.contentType != "" {
		h.Set("Content-Type", a.contentType)
	}
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// storagePrefix is where the job log redirect lands: object storage, which
// is not api.github.com. It charges no bucket and carries neither the rate
// headers nor a validator, and the client reads the budget off the redirect
// on the way through rather than off this answer.
const storagePrefix = "/storage/"

// OffAPI says whether a path is served beside the API rather than by it:
// the object storage a job log redirects to, and the public profile page the
// achievements family reads. Neither is charged, validated, nor sent the
// token, and the suites hold both to that.
func OffAPI(path string) bool {
	return strings.HasPrefix(path, storagePrefix) || path == profilePage
}

// bucketOf names the budget a path is charged to, the way the client infers
// it: GraphQL has its own, anything under /search has its own, everything
// else on the API is core, and object storage is no bucket at all.
func bucketOf(path string) string {
	switch {
	case path == "/graphql":
		return "graphql"
	case strings.HasPrefix(path, "/search/"):
		return "search"
	case OffAPI(path):
		return ""
	}
	return "core"
}

// answer is one response before it is written: what a request would be
// served if it carried no validator.
type answer struct {
	status      int
	contentType string
	body        []byte
	// etag is the validator of the fixture behind the body, empty for an
	// answer GitHub does not validate: a 404, a redirect, a GraphQL answer.
	etag     string
	location string
	// cost is what the answer charges its bucket unless it becomes a 304.
	cost int
	// graphQL is the query text when the answer is to one, so the budget
	// block can be put under the alias the client asked for it by.
	graphQL string
}

const jsonType = "application/json; charset=utf-8"

// answer decides what a request is served.
func (s *Server) answer(r *http.Request, body []byte) answer {
	s.mu.Lock()
	broken := s.failing[r.URL.Path]
	s.mu.Unlock()
	if broken != 0 {
		// Charged, because a request that was made was made, and with no
		// validator, because there is no fixture behind it to validate.
		return answer{
			status: broken, contentType: jsonType, cost: 1,
			body: []byte(`{"message":"` + http.StatusText(broken) + `"}`),
		}
	}
	if r.URL.Path == jobLogPath {
		return answer{status: http.StatusFound, location: "/storage/2000000011.txt", cost: 1}
	}
	var query string
	if r.URL.Path == "/graphql" {
		query = graphQLQuery(body)
	}
	routed, repo := r, ""
	if s.perRepo {
		routed, repo = asHelloWorld(r)
	}
	switch name := route(routed, body); name {
	case "":
		return answer{
			status: http.StatusNotFound, contentType: jsonType, cost: 1,
			body: []byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`),
		}
	case emptyPage:
		return answer{status: http.StatusOK, contentType: jsonType, body: []byte("[]"), etag: etagOf([]byte(emptyPage)), cost: 1}
	case graphQLUnknown:
		return answer{status: http.StatusOK, contentType: jsonType, body: []byte(graphQLNoFixture), cost: 1, graphQL: query}
	default:
		if _, own := s.bodies[repo+"~"+name]; repo != "" && own {
			name = repo + "~" + name
		}
		a := s.render(name)
		a.graphQL = query
		switch {
		case OffAPI(r.URL.Path):
			// Off the API: nothing to charge and nothing to validate.
			a.cost, a.etag = 0, ""
		case query != "":
			// A POST is never conditional, and the gateway sends no
			// validator with a GraphQL answer.
			a.etag = ""
			// The budget probe is the one query GitHub does not charge
			// for (measured, see ghapi.GraphQLRate), and the block it
			// carries is still priced at one.
			if strings.TrimSpace(query) == ghapi.BudgetQuery {
				a.cost = 0
			}
		}
		return a
	}
}

// graphQLQuery reads the query text out of a GraphQL request body.
func graphQLQuery(body []byte) string {
	var env struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Query
}

// etagOf is the validator of a fixture: a digest of the file as it is on
// disk, before any token is rendered, so it is the same for every request
// that lands on the fixture and different for every other fixture. Strong
// rather than weak, quoted, the way GitHub sends them.
func etagOf(fixture []byte) string {
	sum := sha256.Sum256(fixture)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// withBudget puts the budget block under the alias the client selects it by,
// beside whatever the fixture's data already holds, so every GraphQL answer
// prices itself the way api.github.com's do: this query cost one, and the
// bucket has been charged used so far. A body that is not a data envelope,
// an errors-only answer for one, is left as it is.
func withBudget(body []byte, used int, reset time.Time) []byte {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil || len(env["data"]) == 0 {
		return body
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(env["data"], &data); err != nil || data == nil {
		return body
	}
	block, err := json.Marshal(map[string]any{
		"limit": budgets["graphql"], "cost": 1, "used": used,
		"remaining": budgets["graphql"] - used, "resetAt": reset.Format(time.RFC3339),
	})
	if err != nil {
		return body
	}
	data[ghapi.RateLimitAlias] = block
	if env["data"], err = json.Marshal(data); err != nil {
		return body
	}
	out, err := json.Marshal(env)
	if err != nil {
		return body
	}
	return out
}

// route names the fixture that answers this request, "" for a 404 and
// emptyPage for a page past the first.
func route(r *http.Request, body []byte) string {
	q := r.URL.Query()
	switch r.URL.Path {
	case "/graphql":
		return graphQLFixture(body)
	case repoPath + "/actions/runs":
		if q.Get("status") == "failure" {
			return "actions_runs_failed.json"
		}
		return "actions_runs.json"
	case "/user/packages":
		if q.Get("package_type") == "container" {
			return "packages_container.json"
		}
		return emptyPage
	}
	name, ok := rest[r.URL.Path]
	switch {
	case !ok:
		return ""
	case q.Get("page") != "" && q.Get("page") != "1":
		return emptyPage
	default:
		return name
	}
}

// graphQLFixture reads the query out of the body and matches it against the
// markers.
func graphQLFixture(body []byte) string {
	query := graphQLQuery(body)
	if query == "" {
		return graphQLUnknown
	}
	// The budget probe is matched by equality against the constant the client
	// sends, because no substring of it identifies the probe: the same block is
	// injected into the root of every other query as well.
	if strings.TrimSpace(query) == ghapi.BudgetQuery {
		return "graphql_rate_limit.json"
	}
	for _, q := range graphQL {
		if strings.Contains(query, q.marker) {
			return q.fixture
		}
	}
	return graphQLUnknown
}

// render is a fixture as it is served: its tokens replaced, its type set by
// its extension, and the validator of the file it came from beside it.
func (s *Server) render(name string) answer {
	body, ok := s.bodies[name]
	if !ok {
		s.tb.Errorf("no fixture named %s", name)
		return answer{status: http.StatusInternalServerError, cost: 1}
	}
	// The tokens are replaced whatever the file is. A job log is plain text
	// and every line of it starts with a timestamp the collector parses, so a
	// fixture served without its tokens resolved reaches a collector as the
	// literal "@DAYS_AGO_5@" and is read as the zero time, quietly.
	a := answer{
		status: http.StatusOK, etag: etagOf(body), cost: 1,
		body: s.resolve(body),
	}
	switch {
	case strings.HasSuffix(name, ".json"):
		a.contentType = jsonType
	case strings.HasSuffix(name, ".html"):
		a.contentType = "text/html; charset=utf-8"
	default:
		a.contentType = "text/plain; charset=utf-8"
	}
	return a
}

// resolve is a fixture with its tokens replaced: the moment this ran, the day
// it ran on, and every date a fixture spelled as an offset from that day.
//
// "@TODAY@" is the start of that day and not the moment this ran because a
// dated point carries its own date as part of its identity, so a fixture that
// spells one "@NOW@" gives two sweeps of the same test two different points.
// TestFileSinkWritesTheSamePointsInBothFormats caught that on a sponsorship
// whose two renderings were sixteen seconds apart. The start of the day is
// recent enough for any dashboard window and is the same string for both
// sweeps, except across midnight, which is rare and loud.
func (s *Server) resolve(body []byte) []byte {
	now := s.now()
	soon := now.Add(time.Hour)
	today := now.Truncate(24 * time.Hour)
	out := strings.ReplaceAll(string(body), "@NOW@", now.Format(time.RFC3339))
	out = strings.ReplaceAll(out, "@TODAY@", today.Format(time.RFC3339))
	out = strings.ReplaceAll(out, "@SOON_EPOCH@", strconv.FormatInt(soon.Unix(), 10))
	out = strings.ReplaceAll(out, "@SOON@", soon.Format(time.RFC3339))
	out = weekEpochMarker.ReplaceAllStringFunc(out, s.weekEpoch)
	return []byte(daysAgoMarker.ReplaceAllStringFunc(out, s.agoDate))
}

// daysAgoMarker matches "@DAYS_AGO_12@" and "@DAYS_AHEAD_90@", which become
// the date twelve days before or ninety days after the fake's today, without a
// time, so a fixture spells the hour itself: "@DAYS_AGO_12@T16:00:00Z".
//
// A dashboard asks for the last day, week or month, and a fixture that writes
// the date it was authored on drifts out of those windows one panel at a time.
// The day a pull request merged on crossed now-30d between one scheduled run
// and the next, and four panels of every store went blank with nothing having
// changed in the code. Counting back from today keeps a fixture the same
// distance from the present for as long as it exists, which is what the
// fixture meant in the first place. Ahead is the same argument pointed the
// other way: an artifact that expires, a milestone that is due and a payout
// that has not happened yet are all wrong once the day they name goes past.
var daysAgoMarker = regexp.MustCompile(`@DAYS_(AGO|AHEAD)_(\d+)@`)

// agoDate is that replacement, against whatever clock this fake was given. It
// counts whole days from the start of that day for the reason a dated point
// gives: a point carries its own date, so two sweeps of one test have to spell
// it identically.
func (s *Server) agoDate(marker string) string {
	m := daysAgoMarker.FindStringSubmatch(marker)
	n, err := strconv.Atoi(m[2])
	if err != nil {
		// Unreachable through the regexp, which matches digits and nothing
		// else, and said out loud rather than returned quietly: a marker served
		// as itself is a fixture that arrives with "@DAYS_AGO_10@" where a date
		// should be, and what fails then is a collector three packages away.
		s.tb.Errorf("fixture marker %s: %v", marker, err)
		return marker
	}
	if m[1] == "AGO" {
		n = -n
	}
	return s.now().Truncate(24*time.Hour).AddDate(0, 0, n).Format(time.DateOnly)
}

// weekEpochMarker matches "@WEEK_EPOCH_2@", which becomes the week two before
// the fake's current one as the star history spells a week: the Unix second
// of its Sunday at 00:00 UTC, a bare number rather than a date.
//
// A token rather than a number written out for the reason the day offsets
// have one, and more so: a week is a row in every star panel, and a literal
// epoch drifts out of the window a dashboard asks for without anything in
// the fixture looking like a date. TestNoFixtureWritesOutARecentEpoch is the
// guard that makes a written-out one fail.
var weekEpochMarker = regexp.MustCompile(`@WEEK_EPOCH_(\d+)@`)

// weekEpoch is that replacement, against whatever clock this fake was given.
// It starts from the day, not the moment, so two sweeps of one test in the
// same week spell every week identically.
func (s *Server) weekEpoch(marker string) string {
	m := weekEpochMarker.FindStringSubmatch(marker)
	n, err := strconv.Atoi(m[1])
	if err != nil {
		// Unreachable through the regexp, and said out loud for the reason
		// agoDate gives.
		s.tb.Errorf("fixture marker %s: %v", marker, err)
		return marker
	}
	today := s.now().Truncate(24 * time.Hour)
	sunday := today.AddDate(0, 0, -int(today.Weekday())-7*n)
	return strconv.FormatInt(sunday.Unix(), 10)
}

// FreezeAt stops the fake's clock, so every relative date in its fixtures
// resolves to the same day however long from now this runs.
//
// Three suites need this. The card gallery draws the pictures committed under
// site/src/assets and a test compares them byte for byte, so the data behind
// them has to be the same data every day. And the sweep test in internal/run
// that runs two sweeps days apart needs the fixtures to stand still between
// them, so that only the runner's clock moves. The containerised suite's
// check that a second sweep into one InfluxDB database adds no dated row needs
// the same, for the same reason: its two fakes are two, and a UTC midnight
// between them would date every fixture a day later. Everywhere else the
// point of a relative date is that it moves. Call it before the first
// request.
func (s *Server) FreezeAt(at time.Time) {
	s.now = at.UTC
}

// The offsets the fixtures spell their dates with, for the assertions that
// have to name one. A test that wrote the date out instead would be checking a
// day the fixture had stopped mentioning: that is how the release of 2.4.0
// came to fail with four dashboard panels of every store answering nothing,
// twelve hours after the same suite had passed.
const (
	// TrafficDaysAgo is the traffic day carrying 120 views (traffic_views.json).
	TrafficDaysAgo = 12
	// RunDaysAgo is the workflow run that finished at 10:04:10
	// (actions_runs.json).
	RunDaysAgo = 4
)

// DaysAgo is the UTC midnight n days before today, for a test that has to name
// the day a fixture spelled as an offset. Resolving it here rather than
// writing the date out is what stops the assertion and the fixture drifting
// apart.
func DaysAgo(n int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n)
}

// DaysAgoDate is that day as a fixture spells it.
func DaysAgoDate(n int) string { return DaysAgo(n).Format(time.DateOnly) }

// asHelloWorld reads a request for another of octocat's repositories as the
// same request for hello-world, and returns the name it replaced. A request
// that names hello-world, or no repository, comes back as it went in.
func asHelloWorld(r *http.Request) (routed *http.Request, repo string) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/repos/"+Login+"/")
	if !ok {
		return r, ""
	}
	name, suffix, _ := strings.Cut(rest, "/")
	if name == "" || repoPath == "/repos/"+Login+"/"+name {
		return r, ""
	}
	routed = r.Clone(r.Context())
	routed.URL.Path = repoPath
	if suffix != "" {
		routed.URL.Path += "/" + suffix
	}
	return routed, name
}
