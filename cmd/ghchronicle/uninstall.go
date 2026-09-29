package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// The things an uninstall can take away, named on the command line.
const (
	targetDashboard = "dashboard"
	targetData      = "data"
	targetState     = "state"
	targetAll       = "all"
)

// uninstallTargets is every name -uninstall takes, for the usage line and for
// refusing a misspelled one rather than quietly removing less than was asked.
var uninstallTargets = []string{targetDashboard, targetData, targetState, targetAll}

// uninstall removes what this put in place. Without -yes it removes nothing
// and prints what it would: a dry run is the default because the alternative
// is a typo that empties a store.
func uninstall(ctx context.Context, cfg *config.Config, list string, confirmed bool,
	out io.Writer,
) error {
	wanted, err := parseTargets(list)
	if err != nil {
		return err
	}
	lock := &run.Lock{}
	if confirmed && (wanted[targetData] || wanted[targetState]) {
		held, lockErr := holdForUninstall(cfg)
		if lockErr != nil {
			return lockErr
		}
		lock = held
		defer func() { _ = lock.Release() }()
	}
	var found []removal
	if wanted[targetDashboard] {
		grafanaSide, failed := dashboardRemovals(ctx, cfg)
		if failed != nil {
			return failed
		}
		found = append(found, grafanaSide...)
	}
	if wanted[targetData] {
		storeSide, failed := dataRemovals(ctx, cfg, out)
		if failed != nil {
			return failed
		}
		found = append(found, storeSide...)
	}
	if wanted[targetState] {
		found = append(found, stateRemovals(cfg, lock)...)
	}
	return carryOut(ctx, found, confirmed, out)
}

// holdForUninstall takes the lock beside the state file for the length of an
// uninstall that removes tables or the state file, and refuses while another
// process holds it.
//
// Removing either under a running service is removing what it goes on
// writing, and the lock file is among the state files: unlinked while the
// service held it, a -migrate -yes after it took a new one and ran beside the
// service, which is what the lock is there to stop. A lock file that is not
// there is one nobody holds, and is not made here, which would leave one
// behind after an uninstall of the tables alone.
func holdForUninstall(cfg *config.Config) (*run.Lock, error) {
	path := cfg.LockFile()
	if path == "" || !fileThere(path) {
		return &run.Lock{}, nil
	}
	lock, err := run.TakeLock(path, run.HeldByUninstall, version, time.Now())
	if held, isHeld := run.IsHeld(err); isHeld {
		return nil, fmt.Errorf("%w; nothing was removed. That process writes the stores and the state file "+
			"this would remove: stop it, or let it finish, and run this again", held)
	}
	// A lock file that cannot be opened at all is not held by anybody this
	// could ask, and is no reason to refuse what was asked for.
	if lock == nil {
		lock = &run.Lock{}
	}
	return lock, nil
}

// fileThere says whether a file can be found at path.
func fileThere(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removal is one thing to take away, with the words that name it and the call
// that does it.
type removal struct {
	what string
	drop func(context.Context) error
}

// carryOut prints the list and, when told yes, works through it.
func carryOut(ctx context.Context, found []removal, confirmed bool, out io.Writer) error {
	if len(found) == 0 {
		fmt.Fprintln(out, "nothing of this is here to remove")
		return nil
	}
	for _, item := range found {
		fmt.Fprintf(out, "  %s\n", item.what)
	}
	if !confirmed {
		fmt.Fprintf(out, "\n%d thing(s) would be removed. Nothing was. Add -yes to go ahead.\n",
			len(found))
		return nil
	}
	fmt.Fprintln(out, "")
	var failed []string
	for _, item := range found {
		if err := item.drop(ctx); err != nil {
			// One that will not go is reported and the rest still go: stopping
			// at the first would leave the removal half done with no list of
			// what is left.
			fmt.Fprintf(out, "  could not remove %s: %v\n", item.what, err)
			failed = append(failed, item.what)
			continue
		}
		fmt.Fprintf(out, "  removed %s\n", item.what)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d could not be removed: %s",
			len(failed), len(found), strings.Join(failed, ", "))
	}
	return nil
}

// parseTargets reads the comma-separated list, and refuses a name it does not
// know rather than removing whatever else was in the list.
func parseTargets(list string) (map[string]bool, error) {
	wanted := map[string]bool{}
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(uninstallTargets, name) {
			return nil, fmt.Errorf("unknown -uninstall target %q, expected some of %s",
				name, strings.Join(uninstallTargets, ", "))
		}
		wanted[name] = true
	}
	if len(wanted) == 0 {
		return nil, errors.New("-uninstall needs something to remove: " +
			strings.Join(uninstallTargets, ", "))
	}
	if wanted[targetAll] {
		return map[string]bool{targetDashboard: true, targetData: true, targetState: true}, nil
	}
	return wanted, nil
}

// dashboardRemovals is what this published in Grafana, and never what it
// merely used: a datasource named in the config was somebody else's before
// this ran and stays theirs after.
func dashboardRemovals(ctx context.Context, cfg *config.Config) ([]removal, error) {
	if cfg.Grafana == nil || cfg.Grafana.Token == "" {
		return nil, errors.New("there is no grafana section with a token, " +
			"so there is nothing this could have published")
	}
	client := grafana.Client{URL: cfg.Grafana.URL, Token: cfg.Grafana.Token}
	if client.URL == "" {
		client.URL = grafana.DefaultURL
	}
	stores, err := storesToPublish(cfg)
	if err != nil {
		return nil, err
	}
	var found []removal
	for _, store := range stores {
		uid := publishedUID(cfg, store)
		if item := existing(ctx, client, "dashboard", grafana.DashboardPath(uid), uid); item != nil {
			found = append(found, *item)
		}
		// An adopted datasource is not ours to take away.
		if cfg.Grafana.Datasource.UID != "" {
			continue
		}
		if item := existing(ctx, client, "datasource",
			grafana.DatasourcePath(store.UID), store.UID); item != nil {
			found = append(found, *item)
		}
	}
	// The Loki datasource is made under a uid of its own rather than a
	// store's, so the loop above never reaches it. Only this makes that uid:
	// one adopted from Grafana keeps the uid it had and is not looked for,
	// and one named in loki_uid is somebody else's even under this name.
	if cfg.Sinks.Loki != nil && cfg.Grafana.Datasource.LokiUID != lokiDatasourceUID {
		if item := existing(ctx, client, "datasource",
			grafana.DatasourcePath(lokiDatasourceUID), lokiDatasourceUID); item != nil {
			found = append(found, *item)
		}
	}
	return found, nil
}

// existing is one removal when something answers at the path, and nothing when
// it does not. A check that cannot run is treated as nothing there: the list
// is what will be attempted, and attempting a removal of something this could
// not even see would report a failure for a thing that was already gone.
func existing(ctx context.Context, client grafana.Client, kind, path, uid string) *removal {
	there, err := client.Exists(ctx, path, grafanaTimeout)
	if err != nil || !there {
		return nil
	}
	return &removal{
		what: fmt.Sprintf("the %s %s in Grafana", kind, uid),
		drop: func(ctx context.Context) error {
			return client.Delete(ctx, path, grafanaTimeout)
		},
	}
}

// dataRemovals is every table this wrote, asked of the store rather than
// remembered, and a line for each sink whose store cannot be emptied from
// here.
func dataRemovals(ctx context.Context, cfg *config.Config, out io.Writer) ([]removal, error) {
	stores, cannot := teardown.For(cfg)
	for _, u := range cannot {
		fmt.Fprintf(out, "note: %s\n", u)
	}
	var found []removal
	for _, store := range stores {
		items, err := store.Holds(ctx)
		if err != nil {
			return nil, fmt.Errorf("asking %s what it holds: %w", store.Name(), err)
		}
		for _, item := range items {
			found = append(found, removal{
				what: fmt.Sprintf("%s in %s", item, store.Name()),
				drop: func(ctx context.Context) error { return store.Drop(ctx, item) },
			})
		}
		sayLingering(ctx, store, len(items), out)
	}
	return found, nil
}

// sayLingering notes what a store keeps of the tables it was told to delete,
// which Holds leaves out of the list, and, on a server that never purges what
// it deletes, that each table this deletes stays too, under a new name, which
// is worth knowing before yes.
//
// A later release is asked which of those tables it will never purge either:
// one a release before 3.2 deleted keeps no hard deletion time through an
// upgrade, and said to be purged on schedule it would be left for good with
// no word of the request that removes it.
func sayLingering(ctx context.Context, store teardown.Store, deleting int, out io.Writer) {
	var lingering []string
	if l, ok := store.(teardown.Lingerer); ok {
		lingering = l.Lingering()
	}
	k, ok := store.(teardown.Keeper)
	if !ok || (len(lingering) == 0 && deleting == 0) {
		return
	}
	if stay, forGood := k.KeptForGood(ctx); forGood {
		sayKeptForGood(store.Name(), stay, deleting, lingering, out)
		return
	}
	if len(lingering) == 0 {
		return
	}
	staying, err := k.Staying(ctx, lingering)
	if err != nil {
		fmt.Fprintf(out, "note: %s: deleted already, and %s, but a table a release before 3.2 deleted stays for "+
			"good, and which of these that is could not be read (%v): %s\n", store.Name(), k.Schedule(ctx),
			err, strings.Join(lingering, ", "))
		return
	}
	var purged []string
	for _, name := range lingering {
		if stay, stays := staying[name]; stays {
			fmt.Fprintf(out, "note: %s: %s was deleted already and is kept for good: %s\n", store.Name(), name, stay)
			continue
		}
		purged = append(purged, name)
	}
	if len(purged) > 0 {
		fmt.Fprintf(out, "note: %s: deleted already, and %s: %s\n", store.Name(), k.Schedule(ctx),
			strings.Join(purged, ", "))
	}
}

// sayKeptForGood is the note of a store that keeps for good what it deletes.
func sayKeptForGood(name string, stay teardown.Stay, deleting int, lingering []string, out io.Writer) {
	var kept []string
	if deleting > 0 {
		kept = append(kept, "each table deleted here is renamed <table>-<instant> and kept for good")
	}
	if len(lingering) > 0 {
		kept = append(kept, "the tables deleted already are kept for good: "+strings.Join(lingering, ", "))
	}
	if len(kept) > 0 {
		fmt.Fprintf(out, "note: %s: %s. %s\n", name, strings.Join(kept, ", and "), stay)
	}
}

// stateRemovals is what a sweep keeps between runs. All of it is rebuilt by
// running again; what it costs to lose is one full pass over the API.
//
// The lock file goes last, once this uninstall has let go of it. Windows
// will not remove a file Go holds open, in the process asking as in any
// other, so removed while held it was the one state file an uninstall there
// never took away. Removed before the others anywhere else, it would let a
// -migrate -yes started in between lock a new one and change the stores
// while the rest of the state was still being removed.
func stateRemovals(cfg *config.Config, lock *run.Lock) []removal {
	var found []removal
	var lockFile *removal
	for _, path := range statePaths(cfg) {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		item := removal{
			what: "the state file " + path,
			drop: func(_ context.Context) error { return os.Remove(path) },
		}
		// Compared clean: the glob that found it cleans what it returns, and
		// on Windows turns a configured forward slash into a backslash.
		if cfg.LockFile() != "" && filepath.Clean(path) == filepath.Clean(cfg.LockFile()) {
			item.drop = func(_ context.Context) error {
				// A close that fails leaves nothing held either way, and the
				// remove says whether the file went.
				_ = lock.Release()
				return os.Remove(path)
			}
			lockFile = &item
			continue
		}
		found = append(found, item)
	}
	if lockFile != nil {
		found = append(found, *lockFile)
	}
	return found
}

// statePaths is every file a sweep leaves behind. The dedupe ledger, the
// cache file, the backfill and refill checkpoints and the lock sit beside the
// state file under names derived from it, which is why finding them is a
// matter of looking rather than of asking the config for paths it only holds
// one of.
func statePaths(cfg *config.Config) []string {
	state := cfg.StateFile
	if state == "" {
		return nil
	}
	paths := []string{state}
	base := strings.TrimSuffix(state, filepath.Ext(state))
	matches, err := filepath.Glob(base + "-*")
	if err != nil {
		return paths
	}
	paths = append(paths, matches...)
	slices.Sort(paths)
	return slices.Compact(paths)
}
