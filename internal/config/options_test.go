package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEverySettingTheTypesDeclareIsOffered is the claim the configuration
// builder rests on: the inventory it publishes is the inventory of the types,
// neither short nor long.
//
// It compares against yamlKeys, the walk documented_test.go already does over
// the same tags for config.example.yaml, so the two inventories a reader can
// meet, the example file and the page, are held to one list.
func TestEverySettingTheTypesDeclareIsOffered(t *testing.T) {
	options, err := Options()
	if err != nil {
		t.Fatal(err)
	}
	var offered []string
	for _, o := range options {
		offered = append(offered, o.Key)
	}
	want := yamlKeys(reflect.TypeFor[Config](), "")
	if !slices.Equal(offered, want) {
		t.Errorf("Options offers\n%v\nand the yaml tags declare\n%v", offered, want)
	}
}

// TestEveryLeafCarriesAnExampleAndEveryBlockCarriesNone is what the ghc tag is
// for. A control with no example tells a reader nothing about the shape of the
// value, and a block is a heading rather than a control.
func TestEveryLeafCarriesAnExampleAndEveryBlockCarriesNone(t *testing.T) {
	options, err := Options()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range options {
		if o.Kind == KindBlock {
			if o.Example != "" || o.Secret || o.Required {
				t.Errorf("%s is a block and carries %+v", o.Key, o)
			}
			continue
		}
		if o.Example == "" {
			t.Errorf("%s is a setting with no example", o.Key)
		}
	}
}

// TestASettingWithNoTagIsRefusedByName covers the failure a new field
// produces, which is the whole reason the tag is required rather than
// optional: without it the setting would reach the page nameless.
func TestASettingWithNoTagIsRefusedByName(t *testing.T) {
	type untagged struct {
		Thing string `yaml:"thing"`
	}
	_, err := walkOptions(reflect.TypeFor[untagged](), "")
	if err == nil || !strings.Contains(err.Error(), "thing") ||
		!strings.Contains(err.Error(), "ghc") {
		t.Fatalf("a field with no ghc tag = %v, want the key and the tag named", err)
	}
}

// TestATagThisPackageCannotReadIsRefused covers a misspelled flag, which would
// otherwise be read as nothing at all and leave a credential unmarked.
func TestATagThisPackageCannotReadIsRefused(t *testing.T) {
	type mistyped struct {
		Thing string `yaml:"thing" ghc:"secrets,example=x"`
	}
	_, err := walkOptions(reflect.TypeFor[mistyped](), "")
	if err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("an unknown ghc flag = %v, want it quoted back", err)
	}
}

// TestAnExampleWithACommaSurvives pins the one thing the tag's grammar has to
// get right: example comes last and takes the rest of the tag, because a list
// setting's example is itself a comma separated list.
func TestAnExampleWithACommaSurvives(t *testing.T) {
	options, err := Options()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(options, func(o Option) bool { return o.Key == "targets.orgs" })
	if i < 0 {
		t.Fatal("targets.orgs is not offered")
	}
	if !strings.Contains(options[i].Example, ", ") {
		t.Errorf("targets.orgs shows %q, want the whole list the tag carries", options[i].Example)
	}
}

// TestASettingThisPackageHasNoControlForIsRefused covers a field of a type the
// page cannot offer, which has to fail the generator rather than be dropped
// from a form that then silently cannot set it.
func TestASettingThisPackageHasNoControlForIsRefused(t *testing.T) {
	type odd struct {
		Thing float64 `yaml:"thing" ghc:"example=1.5"`
	}
	_, err := walkOptions(reflect.TypeFor[odd](), "")
	if err == nil || !strings.Contains(err.Error(), "float64") {
		t.Fatalf("a field of an unsupported kind = %v, want the kind named", err)
	}
}

// TestTheDefaultsAreTheOnesTheResolverFills reads two of them back through
// Validate itself, so the claim is that the exported default is the behavior
// rather than a number typed beside it.
func TestTheDefaultsAreTheOnesTheResolverFills(t *testing.T) {
	options, err := Options()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Option{}
	for _, o := range options {
		byKey[o.Key] = o
	}
	resolved := &Config{
		GitHub:  GitHub{Token: "x"},
		Targets: Targets{User: "someone"},
		Sinks:   Sinks{Stdout: true},
	}
	if err = resolved.Validate(); err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(resolved.GitHub.ReserveRate); byKey["github.reserve_rate"].Default != want {
		t.Errorf("github.reserve_rate is exported as %q, Validate fills %q",
			byKey["github.reserve_rate"].Default, want)
	}
	if byKey["state_file"].Default != resolved.StateFile {
		t.Errorf("state_file is exported as %q, Validate fills %q",
			byKey["state_file"].Default, resolved.StateFile)
	}
	// A tri-state boolean is the case a probe cannot answer, because the value
	// the file leaves out is nil and nil is not a value. It is exported
	// through Enabled, which is where "absent means on" is decided.
	if byKey["targets.include_private"].Default != "true" {
		t.Errorf("targets.include_private is exported as %q, and Targets{}.PrivateIncluded() is %v",
			byKey["targets.include_private"].Default, Targets{}.PrivateIncluded())
	}
}

// TestEveryChoiceOfferedLoads is the promise a dropdown makes. Each value the
// builder offers for an enumerated setting is written into a configuration on
// its own and loaded, so an option list that came to hold a value the resolver
// refuses fails here rather than on a reader's first run.
func TestEveryChoiceOfferedLoads(t *testing.T) {
	options, err := Options()
	if err != nil {
		t.Fatal(err)
	}
	// Each enumerated setting with a configuration that is legal apart from
	// the value under test, and the line that setting is written on.
	blocks := map[string]func(string) string{
		"sinks.stdout_format": func(v string) string {
			return "sinks:\n  stdout: true\n  stdout_format: " + v + "\n"
		},
		"sinks.file.format": func(v string) string {
			return "sinks:\n  file:\n    path: points.lp\n    format: " + v + "\n"
		},
		"sinks.sql.dialect": func(v string) string {
			return "sinks:\n  sql:\n    path: points.sql\n    dialect: " + v + "\n"
		},
		"log.level":  func(v string) string { return "sinks:\n  stdout: true\nlog:\n  level: " + v + "\n" },
		"log.format": func(v string) string { return "sinks:\n  stdout: true\nlog:\n  format: " + v + "\n" },
	}
	var checked int
	for _, o := range options {
		if len(o.Choices) == 0 {
			continue
		}
		block, known := blocks[o.Key]
		if !known {
			t.Errorf("%s offers a choice of %v and this test has no configuration for it", o.Key, o.Choices)
			continue
		}
		for _, choice := range o.Choices {
			body := "github:\n  token: x\ntargets:\n  user: someone\n" + block(choice)
			if _, err = Load(writeConfig(t, body)); err != nil {
				t.Errorf("%s: %s is offered and does not load: %v", o.Key, choice, err)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no enumerated setting was checked, so this test proves nothing")
	}
}

// TestEveryExampleOfferedLoads is the same promise for the value a control
// shows as its placeholder: a reader who copies it must get a file that
// starts. Every setting is written on its own, with the minimum around it,
// because most of them are optional and several are mutually exclusive.
func TestEveryExampleOfferedLoads(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	options, err := Options()
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, o := range options {
		if o.Kind == KindBlock {
			continue
		}
		body := exampleConfigFor(t, o, options)
		if _, err = Load(writeConfig(t, body)); err != nil {
			t.Errorf("%s: the example %q does not load:\n%s\n%v", o.Key, o.Example, body, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no example was checked, so this test proves nothing")
	}
}

// exampleConfigFor writes one setting at its example value, inside the
// smallest configuration that can carry it.
//
// The document is built as nested maps and marshaled by the same YAML library
// the loader reads it with, rather than assembled as text: a second hand
// written emitter here would be a second thing to get wrong, and the first
// version of it wrote `github:` twice.
func exampleConfigFor(t *testing.T, o Option, options []Option) string {
	t.Helper()
	doc := map[string]any{
		"github":  map[string]any{"token": "x"},
		"targets": map[string]any{"user": "someone"},
	}
	// A destination, because a configuration with none is refused whatever
	// else it says. Standard output for the settings of every other block,
	// and the file sink for the four under sinks. that belong to no sink,
	// since one of them is sinks.stdout itself at its example of false.
	carry := "sinks.stdout"
	switch {
	case !strings.HasPrefix(o.Key, "sinks."):
		doc["sinks"] = map[string]any{"stdout": true}
	case o.Sink == "":
		carry = "sinks.file"
	default:
		carry = "sinks." + o.Sink
	}
	// A setting of a sink carries that sink's required keys with it. A sink
	// refuses to resolve without them, so writing sinks.influxdb.org on its
	// own would be testing the refusal rather than the example.
	for _, other := range options {
		if strings.HasPrefix(other.Key, carry+".") && other.Required && other.Key != o.Key {
			put(t, doc, other, exampleValue(t, other))
		}
	}
	put(t, doc, o, exampleValue(t, o))
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// put writes one setting at its dotted path, making the blocks above it.
func put(t *testing.T, doc map[string]any, o Option, value any) {
	t.Helper()
	parts := strings.Split(o.Key, ".")
	at := doc
	for _, part := range parts[:len(parts)-1] {
		next, held := at[part].(map[string]any)
		if !held {
			next = map[string]any{}
			at[part] = next
		}
		at = next
	}
	at[parts[len(parts)-1]] = value
}

// exampleValue reads one example back as a value of the kind the setting
// takes: a list as a sequence, a map as the one pair the example shows, a
// number as a number.
func exampleValue(t *testing.T, o Option) any {
	t.Helper()
	switch o.Kind {
	case KindList:
		return strings.Split(o.Example, ", ")
	case KindMap:
		key, value, _ := strings.Cut(o.Example, ": ")
		return map[string]string{key: value}
	case KindBool:
		return o.Example == "true"
	case KindInt:
		n, err := strconv.Atoi(o.Example)
		if err != nil {
			t.Fatalf("%s: the example %q is not a number", o.Key, o.Example)
		}
		return n
	default:
		return o.Example
	}
}

// writeConfig puts a configuration in a file of its own and answers with its
// path, because Load takes a path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
