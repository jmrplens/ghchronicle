package collect

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// The sweep's report on itself: which of its own collectors ran, over how many
// repositories, and which repositories each of them could not collect.
//
// gh_rate_limit was the only measurement the collector took of itself, and it
// answers one question: whether a family was skipped for want of budget. It
// does not answer the one that cost the most. Measured on the author's own
// store on 2026-09-16: gh_workflow_run and gh_workflow_job held nothing at all
// for his five busiest repositories, because each of them had met one
// transient 502 on one /repos/<repo>/actions/runs/<id>/jobs call, and the
// runner threw away everything that repository's actions family had already
// collected. The continuous integration row of the dashboard was computed over
// an account missing its five busiest repositories, the cost row on the same
// page reported one of them burning 27.6 K macOS minutes, and nothing on the
// page or in the store said which of the two was wrong. The only record was
// one ERROR line in a journal, hours old, that read like every other line.
//
// So the sweep now writes down what it did. One row per family it ran, always,
// whether or not anything failed, and one more row per repository it could not
// collect. The rows that always arrive are what makes the absence of the
// others mean something: a family with no failure row and no family row has
// not run, which a reader of an empty panel could not tell from a family that
// ran perfectly.
//
// Written every sweep and never accumulated: each row is that sweep's answer,
// dated at the sweep, so a range holds the history and the newest rows hold
// the present.

// FamilyRun is one family's pass in one sweep.
//
// Repos is how many repositories it was asked about, which is zero for a
// family that asks about the account rather than about repositories. Failures
// are the repositories it could not collect; Err is a failure that belongs to
// no repository in particular, which is either an account-wide family's own
// error or the batched query some per-repository families begin with.
type FamilyRun struct {
	Family string
	Repos  int
	// Failed is the runner's own count of what did not come back, which is
	// the number that decides whether the family is marked as having run.
	// It is not len(Failures): a batched query that failed counts every
	// repository it covered and names none of them, and a budget spent
	// mid-family counts the whole list.
	Failed   int
	Points   int
	Err      error
	Failures []RepoFailure
}

// RepoFailure is one repository one family could not collect, and what
// stopped it.
type RepoFailure struct {
	Repo Repo
	Err  error
}

// The two values of the scope tag, which is what tells the row about a family
// from the rows about the repositories it failed on. They sit in one
// measurement rather than in two because a measurement is a table and a table
// that has never been written does not exist: a panel selecting from it is
// refused by InfluxDB outright rather than drawn empty, and on an account
// where nothing has ever failed that would be for ever. The family rows are
// written every sweep, so the table and every column of it exist from the
// first one, and a sweep where nothing failed draws an empty list rather than
// an error.
const (
	scopeFamily = "family"
	scopeRepo   = "repo"
)

// CollectorPoints is what the sweep says about itself, one row per family and
// one per repository a family could not collect.
func CollectorPoints(runs []FamilyRun, now time.Time) []sink.Point {
	points := make([]sink.Point, 0, len(runs))
	for _, run := range runs {
		points = append(points, sink.Point{
			Measurement: "gh_collector_family",
			Tags: merge(repoTags("", ""), map[string]string{
				"family": run.Family, "scope": scopeFamily,
				"reason": FailureReason(run.Err),
			}),
			Fields: map[string]any{
				"repos": run.Repos, "failed": run.Failed, "points": run.Points,
				"error": errorText(run.Err),
			},
			Time: now,
		})
		for _, f := range run.Failures {
			points = append(points, sink.Point{
				Measurement: "gh_collector_family",
				Tags: merge(repoTags(f.Repo.Owner, f.Repo.Name), map[string]string{
					"family": run.Family, "scope": scopeRepo,
					"reason": FailureReason(f.Err),
				}),
				Fields: map[string]any{"failed": 1, "error": errorText(f.Err)},
				Time:   now,
			})
		}
	}
	return points
}

// errorText is the message a row carries, and the sentinel where there is no
// message to carry.
//
// A field and not a tag, because the message names the request and the request
// carries a run id and a query string, which as a tag is one series per failed
// call for ever. The sentinel is what a tag would have written anyway, and here
// it is load-bearing for a different reason: sink.LineProtocol drops an empty
// string field, so a column only ever written on a failure would not exist at
// all on an account where nothing has failed, and InfluxDB refuses a query that
// names a column it has never seen rather than answering it with no rows. The
// panel that lists the failures would then be broken for exactly the readers
// who have nothing to fix.
func errorText(err error) string {
	if err == nil {
		return noneTag
	}
	return err.Error()
}

// FailureReason is what stopped a collector, in the few words a reader can
// group by.
//
// Bounded on purpose, and it is the same reasoning that keeps a workflow's
// dynamic title out of the `workflow` tag: the message names the request, so
// as a tag it is one series per failed call for ever. The status is the
// question a reader actually asks of this column, and "502" answers it: a
// gateway that will be fine next sweep reads differently from a 404 that
// never will be.
func FailureReason(err error) string {
	if err == nil {
		return noneTag
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	if _, ok := errors.AsType[*ghapi.RateLimitedError](err); ok {
		return "rate limited"
	}
	if _, ok := errors.AsType[*ghapi.NotReadyError](err); ok {
		return "not ready"
	}
	if _, ok := errors.AsType[*ghapi.TooLargeError](err); ok {
		return "query too large"
	}
	if e, ok := errors.AsType[*ghapi.UnavailableError](err); ok {
		return strconv.Itoa(e.Status)
	}
	if e, ok := errors.AsType[*ghapi.StatusError](err); ok {
		return strconv.Itoa(e.Code)
	}
	// Everything GitHub answered with a shape of its own is above. What is
	// left is a transport error, a body that would not parse, or a GraphQL
	// errors array, and none of those carries a number to group by.
	return "other"
}
