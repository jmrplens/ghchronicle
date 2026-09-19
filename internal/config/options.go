package config

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// The two settings whose vocabulary, default and membership of the key list
// are all written separately: naming them once keeps a rename from leaving one
// of the three behind.
const (
	keyLogLevel  = "log.level"
	keyLogFormat = "log.format"
)

// probeURL is the address the probe configuration gives every sink that
// validates one. It reaches nothing: the probe is built to be validated, never
// to be run, and readDefaults reports no value the probe itself set.
const probeURL = "http://probe"

// The inventory of settings, as something other than prose.
//
// A configuration file is the one surface of this tool that everybody touches
// and nothing could enumerate. config.example.yaml is the reference a reader
// copies, and it is a hand-written file: a setting added to the types and not
// to it is a setting nobody finds, which happened five times before
// documented_test.go started failing on it. That test closed the gap for the
// example file by walking the yaml tags, and it did it inside a _test.go,
// where nothing else can reach.
//
// This is that walk, exported. It is what lets the configuration builder on
// the documentation site offer exactly the settings the binary has: the page
// reads a generated file (site/src/data/config-options.json, written by
// cmd/gen_config) rather than a form somebody typed, so it cannot offer an
// option the binary does not have, and cannot miss one it does.
//
// Three facts a form needs are not in the Go types: whether a value is a
// credential, a value to show as an example, and whether the setting is
// required once its block is used. They are struct tags, `ghc`, and not a
// table beside the types, for one reason: a table entry can outlive the field
// it describes, and only a test stops it, whereas a tag goes when its field
// goes. The rule below is the other half of that: a leaf with no tag is an
// error, so a setting added without one fails the generator and CI rather than
// reaching the page nameless.
//
// The fourth fact, what the setting is FOR, is prose, and prose already exists
// under Configuration on the site, one page per block. It is not repeated
// here. A help line copied into a tag would be a second English description of
// every setting, untranslated, drifting from the page that explains it; the
// builder links each group to that page instead.
//
// Defaults are not declared at all. They are read back out of the resolver:
// a probe configuration is validated, and every value the resolver filled in
// is the default of that key. A default typed here would be a claim about
// behavior that the behavior could contradict.

// Option is one setting a configuration file may carry.
//
// Key is the dotted YAML path, which is what a reader types and what the
// builder's control is named after. A block is exported alongside its leaves
// so the page can nest the form the way the file nests.
type Option struct {
	// Key is the dotted path, "sinks.influxdb.url".
	Key string `json:"key"`
	// Kind is block, string, bool, int, list or map.
	Kind string `json:"kind"`
	// Group is the documentation section the key belongs to: the first
	// segment of the path, with the two that read better renamed.
	Group string `json:"group"`
	// Sink is the sink a key under sinks. belongs to, empty otherwise.
	Sink string `json:"sink,omitempty"`
	// Secret marks a credential. The builder never writes one into the file
	// it generates; it writes the ${VAR} reference the documentation asks for.
	Secret bool `json:"secret,omitempty"`
	// Required marks a key its block cannot resolve without.
	Required bool `json:"required,omitempty"`
	// Default is the value the resolver fills in when the file is silent,
	// read back from a validated probe rather than declared.
	Default string `json:"default,omitempty"`
	// Example is a value of the shape this key takes, from the struct tag.
	Example string `json:"example,omitempty"`
	// Choices are the values the key accepts, for the settings that have a
	// fixed vocabulary.
	Choices []string `json:"choices,omitempty"`
	// Keys are the names a map's or a list's entries are drawn from, for the
	// settings whose vocabulary is this package's own.
	Keys []string `json:"keys,omitempty"`
}

// Kinds of option, as the page reads them.
const (
	KindBlock  = "block"
	KindString = "string"
	KindBool   = "bool"
	KindInt    = "int"
	KindList   = "list"
	KindMap    = "map"
)

// groupOfKey renames the two top-level keys whose section on the site is not
// called after them, and leaves the rest as the path's first segment. The
// settings that belong to no block of their own (state_file, heartbeat,
// backfill) are one group, because that is how a reader meets them: things
// about the run rather than about a destination.
func groupOfKey(key string) string {
	head, _, _ := strings.Cut(key, ".")
	switch head {
	case "github", "targets", "sinks", "log":
		return head
	case "every":
		return "cadences"
	default:
		return "run"
	}
}

// choicesOf is the fixed vocabulary of the settings that have one. Each entry
// answers with the package's own list rather than with values typed here, so
// the page offers what the resolver accepts. A key that no longer exists is an
// error from Options, not an entry quietly ignored.
func choicesOf(key string) []string {
	switch key {
	case "sinks.file.format", "sinks.stdout_format":
		return PointFormats()
	case "sinks.sql.dialect":
		return SQLDialects()
	case keyLogLevel:
		return LogLevels()
	case keyLogFormat:
		return LogFormats()
	}
	return nil
}

// keysOf is the vocabulary a map's keys, or a list's entries, are drawn from,
// for the three settings whose vocabulary this package owns.
func keysOf(key string) []string {
	switch key {
	case "groups", "every.groups":
		return Groups()
	case "every.families":
		return Families()
	}
	return nil
}

// Options is every setting a configuration file may carry, in the order the
// types declare them, which is the order config.example.yaml presents them in.
//
// It reports an error rather than returning a partial inventory, because every
// caller of it publishes what it returns: the generator writes the file the
// site's builder reads, and a builder missing a setting is a builder that
// silently cannot configure it.
func Options() ([]Option, error) {
	out, err := walkOptions(reflect.TypeFor[Config](), "")
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("config declares no settings at all, so there is nothing to export " +
			"and a check of the export would pass over nothing. Look at internal/config/config.go")
	}
	byKey := make(map[string]int, len(out))
	for i, o := range out {
		byKey[o.Key] = i
	}
	// The two tables above are keyed by path, which is the one thing about
	// them that can go stale, so each entry has to find its setting.
	for _, key := range []string{
		"sinks.file.format", "sinks.stdout_format", "sinks.sql.dialect", keyLogLevel, keyLogFormat,
		"groups", "every.groups", "every.families",
	} {
		i, found := byKey[key]
		if !found {
			return nil, fmt.Errorf("%s has a vocabulary in internal/config/options.go and is not a setting any more; "+
				"drop it from choicesOf or keysOf", key)
		}
		out[i].Choices, out[i].Keys = choicesOf(key), keysOf(key)
	}
	defaults, err := resolvedDefaults()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if value, filled := defaults[out[i].Key]; filled {
			out[i].Default = value
		}
	}
	return out, nil
}

// walkOptions reads one struct's yaml tags, recursing into the blocks.
//
// It stops at a map and at a slice for the same reason the example file's
// coverage test does: what is under every.families or sinks.otlp.headers is
// the user's vocabulary, not this package's, and the three cases where it is
// this package's are answered by keysOf above.
func walkOptions(t reflect.Type, prefix string) ([]Option, error) {
	var out []Option
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue // unexported, or set by a flag rather than by the file
		}
		key := prefix + name
		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, Option{Key: key, Kind: KindBlock, Group: groupOfKey(key), Sink: sinkOfKey(key)})
			inner, err := walkOptions(ft, key+".")
			if err != nil {
				return nil, err
			}
			out = append(out, inner...)
			continue
		}
		option, err := leafOption(key, field)
		if err != nil {
			return nil, err
		}
		out = append(out, option)
	}
	return out, nil
}

// sinkOfKey is the sink a key under sinks. belongs to, and empty for anything
// else, sinks.dedupe_file and the settings of every other block included.
func sinkOfKey(key string) string {
	rest, under := strings.CutPrefix(key, "sinks.")
	if !under {
		return ""
	}
	sink, _, nested := strings.Cut(rest, ".")
	if !nested {
		return "" // sinks.stdout and the two ledger settings belong to no sink
	}
	return sink
}

// leafOption reads one setting's kind off its Go type and the three facts the
// type cannot carry off its ghc tag.
//
// The tag is `secret`, `required` and `example=…`, comma separated, with the
// example last because an example may itself contain a comma (a list's, for
// one) and splitting it would truncate the value shown to a reader.
func leafOption(key string, field reflect.StructField) (Option, error) {
	tag, tagged := field.Tag.Lookup("ghc")
	if !tagged {
		return Option{}, fmt.Errorf("%s carries no ghc tag, so the configuration builder would offer it "+
			"with no example and no idea whether it is a credential. Add ghc:\"[secret,][required,]example=…\" "+
			"beside its yaml tag in internal/config/config.go", key)
	}
	option := Option{Key: key, Group: groupOfKey(key), Sink: sinkOfKey(key)}
	kind, err := kindOf(key, field.Type)
	if err != nil {
		return Option{}, err
	}
	option.Kind = kind
	for rest := tag; rest != ""; {
		var part string
		if example, is := strings.CutPrefix(rest, "example="); is {
			option.Example, rest = example, ""
			continue
		}
		part, rest, _ = strings.Cut(rest, ",")
		switch part {
		case "secret":
			option.Secret = true
		case "required":
			option.Required = true
		default:
			return Option{}, fmt.Errorf("%s: ghc tag says %q, which is not secret, required or example=…", key, part)
		}
	}
	if option.Example == "" {
		return Option{}, fmt.Errorf("%s: ghc tag carries no example=…, and a form field with no example "+
			"tells a reader nothing about the shape of the value", key)
	}
	return option, nil
}

// kindOf is the Go type as the page reads it. A pointer is followed first: the
// tri-state booleans and the groups list are pointers so that an absent key
// and an empty one can be told apart, which is a fact about the default, not
// about the control a reader is shown.
func kindOf(key string, t reflect.Type) (string, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return KindString, nil
	case reflect.Bool:
		return KindBool, nil
	case reflect.Int, reflect.Int64:
		return KindInt, nil
	case reflect.Slice:
		return KindList, nil
	case reflect.Map:
		return KindMap, nil
	default:
		return "", fmt.Errorf("%s is a %s, which the configuration builder has no control for. "+
			"Teach kindOf in internal/config/options.go what to offer for it", key, t.Kind())
	}
}

// resolvedDefaults is every value the resolver fills in when a file leaves the
// key out, by key.
//
// It is measured rather than declared: a probe naming only what validation
// cannot do without is validated, and every value that is not what the probe
// set is one Validate filled. So github.reserve_rate is 500 here because
// resolveGitHub puts 500 there, and the day it stops being 500 the page says
// the new number without anybody editing this file.
//
// Two kinds of default cannot be measured that way and are derived instead,
// each by asking the code that answers the question. A setting whose default
// lives in the accessor that reads it (github.timeout, sinks.dedupe_horizon)
// is read through that accessor on a zero value, which is what the two log
// settings do too: neither is validated at load time, and what an empty level
// or an empty format means is decided by SlogLevel and by the order of
// LogFormats. A tri-state boolean resolves through Enabled, and Enabled(nil)
// is the answer for every one of them.
func resolvedDefaults() (map[string]string, error) {
	probe, set := probeConfig()
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("the probe configuration the defaults are read from does not validate, "+
			"so no default can be reported: %w", err)
	}
	out := map[string]string{
		"github.timeout":       compact(GitHub{}.HTTPTimeout()),
		"sinks.dedupe_horizon": compact((&Sinks{}).DedupeAge()),
		keyLogLevel:            strings.ToLower(Log{}.SlogLevel().String()),
		keyLogFormat:           LogFormats()[0],
	}
	readDefaults(reflect.ValueOf(probe).Elem(), "", set, out)
	return out, nil
}

// probeConfig is a configuration carrying nothing but what validation refuses
// to run without, and the set of keys it carries, so that what the probe set
// is never reported as a default.
//
// Every sink is allocated, because a sink left nil resolves to nothing and its
// defaults would be missing from the page. targets.user is set although no tag
// marks it required: the rule there is a choice between three keys, which
// belongs to targets as a whole rather than to any one of them.
func probeConfig() (probe *Config, set map[string]bool) {
	probe = &Config{
		GitHub:  GitHub{Token: "probe"},
		Targets: Targets{User: "probe"},
		Sinks: Sinks{
			Influx:        &InfluxSink{URL: probeURL, Bucket: "probe"},
			Prometheus:    &PrometheusSink{},
			OTLP:          &OTLPSink{Endpoint: probeURL},
			Loki:          &LokiSink{URL: probeURL},
			File:          &FileSink{Path: "probe"},
			Telegraf:      &TelegrafSink{URL: probeURL},
			Graphite:      &GraphiteSink{Addr: "probe:2003"},
			SQL:           &SQLSink{Path: "probe"},
			Elasticsearch: &ElasticsearchSink{URL: probeURL},
		},
	}
	set = map[string]bool{}
	for _, key := range []string{
		"github.token", "targets.user",
		"sinks.influxdb.url", "sinks.influxdb.bucket", "sinks.otlp.endpoint", "sinks.loki.url",
		"sinks.file.path", "sinks.telegraf.url", "sinks.graphite.addr", "sinks.sql.path",
		"sinks.elasticsearch.url",
	} {
		set[key] = true
	}
	return probe, set
}

// readDefaults walks the validated probe beside the types and records what the
// resolver left behind.
func readDefaults(v reflect.Value, prefix string, set map[string]bool, out map[string]string) {
	t := v.Type()
	for i, field := range enumerate(t) {
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		key := prefix + name
		fv := v.Field(i)
		if fv.Kind() == reflect.Pointer && field.Type.Elem().Kind() == reflect.Struct {
			if !fv.IsNil() {
				readDefaults(fv.Elem(), key+".", set, out)
			}
			continue
		}
		if fv.Kind() == reflect.Struct {
			readDefaults(fv, key+".", set, out)
			continue
		}
		if set[key] {
			continue
		}
		if value, filled := defaultValue(fv); filled {
			out[key] = value
		}
	}
}

// enumerate pairs each field with its index, which reflect.Value.Field needs
// and reflect.Type.Fields does not hand out.
func enumerate(t reflect.Type) map[int]reflect.StructField {
	out := make(map[int]reflect.StructField, t.NumField())
	for i := range t.NumField() {
		out[i] = t.Field(i)
	}
	return out
}

// defaultValue renders one resolved value, and says whether there is a default
// worth reporting at all.
//
// A zero string, a zero number and an empty list are the resolver declining to
// fill anything in, so they are not defaults. A boolean is always reported,
// because false is a real answer to a checkbox and an unreported one would
// leave the control claiming nothing; a tri-state boolean answers through
// Enabled, which is where "absent means on" is decided.
func defaultValue(v reflect.Value) (string, bool) {
	if v.Kind() == reflect.Pointer {
		if on, is := reflect.TypeAssert[*bool](v); is {
			return strconv.FormatBool(Enabled(on)), true
		}
		return "", false
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), v.String() != ""
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), true
	case reflect.Int, reflect.Int64:
		if d, is := reflect.TypeAssert[time.Duration](v); is {
			return compact(d), d != 0
		}
		return strconv.FormatInt(v.Int(), 10), v.Int() != 0
	case reflect.Slice:
		if v.Len() == 0 {
			return "", false
		}
		out := make([]string, v.Len())
		for i := range v.Len() {
			out[i] = fmt.Sprint(v.Index(i).Interface())
		}
		return strings.Join(out, ", "), true
	default:
		return "", false
	}
}
