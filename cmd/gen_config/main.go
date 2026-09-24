// Command gen_config writes the configuration surface the documentation site's
// builder offers, out of the code that defines it.
//
// The builder is a form. A form is a list of controls, and a list of controls
// typed into a page is a copy of the configuration types that nothing holds to
// them: it can offer a setting the binary dropped, and it can miss one the
// binary gained, and both read as a page that renders. That is the same defect
// the layouts page had before cmd/gen_layouts, and this is the same remedy
// with a different source.
//
// Three things are exported, and each has exactly one author:
//
//   - the settings, from internal/config's own types (config.Options), which
//     reads the yaml tags for the shape, the ghc tags for what a form needs
//     that a type cannot say, and the resolver itself for every default;
//   - the cadence vocabulary, from the family table, so the cadence section
//     offers the families and groups that exist with the built-in interval
//     each of them would keep;
//   - the Action's inputs, read out of action.yml, because the composite
//     Action is what the second output of the builder is a step of, and its
//     input names are that file's to decide.
//
// The mapping between the two last, which config setting an Action input
// stands in for, is the one table here, and it is checked in both directions:
// a name on either side that no longer exists fails this command, and
// -check fails CI.
//
//	go run ./cmd/gen_config            # write site/src/data/config-options.json
//	go run ./cmd/gen_config -check     # write nothing, fail if it is stale
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// defaultOut is where the site reads the file from, and defaultAction is the
// composite Action, both relative to the repository root, which is where every
// other generator here is run from.
const (
	defaultOut    = "site/src/data/config-options.json"
	defaultAction = "action.yml"
)

// surface is the whole exported file.
type surface struct {
	Options  []config.Option `json:"options"`
	Groups   []group         `json:"groups"`
	Families []family        `json:"families"`
	Action   action          `json:"action"`
}

// group is one group of families, with the line -groups prints beside it.
type group struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Families    []string `json:"families"`
}

// family is one collector, with the group it answers to and the cadence it
// keeps when no config file touches it. "0" is a family that ships switched
// off, which only its own entry under every.families can turn on.
type family struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	Every string `json:"every"`
}

// action is the composite Action as the builder's second output needs it: the
// inputs it takes, and which of them stands in for a configuration setting.
type action struct {
	Inputs []input           `json:"inputs"`
	Map    map[string]string `json:"map"`
}

// input is one of the Action's inputs. The description is not exported: it is
// English prose, the page is published in two languages, and the page links to
// the Actions page rather than restating it.
type input struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Default  string `json:"default"`
}

// inputFor is the configuration setting each Action input stands in for.
//
// The Action takes a path to a configuration file, so most settings reach it
// through that file and not through an input. These four are the ones a run
// with no file can still be told, and they are the ones the builder can put
// into the step itself. Both sides are checked: the key must be a setting
// config.Options knows, and the input must be one action.yml declares.
//
// github.token is here and is never given a value: the builder writes the
// secret reference the documentation asks for, the way every workflow in this
// repository does.
var inputFor = map[string]string{
	"github.token":            "token",
	"targets.user":            "user",
	"targets.include_private": "include-private",
	"backfill.since":          "backfill-since",
}

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run is the whole command, with the arguments, the two streams and the exit
// status passed in and handed back rather than taken from the process, so a
// test can drive every way it ends. args[0] is the program name, as in os.Args.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", defaultOut, "the file the site reads the configuration surface from")
	actionPath := flags.String("action", defaultAction, "the composite Action whose inputs are exported")
	check := flags.Bool("check", false, "compare with what is on disk instead of writing")
	// The statuses a flag.ExitOnError set would exit with: a request for the
	// usage is not a failure, a flag it does not know is.
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	path := filepath.Clean(*out)
	body, count, err := surfaceJSON(filepath.Clean(*actionPath))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		old, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, readErr)
			return 1
		}
		if !bytes.Equal(old, body) {
			fmt.Fprintf(stderr, "%s no longer matches internal/config and %s. "+
				"Regenerate it with: go run ./cmd/gen_config\n", path, *actionPath)
			return 1
		}
		fmt.Fprintf(stdout, "%s is up to date\n", path)
		return 0
	}
	if err = os.WriteFile(path, body, 0o600); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s, %d settings\n", path, count)
	return 0
}

// surfaceJSON is the file's exact bytes: two-space JSON with a trailing
// newline, which is what prettier asks of a JSON file in the site. A generator
// whose output the site's own formatter then rejects is a gate that cannot be
// satisfied, so the file is in site/.prettierignore and this is its only
// author.
//
// It answers with the number of settings as well, so the line the command
// prints says what it exported rather than that it exported something.
func surfaceJSON(actionPath string) (body []byte, settings int, err error) {
	options, err := config.Options()
	if err != nil {
		return nil, 0, err
	}
	inputs, err := readInputs(actionPath)
	if err != nil {
		return nil, 0, err
	}
	// An empty anything here is refused rather than exported, so that -check
	// cannot pass over nothing: a committed empty list compared with a
	// generated empty list is a gate that agrees with itself and says nothing
	// about any setting. config.Options makes the same refusal of its own.
	// Before the mapping, so that an Action with nothing in it is reported as
	// itself rather than as the first mapping that cannot find its input.
	if len(inputs) == 0 {
		return nil, 0, fmt.Errorf("%s declares no inputs, so the builder could generate no workflow step", actionPath)
	}
	if mapErr := checkMapping(inputFor, options, inputs); mapErr != nil {
		return nil, 0, mapErr
	}
	families := config.Families()
	if len(families) == 0 {
		return nil, 0, errors.New("the family table is empty, so the cadence section would offer nothing")
	}
	body, err = json.MarshalIndent(surface{
		Options:  options,
		Groups:   groups(),
		Families: cadences(families),
		Action:   action{Inputs: inputs, Map: inputFor},
	}, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	return append(body, '\n'), len(options), nil
}

// groups is every group of families with its description and its members, all
// three read from the family table rather than from a list beside it.
func groups() []group {
	out := make([]group, 0, len(config.Groups()))
	for _, name := range config.Groups() {
		out = append(out, group{
			Name:        name,
			Description: config.GroupDescription(name),
			Families:    config.FamiliesIn(name),
		})
	}
	return out
}

// cadences is every family with its group and its built-in interval.
func cadences(families []string) []family {
	out := make([]family, 0, len(families))
	for _, name := range families {
		group, _ := config.GroupOf(name)
		every, _ := config.BuiltinEvery(name)
		// A plain 0 for a family that ships switched off, which is how
		// config.example.yaml writes it and what the page has to offer as the
		// value a reader changes; anything else through the same printer the
		// warnings quote, so the page offers 15m rather than 15m0s.
		cadence := "0"
		if every > 0 {
			cadence = config.Compact(every)
		}
		out = append(out, family{Name: name, Group: group, Every: cadence})
	}
	return out
}

// readInputs reads the composite Action's inputs, in the order it declares
// them, which is the order a reader meets them in the Actions page.
func readInputs(path string) ([]input, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Inputs yaml.Node `yaml:"inputs"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Inputs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: inputs is not a mapping", path)
	}
	// A mapping node keeps the file's order, which a map[string]… would lose.
	out := make([]input, 0, len(doc.Inputs.Content)/2)
	for i := 0; i+1 < len(doc.Inputs.Content); i += 2 {
		var body struct {
			Required bool   `yaml:"required"`
			Default  string `yaml:"default"`
		}
		if err = doc.Inputs.Content[i+1].Decode(&body); err != nil {
			return nil, fmt.Errorf("%s: input %s: %w", path, doc.Inputs.Content[i].Value, err)
		}
		out = append(out, input{
			Name:     doc.Inputs.Content[i].Value,
			Required: body.Required,
			Default:  body.Default,
		})
	}
	return out, nil
}

// checkMapping holds a mapping to both the things it names. The mapping is
// passed in rather than read off the package variable, so a test can drive
// each failure without editing state every other test is reading.
//
// A setting renamed leaves a key here pointing at nothing, and an input
// renamed leaves a value here pointing at nothing. Neither breaks a build, and
// both would leave the builder writing a step the Action ignores, which is the
// quietest way for a generated page to be wrong.
func checkMapping(mapping map[string]string, options []config.Option, inputs []input) error {
	known := make(map[string]bool, len(options))
	for _, o := range options {
		known[o.Key] = true
	}
	names := make([]string, 0, len(inputs))
	for _, in := range inputs {
		names = append(names, in.Name)
	}
	for _, key := range slices.Sorted(maps.Keys(mapping)) {
		if !known[key] {
			return fmt.Errorf("inputFor maps %s, which is not a setting any more; "+
				"see inputFor in cmd/gen_config", key)
		}
		if !slices.Contains(names, mapping[key]) {
			return fmt.Errorf("inputFor sends %s to the Action input %q, which action.yml does not declare (it has: %s)",
				key, mapping[key], strings.Join(names, ", "))
		}
	}
	return nil
}
