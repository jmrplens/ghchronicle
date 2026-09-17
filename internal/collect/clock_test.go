package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// clockException is the one file here allowed to read the wall clock, and why.
//
// Refusals remembers, in memory, which endpoints answered 403 or 404 and when
// to ask them again. Nothing it computes reaches a point: it is a cache
// horizon, it belongs to the process rather than to the data, and it already
// takes an injected clock for its own tests. Everything else in this package
// turns GitHub's answers into points, and a point's fields are the thing this
// rule is about.
const clockException = "refusals.go"

// TestNoCollectorReadsTheWallClock is the door the sweep-wide clock gate
// cannot watch.
//
// TestNoRowDatedInThePastMovesWithTheClock in internal/run sweeps the fake
// account under two clocks and compares every past-dated row, which catches a
// field computed from the `now` a collector is handed. It cannot catch a
// collector that calls time.Now() itself, because the test moves the sweep's
// clock and not the process's: written both ways, the same field on the same
// row fails when it is derived from the parameter and passes when it is
// derived from a direct call. And the direct call is the easier thing to
// write, because it threads no parameter.
//
// So the wall clock is read in one place here and every collector takes its
// instant from the sweep. That is also what makes a collector testable at all:
// every fixture in this package pins its answers to testNow.
func TestNoCollectorReadsTheWallClock(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("cannot list the package: %v", err)
	}
	sort.Strings(files)
	checked, exception := 0, false
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if path == clockException {
			exception = true
			continue
		}
		checked++
		checkNoWallClock(t, path)
	}
	if checked == 0 {
		t.Fatal("no collector was read, so this test proves nothing")
	}
	// The exception is a file, so a rename that leaves the call behind would
	// otherwise turn this gate off rather than fail it.
	if !exception {
		t.Fatalf("%s is not in this package any more: move the exception or drop it", clockException)
	}
}

// checkNoWallClock fails for each time.Now or time.Since in path.
//
// Both, because time.Since is time.Now with a subtraction, and a duration
// since something the collector read is exactly the shape that put an age on a
// row dated when the thing happened.
func checkNoWallClock(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Now" && sel.Sel.Name != "Since") {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "time" {
			return true
		}
		pos := fset.Position(sel.Pos())
		t.Errorf("%s:%d reads the wall clock with time.%s. A collector takes its "+
			"instant from the `now` the sweep hands it, so that a row dated when the "+
			"thing happened says the same thing whenever it is collected, and so that "+
			"the gate in internal/run can see it; %s is the one exception and says why",
			filepath.Base(pos.Filename), pos.Line, sel.Sel.Name, clockException)
		return true
	})
}
