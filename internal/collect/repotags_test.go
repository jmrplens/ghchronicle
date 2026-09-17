package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestARepositoryIsNamedByItsOwnerAndItsShortName(t *testing.T) {
	t.Parallel()
	got := repoTags("acme", "telemetry")
	want := map[string]string{"owner": "acme", "repo": "telemetry", "full_name": "acme/telemetry"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (whole set %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the tag set is %v, want exactly the three that name a repository", got)
	}
}

func TestAFullNameIsCutIntoTheSameThreeTags(t *testing.T) {
	t.Parallel()
	got := fullNameTags("acme/telemetry")
	if got["owner"] != "acme" || got["repo"] != "telemetry" || got["full_name"] != "acme/telemetry" {
		t.Errorf("tags = %v", got)
	}
}

// TestANameWithNoOwnerIsTheNameAndNotTheOwner pins the reading of the one
// string GitHub returns without a slash, a pinned gist's hash. Read the other
// way round it would put a gist id in the owner column and leave every pinned
// gist with no name at all.
func TestANameWithNoOwnerIsTheNameAndNotTheOwner(t *testing.T) {
	t.Parallel()
	got := fullNameTags("aa5a315d61ae9438b18d")
	if got["repo"] != "aa5a315d61ae9438b18d" {
		t.Errorf("repo = %q, want the name", got["repo"])
	}
	if got["owner"] != noneTag || got["full_name"] != noneTag {
		t.Errorf("tags = %v, want no owner and no full name invented", got)
	}
}

// TestANameNobodyGaveIsWrittenAsNoneAndNotLeftOut: an empty tag value is not
// written at all, so a point with no repository would land in a series of its
// own carrying no repository column for a query to name.
func TestANameNobodyGaveIsWrittenAsNoneAndNotLeftOut(t *testing.T) {
	t.Parallel()
	for _, tag := range repoTags("", "") {
		if tag != noneTag {
			t.Errorf("empty tags = %v, want the one spelling of nothing", repoTags("", ""))
			break
		}
	}
}

// TestNoCollectorSpellsTheRepositoryTagsItself is the gate the whole change
// rests on.
//
// Three shapes for naming a repository grew here one collector at a time, each
// spelled by hand where the point was built, and nothing in the build read them
// together, so the store ended with sixty-one measurements in one shape, twelve
// in another and one in a third. Any of the three looked reasonable at the
// keyboard. What made them wrong was only visible across the whole store.
//
// So the keys are spelled in exactly one place now, and this reads the
// package's own source to keep it that way: a collector that writes "owner",
// "repo" or "full_name" into a tag map of its own fails here, whichever value
// it was about to put there.
func TestNoCollectorSpellsTheRepositoryTagsItself(t *testing.T) {
	t.Parallel()
	named := map[string]bool{"owner": true, "repo": true, "full_name": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("cannot list the package: %v", err)
	}
	sort.Strings(files)
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "repotags.go" {
			continue
		}
		checked++
		checkNoHandWrittenRepoTag(t, path, named)
	}
	if checked == 0 {
		t.Fatal("no collector was read, so this test proves nothing")
	}
}

// checkNoHandWrittenRepoTag fails for each map[string]string literal in path
// that sets one of the keys repoTags owns.
//
// Narrowed to map[string]string on purpose: the same words are GraphQL
// variable names and JSON field names elsewhere in these files, and those are
// not tags.
func checkNoHandWrittenRepoTag(t *testing.T, path string, named map[string]bool) {
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
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isStringMap(lit.Type) {
			return true
		}
		for _, elt := range lit.Elts {
			kv, isPair := elt.(*ast.KeyValueExpr)
			if !isPair {
				continue
			}
			key, isLiteral := kv.Key.(*ast.BasicLit)
			if !isLiteral || key.Kind != token.STRING {
				continue
			}
			value, unquoteErr := strconv.Unquote(key.Value)
			if unquoteErr != nil || !named[value] {
				continue
			}
			pos := fset.Position(kv.Pos())
			t.Errorf("%s:%d writes the tag %q by hand. A repository is named by "+
				"repoTags or fullNameTags and by nothing else, so that `repo` holds the "+
				"short name on every measurement and a filter written against one answers "+
				"in the rest", filepath.Base(pos.Filename), pos.Line, value)
		}
		return true
	})
}

// isStringMap reports whether the composite literal's type is map[string]string,
// written out or elided inside another literal of that type.
func isStringMap(expr ast.Expr) bool {
	if expr == nil {
		// An elided type inside a map[string]map[string]string. None exists in
		// this package, and reading it as a tag map would only overreport.
		return false
	}
	m, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	return isIdent(m.Key, "string") && isIdent(m.Value, "string")
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}
