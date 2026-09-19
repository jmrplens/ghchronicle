package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads a YAML config file and validates it.
func Load(path string) (*Config, error) { return LoadWith(path, Relax{}) }

// Relax names the rules a caller may waive, because it is running something
// smaller than a sweep. Each one exists for a reason the waiving command does
// not meet: there must be a destination because a sweep has to put its points
// somewhere, and there must be a token because a sweep has to ask GitHub. A
// card written to a file does neither; reading the backfill's checkpoint does
// neither.
//
// A struct rather than a second boolean parameter, so a call site says which
// rule it is waiving instead of ending in "true, false".
type Relax struct {
	// NoSinks lets a caller that supplies its own destination configure none.
	NoSinks bool
	// NoToken lets a caller that makes no request run without one. The token
	// is still read when it is there: the command that waives this reports on
	// a configuration, and reporting on one that is missing its credential
	// would be the wrong kind of quiet.
	NoToken bool
}

// LoadWith is Load for a caller that may waive one of the rules in Relax.
func LoadWith(path string, relax Relax) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	c.AllowNoSinks = relax.NoSinks
	c.AllowNoToken = relax.NoToken
	// KnownFields makes a typo in a key an error instead of a setting that
	// silently does nothing, which is the worst way to discover a config bug.
	// It is also what keeps every's three layers unshadowable: default and
	// groups are fields of a struct, so no family can ever be read as one.
	dec := yaml.NewDecoder(newReader(raw))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		if hint := flatEveryHint(raw); hint != "" {
			return nil, fmt.Errorf("%s: %s", path, hint)
		}
		// A document with nothing in it ends where it starts, and the decoder
		// reports that as the end of the file. "EOF" on its own says the file
		// was read and nothing about what is missing from it, which is the
		// whole of what a reader who cleared every field is told.
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: the file is empty. A configuration needs at least an account under targets "+
				"and a destination under sinks; see config.example.yaml", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err = c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// flatEveryHint recognizes a config file written for the flat every block that
// came before the three layers, and says how to move it.
//
// Without this the reader of an older file gets "field traffic not found in
// type config.Every", which names neither the change nor the fix. It runs only
// once strict decoding has already failed, so it can weaken nothing: the
// struct is still what the decoder sees.
func flatEveryHint(raw []byte) string {
	var probe struct {
		Every map[string]yaml.Node `yaml:"every"`
	}
	if yaml.Unmarshal(raw, &probe) != nil {
		return ""
	}
	var moved []string
	for name := range probe.Every {
		if _, isFamily := GroupOf(name); isFamily {
			moved = append(moved, name)
		}
	}
	if len(moved) == 0 {
		return ""
	}
	slices.Sort(moved)
	return fmt.Sprintf("every: is now three layers, default, groups and families, and a family's own cadence lives under families. "+
		"Move %s under every.families, and see config.example.yaml", strings.Join(moved, ", "))
}
