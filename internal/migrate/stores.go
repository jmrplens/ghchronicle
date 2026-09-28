package migrate

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// reach is how much of a store this binary can see, which decides who answers
// whether it holds an old shape.
type reach int

const (
	// asked: the store answers queries, so it decides.
	asked reach = iota + 1
	// recorded: the store cannot be asked, so the state file's record of the
	// release that first wrote it decides.
	recorded
	// untouched: nothing kept here has an identity a release could change.
	untouched
)

// store is one configured sink as the planner sees it.
type store struct {
	// name is the sink's key in the configuration.
	name  string
	reach reach
	// destination is where the sink points, from the settings that name the
	// store and never a credential; what a StoreRecord is kept against.
	destination string
	// quiet is why a store that is left untouched needs nothing.
	quiet string
	// fresh says the store was never written, as far as this binary can tell
	// without asking it: a SQL file that is not there.
	fresh bool
	// exclude is the measurements this sink is told never to write.
	exclude []string
	// prefix is the first node of every Graphite path.
	prefix string
}

// storesOf is every store the configuration writes, in the order of the
// configuration's own sinks block.
func storesOf(cfg *config.Config) []store {
	s := cfg.Sinks
	var out []store
	if i := s.Influx; i != nil {
		dest := "url=" + i.URL
		if i.Org != "" {
			dest += " org=" + i.Org
		}
		out = append(out, store{
			name: "influxdb", reach: asked,
			destination: dest + " bucket=" + i.Bucket, exclude: i.Exclude,
		})
	}
	if s.Prometheus != nil {
		out = append(out, store{
			name: "prometheus", reach: untouched,
			quiet: "the exporter holds today's values in memory, and this release starts them in its own shape",
		})
	}
	if s.OTLP != nil {
		out = append(out, store{
			name: "otlp", reach: untouched,
			quiet: "it pushes today's values, which this release sends in its own shape",
		})
	}
	if s.Loki != nil {
		out = append(out, store{
			name: "loki", reach: untouched,
			quiet: "a stream is its job, kind and configured labels, and a tag change moves none of them",
		})
	}
	if s.File != nil {
		out = append(out, store{
			name: "file", reach: untouched,
			quiet: "the file is a record of what was written when, and replayed into a store it brings back " +
				"every shape it holds",
		})
	}
	if s.Stdout {
		out = append(out, store{name: "stdout", reach: untouched, quiet: "nothing printed is kept"})
	}
	if t := s.Telegraf; t != nil {
		out = append(out, store{name: "telegraf", reach: recorded, destination: "url=" + t.URL})
	}
	if g := s.Graphite; g != nil {
		out = append(out, store{
			name: "graphite", reach: recorded,
			destination: "addr=" + g.Addr + " prefix=" + g.Prefix, prefix: g.Prefix,
		})
	}
	if q := s.SQL; q != nil {
		out = append(out, store{
			name: "sql", reach: recorded, destination: "path=" + q.Path,
			fresh: neverWritten(q.Path),
		})
	}
	if p := s.Postgres; p != nil {
		out = append(out, store{name: "postgres", reach: asked, destination: postgresDestination(p.DSN)})
	}
	if e := s.Elasticsearch; e != nil {
		out = append(out, store{
			name: "elasticsearch", reach: asked,
			destination: "url=" + e.URL + " prefix=" + e.Prefix,
		})
	}
	return out
}

// postgresDestination names the database a DSN reaches without the DSN, which
// is a secret: the host, the port, the database and the schema a search_path
// sets. The same database under another password is the same store.
func postgresDestination(dsn string) string {
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The sink cannot connect with it either, so there is no store to
		// keep a record of; a fixed word keeps the DSN out of the file.
		return "unreadable dsn"
	}
	dest := fmt.Sprintf("host=%s port=%s database=%s", c.Host, strconv.Itoa(int(c.Port)), c.Database)
	if schema := c.RuntimeParams["search_path"]; schema != "" {
		dest += " schema=" + schema
	}
	return dest
}

// neverWritten says whether a SQL file sink has written nothing yet: neither
// the file nor a rotation of it is there. Standard output cannot be looked at.
func neverWritten(path string) bool {
	if path == "" || path == "-" {
		return false
	}
	matches, err := filepath.Glob(path + "*")
	return err == nil && len(matches) == 0
}

// Stamp brings the state file's record of every store this configuration
// writes up to date, at the start of a run that is about to write to them.
//
// It has to run before the first sweep marks anything, because the first
// thing it reads is whether the state file has ever recorded a run: a file
// that has not is a first start, and every store is then first written by
// this release. It returns what is worth a warning.
func Stamp(state *run.State, cfg *config.Config, release string) []string {
	fresh := state.Fresh()
	// A state file that already keeps records was written by a release that
	// keeps them, so a store it has none for was added to the configuration
	// since, and this release is the first to write it.
	aware := len(state.Stores) > 0
	var warnings []string
	for _, s := range storesOf(cfg) {
		if s.reach == untouched {
			continue
		}
		rec := state.Stores[s.name]
		switch {
		case rec != nil && rec.Destination == s.destination:
		case rec == nil && (fresh || aware), s.fresh:
			rec = &run.StoreRecord{Destination: s.destination, FirstWrittenBy: release}
		default:
			// A state file from before the record, or a sink pointed at a
			// store this file has no history of: whoever wrote it first is
			// not known, and the planner reads that as older than anything.
			rec = &run.StoreRecord{Destination: s.destination}
		}
		if compareRelease(rec.WrittenBy, release) > 0 {
			warnings = append(warnings, fmt.Sprintf("the state file says %s was last written by %s, "+
				"newer than this %s; a release this old does not know every change a newer one made to "+
				"what it stores", s.name, rec.WrittenBy, release))
		}
		rec.WrittenBy = release
		state.Stores[s.name] = rec
	}
	return warnings
}

// quoted names a list the way a sentence does.
func quoted(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
