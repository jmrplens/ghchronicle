package collect

import (
	"context"
	"fmt"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// RepoCore collects what a repository is right now, plus the things that
// accumulate on GitHub's side and therefore need no backfill: community
// health and per-asset release downloads.
//
// Languages, topics and rulesets used to be here as well, one REST call each.
// They are in RepoDetail now, which asks for ten repositories at a time in a
// single GraphQL point. What is left is REST because it is only in REST:
// `network_count`, the size of the whole fork tree, and the community health
// percentage, neither of which GraphQL exposes.
//
// Release download counts are the one metric here that only one competing tool
// exposes, and none of them break it down per asset, which is what tells a
// Linux build from a macOS one.
type RepoCore struct {
	// Walk bounds the release list. Default one page; a backfill walks all.
	Walk Walk
}

type releaseRow struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []struct {
		Name          string    `json:"name"`
		DownloadCount int       `json:"download_count"`
		Size          int       `json:"size"`
		DownloadURL   string    `json:"browser_download_url"`
		ContentType   string    `json:"content_type"`
		CreatedAt     time.Time `json:"created_at"`
		// The digest GitHub computed when the file was uploaded. It is what
		// turns "two builds of the same tag" into an answerable question,
		// and 10 of the 1,260 assets on this account predate it and carry
		// an empty string.
		Digest   string           `json:"digest"`
		Uploader *releaseUploader `json:"uploader"`
	} `json:"assets"`
}

// securitySettings are the five keys GitHub sends inside
// security_and_analysis. The list is fixed rather than read off the response
// so every repository writes the same five series: a name emitted only where
// the feature happens to exist would give the measurement a different shape
// per repository, and a missing series reads as "no data" rather than "off".
var securitySettings = []string{
	"secret_scanning",
	"secret_scanning_push_protection",
	"dependabot_security_updates",
	"secret_scanning_non_provider_patterns",
	"secret_scanning_validity_checks",
}

func (rc RepoCore) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point

	var r struct {
		ID         int64     `json:"id"`
		HTMLURL    string    `json:"html_url"`
		Stars      int       `json:"stargazers_count"`
		Forks      int       `json:"forks_count"`
		Watchers   int       `json:"subscribers_count"`
		OpenIssues int       `json:"open_issues_count"`
		Size       int       `json:"size"`
		Network    int       `json:"network_count"`
		Language   string    `json:"language"`
		Archived   bool      `json:"archived"`
		Disabled   bool      `json:"disabled"`
		Private    bool      `json:"private"`
		Fork       bool      `json:"fork"`
		Created    time.Time `json:"created_at"`
		Pushed     time.Time `json:"pushed_at"`
		// updated_at moves when the configuration changes, which pushed_at
		// does not: measured on jmrplens/jmrp.io today they are four days
		// apart. It is the only clock GitHub gives for "when was this
		// repository last reconfigured".
		Updated   time.Time `json:"updated_at"`
		DefaultBr string    `json:"default_branch"`
		License   *struct {
			Key string `json:"key"`
		} `json:"license"`
		IsTemplate        bool   `json:"is_template"`
		HasPages          bool   `json:"has_pages"`
		SignoffRequired   bool   `json:"web_commit_signoff_required"`
		AllowUpdateBranch bool   `json:"allow_update_branch"`
		PRCreationPolicy  string `json:"pull_request_creation_policy"`
		// security_and_analysis arrives only in the single-repository body.
		// GET /user/repos omits it on all 52 repositories of this account,
		// so replacing this call with the listing would lose the block.
		Security map[string]*struct {
			Status string `json:"status"`
		} `json:"security_and_analysis"`
	}
	if _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName, &r, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	// Both fall back rather than going out empty. Measured on the 52
	// repositories of this account today, language is null on 18 and license
	// on 9, and a third of the account was landing in a second series of
	// gh_repo carrying no language and no license tag at all, rather than one
	// that says there is none. The measurement is here; the rule and the
	// spelling are noneTag's.
	language := orNone(r.Language)
	license := ""
	if r.License != nil {
		license = r.License.Key
	}
	license = orNone(license)
	points = append(points, sink.Point{
		Measurement: "gh_repo",
		Tags: merge(base, map[string]string{
			"language": language, "license": license, "default_branch": r.DefaultBr,
			"visibility": visibility(r.Private), "fork": boolTag(r.Fork), "archived": boolTag(r.Archived),
		}),
		Fields: map[string]any{
			"stars": r.Stars, "forks": r.Forks, "watchers": r.Watchers,
			"open_issues": r.OpenIssues, "size_kb": r.Size, "network": r.Network,
			// Ages rather than raw timestamps: a dashboard asks "how long since
			// the last push", and a stored epoch would need arithmetic in every
			// query to answer it.
			"age_days":        int(now.Sub(r.Created).Hours() / 24),
			"days_since_push": int(now.Sub(r.Pushed).Hours() / 24),
			// The numeric id, which is the only thing about a repository that
			// never changes. GitHub publishes no rename or transfer history,
			// so "same id, different full_name" is the only way to tell a
			// renamed repository from a new one and a dead one.
			"repo_id": r.ID, "url": r.HTMLURL,
			"days_since_config_change":    int(now.Sub(r.Updated).Hours() / 24),
			"is_template":                 r.IsTemplate,
			"has_pages":                   r.HasPages,
			"web_commit_signoff_required": r.SignoffRequired,
			"allow_update_branch":         r.AllowUpdateBranch,
			// A string rather than a tag: GitHub answers "all" on all 52
			// repositories here, and a policy that never varies would only
			// add a level to every Graphite path.
			"pull_request_creation_policy": r.PRCreationPolicy,
		},
		Time: now,
	})

	// The three states of a security setting, which is the whole point of
	// collecting it. GitHub sends the block with a status per feature on a
	// public repository, and omits it entirely on a private one: 4 of 4
	// private repositories checked today have no such key at all, which is
	// "the feature does not exist here" and not "switched off". Writing
	// those as `disabled` would repeat the confusion gh_security_feature
	// already causes, where an empty alert list means both "clean" and
	// "never scanned".
	for _, name := range securitySettings {
		status := "unavailable"
		if s := r.Security[name]; s != nil && s.Status != "" {
			status = s.Status
		}
		points = append(points, sink.Point{
			Measurement: "gh_security_setting",
			Tags:        merge(base, map[string]string{"setting": name, "status": status}),
			Fields:      map[string]any{"enabled": status == "enabled"},
			// Current state, like gh_repo_policy: the day a protection was
			// switched off is read by comparing consecutive sweeps.
			Time: now,
		})
	}

	// Community health: the percentage plus which files exist.
	var community struct {
		Health int            `json:"health_percentage"`
		Files  map[string]any `json:"files"`
	}
	if _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/community/profile", &community, ""); err == nil {
		f := map[string]any{"health_percentage": community.Health}
		setNonEmpty(f, "url", pageURL(r.HTMLURL, "community"))
		for _, key := range []string{"code_of_conduct", "contributing", "license", "readme", "issue_template", "pull_request_template"} {
			f["has_"+key] = community.Files[key] != nil
		}
		points = append(points, sink.Point{
			Measurement: "gh_repo_community", Tags: base, Fields: f, Time: now,
		})
	} else if !isSkippable(err) {
		return nil, err
	}

	rel, err := releasePoints(ctx, c, repo, base, now, rc.Walk)
	if err != nil {
		return nil, err
	}
	return append(points, rel...), nil
}

func releasePoints(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, now time.Time, w Walk) ([]sink.Point, error) {
	var releases []releaseRow
	// One page is the increment a sweep needs; a backfill walks them all. A
	// repository with more than a hundred releases is not rare.
	most := w.limit(1)
	for page := 1; page <= most; page++ {
		var batch []releaseRow
		path := fmt.Sprintf("/repos/%s/releases?per_page=100&page=%d", repo.FullName, page)
		if _, _, err := c.GetJSON(ctx, path, &batch, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				break
			}
			return nil, err
		}
		releases = append(releases, batch...)
		if len(batch) < 100 || w.past(batch[len(batch)-1].PublishedAt) {
			break
		}
	}
	// The assets are inventory, anchored to the start of the UTC day like the
	// cache entries: a download count changes only when someone downloads,
	// and stamped at the sweep every asset was a fresh row every hour, 1,233
	// rows an hour on this account and 15 per cent of the whole database
	// after eleven hours. One row per asset per day still answers "downloads
	// per day", by the difference between two days, and the newest row is
	// still the value. The release itself stays stamped at the sweep: it is
	// one row per release, and the Loki line it renders as is the one dated
	// event a sweep is sure to produce.
	day := now.UTC().Truncate(24 * time.Hour)
	var points []sink.Point
	for _, rel := range releases {
		total := 0
		for _, a := range rel.Assets {
			total += a.DownloadCount
			// Per asset, because "27 downloads" hides that 12 were the x86_64
			// Linux build and 4 the macOS one.
			points = append(points, sink.Point{
				Measurement: "gh_release_asset",
				Tags:        merge(base, map[string]string{"tag": rel.TagName, "asset": a.Name}),
				Fields: map[string]any{
					"downloads": a.DownloadCount, "size_bytes": a.Size, "url": a.DownloadURL,
					"digest": a.Digest, "content_type": a.ContentType,
					// Who published the file, which separates a release cut
					// by CI from one uploaded by hand.
					"uploader": uploaderOf(a.Uploader),
					// The asset has its own clock: a file added to an old
					// release is younger than the release.
					"age_days": int(now.Sub(a.CreatedAt).Hours() / 24),
				},
				Time: day,
			})
		}
		points = append(points, sink.Point{
			Measurement: "gh_release",
			Tags: merge(base, map[string]string{
				"tag": rel.TagName, "prerelease": boolTag(rel.Prerelease), "draft": boolTag(rel.Draft),
			}),
			Fields: map[string]any{
				"downloads": total, "assets": len(rel.Assets),
				"age_days": int(now.Sub(rel.PublishedAt).Hours() / 24),
				"url":      rel.HTMLURL,
			},
			Time: now,
		})
	}
	return points, nil
}

// releaseUploader is whoever published one release asset. Named rather than
// anonymous so uploaderOf can take it.
type releaseUploader struct {
	Login string `json:"login"`
}

// uploaderOf reads the login of whoever uploaded a release asset. The field
// is a nullable object, and an empty string keeps the field set of
// gh_release_asset the same on every point.
func uploaderOf(u *releaseUploader) string {
	if u == nil {
		return ""
	}
	return u.Login
}

func visibility(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

func boolTag(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
