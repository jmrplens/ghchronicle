package collect

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

func TestProfile(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/user/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("package_type") == "container" {
			f.write(w, "packages_container.json")
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	f.file("/user/packages/container/ghchronicle", "package_element.json")
	f.file("/user/packages/container/ghchronicle/versions", "package_versions.json")
	f.file("/gists", "gists.json")
	f.file("/users/octocat/social_accounts", "social_accounts.json")
	// The profile document, blog null: octocat names no homepage.
	f.file("/users/octocat", "user_profile.json")

	points, err := Profile{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_package", "gh_package_version", "gh_gist", "gh_social_account")

	// Every package type is asked for; GraphQL cannot be trusted to count them.
	if n := len(f.calls("/user/packages")); n != 6 {
		t.Errorf("asked for %d package types, want 6", n)
	}
	checkContainerPackage(t, points)

	gist := only(t, points, "gh_gist")[0]
	if gist.Tags["gist"] != "aa5a315d61ae9438b18d" || gist.Tags["public"] != "true" {
		t.Errorf("gist tags = %v", gist.Tags)
	}
	if fieldInt(t, gist, "files") != 2 || fieldInt(t, gist, "size_bytes") != 206 || fieldInt(t, gist, "comments") != 2 {
		t.Errorf("gist fields = %v", gist.Fields)
	}
	if !gist.Time.Equal(testNow) {
		t.Errorf("a gist is inventory and is stamped now, got %s", gist.Time)
	}

	social := only(t, points, "gh_social_account")
	if len(social) != 2 {
		t.Fatalf("got %d social accounts", len(social))
	}
	m := find(t, points, "gh_social_account", map[string]string{"provider": "mastodon"})
	if m.Fields["url"] != "https://mastodon.social/@octocat" {
		t.Errorf("social url = %v", m.Fields["url"])
	}
}

// TestProfileListsTheHomepageAsASocialAccount pins the row the social
// listing cannot supply. Both fixtures were recorded from the live API on
// 2026-09-12: /users/jmrplens/social_accounts answers three accounts and
// /users/jmrplens answers blog https://jmrp.io/, which the profile page shows
// above them as the website.
func TestProfileListsTheHomepageAsASocialAccount(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/users/jmrplens/social_accounts", "social_accounts_jmrplens.json")
	f.file("/users/jmrplens", "user_profile_website.json")
	f.handle("/user/packages", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	f.handle("/gists", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })

	points, err := Profile{Login: "jmrplens"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if got := len(only(t, points, "gh_social_account")); got != 4 {
		t.Fatalf("got %d social accounts, want mastodon, linkedin, bluesky and the website", got)
	}
	site := find(t, points, "gh_social_account", map[string]string{"user": "jmrplens", "provider": "website"})
	if site.Fields["url"] != "https://jmrp.io/" || site.Fields["present"] != 1 {
		t.Errorf("website row = %v", site.Fields)
	}
	if !site.Time.Equal(testNow) {
		t.Errorf("the website is a current state and is stamped now, got %s", site.Time)
	}
}

// The blog field is free text. A bare host gets a scheme, as the profile
// page gives it one when it links it, and anything that is still not an
// absolute url is no row at all.
func TestWebsiteURLIsAbsoluteOrAbsent(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://jmrp.io/": "https://jmrp.io/",
		"jmrp.io":          "https://jmrp.io",
		" http://a.b ":     "http://a.b",
		"":                 "",
		"   ":              "",
		"not a url":        "",
	} {
		if got := websiteURL(in); got != want {
			t.Errorf("websiteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// checkContainerPackage reads the one container package and its tagged
// versions, the attestation manifest left out.
func checkContainerPackage(t *testing.T, points []sink.Point) {
	t.Helper()
	pkg := only(t, points, "gh_package")[0]
	if pkg.Tags["package"] != "ghchronicle" || pkg.Tags["type"] != "container" ||
		pkg.Tags["visibility"] != "public" || pkg.Tags["full_name"] != "octocat/hello-world" ||
		pkg.Tags["owner"] != "octocat" || pkg.Tags["repo"] != "hello-world" {
		t.Errorf("package tags = %v", pkg.Tags)
	}
	// The listing carries no version_count at all, so the total is the one the
	// package element declares, not the two versions the walk happened to see.
	// tagged_versions counts the two named tags, not the attestation manifest
	// that also carries one.
	if fieldInt(t, pkg, "versions") != 105 || fieldInt(t, pkg, "tagged_versions") != 2 {
		t.Errorf("package fields = %v", pkg.Fields)
	}
	if !pkg.Time.Equal(testNow) {
		t.Errorf("a package is inventory and is stamped now, got %s", pkg.Time)
	}
	versions := only(t, points, "gh_package_version")
	if len(versions) != 2 {
		t.Fatalf("got %d version points, want one per tag and none for the untagged digest", len(versions))
	}
	// The OCI referrers fallback tag is a new tag value on every build and
	// names no release: it must not become a series.
	for _, v := range versions {
		if strings.HasPrefix(v.Tags["tag"], "sha256-") {
			t.Errorf("attestation manifest kept as a version: tag %q", v.Tags["tag"])
		}
	}
	latest := find(t, points, "gh_package_version", map[string]string{"tag": "latest"})
	if want := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC); !latest.Time.Equal(want) {
		t.Errorf("version stamped %s, want created_at %s", latest.Time, want)
	}
	if latest.Fields["digest"] != "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0" {
		t.Errorf("digest = %v", latest.Fields["digest"])
	}
}

func TestProfilePackageCountFallsBackToTheWalk(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/user/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("package_type") == "container" {
			f.write(w, "packages_container.json")
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	f.file("/user/packages/container/ghchronicle/versions", "package_versions.json")
	// The element 404s, as it does for a token without the packages scope.
	points, err := Profile{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a missing package element is not a failure: %v", err)
	}
	checkPoints(t, points)
	pkg := only(t, points, "gh_package")[0]
	// Three versions were walked, one of them an attestation manifest: the
	// fallback is the number of versions seen, the same thing the element
	// would have declared, not the number of releases.
	if fieldInt(t, pkg, "versions") != 3 {
		t.Errorf("versions = %v, want the walked count when the element says nothing", pkg.Fields["versions"])
	}
}

func TestProfileWithNothing(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// Everything 404s: a token without the packages scope, no gists.
	points, err := Profile{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("404 everywhere is not a failure: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points", len(points))
	}
}
