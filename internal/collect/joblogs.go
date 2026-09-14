package collect

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// JobLogs collects the text a failed job printed.
//
// This is the one thing here that is a log rather than a measurement, and it
// answers the question a chart never can: not "the build failed" but why. The
// duration of a failing job is a number; its last forty lines are the answer.
//
// Only failures, and only their tail. A successful job's output is thousands
// of lines nobody will read, GitHub deletes logs after ninety days anyway, and
// each one costs a request. Storing the tail of the failures is the whole
// value at a fraction of the cost.
type JobLogs struct {
	// Since bounds which runs are considered.
	Since time.Time
	// Tail is how many lines to keep from the end of each failed job. Zero
	// means 40.
	Tail int
	// MaxJobs caps how many failed jobs are fetched per repository per sweep.
	MaxJobs int
	// Walk bounds the failed-run list. GitHub keeps logs for ninety days, so
	// walking further is paying for 410s.
	Walk Walk
}

// jobRef is the part of a run's job listing this collector needs: which job it
// was, and when it finished, which is the date a log line falls back to when
// its own timestamp will not parse.
type jobRef struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	CompletedAt time.Time `json:"completed_at"`
}

func (j JobLogs) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	tail := j.Tail
	if tail <= 0 {
		tail = 40
	}
	maxJobs := j.MaxJobs
	if maxJobs <= 0 {
		maxJobs = 10
	}
	runs, err := j.failedRuns(ctx, c, repo)
	if err != nil {
		return nil, err
	}

	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	var points []sink.Point
	fetched := 0
	for i := range runs {
		run := &runs[i]
		if !j.Since.IsZero() && run.UpdatedAt.Before(j.Since) {
			continue
		}
		if fetched >= maxJobs {
			break
		}
		jobs, listErr := failedJobs(ctx, c, repo, run.ID)
		if listErr != nil {
			return points, listErr
		}
		for k := range jobs {
			if fetched >= maxJobs {
				break
			}
			fetched++
			pts, logErr := logTailPoints(ctx, c, repo, run, &jobs[k], base, tail)
			if logErr != nil {
				return points, logErr
			}
			points = append(points, pts...)
		}
	}
	return points, nil
}

// failedRuns lists the runs that failed, newest first.
//
// Asking GitHub for the failures rather than filtering here: it is one query
// parameter and it saves walking pages of successes.
//
// runRow is the same type actions.go decodes, because this is the same
// endpoint with a status filter. Sharing it is what keeps the two measurements
// taggable by the same identity: a second local struct is how gh_job_log came
// to tag the run's name long after gh_workflow_run stopped.
func (j JobLogs) failedRuns(ctx context.Context, c *ghapi.Client, repo Repo) ([]runRow, error) {
	var runs []runRow
	most := j.Walk.limit(1)
	for page := 1; page <= most; page++ {
		var batch struct {
			Runs []runRow `json:"workflow_runs"`
		}
		path := fmt.Sprintf("/repos/%s/actions/runs?status=failure&per_page=100&page=%d%s",
			repo.FullName, page, j.createdFilter())
		if _, _, err := c.GetJSON(ctx, path, &batch, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return nil, err
		}
		runs = append(runs, batch.Runs...)
		if len(batch.Runs) < 100 || j.Walk.past(batch.Runs[len(batch.Runs)-1].UpdatedAt) {
			break
		}
	}
	return runs, nil
}

// rerunReach is how long before it finished a failure may have been created.
// GitHub lets a run be re-run for thirty days after its first attempt, and a
// re-run keeps the run's created_at: the newest failure of this account's
// busiest repository, measured on 2026-09-11, was the third attempt of a run
// created two hours before it finished, and a run re-run on the last day
// allowed may still take a day to fail. A margin the length of a job would
// have missed every re-run of a failure older than a morning.
const rerunReach = 31 * 24 * time.Hour

// createdFilter is the query that asks GitHub for the failures created since
// a re-run could still reach, rather than for its whole failure history.
//
// Without it the listing is the newest hundred failures the repository ever
// had, six hundred kilobytes a repository, for a window of an hour that is
// nearly always empty; with it, a month of failures, which on the busiest
// repository measured was sixty eight rows, a megabyte decompressed and sixty
// one kilobytes on the wire, and on most repositories a few rows or none.
// The bound is rounded down to the day so the URL, and with it the ETag,
// stays the same across the sweeps of that day: every distinct URL is a fresh
// 200 charged to the budget, forty eight a day per repository unrounded, one
// rounded, and within the day the page changes only when a failure is created
// or re-run, which is exactly when there is something to fetch. Collect still
// cuts at Since exactly, by when the run finished; the filter only decides
// what is worth downloading.
func (j JobLogs) createdFilter() string {
	if j.Since.IsZero() {
		return ""
	}
	from := j.Since.Add(-rerunReach).UTC().Truncate(24 * time.Hour)
	return "&created=" + url.QueryEscape(">="+from.Format(time.RFC3339))
}

// failedJobs is the jobs of one run that failed. A run whose job listing is
// gone answers with none, which is not a failure of the sweep.
func failedJobs(ctx context.Context, c *ghapi.Client, repo Repo, runID int64) ([]jobRef, error) {
	var res struct {
		Jobs []jobRef `json:"jobs"`
	}
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?per_page=100", repo.FullName, runID)
	if _, _, err := c.GetJSON(ctx, path, &res, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	var failed []jobRef
	for _, job := range res.Jobs {
		if job.Conclusion == "failure" {
			failed = append(failed, job)
		}
	}
	return failed, nil
}

// logTailPoints turns the tail of one failed job's log into one point per
// line. A log GitHub has already deleted yields none.
func logTailPoints(ctx context.Context, c *ghapi.Client, repo Repo, run *runRow, job *jobRef,
	base map[string]string, tail int,
) ([]sink.Point, error) {
	text, err := c.GetText(ctx, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo.FullName, job.ID))
	if err != nil {
		if isSkippable(err) {
			return nil, nil // GitHub deletes logs after ninety days
		}
		return nil, err
	}
	var points []sink.Point
	for _, line := range lastLines(text, tail) {
		at, msg := splitLogLine(line)
		if at.IsZero() {
			at = job.CompletedAt
		}
		points = append(points, sink.Point{
			Measurement: "gh_job_log",
			Tags: merge(base, map[string]string{
				// The workflow file path, through the same helper
				// gh_workflow_run uses. The run's name is the pull
				// request title on a dynamically named run, so
				// tagging it minted a series per pull request here
				// too, and it left the two measurements keyed on
				// values that could not be joined.
				"workflow": workflowTag(run), "job_name": job.Name,
				// A series per run, which a log line genuinely is:
				// this is the only thing that says which run printed
				// it, and the family is off by default.
				"run": strconv.FormatInt(run.ID, 10),
			}),
			Fields: map[string]any{
				"line": msg,
				// Was a tag, and unbounded for the reason the run's
				// own branch was demoted: every pull request and
				// every Dependabot bump mints a name that is never
				// reused. Published under the API's own name, as on
				// gh_workflow_run, because InfluxDB 3 fixes a column
				// as a tag or a field the first time it sees it and
				// rejects every later write that disagrees, so
				// reusing "branch" would have meant dropping the
				// table (docs/sinks.md).
				"head_branch": run.Branch,
			},
			Time: at,
		})
	}
	return points, nil
}

// lastLines keeps the tail, which is where a failure explains itself.
func lastLines(text string, n int) []string {
	// GitHub writes a byte order mark before the first timestamp, which would
	// otherwise make the first line the only one that fails to parse its date.
	text = strings.TrimPrefix(text, "\ufeff")
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ring := make([]string, 0, n)
	for sc.Scan() {
		line := stripANSI(strings.TrimRight(sc.Text(), "\r"))
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	return ring
}

// stripANSI removes the color codes Actions writes into its logs.
//
// They are noise in a log store, they make a search for a word fail when the
// word happens to be colored, and the escape character itself is a control
// byte that has no business in a stored field.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		// Skip to the end of the sequence: ESC [ params letter.
		i++
		if i < len(s) && s[i] == '[' {
			i++
			for i < len(s) && (s[i] == ';' || (s[i] >= '0' && s[i] <= '9')) {
				i++
			}
		}
		// i now points at the final letter, which the loop increment drops.
	}
	return b.String()
}

// splitLogLine peels off the RFC 3339 timestamp GitHub puts at the start of
// every line, so the log lands in the store at the moment it was printed
// rather than at the moment it was read.
func splitLogLine(line string) (at time.Time, text string) {
	space := strings.IndexByte(line, ' ')
	if space <= 0 || space > 40 {
		return time.Time{}, line
	}
	at, err := time.Parse(time.RFC3339Nano, line[:space])
	if err != nil {
		return time.Time{}, line
	}
	return at, line[space+1:]
}
