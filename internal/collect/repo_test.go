package collect

import (
	"net/http"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

func TestRepoCoreSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world", "repo.json")
	f.file("/repos/octocat/hello-world/languages", "repo_languages.json")
	f.file("/repos/octocat/hello-world/community/profile", "repo_community.json")
	f.file("/repos/octocat/hello-world/topics", "repo_topics.json")
	f.file("/repos/octocat/hello-world/releases", "releases.json")

	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	// Languages, topics and rulesets are RepoDetail's now, in one batched
	// GraphQL query rather than a REST call each.
	wantMeasurements(t, points, "gh_repo", "gh_repo_community", "gh_release", "gh_release_asset", "gh_security_setting")

	checkRepoRow(t, points)
	checkSecuritySettings(t, points)

	community := only(t, points, "gh_repo_community")[0]
	if fieldInt(t, community, "health_percentage") != 100 {
		t.Errorf("health = %v", community.Fields["health_percentage"])
	}
	// The community page, built from the repository's own html_url. It is
	// asserted whole because half of it is a suffix this collector appends:
	// see TestRepoCoreWritesNoCommunityLinkWithoutAnHTMLURL for what the
	// other half being absent has to do.
	if community.Fields["url"] != "https://github.com/octocat/hello-world/community" {
		t.Errorf("community url = %v", community.Fields["url"])
	}
	if community.Fields["has_readme"] != true || community.Fields["has_issue_template"] != false {
		t.Errorf("community files = %v", community.Fields)
	}

	checkReleaseDownloads(t, points)
	checkReleaseAssetProvenance(t, points)
}

// checkRepoRow reads the repository snapshot: its tags, its counts, and its
// ages measured against the sweep rather than stored as timestamps.
func checkRepoRow(t *testing.T, points []sink.Point) {
	t.Helper()
	repo := only(t, points, "gh_repo")[0]
	wantTags := map[string]string{
		"language": "Go", "license": "mit", "default_branch": "main",
		"visibility": "public", "fork": "false", "archived": "false",
	}
	for k, v := range wantTags {
		if repo.Tags[k] != v {
			t.Errorf("gh_repo tag %s = %q, want %q", k, repo.Tags[k], v)
		}
	}
	if fieldInt(t, repo, "stars") != 80 || fieldInt(t, repo, "forks") != 9 || fieldInt(t, repo, "watchers") != 42 {
		t.Errorf("gh_repo counts = %v", repo.Fields)
	}
	// Ages, not timestamps: pushed 2026-09-01T09:14 against a sweep on the 8th.
	if fieldInt(t, repo, "days_since_push") != 7 {
		t.Errorf("days_since_push = %v", repo.Fields["days_since_push"])
	}
	if fieldInt(t, repo, "age_days") < 5700 {
		t.Errorf("age_days = %v, a repository from 2011 is older than that", repo.Fields["age_days"])
	}
	if !repo.Time.Equal(testNow) {
		t.Errorf("a snapshot is stamped now, got %s", repo.Time)
	}
	// updated_at is the configuration clock and is two days behind
	// pushed_at in this fixture, exactly as it is on the live repository
	// this was measured against.
	if fieldInt(t, repo, "days_since_config_change") != 2 {
		t.Errorf("days_since_config_change = %v, want the distance to updated_at", repo.Fields["days_since_config_change"])
	}
	for name, want := range map[string]any{
		"is_template": false, "has_pages": true,
		"web_commit_signoff_required": false, "allow_update_branch": true,
		"pull_request_creation_policy": "all",
	} {
		if repo.Fields[name] != want {
			t.Errorf("gh_repo field %s = %v, want %v", name, repo.Fields[name], want)
		}
	}
}

// checkReleaseDownloads reads the per-asset downloads, and a release total
// that is their sum.
func checkReleaseDownloads(t *testing.T, points []sink.Point) {
	t.Helper()
	assets := only(t, points, "gh_release_asset")
	if len(assets) != 2 {
		t.Fatalf("got %d asset points, want 2", len(assets))
	}
	linux := find(t, points, "gh_release_asset", map[string]string{"tag": "v1.2.0", "asset": "ghchronicle_1.2.0_linux_amd64.tar.gz"})
	if fieldInt(t, linux, "downloads") != 12 || fieldInt(t, linux, "size_bytes") != 4812331 {
		t.Errorf("linux asset = %v", linux.Fields)
	}
	// Inventory, so a day's sweeps converge on one row per asset rather than
	// writing a fresh copy every hour.
	if !linux.Time.Equal(startOfDay(testNow)) {
		t.Errorf("asset stamped %s, want the start of the day %s", linux.Time, startOfDay(testNow))
	}
	rel := find(t, points, "gh_release", map[string]string{"tag": "v1.2.0"})
	if fieldInt(t, rel, "downloads") != 16 || fieldInt(t, rel, "assets") != 2 || rel.Tags["prerelease"] != "false" {
		t.Errorf("release = %v %v", rel.Tags, rel.Fields)
	}
	if !rel.Time.Equal(testNow) {
		t.Errorf("release stamped %s, want the sweep's own time %s", rel.Time, testNow)
	}
	rc := find(t, points, "gh_release", map[string]string{"tag": "v1.3.0-rc1"})
	if rc.Tags["prerelease"] != "true" || fieldInt(t, rc, "assets") != 0 {
		t.Errorf("prerelease = %v %v", rc.Tags, rc.Fields)
	}
}

// checkSecuritySettings reads the block GET /repos/{owner}/{repo} sends and
// GET /user/repos does not.
func checkSecuritySettings(t *testing.T, points []sink.Point) {
	t.Helper()
	settings := only(t, points, "gh_security_setting")
	if len(settings) != 5 {
		t.Fatalf("got %d security settings, want the five keys GitHub sends", len(settings))
	}
	on := find(t, points, "gh_security_setting", map[string]string{"setting": "secret_scanning_push_protection"})
	if on.Tags["status"] != "enabled" || on.Fields["enabled"] != true {
		t.Errorf("push protection = %v %v", on.Tags, on.Fields)
	}
	if !on.Time.Equal(testNow) {
		t.Errorf("a security setting is current state and is stamped now, got %s", on.Time)
	}
	off := find(t, points, "gh_security_setting", map[string]string{"setting": "secret_scanning_validity_checks"})
	if off.Tags["status"] != "disabled" || off.Fields["enabled"] != false {
		t.Errorf("validity checks = %v %v", off.Tags, off.Fields)
	}
}

// checkReleaseAssetProvenance reads the half of each asset that says what the
// file is and who put it there.
func checkReleaseAssetProvenance(t *testing.T, points []sink.Point) {
	t.Helper()
	linux := find(t, points, "gh_release_asset", map[string]string{"asset": "ghchronicle_1.2.0_linux_amd64.tar.gz"})
	if linux.Fields["digest"] != "sha256:6d974306d539ea933a542913f5736ab490d43c46029665b9e83d5290e566f3b6" {
		t.Errorf("digest = %v", linux.Fields["digest"])
	}
	if linux.Fields["content_type"] != "application/gzip" || linux.Fields["uploader"] != "github-actions[bot]" {
		t.Errorf("asset provenance = %v", linux.Fields)
	}
	// The asset has its own clock: uploaded on 10 August against a sweep on
	// 8 September, which is not the age of the release it hangs off.
	if fieldInt(t, linux, "age_days") != 29 {
		t.Errorf("age_days = %v", linux.Fields["age_days"])
	}
	// Ten of the 1,260 assets on the account this was measured against
	// predate the digest and send an explicit null. The field is written
	// even so, empty, so the measurement keeps one field set.
	old := find(t, points, "gh_release_asset", map[string]string{"asset": "ghchronicle_1.2.0_darwin_arm64.tar.gz"})
	if !hasField(old, "digest") || old.Fields["digest"] != "" {
		t.Errorf("an asset with no digest = %v", old.Fields)
	}
	if old.Fields["uploader"] != "octocat" {
		t.Errorf("uploader = %v", old.Fields["uploader"])
	}
}

// TestRepoCoreFallsBackRatherThanEmittingAnEmptyTag pins that no tag of
// gh_repo can go out empty. InfluxDB's line protocol drops a tag whose value
// is empty, which would put those repositories in a second series of gh_repo
// that carries no language and no license tag at all.
func TestRepoCoreFallsBackRatherThanEmittingAnEmptyTag(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world", func(w http.ResponseWriter, _ *http.Request) {
		// A repository with no detected language and no license, which is
		// 18 and 9 of the 52 on the account this was measured against.
		_, _ = w.Write([]byte(`{"id":7,"full_name":"octocat/hello-world","default_branch":"main",` +
			`"language":null,"license":null,"created_at":"2026-01-01T00:00:00Z",` +
			`"updated_at":"2026-09-06T09:01:44Z","pushed_at":"2026-09-01T00:00:00Z"}`))
	})
	bare, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, bare)
	repo := only(t, bare, "gh_repo")[0]
	if repo.Tags["language"] != noneTag || repo.Tags["license"] != noneTag {
		t.Errorf("a repository with neither = %v", repo.Tags)
	}
	// Every date this measurement reports is a distance from now, so a body
	// that lost one would report the distance to the zero time rather than
	// nothing at all.
	if got := fieldInt(t, repo, "days_since_config_change"); got != 2 {
		t.Errorf("days_since_config_change = %v, want the distance to updated_at", got)
	}
}

// TestRepoCoreWritesNoCommunityLinkWithoutAnHTMLURL pins what a missing
// html_url does to the one field that exists to be clicked.
//
// gh_repo_community's url is html_url with /community appended, and appending
// to nothing used to produce "/community": a value no Link column can render
// as a url, whose data link is a relative path and resolves against the
// Grafana host rather than against GitHub. GitHub always sends html_url, so
// this is the guard rather than the case, and the guard is that the field is
// left off the point entirely.
func TestRepoCoreWritesNoCommunityLinkWithoutAnHTMLURL(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/repos/octocat/hello-world", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":7,"full_name":"octocat/hello-world","default_branch":"main",` +
			`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-09-06T09:01:44Z",` +
			`"pushed_at":"2026-09-01T00:00:00Z"}`))
	})
	f.file("/repos/octocat/hello-world/community/profile", "repo_community.json")

	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	community := only(t, points, "gh_repo_community")[0]
	if hasField(community, "url") {
		t.Errorf("url = %v, want no url field at all: a suffix on its own is a "+
			"relative path, not a link to GitHub", community.Fields["url"])
	}
	// The rest of the row still arrives, so the missing link costs the
	// measurement nothing else.
	if fieldInt(t, community, "health_percentage") != 100 {
		t.Errorf("health = %v", community.Fields["health_percentage"])
	}
}

// TestRepoCorePrivateRepositoryHasNoSecurityBlock pins the third state. GitHub
// omits security_and_analysis entirely on a private repository, which means
// the feature does not exist there. Reading that as `disabled` would repeat
// the confusion gh_security_feature already causes.
func TestRepoCorePrivateRepositoryHasNoSecurityBlock(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world", "repo_private.json")

	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	settings := only(t, points, "gh_security_setting")
	if len(settings) != 5 {
		t.Fatalf("got %d security settings, want all five even where the block is absent", len(settings))
	}
	for _, p := range settings {
		if p.Tags["status"] != "unavailable" || p.Fields["enabled"] != false {
			t.Errorf("%s = %v %v, want unavailable", p.Tags["setting"], p.Tags, p.Fields)
		}
	}
	repo := only(t, points, "gh_repo")[0]
	if repo.Tags["visibility"] != "private" {
		t.Errorf("visibility = %q", repo.Tags["visibility"])
	}
}

// TestUploaderOfHandlesANullObject covers the guard rather than faking a null
// uploader in a fixture: GitHub documents the field as nullable and every one
// of the 1,260 assets measured on this account carries an object.
func TestUploaderOfHandlesANullObject(t *testing.T) {
	t.Parallel()
	if got := uploaderOf(nil); got != "" {
		t.Errorf("uploaderOf(nil) = %q", got)
	}
	if got := uploaderOf(&releaseUploader{Login: "octocat"}); got != "octocat" {
		t.Errorf("uploaderOf = %q", got)
	}
}

func TestRepoCoreMissingRepositoryIsNotAnError(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// No route: the repository answers 404, which is "nothing here".
	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("404 must be skipped: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points for a missing repository", len(points))
	}
}

func TestRepoCoreSurvivesOptionalSurfacesBeingOff(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world", "repo.json")
	// Languages, community profile, topics and releases all 404: the
	// snapshot point must still be written.
	points, err := RepoCore{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	// The security block rides in the repository body, so it survives every
	// other surface being switched off.
	wantMeasurements(t, points, "gh_repo", "gh_security_setting")
	for _, m := range measurements(points) {
		if m != "gh_repo" && m != "gh_security_setting" {
			t.Errorf("got %s, want only the two that come from the repository body", m)
		}
	}
}
