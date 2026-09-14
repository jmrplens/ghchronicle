package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOrNoneKeepsWhatGitHubSent(t *testing.T) {
	t.Parallel()
	if got := orNone(""); got != noneTag {
		t.Errorf("orNone(%q) = %q, want %q", "", got, noneTag)
	}
	// The two words this fallback used to be spelled with are values GitHub
	// itself sends, and both have to survive as themselves. That is the whole
	// argument for the parentheses: with either spelling as the fallback,
	// these two rows and a row GitHub said nothing about are one series.
	for _, sent := range []string{"unknown", "none"} {
		if got := orNone(sent); got != sent {
			t.Errorf("orNone(%q) = %q, a value GitHub sent must survive", sent, got)
		}
	}
}

// TestOneSpellingOfTheFallback reads this package's own source and fails when
// a second fallback is written into it.
//
// There were five, spelling one rule three different ways (orNone, tagOrUnknown,
// alertTagOr, inventoryTagOr and unknownIfEmpty, plus the noneTag constant),
// which is why docs/metrics.md could not say what the fallback was: the answer
// depended on which collector had written the row. A sixth would be four lines
// in a collector nobody reads next to this one, so the check is mechanical.
func TestOneSpellingOfTheFallback(t *testing.T) {
	t.Parallel()
	found := fallbackFunctions(t)
	if len(found) == 0 {
		t.Fatal("orNone itself was not recognized, so this test proves nothing")
	}
	for _, f := range found {
		if f.name != "orNone" {
			t.Errorf("%s:%s is a second spelling of the tag fallback. There is one, orNone in "+
				"strings.go, and every collector calls it: a tag value differs between two "+
				"measurements otherwise, and the documented rule stops being true.",
				f.where, f.name)
		}
		if f.returns != noneTag {
			t.Errorf("%s:%s falls back to %q rather than noneTag (%q)",
				f.where, f.name, f.returns, noneTag)
		}
	}
}

// fallback is one function of the shape "return a constant when the argument
// is empty, and the argument otherwise", which is the rule written out.
type fallback struct{ name, where, returns string }

// fallbackFunctions finds that shape in the package source, whatever the
// author called it: one string parameter, a first statement comparing it to
// "" and returning a string literal, and a final return of the parameter.
func fallbackFunctions(t *testing.T) []fallback {
	t.Helper()
	var found []fallback
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc {
				continue
			}
			if lit, ok := fallbackLiteral(fn); ok {
				found = append(found, fallback{fn.Name.Name, path, lit})
			}
		}
		return nil
	})
	if walkErr != nil {
		// Skipping here would restore exactly the hole this test closes.
		t.Fatalf("cannot read the collectors: %v", walkErr)
	}
	return found
}

// fallbackLiteral reports the constant a function of that shape returns.
func fallbackLiteral(fn *ast.FuncDecl) (string, bool) {
	if fn.Recv != nil || fn.Body == nil || len(fn.Body.List) != 2 {
		return "", false
	}
	param, isSoleString := soleStringParam(fn.Type)
	if !isSoleString {
		return "", false
	}
	fallback, isGuard := emptyGuard(fn.Body.List[0], param)
	if !isGuard {
		return "", false
	}
	// The tail has to hand the argument back, or the function is a mapping
	// rather than a fallback and this test has no business reading it.
	if !returnsIdent(fn.Body.List[1], param) {
		return "", false
	}
	return constantString(fallback)
}

// soleStringParam names the parameter of a function that takes one string
// and nothing else.
func soleStringParam(sig *ast.FuncType) (string, bool) {
	if sig.Params == nil || len(sig.Params.List) != 1 || len(sig.Params.List[0].Names) != 1 {
		return "", false
	}
	if !isIdentNamed(sig.Params.List[0].Type, "string") {
		return "", false
	}
	return sig.Params.List[0].Names[0].Name, true
}

// emptyGuard reads `if param == "" { return X }` and hands back X.
func emptyGuard(stmt ast.Stmt, param string) (ast.Expr, bool) {
	guard, isIf := stmt.(*ast.IfStmt)
	if !isIf || guard.Else != nil || len(guard.Body.List) != 1 {
		return nil, false
	}
	cond, isBinary := guard.Cond.(*ast.BinaryExpr)
	if !isBinary || cond.Op != token.EQL || !isIdentNamed(cond.X, param) {
		return nil, false
	}
	empty, isLit := cond.Y.(*ast.BasicLit)
	if !isLit || empty.Kind != token.STRING || empty.Value != `""` {
		return nil, false
	}
	ret, isReturn := guard.Body.List[0].(*ast.ReturnStmt)
	if !isReturn || len(ret.Results) != 1 {
		return nil, false
	}
	return ret.Results[0], true
}

// returnsIdent reports whether stmt is `return name`.
func returnsIdent(stmt ast.Stmt, name string) bool {
	ret, isReturn := stmt.(*ast.ReturnStmt)
	return isReturn && len(ret.Results) == 1 && isIdentNamed(ret.Results[0], name)
}

// isIdentNamed reports whether expr is the bare identifier name.
func isIdentNamed(expr ast.Expr, name string) bool {
	ident, isIdent := expr.(*ast.Ident)
	return isIdent && ident.Name == name
}

// constantString reports the string a fallback returns: a string literal, or
// the noneTag constant itself, which is what orNone returns.
func constantString(expr ast.Expr) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		lit, err := strconv.Unquote(value.Value)
		return lit, err == nil
	case *ast.Ident:
		if value.Name == "noneTag" {
			return noneTag, true
		}
	}
	return "", false
}
