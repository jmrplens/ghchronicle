package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// flagOthers is the flag that lets -migrate -yes clear a store holding rows
// of accounts this configuration does not collect, named once because the
// warnings, the report and the usage all say it.
const flagOthers = "-migrate-others"

// storeQuestionTimeout bounds what a start asks each store about the
// registry. A store that does not answer in that time is said to be
// unreachable and asked again at the next start; the sweep that follows is
// not held for longer than this per store.
const storeQuestionTimeout = 30 * time.Second

// lockPoll is how often a service waiting for the state file asks again.
const lockPoll = 2 * time.Second

// migration is what bringing the stores along takes from the run.
type migration struct {
	cfg   *config.Config
	api   *ghapi.Client
	sinks []sink.Sink
	state *run.State
	// ledger is the run's write ledger, nil for a run that opens none.
	ledger *sink.Ledger
	log    *slog.Logger
	// configPath is -config as it was given, for the command lines a warning
	// names: the reader copies them into the same shell.
	configPath string
	// fresh says the state file had recorded nothing before this run, so it
	// cannot name a copy an earlier run set aside, and the stores are asked.
	fresh bool
}

// storeWays is how this build brings each store along, by the sink's name,
// and how it reads the history of what it cleared again. A store with no
// entry is one this build has no way to change: its items are said and left
// pending.
//
// InfluxDB, PostgreSQL and Elasticsearch clear the measurement themselves,
// keeping the old rows aside for a day where they can; the SQL file is told
// to write the drop into its stream. Graphite and whatever is behind a
// Telegraf are changed by whoever runs them, so their way is to say what the
// plan says; the plan marks both, and the SQL file, as needing somebody's
// word, so only -migrate -yes ever applies them. A variable so that a test
// can put a store's way in place of the real one.
var storeWays = func(m migration) (map[string]migrate.Applier, migrate.Refiller) {
	ways := map[string]migrate.Applier{
		"graphite": migrate.Instructions{},
		"telegraf": migrate.Instructions{},
	}
	for _, c := range teardown.Clearers(m.cfg) {
		clearing := migrate.Clearing{Store: c}
		if pg, ok := sinkOf[*sink.Postgres](m.sinks); ok && c.Name() == pg.Name() {
			clearing.Forget = pg.Forget
		}
		ways[c.Name()] = clearing
	}
	if q, ok := sinkOf[*sink.SQL](m.sinks); ok && m.cfg.Sinks.SQL != nil {
		ways[q.Name()] = migrate.Dropping{Sink: q, Path: m.cfg.Sinks.SQL.Path}
	}
	return ways, nil
}

// sinkOf is the sink of one type among a run's sinks, behind whatever ledger
// wraps it.
func sinkOf[T sink.Sink](sinks []sink.Sink) (T, bool) {
	for _, s := range sinks {
		if found, ok := sink.Inner(s).(T); ok {
			return found, true
		}
	}
	var none T
	return none, false
}

// forgetCleared is what a run forgets once a migration has cleared a store:
// the write ledger's entries of that measurement in that store, through the
// salt the record now gives it, and the cache file's claims about the
// families that write it. Without the first the refill and the sweeps after
// it would be held back from writing into the cleared table every row the
// ledger remembers writing into the old one.
func forgetCleared(m migration) func(migrate.Chosen) {
	return func(c migrate.Chosen) {
		measurement := c.Item.Migration.Measurement
		m.ledger.Salt(c.Store, measurement, migrate.Salt(m.state.Stores[c.Store], measurement))
		if err := run.ForgetInCache(m.cfg.CacheFile(), c.Item.Refill); err != nil {
			m.log.Warn("the cache file still claims what the cleared store held", "file", m.cfg.CacheFile(),
				"err", err, "families", strings.Join(c.Item.Refill, ","))
		}
	}
}

// saltLedger mixes into the ledger every migration the state file records as
// applied, so that a store cleared by an earlier run, -migrate -yes among
// them, is written again by this one rather than held back.
func saltLedger(ledger *sink.Ledger, state *run.State) {
	for store, measurements := range migrate.Salts(state) {
		for measurement, salt := range measurements {
			ledger.Salt(store, measurement, salt)
		}
	}
}

// purging is how a run purges the copies migrations set aside once they have
// been kept their day.
func purging(m migration, ask bool) migrate.Purging {
	return migrate.Purging{
		Config: m.cfg, State: m.state, Save: m.state.Save, Ask: ask, Now: time.Now(), Log: m.log,
	}
}

// commandLine is a command the reader can copy, with the configuration this
// run was given. A path with a character the shell reads is quoted.
func commandLine(configPath string, flags ...string) string {
	path := configPath
	if strings.ContainsAny(path, " \t'\"$`\\&;|<>()*?[]#~!{}") {
		path = "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	}
	return strings.Join(append([]string{"ghchronicle", "-config", path}, flags...), " ")
}

// migrateOnStart is what a run that writes to the stores does about the
// registry before its first sweep: see migrate.Start for the rule. It never
// stops the run. A store it cannot bring along is said and left as it is,
// which is how every release before this one left it, and the sweep that
// follows writes this release's shape either way.
func migrateOnStart(ctx context.Context, m migration, service bool) {
	ways, refill := storeWays(m)
	in := migrate.Input{
		Config: m.cfg, State: m.state, Release: version, Now: time.Now(),
		TrustRecord: true, StoreTimeout: storeQuestionTimeout,
		LoadRepos: func(ctx context.Context) ([]string, error) {
			repos, why := coveredRepos(ctx, m.api, m.cfg)
			if why != "" {
				return nil, errors.New(why)
			}
			return repos, nil
		},
	}
	start := migrate.Start{
		Plan: migrate.Make(ctx, in), Auto: m.cfg.MigratesOnItsOwn(),
		CanApply: func(store string) bool { return ways[store] != nil },
		DryRun:   commandLine(m.configPath, "-migrate"),
		Apply:    commandLine(m.configPath, "-migrate", "-yes"),
		Others:   flagOthers, Service: service, State: m.state, Now: time.Now(), Log: m.log,
	}
	chosen := start.Decide(ctx)
	purge := purging(m, m.fresh)
	if len(chosen) == 0 && !purge.Owed() {
		return
	}
	// The service holds the state file for as long as it runs. A one-shot
	// run takes it for as long as it applies or purges, and does neither
	// when somebody else has it: two processes changing one store at once is
	// what the lock exists to stop.
	if !service {
		lock, err := run.TakeLock(m.cfg.LockFile(), run.HeldByStart, version, time.Now())
		if err != nil {
			start.Held(chosen, "the state file is not this run's to change: "+err.Error())
			return
		}
		defer func() { _ = lock.Release() }()
	}
	if len(chosen) > 0 {
		resume := "the next start tries again, and " + commandLine(m.configPath, "-migrate") + " says what is left"
		_, err := migrate.Applying{
			State: m.state, Save: m.state.Save, Appliers: ways, Refill: refill,
			Cleared: forgetCleared(m), Log: m.log, Resume: resume,
		}.Apply(ctx, chosen)
		if err != nil {
			m.log.Error("reading the history of what was cleared again did not finish", "err", err, "resume", resume)
		}
	}
	purge.Now = time.Now()
	purge.Run(ctx)
}

// holdStateFile takes the state file for the service's whole life.
//
// A migration applied by -migrate -yes, or by a one-shot run before its
// sweep, is waited for: it ends, and the service then starts on the stores it
// left. A second service on the same state file is refused, because it never
// ends, and two processes saving one state file undo each other's marks. A
// lock that cannot be taken at all, a directory that refuses the file, is
// said and the service runs without one, as every release before this did.
func holdStateFile(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*run.Lock, error) {
	waiting := false
	for {
		lock, err := run.TakeLock(cfg.LockFile(), run.HeldByService, version, time.Now())
		if err == nil {
			return lock, nil
		}
		held, isHeld := run.IsHeld(err)
		switch {
		case !isHeld:
			logger.Warn("the state file cannot be locked, so -migrate -yes cannot tell this service is running",
				"lock", cfg.LockFile(), "err", err)
			return &run.Lock{}, nil
		case held.Holder.Run == run.HeldByService:
			return nil, fmt.Errorf("%w: another ghchronicle service keeps the same state file, and two undo "+
				"each other's marks; stop one of them, or give each a state_file of its own", err)
		case !waiting:
			logger.Info("waiting for the process holding the state file to finish", "holder", held.Holder.String(),
				"lock", held.Path)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// migratePlan prints, for every configured store, what an earlier release
// left there in a shape this one no longer writes and what bringing it along
// would take.
//
// It changes nothing, anywhere: the stores are asked questions, GitHub is
// asked for the repository list, and the state file is read and never
// written, not even to record what the stores said. A dry run that wrote
// its findings down would make the next plan read differently from this one
// for no reason the reader could see.
func migratePlan(ctx context.Context, cfg *config.Config, api *ghapi.Client, configPath string,
	stdout io.Writer, now time.Time,
) {
	in := migrate.Input{
		Config: cfg, State: run.LoadState(cfg.StateFile), Release: version, Now: now,
	}
	in.Repos, in.ReposWhy = coveredRepos(ctx, api, cfg)
	in.ReposKnown = in.ReposWhy == ""
	plan := migrate.Make(ctx, in)
	plan.Print(stdout)
	apply, held := plan.Pending(false)
	if len(apply)+len(held) == 0 {
		return
	}
	fmt.Fprintf(stdout, "\nTo apply every pending one, with the service stopped:\n  %s\n",
		commandLine(configPath, "-migrate", "-yes"))
	if len(held) > 0 {
		fmt.Fprintf(stdout, "A store holding rows of accounts this configuration does not collect is left "+
			"alone unless %s is added too.\n", flagOthers)
	}
	fmt.Fprintf(stdout, "Under migrate: %s, the setting this configuration has, ", cfg.Migrate)
	if cfg.MigratesOnItsOwn() {
		fmt.Fprintln(stdout, "every start applies on its own the ones marked safe to apply unattended "+
			"and warns about the rest.")
	} else {
		fmt.Fprintln(stdout, "a start applies none of them and warns about each.")
	}
}

// migrateApply is -migrate -yes: the plan, and then every pending migration
// applied, the unsafe ones too, since -yes is the word the plan asked for.
// What it holds back is a store holding rows of accounts this configuration
// does not collect, which also takes -migrate-others: set aside, those rows
// come back only when whoever collects them reads them again.
//
// It refuses before it changes anything when it could not finish what it
// starts: with no token, or no repository list, nothing can be read again,
// and beside a process that holds the state file it would change the stores
// that process writes and the state file it saves.
func migrateApply(ctx context.Context, cfg *config.Config, api *ghapi.Client, o options, sinks []sink.Sink,
	stdout io.Writer, logger *slog.Logger,
) error {
	if cfg.GitHub.Token == "" {
		return errors.New("-migrate -yes reads what it clears again from GitHub, and the configuration has no " +
			"GitHub token; nothing was changed")
	}
	lock, err := run.TakeLock(cfg.LockFile(), run.HeldByMigrate, version, time.Now())
	if held, isHeld := run.IsHeld(err); isHeld {
		return fmt.Errorf("%w; nothing was changed. -migrate -yes changes the stores that process writes and the "+
			"state file it saves, so it does not run beside it: stop it, or let it finish, and run this again. "+
			"Under migrate: auto the service applies the safe ones itself when it starts", held)
	}
	if err != nil {
		return fmt.Errorf("the state file cannot be locked (%w), so nothing could stop the service starting "+
			"half way through; nothing was changed", err)
	}
	defer func() { _ = lock.Release() }()
	repos, why := coveredRepos(ctx, api, cfg)
	if why != "" {
		return fmt.Errorf("the repository list could not be read (%s), and bringing a store along needs it to "+
			"read the history again and to know whose rows the store holds; nothing was changed", why)
	}
	state := run.LoadState(cfg.StateFile)
	for _, w := range migrate.Stamp(state, cfg, version) {
		logger.Warn(w)
	}
	plan := migrate.Make(ctx, migrate.Input{
		Config: cfg, State: state, Release: version, Now: time.Now(), Repos: repos, ReposKnown: true,
	})
	plan.PrintStores(stdout)
	chosen, held := plan.Pending(o.migrateOthers)
	report := migrate.Report{
		Held: held, Unreached: plan.Unreached(), Others: flagOthers,
		Resume: "Run the same command again once the cause is fixed: what was applied is recorded and is not " +
			"done twice.",
	}
	m := migration{cfg: cfg, api: api, sinks: sinks, state: state, log: logger, configPath: o.path}
	if len(chosen) > 0 {
		ways, refill := storeWays(m)
		report.Results, report.Refill = migrate.Applying{
			State: state, Save: state.Save, Appliers: ways, Refill: refill, Cleared: forgetCleared(m),
			Log: logger, Resume: report.Resume,
		}.Apply(ctx, chosen)
	}
	// Copies set aside a day ago or more, by an earlier run of this command
	// or by a service, are purged here as the service would purge them. The
	// stores are asked as well, since a state file this command did not
	// keep, the Action's for one, does not name them.
	purging(m, true).Run(ctx)
	if err = state.Save(); err != nil {
		logger.Warn("state not saved", "err", err)
	}
	report.Print(stdout)
	if report.Failed() {
		return errors.New("-migrate -yes left something undone; the lines above say what")
	}
	return nil
}

// migrateAndClose is -migrate -yes over a one-shot run's sinks, which open
// no ledger, so what is read again is written whole. The sinks are closed
// before the exit, which fatal takes without the deferred closes: what was
// read again has to have reached the stores by then.
func migrateAndClose(ctx context.Context, cfg *config.Config, api *ghapi.Client, o options, sinks []sink.Sink,
	stdout, stderr io.Writer, logger *slog.Logger,
) {
	err := migrateApply(ctx, cfg, api, o, sinks, stdout, logger)
	for _, s := range sinks {
		_ = s.Close()
	}
	if err != nil {
		fatal(stderr, err)
	}
}

// holdIfServing takes the state file for a service and reports whether the
// run goes on: a service stopped while it waited ends cleanly, and one
// refused ends with the reason. Any other run takes nothing here.
func holdIfServing(ctx context.Context, cfg *config.Config, serving bool, logger *slog.Logger,
	stderr io.Writer,
) (*run.Lock, bool) {
	if !serving {
		return nil, true
	}
	lock, err := holdStateFile(ctx, cfg, logger)
	if err == nil {
		return lock, true
	}
	if ctx.Err() == nil {
		fatal(stderr, err)
	}
	return nil, false
}

// coveredRepos is every repository a refill of cfg would read, which is what
// a backfill covers, archived repositories included, or why the list could
// not be read. Without a token, or with GitHub not answering, the plan still
// runs and says what it could not compare.
func coveredRepos(ctx context.Context, api *ghapi.Client, cfg *config.Config) (repos []string, why string) {
	if cfg.GitHub.Token == "" {
		return nil, "the configuration has no GitHub token"
	}
	found, err := collect.Discover(ctx, api, run.DiscoveryFilter(cfg, true))
	if err != nil {
		return nil, err.Error()
	}
	repos = make([]string, 0, len(found.Repos))
	for _, r := range found.Repos {
		repos = append(repos, r.FullName)
	}
	return repos, ""
}
