package dashboards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// collectDir is the collectors' own source, which is the only authority on
// what measurements exist.
const collectDir = "../../../internal/collect"

// tableExclusions is the one measurement deliberately absent from the table,
// for the reason given in tags.go: gh_job_log has no panel and so no node
// index to get wrong.
var tableExclusions = map[string]bool{"gh_job_log": true}

// TestEveryMeasurementIsInTheTagTable reads the collectors' own source and
// fails when one writes a measurement this table does not name.
//
// The table decides the node index every Graphite target uses, so a
// measurement missing from it cannot be queried by position at all, and the
// omission is silent: nothing else in the build reads the two together. This
// is the same hole that internal/sink and internal/config each closed by
// asking the source rather than by trusting a hand-written list.
func TestEveryMeasurementIsInTheTagTable(t *testing.T) {
	t.Parallel()
	for _, m := range measurementsInCollectors(t) {
		if tableExclusions[m] {
			continue
		}
		if _, ok := tags[m]; !ok {
			t.Errorf("internal/collect writes %s and the tag table has no entry for it, "+
				"so every Graphite target that indexes its nodes by position is written blind. "+
				"Add its tag keys, or record it in tableExclusions with the reason", m)
		}
	}
}

// TestNoTagTableEntryOutlivesItsCollector is the other direction: an entry for
// a measurement nobody writes any more sets node indices for a path that never
// arrives.
func TestNoTagTableEntryOutlivesItsCollector(t *testing.T) {
	t.Parallel()
	written := map[string]bool{}
	for _, m := range measurementsInCollectors(t) {
		written[m] = true
	}
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !written[name] {
			t.Errorf("the tag table carries %s, which no collector writes", name)
		}
	}
}

// TestEveryRepositoryTagSetIsTheSameShape fails when an entry names a
// repository in any shape but the one every measurement now uses.
//
// The three keys travel together or not at all. `repo` alone was the twelve's
// old shape, where it held the full name; `owner` and `repo` without
// `full_name` would leave a reader rejoining a string to get an identity back,
// which is the work this shape exists to remove. The table is hand-written and
// decides the node index of every Graphite target, so an entry that drifts
// takes the paths with it.
func TestEveryRepositoryTagSetIsTheSameShape(t *testing.T) {
	t.Parallel()
	const owner, repo, full = "owner", "repo", "full_name"
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		has := map[string]bool{}
		for _, tag := range tags[name] {
			has[tag] = true
		}
		if !has[owner] && !has[repo] && !has[full] {
			continue
		}
		if has[owner] && has[repo] && has[full] {
			continue
		}
		t.Errorf("%s names a repository with %v. A measurement that names one carries "+
			"all three of %q, %q and %q, with %q holding the short name: %q alone is the "+
			"shape twelve measurements used to have, where it held the full name and no "+
			"filter built from any other measurement could match it",
			name, tags[name], owner, repo, full, repo, repo)
	}
}

// measurementsInCollectors is every measurement name written as a literal in
// internal/collect, sorted.
func measurementsInCollectors(t *testing.T) []string {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(collectDir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				addMeasurement(t, fset, kv, found)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		// Skipping here would restore exactly the hole this test closes.
		t.Fatalf("cannot read %s: %v", collectDir, walkErr)
	}
	if len(found) == 0 {
		t.Fatalf("no measurement found in %s, so this test proves nothing", collectDir)
	}
	out := make([]string, 0, len(found))
	for m := range found {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// addMeasurement records the name a `Measurement:` field sets, when kv is one.
func addMeasurement(t *testing.T, fset *token.FileSet, kv *ast.KeyValueExpr, found map[string]bool) {
	t.Helper()
	key, ok := kv.Key.(*ast.Ident)
	if !ok || key.Name != "Measurement" {
		return
	}
	lit, ok := kv.Value.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		// A name assembled at run time would make this test blind without
		// failing it, which is the failure it exists to prevent. Every one is
		// a literal today.
		pos := fset.Position(kv.Pos())
		t.Errorf("%s:%d writes a measurement whose name is not a literal",
			filepath.Base(pos.Filename), pos.Line)
		return
	}
	if value, err := strconv.Unquote(lit.Value); err == nil {
		found[value] = true
	}
}
