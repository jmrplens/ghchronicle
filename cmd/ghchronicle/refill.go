package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// refiller is how this build reads again what a migration cleared: a
// backfill of the families that write it, writing only what was cleared,
// only into the stores it was cleared from, and keeping a checkpoint of its
// own so that a refill cut short resumes where it stopped.
//
// It runs a runner of its own beside the run's. Its client waits for the rate
// limit to turn over, as a backfill's does, where a sweep's skips: a refill
// that gives up half way leaves a store without the history it had an hour
// before. Its state is a copy that is never saved (see run.State.Detached),
// and its sinks are the run's own, behind the run's write ledger, whose salt
// already makes it forget what was cleared.
func refiller(m migration) migrate.Refiller {
	return func(ctx context.Context, w migrate.RefillWalk) error {
		log := m.log.With("run", "refill")
		runner := &run.Runner{
			Cfg: m.cfg, API: newAPI(m.cfg, true, log), Sinks: refillSinks(m.sinks, w.Keep), State: m.state.Detached(),
			Log: log, Prime: true, Backfill: true, BackfillSince: w.Since,
			Only: set(w.Families), Keep: keepOf(w.Keep),
			// Read for its conditional answers, which a 304 replays whole;
			// a backfill writes nothing back to it.
			CacheFile: m.cfg.CacheFile(),
		}
		for _, f := range w.Families {
			if _, enabled := m.cfg.Interval(f); !enabled {
				log.Warn("the refill cannot read a family this configuration switches off, so what it wrote "+
					"into the stores cleared does not come back", "family", f,
					"then", "give it a cadence and run "+commandLine(m.configPath, "-backfill", "-families", f))
			}
		}
		scope := run.ScopeOf(m.cfg, refillSince(w.Since)).Narrowed(w.Families, w.Keep)
		progress, err := openRefill(m.cfg.RefillProgressFile(), scope, log)
		if err != nil {
			return err
		}
		runner.Progress = progress
		if walked := walkUntilDoneOrStuck(ctx, runner, m.retry, log); walked != nil {
			return walked
		}
		if ctx.Err() != nil {
			return fmt.Errorf("stopped before it ended; %s keeps what it wrote, and the next run resumes it",
				shownPath(progress.Path()))
		}
		if left := progress.Unfinished(); len(left) > 0 {
			return fmt.Errorf("%s did not reach the end of what it reads; %s keeps what the others wrote, "+
				"and the next run resumes it", quotedList(left), shownPath(progress.Path()))
		}
		return nil
	}
}

// openRefill opens the refill's checkpoint, and starts it afresh when it
// belongs to another refill.
//
// A backfill's checkpoint that does not match is refused, since it records a
// walk somebody asked for and only they know whether to put the settings back.
// This one is only ever written by a refill, which the state file asks for:
// one that no longer matches was left by a refill that owed less, or before
// the configuration changed, and walking the refill owed now from the start
// costs the time the old one had spent and loses nothing, since what it wrote
// is written again over the same rows.
func openRefill(path string, scope run.Scope, log *slog.Logger) (*run.Progress, error) {
	progress, err := run.OpenProgress(path, version, scope, time.Now())
	if err == nil {
		return progress, nil
	}
	log.Info("the refill checkpoint was left by another refill, so this one starts from the first family",
		"checkpoint", path, "why", err.Error())
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return nil, fmt.Errorf("the refill checkpoint %s is another refill's and cannot be removed: %w", path, removeErr)
	}
	return run.OpenProgress(path, version, scope, time.Now())
}

// refillSince is a refill's bound as the checkpoint keeps it: the day, or
// nothing for no bound.
func refillSince(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return since.UTC().Format(time.DateOnly)
}

// refillSinks is the run's sinks that a refill writes to: the stores that
// were cleared, and never Loki, the exporter, OTLP, the file, stdout or a
// Telegraf, which are either not cleared or keep what they were sent as it
// was sent, so a refill written to them is every row of it a second time.
func refillSinks(sinks []sink.Sink, keep map[string][]string) []sink.Sink {
	var out []sink.Sink
	for _, s := range sinks {
		if _, cleared := keep[s.Name()]; cleared {
			out = append(out, s)
		}
	}
	return out
}

// keepOf is a walk's measurements per store as the runner reads them.
func keepOf(keep map[string][]string) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(keep))
	for store, measurements := range keep {
		out[store] = set(measurements)
	}
	return out
}

// quotedList names a list the way a sentence does.
func quotedList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// itemReaders is the stores that can say which items a table holds, by the
// sink's name, for the comparison after a refill.
func itemReaders(m migration) map[string]teardown.ItemReader {
	out := map[string]teardown.ItemReader{}
	for _, r := range teardown.ItemReaders(m.cfg) {
		out[r.Name()] = r
	}
	return out
}

// sayOwed warns about every refill owed that this run does not pay, with why
// and the command that pays it.
func sayOwed(log *slog.Logger, owed []migrate.Owed, why, apply string, service bool) {
	for _, o := range owed {
		args := []any{
			"sink", o.Store, "measurements", strings.Join(o.Refill.Measurements, ","),
			"families", strings.Join(o.Refill.Families, ","), "since", refillSinceOr(o.Refill.Since),
			"migrations", strings.Join(o.Refill.Migrations, ","), "not_read", why, "apply", apply,
		}
		if service {
			args = append(args, "first", "stop this service: -migrate -yes refuses to run beside it")
		}
		log.Warn("refill owed: a store a migration cleared does not hold that history yet", args...)
	}
}

// refillSinceOr is a bound as a log attribute, which says none rather than
// nothing.
func refillSinceOr(since time.Time) string {
	if s := refillSince(since); s != "" {
		return s
	}
	return "none"
}

// storesOwed is the stores of a list of refills, sorted.
func storesOwed(owed []migrate.Owed) []string {
	out := make([]string, 0, len(owed))
	for _, o := range owed {
		out = append(out, o.Store)
	}
	slices.Sort(out)
	return out
}
