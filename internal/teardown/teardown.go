// Package teardown removes what a collector left in a store.
//
// It asks the store what it holds rather than carrying a list of measurement
// names. A compiled list would be the names this version writes, and the
// tables worth removing are exactly the ones nobody writes any more: what an
// earlier version collected, or a family switched off since. Asking finds
// those; a list is blind to them by construction.
//
// Everything here is destructive, so nothing here decides anything. Each store
// says what it holds and drops what it is told to drop, one item at a time,
// and the caller is the one that prints the list and waits to be told yes.
package teardown

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmrplens/ghchronicle/internal/config"
)

// Prefix is the namespace every measurement this writes lives under, and so
// the one thing an uninstall claims as its own.
const Prefix = "gh_"

// Store is a place this wrote that can be asked what it holds and told to drop
// it.
type Store interface {
	// Name is the sink's name in the config, for the lines a person reads.
	Name() string
	// Holds is what the store has that this put there, named the way the
	// store names it.
	Holds(ctx context.Context) ([]string, error)
	// Drop removes one of them.
	Drop(ctx context.Context, item string) error
}

// Unsupported is a sink whose store cannot be emptied from here, with the
// reason, which is worth printing: silence would read as nothing to remove.
type Unsupported struct {
	Sink   string
	Reason string
}

// String is the line the reason prints as.
func (u Unsupported) String() string {
	return fmt.Sprintf("%s: %s", u.Sink, u.Reason)
}

// For is one Store per configured sink that can be emptied, and one Unsupported
// per sink that cannot.
func For(cfg *config.Config) (stores []Store, cannot []Unsupported) {
	if s := cfg.Sinks.Influx; s != nil {
		stores = append(stores, &influx{sink: s})
	}
	if s := cfg.Sinks.Elasticsearch; s != nil {
		stores = append(stores, &elastic{sink: s})
	}
	if s := cfg.Sinks.Postgres; s != nil {
		stores = append(stores, &postgres{sink: s})
	}
	if s := cfg.Sinks.SQL; s != nil {
		stores = append(stores, &sqlFile{sink: s})
	}
	if cfg.Sinks.Graphite != nil {
		cannot = append(cannot, Unsupported{
			"graphite",
			"Graphite takes metrics on its ingest port and offers no way to delete them; " +
				"remove the whisper files under its storage directory by hand",
		})
	}
	if cfg.Sinks.SQL != nil {
		// Said out loud because the file is only half the story for anyone who
		// has loaded it: this sink emits statements, it never connects, so a
		// PostgreSQL those statements were run against is not something this
		// knows about or can undo.
		cannot = append(cannot, Unsupported{
			"sql",
			"the file above is everything this sink wrote; it emits statements and never " +
				"connects, so rows already loaded into a real database have to be removed there",
		})
	}
	if cfg.Sinks.Prometheus != nil {
		cannot = append(cannot, Unsupported{
			"prometheus",
			"the Prometheus sink is scraped rather than written to, so this stored nothing " +
				"to remove; the series live in the Prometheus server and age out with its retention",
		})
	}
	return stores, cannot
}

// ours says whether a name is one of this project's.
func ours(name string) bool { return strings.HasPrefix(name, Prefix) }
