package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Runner owns one sweep of every collector that is due.
type Runner struct {
	Cfg   *config.Config
	API   *ghapi.Client
	Sinks []sink.Sink
	State *State
	Log   *slog.Logger

	// Backfill reaches as far back as each surface allows and waits for the
	// rate limit to reset rather than stopping when the reserve is reached.
	//
	// A normal sweep is an increment and must never block: it protects the
	// reserve so whatever else uses the token keeps working, and skips a
	// family rather than sleeping. A backfill is the opposite intention. It is
	// run deliberately, once, and the only thing that matters is that it
	// finishes, so it parks until the window turns over. GitHub tells it
	// exactly when that is in every response.
	Backfill bool
	// BackfillSince bounds a backfill by date. Zero means as far as the API
	// goes, however long that takes.
	BackfillSince time.Time

	// Progress is where this backfill writes down what it has already
	// delivered, so that a stop costs one repository instead of the walk. Nil
	// is a run that keeps no checkpoint, which every sweep is: see Progress
	// for why a walk and a sweep do not share one.
	Progress *Progress
	// progressWarned is whether a checkpoint that cannot be written has
	// already been reported. A disk that refuses one save refuses the next
	// fifteen hundred, and the walk goes on either way.
	progressWarned bool

	// Prime makes the first sweep after start-up run every enabled family,
	// whatever the state file says they last did.
	//
	// An exporter holds its samples in memory, so a restart empties it and it
	// stays empty until each family's cadence comes round, which for the
	// twelve hour ones is half a day of a dashboard reading zero. Paying for
	// one full sweep is the cheaper mistake.
	Prime bool

	// Card says this sweep draws a card, which runs every enabled family the
	// way Prime does.
	//
	// A card is drawn from the points of this one sweep and from nothing else,
	// so a family skipped as not due is not a saving, it is a zero on the
	// card: a card sharing a state file with the sweep before it came out
	// with a zero in every number. Every number the card shows is asked for
	// again, which is one sweep's worth of quota for a picture that is
	// redrawn whole each time.
	Card bool

	// CardOnly says the points of this sweep reach the card and no store,
	// which is what -card-only asks for. Such a sweep saves nothing to the
	// state file.
	//
	// Every field in State is a claim that something has already been
	// delivered somewhere, and a card-only sweep delivers to nobody, so
	// writing any of them would make the next real collection skip or narrow
	// a read whose data went into a picture and nowhere else. Field by field:
	// last_run would make a family not due and skip it outright; first_saw
	// would retire the one-off full walk of a repository's star history, which
	// no later sweep does again; last_head would move the dependency diff's
	// base past changes no range can name afterwards; last_full would spend
	// the day's whole-page read of the pull requests nobody touched;
	// last_notified would cut the inbox window past threads the stores never
	// saw; and last_event would stop the next feed read at an event they never
	// got. The state file is still read: what it remembers only makes this
	// sweep cheaper, never less complete.
	CardOnly bool

	repos   []collect.Repo
	reposAt time.Time
	primed  bool
	prime   bool
	// archived is the archived repositories the filter set aside, from the
	// same listing as repos. A sweep's totals family dates each one's archive
	// and asks nothing else about it; a backfill has none, because it
	// collects them.
	archived []collect.Repo

	// counts is what the totals family last said each repository holds, and
	// is what sizes the pull request page. Memory only: a restart runs totals
	// on its first sweep, before any per-repository family.
	counts map[string]collect.ItemCounts

	// expanded is every workflow run attempt whose jobs a sweep of this
	// process already wrote, so the next sweep does not list them again.
	// Memory only, like the ETag cache: after a restart the first sweep
	// lists the jobs of the newest runs once and then stops asking.
	expanded map[collect.RunKey]struct{}

	// refusals is, per family, the endpoints that answered 403 or 404 and
	// when to ask them again. Memory only, for the same reason as the two
	// above: see refusalsFor.
	refusals map[string]*collect.Refusals

	// health is what this sweep has learned about its own collectors, which
	// it writes as gh_collector_family on the way out. Per sweep: see
	// health.go.
	health health

	// Now is the clock the sweep reads, for the one test that needs two of
	// them. Nil is time.Now, which is what everything but that test uses.
	Now func() time.Time

	// filtered counts, per sink name, the points that sink was offered and
	// did not write. It is never reset, so the total printed at the end of
	// each sweep is the count since the run started, which is how the ledger's
	// own total beside it reads too: the per-family lines say where, and this
	// says how much.
	filtered map[string]uint64

	// markupWarned is whether the achievements page has already been
	// reported as changed, so a redesign is one line in the log and not one
	// per sweep for as long as the parser lags.
	markupWarned bool

	// achievementsSaid is every warning the achievements family has already
	// given, by its text: a count that is a floor and a tier the page
	// disagrees with are true for weeks, and one line each is what the log
	// needs to say so.
	achievementsSaid map[string]bool

	// forkOverflow is every repository the forks batch of this sweep found
	// holding more forks than the hundred it reads, which the REST walk then
	// takes as it did before the batch. Rebuilt by every forks batch.
	forkOverflow map[string]bool
}

// walk is the pagination bound for the current sweep: the collector's own
// default normally, and everything back to BackfillSince during a backfill.
func (r *Runner) walk() collect.Walk {
	if !r.Backfill {
		return collect.Walk{}
	}
	return collect.Walk{Pages: -1, Since: r.BackfillSince}
}

// discoverInterval bounds how often the repository list is rebuilt. Repos are
// created rarely and listing them costs a page per hundred, so once an hour is
// generous.
const discoverInterval = time.Hour

// discoverFamily is the name the repository listing reports itself under on
// the collector's own row when it fails.
//
// Not a family of the configuration and it cannot be one: nothing sets its
// cadence and nothing may switch it off. It is on that row with them because
// it is the one collector every family depends on: a sweep that cannot list
// the repositories runs none of them, and without a row of its own the page
// would show sixteen families that never ran and no reason for any of it.
//
// Only when it fails, which is the one place this measurement does not need
// the row that always arrives. A listing that worked is already stated by
// every other row of the sweep, since no family could have run without it, so
// a heartbeat here would be one row every fifteen minutes saying what the
// rows beside it already say. What was ambiguous is the sweep with no family
// row at all, and that is exactly the case this fills in: nothing was due, or
// the listing failed and here is why.
const discoverFamily = "discover"

// PrimeAgain makes the next pass run every family again, whatever the state
// file says they last did.
//
// Priming is spent on the first pass of a process, which is what the long
// running service wants: fill the exporter at start-up, then follow the
// cadences. A backfill going back for what a pass left behind needs it back,
// because that pass marked the families it ran, and a second pass honoring the
// cadence would skip the very family it came back for and quietly do nothing.
//
// Between the passes of one walk and nowhere else. Priming every pass of any
// run with Backfill set is the wider rule, and it is wrong: a sweep that turns
// Backfill on after collecting would re-walk every family instead of the ones
// it came for, which is what TestStarsAndForksAreBatchedOnceWalked walked into.
func (r *Runner) PrimeAgain() { r.primed = false }

// Once runs every family whose interval has elapsed.
//
// A family that fails is logged and skipped; the sweep continues. One
// repository with a broken feature must not stop the other forty. Only a
// canceled context and a failed discovery stop it, and both return without
// marking anything as run.
func (r *Runner) Once(ctx context.Context) error {
	now := r.clock()
	r.prime = (r.Prime || r.Card) && !r.primed
	r.primed = true
	if r.prime {
		why := "first sweep after start-up, running every family to fill the exporter"
		switch {
		case r.Backfill:
			why = "running every family, whatever the state file says they last did"
		case r.Card:
			why = "running every family, whatever the state file says they last did, because the card shows all of them"
		}
		r.Log.Info(why)
	}
	r.openProgress(now)
	r.beginHealth()
	err := r.sweep(ctx, now)
	// On the way out whatever happened, and not only when the sweep reached
	// the end of it. The rule this measurement is read by is that a family
	// with no row did not run, so a sweep that listed its repositories, ran
	// its account families and was then cut short by a discovery that failed
	// or by a shutdown would leave that sentence saying something false about
	// the families that did run. emitHealth detaches from this context for
	// the shutdown half of that.
	r.emitHealth(ctx, now)
	if err != nil {
		r.closeProgress(ctx, err)
		return err
	}
	r.finish()
	r.closeProgress(ctx, nil)
	return nil
}

// sweep is the collection itself: the repository list, the account-wide
// families and then the per-repository ones. Separate from Once so that every
// way it can end passes through the same place on the way out.
func (r *Runner) sweep(ctx context.Context, now time.Time) error {
	if err := r.discoverRepos(ctx, now); err != nil {
		return err
	}
	r.accountFamilies(ctx, now)
	return r.repoFamilies(ctx, now)
}

// clock is the instant a sweep dates itself by. It is the wall clock, and it
// is a field so that one test can run the same sweep twice under two clocks
// and hold every row dated in the past to saying the same thing both times.
// Nothing in production sets it.
func (r *Runner) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// discoverRepos rebuilds the repository list when it has gone stale, and
// leaves the previous one in place otherwise.
func (r *Runner) discoverRepos(ctx context.Context, now time.Time) error {
	if r.repos != nil && now.Sub(r.reposAt) <= discoverInterval {
		return nil
	}
	found, err := collect.Discover(ctx, r.API, &collect.Filter{
		User: r.Cfg.Targets.User, Orgs: r.Cfg.Targets.Orgs, Repos: r.Cfg.Targets.Repos,
		Exclude: r.Cfg.Targets.Exclude, IncludeForks: r.Cfg.Targets.IncludeForks,
		// A backfill is the one walk that wants the whole history, and an
		// archived repository has one: its pull requests, releases and runs
		// are as much the account's as a live repository's, they just never
		// move again, which is exactly why a sweep leaves them out. Forks
		// stay as configured either way.
		IncludeArchived: r.Cfg.Targets.IncludeArchived || r.Backfill,
		IncludePrivate:  r.Cfg.Targets.PrivateIncluded(),
	})
	if err != nil {
		r.noteFamily(discoverFamily, 0, 1, 0)
		r.noteFamilyFailure(discoverFamily, err)
		return err
	}
	r.repos, r.archived, r.reposAt = found.Repos, found.Archived, now
	r.Log.Info("repositories discovered", "count", len(r.repos), "archived_aside", len(r.archived))
	return nil
}

// accountFamilies runs the families that ask about the account rather than
// about a repository: once per sweep, not once per repository.
//
// They need a login to ask about, so a configuration that names repositories
// alone has nothing to hand GraphQL and they are all skipped. r.family does
// that check; the line below says so once instead of eleven times.
func (r *Runner) accountFamilies(ctx context.Context, now time.Time) {
	user := r.Cfg.Targets.User
	if user == "" {
		r.Log.Debug("no targets.user, account-wide families skipped")
	}
	r.family(ctx, "account", now, func() ([]sink.Point, error) {
		return collect.Account{Login: user}.Collect(ctx, r.API, now)
	})
	// The lifetime numbers, asked of GitHub rather than added up here, so a
	// tile that wants "ever" reads one row instead of scanning a table.
	// The archived repositories set aside ride along here, for the one row
	// each has, the date it was archived, and every totals sweep asks it
	// again: one GraphQL query for all of them, at this family's cadence,
	// and the rows it rewrites are the same rows, so every store converges
	// on them the way it does on a collected archived repository's. Asking
	// once and remembering it was tried, and it is what the exporters cannot
	// hold: the Prometheus exporter drops a series not rewritten within a
	// day, and the OTLP state keeps the newest batch per series, so the count
	// of archived repositories would have expired, or become the one newly
	// archived, until the next restart.
	r.family(ctx, "totals", now, func() ([]sink.Point, error) {
		totals := collect.Totals{Login: user, Repos: r.repos, Archived: r.archived}
		points, err := totals.Collect(ctx, r.API, now)
		r.noteCounts(points)
		return points, err
	})
	// What the collector has left to spend. `GET /rate_limit` charges nothing,
	// and a family skipped for lack of budget is invisible without it.
	r.family(ctx, "ratelimit", now, func() ([]sink.Point, error) {
		return collect.RateLimit{}.Collect(ctx, r.API, now)
	})
	r.family(ctx, "events", now, func() ([]sink.Point, error) {
		return r.events(ctx, user, now)
	})
	r.family(ctx, "notifs", now, func() ([]sink.Point, error) {
		return r.notifications(ctx, now)
	})
	r.family(ctx, "billing", now, func() ([]sink.Point, error) {
		if r.Backfill {
			// Every month GitHub still has. It stops answering somewhere in
			// the past and the collector notices three empty months in a row.
			return collect.Billing{Login: user, Walk: r.walk()}.Collect(ctx, r.API, now)
		}
		// Two months, because the first days of a month still need last
		// month's rows to be complete before they stop changing.
		return collect.Billing{Login: user, Months: 2}.Collect(ctx, r.API, now)
	})
	r.family(ctx, "profile", now, func() ([]sink.Point, error) {
		return collect.Profile{Login: user, Walk: r.walk()}.Collect(ctx, r.API, now)
	})
	r.family(ctx, "outbound", now, func() ([]sink.Point, error) {
		return collect.Outbound{Login: user, Walk: r.walk()}.Collect(ctx, r.API, now)
	})
	// The whole green-squares history of every past year, for one point of
	// GraphQL each, and the year so far as a daily snapshot. Disabled by
	// default; setting `every.history` to any duration runs it, the past
	// years are there after the first sweep, and a daily cadence is what keeps
	// this year's row current.
	r.family(ctx, "history", now, func() ([]sink.Point, error) {
		return collect.History{Login: user}.Collect(ctx, r.API, now)
	})
	// The badges on the public profile page, which no API lists. The page
	// is read as an anonymous visitor beside the API, and a page GitHub has
	// redesigned is a *collect.MarkupError: said once, at warning, and the
	// family writes nothing until the parser catches up, because a number
	// read off the wrong element would look exactly like a right one.
	//
	// The progress rows beside the badges compare the tier a count implies
	// with the tier the page shows, and both what the walk could not settle
	// and a page that disagrees are said once per process: each stays true
	// for as long as the count stays, and the row carries it every day.
	r.family(ctx, "achievements", now, func() ([]sink.Point, error) {
		a := collect.Achievements{
			Login: user, WebBase: collect.WebBaseFor(r.Cfg.GitHub.BaseURL, r.Cfg.GitHub.WebURL),
			Warn: r.achievementsWarn,
		}
		points, err := a.Collect(ctx, r.API, now)
		if collect.IsMarkupError(err) {
			if !r.markupWarned {
				r.Log.Warn("achievements page changed, family writes nothing until the parser is updated", "err", err)
				r.markupWarned = true
			}
			return nil, nil
		}
		for _, d := range collect.Disagreements(points) {
			r.achievementsWarn("achievement tier disagrees with the profile page, its rule may have changed", "badge", d)
		}
		return points, err
	})
	// The account's own keys: which have never been used, and when the one
	// that signs every commit expires.
	r.family(ctx, "keys", now, func() ([]sink.Point, error) {
		return collect.Keys{Login: user}.Collect(ctx, r.API, now)
	})
}

// perRepoFamilies are the families that run once per repository, in the order
// a sweep works through them.
//
// Three of them collect nothing per repository at all: branches, deployments
// and policyfiles ask about every repository in one batched GraphQL query, the
// way family repo already asks for its detail. They are listed here rather
// than in accountFamilies because they need the repository list and not a
// login, and r.family returns early when targets.user is unset, which is a
// valid configuration: a config that names repositories alone would have left
// all three silently uncollected.
var perRepoFamilies = []string{
	"traffic", "repo", "branches", "stars", "issues", "issueevents", "actions",
	"artifacts", "security", "stats", "discussions", "commits", "activity",
	"analyses", "forks", "planning", "joblogs", "settings", "rulesets", "inventory",
	"deployments", "policyfiles", "deps",
}

// batchOnlyFamilies collect entirely in familyBatch and have no per-repository
// part, so a failed batch is the whole family failing rather than one
// repository of many.
var batchOnlyFamilies = map[string]bool{
	"branches": true, "deployments": true, "policyfiles": true,
}

// repoFamilies runs each due per-repository family over every repository and
// sends what it collected.
func (r *Runner) repoFamilies(ctx context.Context, now time.Time) error {
	for _, family := range perRepoFamilies {
		every, enabled := r.Cfg.Interval(family)
		if !enabled || (!r.prime && !r.State.Due(family, every, now)) {
			continue
		}
		// A family the interrupted walk finished is not run again. Its rows
		// are in the stores already, and the checkpoint only says so after
		// every one of them reached every sink. Said in the log, because a
		// family with no row of its own in gh_collector_family is read as one
		// that did not run, and in this process it did not.
		if done, already := r.Progress.Done(family); already {
			// Its row as well as the line. The panel's rule is that a family
			// with no row did not run, and this family did: in the process
			// this one resumes, with the counts that process recorded. Saying
			// nothing here makes a resumed backfill read as one that skipped
			// most of what it was asked for.
			r.noteFamily(family, done.Repos, 0, done.Points)
			r.Log.Info("family already written by the walk this resumes",
				"family", family, "repos", done.Repos, "points", done.Points)
			continue
		}
		if !r.awaitBudget(ctx, family) {
			continue
		}
		// Counted before the pass, because a resumed family covers the
		// repositories this pass walks and the ones an earlier process
		// already wrote. Reporting only this pass would tell the panel that a
		// family of thirty-five repositories covered the two that were left.
		written := len(r.repos) - r.reposToCover(family)
		pass, err := r.collectFamily(ctx, family, now)
		if err != nil {
			return err
		}
		r.emit(ctx, family, pass.points)
		r.noteFamily(family, pass.covered+written, pass.failed, pass.written)
		// A family where every repository failed has not run. Marking it would
		// hide the outage until its next cadence, which for the slow families
		// is half a day.
		//
		// Counted against the repositories this pass covers rather than against
		// every repository there is, because a resumed walk covers only the
		// ones the checkpoint does not already hold. Without a checkpoint the
		// two are the same number, and settled before the loop rather than by
		// it, so a pass that stopped early for want of budget is read as it
		// always was: some repositories failed, not all of them.
		if pass.failed > 0 && pass.failed == pass.covered {
			r.Log.Warn("family failed everywhere, not marking it as run", "family", family, "repos", pass.failed)
			continue
		}
		// Recorded complete only when every repository of this walk is
		// recorded for it, which is asked of the checkpoint rather than
		// counted off this pass. Counting would miss two cases that leave
		// repositories behind without a collector failing: a pass that stops
		// early for want of budget, and a sink that refused one repository's
		// rows. A family recorded complete is a family the resume never opens
		// again, so what it is recorded on has to be the thing itself.
		if pass.failed == 0 && r.everyRepoWritten(family) {
			// Every repository of the walk, not this pass's share: the
			// per-repository records already carry the points, and the count
			// is what the family covered.
			r.checkpointFamily(family, len(r.repos), 0)
		}
		// Marked after the repositories ran, so every one of them saw the
		// same answer to "is the whole page due" that the first did; and
		// only when every one of them answered, because the whole page is
		// the one read that rewrites an open pull request nobody touched,
		// and a repository it failed on would otherwise wait a day for the
		// next. Left unrecorded, the next sweep reads the whole page again,
		// which costs one more daily pass and loses nothing.
		if pass.failed == 0 && r.fullPassDue(family, now) {
			r.State.MarkFull(family, now)
		}
		r.State.Mark(family, now)
	}
	return nil
}

// reposToCover is how many repositories a family's pass is about to ask, which
// is all of them and, on a resumed walk, all of them but the ones already
// written.
func (r *Runner) reposToCover(family string) int {
	left := 0
	for _, repo := range r.repos {
		if !r.Progress.RepoDone(family, repo.FullName) {
			left++
		}
	}
	return left
}

// everyRepoWritten reports whether the checkpoint holds every repository of
// this walk for a family.
//
// True of a family with no per-repository part at all, and rightly: the loop
// still asks it about each repository, that part answers with nothing to do,
// and nothing to do is done. What such a family collects is in its batch,
// which a resume runs again.
func (r *Runner) everyRepoWritten(family string) bool {
	if !r.Progress.Active() {
		return true
	}
	for _, repo := range r.repos {
		if !r.Progress.RepoDone(family, repo.FullName) {
			return false
		}
	}
	return true
}

// familyPass is what one family's pass over the repositories came to.
//
// points is what the caller still has to send, and it is empty on a walk that
// keeps a checkpoint: such a pass sends each repository's rows itself, because
// the checkpoint it writes beside them is a claim that they have been sent.
// written counts the rows either way, so the family's own row in
// gh_collector_family says the same thing in both.
//
// covered is how many repositories this pass is about to ask about, which on a
// resumed walk is fewer than there are: the ones the checkpoint already holds
// are not among them. It is the denominator "every repository failed" is read
// against, and it is settled before the pass rather than counted by it, so a
// pass cut short for want of budget still reads as some repositories failing
// and not as all of them.
type familyPass struct {
	points  []sink.Point
	written int
	failed  int
	covered int
}

// collectFamily runs one family over every repository and reports what it
// collected along with how many repositories failed.
//
// An error return is a canceled sweep and nothing else: a repository that
// fails is counted and the loop goes on, because one repository with a broken
// feature must not stop the other forty.
func (r *Runner) collectFamily(ctx context.Context, family string, now time.Time) (familyPass, error) {
	pass := familyPass{covered: r.reposToCover(family)}
	// Some families begin with one batched query for every repository at
	// once, which is what turns a REST call per repository into a shared
	// GraphQL point. It is not a per-repository failure if it fails: the
	// loop below still runs.
	batch, batchErr := r.familyBatch(ctx, family, now)
	// Kept whichever way it went. A batched read covers fourteen
	// repositories at a time and the collectors hand back every repository
	// that answered beside the error, so the rows of the batches that
	// succeeded are as true as if none had failed.
	pass.written += len(batch)
	if r.Progress.Active() {
		// Sent here rather than with the rest of the family, so that nothing
		// the first checkpointed repository claims is still waiting in a
		// slice. The batch itself is never checkpointed: it is one query for
		// every repository at once, a resume runs it again, and running it
		// again rewrites the rows it wrote the first time.
		r.emit(ctx, family, batch, "part", "batch")
	} else {
		pass.points = append(pass.points, batch...)
	}
	if batchErr != nil {
		r.Log.Error("batched collector failed", "family", family, "err", batchErr)
		r.noteFamilyFailure(family, batchErr)
		pass.failed = batchFailures(batchAnswered(batch, batchErr), r.batched(family))
	}
	for _, repo := range r.repos {
		// A shutdown cancels the sweep. Without this every remaining
		// repository logs its own "context canceled" and the family is
		// then marked as done, which is exactly backwards.
		if ctx.Err() != nil {
			return pass, ctx.Err()
		}
		// Walked by the run this one resumes, and its rows delivered before it
		// was written down. Skipping it is the whole saving, and it costs
		// nothing: the checkpoint is only written after a repository's walk
		// reached the end of its own pagination, so there is no tail of it
		// left behind to lose.
		if r.Progress.RepoDone(family, repo.FullName) {
			continue
		}
		pts, err := r.repoFamily(ctx, family, repo, now)
		// Before the error is looked at, because what a collector gathered
		// before it failed is not made untrue by the call that failed after
		// it. This line is the defect the store showed: measured on the
		// author's own account on 2026-09-16, gh_workflow_run and
		// gh_workflow_job held nothing at all for his five busiest
		// repositories, each because one /repos/<repo>/actions/runs/<id>/jobs
		// call had answered 502 once, and every run and job that repository's
		// actions family had already collected was dropped with it. A retry
		// would have helped that 502 and nothing else; keeping what was
		// collected is right whatever failed, which is why it is this and not
		// a retry. The repository is still counted as failed below: half a
		// repository is not a repository that succeeded, and pass.failed is
		// what decides whether the family is marked as run and what the
		// collector's own rows report. It is also what keeps half a repository
		// out of the checkpoint: kept is not the same as complete, and only
		// what is complete may be skipped by a resume.
		pass.written += len(pts)
		if r.Progress.Active() {
			// Delivered now, and written down only when every sink took it.
			// A sink that refused leaves the repository unrecorded, so the
			// resume walks it again rather than believing a store holds rows
			// it never got.
			//
			// One write per repository rather than one per family is what
			// makes a record mean delivered, and it is not free in a store
			// that writes a file per partition per request. Measured against
			// InfluxDB 3 Core with the same rows written both ways: where the
			// repositories of a family share a timestamp bucket, as
			// gh_traffic's do, 826 rows went from 14 files to 280, twenty
			// times as many; where each repository has buckets of its own, as
			// gh_commit does, 11,800 rows came to the same 11,800 files
			// either way. So the multiplier is the number of snapshots a
			// family spans, applied only to the shared buckets, and the
			// families that take hours are the near-disjoint ones. It is paid
			// only by a backfill carrying a checkpoint; an ordinary sweep
			// gathers the family and writes it once.
			delivered := r.emit(ctx, family, pts, "repo", repo.FullName)
			if err == nil && delivered {
				r.checkpointRepo(family, repo.FullName, len(pts))
			}
		} else {
			pass.points = append(pass.points, pts...)
		}
		if err != nil {
			if limited, ok := errors.AsType[*ghapi.RateLimitedError](err); ok {
				// Not a failure of this repository: the budget ran out
				// mid-family. Stop here and leave the family unmarked so
				// the next sweep picks it up rather than skipping a day.
				r.Log.Warn("budget spent mid-family", "family", family,
					"at", repo.FullName, "resets", limited.Reset.Format(time.TimeOnly))
				r.noteFamilyFailure(family, err)
				pass.failed = pass.covered
				return pass, nil
			}
			pass.failed++
			r.Log.Error("collector failed", "family", family, "repo", repo.FullName, "err", err)
			r.noteRepoFailure(family, repo, err)
			continue
		}
		if !r.awaitBudget(ctx, family) {
			r.Log.Warn("stopping this family here", "family", family, "at", repo.FullName)
			break
		}
	}
	return pass, nil
}

// batchFailures is what a failed batch counts as: how many repositories it
// took with it, or one when some of them answered.
//
// The two are different questions and they used to be the same answer.
// Where the batch is the whole family, or the part of it these repositories
// were going to get, and no chunk of it answered, every repository it covered
// is lost: repoFamilies reads failed == len(repos) as "this family has not
// run" and leaves it unmarked, which is what stops an outage hiding until the
// family's next cadence, a whole day for two of the three.
//
// A batch that failed and still answered for some repositories is the other
// case, and counting it as every repository was a cost regression this branch
// introduced by itself: the batched collectors now report a chunk they lost
// instead of swallowing it, so a single repository failing its alias batch
// marked all fifty-nine as failed, left the family unmarked, and turned a
// daily family into a quarter-hourly one for as long as that repository stayed
// broken. It counts as one thing that failed, which is what r.family already
// counts an account-wide failure as and for the same reason: nothing in the
// error names a repository, and the family delivered some of what it was asked
// for. The failure is not lost by the family being marked; it is in the log
// and in the family's own row, with the reason that says which kind it was.
func batchFailures(answered bool, covered int) int {
	if answered {
		return 1
	}
	return covered
}

// batchAnswered reports whether the batched part of a family answered for any
// repository at all.
//
// Rows are not the test, and reading them as one was a corner that survived
// the first fix: a collector that legitimately writes nothing for a repository
// with nothing to report produces zero points from chunks that all answered
// perfectly. deployments is the live example, hourly, on an account that has
// never deployed anything, so its batch yields no rows on every sweep and one
// chunk failing would have left it due on every tick. The collectors say how
// many repositories were in the chunks that answered, through
// collect.PartialError, which is the question this is really asking.
func batchAnswered(points []sink.Point, err error) bool {
	if len(points) > 0 {
		return true
	}
	partial, ok := errors.AsType[*collect.PartialError](err)
	return ok && partial.Asked > 0
}

// finish saves what the sweep learned, unless it was a card-only sweep, which
// has nothing it may claim to have delivered, and says what it spent.
func (r *Runner) finish() {
	// Nothing a card-only sweep collected reached a store, so nothing it
	// learned may tell the next collection that it did. See CardOnly.
	if r.CardOnly {
		r.Log.Debug("card-only sweep, the state file is left as it was")
	} else if err := r.State.Save(); err != nil {
		r.Log.Warn("state not saved", "err", err)
	}
	for name, rate := range r.API.Rates() {
		r.Log.Info("rate budget", "bucket", name, "remaining", rate.Remaining, "limit", rate.Limit)
	}
	for _, s := range r.Sinks {
		if u, ok := s.(*sink.Unchanged); ok {
			r.Log.Info("points already written and not sent again",
				"sink", u.Name(), "skipped", u.Dropped())
		}
		if n := r.filtered[s.Name()]; n > 0 {
			r.Log.Info("points the sink did not write", "sink", s.Name(), "filtered", n)
		}
	}
	r.Log.Info("sweep finished")
}

// familyBatch runs the part of a family that asks about every repository in
// one query, or returns nothing for a family that has no such part.
func (r *Runner) familyBatch(ctx context.Context, family string, now time.Time) ([]sink.Point, error) {
	switch family {
	case "repo":
		return collect.RepoDetail{Repos: r.repos}.Collect(ctx, r.API, now)
	case "branches":
		// One row per live branch and how stale its tip is, fourteen
		// repositories to a query.
		return collect.Branches{Repos: r.repos}.Collect(ctx, r.API, now)
	case "deployments":
		// The one family here that wants the walk: a sweep takes the newest
		// page per repository, a backfill follows one cursor per repository
		// back to BackfillSince.
		return collect.Deployments{Repos: r.repos, Walk: r.walk()}.Collect(ctx, r.API, now)
	case "policyfiles":
		// No walk: one query reaches the birth of the repository, because
		// what it asks for is the last commit against each path.
		return collect.PolicyFiles{Repos: r.repos}.Collect(ctx, r.API, now)
	case "stars", "forks":
		// The newest hundred of every repository already walked, in one
		// query per ten; the REST walks below take the rest.
		return r.audience(ctx, family, now)
	}
	return nil, nil
}

func (r *Runner) repoFamily(ctx context.Context, family string, repo collect.Repo, now time.Time) ([]sink.Point, error) {
	switch family {
	case "traffic":
		return collect.Traffic{}.Collect(ctx, r.API, repo, now)
	case "repo":
		return collect.RepoCore{Walk: r.walk()}.Collect(ctx, r.API, repo, now)
	case "stars", "forks":
		// The REST walks: a repository's whole star history the first time
		// it is seen, the forks of a fresh install, and every backfill. The
		// batch above reads the newest hundred of both otherwise.
		return r.audienceWalk(ctx, family, repo, now)
	case "issues":
		return r.pulls(repo, now).Collect(ctx, r.API, repo, now)
	case "actions":
		return r.actions(now).Collect(ctx, r.API, repo, now)
	case "artifacts":
		return collect.Artifacts{Walk: r.walk()}.Collect(ctx, r.API, repo, now)
	case "security":
		return collect.Security{Walk: r.walk(), Refusals: r.refusalsFor(family)}.Collect(ctx, r.API, repo, now)
	case "stats":
		return collect.RepoActivity{}.Collect(ctx, r.API, repo, now)
	case "discussions":
		return r.discussionPoints(ctx, repo, now)
	case "commits":
		return r.commits(now).Collect(ctx, r.API, repo, now)
	case "activity":
		return collect.RepoActivityLog{Walk: r.walk()}.Collect(ctx, r.API, repo, now)
	case "analyses":
		return collect.Analyses{Walk: r.walk(), Refusals: r.refusalsFor(family)}.Collect(ctx, r.API, repo, now)
	case "planning":
		return collect.Planning{}.Collect(ctx, r.API, repo, now)
	case "issueevents":
		return r.issueEvents(now).Collect(ctx, r.API, repo, now)
	case "deps":
		return r.dependencies(ctx, repo, now)
	case "inventory":
		// Four core requests a repository a day: what a workflow's own token
		// may do, both secret stores, and whether GitHub's default code
		// scanning is set up.
		return collect.RepoInventory{Refusals: r.refusalsFor(family)}.Collect(ctx, r.API, repo, now)
	case "settings":
		return r.settings().Collect(ctx, r.API, repo, now)
	case "rulesets":
		// The newest page of every ruleset's changelog on a sweep, which is
		// every version any ruleset measured has; the walk only matters to a
		// backfill of a ruleset edited more than a hundred times.
		return collect.RulesetHistory{Walk: r.walk()}.Collect(ctx, r.API, repo, now)
	case "joblogs":
		return r.jobLogs(now).Collect(ctx, r.API, repo, now)
	}
	return nil, nil
}

// pulls is the pull request collector for this run.
//
// A backfill walks every page. A sweep reads what was updated since the sweep
// before it, twice the cadence back the way actions and commits do so a late
// sweep still overlaps, and further when the family last ran earlier than
// that, walking pages until the updatedAt ordering is past that mark. Once a
// day it reads a whole page with no bound instead: an open pull request
// nobody touches keeps its updatedAt, so nothing else would rewrite its
// seconds_open and mergeable. That is what the daily pass is for, and it is
// the only thing that waits for it; a close, a merge, a review or a comment
// moves updatedAt and is seen by the sweep that follows it.
//
// GitHub charges the query for the page asked for, not for what it returns:
// measured on 2026-09-11, fifty costs eight points on a repository with no
// pull requests at all. So the page is sized from the last lifetime totals
// where they are known, and never smaller than the repository, which is what
// makes the daily page exactly the page of fifty it replaces on every
// repository that had fewer than fifty. The totals are up to twelve hours
// old, so a repository that crossed a page size since they ran holds more
// than the page asked for: the daily pass is allowed one page more, which it
// only takes when the first came back full with more behind it. A page of
// fifty is never followed; fifty is where today's read stopped too.
func (r *Runner) pulls(repo collect.Repo, now time.Time) collect.Pulls {
	if r.Backfill {
		// Fifty, not the hundred this used to ask for. A pull request now
		// carries its review threads as well as its reviews and its
		// timeline, and measured on 2026-09-10 a hundred of them is an
		// HTML 502 from the gateway after ten seconds, three attempts out
		// of three. Pulls halves and retries on the same cursor, so the
		// hundred still completed, at one wasted ten second round trip
		// per page of every busy repository.
		return collect.Pulls{First: 50, Walk: r.walk()}
	}
	first := 50
	if counts, known := r.counts[repo.FullName]; known {
		first = collect.PageFor(counts.Most())
	}
	if r.fullPassDue("issues", now) {
		daily := collect.Pulls{First: first}
		if first < 50 {
			daily.Walk.Pages = 2
		}
		return daily
	}
	every, _ := r.Cfg.Interval("issues")
	since := now.Add(-2 * every)
	if last, ran := r.State.LastRun["issues"]; ran && last.Before(since) {
		since = last.Add(-every)
	}
	// Ten at a time, because ten is what a repository touches in two hours
	// and costs two points against the eight of fifty; the walk goes on to
	// the next page whenever more than ten were, so a mass relabel is a
	// longer walk rather than a lost row.
	return collect.Pulls{First: min(first, 10), Walk: collect.Walk{Pages: -1, Since: since}}
}

// discussionPoints collects the discussions of a repository whose forum is
// on, and nothing from one whose forum is off. The query costs the same
// whether or not there is a forum to page, and the listing already said there
// is none; a forum switched on is seen the next time the list is rebuilt,
// within the hour.
func (r *Runner) discussionPoints(ctx context.Context, repo collect.Repo, now time.Time) ([]sink.Point, error) {
	if !repo.HasDiscussions {
		return nil, nil
	}
	return r.discussions().Collect(ctx, r.API, repo, now)
}

// discussions is the discussions collector for this run: the ten most recently
// updated threads on a sweep, at a cost of two points instead of eleven, and
// fifty on a backfill, with the walk. Both read twenty comments and twenty
// replies per thread, the page this asked for before the round: comments come
// oldest first, so ten of them would not be the ten newest but the ten
// oldest, and a thread past its tenth comment would stop being recorded.
func (r *Runner) discussions() collect.Discussions {
	d := collect.Discussions{Login: r.Cfg.Targets.User, Comments: 20, Replies: 20}
	if r.Backfill {
		d.First, d.Walk = 50, r.walk()
		return d
	}
	d.First = 10
	return d
}

// fullPassDue reports whether this sweep of family reads a whole page rather
// than what changed. Only issues has the two shapes.
//
// Due once per UTC day, not once every twenty-four hours. An open pull
// request nobody touches is stamped at the start of the UTC day and this pass
// is the only read that rewrites it, so the day that matters is the one the
// row is stamped at. Twenty-four hours after the last pass is not that: an
// hourly family slips a tick now and then and the pass drifts later with it,
// and the day it drifted across midnight would hold no row at all for any
// untouched open pull request.
func (r *Runner) fullPassDue(family string, now time.Time) bool {
	if family != "issues" {
		return false
	}
	last, ok := r.State.LastFull[family]
	return !ok || !sameUTCDay(last, now)
}

// sameUTCDay reports whether a and b fall on the same UTC calendar day.
func sameUTCDay(a, b time.Time) bool {
	return a.UTC().Truncate(24 * time.Hour).Equal(b.UTC().Truncate(24 * time.Hour))
}

// noteCounts remembers the lifetime totals the totals family just collected,
// which is what sizes the pull request page for every repository.
func (r *Runner) noteCounts(points []sink.Point) {
	if r.counts == nil {
		r.counts = map[string]collect.ItemCounts{}
	}
	collect.ReadItemCounts(points, r.counts)
}

// actions is the workflow run collector for this run.
//
// A sweep reads twice the cadence, so a sweep that was late still overlaps the
// previous one, but never less than two hours. The exporter only holds what
// the last sweep collected, and a thirty minute window on a quiet account is
// usually empty, which left the continuous integration panels reading nothing
// between builds. InfluxDB is unaffected either way: it keeps the history.
//
// The very first sweep reaches back a month instead: otherwise a fresh install
// charts a workflow history that starts fifteen minutes ago. It asks for pages
// of a hundred, as a backfill does; every later sweep asks for thirty, which
// is a third of the bytes for the same runs, since the window it covers is
// two hours and the walk pages on when thirty is not enough. Seven pages of
// thirty, so the walk reaches the two hundred and ten runs two pages of a
// hundred did and a burst of Dependabot runs is not cut at sixty; a quiet
// repository still stops at the first page, whose oldest run is already
// past the window.
//
// A backfill lists the jobs of every run, whatever this process remembers;
// a sweep skips the runs whose jobs this process already wrote, which after
// the first sweep is nearly all of them. The memory starts empty with the
// process and is never reset: the first sweep runs this once per repository
// before the family is marked, and a reset here would keep only the last
// repository's runs.
func (r *Runner) actions(now time.Time) collect.Actions {
	if r.Backfill {
		// Every run's jobs, however many requests that is. A backfill
		// was asked to take as long as it takes. GitHub serves the jobs
		// for as long as it holds the run, but not their steps: measured
		// on 2026-09-24, a run 278 days old still listed every job with
		// its times and runner, and every run created before 12 April,
		// about five and a half months back, listed them with no steps.
		return collect.Actions{Since: r.BackfillSince, Jobs: true, MaxJobRuns: 0, Walk: r.walk()}
	}
	every, _ := r.Cfg.Interval("actions")
	since := now.Add(-max(2*every, 2*time.Hour))
	pages, perPage := 7, 30
	if _, ran := r.State.LastRun["actions"]; !ran {
		since, pages, perPage = now.AddDate(0, 0, -30), 10, 100
	}
	if r.expanded == nil {
		r.expanded = map[collect.RunKey]struct{}{}
	}
	return collect.Actions{
		Since: since, Jobs: true, MaxJobRuns: 20, Expanded: r.expanded,
		PerPage: perPage, Walk: collect.Walk{Pages: pages},
	}
}

// commits is the commit collector for this run.
func (r *Runner) commits(now time.Time) collect.Commits {
	if r.Backfill {
		// The whole history, walked page by page. This is the one family
		// where a backfill is qualitatively different rather than just
		// wider: without it the lines-changed series starts at install.
		return collect.Commits{Since: r.BackfillSince, Walk: r.walk()}
	}
	// Twice the cadence so a late sweep still overlaps, and a month on the
	// first run so a fresh install does not chart a history that starts an
	// hour ago.
	every, _ := r.Cfg.Interval("commits")
	since := now.Add(-2 * every)
	if _, ran := r.State.LastRun["commits"]; !ran {
		since = now.AddDate(0, 0, -30)
	}
	return collect.Commits{Since: since}
}

// dependencies diffs the dependency graph from where the last diff ended, so
// the ranges join up instead of overlapping or leaving a gap, and remembers
// where this one ended for the next.
func (r *Runner) dependencies(ctx context.Context, repo collect.Repo, now time.Time) ([]sink.Point, error) {
	deps := &collect.Dependencies{Base: r.State.LastHead[repo.FullName], SBOM: true, Refusals: r.refusalsFor("deps")}
	points, err := deps.Collect(ctx, r.API, repo, now)
	if deps.ResolvedHead != "" {
		r.State.LastHead[repo.FullName] = deps.ResolvedHead
	}
	return points, err
}

// settings is the settings collector for this run, which on a backfill also
// reads the webhook deliveries.
func (r *Runner) settings() collect.Settings {
	if r.Backfill {
		return collect.Settings{Deliveries: 100}
	}
	return collect.Settings{}
}

// jobLogs is the job log collector for this run.
func (r *Runner) jobLogs(now time.Time) collect.JobLogs {
	if !r.Backfill {
		every, _ := r.Cfg.Interval("joblogs")
		return collect.JobLogs{Since: now.Add(-2 * every)}
	}
	// GitHub keeps logs for the repository's retention period, ninety days
	// by default, at most ninety on a public repository and up to four
	// hundred on a private one, and answers 410 after it: measured on
	// 2026-09-24, a public repository's log answered at ninety days and
	// was a 410 at ninety two. The walk stops at ninety whatever the
	// setting, which is every log a public repository still has; a private
	// repository kept longer loses the rest, since the setting is not read.
	since := now.AddDate(0, 0, -90)
	if r.BackfillSince.After(since) {
		since = r.BackfillSince
	}
	return collect.JobLogs{Since: since, MaxJobs: 500, Walk: collect.Walk{Pages: -1, Since: since}}
}

// family runs one account-wide collector if it is due and within budget.
// achievementsWarn logs one line per distinct warning of the achievements
// family. The key is the message and its arguments rendered as text, so the
// same floor or the same disagreement is one line however many days it holds,
// and a new count or a new badge is a new line.
func (r *Runner) achievementsWarn(msg string, args ...any) {
	key := fmt.Sprint(append([]any{msg}, args...)...)
	if r.achievementsSaid[key] {
		return
	}
	if r.achievementsSaid == nil {
		r.achievementsSaid = map[string]bool{}
	}
	r.achievementsSaid[key] = true
	r.Log.Warn(msg, args...)
}

func (r *Runner) family(ctx context.Context, name string, now time.Time, run func() ([]sink.Point, error)) {
	if r.Cfg.Targets.User == "" {
		return
	}
	every, enabled := r.Cfg.Interval(name)
	if !enabled || (!r.prime && !r.State.Due(name, every, now)) {
		return
	}
	// Already written by the walk this run resumes. An account family is its
	// own unit: it asks about no repository, so there is nothing smaller of it
	// to record, and the four minutes the eleven of them took on the author's
	// account is a checkpoint fine enough for them.
	if done, already := r.Progress.Done(name); already {
		// Its row too: see the same case in the per-repository loop. An
		// account-wide family covers no repository, so the count that means
		// anything for it is the points the earlier process wrote.
		r.noteFamily(name, done.Repos, 0, done.Points)
		r.Log.Info("family already written by the walk this resumes",
			"family", name, "points", done.Points)
		return
	}
	if !r.awaitBudget(ctx, name) {
		return
	}
	points, err := run()
	if err != nil {
		r.Log.Error("collector failed", "family", name, "err", err)
	}
	// Emitted whether or not it failed, for the reason collectFamily gives at
	// length: several of these families read every repository in batches of
	// fourteen, and the batches that answered answered.
	delivered := r.emit(ctx, name, points)
	// An account-wide family asks about no repository, so it fails as one
	// thing or not at all.
	failed := 0
	if err != nil {
		failed = 1
		r.noteFamilyFailure(name, err)
	}
	r.noteFamily(name, 0, failed, len(points))
	if err != nil && len(points) == 0 {
		// Nothing at all came back, so the family has not run. Marking it
		// would hide the outage until its next cadence, which for the twelve
		// hour families is half a day.
		return
	}
	// Marked although some of it failed, which is the rule repoFamilies
	// already follows for a family that failed on some of its repositories
	// and not on all of them: a batch that lost one repository must not cost
	// a whole family's pass on every sweep until it comes back. The failure
	// is not lost by being marked; it is in the log and in the collector's own
	// rows, which is where it was missing.
	r.State.Mark(name, now)
	// The checkpoint is stricter than the mark above, and on purpose. A mark
	// costs a family one pass of its own cadence; a checkpoint is a family a
	// resume never opens again, so a family that lost a batch, or whose rows
	// one sink refused, is left out of it and walked again.
	if err == nil && delivered {
		// An account-wide family asks about no repository, so it has no
		// per-repository records and its points are counted here.
		r.checkpointFamily(name, 0, len(points))
	}
}

// budgetLeft brakes before the token runs out, leaving the reserve untouched so
// whatever else uses the same token keeps working.
//
// It looks at the buckets the sweep actually spends from, not at whichever was
// charged last. Those are three different budgets of very different sizes, and
// judging the sweep by the smallest of them stopped it dead: one call to the
// search API left "29 of 30" in the last-seen field, a reserve of five hundred
// read that as exhausted, and every remaining family was skipped.
func (r *Runner) budgetLeft() bool {
	for _, bucket := range []string{"core", "graphql", "search"} {
		rate, seen := r.API.RateFor(bucket)
		if !seen || rate.Limit == 0 {
			continue
		}
		if rate.Remaining > r.reserveFor(rate) {
			continue
		}
		// Once the window resets the budget is whole again, so a reserve
		// reached an hour ago is not a reason to stay stopped.
		if rate.Reset.IsZero() || time.Now().Before(rate.Reset) {
			return false
		}
	}
	return true
}

// awaitBudget reports whether there is budget to continue.
//
// In a normal sweep it just answers. In a backfill it waits for the window to
// turn over first, because a backfill that gives up half way has done the
// expensive part and kept none of the benefit.
func (r *Runner) awaitBudget(ctx context.Context, family string) bool {
	if r.budgetLeft() {
		return true
	}
	if !r.Backfill {
		r.Log.Warn("rate limit reserve reached, family skipped", "family", family)
		return false
	}
	wait := r.timeToReset()
	if wait <= 0 {
		return true
	}
	r.Log.Info("rate limit reserve reached, waiting for the window to reset",
		"family", family, "wait", wait.Round(time.Second).String())
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
	}
	return true
}

// timeToReset is how long until the most constrained bucket refills, plus a
// second of slack because the reset instant is the server's, not ours.
func (r *Runner) timeToReset() time.Duration {
	var longest time.Duration
	now := time.Now()
	for _, bucket := range []string{"core", "graphql", "search"} {
		rate, seen := r.API.RateFor(bucket)
		if !seen || rate.Limit == 0 || rate.Reset.IsZero() {
			continue
		}
		if rate.Remaining > r.reserveFor(rate) {
			continue
		}
		if d := rate.Reset.Sub(now) + time.Second; d > longest {
			longest = d
		}
	}
	// A clock skew or a stale header must not turn into an hour of sleep for
	// nothing, nor into a busy loop.
	if longest > time.Hour {
		longest = time.Hour
	}
	if longest > 0 && longest < time.Second {
		longest = time.Second
	}
	return longest
}

// reserveFor scales the configured reserve to the bucket. Search allows thirty
// requests a minute, so a flat reserve of five hundred would mean it is never
// usable at all.
func (r *Runner) reserveFor(rate ghapi.RateState) int {
	reserve := r.Cfg.GitHub.ReserveRate
	if fifth := rate.Limit / 5; fifth < reserve {
		reserve = fifth
	}
	return reserve
}

// emit sends one family's points, or one repository's of them, and reports
// whether every sink took them.
//
// The answer is what the backfill checkpoint is written on: a record there is
// a claim that a store already holds those rows, and a sink that failed is the
// one case where that claim would be false. Nothing else reads it, because
// nothing else may skip work on the strength of it.
//
// attrs are added to each line this writes. A walk that keeps a checkpoint
// sends a repository at a time, and without the repository's name the journal
// would carry fifty nine lines a family that a reader cannot tell apart.
func (r *Runner) emit(ctx context.Context, family string, points []sink.Point, attrs ...any) bool {
	if len(points) == 0 {
		// Nothing to deliver is delivered: a repository a family has no rows
		// for is done, and a checkpoint that refused to say so would walk it
		// again on every resume forever.
		return true
	}
	delivered := true
	for _, s := range r.Sinks {
		// A sink that skips unchanged points reports how many, so the log line
		// says what reached the store rather than what was offered to it.
		var filter *sink.Unchanged
		before := uint64(0)
		if u, ok := s.(*sink.Unchanged); ok {
			filter, before = u, u.Dropped()
		}
		accepted, err := s.Write(ctx, points)
		if rejected, ok := errors.AsType[*sink.RejectedError](err); ok {
			// Everything parseable was written. The lines themselves are
			// reported by the sink through OnReject.
			r.Log.Warn("sink rejected some lines", "sink", s.Name(), "family", family,
				"rejected", rejected.N)
			err = nil
		}
		if dropped, ok := errors.AsType[*sink.DroppedError](err); ok {
			// Everything writable was written. Worth saying once, not worth
			// treating as a failure.
			r.Log.Debug("sink dropped old entries", "sink", s.Name(), "family", family,
				"dropped", dropped.N, "older_than", dropped.Older.String())
			err = nil
		}
		if err != nil {
			r.Log.Error("sink write failed", append([]any{
				"sink", s.Name(), "family", family,
				"points", len(points), "err", err,
			}, attrs...)...)
			delivered = false
			continue
		}
		// Counted in the sink's own unsigned type rather than converted into
		// an int, so neither figure in the line below can come from a
		// conversion that wraps.
		offered, skipped := uint64(len(points)), uint64(0)
		if filter != nil {
			// The counter is cumulative, so the delta is what this write
			// skipped, and one write can skip no more points than it was
			// given. Bounding it there keeps a counter that ever ran backwards
			// out of the log line instead of reporting a negative total.
			skipped = min(filter.Dropped()-before, offered)
		}
		// What the sink says it took, bounded by what it was given: a sink
		// that over-reports is a bug, and it is not one this line will carry.
		written := min(nonNegative(accepted), offered-skipped)
		filtered := offered - skipped - written
		if r.filtered == nil {
			r.filtered = map[string]uint64{}
		}
		r.filtered[s.Name()] += filtered
		if filtered > 0 {
			// Said only when there is something to say, so the key appears
			// exactly where the question "where did the rest go" arises.
			r.Log.Info("written", append([]any{
				"sink", s.Name(), "family", family,
				"points", written, "unchanged", skipped, "filtered", filtered,
			}, attrs...)...)
			continue
		}
		r.Log.Info("written", append([]any{
			"sink", s.Name(), "family", family,
			"points", written, "unchanged", skipped,
		}, attrs...)...)
	}
	return delivered
}

// nonNegative is a sink's own count as an unsigned one. A negative is a sink
// returning nonsense, and it becomes zero rather than a number near the top of
// the unsigned range.
func nonNegative(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// Serve runs sweeps until the context is canceled.
//
// The tick is the shortest configured interval, so a family that wants to run
// every quarter of an hour is not delayed by one that runs every twelve hours,
// unless the config forces one: see tick.
func (r *Runner) Serve(ctx context.Context) error {
	tick, from := r.tick()
	r.Log.Info("ghchronicle running", "tick", tick.String(), "tick_from", from)
	if err := r.Once(ctx); err != nil {
		r.Log.Error("sweep failed", "err", err)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := r.Once(ctx); err != nil {
				r.Log.Error("sweep failed", "err", err)
			}
		}
	}
}

// tick is how often the loop wakes to ask which families are due, and where
// that number came from.
//
// config.heartbeat wins outright, floor and all. The floor below exists to
// stop a mistyped cadence spinning the loop; an explicit heartbeat is not a
// mistyped cadence, and a test run that wants the loop to turn every two
// hundred milliseconds has asked for exactly that.
func (r *Runner) tick() (every time.Duration, from string) {
	if d, ok := r.Cfg.HeartbeatEvery(); ok {
		return d, "heartbeat"
	}
	return r.shortestInterval(), "shortest cadence"
}

func (r *Runner) shortestInterval() time.Duration {
	shortest := time.Hour
	for _, name := range config.Families() {
		if d, ok := r.Cfg.Interval(name); ok && d < shortest {
			shortest = d
		}
	}
	if shortest < time.Minute {
		shortest = time.Minute
	}
	return shortest
}
