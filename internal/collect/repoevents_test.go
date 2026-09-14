package collect

import (
	"net/http"
	"testing"
	"time"
)

func TestRepoActivityLogPagesByCursor(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	const cursor = "Y3Vyc29yOnYyOpK5MjAyNi0wOS0wNlQxMzo1MDowMFrOAA22gA=="
	f.handle("/repos/octocat/hello-world/activity", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			w.Header().Set("Link", `<https://api.github.com/repos/octocat/hello-world/activity?per_page=100&after=`+cursor+`>; rel="next"`)
			_, _ = w.Write(repeat(t, "repo_activity.json", "", 100, func(i int, row map[string]any) {
				row["id"] = 1000000 + i
				row["timestamp"] = spacedOut(i)
			}))
			return
		}
		f.write(w, "repo_activity.json")
	})

	points, err := RepoActivityLog{Walk: Walk{Pages: 5}}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/repos/octocat/hello-world/activity")
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want 2: the short second page ends the walk", len(calls))
	}
	if calls[0].Query["after"] != "" || calls[1].Query["after"] != cursor {
		t.Errorf("the second call must carry the cursor from the Link header, got %q", calls[1].Query["after"])
	}
	if len(points) != 102 {
		t.Errorf("got %d activity points, want 102", len(points))
	}
	force := find(t, points, "gh_repo_activity", map[string]string{"activity": "force_push"})
	if force.Tags["actor"] != "(ghost)" || force.Fields["ref_name"] != "feature/x" {
		t.Errorf("force push = %v %v", force.Tags, force.Fields)
	}
	// A branch name is minted per pull request and never reused, so as a tag
	// it was a series per branch ever pushed.
	if _, isTag := force.Tags["ref"]; isTag {
		t.Error("ref must be a field, not a tag")
	}
	if hasField(force, "ref") {
		t.Error("the demoted ref must not reuse the old tag's column name")
	}
	if fieldInt(t, force, "id") != 900000 || fieldInt(t, force, "events") != 1 {
		t.Errorf("force push fields = %v", force.Fields)
	}
	if want := time.Date(2026, 9, 6, 13, 50, 0, 0, time.UTC); !force.Time.Equal(want) {
		t.Errorf("activity stamped %s, want its own timestamp %s", force.Time, want)
	}
	push := find(t, points, "gh_repo_activity", map[string]string{"activity": "push", "actor": "octocat"})
	if push.Fields["ref_name"] != "main" {
		t.Errorf("push = %v %v", push.Tags, push.Fields)
	}
}

func TestRepoActivityLogDefaultsToTwoPages(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/activity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/repos/octocat/hello-world/activity?per_page=100&after=next-`+r.URL.Query().Get("after")+`>; rel="next"`)
		offset := 0
		if r.URL.Query().Get("after") != "" {
			offset = 100
		}
		_, _ = w.Write(repeat(t, "repo_activity.json", "", 100, func(i int, row map[string]any) {
			row["timestamp"] = spacedOut(offset + i)
		}))
	})
	points, err := RepoActivityLog{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/repos/octocat/hello-world/activity")); n != 2 {
		t.Errorf("made %d calls, want the default of 2", n)
	}
	if len(points) != 200 {
		t.Errorf("got %d points", len(points))
	}
}

// spacedOut is a timestamp of its own for the i-th repeated row: entries in
// one second by one actor fold into one point, which the page tests are not
// about.
func spacedOut(i int) string {
	return time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)
}

// TestRepoActivityLogFoldsOnePushIntoOneRow pins what happens to a push that
// moves several branches at once. Every store keys a point by its tags and
// its time, and the branch is a field, so three entries in one second by one
// actor would be three writes to one row, the last one winning: measured on
// this account, eight force pushes at 07:23:54 kept as one. They are one
// point instead, counting three, naming every branch, and a different actor
// or type in the same second is still its own row.
func TestRepoActivityLogFoldsOnePushIntoOneRow(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world/activity", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		  {"id": 903, "ref": "refs/heads/feat/c", "timestamp": "2026-09-12T07:23:54Z", "activity_type": "force_push", "actor": {"login": "octocat"}},
		  {"id": 902, "ref": "refs/heads/feat/b", "timestamp": "2026-09-12T07:23:54Z", "activity_type": "force_push", "actor": {"login": "octocat"}},
		  {"id": 901, "ref": "refs/heads/feat/a", "timestamp": "2026-09-12T07:23:54Z", "activity_type": "force_push", "actor": {"login": "octocat"}},
		  {"id": 900, "ref": "refs/heads/feat/a", "timestamp": "2026-09-12T07:23:54Z", "activity_type": "branch_creation", "actor": {"login": "octocat"}},
		  {"id": 899, "ref": "refs/heads/feat/z", "timestamp": "2026-09-12T07:23:54Z", "activity_type": "force_push", "actor": {"login": "dave"}}
		]`))
	})
	points, err := RepoActivityLog{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 3 {
		t.Fatalf("got %d points, want 3: one push of three branches, one branch creation, one push by somebody else", len(points))
	}
	push := find(t, points, "gh_repo_activity", map[string]string{"activity": "force_push", "actor": "octocat"})
	if fieldInt(t, push, "events") != 3 || push.Fields["ref_name"] != "feat/c,feat/b,feat/a" || fieldInt(t, push, "id") != 903 {
		t.Errorf("folded push = %v, want events 3, every branch in GitHub's order and the newest id", push.Fields)
	}
	if want := time.Date(2026, 9, 12, 7, 23, 54, 0, time.UTC); !push.Time.Equal(want) {
		t.Errorf("folded push stamped %s, want the second it happened %s", push.Time, want)
	}
	created := find(t, points, "gh_repo_activity", map[string]string{"activity": "branch_creation"})
	other := find(t, points, "gh_repo_activity", map[string]string{"actor": "dave"})
	if fieldInt(t, created, "events") != 1 || fieldInt(t, other, "events") != 1 || other.Fields["ref_name"] != "feat/z" {
		t.Errorf("the other rows must stay apart: %v %v", created.Fields, other.Fields)
	}
}

func TestRepoActivityLogUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	points, err := RepoActivityLog{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("err=%v points=%d", err, len(points))
	}
}

func TestShortRef(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"refs/heads/main":      "main",
		"refs/heads/feature/x": "feature/x",
		"refs/tags/v1.2.0":     "v1.2.0",
		"main":                 "main",
		"refs/heads/":          "refs/heads/",
		"":                     "",
	}
	for in, want := range cases {
		if got := shortRef(in); got != want {
			t.Errorf("shortRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAfterCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, link, want string
	}{
		{"next with cursor", `<https://api.github.com/repos/o/n/activity?per_page=100&after=abc>; rel="next"`, "abc"},
		{"cursor before other params", `<https://api.github.com/repos/o/n/activity?after=abc&per_page=100>; rel="next"`, "abc"},
		{"next and prev", `<https://x/activity?before=zzz>; rel="prev", <https://x/activity?after=abc>; rel="next"`, "abc"},
		{"only prev", `<https://x/activity?before=zzz>; rel="prev"`, ""},
		{"no link", "", ""},
		{"next without after", `<https://x/activity?page=2>; rel="next"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := afterCursor(tc.link); got != tc.want {
				t.Errorf("afterCursor(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

func TestSplitLinks(t *testing.T) {
	t.Parallel()
	parts := splitLinks(`<https://a/x?page=2>; rel="next", <https://a/x?page=9>; rel="last"`)
	if len(parts) != 2 || parts[0].url != "https://a/x?page=2" || parts[0].rel != "next" || parts[1].rel != "last" {
		t.Errorf("splitLinks = %+v", parts)
	}
	if got := splitLinks(""); len(got) != 0 {
		t.Errorf("empty header gave %+v", got)
	}
}
