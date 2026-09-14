package run

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestTheStateCommentAccountsForEveryFieldItKeeps reads this package's own
// source and fails when State persists something its doc comment does not
// mention.
//
// The comment said "Two things only" while the type had grown to six, and the
// four it left out are the four with consequences: deleting the file loses the
// commit a dependency diff starts from, when each family last read a whole
// page, where the notification window was cut, and the newest event seen. Two
// documents and the site repeated the sentence, because the comment is what
// they were written from.
func TestTheStateCommentAccountsForEveryFieldItKeeps(t *testing.T) {
	doc, fields := stateDocAndFields(t)
	for _, name := range fields {
		if !strings.Contains(doc, name) {
			t.Errorf("State persists %q and its doc comment never names it:\n%s", name, doc)
		}
	}
	if strings.Contains(doc, "Two things only") {
		t.Errorf("State keeps %d fields and its doc comment still says two:\n%s", len(fields), doc)
	}
}

// stateDocAndFields returns State's doc comment and the json name of every
// field it writes to disk, read out of the source rather than by reflection
// because the comment is only in the source.
func stateDocAndFields(t *testing.T) (string, []string) {
	t.Helper()
	doc, st := stateStruct(t)
	var names []string
	for _, field := range st.Fields.List {
		if field.Tag == nil {
			continue // path, which is not persisted
		}
		_, tag, _ := strings.Cut(field.Tag.Value, `json:"`)
		key, _, _ := strings.Cut(tag, `"`)
		if name, _, _ := strings.Cut(key, ","); name != "" {
			names = append(names, name)
		}
	}
	if doc == "" || len(names) == 0 {
		t.Fatal("state.go has no documented State struct, so this test proves nothing")
	}
	return doc, names
}

// stateStruct finds the State declaration in this package's own source.
func stateStruct(t *testing.T) (string, *ast.StructType) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "state.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("cannot read state.go: %v", err)
	}
	for _, decl := range file.Decls {
		gen, isType := decl.(*ast.GenDecl)
		if !isType || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, isSpec := spec.(*ast.TypeSpec)
			if !isSpec || ts.Name.Name != "State" {
				continue
			}
			if st, isStruct := ts.Type.(*ast.StructType); isStruct {
				return gen.Doc.Text(), st
			}
		}
	}
	t.Fatal("state.go declares no State struct, so this test proves nothing")
	return "", nil
}
