package config

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads a YAML config file and validates it.
func Load(path string) (*Config, error) { return LoadWith(path, false) }

// LoadWith is Load, with a caller that supplies its own destination able to
// waive the "configure at least one sink" rule.
func LoadWith(path string, allowNoSinks bool) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	c.AllowNoSinks = allowNoSinks
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
