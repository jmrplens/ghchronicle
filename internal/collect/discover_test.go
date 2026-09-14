package collect

import (
	"sort"
	"strings"
	"testing"
)

func names(repos []Repo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.FullName)
	}
	sort.Strings(out)
	return out
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiscoverFilters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{
			name:   "defaults exclude forks, archived, private and other owners",
			filter: Filter{User: "octocat"},
			want:   []string{"octocat/experiment-1", "octocat/hello-world"},
		},
		{
			name:   "forks when asked",
			filter: Filter{User: "octocat", IncludeForks: true},
			want:   []string{"octocat/experiment-1", "octocat/hello-world", "octocat/linguist"},
		},
		{
			name:   "archived when asked",
			filter: Filter{User: "octocat", IncludeArchived: true},
			want:   []string{"octocat/experiment-1", "octocat/hello-world", "octocat/old-thing"},
		},
		{
			name:   "private when asked",
			filter: Filter{User: "octocat", IncludePrivate: true},
			want:   []string{"octocat/experiment-1", "octocat/hello-world", "octocat/secret-lab"},
		},
		{
			name:   "exclude accepts globs",
			filter: Filter{User: "octocat", Exclude: []string{"octocat/experiment-*"}},
			want:   []string{"octocat/hello-world"},
		},
		{
			name:   "exclude accepts exact names",
			filter: Filter{User: "octocat", Exclude: []string{"octocat/hello-world"}},
			want:   []string{"octocat/experiment-1"},
		},
		{
			name:   "an org listing is not filtered by owner",
			filter: Filter{Orgs: []string{"someorg"}},
			want:   []string{"octocat/experiment-1", "octocat/hello-world", "someorg/shared"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.file("/user/repos", "user_repos.json")
			f.file("/orgs/someorg/repos", "user_repos.json")
			found, err := Discover(ctx(t), f.Client, &tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			repos := found.Repos
			if got := names(repos); !equalNames(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			for _, r := range repos {
				if r.Owner == "" || r.Name == "" || r.FullName != r.Owner+"/"+r.Name {
					t.Errorf("malformed repo %+v", r)
				}
			}
			if tc.filter.User != "" {
				calls := f.calls("/user/repos")
				if len(calls) != 1 || calls[0].Query["affiliation"] != "owner" {
					t.Errorf("user listing calls = %d, query %v", len(calls), calls[0].Query)
				}
			}
		})
	}
}

// TestDiscoverSetsTheArchivedAside is what lets a sweep put the archive on
// record without collecting archived repositories: the listing already says
// which are archived, and the filter that drops them for the collectors keeps
// them in a second list. Only the ones dropped for being archived and nothing
// else belong there: an archived fork under the default fork rule, an
// excluded one, and every archived repository once `include_archived` is on
// (they are then collected in full, and set aside for nobody) are all out.
func TestDiscoverSetsTheArchivedAside(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{
			name:   "the archived, and not the archived fork",
			filter: Filter{User: "octocat"},
			want:   []string{"octocat/old-thing"},
		},
		{
			name:   "the archived fork once forks are wanted",
			filter: Filter{User: "octocat", IncludeForks: true},
			want:   []string{"octocat/old-fork", "octocat/old-thing"},
		},
		{
			name:   "nothing when the exclusion would have dropped it anyway",
			filter: Filter{User: "octocat", Exclude: []string{"octocat/old-*"}},
			want:   nil,
		},
		{
			name:   "nothing once archived repositories are collected",
			filter: Filter{User: "octocat", IncludeArchived: true},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.file("/user/repos", "user_repos.json")
			found, err := Discover(ctx(t), f.Client, &tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(found.Archived); !equalNames(got, tc.want) {
				t.Errorf("set aside %v, want %v", got, tc.want)
			}
			for _, r := range found.Repos {
				if r.Archived && !tc.filter.IncludeArchived {
					t.Errorf("%s is archived and was still handed to the collectors", r.FullName)
				}
			}
		})
	}

	// Named, it is collected in full, so it leaves the second list: its own
	// totals row puts the archive on record.
	f := newFixtureServer(t)
	f.file("/user/repos", "user_repos.json")
	f.file("/repos/octocat/old-thing", "repo_archived_fork.json")
	found, err := Discover(ctx(t), f.Client, &Filter{User: "octocat", Repos: []string{"octocat/old-thing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Archived) != 0 {
		t.Errorf("a named repository was set aside as well as collected: %v", names(found.Archived))
	}
	if got := names(found.Repos); !equalNames(got, []string{"octocat/experiment-1", "octocat/hello-world", "octocat/old-thing"}) {
		t.Errorf("collected %v", got)
	}
}

func TestDiscoverNamedRepositoryOverridesEveryFilter(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/user/repos", "user_repos.json")
	// An archived fork, excluded twice over by the defaults and once more
	// by the glob. Naming it wins.
	f.file("/repos/octocat/linguist", "repo_archived_fork.json")
	found, err := Discover(ctx(t), f.Client, &Filter{
		User: "octocat", Repos: []string{"octocat/linguist", "octocat/hello-world"},
		Exclude: []string{"octocat/*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(found.Repos); !equalNames(got, []string{"octocat/hello-world", "octocat/linguist"}) {
		t.Errorf("got %v", got)
	}
	for _, r := range found.Repos {
		if r.FullName == "octocat/linguist" && (!r.Fork || !r.Archived) {
			t.Errorf("named repository lost its flags: %+v", r)
		}
	}
	// A named repository that does not exist is still listed: naming it is
	// the intent, and the collectors report what they find.
	found, err = Discover(ctx(t), f.Client, &Filter{Repos: []string{"nobody/nothing"}})
	if repos := found.Repos; err != nil || len(repos) != 1 || repos[0].Owner != "nobody" || repos[0].Name != "nothing" {
		t.Errorf("repos=%v err=%v", repos, err)
	}
}

func TestDiscoverRejectsAMalformedName(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	_, err := Discover(ctx(t), f.Client, &Filter{Repos: []string{"notaslash"}})
	if err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("err = %v, want a complaint about owner/name", err)
	}
}

func TestDiscoverDeduplicatesAcrossSources(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/user/repos", "user_repos.json")
	f.file("/orgs/someorg/repos", "user_repos.json")
	found, err := Discover(ctx(t), f.Client, &Filter{User: "octocat", Orgs: []string{"someorg"}, Repos: []string{"octocat/hello-world"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, r := range found.Repos {
		seen[r.FullName]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%s listed %d times", name, n)
		}
	}
}

// TestDiscoverReadsWhetherDiscussionsAreOn is what lets the runner skip the
// discussions query on a repository whose forum is off, which on the account
// this was measured against is sixteen repositories of eighteen. The flag has
// to come from the listing discovery already pays for, and from the one-off
// read of a named repository, so no collector pays a request to learn it.
func TestDiscoverReadsWhetherDiscussionsAreOn(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/user/repos", "user_repos.json")
	f.file("/repos/octocat/linguist", "repo_archived_fork.json")
	found, err := Discover(ctx(t), f.Client, &Filter{User: "octocat", Repos: []string{"octocat/linguist"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"octocat/hello-world":  true,
		"octocat/experiment-1": false,
		"octocat/linguist":     true,
	}
	for _, r := range found.Repos {
		if on, known := want[r.FullName]; known && r.HasDiscussions != on {
			t.Errorf("%s: HasDiscussions = %v, want %v", r.FullName, r.HasDiscussions, on)
		}
	}
}

func TestExcluded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		full     string
		patterns []string
		want     bool
	}{
		{"octocat/hello-world", nil, false},
		{"octocat/hello-world", []string{"octocat/hello-world"}, true},
		{"octocat/experiment-3", []string{"octocat/experiment-*"}, true},
		{"octocat/hello-world", []string{"octocat/experiment-*"}, false},
		{"octocat/hello-world", []string{"*/hello-world"}, true},
		{"octocat/hello-world", []string{"[bad"}, false},
	}
	for _, tc := range cases {
		if got := excluded(tc.full, tc.patterns); got != tc.want {
			t.Errorf("excluded(%q, %v) = %v, want %v", tc.full, tc.patterns, got, tc.want)
		}
	}
}
