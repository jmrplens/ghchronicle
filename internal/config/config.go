// Package config loads and validates ghchronicle's configuration.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of the tool's settings.
type Config struct {
	GitHub  GitHub  `yaml:"github"`
	Targets Targets `yaml:"targets"`
	Sinks   Sinks   `yaml:"sinks"`
	Every   Every   `yaml:"every"`
	// Heartbeat forces how often the sweep loop wakes to ask which families
	// are due. It is not a cadence: it sets no family's interval and is not
	// compared against the built-in table. Empty means the runner derives it
	// from the shortest cadence, which is what production wants; a test run
	// that needs the loop to turn faster than any cadence sets it here.
	Heartbeat string `yaml:"heartbeat" ghc:"example=15s"`
	// Groups narrows a sweep to the named groups of families. Absent means
	// every group, which is what "collects every metric GitHub exposes" has
	// always meant. A pointer, because `groups: []` and no key at all mean
	// opposite things and a nil slice cannot tell them apart.
	Groups *[]string `yaml:"groups" ghc:"example=audience, account, repos"`
	Log    Log       `yaml:"log"`
	// StateFile remembers when each family last ran and which repositories
	// have already had their one-off full star walk.
	StateFile string `yaml:"state_file" ghc:"example=/var/lib/ghchronicle/state.json"`

	// Backfill settings. They only apply to a run started with -backfill.
	Backfill Backfill `yaml:"backfill"`

	// Grafana is where the dashboard and the datasource it reads from are
	// published. A pointer because the whole feature is opt-in: without the
	// key the binary never talks to a Grafana, which is how it behaved before
	// it could.
	Grafana *Grafana `yaml:"grafana"`

	// AllowNoSinks lets a caller that supplies its own destination pass
	// validation with none configured. `-card-only` is the case: it renders an
	// SVG and needs no database at all. Not a YAML setting, because a config
	// file with no sink is a mistake rather than an intention.
	AllowNoSinks bool `yaml:"-"`

	// AllowNoToken lets a caller that makes no request pass validation with no
	// credential. `-backfill-status` is the case: it reads the checkpoint file
	// and prints it, and requiring a token to do that would put the status of
	// a run behind the credential the run needs rather than the one the reader
	// has. Not a YAML setting, for the same reason as AllowNoSinks.
	AllowNoToken bool `yaml:"-"`

	intervals map[string]time.Duration
	// everySource records which config key gave each family its interval, so
	// a warning can point at the knob to turn rather than at the family.
	everySource map[string]string
	heartbeat   time.Duration
	// selected is the resolved group choice, kept so the start-up line can
	// report it without normalizing the user's list a second time. Nil means
	// every group.
	selected map[string]bool
	// notes are the things worth saying about a configuration that is legal.
	// Validate has no logger, and main has one only after the config is loaded.
	notes []string
}

// Backfill bounds how far back a backfill reaches.
type Backfill struct {
	// Since is a date (2024-01-01), a duration (720h, 90d, 2y) or empty for
	// no bound at all. No bound means the walk stops only when the API does,
	// however many hours that takes.
	Since string `yaml:"since" ghc:"example=2y"`
}

// SinceTime resolves Since against now. Zero means unbounded.
func (b Backfill) SinceTime(now time.Time) (time.Time, error) {
	return parseSince(b.Since, now)
}

// parseSince accepts a date, a Go duration, or a duration in days or years,
// which Go's parser does not know and people always reach for first.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "unlimited", "all", "0":
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if n, unit, ok := strings.Cut(s, "d"); ok && unit == "" {
		if days, err := strconv.Atoi(n); err == nil {
			return now.AddDate(0, 0, -days), nil
		}
	}
	if n, unit, ok := strings.Cut(s, "y"); ok && unit == "" {
		if years, err := strconv.Atoi(n); err == nil {
			return now.AddDate(-years, 0, 0), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("backfill.since: %q is not a date, a duration, or a count of days or years", s)
	}
	return now.Add(-d), nil
}

type GitHub struct {
	// Token may be given inline or, preferably, as ${GITHUB_TOKEN}.
	Token string `yaml:"token" ghc:"secret,required,example=${GITHUB_TOKEN}"`
	// BaseURL is the API root. Empty means api.github.com; a GitHub Enterprise
	// instance uses https://<host>/api/v3.
	BaseURL string `yaml:"base_url" ghc:"example=https://github.example.com/api/v3"`
	// WebURL is the site the profile page is on, for the one family that
	// reads a page rather than the API. Empty means it is derived from
	// BaseURL, which is right for api.github.com and for GitHub Enterprise
	// and wrong for a proxy in front of the API, whose root says nothing
	// about where the site is.
	WebURL  string `yaml:"web_url" ghc:"example=https://github.example.com"`
	Timeout string `yaml:"timeout" ghc:"example=30s"`

	// ReserveRate is the number of API calls never spent. The collector stops
	// early rather than exhausting the budget, so anything else using the same
	// token keeps working.
	ReserveRate int `yaml:"reserve_rate" ghc:"example=500"`
}

type Targets struct {
	User            string   `yaml:"user" ghc:"example=your-github-login"`
	Orgs            []string `yaml:"orgs" ghc:"example=some-org, another-org"`
	Repos           []string `yaml:"repos" ghc:"example=someone/one-repo"`
	Exclude         []string `yaml:"exclude" ghc:"example=someone/experiment-*"`
	IncludeForks    bool     `yaml:"include_forks" ghc:"example=false"`
	IncludeArchived bool     `yaml:"include_archived" ghc:"example=false"`
	// IncludePrivate collects the account's private repositories as well as
	// its public ones. A pointer, because the absent key has to mean on: the
	// token already reaches them, they are most of what an account with any
	// private work has, and a plain bool made the default the opposite of
	// what every document promised. Nil means on. See sinks.dedupe for the
	// same shape.
	IncludePrivate *bool `yaml:"include_private" ghc:"example=true"`
}

// PrivateIncluded resolves IncludePrivate: on unless the config refuses it.
func (t Targets) PrivateIncluded() bool { return Enabled(t.IncludePrivate) }

// Enabled resolves a tri-state setting: absent means on.
//
// Every *bool this package declares has that shape, and each of them was
// written as a pointer for the same reason: a plain bool cannot tell "the key
// says false" from "there is no key", and for all of them the absent key has
// to mean on. The rule lived in as many places as there were readers, one of
// them a closure in cmd/ghchronicle, so the options the site offers would have
// had to restate it a fourth time to say what an unset checkbox means. Here it
// is one function, and the default the builder publishes is Enabled(nil)
// rather than a true typed into a table.
func Enabled(v *bool) bool { return v == nil || *v }

type Sinks struct {
	Influx     *InfluxSink     `yaml:"influxdb"`
	Prometheus *PrometheusSink `yaml:"prometheus"`
	OTLP       *OTLPSink       `yaml:"otlp"`
	Loki       *LokiSink       `yaml:"loki"`
	File       *FileSink       `yaml:"file"`
	Stdout     bool            `yaml:"stdout" ghc:"example=false"`
	// StdoutFormat is "influx" for line protocol or "json" for one object
	// per line, the shape the file sink writes.
	StdoutFormat  string             `yaml:"stdout_format" ghc:"example=influx"`
	Telegraf      *TelegrafSink      `yaml:"telegraf"`
	Graphite      *GraphiteSink      `yaml:"graphite"`
	SQL           *SQLSink           `yaml:"sql"`
	Postgres      *PostgresSink      `yaml:"postgres"`
	Elasticsearch *ElasticsearchSink `yaml:"elasticsearch"`

	// DedupeFile is where the ledger of what has already been written lives.
	// Empty means beside the state file. "off" disables the ledger for every
	// sink, which is what a store that has been wiped wants for one run.
	DedupeFile string `yaml:"dedupe_file" ghc:"example=/var/lib/ghchronicle/state-written.bin"`
	// DedupeHorizon is how long the ledger remembers a point nothing offers
	// any more. Empty means 720h.
	DedupeHorizon string `yaml:"dedupe_horizon" ghc:"example=720h"`
}

// DedupeAge resolves DedupeHorizon. An unparseable value falls back to the
// default rather than refusing to start: it is a housekeeping bound, not a
// correctness one.
func (s *Sinks) DedupeAge() time.Duration {
	if s.DedupeHorizon == "" {
		return 720 * time.Hour
	}
	d, err := time.ParseDuration(s.DedupeHorizon)
	if err != nil || d <= 0 {
		return 720 * time.Hour
	}
	return d
}

type InfluxSink struct {
	URL    string `yaml:"url" ghc:"required,example=http://localhost:8181"`
	Token  string `yaml:"token" ghc:"secret,example=${INFLUX_TOKEN}"`
	Org    string `yaml:"org" ghc:"example=default"`
	Bucket string `yaml:"bucket" ghc:"required,example=github"`
	Batch  int    `yaml:"batch" ghc:"example=5000"`
	// Exclude names measurements this sink should not receive. It defaults to
	// the job log, which is text meant for a log store: writing thousands of
	// lines of build output into a metrics database is a lot of storage for
	// something nobody will query as a number.
	Exclude []string `yaml:"exclude" ghc:"example=gh_job_log"`

	// Dedupe skips writing a point whose fields have not changed since the
	// last time this sink was given it. A store that keys a row by series and
	// timestamp overwrites, so rewriting unchanged history changes nothing it
	// holds and costs a file per partition per write in a store that never
	// compacts. Nil means on. See sinks.dedupe_file.
	Dedupe *bool `yaml:"dedupe" ghc:"example=true"`
}

type PrometheusSink struct {
	Listen string `yaml:"listen" ghc:"example=127.0.0.1:9605"`
	Path   string `yaml:"path" ghc:"example=/metrics"`
	// NoPrime keeps the first sweep after start-up on the normal schedule.
	//
	// By default it runs every family instead, because an exporter keeps its
	// samples in memory: a restart empties it, and without this it stays empty
	// until each cadence comes round, which for the twelve hour families is
	// half a day of a dashboard reading zero.
	NoPrime bool `yaml:"no_prime" ghc:"example=false"`
}

// OTLPSink pushes metrics to an OpenTelemetry collector over HTTP.
type OTLPSink struct {
	Endpoint string            `yaml:"endpoint" ghc:"required,example=http://collector:4318/v1/metrics"`
	Headers  map[string]string `yaml:"headers" ghc:"secret,example=Authorization: Bearer ${OTLP_TOKEN}"`
	Service  string            `yaml:"service" ghc:"example=ghchronicle"`
	// Raw sends the dated points instead of the reduced current values. Only
	// set it when the backend accepts old timestamps: Prometheus's OTLP
	// receiver rejects a sample dated two days back with HTTP 400.
	Raw   bool `yaml:"raw" ghc:"example=false"`
	Batch int  `yaml:"batch" ghc:"example=2000"`
	// Repeat republishes the current state at this interval, so a Prometheus
	// fed by push keeps answering instant queries between sweeps. Its lookback
	// is five minutes; "1m" is a safe value. Empty means no repeat, which is
	// right for a backend that keeps history on its own.
	Repeat string `yaml:"repeat" ghc:"example=1m"`
}

// RepeatEvery parses Repeat. Empty or invalid means zero.
func (o *OTLPSink) RepeatEvery() time.Duration {
	d, err := time.ParseDuration(o.Repeat)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// LokiSink pushes the events, not the numbers.
type LokiSink struct {
	URL      string            `yaml:"url" ghc:"required,example=http://loki:3100/loki/api/v1/push"`
	TenantID string            `yaml:"tenant_id" ghc:"example=tenant-one"`
	Labels   map[string]string `yaml:"labels" ghc:"example=job: ghchronicle"`
	Batch    int               `yaml:"batch" ghc:"example=1000"`
	// MaxAge drops entries older than this. Loki refuses a whole push when one
	// entry predates its reject_old_samples_max_age, a week by default, and
	// much of what this collects is older than that on purpose. The tighter
	// limit is the out-of-order window, about two hours, which is why the
	// sink settles on an hour rather than a day. Match it to your Loki.
	// Empty means the sink's own default, one hour.
	MaxAge string `yaml:"max_age" ghc:"example=1h"`
}

// Age parses MaxAge. Zero says the key was not set, or was set to something
// unusable, and leaves the default to the sink, which is the one place that
// knows what Loki will accept. A value answered here instead was how the
// config layer came to promise seven days while the sink documented an hour.
func (l *LokiSink) Age() time.Duration {
	d, err := time.ParseDuration(l.MaxAge)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// FileSink appends to a rotating file, for the setups that already run a log
// shipper.
type FileSink struct {
	Path     string `yaml:"path" ghc:"required,example=/var/log/ghchronicle/points.lp"`
	Format   string `yaml:"format" ghc:"example=influx"`
	MaxBytes int64  `yaml:"max_bytes" ghc:"example=67108864"`
	Keep     int    `yaml:"keep" ghc:"example=5"`
}

// TelegrafSink posts line protocol to Telegraf's http_listener_v2 input, the
// door to every output Telegraf has.
type TelegrafSink struct {
	// URL is the listener, path included. A bare host gets /telegraf.
	URL      string `yaml:"url" ghc:"required,example=http://telegraf:8186/telegraf"`
	Username string `yaml:"username" ghc:"example=telegraf"`
	Password string `yaml:"password" ghc:"secret,example=${TELEGRAF_PASSWORD}"`
	Batch    int    `yaml:"batch" ghc:"example=5000"`

	// Dedupe skips writing a point whose fields have not changed since the
	// last time this sink was given it. A store that keys a row by series and
	// timestamp overwrites, so rewriting unchanged history changes nothing it
	// holds and costs a file per partition per write in a store that never
	// compacts. Nil means on. See sinks.dedupe_file.
	Dedupe *bool `yaml:"dedupe" ghc:"example=true"`
}

// GraphiteSink writes the plaintext protocol over TCP.
type GraphiteSink struct {
	Addr string `yaml:"addr" ghc:"required,example=graphite:2003"`
	// Prefix starts every metric path. Empty means "github".
	Prefix string `yaml:"prefix" ghc:"example=github"`
	Batch  int    `yaml:"batch" ghc:"example=1000"`

	// Dedupe skips writing a point whose fields have not changed since the
	// last time this sink was given it. A store that keys a row by series and
	// timestamp overwrites, so rewriting unchanged history changes nothing it
	// holds and costs a file per partition per write in a store that never
	// compacts. Nil means on. See sinks.dedupe_file.
	Dedupe *bool `yaml:"dedupe" ghc:"example=true"`
}

// PostgresSink writes to a PostgreSQL that is running, rather than to a file
// for somebody to replay. It is the SQL sink's other half, not its
// replacement: the file is still what a load meant for later, for review, or
// for another SQL engine wants.
type PostgresSink struct {
	// DSN is the connection string, in either shape libpq takes:
	// postgres://user:pass@host:5432/db?sslmode=require, or the keyword form.
	DSN string `yaml:"dsn" ghc:"required,secret,example=${DATABASE_URL}"`
	// Batch is how many upserts go in one round trip.
	Batch int `yaml:"batch" ghc:"example=1000"`
	// Dedupe skips writing a point whose fields have not changed since the
	// last time this sink was given it. See sinks.dedupe_file.
	Dedupe *bool `yaml:"dedupe" ghc:"example=true"`
}

// SQLSink writes INSERT statements to pipe into psql.
type SQLSink struct {
	// Dialect is "postgres", the only one so far.
	Dialect string `yaml:"dialect" ghc:"example=postgres"`
	// Path is a rotating file, or "-" for standard output.
	Path     string `yaml:"path" ghc:"required,example=/var/lib/ghchronicle/points.sql"`
	MaxBytes int64  `yaml:"max_bytes" ghc:"example=67108864"`
	Keep     int    `yaml:"keep" ghc:"example=5"`

	// Dedupe skips writing a point whose fields have not changed since the
	// last time this sink was given it. A store that keys a row by series and
	// timestamp overwrites, so rewriting unchanged history changes nothing it
	// holds and costs a file per partition per write in a store that never
	// compacts. Nil means on. See sinks.dedupe_file.
	Dedupe *bool `yaml:"dedupe" ghc:"example=true"`
}

// ElasticsearchSink writes documents through the _bulk API, to Elasticsearch
// or OpenSearch.
type ElasticsearchSink struct {
	URL string `yaml:"url" ghc:"required,example=http://elasticsearch:9200"`
	// Prefix starts every index name: <prefix>-<measurement>. Empty means
	// "ghchronicle".
	Prefix   string `yaml:"prefix" ghc:"example=ghchronicle"`
	Username string `yaml:"username" ghc:"example=elastic"`
	Password string `yaml:"password" ghc:"secret,example=${ES_PASSWORD}"`
	APIKey   string `yaml:"api_key" ghc:"secret,example=${ES_API_KEY}"`
	Batch    int    `yaml:"batch" ghc:"example=1000"`

	// Dedupe skips writing a point whose fields have not changed since the
	// last time this sink was given it. A store that keys a row by series and
	// timestamp overwrites, so rewriting unchanged history changes nothing it
	// holds and costs a file per partition per write in a store that never
	// compacts. Nil means on. See sinks.dedupe_file.
	Dedupe *bool `yaml:"dedupe" ghc:"example=true"`
}

type Log struct {
	Level  string `yaml:"level" ghc:"example=info"`
	Format string `yaml:"format" ghc:"example=text"`
	// File sends the tool's own log to a rotating file as well as to standard
	// error. Empty means standard error only.
	File     string `yaml:"file" ghc:"example=/var/log/ghchronicle/ghchronicle.log"`
	MaxBytes int64  `yaml:"max_bytes" ghc:"example=67108864"`
	Keep     int    `yaml:"keep" ghc:"example=5"`
}

// logLevels is what log.level accepts, and the threshold each name means.
// The command reads the level through SlogLevel below rather than switching on
// the string itself, so this map is the only place the vocabulary is written
// and the builder on the documentation site offers exactly these four.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// LogLevels are the accepted levels, quietest last, which is the order they
// are documented in and the order a reader chooses from.
func LogLevels() []string {
	out := slices.Collect(maps.Keys(logLevels))
	slices.SortFunc(out, func(a, b string) int { return int(logLevels[a] - logLevels[b]) })
	return out
}

// SlogLevel resolves log.level. An unknown name is info, which is what the
// command has always done: a misspelled level is not worth refusing to start
// over, and the level is not validated at load time for that reason.
func (l Log) SlogLevel() slog.Level {
	if level, known := logLevels[l.Level]; known {
		return level
	}
	return slog.LevelInfo
}

// logFormats is what log.format accepts. The first is what an empty value
// means, and the second is the one UsesJSON answers to.
var logFormats = []string{"text", "json"}

// LogFormats are the accepted log formats, the default first.
func LogFormats() []string { return slices.Clone(logFormats) }

// UsesJSON reports whether the run's own log is written as JSON objects.
func (l Log) UsesJSON() bool { return l.Format == logFormats[1] }

// Every is the cadence table, in three layers. Most specific wins: a family's
// own entry, then its group's, then default, then the built-in value below.
//
// It is a struct of exactly three fields rather than three flat keys for two
// reasons the shape has to keep earning. It resolves a collision: security and
// account are each both a family name and a group name, so in one flat map
// `security: 1m` cannot be read. And because the decoder runs with
// KnownFields, a struct is also the guard: `default` and `groups` are fields
// of this type and can never be mistaken for a family, so no family name can
// shadow a layer and no layer name can shadow a family.
type Every struct {
	// Default is the cadence of every family the two layers below leave
	// alone. It does not reach a family whose built-in cadence is zero: see
	// the note on layers, below.
	Default string `yaml:"default" ghc:"example=15m"`
	// Groups is a cadence per group of families, keyed by the group names
	// Groups() lists. Same reach as Default.
	Groups map[string]string `yaml:"groups" ghc:"example=ci: 1m"`
	// Families is a cadence per family, keyed by the names Families() lists.
	// It is the only layer that can give a duration to a family that ships
	// switched off.
	Families map[string]string `yaml:"families" ghc:"example=deps: 24h"`
}

// layers is Every with every duration parsed and every name checked, so
// resolution below cannot fail and has no strings left in it.
type layers struct {
	def        time.Duration
	hasDefault bool
	groups     map[string]time.Duration
	families   map[string]time.Duration
}

// parse checks the three layers in a fixed order and each layer's keys in
// sorted order, so a file with two typos in it names the same one on every
// run. Map iteration order would otherwise make the message a coin toss, and a
// config error a reader cannot reproduce is a config error he cannot fix.
//
// The two hint messages are the point of the nesting made visible: a group
// name written under families, or a family name written under groups, is the
// mistake the collision used to make unanswerable, and here each one knows
// exactly which key the user was reaching for.
func (e Every) parse() (layers, error) {
	out := layers{
		groups:   make(map[string]time.Duration, len(e.Groups)),
		families: make(map[string]time.Duration, len(e.Families)),
	}
	if e.Default != "" {
		d, err := time.ParseDuration(e.Default)
		if err != nil {
			return layers{}, fmt.Errorf("every.default: %w", err)
		}
		out.def, out.hasDefault = d, true
	}
	for _, name := range slices.Sorted(maps.Keys(e.Families)) {
		if _, isFamily := defaultEvery[name]; !isFamily {
			if slices.Contains(Groups(), name) {
				return layers{}, fmt.Errorf("every.families.%s: %q is a group, not a family; every.groups.%s is where a whole group's cadence lives",
					name, name, name)
			}
			return layers{}, fmt.Errorf("every.families.%s: unknown collector (known: %s)", name, strings.Join(knownCollectors(), ", "))
		}
		d, err := time.ParseDuration(e.Families[name])
		if err != nil {
			return layers{}, fmt.Errorf("every.families.%s: %w", name, err)
		}
		out.families[name] = d
	}
	for _, name := range slices.Sorted(maps.Keys(e.Groups)) {
		if !slices.Contains(Groups(), name) {
			if group, isFamily := GroupOf(name); isFamily {
				return layers{}, fmt.Errorf("every.groups.%s: %q is a family, not a group; it is in group %q, and every.families.%s is where its cadence lives",
					name, name, group, name)
			}
			return layers{}, fmt.Errorf("every.groups.%s: unknown group (known: %s)", name, strings.Join(Groups(), ", "))
		}
		d, err := time.ParseDuration(e.Groups[name])
		if err != nil {
			return layers{}, fmt.Errorf("every.groups.%s: %w", name, err)
		}
		out.groups[name] = d
	}
	return out, nil
}

// resolve returns one family's cadence and the config key that set it.
//
// The two broad layers deliberately cannot reach a family whose built-in
// cadence is zero. Zero is how the table below says "off until somebody asks
// for this by name", and deps alone is 1.8 MB of SBOM per repository: a
// default written to speed up the fast families must not also switch on three
// families the reader never mentioned. It is the same rule groups already
// follows, where naming a group never resurrects a family whose cadence is
// zero, and naming the family is still the way to turn one on.
func (l layers) resolve(name string, f family) (every time.Duration, source string) {
	if d, ok := l.families[name]; ok {
		return d, "every.families." + name
	}
	if f.every <= 0 {
		return f.every, builtinSource
	}
	if d, ok := l.groups[f.group]; ok {
		return d, "every.groups." + f.group
	}
	if l.hasDefault {
		return l.def, "every.default"
	}
	return f.every, builtinSource
}

// Compact prints a duration the way a config file writes one: 15m and 24h
// rather than 15m0s and 24h0m0s. A warning that quotes a value back at the
// reader should quote it in the units he typed, and so should a page that
// offers the built-in cadences as the values a reader would override.
func Compact(d time.Duration) string { return compact(d) }

// compact is Compact's own name inside this package, where it is quoted into
// half the warnings.
func compact(d time.Duration) string {
	switch {
	case d <= 0:
		return d.String()
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	default:
		return d.String()
	}
}

// builtinSource is what everySource holds for a family nothing overrode. It is
// prose rather than a key because there is no key to point the user at.
const builtinSource = "the built-in table"

// family is one collector's cadence, the group it belongs to, and why the
// cadence is that number.
//
// One table, one row per family, because a membership kept in a second list is
// a membership that drifts the first time somebody adds a collector: the
// struct has the field, so the question cannot be skipped. why is a field and
// not a comment for the same reason twice over: the start-up warning quotes it
// when a configuration speeds a family up past what it is worth, and the
// documented table of cadences is pinned to it, so neither can drift from the
// number it explains.
type family struct {
	every time.Duration
	group string
	why   string
}

// Default cadences, and the group each family answers to. The families move at
// very different speeds, and one interval for all of them would either waste
// quota on the slow ones or lose detail on the fast ones. These came out of a
// costed audit; the why column is that audit, and it is what the start-up
// warning reads out when a configuration overrides one of them downwards.
//
// A cadence of zero means the family is off until a config file names it.
// Groups only ever subtract from this, and so do every.default and
// every.groups: none of them ever resurrects a family whose cadence is zero.
var defaultEvery = map[string]family{
	"traffic": {
		every: 6 * time.Hour, group: "audience",
		why: "the fourteen-day window is rewritten whole each time, so a missed sweep repairs itself on the next one",
	},
	"repo": {
		every: 1 * time.Hour, group: "repos",
		why: "stars, forks, languages and topics move slowly, and this is one request per repository",
	},
	"stars": {
		every: 6 * time.Hour, group: "audience",
		why: "the full stargazer walk happens once; after that the newest hundred ride in one GraphQL query per ten repositories",
	},
	"actions": {
		every: 15 * time.Minute, group: "ci",
		why: "a workflow run is over in minutes, and its queue time is only worth watching while it is happening",
	},
	"security": {
		every: 1 * time.Hour, group: "security",
		why: "an alert is something to act on today, and the list of open ones is short",
	},
	"issues": {
		every: 1 * time.Hour, group: "work",
		why: "one request per item, so the cost follows how much is open rather than how often this asks",
	},
	"events": {
		every: 30 * time.Minute, group: "feeds",
		why: "the feed keeps the last three hundred events whatever their dates, so this is the size of a window, not a speed",
	},
	"notifs": {
		every: 30 * time.Minute, group: "feeds",
		why: "read notifications disappear quickly, so this is the size of a window, not a speed",
	},
	"stats": {
		every: 12 * time.Hour, group: "work",
		why: "GitHub recomputes these slowly anyway, so asking more often returns the same numbers",
	},
	"account": {
		every: 12 * time.Hour, group: "account",
		why: "the contribution calendar changes once a day, and the whole family costs one GraphQL point",
	},
	"billing": {
		every: 6 * time.Hour, group: "account",
		why: "GitHub updates the usage report a few times a day at most",
	},
	"profile": {
		every: 12 * time.Hour, group: "account",
		why: "packages, gists and social accounts, all of them edited by hand",
	},
	"artifacts": {
		every: 1 * time.Hour, group: "ci",
		why: "artifacts appear with the run that made them and expire on a scale of days",
	},
	"discussions": {
		every: 2 * time.Hour, group: "work",
		why: "a discussion is answered over hours or days, and few repositories have any",
	},
	"commits": {
		every: 1 * time.Hour, group: "work",
		why: "one request per commit, so the cost follows how much was pushed rather than how often this asks",
	},
	"activity": {
		every: 30 * time.Minute, group: "feeds",
		why: "the repository log holds a hundred entries, which covered twenty-six hours on the busiest repository measured",
	},
	"analyses": {
		every: 6 * time.Hour, group: "security",
		why: "GitHub prunes code scanning analyses, and a repository produces a handful a day",
	},
	"forks": {
		every: 12 * time.Hour, group: "audience",
		why: "the whole list fits in one page, and a fork is a rare event",
	},
	"planning": {
		every: 6 * time.Hour, group: "work",
		why: "labels and milestones are edited by hand, a few times a week at most",
	},
	"outbound": {
		every: 12 * time.Hour, group: "account",
		why: "stars given and work in other people's repositories move at the speed of a person",
	},
	"issueevents": {
		every: 1 * time.Hour, group: "work",
		why: "the timeline of what moved in two cadences, one GraphQL point a repository, so the hour is how soon a transition is worth seeing",
	},
	"achievements": {
		every: 24 * time.Hour, group: "account",
		why: "the badges on the public profile page, read from the page itself because no API lists them, and the distance to each badge's next tier from the API beside; a badge is earned over weeks and the day costs one page and some thirty GraphQL points",
	},
	"keys": {
		every: 24 * time.Hour, group: "account",
		why: "an SSH or GPG key changes when somebody changes it, and what matters is its expiry date, not the hour it was noticed",
	},
	"deps": {
		every: 0, group: "repos",
		why: "off until asked for by name: the SBOM is 1.8 MB per repository and has its own budget of a hundred a minute",
	},
	"totals": {
		every: 12 * time.Hour, group: "account",
		why: "twice a day is plenty for a number that only grows",
	},
	"ratelimit": {
		every: 15 * time.Minute, group: "collector",
		why: "free, and worth having at the resolution of the busiest family",
	},
	"settings": {
		every: 6 * time.Hour, group: "repos",
		why: "webhooks, rulesets, environments and deploy keys change only when somebody changes them",
	},
	"history": {
		every: 0, group: "account",
		why: "off until asked for by name: it walks every past year and the year so far, and the rows are idempotent",
	},
	"joblogs": {
		every: 0, group: "ci",
		why: "off until asked for by name: it is text rather than a measurement, it costs a request per failure, and it only makes sense with a log store attached",
	},
	"branches": {
		every: 24 * time.Hour, group: "repos",
		why: "branches are created and deleted all day, but the question the row answers is which are stale right now, which is a daily one",
	},
	"inventory": {
		every: 24 * time.Hour, group: "repos",
		why: "four core requests per repository, for settings that change only when somebody changes them",
	},
	"deployments": {
		every: 1 * time.Hour, group: "ci",
		why: "the surface a delivery dashboard reads, and the newest page is cheap: one GraphQL point per five repositories",
	},
	"policyfiles": {
		every: 24 * time.Hour, group: "repos",
		why: "SECURITY.md, CODEOWNERS, dependabot.yml and FUNDING.yml move about once a quarter",
	},
	"rulesets": {
		every: 24 * time.Hour, group: "repos",
		why: "a ruleset is edited a few times a year, every version keeps its own date, and both requests answer 304 until somebody edits one",
	},
}

// groupDescriptions is what `-groups` prints beside each name. Prose only: the
// membership lives in defaultEvery and nowhere else, so this table cannot come
// to disagree with it about who is in what.
//
// Two of these names, account and security, are also family names. That is
// deliberate: groups and every are different keys with different vocabularies,
// and in both cases the group is a superset of the family it is named after,
// so the name can only ever widen.
var groupDescriptions = map[string]string{
	"audience":  "who is looking at the projects, who starred them and who copied them",
	"account":   "the account itself: its lifetime numbers, its profile, its keys, its spending and what it does in other people's repositories",
	"repos":     "what each repository is: metadata and releases, live branches, webhooks and environments, governance files, dependencies",
	"work":      "the work done in them: pull requests, issues and their transitions, commits, discussions, labels and milestones",
	"ci":        "continuous integration and deployment: runs, jobs, steps, artifacts, caches and deployments",
	"security":  "Dependabot alerts and code scanning",
	"feeds":     "the three feeds GitHub keeps only briefly: the event feed, notifications and each repository's log",
	"collector": "what the collector has left to spend",
}

var envRef = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// expandEnv replaces ${VAR} with the environment, so a config file can be
// committed without the token in it.
func expandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})
}

// Validate fills defaults and reports what is unusable.
func (c *Config) Validate() error {
	if err := c.resolveGitHub(); err != nil {
		return err
	}
	if err := c.resolveTargets(); err != nil {
		return err
	}
	c.resolveFilePaths()
	if err := c.resolveSinks(); err != nil {
		return err
	}
	if err := c.resolveIntervals(); err != nil {
		return err
	}
	if err := c.resolveGroups(); err != nil {
		return err
	}
	if err := c.resolveHeartbeat(); err != nil {
		return err
	}
	c.resolveGrafana()
	// Last, because it reads the finished schedule: a family the groups key
	// removed has no cadence left to complain about.
	c.noteCadences()
	return nil
}

// resolveGrafana expands the credential and the two addresses, and falls back
// to GRAFANA_TOKEN the way the GitHub token falls back to GITHUB_TOKEN.
//
// Every field a reader is likely to write a ${VAR} into has to be named here:
// expansion is per field rather than over the whole file, so a field left out
// carries the reference through to the server as text. That is what happened
// to grafana.token the first time this section was published, and the server
// answered "Invalid API key" to a request carrying the four characters ${GR.
func (c *Config) resolveGrafana() {
	if c.Grafana == nil {
		return
	}
	c.Grafana.URL = expandEnv(c.Grafana.URL)
	c.Grafana.Token = expandEnv(c.Grafana.Token)
	c.Grafana.DashboardUID = expandEnv(c.Grafana.DashboardUID)
	c.Grafana.Datasource.URL = expandEnv(c.Grafana.Datasource.URL)
	c.Grafana.Datasource.UID = expandEnv(c.Grafana.Datasource.UID)
	c.Grafana.Datasource.LokiUID = expandEnv(c.Grafana.Datasource.LokiUID)
	if c.Grafana.Token == "" {
		c.Grafana.Token = os.Getenv("GRAFANA_TOKEN")
	}
}

// resolveGitHub expands the credentials and fills the budget reserve.
func (c *Config) resolveGitHub() error {
	c.GitHub.Token = expandEnv(c.GitHub.Token)
	c.GitHub.BaseURL = expandEnv(c.GitHub.BaseURL)
	c.GitHub.WebURL = expandEnv(c.GitHub.WebURL)
	if c.GitHub.Token == "" {
		c.GitHub.Token = os.Getenv("GITHUB_TOKEN")
	}
	if c.GitHub.Token == "" && !c.AllowNoToken {
		return errors.New("github.token is empty and GITHUB_TOKEN is unset")
	}
	if c.GitHub.ReserveRate <= 0 {
		// Leaving a tenth of the budget untouched costs little and keeps a
		// `gh` command working while a sweep runs.
		c.GitHub.ReserveRate = 500
	}
	return nil
}

func (c *Config) resolveTargets() error {
	if c.Targets.User == "" && len(c.Targets.Orgs) == 0 && len(c.Targets.Repos) == 0 {
		return errors.New("targets: set at least one of user, orgs or repos")
	}
	return nil
}

// resolveFilePaths names the two files a sweep remembers itself in.
func (c *Config) resolveFilePaths() {
	if c.StateFile == "" {
		c.StateFile = "ghchronicle-state.json"
	}
	if c.Sinks.DedupeFile == "" {
		// Beside the state file, since it is the same kind of thing: what a
		// sweep has to remember so the next one does less.
		c.Sinks.DedupeFile = strings.TrimSuffix(c.StateFile, ".json") + "-written.bin"
	}
}

// BackfillProgressFile is where a backfill keeps its checkpoint: beside the
// state file, the way the dedupe ledger is.
//
// Derived and not a setting of its own, because there is no configuration to
// make. The file belongs to one walk, it is written only while that walk is
// running and removed when it ends, and the one thing it has to agree with is
// which walk: two ghchronicle instances that already keep separate state files,
// as the author's service and his backfill do, keep separate checkpoints for
// free, and two that share one would have shared the state file's marks long
// before they could confuse each other here.
//
// Empty when there is no state file, which is what a run keeping its state in
// memory is, and such a run keeps no checkpoint either.
func (c *Config) BackfillProgressFile() string {
	if c.StateFile == "" {
		return ""
	}
	return strings.TrimSuffix(c.StateFile, ".json") + "-progress.json"
}

// resolveSinks expands the environment in every configured sink, fills its
// defaults and reports the first that cannot work.
//
// Each sink resolves itself, and a sink that is not configured resolves to
// nothing, so this stays a list of the sinks there are rather than a ladder of
// nil checks. What a URL, a dialect or a format has to look like is the sink's
// own business and lives next to its type.
func (c *Config) resolveSinks() error {
	s := &c.Sinks
	for _, resolve := range []func() error{
		s.Influx.resolve, s.Prometheus.resolve, s.OTLP.resolve, s.Loki.resolve,
		s.File.resolve, s.resolveStdout, s.Telegraf.resolve, s.Graphite.resolve,
		s.SQL.resolve, s.Postgres.resolve, s.Elasticsearch.resolve,
	} {
		if err := resolve(); err != nil {
			return err
		}
	}
	return c.requireOneSink()
}

func (i *InfluxSink) resolve() error {
	if i == nil {
		return nil
	}
	i.Token, i.URL = expandEnv(i.Token), expandEnv(i.URL)
	if i.URL == "" || i.Bucket == "" {
		return errors.New("sinks.influxdb: url and bucket are required")
	}
	if i.Org == "" {
		i.Org = "default"
	}
	if i.Exclude == nil {
		i.Exclude = []string{"gh_job_log"}
	}
	return nil
}

func (p *PrometheusSink) resolve() error {
	if p == nil {
		return nil
	}
	if p.Listen == "" {
		p.Listen = ":9605"
	}
	if p.Path == "" {
		p.Path = "/metrics"
	}
	return nil
}

func (o *OTLPSink) resolve() error {
	if o == nil {
		return nil
	}
	o.Endpoint = expandEnv(o.Endpoint)
	if o.Endpoint == "" {
		return errors.New("sinks.otlp: endpoint is required, for example http://collector:4318/v1/metrics")
	}
	for k, v := range o.Headers {
		o.Headers[k] = expandEnv(v)
	}
	return nil
}

func (l *LokiSink) resolve() error {
	if l == nil {
		return nil
	}
	l.URL = expandEnv(l.URL)
	if l.URL == "" {
		return errors.New("sinks.loki: url is required, for example http://loki:3100/loki/api/v1/push")
	}
	return nil
}

// PointFormats are the two renderings of a point the file sink and the
// standard-output sink accept, in the order they are documented. Empty means
// the first of them.
//
// A list rather than a case arm in each of the two resolvers below, because
// the builder on the documentation site offers these as a choice and a choice
// typed there is a choice that can come to offer a third format, or to stop
// offering one of these, while both resolvers still refuse it.
func PointFormats() []string { return slices.Clone(pointFormats) }

var pointFormats = []string{"influx", "json"}

// SQLDialects are the dialects sinks.sql.dialect accepts. One so far, and the
// list is what makes that a fact the site reads rather than one it states.
func SQLDialects() []string { return slices.Clone(sqlDialects) }

var sqlDialects = []string{"postgres"}

func (f *FileSink) resolve() error {
	if f == nil {
		return nil
	}
	if f.Path == "" {
		return errors.New("sinks.file: path is required")
	}
	if f.Format != "" && !slices.Contains(pointFormats, f.Format) {
		return fmt.Errorf("sinks.file.format: %q is not %s", f.Format, strings.Join(pointFormats, " or "))
	}
	return nil
}

// resolveStdout checks the one sink that is a bool rather than a struct.
func (s *Sinks) resolveStdout() error {
	if s.StdoutFormat != "" && !slices.Contains(pointFormats, s.StdoutFormat) {
		return fmt.Errorf("sinks.stdout_format: %q is not %s", s.StdoutFormat, strings.Join(pointFormats, " or "))
	}
	return nil
}

func (t *TelegrafSink) resolve() error {
	if t == nil {
		return nil
	}
	t.URL, t.Username, t.Password = expandEnv(t.URL), expandEnv(t.Username), expandEnv(t.Password)
	if t.URL == "" {
		return errors.New("sinks.telegraf: url is required, for example http://telegraf:8186/telegraf")
	}
	return nil
}

func (g *GraphiteSink) resolve() error {
	if g == nil {
		return nil
	}
	g.Addr = expandEnv(g.Addr)
	if g.Addr == "" {
		return errors.New("sinks.graphite: addr is required, for example graphite:2003")
	}
	if g.Prefix == "" {
		g.Prefix = "github"
	}
	return nil
}

// resolve checks the one thing a connection needs and fills the batch.
func (s *PostgresSink) resolve() error {
	if s == nil {
		return nil
	}
	s.DSN = expandEnv(s.DSN)
	if strings.TrimSpace(s.DSN) == "" {
		return errors.New("sinks.postgres: dsn is required, " +
			"either postgres://user:pass@host:5432/db or the keyword form")
	}
	if s.Batch <= 0 {
		s.Batch = 1000
	}
	return nil
}

func (s *SQLSink) resolve() error {
	if s == nil {
		return nil
	}
	if s.Path == "" {
		return errors.New("sinks.sql: path is required, a file or - for standard output")
	}
	if s.Dialect == "" {
		s.Dialect = sqlDialects[0]
	}
	if !slices.Contains(sqlDialects, s.Dialect) {
		return fmt.Errorf("sinks.sql.dialect: %q is not %s, the only dialect so far",
			s.Dialect, strings.Join(sqlDialects, " or "))
	}
	return nil
}

func (e *ElasticsearchSink) resolve() error {
	if e == nil {
		return nil
	}
	e.URL, e.APIKey = expandEnv(e.URL), expandEnv(e.APIKey)
	e.Username, e.Password = expandEnv(e.Username), expandEnv(e.Password)
	if e.URL == "" {
		return errors.New("sinks.elasticsearch: url is required, for example http://elasticsearch:9200")
	}
	if e.APIKey != "" && e.Username != "" {
		return errors.New("sinks.elasticsearch: set either api_key or username and password, not both")
	}
	if e.Prefix == "" {
		e.Prefix = "ghchronicle"
	}
	return nil
}

// requireOneSink refuses a config that would collect and then throw the
// result away. AllowNoSinks waives it for a caller that supplies its own
// destination, which is what -card <path> -card-only does.
func (c *Config) requireOneSink() error {
	if c.AllowNoSinks {
		return nil
	}
	s := &c.Sinks
	if s.Influx == nil && s.Prometheus == nil && s.OTLP == nil && s.Loki == nil &&
		s.File == nil && !s.Stdout && s.Telegraf == nil && s.Graphite == nil &&
		s.SQL == nil && s.Postgres == nil && s.Elasticsearch == nil {
		return errors.New("sinks: enable at least one of influxdb, prometheus, otlp, loki, file, stdout, telegraf, graphite, sql or elasticsearch, " +
			"or run with -card <path> -card-only to draw a card and write the points nowhere")
	}
	return nil
}

// resolveIntervals resolves every family through the three layers of every,
// refusing a name that is neither a collector nor a group: a typo there would
// otherwise be a family silently left on its default forever.
//
// A resolved cadence of zero is not copied in. Zero is how a family is
// switched off, and State.Due reads a zero interval as always due, so a family
// seeded with one would run on every single sweep instead of never.
func (c *Config) resolveIntervals() error {
	l, err := c.Every.parse()
	if err != nil {
		return err
	}
	c.intervals = make(map[string]time.Duration, len(defaultEvery))
	c.everySource = make(map[string]string, len(defaultEvery))
	for name, f := range defaultEvery {
		d, source := l.resolve(name, f)
		if d <= 0 {
			continue
		}
		c.intervals[name] = d
		c.everySource[name] = source
	}
	return nil
}

// resolveHeartbeat parses the loop's forced tick. Unlike a cadence it is
// refused rather than read as "off": nobody writes heartbeat: 0 meaning "never
// wake up", so a zero here is a mistake and not an instruction.
func (c *Config) resolveHeartbeat() error {
	c.heartbeat = 0
	if c.Heartbeat == "" {
		return nil
	}
	d, err := time.ParseDuration(c.Heartbeat)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if d <= 0 {
		return errors.New("heartbeat: must be positive; omit the key to derive the loop's tick from the shortest cadence")
	}
	c.heartbeat = d
	return nil
}

// HeartbeatEvery is the forced loop tick and whether the config set one. False
// means the runner derives the tick from the shortest cadence.
func (c *Config) HeartbeatEvery() (time.Duration, bool) { return c.heartbeat, c.heartbeat > 0 }

// tooFastFactor is where "substantially shorter than the built-in cadence"
// starts: at least four times more often.
//
// The built-in values are a ladder, 15m 30m 1h 2h 6h 12h 24h, and the widest
// gap between two neighboring rungs is three (2h to 6h). Four is therefore
// the smallest factor no single step down the ladder can reach, which is the
// number that separates a deliberate one-rung adjustment, made by somebody
// looking at that family, from the thing this warning exists for: a default or
// a group value landing on a family it was never chosen for. The owner's own
// example is far past it, a 15m default against 24h for keys being ninety-six
// times more often, and the group work at one number flattens 1h and 12h,
// which is twelve.
const tooFastFactor = 4

// noteCadences reports every family a configuration collects substantially
// more often than the costed default is worth, and a heartbeat that would hold
// the fastest of them back.
//
// Per family and never per group, because the reason a cadence is what it is
// belongs to the family: "the group work is too fast" tells a reader nothing
// he can act on, while "stats every minute against a built-in 12h, because
// GitHub recomputes these slowly anyway" names the number, the cost and the
// knob that set it.
func (c *Config) noteCadences() {
	for _, name := range slices.Sorted(maps.Keys(c.intervals)) {
		f, got := defaultEvery[name], c.intervals[name]
		// Slower or equal is checked before the multiplication rather than by
		// it, so an absurd duration cannot overflow int64 and report a cadence
		// of three centuries as one that is too fast.
		if f.every <= 0 || got >= f.every || got*tooFastFactor > f.every {
			continue
		}
		c.notes = append(c.notes, fmt.Sprintf(
			"%s sets %s to %s against a built-in %s, %d times more often: %s",
			c.everySource[name], name, compact(got), compact(f.every), f.every/got, f.why,
		))
	}
	if len(c.intervals) == 0 {
		c.notes = append(c.notes,
			"every family resolves to a cadence of zero, so this run collects nothing; "+
				"name the families to keep under every.families")
	}
	c.noteHeartbeat()
}

// noteHeartbeat reports a forced tick slower than the fastest cadence, which
// is the quiet half of the same mistake: nothing is misconfigured, and yet a
// family cannot run at the interval its own entry asks for.
func (c *Config) noteHeartbeat() {
	if c.heartbeat <= 0 || len(c.intervals) == 0 {
		return
	}
	fastest, shortest := "", time.Duration(0)
	for _, name := range slices.Sorted(maps.Keys(c.intervals)) {
		if d := c.intervals[name]; shortest == 0 || d < shortest {
			fastest, shortest = name, d
		}
	}
	if c.heartbeat <= shortest {
		return
	}
	c.notes = append(c.notes, fmt.Sprintf(
		"heartbeat is %s and the shortest cadence is %s (%s), so no family can run more often than every %s",
		compact(c.heartbeat), compact(shortest), fastest, compact(c.heartbeat),
	))
}

// resolveGroups narrows the sweep to the groups the config named, and is the
// whole of the feature at runtime: every dispatch path in internal/run reaches
// its collectors through Interval, so a family deleted from this map is never
// requested and never written.
//
// It runs after resolveIntervals, so a cadence of zero has already removed its
// family by the time the warnings below are recorded and they can say what is
// true rather than what was asked for.
func (c *Config) resolveGroups() error {
	// Cleared rather than appended to, so validating the same config twice
	// says each thing once, the way resolveIntervals rebuilds its map.
	c.notes, c.selected = nil, nil
	if c.Groups == nil {
		return nil // no key at all means every group, which is the default
	}
	selected, err := c.selectGroups()
	if err != nil {
		return err
	}
	for name := range c.intervals {
		if !selected[defaultEvery[name].group] {
			delete(c.intervals, name)
		}
	}
	c.selected = selected
	c.noteGroupChoices(selected)
	return nil
}

// selectGroups turns the list the config carries into a set, refusing the three
// ways of naming something that is not a group.
//
// A family name is worth its own message: both are lowercase nouns from the
// same document, one table holds both, and pointing at every is the answer the
// user was reaching for.
func (c *Config) selectGroups() (map[string]bool, error) {
	if len(*c.Groups) == 0 {
		return nil, errors.New("groups: is empty, which would collect nothing; omit the key to collect everything")
	}
	known := map[string]bool{}
	for _, name := range Groups() {
		known[name] = true
	}
	selected := make(map[string]bool, len(*c.Groups))
	for i, raw := range *c.Groups {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch group, isFamily := GroupOf(name); {
		case known[name]:
			selected[name] = true
		case isFamily:
			return nil, fmt.Errorf("groups[%d]: %q is a family, not a group; it is in group %q, and every.families.%s is where its cadence lives",
				i, raw, group, name)
		default:
			return nil, fmt.Errorf("groups[%d]: %q is not a group (known: %s)", i, raw, strings.Join(Groups(), ", "))
		}
	}
	return selected, nil
}

// noteGroupChoices records what is legal but probably not what was meant.
//
// None of these refuses the config. A user narrowing his groups should not
// also have to prune an every block he tuned last year, and a group that
// collects nothing is a thing to be told once rather than a reason not to
// start.
func (c *Config) noteGroupChoices(selected map[string]bool) {
	for _, name := range slices.Sorted(maps.Keys(c.Every.Families)) {
		if group := defaultEvery[name].group; !selected[group] {
			c.notes = append(c.notes, fmt.Sprintf(
				"every.families.%s names a family in group %s, which groups does not select, so it will not run", name, group,
			))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Every.Groups)) {
		if !selected[name] {
			c.notes = append(c.notes, fmt.Sprintf(
				"every.groups.%s sets a cadence for a group that groups does not select, so it sets nothing", name,
			))
		}
	}
	for _, group := range Groups() {
		if !selected[group] || c.collectsAnythingIn(group) {
			continue
		}
		c.notes = append(c.notes, fmt.Sprintf(
			"groups names %s, but every family in it has a cadence of zero, so it collects nothing", group,
		))
	}
	if !selected["collector"] {
		c.notes = append(c.notes, "groups does not name collector, so gh_rate_limit is not collected "+
			"and a family skipped for lack of budget will look the same as one with nothing to report")
	}
}

// collectsAnythingIn reports whether any family of a group survived both the
// group selection and its own cadence.
func (c *Config) collectsAnythingIn(group string) bool {
	for _, name := range FamiliesIn(group) {
		if _, enabled := c.intervals[name]; enabled {
			return true
		}
	}
	return false
}

// Warnings are the things worth saying about a configuration that is legal.
// Validate has no logger and main has one only after the config is loaded, so
// they are recorded during validation and emitted once there is somewhere to
// put them.
func (c *Config) Warnings() []string { return c.notes }

// Selection reports which groups this configuration collects for and which it
// leaves out, both sorted, and whether it narrows anything at all.
func (c *Config) Selection() (on, off []string, narrowed bool) {
	if c.selected == nil {
		return Groups(), nil, false
	}
	for _, group := range Groups() {
		if c.selected[group] {
			on = append(on, group)
			continue
		}
		off = append(off, group)
	}
	return on, off, true
}

// Interval returns the cadence of a collector, and whether it is enabled.
func (c *Config) Interval(name string) (time.Duration, bool) {
	d, ok := c.intervals[name]
	return d, ok
}

// knownCollectors is sorted, so the list quoted in an error message is the
// same on every run rather than whatever order the map happened to have.
func knownCollectors() []string { return slices.Sorted(maps.Keys(defaultEvery)) }

// Timeout parses github.timeout, defaulting to 30s.
func (g GitHub) HTTPTimeout() time.Duration {
	if g.Timeout == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(g.Timeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// Families lists every collector family by name, sorted, so a caller can
// iterate the schedule without duplicating the list.
func Families() []string { return knownCollectors() }

// Groups lists every group name, sorted. It is derived from the family table
// rather than from the descriptions, so membership has exactly one source.
func Groups() []string {
	seen := make(map[string]struct{}, len(defaultEvery))
	for _, f := range defaultEvery {
		seen[f.group] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
}

// GroupOf reports which group a family belongs to, and whether the name is a
// family at all.
func GroupOf(fam string) (string, bool) {
	f, known := defaultEvery[fam]
	return f.group, known
}

// FamiliesIn lists the families of one group, sorted.
func FamiliesIn(group string) []string {
	var out []string
	for name, f := range defaultEvery {
		if f.group == group {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// GroupDescription is the one line `-groups` prints beside a group name.
func GroupDescription(group string) string { return groupDescriptions[group] }

// Why is the reason a family's built-in cadence is the number it is. It is
// what the start-up warning quotes, and it is the source the documented table
// of cadences is pinned to.
func Why(fam string) string { return defaultEvery[fam].why }

// BuiltinEvery is a family's cadence before any config file touches it, and
// whether the name is a family at all. Zero means the family ships switched
// off.
func BuiltinEvery(fam string) (time.Duration, bool) {
	f, known := defaultEvery[fam]
	return f.every, known
}

// Grafana is the server the dashboard is published to, and how it should
// reach the store this writes into.
type Grafana struct {
	URL   string `yaml:"url" ghc:"example=http://localhost:3000"`
	Token string `yaml:"token" ghc:"secret,example=${GRAFANA_TOKEN}"`
	// Folder is the folder the dashboard goes in, by title, created when it is
	// not there. Empty means Grafana's default folder.
	Folder string `yaml:"folder" ghc:"example=GitHub"`
	// PublishOnStart reconciles the datasource and the dashboard once when the
	// collector starts, before the first sweep. Off by default: a collector
	// that writes to Grafana without being asked would surprise, and the
	// dashboard is generated from the code, so this is what keeps a server
	// from quietly falling behind the binary that feeds it.
	PublishOnStart bool `yaml:"publish_on_start" ghc:"example=false"`
	// DashboardUID writes over the dashboard at this uid instead of the one
	// named after the store. The generated dashboards carry their own uid and
	// every publish overwrites it, so this is only for a server where the
	// dashboard already lives somewhere else: one imported through the UI with
	// Grafana's "import as new" asked for, or one whose uid was changed by
	// hand. Without it a second dashboard would appear beside the first and
	// the one being looked at would stop being the one being updated.
	DashboardUID string `yaml:"dashboard_uid" ghc:"example=my-existing-dashboard"`
	// Datasource overrides what is otherwise read from the sink.
	Datasource GrafanaDatasource `yaml:"datasource"`
}

// GrafanaDatasource is the two things about a datasource that the sink cannot
// answer for itself.
type GrafanaDatasource struct {
	// URL is the address Grafana reaches the store by, when that is not the
	// address the collector writes to. They differ more often than not: a
	// collector on the host writes to a published port and Grafana in a
	// container reaches the same store by its name on the container network,
	// and copying the sink's address across produces a datasource Grafana
	// accepts and cannot use.
	URL string `yaml:"url" ghc:"example=http://influxdb:8181"`
	// UID adopts a datasource that already exists instead of managing one
	// named after the store.
	UID string `yaml:"uid" ghc:"example=ae3x9k2"`
	// SSLMode is what the PostgreSQL datasource connects with, when the mode
	// in the dsn is one Grafana cannot express. libpq's default is "prefer",
	// try TLS and carry on without it, and Grafana's datasource either
	// insists or refuses, so that case is chosen here rather than guessed.
	SSLMode string `yaml:"sslmode" ghc:"example=require"`
	// LokiUID is a Loki datasource that already exists. With one, the panel
	// that would say where a failed job's output went reads the lines from it
	// instead. It is adopted rather than made: the Loki sink writes to the
	// push endpoint, and a datasource built from that address would be
	// pointed at the half of the API that does not answer queries.
	LokiUID string `yaml:"loki_uid" ghc:"example=be7m1q4"`
}
