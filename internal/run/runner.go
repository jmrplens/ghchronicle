package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
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

	// Now is the clock the sweep reads, for the one test that needs two of
	// them. Nil is time.Now, which is what everything but that test uses.
	Now func() time.Time

	// filtered counts, per sink name, the points that sink was offered and
	// did not write. A sweep prints its own total at the end, beside the
	// ledger's: the per-family lines say where, and this says how much.
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
	if err := r.discoverRepos(ctx, now); err != nil {
		return err
	}
	r.accountFamilies(ctx, now)
	if err := r.repoFamilies(ctx, now); err != nil {
		return err
	}
	r.finish()
	return nil
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
		if !r.awaitBudget(ctx, family) {
			continue
		}
		points, failed, err := r.collectFamily(ctx, family, now)
		if err != nil {
			return err
		}
		r.emit(ctx, family, points)
		// A family where every repository failed has not run. Marking it would
		// hide the outage until its next cadence, which for the slow families
		// is half a day.
		if failed > 0 && failed == len(r.repos) {
			r.Log.Warn("family failed everywhere, not marking it as run", "family", family, "repos", failed)
			continue
		}
		// Marked after the repositories ran, so every one of them saw the
		// same answer to "is the whole page due" that the first did; and
		// only when every one of them answered, because the whole page is
		// the one read that rewrites an open pull request nobody touched,
		// and a repository it failed on would otherwise wait a day for the
		// next. Left unrecorded, the next sweep reads the whole page again,
		// which costs one more daily pass and loses nothing.
		if failed == 0 && r.fullPassDue(family, now) {
			r.State.MarkFull(family, now)
		}
		r.State.Mark(family, now)
	}
	return nil
}

// collectFamily runs one family over every repository and reports what it
// collected along with how many repositories failed.
//
// An error return is a canceled sweep and nothing else: a repository that
// fails is counted and the loop goes on, because one repository with a broken
// feature must not stop the other forty.
func (r *Runner) collectFamily(ctx context.Context, family string, now time.Time) ([]sink.Point, int, error) {
	var points []sink.Point
	// Some families begin with one batched query for every repository at
	// once, which is what turns a REST call per repository into a shared
	// GraphQL point. It is not a per-repository failure if it fails: the
	// loop below still runs.
	failed := 0
	if batch, err := r.familyBatch(ctx, family, now); err != nil {
		r.Log.Error("batched collector failed", "family", family, "err", err)
		// Where the batch was the family, or the part of it these
		// repositories were going to get, marking it as run would hide the
		// outage until its next cadence, which for two of the three is a
		// whole day. Counting them as failed is what repoFamilies already
		// reads as "this family has not run".
		failed = r.batched(family)
	} else {
		points = append(points, batch...)
	}
	for _, repo := range r.repos {
		// A shutdown cancels the sweep. Without this every remaining
		// repository logs its own "context canceled" and the family is
		// then marked as done, which is exactly backwards.
		if ctx.Err() != nil {
			return nil, failed, ctx.Err()
		}
		pts, err := r.repoFamily(ctx, family, repo, now)
		if err != nil {
			if limited, ok := errors.AsType[*ghapi.RateLimitedError](err); ok {
				// Not a failure of this repository: the budget ran out
				// mid-family. Stop here and leave the family unmarked so
				// the next sweep picks it up rather than skipping a day.
				r.Log.Warn("budget spent mid-family", "family", family,
					"at", repo.FullName, "resets", limited.Reset.Format(time.TimeOnly))
				return points, len(r.repos), nil
			}
			failed++
			r.Log.Error("collector failed", "family", family, "repo", repo.FullName, "err", err)
			continue
		}
		points = append(points, pts...)
		if !r.awaitBudget(ctx, family) {
			r.Log.Warn("stopping this family here", "family", family, "at", repo.FullName)
			break
		}
	}
	return points, failed, nil
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
		// was asked to take as long as it takes.
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
	// GitHub keeps logs for ninety days and answers 410 after that,
	// so walking further would be paying for nothing.
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
	if !r.awaitBudget(ctx, name) {
		return
	}
	points, err := run()
	if err != nil {
		r.Log.Error("collector failed", "family", name, "err", err)
		return
	}
	r.emit(ctx, name, points)
	r.State.Mark(name, now)
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

func (r *Runner) emit(ctx context.Context, family string, points []sink.Point) {
	if len(points) == 0 {
		return
	}
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
			r.Log.Error("sink write failed", "sink", s.Name(), "family", family, "points", len(points), "err", err)
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
			r.Log.Info("written", "sink", s.Name(), "family", family,
				"points", written, "unchanged", skipped, "filtered", filtered)
			continue
		}
		r.Log.Info("written", "sink", s.Name(), "family", family,
			"points", written, "unchanged", skipped)
	}
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
