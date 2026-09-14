// Command ghchronicle collects every metric GitHub will give about an account
// and writes it where it can be charted.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/jmrplens/ghchronicle"
	"github.com/jmrplens/ghchronicle/internal/collect"
	"github.com/jmrplens/ghchronicle/internal/config"
	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/render"
	"github.com/jmrplens/ghchronicle/internal/run"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// version, commit and date are stamped at build time with -ldflags
// "-X main.version=... -X main.commit=... -X main.date=...". The Makefile, the
// Dockerfile and GoReleaser all stamp them, and they are the only place a
// release number is injected.
//
// They stay plain uninitialized strings on purpose. The linker's -X only
// reaches a variable that is uninitialized or initialized to a constant
// expression, so writing `var version = ghchronicle.Version` here would make
// every release stamp a silent no-op and the binary would report whatever the
// VERSION file said at the time it was compiled, tag or no tag. The fallback
// belongs in resolveBuild instead.
var (
	version string
	commit  string
	date    string
)

func init() {
	version, commit, date = resolveBuild(version, commit, date, debug.ReadBuildInfo)
}

// resolveBuild decides what this binary says about itself. A stamped value
// always wins, because that is what a release carries and it is the only one
// that can know a tag. Otherwise the version comes from the VERSION file the
// root package embeds, and the commit and the date from the VCS stamps the
// toolchain records in any build made inside a checkout, so an unstamped
// `go build` or `go run` is honest rather than "dev".
func resolveBuild(ldVersion, ldCommit, ldDate string,
	readBuildInfo func() (*debug.BuildInfo, bool),
) (v, c, d string) {
	v, c, d = ldVersion, ldCommit, ldDate
	if v == "" {
		v = ghchronicle.Version
	}
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return v, c, d
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if c == "" {
				c = s.Value
			}
		case "vcs.time":
			if d == "" {
				d = s.Value
			}
		}
	}
	return v, c, d
}

// buildLine is what -version prints: one line, because a release smoke test
// greps it and a bug report is pasted from it.
func buildLine() string {
	c, d := commit, date
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return fmt.Sprintf("ghchronicle %s (commit %s, built %s)", version, c, d)
}

// options is what the command line asked for, once parseFlags has read it.
// One value rather than a dozen loose pointers, so the run functions below can
// be handed the whole answer and take from it what they need.
type options struct {
	path     string
	once     bool
	list     bool
	showVer  bool
	backfill bool
	since    string
	card     string
	theme    string
	layout   string
	fields   string
	layouts  bool
	cardOnly bool
	groups   bool
}

// parseFlags reads the command line, args[0] being the program name the usage
// is printed under, the way flag.CommandLine names it.
//
// It parses a set of its own that continues on error rather than the global
// one that exits, so the exit is taken in execute, through exitProcess. The
// flag package has printed the reason and the usage to stderr either way.
func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.path, "config", "config.yaml", "path to the configuration file")
	fs.BoolVar(&o.once, "once", false, "run one sweep and exit")
	fs.BoolVar(&o.list, "list", false, "list the repositories that would be collected and exit")
	fs.BoolVar(&o.showVer, "version", false,
		"print the version, the commit and the build date, then exit")

	fs.BoolVar(&o.backfill, "backfill", false,
		"reach as far back as each surface allows, waiting for the rate limit to reset rather than stopping")
	fs.StringVar(&o.since, "backfill-since", "",
		"bound the backfill: a date (2024-01-01), a duration (720h), days (90d) or years (2y); empty means no bound")
	fs.StringVar(&o.card, "card", "", "run one sweep and write a summary SVG to this path")
	fs.StringVar(&o.theme, "card-theme", "auto", "card theme: dark, light or auto")
	fs.StringVar(&o.layout, "card-layout", "summary", "card layout; see -card-layouts")
	fs.StringVar(&o.fields, "card-fields", "",
		"comma-separated fields the card shows; empty means the layout's default")
	fs.BoolVar(&o.layouts, "card-layouts", false, "list the card layouts and their fields, then exit")
	fs.BoolVar(&o.groups, "groups", false, "list the metric groups and the families in each, then exit")
	fs.BoolVar(&o.cardOnly, "card-only", false, "with -card, write the SVG and nothing else")
	err := fs.Parse(args[1:])
	return o, err
}

// exitProcess is every exit execute takes, so a test can drive execute through
// each of its ends and read the status back instead of having the test binary
// end. execute returns right after calling it for the same reason: os.Exit
// never returns, and a test's replacement does.
//
// It is an exit rather than a status execute hands back on purpose. A fatal
// error leaves without closing the sinks or the log file, as it always has,
// and a returned status would run those deferred closes first.
var exitProcess = os.Exit

// notifyContext is how execute learns of SIGINT and SIGTERM. It is a variable
// so a test can end the serve loop the way SIGTERM does, by canceling a
// context of its own, without signaling the test binary: that is not
// something every platform the tests build on can do.
var notifyContext = signal.NotifyContext

func main() {
	execute(os.Args, os.Stdout, os.Stderr)
}

// execute is the whole command. The arguments and the two streams are passed
// in rather than taken from the process, so a test can drive every path
// through it; every exit goes through exitProcess.
func execute(args []string, stdout, stderr io.Writer) {
	o, goOn := readCommandLine(args, stdout, stderr)
	if !goOn {
		return
	}

	cfg, err := config.LoadWith(o.path, o.cardOnly)
	if err != nil {
		fatal(stderr, err)
		return
	}

	logger, logCloser := newLogger(cfg.Log, stderr)
	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}
	for _, w := range cfg.Warnings() {
		logger.Warn(w)
	}
	logSelection(cfg, logger)
	api := newAPI(cfg, o.backfill, logger)

	ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if o.list {
		if err = listRepositories(ctx, api, cfg, stdout); err != nil {
			fatal(stderr, err)
		}
		return
	}

	// A one-shot run exits when the sweep does, so an exporter it starts would
	// serve nobody, and starting one collides with the port a long-running
	// instance already holds. The push sinks all still run.
	oneShot := o.once || o.backfill || o.card != ""
	sinks, err := buildSinks(cfg, logger, oneShot)
	if err != nil {
		fatal(stderr, err)
		return
	}

	// One sweep, one SVG. This is the shape a GitHub Action wants: run it on a
	// schedule, commit the file into a profile README, and the card is drawn
	// from the same points the databases get rather than from a second pass
	// over the API.
	var accumulator *render.Accumulator
	if o.card != "" {
		accumulator = render.NewAccumulator(cfg.Targets.User)
		if o.cardOnly {
			// Nothing else runs, so a card can be produced with no database
			// configured at all.
			sinks = []sink.Sink{accumulator}
		} else {
			sinks = append(sinks, accumulator)
		}
	}
	defer func() {
		for _, s := range sinks {
			_ = s.Close()
		}
	}()

	runner := newRunner(cfg, api, sinks, logger, &o)
	switch {
	case o.backfill:
		err = runBackfill(ctx, runner, cfg, &o, logger)
	case o.once || o.card != "":
		err = runSweep(ctx, runner, accumulator, &o, logger)
	default:
		// Serve ends when the context does, and a stop asked for by a signal
		// is a clean exit, not a failure.
		if err = runner.Serve(ctx); err != nil && ctx.Err() != nil {
			err = nil
		}
	}
	if err != nil {
		fatal(stderr, err)
	}
}

// readCommandLine parses args and answers the flags that print and stop. It
// reports whether the run goes on; when it does not, any exit it needed has
// been taken.
func readCommandLine(args []string, stdout, stderr io.Writer) (options, bool) {
	o, err := parseFlags(args, stderr)
	if err != nil {
		// What flag.ExitOnError does: help asked for is a success, anything
		// else the flag package could not read is a 2.
		if errors.Is(err, flag.ErrHelp) {
			exitProcess(0)
		} else {
			exitProcess(2)
		}
		return o, false
	}
	return o, !printOnly(&o, stdout)
}

// newRunner is the sweep scheduler for the run the command line asked for.
func newRunner(cfg *config.Config, api *ghapi.Client, sinks []sink.Sink,
	logger *slog.Logger, o *options,
) *run.Runner {
	return &run.Runner{
		Cfg: cfg, API: api, Sinks: sinks,
		State: run.LoadState(cfg.StateFile), Log: logger,
		// Only the in-memory exporter needs it. A push sink has already
		// delivered what it collected, and a one-shot run is a full sweep by
		// definition.
		Prime: cfg.Sinks.Prometheus != nil && !cfg.Sinks.Prometheus.NoPrime && !o.once && o.card == "",
		// A backfill runs every family whatever the state says, because that
		// is the whole point of asking for one.
		Backfill: o.backfill,
	}
}

// printOnly answers the flags that print something and exit, and reports
// whether one of them was given, so that nothing else runs after it.
func printOnly(o *options, stdout io.Writer) bool {
	switch {
	case o.showVer:
		fmt.Fprintln(stdout, buildLine())
	case o.layouts:
		printLayouts(stdout)
	case o.groups:
		printGroups(stdout)
	default:
		return false
	}
	return true
}

// printLayouts lists the card layouts and the fields each one draws by default.
func printLayouts(w io.Writer) {
	for _, l := range render.Layouts() {
		fmt.Fprintf(w, "%-18s %-10s %s\n", l.Name, l.Family, l.Description)
		fmt.Fprintf(w, "%-18s %-10s default: %s\n", "", "", strings.Join(l.Fields, ", "))
	}
	fmt.Fprintln(w, "fields:", strings.Join(render.Fields(), ", "))
}

// printGroups lists the metric groups and the families in each, so the
// authoritative list is the binary and the documentation never has to repeat
// thirty-two names.
//
// It takes a writer rather than printing to standard output directly, because
// a list nothing can read back is a list nothing can test.
func printGroups(w io.Writer) {
	for _, group := range config.Groups() {
		fmt.Fprintf(w, "%-11s %s\n", group, config.GroupDescription(group))
		fmt.Fprintf(w, "%-11s %s\n", "", strings.Join(config.FamiliesIn(group), ", "))
	}
}

// logSelection says once, at start-up, which groups this run collects for.
//
// It names groups and not dashboard sections. A family feeds several sections
// and a section is fed by several families, so "Security will not render"
// would be false while repo still fills three of its panels; the prose in
// docs/configuration.md can say "some panels" honestly, and a log attribute
// cannot.
// A default start gains no line at all.
func logSelection(cfg *config.Config, logger *slog.Logger) {
	on, off, narrowed := cfg.Selection()
	if !narrowed {
		return
	}
	families := config.Families()
	running := 0
	for _, name := range families {
		if _, enabled := cfg.Interval(name); enabled {
			running++
		}
	}
	logger.Info("metric groups selected",
		"on", strings.Join(on, ","),
		"off", strings.Join(off, ","),
		"families", fmt.Sprintf("%d of %d", running, len(families)),
		"note", "the dashboards are generated for the full set of metrics; "+
			"the panels fed by a group that is off will fail, which is expected, see docs/configuration.md")
}

// newAPI is the GitHub client the whole run shares.
//
// The brake lives in the client because that is the only place that sees every
// request. One repository's workflow runs can be a thousand of them, and a
// brake checked between repositories is never reached in time. A backfill
// waits for the window; a sweep refuses and moves on.
func newAPI(cfg *config.Config, backfill bool, logger *slog.Logger) *ghapi.Client {
	api := ghapi.New(cfg.GitHub.Token, cfg.GitHub.HTTPTimeout())
	api.SetBaseURL(cfg.GitHub.BaseURL)
	api.SetReserve(cfg.GitHub.ReserveRate, backfill)
	api.OnWait = func(bucket string, d time.Duration) {
		logger.Info("budget spent, waiting for the window to reset",
			"bucket", bucket, "wait", d.Round(time.Second).String())
	}
	return api
}

// listRepositories prints what a sweep would collect and collects nothing.
// The archived repositories the filter sets aside follow, marked, because a
// sweep still writes the one row each has, the date it was archived, and a
// backfill collects them in full.
func listRepositories(ctx context.Context, api *ghapi.Client, cfg *config.Config, stdout io.Writer) error {
	found, err := collect.Discover(ctx, api, &collect.Filter{
		User: cfg.Targets.User, Orgs: cfg.Targets.Orgs, Repos: cfg.Targets.Repos,
		Exclude: cfg.Targets.Exclude, IncludeForks: cfg.Targets.IncludeForks,
		IncludeArchived: cfg.Targets.IncludeArchived, IncludePrivate: cfg.Targets.PrivateIncluded(),
	})
	if err != nil {
		return err
	}
	for _, r := range found.Repos {
		fmt.Fprintln(stdout, r.FullName)
	}
	for _, r := range found.Archived {
		fmt.Fprintln(stdout, r.FullName, "(archived: the archive date only; a backfill collects it)")
	}
	return nil
}

// runBackfill reaches as far back as each surface allows. The bound is the one
// asked for on the command line, or the configured one, or none at all, in
// which case it stops where the API does.
//
// A sweep cut short by a signal is not a failure: what it reached is written,
// and the log says the backfill finished.
func runBackfill(ctx context.Context, runner *run.Runner, cfg *config.Config,
	o *options, logger *slog.Logger,
) error {
	runner.Prime = true
	bound := cfg.Backfill.Since
	if o.since != "" {
		bound = o.since
	}
	var err error
	runner.BackfillSince, err = config.Backfill{Since: bound}.SinceTime(time.Now())
	if err != nil {
		return err
	}
	if runner.BackfillSince.IsZero() {
		logger.Info("backfill has no lower bound; it stops where the API does")
	} else {
		logger.Info("backfill bounded", "since", runner.BackfillSince.Format("2006-01-02"))
	}
	logger.Info("backfill starting, this reaches as far back as GitHub allows and may take a while")
	if err = runner.Once(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	logger.Info("backfill finished")
	return nil
}

// runSweep runs one sweep and, when one was asked for, draws the card from
// what that sweep collected.
func runSweep(ctx context.Context, runner *run.Runner, accumulator *render.Accumulator,
	o *options, logger *slog.Logger,
) error {
	if err := runner.Once(ctx); err != nil {
		return err
	}
	if accumulator == nil {
		return nil
	}
	opts := &render.Options{Theme: o.theme, Layout: o.layout}
	if o.fields != "" {
		opts.Fields = strings.Split(o.fields, ",")
		for i := range opts.Fields {
			opts.Fields[i] = strings.TrimSpace(opts.Fields[i])
		}
	}
	built := accumulator.Card()
	if err := writeCard(o.card, &built, opts); err != nil {
		return err
	}
	logger.Info("card written", "path", o.card, "theme", o.theme)
	return nil
}

func buildSinks(cfg *config.Config, log *slog.Logger, oneShot bool) ([]sink.Sink, error) {
	var out []sink.Sink

	// One ledger, shared by the stores that keep history. It answers per sink,
	// so a destination that was down still gets everything on its next write.
	// A one-shot run never opens it: it writes once and exits, so there is
	// nothing to save, and a card render must not be able to prune it.
	var ledger *sink.Ledger
	if !oneShot && cfg.Sinks.DedupeFile != "off" {
		ledger = sink.LoadLedger(cfg.Sinks.DedupeFile, cfg.Sinks.DedupeAge(), 0)
		log.Debug("loaded the written-points ledger",
			"file", cfg.Sinks.DedupeFile, "points", ledger.Len())
	}
	// dedupe returns the ledger a sink should use: its own setting, defaulting
	// to on, and nil when the ledger is off altogether.
	dedupe := func(enabled *bool) *sink.Ledger {
		if enabled != nil && !*enabled {
			return nil
		}
		return ledger
	}
	if i := cfg.Sinks.Influx; i != nil {
		influx := sink.NewInflux(i.URL, i.Token, i.Org, i.Bucket, i.Batch, cfg.GitHub.HTTPTimeout())
		influx.Exclude = map[string]bool{}
		for _, m := range i.Exclude {
			influx.Exclude[m] = true
		}
		influx.OnReject = func(line string) {
			log.Error("influxdb rejected a line as unparseable", "line", line)
		}
		out = append(out, sink.OnlyChanged(influx, dedupe(i.Dedupe)))
	}
	if o := cfg.Sinks.OTLP; o != nil {
		otlp := sink.NewOTLP(o.Endpoint, o.Service, o.Headers, o.Raw, o.Batch, cfg.GitHub.HTTPTimeout())
		otlp.Repeat = o.RepeatEvery()
		otlp.Start()
		out = append(out, otlp)
	}
	if l := cfg.Sinks.Loki; l != nil {
		out = append(out, sink.NewLoki(l.URL, l.TenantID, l.Labels, l.Batch, l.Age(), cfg.GitHub.HTTPTimeout()))
	}
	if f := cfg.Sinks.File; f != nil {
		out = append(out, sink.NewFile(f.Path, f.Format, f.MaxBytes, f.Keep))
	}
	if p := cfg.Sinks.Prometheus; p != nil && !oneShot {
		exporter := sink.NewProm(p.Listen, p.Path)
		// Started here rather than lazily, so a port already in use is an
		// error at start-up instead of a silently missing exporter.
		if err := exporter.Start(); err != nil {
			return nil, fmt.Errorf("prometheus exporter: %w", err)
		}
		out = append(out, exporter)
	}
	if t := cfg.Sinks.Telegraf; t != nil {
		out = append(out, sink.OnlyChanged(
			sink.NewTelegraf(t.URL, t.Username, t.Password, t.Batch, cfg.GitHub.HTTPTimeout()),
			dedupe(t.Dedupe),
		))
	}
	if g := cfg.Sinks.Graphite; g != nil {
		out = append(out, sink.OnlyChanged(
			sink.NewGraphite(g.Addr, g.Prefix, g.Batch, cfg.GitHub.HTTPTimeout()),
			dedupe(g.Dedupe),
		))
	}
	if s := cfg.Sinks.SQL; s != nil {
		out = append(out, sink.OnlyChanged(sink.NewSQL(s.Dialect, s.Path, s.MaxBytes, s.Keep), dedupe(s.Dedupe)))
	}
	if e := cfg.Sinks.Elasticsearch; e != nil {
		es := sink.NewElasticsearch(e.URL, e.Prefix, e.Username, e.Password, e.APIKey, e.Batch, cfg.GitHub.HTTPTimeout())
		es.OnReject = func(reason string) {
			log.Error("elasticsearch rejected a document", "reason", reason)
		}
		out = append(out, sink.OnlyChanged(es, dedupe(e.Dedupe)))
	}
	if cfg.Sinks.Stdout {
		if cfg.Sinks.StdoutFormat == "json" {
			out = append(out, sink.NewStdoutJSON())
		} else {
			out = append(out, sink.NewStdout())
		}
	}
	return out, nil
}

// writeCard renders the SVG through a temporary file, so a reader watching the
// path in a repository never sees a half-written document.
func writeCard(path string, c *render.Card, opts *render.Options) error {
	svg, err := render.SVG(c, opts)
	if err != nil {
		return err
	}
	return replaceFile(path, svg)
}

// replaceFile writes data next to path and renames it into place. A rename
// within one directory is atomic, so the path holds either the old file or the
// whole new one.
//
// The path is the one -card was given, and it is cleaned before anything is
// written so that it means what it says on every system. Windows resolves a
// `..` before it looks at the disk, while Linux and macOS walk every step and
// fail on one that does not exist; read as written, `out/../card.svg` is the
// card beside out on all three, whether or not out is there.
func replaceFile(path string, data []byte) error {
	path = filepath.Clean(path)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newLogger is the run's logger, writing to stderr and, when the config names
// a file, to that as well.
func newLogger(l config.Log, stderr io.Writer) (*slog.Logger, io.Closer) {
	level := slog.LevelInfo
	switch l.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	out := stderr
	var closer io.Closer
	if l.File != "" {
		// Both, not instead: under systemd the journal is where anyone looks
		// first, and a log file that silently replaced it would be a trap.
		w := sink.NewLogWriter(l.File, l.MaxBytes, l.Keep)
		out, closer = sink.Tee(stderr, w), w
	}

	opts := &slog.HandlerOptions{Level: level}
	if l.Format == "json" {
		return slog.New(slog.NewJSONHandler(out, opts)), closer
	}
	return slog.New(slog.NewTextHandler(out, opts)), closer
}

// fatal reports what stopped the run and exits with 1, leaving the sinks and
// the log file as they are; see exitProcess.
func fatal(stderr io.Writer, err error) {
	fmt.Fprintln(stderr, "ghchronicle:", err)
	exitProcess(1)
}
