package run

import (
	"context"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
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
// walk, for the repositories the batch does not serve, and nothing for the
// rest.
func (r *Runner) audienceWalk(ctx context.Context, family string, repo collect.Repo, now time.Time) ([]sink.Point, error) {
	if !r.walksInREST(family, repo) {
		return nil, nil
	}
	if family == "forks" {
		return collect.Forks{Walk: r.walk()}.Collect(ctx, r.API, repo, now)
	}
	// The full paginated walk happens the first time a repository is seen and
	// never again: after that only the last page can have changed.
	full := r.State.FirstSight(repo.FullName, now)
	return collect.Stargazers{Full: full}.Collect(ctx, r.API, repo, now)
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
func (r *Runner) batched(family string) int {
	switch {
	case batchOnlyFamilies[family]:
		return len(r.repos)
	case family == "stars" || family == "forks":
		return len(r.batchedRepos(family))
	}
	return 0
}
