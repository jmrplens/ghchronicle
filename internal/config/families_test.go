package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// TestEveryScheduledFamilyHasACadence reads the runner's own source and fails
// when it schedules a family this package has no default interval for.
//
// resolveIntervals rejects an unknown name in every: a family the runner runs
// but this map does not know cannot be configured at all, and one the config
// file names but the runner never runs is rejected at start-up. Both have
// happened here, which is why the check reads the code rather than a list
// somebody has to remember to update.
func TestEveryScheduledFamilyHasACadence(t *testing.T) {
	for _, family := range familiesTheRunnerSchedules(t) {
		if _, known := defaultEvery[family]; !known {
			t.Errorf("internal/run/runner.go schedules %q and defaultEvery has no entry for it, "+
				"so a config naming it is rejected at start-up and it never runs at all", family)
		}
	}
}

// TestNoCadenceOutlivesItsFamily is the other direction: a cadence for a
// family nobody runs is a name the config file accepts and nothing acts on.
func TestNoCadenceOutlivesItsFamily(t *testing.T) {
	scheduled := map[string]bool{}
	for _, family := range familiesTheRunnerSchedules(t) {
		scheduled[family] = true
	}
	names := make([]string, 0, len(defaultEvery))
	for name := range defaultEvery {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !scheduled[name] {
			t.Errorf("defaultEvery carries %q, which internal/run/runner.go never schedules", name)
		}
	}
}

// familiesTheRunnerSchedules is every family name the runner names: the
// entries of perRepoFamilies, and the name handed to each r.family call.
func familiesTheRunnerSchedules(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "run", "runner.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		// Skipping here would restore exactly the hole this test closes.
		t.Fatalf("cannot read %s: %v", path, err)
	}
	var named []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ValueSpec:
			named = append(named, perRepoFamiliesElements(node)...)
		case *ast.CallExpr:
			if arg, ok := familyCallName(node); ok {
				named = append(named, arg)
			}
		}
		return true
	})
	var found []string
	for _, e := range named {
		if name, ok := stringLiteral(e); ok {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no family name found in %s, so this test proves nothing", path)
	}
	return found
}

// perRepoFamiliesElements returns the elements of the perRepoFamilies
// literal, and nothing for any other declaration.
func perRepoFamiliesElements(spec *ast.ValueSpec) []ast.Expr {
	for i, name := range spec.Names {
		if name.Name != "perRepoFamilies" || i >= len(spec.Values) {
			continue
		}
		if lit, ok := spec.Values[i].(*ast.CompositeLit); ok {
			return lit.Elts
		}
	}
	return nil
}

// familyCallName returns the name argument of an r.family call. In
// r.family(ctx, "name", now, func() ...) the family name is the second
// argument, and it is a literal in every call.
func familyCallName(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "family" || len(call.Args) < 2 {
		return nil, false
	}
	return call.Args[1], true
}

// stringLiteral returns the value of a string literal, and false for any
// other expression.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}
