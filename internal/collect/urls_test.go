package collect

import "testing"

// The url builders, at the level where the rule is one line: a part that is
// missing makes the whole url missing, rather than a shorter url that
// addresses something else or a path that addresses nothing.
func TestAURLIsNothingUntilEveryPartOfItIsThere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{
			"a page below one GitHub gave",
			pageURL("https://github.com/octocat/hello-world", "community"),
			"https://github.com/octocat/hello-world/community",
		},
		{
			"the base GitHub did not give",
			pageURL("", "community"), "",
		},
		{
			"a path in pieces",
			pageURL("https://github.com/octocat/hello-world", "settings", "branches"),
			"https://github.com/octocat/hello-world/settings/branches",
		},
		{
			"a piece of the path missing",
			pageURL("https://github.com/octocat/hello-world", "settings", ""), "",
		},
		{
			"no path at all, which would leave a trailing slash",
			pageURL("https://github.com/octocat/hello-world"), "",
		},
		{
			"a page on github.com",
			githubPage("octocat/hello-world", "security", "dependabot"),
			"https://github.com/octocat/hello-world/security/dependabot",
		},
		// The one that would be silently wrong rather than empty: without the
		// owner this addresses https://github.com/security, a real page about
		// something else entirely.
		{
			"a page on github.com with no repository",
			githubPage("", "security"), "",
		},
		{
			"an absolute path GitHub named itself",
			githubRootedPage("/octocat/hello-world/blob/main/README.md"),
			"https://github.com/octocat/hello-world/blob/main/README.md",
		},
		{
			"a path that is not one",
			githubRootedPage("octocat/hello-world"), "",
		},
		{
			"no path",
			githubRootedPage(""), "",
		},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// withURL is the other half of the rule: nothing to link to means no field,
// not a field holding nothing. An empty string would reach the stores as a
// column of empty cells, which reads as "this item has no page" only by
// accident.
func TestWithURLLeavesTheFieldOffRatherThanEmpty(t *testing.T) {
	t.Parallel()
	if f := withURL(map[string]any{"stars": 1}, ""); f["url"] != nil {
		t.Errorf("fields = %v, want no url key", f)
	}
	f := withURL(map[string]any{"stars": 1}, "https://github.com/octocat")
	if f["url"] != "https://github.com/octocat" {
		t.Errorf("fields = %v", f)
	}
}
