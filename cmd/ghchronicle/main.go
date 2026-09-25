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
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/jmrplens/ghchronicle/v2"
	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/render"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
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

// self is what this binary says about itself, settled once from the stamps
// above and from what the toolchain recorded.
var self buildFacts

func init() {
	self = resolveBuild(version, commit, date, debug.ReadBuildInfo)
	// The rest of the command reads the release number by this name: the
	// backfill checkpoint records it.
	version = self.version
}

// buildFacts is where a binary came from, as far as it can tell.
type buildFacts struct {
	version, commit, date string
	// module is the version the go command gave the main module, kept only
	// when no commit is known. That is a `go install ...@v2.5.1`: the module
	// arrives as a checksummed zip with no checkout in it, so there is no
	// revision and no commit time to read, but the version that was asked for
	// is recorded. For a pseudo-version (@main) it also names the commit and
	// its time, in the go command's words rather than a guess of ours.
	module string
	// toolchain is the Go release that compiled a module build. A release is
	// compiled by the Go its workflow pins; a `go install` by whatever the
	// user has, which is the one fact about that binary nobody else knows.
	toolchain string
}

// resolveBuild decides what this binary says about itself. A stamped value
// always wins, because that is what a release carries and it is the only one
// that can know a tag. Otherwise the version comes from the VERSION file the
// root package embeds, and the commit and the date from the VCS stamps the
// toolchain records in any build made inside a checkout, so an unstamped
// `go build` is honest rather than "dev". With neither, the main module's
// version is the best answer left. `go run`, `-buildvcs=false` and the plain
// Dockerfile (whose context leaves .git out) record it as "(devel)", which
// says nothing, so those still report nothing.
func resolveBuild(ldVersion, ldCommit, ldDate string,
	readBuildInfo func() (*debug.BuildInfo, bool),
) buildFacts {
	b := buildFacts{version: ldVersion, commit: ldCommit, date: ldDate}
	if b.version == "" {
		b.version = ghchronicle.Version
	}
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if b.commit == "" {
				b.commit = s.Value
			}
		case "vcs.time":
			if b.date == "" {
				b.date = s.Value
			}
		}
	}
	if b.commit == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		b.module = info.Main.Version
		b.toolchain = info.GoVersion
		if b.toolchain == "" {
			b.toolchain = runtime.Version()
		}
	}
	return b
}

// line is what -version prints: one line, because a release smoke test greps
// it and a bug report is pasted from it. The smoke tests match the leading
// "ghchronicle <version> ", so every shape keeps it.
func (b buildFacts) line() string {
	if b.module != "" {
		return fmt.Sprintf("ghchronicle %s (module %s, built with %s)", b.version, b.module, b.toolchain)
	}
	c, d := b.commit, b.date
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return fmt.Sprintf("ghchronicle %s (commit %s, built %s)", b.version, c, d)
}

// buildLine is the line for the running binary.
func buildLine() string {
	return self.line()
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
	// setup asks what a working configuration needs and writes it, which is
	// the shortest honest answer to "how do I start".
	setup bool
	// uninstall names what to take away, and yes is the word that makes it
	// happen: without it the run prints the list and removes nothing, because
	// the alternative is a typo that empties a store.
	uninstall string
	yes       bool
	// publishDashboard reconciles the Grafana datasource and dashboard for
	// every store this writes to, then exits. Like backfillStatus it asks
	// GitHub nothing, so it needs no token.
	publishDashboard bool
	// backfillStatus reads the checkpoint and prints it, and is the one run
	// that neither asks GitHub anything nor writes anywhere.
	backfillStatus bool
	// retry is how long a backfill waits before going back for the families a
	// pass left behind. Zero never goes back, which is what it did before
	// this existed.
	retry    time.Duration
	since    string
	card     string
	theme    string
	layout   string
	fields   string
	motion   string
	width    int
	speed    float64
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
	fs.BoolVar(&o.setup, "setup", false,
		"ask what a working configuration needs, check each answer, and write it")
	fs.StringVar(&o.uninstall, "uninstall", "",
		"remove what this put in place and exit: "+strings.Join(uninstallTargets, ", ")+
			", comma separated; prints the list and removes nothing without -yes")
	fs.BoolVar(&o.yes, "yes", false,
		"go ahead with -uninstall rather than only listing what it would remove")
	fs.BoolVar(&o.publishDashboard, "publish-dashboard", false,
		"publish the Grafana dashboard and the datasource it reads from, then exit; "+
			"needs the grafana section of the config and asks GitHub nothing")
	fs.BoolVar(&o.backfillStatus, "backfill-status", false,
		"print how far the backfill in progress has got, and exit; asks GitHub nothing and writes nothing")
	fs.DurationVar(&o.retry, "backfill-retry", 0,
		"after a backfill ends with families left, wait this long and go back for them, until a pass records nothing new; zero does not go back")
	fs.StringVar(&o.card, "card", "", "run one sweep and write a summary SVG to this path")
	fs.StringVar(&o.theme, "card-theme", "auto",
		"card theme: dark, light, auto, or both to write the light card at -card and the dark one beside it with _dark before the extension")
	fs.StringVar(&o.layout, "card-layout", "summary", "card layout; see -card-layouts")
	fs.StringVar(&o.fields, "card-fields", "",
		"comma-separated fields the card shows; empty means the layout's default")
	fs.StringVar(&o.motion, "card-motion", render.MotionOnce,
		"how an animated layout moves: once, loop or off; a layout that does not move ignores it")
	fs.IntVar(&o.width, "card-width", 0,
		"card width in pixels; 0 draws the layout at its own width. Each layout draws between two ends of "+
			"its own and refuses anything outside them, and -card-layouts states both. Only "+
			"activity-heatmap turns the room into more data, one more week of the contribution calendar at "+
			"a time until the year it holds is drawn, which is also where its far end is; every other "+
			"layout spreads the same content wider. badge-row ignores it: its width follows its pills")
	fs.Float64Var(&o.speed, "card-speed", render.SpeedDefault,
		"how fast an animated layout plays, as a decimal from 0 to 1: 0 is the slowest animation and 1 the "+
			"fastest, and 0.5 is the pace the cards have always been drawn at, to the byte. Every animated "+
			"layout scales together, the continuous motions with the rest. 0 is not a still card: "+
			"-card-motion off is what draws one")
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

	if ranBeforeConfig(o, stdout, stderr) {
		return
	}

	cfg, err := config.LoadWith(o.path, config.Relax{
		NoSinks: o.cardOnly,
		NoToken: o.backfillStatus || o.publishDashboard || o.uninstall != "",
	})
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

	if reported(ctx, o, cfg, api, stdout, stderr) {
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

	accumulator, sinks := withCard(cfg, o, sinks)
	defer func() {
		for _, s := range sinks {
			_ = s.Close()
		}
	}()

	publishOnStart(ctx, cfg, o, logger)

	runner := newRunner(cfg, api, sinks, logger, &o)
	switch {
	case o.backfill:
		err = runBackfill(ctx, runner, cfg, accumulator, &o, logger)
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
		// Only the in-memory exporter needs it, and only when it will serve:
		// a push sink has already delivered what it collected, and -once
		// starts no exporter to fill. What -once does not do is collect every
		// family: a family that is not due is skipped there as in any sweep,
		// which is why a card asks for Card below rather than riding on this.
		Prime: cfg.Sinks.Prometheus != nil && !cfg.Sinks.Prometheus.NoPrime && !o.once && o.card == "",
		// A backfill runs every family whatever the state says, because that
		// is the whole point of asking for one.
		Backfill: o.backfill,
		// A card is drawn from one sweep, so that sweep runs every family
		// whatever the state says; and when it feeds nothing but the card it
		// leaves the state file alone. See run.Runner's Card and CardOnly.
		Card:     o.card != "",
		CardOnly: o.card != "" && o.cardOnly,
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
		fmt.Fprintf(w, "%-18s %-10s %s\n", "", "", layoutWidth(l))
		fmt.Fprintf(w, "%-18s %-10s default: %s\n", "", "", strings.Join(l.Fields, ", "))
	}
	fmt.Fprintln(w, "fields:", strings.Join(render.Fields(), ", "))
}

// layoutWidth is the width line -card-layouts prints under a layout, which is
// what a reader needs before -card-width is any use to him: the width he gets
// if he says nothing, and the two ends of what the layout accepts.
func layoutWidth(l render.Layout) string {
	if l.Width == 0 {
		return "width: follows its content"
	}
	return fmt.Sprintf("width: %d, from %d to %d", l.Width, l.MinWidth, l.MaxWidth)
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

// reported runs the flags that answer a question and return, and says whether
// one of them did.
//
// Together rather than as two branches of execute, which each new one would
// grow by two: they have the same shape, they write to stdout, and the run
// ends after them.
// ranBeforeConfig handles the modes that come before the configuration is
// read, and says whether one of them did.
//
// Only -setup so far, and its whole reason is that one: the ordinary time to
// run it is when there is no configuration yet, so loading one first would
// refuse the very case it exists for.
func ranBeforeConfig(o options, stdout, stderr io.Writer) bool {
	if !o.setup {
		return false
	}
	if err := setupFrom(context.Background(), o, os.Stdin, stdout); err != nil {
		fatal(stderr, err)
	}
	return true
}

// withCard adds the thing that draws the SVG to the destinations, when one
// was asked for.
//
// One sweep, one SVG. This is the shape a GitHub Action wants: run it on a
// schedule, commit the file into a profile README, and the card is drawn from
// the same points the databases get rather than from a second pass over the
// API.
func withCard(cfg *config.Config, o options, sinks []sink.Sink) (*render.Accumulator, []sink.Sink) {
	if o.card == "" {
		return nil, sinks
	}
	accumulator := render.NewAccumulator(cfg.Targets.User)
	if o.cardOnly {
		// Nothing else runs, so a card can be produced with no database
		// configured at all.
		return accumulator, []sink.Sink{accumulator}
	}
	return accumulator, append(sinks, accumulator)
}

func reported(ctx context.Context, o options, cfg *config.Config,
	api *ghapi.Client, stdout, stderr io.Writer,
) bool {
	var err error
	switch {
	case o.backfillStatus:
		// This run asks GitHub nothing: it reads the file a backfill leaves
		// behind and prints it.
		err = reportBackfill(cfg, o.path, stdout, time.Now())
	case o.publishDashboard:
		// This one asks GitHub nothing either. It talks to Grafana instead.
		err = publishDashboards(ctx, cfg, stdout)
	case o.uninstall != "":
		// Nor this one, which takes away rather than collects.
		err = uninstall(ctx, cfg, o.uninstall, o.yes, stdout)
	case o.list:
		err = listRepositories(ctx, api, cfg, stdout)
	default:
		return false
	}
	if err != nil {
		fatal(stderr, err)
	}
	return true
}

// listRepositories prints what a sweep would collect and collects nothing.
// retryLimit is how many passes a backfill will make in one run.
//
// A backstop and not a tuning knob: what actually stops a retrying backfill is
// a pass that records nothing new, and that catches the case worth catching,
// which is an obstacle no amount of waiting clears. This is here so that a
// walk which creeps forward by one repository a pass cannot run for a week
// unattended.
const retryLimit = 10

// verdict is what to do after a backfill pass.
type verdict int

const (
	// stop: the walk covered everything, or it was stopped, or going back was
	// never asked for.
	stop verdict = iota
	// stuck: the pass recorded nothing new, so waiting will not help.
	stuck
	// atLimit: there is work left and passes have run out.
	atLimit
	// again: wait, then go back for what is left.
	again
)

// afterPass decides what a backfill does once a pass has ended.
//
// A function of its own, and of four plain values, because this is the whole
// of the thinking: the loop around it only waits. stopped is a run that was
// asked to end, left is the families the checkpoint still does not hold,
// retry is what the reader asked for, and gained says the pass recorded
// something it had not before.
//
// It decides by what the checkpoint gained rather than by the errors the pass
// reported. Sorting errors into the transient and the permanent means a list
// that is wrong the moment GitHub answers something new; "did this pass record
// anything" needs no list and answers the question that matters, which is
// whether coming back has any chance of helping.
func afterPass(stopped bool, left []string, retry time.Duration, gained bool, passes int) verdict {
	switch {
	case stopped || len(left) == 0 || retry <= 0:
		return stop
	case !gained:
		return stuck
	case passes >= retryLimit:
		return atLimit
	default:
		return again
	}
}

// walkUntilDoneOrStuck runs the walk, and for as long as retry asks, goes back
// for whatever a pass left behind.
//
// A pass can end with families left and no error at all: a family truncated by
// a secondary rate limit is handed back as a pass, and one that failed on every
// repository is deliberately left unmarked. Both are usually a bad few minutes
// at the other end rather than anything about this account, and the checkpoint
// makes going back cheap, because a resume walks only what it does not already
// hold.
func walkUntilDoneOrStuck(ctx context.Context, runner *run.Runner,
	retry time.Duration, logger *slog.Logger,
) error {
	for passes := 1; ; passes++ {
		families, repos := runner.Progress.Recorded()
		if err := runner.Once(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		left := runner.Progress.Unfinished()
		after, afterRepos := runner.Progress.Recorded()
		gained := after != families || afterRepos != repos
		switch afterPass(ctx.Err() != nil, left, retry, gained, passes) {
		case stop:
			return nil
		case stuck:
			logger.Info("this pass recorded nothing new, so waiting will not help; not going back again",
				"families_left", strings.Join(left, ","), "passes", passes)
			return nil
		case atLimit:
			logger.Warn("stopping after the pass limit, with families still left",
				"families_left", strings.Join(left, ","), "passes", passes,
				"resume", "run the same command again")
			return nil
		case again:
		}
		logger.Info("waiting before going back for the families this pass left",
			"families_left", strings.Join(left, ","), "waiting", retry.String(), "pass", passes+1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
		// The pass just run marked the families it collected, so without this
		// the next one honors those cadences and skips the family it came
		// back for.
		runner.PrimeAgain()
	}
}

// reportBackfill prints how far the backfill in progress has got.
//
// It reads the checkpoint and nothing else: no request, no sink, no write, and
// no checkpoint created for a walk nobody is running. The file was always
// meant to be read, and until this existed reading it meant knowing the path
// and parsing JSON by hand.
//
// Success either way. "There is no backfill in progress" is an answer, and a
// status command that exits non-zero for an ordinary state is one nobody can
// put in a script.
func reportBackfill(cfg *config.Config, configPath string, stdout io.Writer, now time.Time) error {
	path := cfg.BackfillProgressFile()
	progress, inProgress, err := run.ReadProgress(path)
	if err != nil {
		return err
	}
	if !inProgress {
		fmt.Fprintln(stdout, "no backfill in progress")
		fmt.Fprintf(stdout, "  the checkpoint one leaves behind is not there: %s\n", shownPath(path))
		return nil
	}

	left := progress.Unfinished()
	families, inFlight, repos := progress.Where()
	fmt.Fprintln(stdout, "backfill in progress")
	fmt.Fprintf(stdout, "  started      %s (%s ago)\n",
		progress.Started.Format(time.RFC3339), since(progress.Started, now))
	fmt.Fprintf(stdout, "  last written %s ago\n", since(progress.Updated, now))
	fmt.Fprintf(stdout, "  families     %d of %d complete\n", families, len(progress.Scope.Families))
	if inFlight != "" {
		fmt.Fprintf(stdout, "  in flight    %s, %d repositories written\n", inFlight, repos)
	}
	if len(left) > 0 {
		fmt.Fprintf(stdout, "  left         %s\n", strings.Join(left, ", "))
	}
	fmt.Fprintf(stdout, "  written by   %s\n", progress.WrittenBy)
	fmt.Fprintf(stdout, "  checkpoint   %s\n", path)
	fmt.Fprintf(stdout, "  resume       ghchronicle -config %s -backfill\n", configPath)
	return nil
}

// since is how long ago an instant was, rounded to the second, and never
// negative: a clock that moved backwards is not worth reporting as the future.
func since(then, now time.Time) time.Duration {
	d := now.Sub(then).Round(time.Second)
	if d < 0 {
		return 0
	}
	return d
}

// shownPath names the file, or says there is none to name: a configuration
// with no state file keeps no checkpoint, and "" in a sentence reads as a bug.
func shownPath(path string) string {
	if path == "" {
		return "there is none, because this configuration sets no state_file"
	}
	return path
}

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
// the checkpoint keeps the repositories it had already delivered, and the log
// says where it stopped. Running the same command again carries on from there.
func runBackfill(ctx context.Context, runner *run.Runner, cfg *config.Config,
	accumulator *render.Accumulator, o *options, logger *slog.Logger,
) error {
	runner.Prime = true
	files, err := planCards(accumulator, o)
	if err != nil {
		return err
	}
	bound := cfg.Backfill.Since
	if o.since != "" {
		bound = o.since
	}
	runner.BackfillSince, err = config.Backfill{Since: bound}.SinceTime(time.Now())
	if err != nil {
		return err
	}
	// Opened before anything is collected, because the one thing it can say is
	// that this walk must not be resumed, and a refusal is only worth
	// something before the quota is spent. The bound goes in as it was
	// spelled, not as it just resolved: see run.Scope.
	if runner.Progress, err = run.OpenProgress(
		cfg.BackfillProgressFile(), version, run.ScopeOf(cfg, bound), time.Now(),
	); err != nil {
		return err
	}
	if runner.BackfillSince.IsZero() {
		logger.Info("backfill has no lower bound; it stops where the API does")
	} else {
		logger.Info("backfill bounded", "since", runner.BackfillSince.Format("2006-01-02"))
	}
	logger.Info("backfill starting, this reaches as far back as GitHub allows and may take a while",
		"checkpoint", runner.Progress.Path())
	// Its own name: reusing err here is a re-assignment one linter wants
	// written as a declaration and another reads as shadowing the one above.
	if walked := walkUntilDoneOrStuck(ctx, runner, o.retry, logger); walked != nil {
		return walked
	}
	logger.Info("backfill finished")
	// A backfill asked for a card draws it from what the backfill collected,
	// the same way a sweep does, and a backfill cut short by a signal draws it
	// from what it reached: its points have been written, and a card is a
	// picture of those points.
	//
	// A resumed one is the case where that sentence needs saying out loud. The
	// accumulator is a sink, so it holds what this process collected and not
	// what the process before it did: the families the first half finished are
	// skipped, and the card is drawn without them. Said rather than refused,
	// because a partial card is still a picture of real points and the reader
	// is the one who knows whether that will do.
	if o.card != "" && runner.Progress.Resumed() {
		logger.Warn("this card is drawn from the part of the walk this process did; "+
			"the families the walk it resumes had already finished are not in it",
			"complete_before_this_process", len(runner.Progress.Complete))
	}
	return writeCards(accumulator, files, o, logger)
}

// runSweep runs one sweep and, when one was asked for, draws the card from
// what that sweep collected. Under -card-theme both that is the light card at
// -card and the dark one beside it with _dark before the extension, both from
// the one sweep, so the two can never disagree about the moment they show.
func runSweep(ctx context.Context, runner *run.Runner, accumulator *render.Accumulator,
	o *options, logger *slog.Logger,
) error {
	files, err := planCards(accumulator, o)
	if err != nil {
		return err
	}
	if sweepErr := runner.Once(ctx); sweepErr != nil {
		return sweepErr
	}
	return writeCards(accumulator, files, o, logger)
}

// planCards settles the files a run writes before it collects anything, and
// returns none for a run that was not asked for a card. Checked this early
// because a typo in the theme, the motion or a field found after the sweep
// has already spent the rate limit; see checkCardFiles.
func planCards(accumulator *render.Accumulator, o *options) ([]cardFile, error) {
	if accumulator == nil {
		return nil, nil
	}
	files := cardFiles(o.card, o.theme)
	if err := checkCardFiles(o, files); err != nil {
		return nil, err
	}
	return files, nil
}

// writeCards draws every file planCards named from what the run collected, so
// a sweep and a backfill write the same card in the same way: one accumulator,
// the options already checked, and under -card-theme both the light card at
// -card and the dark one beside it.
func writeCards(accumulator *render.Accumulator, files []cardFile,
	o *options, logger *slog.Logger,
) error {
	if accumulator == nil {
		return nil
	}
	built := accumulator.Card()
	for _, f := range files {
		if err := writeCard(f.path, &built, f.opts); err != nil {
			return err
		}
		logger.Info("card written", "path", f.path, "theme", f.theme, "motion", o.motion)
	}
	return nil
}

// splitFields turns the comma-separated -card-fields flag into the trimmed
// field names render.Options takes; empty stays nil so the layout's default
// applies.
func splitFields(fields string) []string {
	if fields == "" {
		return nil
	}
	out := strings.Split(fields, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

// checkCardFiles settles each file's options and renders a placeholder card
// with them before the sweep, because a typo in the theme, the motion or a
// field found after the sweep has already spent the rate limit on a card that
// was never going to be written. It sets files[i].opts rather than building a
// second value later: the write loop draws with this exact *render.Options,
// never a value cardOptions was asked to build again, so what is checked here
// is what is drawn, not something that merely looks the same today.
func checkCardFiles(o *options, files []cardFile) error {
	for i := range files {
		files[i].opts = cardOptions(o, files[i].theme)
		if _, err := render.SVG(&render.Card{Login: "check"}, files[i].opts); err != nil {
			return err
		}
	}
	return nil
}

// cardFile is one SVG a run writes, the palette it is drawn in, and the
// options it is drawn with. opts starts nil: cardFiles only names the files
// -card-theme asks for, and checkCardFiles fills it in from the one call to
// cardOptions that both the placeholder check and the later write read from,
// so the two can never draw from different options.
type cardFile struct {
	path, theme string
	opts        *render.Options
}

// cardFiles is what -card-theme asks for: one file, or under both the light
// card at the path given and the dark one beside it with _dark before the
// extension, from the same sweep.
func cardFiles(path, theme string) []cardFile {
	if theme != "both" {
		return []cardFile{{path: path, theme: theme}}
	}
	ext := filepath.Ext(path)
	return []cardFile{
		{path: path, theme: "light"},
		{path: strings.TrimSuffix(path, ext) + "_dark" + ext, theme: "dark"},
	}
}

// cardOptions turns the command line into the renderer's options for one theme.
func cardOptions(o *options, theme string) *render.Options {
	return &render.Options{
		Theme: theme, Layout: o.layout, Motion: o.motion,
		Fields: splitFields(o.fields), Width: o.width, Speed: &o.speed,
	}
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
		if !config.Enabled(enabled) {
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
	if s := cfg.Sinks.Postgres; s != nil {
		out = append(out, sink.OnlyChanged(sink.NewPostgres(s.DSN, s.Batch), dedupe(s.Dedupe)))
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
	out := stderr
	var closer io.Closer
	if l.File != "" {
		// Both, not instead: under systemd the journal is where anyone looks
		// first, and a log file that silently replaced it would be a trap.
		w := sink.NewLogWriter(l.File, l.MaxBytes, l.Keep)
		out, closer = sink.Tee(stderr, w), w
	}

	opts := &slog.HandlerOptions{Level: l.SlogLevel()}
	if l.UsesJSON() {
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
