package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func stargazerRoutes(f *fixtureServer, lastPage int) {
	f.handle("/repos/octocat/hello-world/stargazers", func(w http.ResponseWriter, r *http.Request) {
		if lastPage > 1 {
			w.Header().Set("Link", `<https://api.github.com/repositories/1296269/stargazers?per_page=100&page=2>; rel="next", `+
				`<https://api.github.com/repositories/1296269/stargazers?per_page=100&page=2>; rel="last"`)
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			f.write(w, "stargazers_page1.json")
		case "2":
			f.write(w, "stargazers_page2.json")
		default:
			_, _ = w.Write([]byte("[]"))
		}
	})
}

func TestStargazersFullWalkFollowsTheLinkHeader(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	stargazerRoutes(f, 2)

	points, err := Stargazers{Full: true}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 4 {
		t.Fatalf("got %d stars, want 4 across two pages", len(points))
	}
	calls := f.calls("/repos/octocat/hello-world/stargazers")
	if len(calls) != 2 {
		t.Fatalf("made %d requests, want 2 (the Link header says page 2 is last)", len(calls))
	}
	for _, c := range calls {
		if got := c.Header.Get("Accept"); got != starAccept {
			t.Errorf("Accept = %q, the star media type is what carries starred_at", got)
		}
	}
	// Every star at the instant it was given.
	alice := find(t, points, "gh_star", map[string]string{"user": "alice"})
	if want := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC); !alice.Time.Equal(want) {
		t.Errorf("alice's star stamped %s, want %s", alice.Time, want)
	}
	dave := find(t, points, "gh_star", map[string]string{"user": "dave"})
	if want := time.Date(2026, 8, 30, 22, 10, 0, 0, time.UTC); !dave.Time.Equal(want) {
		t.Errorf("dave's star stamped %s, want %s", dave.Time, want)
	}
}

func TestStargazersIncrementReadsOnlyTheLastPage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	stargazerRoutes(f, 2)

	points, err := Stargazers{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/repos/octocat/hello-world/stargazers")
	if len(calls) != 2 || calls[1].Query["page"] != "2" {
		t.Errorf("an incremental sweep reads the first page and the last, got %d calls", len(calls))
	}
	if len(points) != 4 {
		t.Errorf("got %d points", len(points))
	}
}

func TestStargazersSinglePageMakesOneCall(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	stargazerRoutes(f, 1)
	points, err := Stargazers{Full: true}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/stargazers")); n != 1 {
		t.Errorf("no Link header means one page, got %d calls", n)
	}
	if len(points) != 2 {
		t.Errorf("got %d points, want 2", len(points))
	}
}

func TestStargazersUnavailableIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := Stargazers{Full: true}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("404: err=%v points=%d, want nil and none", err, len(points))
	}
}

// The last page is where new stars arrive, and it is read with a second
// request. A spent budget there has to be reported: answering with the first
// page alone and no error would publish a star count that quietly stopped
// growing.
func TestStargazersRateLimitOnTheLastPageIsReported(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/stargazers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/repositories/1296269/stargazers?per_page=100&page=2>; rel="last"`)
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", strconv.FormatInt(testNow.Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		f.write(w, "stargazers_page1.json")
	})
	if _, err := (Stargazers{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
		t.Fatal("a spent budget on the last page must not read as no new stars")
	}
}

// `url` is the page of the thing the row is about, and the row is about a star
// on a repository. The stargazer keeps a link of their own under user_url,
// which is where this value used to live.
func TestStargazerLinks(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	stargazerRoutes(f, 1)
	points, err := Stargazers{Full: true}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	alice := find(t, points, "gh_star", map[string]string{"user": "alice"})
	if alice.Fields["url"] != "https://github.com/octocat/hello-world/stargazers" {
		t.Errorf("url = %v, want the repository's stargazer list", alice.Fields["url"])
	}
	if alice.Fields["user_url"] != "https://github.com/alice" {
		t.Errorf("user_url = %v, want the stargazer's own page", alice.Fields["user_url"])
	}
	for _, p := range points {
		if !hasField(p, "url") || !hasField(p, "user_url") {
			t.Errorf("a star row lost a link: %v", p.Fields)
		}
	}
}

// TestTheStargazerCommentSendsTheReaderToTheBatch holds this file's own
// documentation to the shape the collector has had since the sweep stopped
// coming here.
//
// The comment described the mechanism of before 639ef16: "After the first
// sweep only the last page is read". An ordinary sweep no longer reads any
// page of this list for a repository it has seen; the newest hundred arrive
// in the GraphQL batch. Four documents were written from this comment, so the
// comment is what has to name where the work went.
func TestTheStargazerCommentSendsTheReaderToTheBatch(t *testing.T) {
	t.Parallel()
	doc := docComment(t, "stars.go", "Stargazers")
	if strings.Contains(doc, "After the first sweep only the last page is read") {
		t.Errorf("the Stargazers comment still describes the walk of before 639ef16:\n%s", doc)
	}
	if !strings.Contains(doc, "audience.go") {
		t.Errorf("the Stargazers comment does not say where an ordinary sweep reads the newest stars instead:\n%s", doc)
	}
}

// docComment returns the doc comment of one type in one file of this package.
func docComment(t *testing.T, path, name string) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		gen, isType := decl.(*ast.GenDecl)
		if !isType || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			if ts, isSpec := spec.(*ast.TypeSpec); isSpec && ts.Name.Name == name {
				return gen.Doc.Text()
			}
		}
	}
	t.Fatalf("%s has no type %s, so this test proves nothing", path, name)
	return ""
}
