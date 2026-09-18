package ghchronicle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryHTTPClientBringsItsOwnTransport.
//
// An http.Client built with no Transport uses http.DefaultTransport, which is
// one connection pool shared by every client in the process. This program
// writes to several stores at once, so a store that is slow or gone holds
// connections the others are drawing from; and in the tests it is worse than
// untidy, because closing one test's server reaches into that shared pool and
// a request another test had in flight fails with "http: CloseIdleConnections
// called" rather than with anything about itself. That is a real failure this
// repository met on CI, in internal/sink, on a run that had nothing to do with
// the sink.
//
// Six clients had to be found by hand to fix it once. This is what stops the
// seventh: a rule that holds where somebody remembered to apply it is not a
// rule, so it is asked of the source rather than of anybody's memory.
//
// Test files are left out. The pool a test shares is the same pool, but a test
// that builds a client is usually driving an httptest server that hands one
// out, and holding those to this would be noise rather than a rule.
func TestEveryHTTPClientBringsItsOwnTransport(t *testing.T) {
	t.Parallel()
	var offenders []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return skipUninteresting(d)
		case !isOwnSource(path):
			return nil
		}
		found, parseErr := clientsWithoutTransport(path)
		offenders = append(offenders, found...)
		return parseErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("these build an http.Client with no Transport, so it shares the process-wide "+
			"connection pool with every other client: %s. Give it httpx.OwnTransport(), or the "+
			"transport of the client whose request it is carrying on",
			strings.Join(offenders, ", "))
	}
}

// skipUninteresting keeps the walk inside this module's own Go source. plan/ is
// scratch and is not part of it; the rest are not ours to hold to anything.
func skipUninteresting(d fs.DirEntry) error {
	switch d.Name() {
	case "plan", "node_modules", "site", "testdata", ".git":
		return filepath.SkipDir
	}
	return nil
}

// isOwnSource is a Go file this rule applies to. Test files are left out: the
// pool a test shares is the same pool, but a test that builds a client is
// usually driving an httptest server that hands one out, and holding those to
// this would be noise rather than a rule.
func isOwnSource(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// clientsWithoutTransport names each place in one file where an http.Client is
// built with no Transport field.
func clientsWithoutTransport(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if ok && isHTTPClient(lit.Type) && !setsTransport(lit) {
			found = append(found, fset.Position(lit.Pos()).String())
		}
		return true
	})
	return found, nil
}

// setsTransport reports whether a client literal names a Transport.
func setsTransport(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if name, isName := kv.Key.(*ast.Ident); isName && name.Name == "Transport" {
			return true
		}
	}
	return false
}

// isHTTPClient reports whether a composite literal's type is http.Client,
// written either as a value or behind a pointer.
func isHTTPClient(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}
