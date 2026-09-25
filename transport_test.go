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
// http.DefaultClient, and the package-level http.Get, Head, Post and PostForm
// that send through it, are the same pool reached without building anything,
// and the rule missed them until 2.5.1: the Grafana client, the uninstall's
// store calls, the setup's probe and the Docker suite's harness all sent
// through it.
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
		t.Errorf("these build an http.Client with no Transport, or send through "+
			"http.DefaultClient, so they share the process-wide connection pool with every "+
			"other client: %s. Give a client of their own httpx.OwnTransport(), or the "+
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
// built with no Transport field, or where a request goes out through the
// shared client.
func clientsWithoutTransport(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if isHTTPClient(node.Type) && !setsTransport(node) {
				found = append(found, fset.Position(node.Pos()).String())
			}
		case *ast.SelectorExpr:
			if sendsThroughTheSharedClient(node) {
				found = append(found, fset.Position(node.Pos()).String())
			}
		}
		return true
	})
	return found, nil
}

// sendsThroughTheSharedClient reports whether a selector is http.DefaultClient
// or one of the package-level functions that send through it.
func sendsThroughTheSharedClient(sel *ast.SelectorExpr) bool {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "http" {
		return false
	}
	switch sel.Sel.Name {
	case "DefaultClient", "Get", "Head", "Post", "PostForm":
		return true
	}
	return false
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

// TestEveryCommandKeepsItsMainEmpty.
//
// Nine of the ten commands here report no coverage for main, and that is the
// design rather than a gap: each one is `os.Exit(run(...))`, with the
// arguments, the streams and the status passed in and handed back so a test
// can drive every way the command ends. A main that cannot be entered without
// ending the process is worth nothing to cover, as long as nothing is hiding
// in it.
//
// This is what makes that last clause checkable. A main that grows a second
// statement has grown something no test reaches, and it fails here rather than
// shipping unexercised.
func TestEveryCommandKeepsItsMainEmpty(t *testing.T) {
	t.Parallel()
	commands, err := filepath.Glob(filepath.Join("cmd", "*", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) < 5 {
		t.Fatalf("found %d commands, which is too few to be the whole set", len(commands))
	}
	for _, path := range commands {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			body := mainBody(t, path)
			if body == nil {
				t.Skipf("%s declares no main", path)
			}
			// No branching, which is the property that matters: a statement
			// with no condition in it does one thing and a test of run covers
			// the thing it does, while an `if` here is a decision nothing
			// reaches. Counting statements instead would forbid the two mains
			// that build a client from the environment before handing it over,
			// which hide nothing.
			if where := branchIn(body); where != nil {
				t.Errorf("main branches at %T, and nothing a test can enter reaches it. "+
					"Move the decision into run, or into a seam beside it", where)
			}
		})
	}
}

// branchIn is the first control-flow statement anywhere inside the body, or
// nil when there is none.
func branchIn(body *ast.BlockStmt) ast.Node {
	var found ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt:
			found = n
			return false
		}
		return true
	})
	return found
}

// mainBody is the body of func main in the file at path, or nil when it has
// none.
func mainBody(t *testing.T, path string) *ast.BlockStmt {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "main" && fn.Recv == nil {
			return fn.Body
		}
	}
	return nil
}
