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

// Actions collects workflow runs as dated facts.
//
// A run is an event that happened and finished: it belongs at the instant it
// completed, not at the instant we noticed. That makes "how long did CI take
// last Tuesday" answerable, which a gauge of "current run duration" never is.
//
// Only runs newer than `since` are fetched, so a sweep every quarter of an hour
// costs one page rather than the whole history.
type Actions struct {
	Since time.Time
	// Jobs also collects the per-job and per-step breakdown of every run in
	// this sweep. It costs one extra request per run, so it is opt-in.
	Jobs bool
	// MaxJobRuns caps that expansion. Zero means every run in the sweep.
	//
	// It bounds what one sweep pays, not which runs get their jobs: a run an
	// earlier sweep already expanded (see Expanded) does not count, so a
	// window with more runs than the cap fills in over successive sweeps at
	// MaxJobRuns listings each, and then costs nothing until a new run lands.
	MaxJobRuns int
	// Expanded is the set of runs whose jobs an earlier sweep already wrote.
	// A run in it is not listed again, and every run expanded now is added.
	//
	// The jobs of a completed attempt never change, and GitHub answers a
	// second listing of them with a 304 that costs no quota but does cost a
	// round trip: measured on eighteen repositories, three hundred and two
	// job listings a sweep, two hundred and eighty six of them 304, and a
	// hundred and thirty seven seconds of waiting for them, ninety six times
	// a day. Nil keeps no memory, which is what a backfill wants.
	Expanded map[RunKey]struct{}
	// PerPage is how many runs a page of the listing holds. Zero means a
	// hundred, the most GitHub serves and what a first sweep and a backfill
	// ask for. A sweep every quarter of an hour asks for thirty: the page is
	// thirteen kilobytes a run of which the collector keeps six hundred
	// bytes, and a page of a hundred was a megabyte and a half decompressed
	// per active repository per sweep, forty six percent of everything a day
	// downloads. Since still pages on when a page fills with runs newer than
	// it, so nothing is lost; what changes is how many runs the exporter,
	// which holds only the last sweep, shows between builds.
	PerPage int
	// Walk bounds the run list. Its default is two pages; a backfill asks for
	// everything and bounds itself by date instead.
	Walk Walk
}

// RunKey is the identity of one attempt of a workflow run. The attempt is
// part of it because a re-run keeps the run's id and replaces its jobs: keyed
// by id alone, every retry after the first sweep that saw the run would be
// invisible.
type RunKey struct {
	ID      int64
	Attempt int
}

// runRow is one row of the run list. It is package level rather than local to
// Collect because jobsFor needs the run a job belongs to: the job listing
// names its workflow only by the same dynamic title that makes `name`
// useless as a tag.
type runRow struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Path         string `json:"path"`
	WorkflowID   int64  `json:"workflow_id"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	Branch       string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	RunAttempt   int    `json:"run_attempt"`
	RunNumber    int    `json:"run_number"`
	HTMLURL      string `json:"html_url"`
	// PullRequests are the pull requests GitHub linked to the run, which is
	// the only way from a run to its pull request other than matching
	// head_sha by hand. Empty on a push to a branch nobody has opened one
	// for, and on a run from a fork, whose pull request GitHub does not
	// link.
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
	HeadCommit struct {
		Message string `json:"message"`
	} `json:"head_commit"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
	CreatedAt    time.Time `json:"created_at"`
	RunStartedAt time.Time `json:"run_started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Actor        struct {
		Login string `json:"login"`
	} `json:"actor"`
	TriggeringActor struct {
		Login string `json:"login"`
	} `json:"triggering_actor"`
}

// workflowTag is the bounded identity of the workflow a run belongs to.
//
// `name` is not it. On a dynamically named run GitHub puts the pull request
// title or the Dependabot update there: measured over the whole history of one
// repository, 595 distinct names against 18 workflow files, one new series per
// pull request and per dependency bump for ever. This is the hosted runner
// mistake with a different field name. The file path is stable, including for
// the generated ones ("dynamic/github-code-scanning/codeql"), and
// /actions/workflows keys the human name by that same path, so a dashboard
// that wants to print "CodeQL" joins on it.
func workflowTag(r *runRow) string {
	if r.Path != "" {
		return r.Path
	}
	// A response without a path still has a stable numeric identity, and it is
	// never the title of a pull request.
	if r.WorkflowID != 0 {
		return strconv.FormatInt(r.WorkflowID, 10)
	}
	return noneTag
}

// actorTag is who caused this attempt to run.
//
// `triggering_actor` rather than `actor`: they differ exactly when it matters,
// on a re-run and on a scheduled run, and this is the tag that answers who
// burns the CI budget. Bounded by the people and apps that can push to the
// account's own repositories, four on the busiest one.
func actorTag(r *runRow) string {
	if r.TriggeringActor.Login != "" {
		return r.TriggeringActor.Login
	}
	if r.Actor.Login != "" {
		return r.Actor.Login
	}
	return noneTag
}

func (a Actions) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)

	runs, declared, listed, err := a.runList(ctx, c, repo)
	if err != nil {
		return nil, err
	}
	points, expanded, err := a.runPoints(ctx, c, repo, runs, base)
	// Remembered here, beside the points that carry those jobs, and not on
	// the way out: the runner keeps what a collector that failed returned, so
	// a run whose jobs are in points has been written whether or not a later
	// call failed, and listing it again next sweep is the round trip this
	// memory exists to save. It used to be remembered only once the whole
	// collection had succeeded, which was right for as long as the runner
	// threw a failed collector's points away.
	if a.Expanded != nil {
		for _, key := range expanded {
			a.Expanded[key] = struct{}{}
		}
	}
	if err != nil {
		return points, err
	}
	// How many runs the repository has ever had. The walk sees the newest few
	// hundred by design, so this is the only place the whole history is
	// counted, and it saves a dashboard from scanning the entire table to
	// answer "how many ever" (which InfluxDB 3 Core now refuses outright).
	// Current state, so stamped now, like gh_repo_total.
	if listed {
		points = append(points, sink.Point{
			Measurement: "gh_workflow_run_total",
			Tags:        base,
			Fields:      map[string]any{"runs": declared},
			Time:        now,
		})
	}
	// The cache totals are the last call of the family and the least of it:
	// a repository's whole run and job history is already in points, so a
	// failure here goes back with them rather than instead of them.
	cache, err := cachePoints(ctx, c, repo, base, now)
	return append(points, cache...), err
}

// runList walks the run listing newest first and says how many the repository
// has ever had.
//
// No `created` filter. GitHub returns runs newest first, so `Since` is used to
// stop paging rather than to exclude anything from the first page. Filtering
// server side looked tidier and was wrong: on a quiet account a two hour
// window is usually empty, so every sweep published nothing and the exporter,
// which holds only what the last sweep collected, showed "no data" for
// continuous integration between builds. The newest runs are always worth
// having, however old they are.
//
// Paginated, because one page of 100 is not "the runs since the cutoff", it is
// the newest hundred: on a busy repository a first sweep asking for a month got
// seven hours and reported it as a month.
//
// listed says whether the listing answered at all, which is what decides
// whether declared is a count or an absence.
func (a Actions) runList(ctx context.Context, c *ghapi.Client, repo Repo) (all []runRow, declared int, listed bool, err error) {
	perPage := a.PerPage
	if perPage <= 0 {
		perPage = 100
	}
	path := fmt.Sprintf("/repos/%s/actions/runs?per_page=%d", repo.FullName, perPage)
	w := a.Walk
	if w.Since.IsZero() {
		w.Since = a.Since
	}
	most := w.limit(2)
	for page := 1; page <= most; page++ {
		var runs struct {
			TotalCount int      `json:"total_count"`
			Runs       []runRow `json:"workflow_runs"`
		}
		if _, _, e := c.GetJSON(ctx, fmt.Sprintf("%s&page=%d", path, page), &runs, ""); e != nil {
			if isSkippable(e) || isPaginationLimit(e) {
				break
			}
			return nil, 0, false, e
		}
		listed = true
		declared = runs.TotalCount
		if len(runs.Runs) == 0 {
			break
		}
		all = append(all, runs.Runs...)
		if len(runs.Runs) < perPage || w.past(runs.Runs[len(runs.Runs)-1].UpdatedAt) {
			break
		}
	}
	return all, declared, listed, nil
}

// runPoints renders one point per finished run, and the jobs of as many of
// them as the caller is willing to pay for. It also says which runs those
// were, for Collect to remember, and it says it on the failing path too: the
// points rendered before the failure are kept, so the runs behind them have
// been written.
func (a Actions) runPoints(ctx context.Context, c *ghapi.Client, repo Repo, all []runRow, base map[string]string) (points []sink.Point, expanded []RunKey, err error) {
	for i := range all {
		r := &all[i]
		if r.Status != "completed" {
			continue // an unfinished run has no duration yet; it will be caught next sweep
		}
		key := RunKey{ID: r.ID, Attempt: r.RunAttempt}
		_, written := a.Expanded[key]
		if a.Jobs && !written && (a.MaxJobRuns == 0 || len(expanded) < a.MaxJobRuns) {
			jp, listErr := a.jobsFor(ctx, c, repo, r, base)
			if listErr != nil {
				// The runs and jobs already rendered go back with the error,
				// and so do the runs they belong to: both are kept now.
				return points, expanded, listErr
			}
			points = append(points, jp...)
			// Counted even when the listing answered nothing, and so
			// remembered: a run whose jobs are gone stays gone, and asking
			// again every sweep is the round trip the memory exists to save.
			expanded = append(expanded, key)
		}
		points = append(points, sink.Point{
			Measurement: "gh_workflow_run",
			Tags: merge(base, map[string]string{
				"workflow": workflowTag(r), "event": r.Event,
				"conclusion": r.Conclusion, "actor": actorTag(r),
			}),
			Fields: workflowRunFields(r, base),
			Time:   r.UpdatedAt, // when it finished
		})
	}
	return points, expanded, nil
}

// workflowRunFields is everything about one run that is a value to read rather
// than a series to group by. base names the repository, so a run that came
// from another one can say so.
func workflowRunFields(r *runRow, base map[string]string) map[string]any {
	start := r.RunStartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	fields := map[string]any{
		"duration_seconds": int(r.UpdatedAt.Sub(start).Seconds()),
		"attempt":          r.RunAttempt,
		"success":          r.Conclusion == "success",
		// Fields rather than tags: both are unbounded. They are here so a
		// retry can be paired with the run it retried, which is the only
		// way to tell a flaky test from a broken one.
		"run_id":   r.ID,
		"head_sha": r.HeadSHA,
		"url":      r.HTMLURL,
		// What the `workflow` tag used to hold. Kept so nothing is lost,
		// as a value to read rather than a series to group by.
		"name":        r.Name,
		"workflow_id": r.WorkflowID,
		// A branch is an identity, and this one is still unbounded: 682
		// distinct values on 18 repositories, because every pull request
		// and every Dependabot bump makes one and it never comes back.
		// The bounded question ("was this a push or a pull request") is
		// already the `event` tag.
		//
		// Named head_branch, not branch, and that is not cosmetic: `branch`
		// was a tag on this measurement until now, and InfluxDB 3 fixes a
		// column as one or the other the first time it sees it and rejects
		// every later write that disagrees (docs/sinks.md). Reusing the
		// name would have meant dropping gh_workflow_run, losing the CI
		// history the walk cannot backfill, to publish a value the API
		// itself calls head_branch.
		"head_branch": r.Branch,
	}
	if r.DisplayTitle != "" && r.DisplayTitle != r.Name {
		fields["title"] = r.DisplayTitle
	}
	// The number GitHub shows as "#1483" and the one people quote; run_id is
	// what the API keys by and nobody says out loud.
	if r.RunNumber > 0 {
		fields["run_number"] = r.RunNumber
	}
	if n := r.PullRequests; len(n) > 0 {
		fields["pull_request"] = n[0].Number
		fields["pull_requests"] = len(n)
	}
	// The first line of the commit that ran, so a table of runs reads like
	// the log rather than like a list of hashes. gh_commit.headline exists
	// only for the default branch and the last thirty days.
	setNonEmpty(fields, "headline", headline(r.HeadCommit.Message))
	// Only when the run came from somewhere else, which is what a fork's
	// pull request looks like.
	if r.HeadRepository.FullName != "" && !strings.EqualFold(r.HeadRepository.FullName, base["full_name"]) {
		fields["head_repo"] = r.HeadRepository.FullName
	}
	// Only when they differ, which is what a re-run looks like: Dependabot
	// opened the run, a human asked for the second attempt.
	if r.Actor.Login != "" && r.Actor.Login != actorTag(r) {
		fields["initial_actor"] = r.Actor.Login
	}
	// Queue time is the wait before a runner picked it up. It is the number
	// that explains a slow pipeline that is not slow to execute.
	//
	// First attempts only. created_at is the run's, not the attempt's, and a
	// re-run keeps it: measured on a retried run, attempt 2 had run_started_at
	// twelve minutes after created_at, which is the time a person took to
	// press the button, not a runner queue. Over this account's rows the mean
	// was 0.4 s on first attempts and 1,747 s on second ones. The real queue
	// of a retry exists only per job, in gh_workflow_job.
	if r.RunAttempt <= 1 && !r.RunStartedAt.IsZero() && !r.CreatedAt.IsZero() {
		fields["queued_seconds"] = int(r.RunStartedAt.Sub(r.CreatedAt).Seconds())
	}
	return fields
}

// headline is the first line of a commit message, the way GitHub shows it.
func headline(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return strings.TrimSpace(line)
}

// cachePoints is what Actions has stored: the total, and a row per entry.
//
// The total says a repository holds twelve gigabytes; only the per-entry rows
// say which key holds them and which key has not been touched for a week,
// which is what decides what GitHub evicts when a repository crosses its ten
// gigabyte ceiling. Both are current state, so both are stamped now.
func cachePoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time) ([]sink.Point, error) {
	var points []sink.Point
	var usage struct {
		Size  int64 `json:"active_caches_size_in_bytes"`
		Count int   `json:"active_caches_count"`
	}
	switch _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/actions/cache/usage", &usage, ""); {
	case err == nil:
		points = append(points, sink.Point{
			Measurement: "gh_actions_cache",
			Tags:        base,
			Fields:      map[string]any{"size_bytes": usage.Size, "count": usage.Count},
			Time:        now,
		})
	case !isSkippable(err):
		return points, err
	}

	var caches struct {
		TotalCount int `json:"total_count"`
		Caches     []struct {
			Ref            string    `json:"ref"`
			Key            string    `json:"key"`
			SizeInBytes    int64     `json:"size_in_bytes"`
			CreatedAt      time.Time `json:"created_at"`
			LastAccessedAt time.Time `json:"last_accessed_at"`
		} `json:"actions_caches"`
	}
	switch _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/actions/caches?per_page=100", &caches, ""); {
	case err == nil:
		for _, e := range caches.Caches {
			points = append(points, sink.Point{
				Measurement: "gh_actions_cache_entry",
				Tags: merge(base, map[string]string{
					// The key carries a content hash, so it is a series per
					// build and cannot be a tag. The prefix before the hash is
					// the thing a reader means by "the pnpm cache".
					"cache": cachePrefix(e.Key),
					"ref":   e.Ref,
				}),
				Fields: map[string]any{
					"size_bytes": e.SizeInBytes, "caches": 1, "key": e.Key,
					"days_since_use": int(now.Sub(e.LastAccessedAt).Hours() / 24),
					"age_days":       int(now.Sub(e.CreatedAt).Hours() / 24),
				},
				// Stamped at the start of the day rather than at creation: it
				// is a snapshot of what is stored now, and a day's sweeps
				// should rewrite one row rather than add one an hour.
				Time: now.UTC().Truncate(24 * time.Hour),
			})
		}
	case !isSkippable(err):
		// The totals row above is already in hand, and the per-entry listing
		// failing does not make it less true.
		return points, err
	}
	return points, nil
}

// cachePrefix is the part of a cache key before its content hash.
//
// GitHub's own convention is dash-separated segments ending in a hash, as in
// node-cache-Linux-x64-pnpm-95070cf0a7bb. Keeping the whole key as a tag would
// be one series per build; keeping the prefix groups every build of the same
// cache together, and the whole key stays as a field.
func cachePrefix(key string) string {
	parts := strings.Split(key, "-")
	for i, p := range parts {
		if len(p) >= 16 && isHex(p) {
			if i == 0 {
				return key
			}
			return strings.Join(parts[:i], "-")
		}
	}
	return key
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// jobsFor collects the individual jobs of one run.
//
// Run-level timing hides where the time went: a run that takes twenty minutes
// because one job waited eighteen for a runner looks exactly like one that
// spent eighteen executing. Only the job level separates them, and only the
// job level names the runner and the steps.
//
// It costs one request per run, so the caller decides how many runs are worth
// it rather than this walking everything.
func (a Actions) jobsFor(ctx context.Context, c *ghapi.Client, repo Repo, r *runRow, base map[string]string) ([]sink.Point, error) {
	var res struct {
		Jobs []struct {
			Name        string    `json:"name"`
			Status      string    `json:"status"`
			Conclusion  string    `json:"conclusion"`
			CreatedAt   time.Time `json:"created_at"`
			StartedAt   time.Time `json:"started_at"`
			CompletedAt time.Time `json:"completed_at"`
			RunnerName  string    `json:"runner_name"`
			HTMLURL     string    `json:"html_url"`
			RunnerGroup string    `json:"runner_group_name"`
			RunAttempt  int       `json:"run_attempt"`
			HeadBranch  string    `json:"head_branch"`
			HeadSHA     string    `json:"head_sha"`
			Labels      []string  `json:"labels"`
			Steps       []struct {
				Name        string    `json:"name"`
				Number      int       `json:"number"`
				Conclusion  string    `json:"conclusion"`
				StartedAt   time.Time `json:"started_at"`
				CompletedAt time.Time `json:"completed_at"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	// filter=all rather than the default latest. Measured on one re-run:
	// latest served 9 jobs, all served 18, and the nine it hides are the first
	// attempt with its two failures. Same request, same cost, and without them
	// a job that failed and passed on the retry is indistinguishable from one
	// that passed first time, which is the definition of a flaky test.
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?per_page=100&filter=all", repo.FullName, r.ID)
	if _, _, err := c.GetJSON(ctx, path, &res, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	var points []sink.Point
	for i := range res.Jobs {
		j := &res.Jobs[i]
		if j.CompletedAt.IsZero() {
			continue
		}
		labels := ""
		if len(j.Labels) > 0 {
			labels = strings.Join(j.Labels, ",")
		}
		// The listing is asked for every attempt, so the attempt is part of the
		// job's identity: without it the two tries of a re-run are one series
		// and only their completed_at tells them apart.
		attempt := j.RunAttempt
		if attempt == 0 {
			attempt = max(r.RunAttempt, 1)
		}
		branch := j.HeadBranch
		if branch == "" {
			branch = r.Branch
		}
		headSHA := j.HeadSHA
		if headSHA == "" {
			headSHA = r.HeadSHA
		}
		fields := map[string]any{
			"duration_seconds": int(j.CompletedAt.Sub(j.StartedAt).Seconds()),
			"success":          j.Conclusion == "success",
			// Pairs a job with the run it belongs to, and a retried job with
			// the commit it retried. Both unbounded, both fields, for the same
			// reason as on the run itself.
			"run_id":      r.ID,
			"head_sha":    headSHA,
			"head_branch": branch,
			// A hosted runner is named uniquely per run ("GitHub Actions
			// 1000163135"), so this is a field. As a tag it would create one
			// series per job ever run. Self-hosted names are stable and this
			// still records which machine took the job.
			"runner": j.RunnerName,
			"url":    j.HTMLURL,
		}
		if !j.CreatedAt.IsZero() && !j.StartedAt.IsZero() {
			fields["queued_seconds"] = int(j.StartedAt.Sub(j.CreatedAt).Seconds())
		}
		if !stepsWithheld(j.Conclusion, len(j.Steps)) {
			fields["steps"] = len(j.Steps)
		}
		points = append(points, sink.Point{
			Measurement: "gh_workflow_job",
			Tags: merge(base, map[string]string{
				// job_name rather than job: Prometheus reserves `job` for the
				// scrape job and the OTLP receiver overwrites it with the
				// service name, which made every row read "ghchronicle".
				"job_name": j.Name, "conclusion": j.Conclusion,
				// A job that ran on a hosted runner belongs to no runner
				// group, and one that asked for no labels has none: measured
				// on 2026-09-10, 25 of 349 jobs carried no group. Written raw
				// both are empty tag values, which InfluxDB drops, and those
				// 25 rows would form a series of their own.
				"runner_group": orNone(j.RunnerGroup), "labels": orNone(labels),
				// Nothing here said which workflow a job belonged to, and the
				// names collide: 'Analyze (actions)' was measured under seven
				// different workflow titles on one repository. The job's own
				// workflow_name is the dynamic title again, so the identity
				// comes from the run.
				"workflow": workflowTag(r),
				"attempt":  strconv.Itoa(attempt),
			}),
			Fields: fields,
			Time:   j.CompletedAt,
		})
		// The slowest step is what a maintainer would act on, so each step is
		// kept rather than only the job total.
		for _, s := range j.Steps {
			if s.CompletedAt.IsZero() || s.StartedAt.IsZero() {
				continue
			}
			points = append(points, sink.Point{
				Measurement: "gh_workflow_step",
				Tags: merge(base, map[string]string{
					"job_name": j.Name, "step": s.Name, "conclusion": s.Conclusion,
					// A step inherits both collisions from its job: the same
					// job name under several workflows, and the same step in
					// two attempts of one run.
					"workflow": workflowTag(r),
					"attempt":  strconv.Itoa(attempt),
				}),
				Fields: map[string]any{
					"duration_seconds": int(s.CompletedAt.Sub(s.StartedAt).Seconds()),
					// Its position in the job, which is how a log reader finds
					// it and what orders two steps that finished in the same
					// second.
					"step_number": s.Number,
				},
				Time: s.CompletedAt,
			})
		}
	}
	return points, nil
}

// stepsWithheld reports whether a job's empty steps list is GitHub's
// retention rather than a count, in which case the job writes no steps field.
//
// GitHub stops serving a job's steps long before the job itself: measured on
// 2026-09-24, every job of a run created before about 12 April listed its
// times, runner and conclusion over an empty steps list, and every later
// run's jobs listed theirs. A job that finished as success, failure or
// timed_out ran at least one step, so its empty list says nothing, and a zero
// written for it drags every mean of steps toward nothing as far back as a
// backfill reached. A skipped job runs none (measured) and a canceled one can
// stop before its first, so their zero may be the truth and is kept.
func stepsWithheld(conclusion string, listed int) bool {
	if listed > 0 {
		return false
	}
	switch conclusion {
	case "success", "failure", "timed_out":
		return true
	}
	return false
}

// Artifacts collects what the workflows left behind.
//
// Artifacts are the part of Actions that costs storage rather than minutes,
// and they expire silently. Each one is stamped at its creation so the chart
// shows what a given day's builds produced, with the retention it was actually
// given recorded as a field so a policy can be checked instead of assumed.
type Artifacts struct {
	// Walk bounds the list. Its default is five pages, five hundred
	// artifacts; a repository past that is reported honestly through the
	// `walked` field rather than silently under-counted. A backfill asks for
	// everything.
	Walk Walk
}

type artifactRow struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_in_bytes"`
	Expired   bool      `json:"expired"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Present on 1,250 of the 1,260 artifacts of this account; the ten without
	// it are from 2020 to 2022, before GitHub computed one.
	Digest   string `json:"digest"`
	Workflow struct {
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		ID         int64  `json:"id"`
	} `json:"workflow_run"`
}

func (a Artifacts) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	// Five pages, five hundred artifacts. Twenty pages across eighteen
	// repositories was three hundred and sixty requests an hour on its own,
	// which ate the rate budget the rest of the sweep needed. When a
	// repository has more, the `walked` field says so instead of the total
	// quietly being a floor.
	most := a.Walk.limit(5)

	var points []sink.Point
	var live int64
	var liveCount, walked, declared int

	// Paginated. Summing only the first hundred artifacts published a live
	// total of three megabytes next to a count of twenty-eight thousand, which
	// is not a small error but a wrong answer.
	for page := 1; page <= most; page++ {
		var res struct {
			TotalCount int           `json:"total_count"`
			Artifacts  []artifactRow `json:"artifacts"`
		}
		path := fmt.Sprintf("/repos/%s/actions/artifacts?per_page=100&page=%d", repo.FullName, page)
		if _, _, err := c.GetJSON(ctx, path, &res, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return points, err
		}
		declared = res.TotalCount
		if len(res.Artifacts) == 0 {
			break
		}
		walked += len(res.Artifacts)
		for i := range res.Artifacts {
			art := &res.Artifacts[i]
			if !art.Expired {
				live += art.SizeBytes
				liveCount++
			}
			fields := map[string]any{
				"size_bytes": art.SizeBytes,
				"run_id":     art.Workflow.ID,
				"head_sha":   art.Workflow.HeadSHA,
				// Whether GitHub still holds the file. A field and not a tag,
				// because the row is dated when the artifact was created and
				// expiry comes later: as the tag `expired` it opened a second
				// series at the same instant the day the artifact expired,
				// and the two rows were summed as two artifacts for ever.
				// Measured on this account after eleven hours: 77 of 3,615
				// artifacts doubled, and a day's storage read 18 per cent
				// high. Under a new name so a database that already holds
				// the tag column keeps accepting writes.
				"live": !art.Expired,
				// The same unbounded value as on the run: one branch per pull
				// request, never reused. It was a tag and is now a field, and
				// under the API's own name for the same reason as there: the
				// old `branch` column of gh_artifact is a tag, and InfluxDB 3
				// would reject a field that reused the name.
				"head_branch": art.Workflow.HeadBranch,
			}
			// An artifact has no page on GitHub; the run that produced it
			// does, and that is where a reader wants to land.
			setNonEmpty(fields, "url", githubPage(repo.FullName, "actions", "runs",
				strconv.FormatInt(art.Workflow.ID, 10)))
			// The retention actually applied, which is the point of keeping
			// the expiry at all: measured on this account, 88 of 100 artifacts
			// live one day and 12 live seven, against a default setting of 90.
			// Rounded because GitHub sets expires_at a few seconds short of a
			// whole number of days.
			if !art.ExpiresAt.IsZero() && !art.CreatedAt.IsZero() {
				fields["retention_days"] = int(art.ExpiresAt.Sub(art.CreatedAt).Round(24*time.Hour) / (24 * time.Hour))
			}
			// Two builds of the same commit that produce the same digest is
			// reproducibility measured rather than assumed.
			if art.Digest != "" {
				fields["digest"] = art.Digest
			}
			points = append(points, sink.Point{
				Measurement: "gh_artifact",
				Tags:        merge(base, map[string]string{"artifact": art.Name}),
				Fields:      fields,
				Time:        art.CreatedAt,
			})
		}
		if len(res.Artifacts) < 100 || a.Walk.past(res.Artifacts[len(res.Artifacts)-1].CreatedAt) {
			break
		}
	}

	// One current total, so a dashboard can show storage without summing a
	// window that would double-count artifacts still alive from earlier days.
	//
	// Three counts rather than one, because `live_bytes` and `count` are not
	// on the same denominator and a reader has no way to tell. `count` is
	// GitHub's own total and it counts expired artifacts: measured on
	// jmrplens/jmrp.io on 2026-09-17, page 40 of the listing was expired
	// artifacts to the last row, against a declared 29,405. `live_bytes` is
	// the size of the artifacts GitHub still holds, over the ones this walk
	// reached, which the page cap stops at five hundred. So the panel read
	// 11.5 GB beside a count of 29,361 with nothing saying the two are
	// counting different things. `live_count` gives the bytes the count they
	// are the size of, and `walked` against `count` says whether that is a
	// total or a floor.
	points = append(points, sink.Point{
		Measurement: "gh_artifact_total",
		Tags:        base,
		Fields: map[string]any{
			"live_bytes": live, "count": declared,
			// How many of the declared total were actually walked.
			"walked": walked,
			// The artifacts behind live_bytes: the walked ones GitHub has
			// not expired.
			"live_count": liveCount,
		},
		Time: now,
	})
	return points, nil
}
