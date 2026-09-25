package run

import (
	"context"
	"errors"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// audience is the batched half of the stars and forks families: the newest
// hundred of the list for every repository already walked, one GraphQL point
// per ten repositories instead of one REST round trip per repository. Nothing
// on a backfill, which walks the REST lists page by page.
func (r *Runner) audience(ctx context.Context, family string, now time.Time) ([]sink.Point, error) {
	a := collect.Audience{Repos: r.batchedRepos(family), Stars: family == "stars", Forks: family == "forks"}
	if family == "forks" {
		// Rebuilt by every forks batch, so a repository that fell back under
		// a hundred forks stops being walked, and one the batch could not
		// ask this sweep is not walked on the strength of the last.
		r.forkOverflow = map[string]bool{}
		a.Overflow = func(repo collect.Repo) { r.forkOverflow[repo.FullName] = true }
	}
	return a.Collect(ctx, r.API, now)
}

// audienceWalk is the per-repository half of the same two families: the REST
// walk of the list, for the repositories the batch does not serve, and for
// stars the daily star history of every repository, served by the batch or
// not.
//
// The history is read everywhere because it is where the dated star counts
// come from. Since July 2026 GitHub gives the stargazer list only to a
// repository's admins and collaborators, so a repository the token merely
// reads answers the list with a 404 and the batch with an empty connection,
// and its stars would be in no dated count at all. The history answers
// anyone who can read the repository, and it costs one request a sweep, most
// of them a 304 that GitHub does not charge.
func (r *Runner) audienceWalk(ctx context.Context, family string, repo collect.Repo, now time.Time) ([]sink.Point, error) {
	// Asked once, before the walk: FirstSight below changes the answer.
	rest := r.walksInREST(family, repo)
	if family == "forks" {
		if !rest {
			return nil, nil
		}
		return collect.Forks{Walk: r.walk()}.Collect(ctx, r.API, repo, now)
	}
	var list []sink.Point
	var listErr error
	if rest {
		// The full paginated walk happens the first time a repository is seen
		// and never again: after that only the last page can have changed.
		full := r.State.FirstSight(repo.FullName, now)
		list, listErr = collect.Stargazers{Full: full}.Collect(ctx, r.API, repo, now)
	}
	history, historyErr := r.starHistory(ctx, repo, now)
	if historyErr != nil && !rest && r.batchFailed(family) && !rateLimited(historyErr) {
		// This sweep's batch failed and collectFamily has counted it already,
		// against every repository it served or as the one thing that failed
		// when some of them answered. Returning this error would count the
		// same repository a second time, and a family whose one repository
		// failed twice reads as one where not every repository failed: it
		// would be marked as run with nothing collected. So it is reported
		// here, the way collectFamily reports it, and not counted again. A
		// spent budget is the exception, because collectFamily stops the
		// family on it and has to see it to do so.
		r.Log.Error("collector failed", "family", family, "repo", repo.FullName, "err", historyErr)
		r.noteRepoFailure(family, repo, historyErr)
		historyErr = nil
	}
	// Both halves' rows whichever of them failed: the list's rows are not made
	// untrue by the history failing, nor the other way round, and
	// collectFamily keeps whatever a repository gathered beside its error.
	return append(list, history...), errors.Join(listErr, historyErr)
}

// starHistory reads one repository's daily star history: back to its first
// week the first time, and after that the newest page on a sweep and back to
// BackfillSince on a backfill. The whole walk is recorded only once it reached
// the end of the history, so one cut short is walked whole again by the next
// sweep instead of leaving the older weeks unread until a backfill. Cut short
// is a 502 and it is also an answer that there is nothing here: a 403 past
// page one is a secondary limit that came without its headers, and a 404 on
// page one is what every repository answers on a GitHub Enterprise Server
// that does not serve the history. Walking such a repository whole again
// costs the one request a sweep makes of it anyway.
func (r *Runner) starHistory(ctx context.Context, repo collect.Repo, now time.Time) ([]sink.Point, error) {
	walk, whole := r.walk(), r.State.HistoryDue(repo.FullName)
	if whole {
		walk = collect.Unbounded
	}
	points, complete, err := collect.StarHistory{Walk: walk}.Read(ctx, r.API, repo, now)
	if whole && complete {
		r.State.MarkHistory(repo.FullName, now)
	}
	return points, err
}

// batchFailed reports whether this sweep's batch of family has failed, which
// collectFamily records on the family's own row before it walks a single
// repository, and nothing else records there until the walk is over.
func (r *Runner) batchFailed(family string) bool {
	run := r.health.runs[family]
	return run != nil && run.Err != nil
}

// rateLimited reports whether err is the budget running out.
func rateLimited(err error) bool {
	_, limited := errors.AsType[*ghapi.RateLimitedError](err)
	return limited
}

// walksInREST reports whether this sweep reads a repository's star or fork
// list page by page through REST rather than in the batch, which only serves
// the newest hundred: a backfill always; for stars, a repository never seen
// before, whose whole history is read once; for forks, the first sweep of a
// fresh install, which is the one time a repository with more than a hundred
// forks has rows the batch would never reach, and any repository the batch
// of this sweep found holding more than that hundred, whose older forks the
// walk keeps refreshing as it did before the batch.
func (r *Runner) walksInREST(family string, repo collect.Repo) bool {
	if r.Backfill {
		return true
	}
	switch family {
	case "stars":
		_, seen := r.State.FirstSaw[repo.FullName]
		return !seen
	case "forks":
		_, ran := r.State.LastRun[family]
		return !ran || r.forkOverflow[repo.FullName]
	}
	return false
}

// batchedRepos is the repositories the batch of family serves this sweep:
// every one whose list the REST walk is not reading. Decided before the
// batch runs, so the overflow it reports adds a walk without taking a
// repository out of it.
func (r *Runner) batchedRepos(family string) []collect.Repo {
	var repos []collect.Repo
	for _, repo := range r.repos {
		if !r.walksInREST(family, repo) {
			repos = append(repos, repo)
		}
	}
	return repos
}

// batched reports how many repositories a failed batch of family took with
// it: every one of them for a family that collects nowhere else, the ones
// the batch was serving for stars and forks, and none for a family whose
// per-repository part reads the same thing again. It is what collectFamily
// counts as failed when the batch fails, so a sweep in which the batch was
// the whole family is left unmarked the way a family that failed on every
// repository is.
//
// Stars still counts the repositories it served although each of them also
// has its daily history read, because the names of their newest stars are
// what the batch reads and nothing else does; a history that then fails for
// one of them is kept from counting it twice by audienceWalk.
func (r *Runner) batched(family string) int {
	switch {
	case batchOnlyFamilies[family]:
		return len(r.repos)
	case family == "stars" || family == "forks":
		return len(r.batchedRepos(family))
	}
	return 0
}
