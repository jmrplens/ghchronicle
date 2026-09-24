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

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
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
		found = append(found, stateRemovals(cfg)...)
	}
	return carryOut(ctx, found, confirmed, out)
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
	}
	return found, nil
}

// stateRemovals is what a sweep keeps between runs. All of it is rebuilt by
// running again; what it costs to lose is one full pass over the API.
func stateRemovals(cfg *config.Config) []removal {
	var found []removal
	for _, path := range statePaths(cfg) {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found = append(found, removal{
			what: "the state file " + path,
			drop: func(_ context.Context) error { return os.Remove(path) },
		})
	}
	return found
}

// statePaths is every file a sweep leaves behind. The dedupe ledger and the
// backfill checkpoint sit beside the state file under names derived from it,
// which is why finding them is a matter of looking rather than of asking the
// config for three paths it only holds one of.
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
