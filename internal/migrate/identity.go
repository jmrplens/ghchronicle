package migrate

import (
	_ "embed"
	"encoding/json"
	"slices"
	"sync"
)

// identityFile is the tag keys of every measurement one sweep of the fake
// GitHub writes, measurement by measurement. It is the gate that makes an
// identity change impossible to ship unregistered: the test beside it sweeps
// the fake again, fails on any difference, and rewrites this file with
// -update only when Registry explains every tag that went away. The planner
// reads it too, for the one thing only a tag count gives: how deep Graphite
// nested a measurement's old paths.
//
//go:embed identity.json
var identityFile []byte

// identity is identityFile read once. A file that does not parse is a build
// that cannot have passed its own tests, so it reads as empty rather than as
// a reason to stop a collector.
var identity = sync.OnceValue(func() map[string][]string {
	out := map[string][]string{}
	if err := json.Unmarshal(identityFile, &out); err != nil {
		return map[string][]string{}
	}
	return out
})

// Tags is the tag keys this release writes on a measurement, sorted, and
// whether the sweep the file was made from wrote it at all.
func Tags(measurement string) ([]string, bool) {
	tags, ok := identity()[measurement]
	return slices.Clone(tags), ok
}
