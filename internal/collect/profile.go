package collect

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Profile collects the account-wide surfaces that are not repositories:
// packages, gists, social accounts and the follower graph.
//
// Packages come from REST deliberately. GraphQL reports zero packages for this
// account while REST lists four containers in the registry, so the pretty
// query is simply wrong here.
type Profile struct {
	Login string
	Walk  Walk
}

func (p Profile) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	base := map[string]string{"user": p.Login}
	var points []sink.Point

	packages, err := p.packagePoints(ctx, c, base, now)
	points = append(points, packages...)
	if err != nil {
		return points, err
	}
	gists, err := gistPoints(ctx, c, base, now)
	points = append(points, gists...)
	if err != nil {
		return points, err
	}
	socials, err := p.socialAccountPoints(ctx, c, base, now)
	points = append(points, socials...)
	return points, err
}

// packageKinds are the registries REST lists one at a time; there is no
// listing across all of them. Shared with the account family, which counts
// the same listings for gh_account.packages.
var packageKinds = []string{"container", "npm", "maven", "rubygems", "docker", "nuget"}

// packagePoints is one point per package with its version count and
// visibility, plus a point per tagged version.
func (p Profile) packagePoints(ctx context.Context, c *ghapi.Client, base map[string]string, now time.Time) ([]sink.Point, error) {
	var points []sink.Point
	for _, kind := range packageKinds {
		var pkgs []struct {
			Name       string    `json:"name"`
			Type       string    `json:"package_type"`
			Visibility string    `json:"visibility"`
			HTMLURL    string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
			UpdatedAt  time.Time `json:"updated_at"`
			Repository *struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		path := "/user/packages?package_type=" + kind + "&per_page=100"
		if _, _, err := c.GetJSON(ctx, path, &pkgs, ""); err != nil {
			if isSkippable(err) {
				continue
			}
			return points, err
		}
		for _, pk := range pkgs {
			repo := ""
			if pk.Repository != nil {
				repo = pk.Repository.FullName
			}
			tags := merge(base, map[string]string{
				"package": pk.Name, "type": pk.Type,
				"visibility": pk.Visibility, "repo": repo,
			})
			fields := map[string]any{
				"age_days":          int(now.Sub(pk.CreatedAt).Hours() / 24),
				"days_since_update": int(now.Sub(pk.UpdatedAt).Hours() / 24),
				"url":               pk.HTMLURL,
			}
			versions, tagged, vpts, err := p.versionsOf(ctx, c, pk.Type, pk.Name, tags)
			if err != nil {
				return points, err
			}
			fields["versions"] = versions
			fields["tagged_versions"] = tagged
			points = append(points, sink.Point{
				Measurement: "gh_package", Tags: tags, Fields: fields, Time: now,
			})
			points = append(points, vpts...)
		}
	}
	return points, nil
}

// versionsOf counts a package's versions and renders the tagged ones.
//
// The version list is walked for the publication dates, which exist nowhere
// else, and the walk also counts what it saw. That count is a floor, not the
// total: it is bounded by Walk, and a package can have far more versions than
// the walk is allowed to page through, so the element's own number wins when
// there is one.
//
// version_count is absent from the listing and present on the element, which
// the old comment here had half right. Measured on this account: GET
// /user/packages?package_type=container returns no such key at all, while GET
// /user/packages/container/{name} answers 105 for a package whose version list
// declares rel="last" at page 35 with per_page=3. One request, and it is the
// only authoritative number.
func (p Profile) versionsOf(ctx context.Context, c *ghapi.Client, kind, name string,
	tags map[string]string,
) (versions, tagged int, points []sink.Point, err error) {
	versions, tagged, points, err = p.versions(ctx, c, kind, name, tags)
	if err != nil {
		return 0, 0, nil, err
	}
	declared, err := p.versionCount(ctx, c, kind, name)
	if err != nil {
		return 0, 0, nil, err
	}
	if declared > 0 {
		versions = declared
	}
	return versions, tagged, points, nil
}

// gistPoints counts the gists and what is in them.
//
// A gist is a repository GitHub hides from every repository listing, so
// nothing else counts them. Each is stamped now, not at its last edit: a gist
// is inventory, and one untouched for a year would otherwise fall outside
// every sensible dashboard range and read as "no gists".
func gistPoints(ctx context.Context, c *ghapi.Client, base map[string]string, now time.Time) ([]sink.Point, error) {
	var gists []struct {
		ID          string    `json:"id"`
		Description string    `json:"description"`
		Public      bool      `json:"public"`
		HTMLURL     string    `json:"html_url"`
		Comments    int       `json:"comments"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
		Files       map[string]struct {
			Language string `json:"language"`
			Size     int    `json:"size"`
		} `json:"files"`
	}
	if _, _, err := c.GetJSON(ctx, "/gists?per_page=100", &gists, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	var points []sink.Point
	for _, g := range gists {
		var size int
		for _, f := range g.Files {
			size += f.Size
		}
		points = append(points, sink.Point{
			Measurement: "gh_gist",
			Tags:        merge(base, map[string]string{"gist": g.ID, "public": boolTag(g.Public)}),
			Fields: map[string]any{
				"files": len(g.Files), "comments": g.Comments,
				"size_bytes": size, "description": g.Description,
				"age_days":          int(now.Sub(g.CreatedAt).Hours() / 24),
				"days_since_update": int(now.Sub(g.UpdatedAt).Hours() / 24),
				"url":               g.HTMLURL,
			},
			Time: now,
		})
	}
	return points, nil
}

// socialAccountPoints records the identity the profile advertises, so it sits
// alongside the numbers rather than being assumed.
//
// The homepage is one of them and comes from the other endpoint: the profile
// page lists the website above the social accounts, and it is the `blog` of
// GET /users/{login}, which the social_accounts listing does not repeat.
// Verified on 2026-09-12: the listing answers mastodon, linkedin and bluesky
// and the profile answers blog https://jmrp.io/. It is written under the
// provider `website`, which is not a name GitHub's enum can produce, so it
// cannot collide with an account. The ORCID iD the same page shows is in
// neither endpoint nor in GraphQL's socialAccounts, and there is no ORCID in
// SocialAccountProvider; it exists in the profile HTML only, and the
// achievements family, which reads that page once a day, writes it from
// there under the provider orcid (see Achievements.vcardSocialPoints).
func (p Profile) socialAccountPoints(ctx context.Context, c *ghapi.Client, base map[string]string, now time.Time) ([]sink.Point, error) {
	var socials []struct {
		Provider string `json:"provider"`
		URL      string `json:"url"`
	}
	if _, _, err := c.GetJSON(ctx, fmt.Sprintf("/users/%s/social_accounts", p.Login), &socials, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	var points []sink.Point
	for _, s := range socials {
		points = append(points, sink.Point{
			Measurement: "gh_social_account",
			Tags:        merge(base, map[string]string{"provider": s.Provider}),
			Fields:      map[string]any{"url": s.URL, "present": 1},
			Time:        now,
		})
	}
	website, err := p.websitePoint(ctx, c, base, now)
	return append(points, website...), err
}

// websitePoint is the homepage row, or nothing when the profile names none. The
// profile field is free text that GitHub stores as typed, so a bare host is
// given the scheme the profile page itself adds when it links it; a value
// that still is not an absolute url is dropped rather than written, since a
// url field is absolute or absent.
func (p Profile) websitePoint(ctx context.Context, c *ghapi.Client, base map[string]string, now time.Time) ([]sink.Point, error) {
	var profile struct {
		Blog string `json:"blog"`
	}
	if _, _, err := c.GetJSON(ctx, "/users/"+url.PathEscape(p.Login), &profile, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	link := websiteURL(profile.Blog)
	if link == "" {
		return nil, nil
	}
	return []sink.Point{{
		Measurement: "gh_social_account",
		Tags:        merge(base, map[string]string{"provider": "website"}),
		Fields:      map[string]any{"url": link, "present": 1},
		Time:        now,
	}}, nil
}

// websiteURL is the profile's blog field as an absolute url, or empty.
func websiteURL(blog string) string {
	blog = strings.TrimSpace(blog)
	if blog == "" {
		return ""
	}
	if !strings.HasPrefix(blog, "http://") && !strings.HasPrefix(blog, "https://") {
		blog = "https://" + blog
	}
	u, err := url.Parse(blog)
	if err != nil || u.Host == "" {
		return ""
	}
	return blog
}

// versionCount reads the version total from the package element.
//
// It is the one place GitHub serves the number for a personal account: the
// listing omits the key entirely, so a decoded zero there means "not told",
// not "no versions".
func (p Profile) versionCount(ctx context.Context, c *ghapi.Client, kind, name string) (int, error) {
	var pkg struct {
		VersionCount int `json:"version_count"`
	}
	path := fmt.Sprintf("/user/packages/%s/%s", kind, url.PathEscape(name))
	if _, _, err := c.GetJSON(ctx, path, &pkg, ""); err != nil {
		if isSkippable(err) {
			return 0, nil
		}
		return 0, err
	}
	return pkg.VersionCount, nil
}

// isReferrerTag reports whether a container tag is the OCI referrers fallback
// tag, "sha256-" followed by the sixty four hex digits of the digest it hangs
// from. GitHub publishes one for every attestation and signature manifest.
//
// It is not a release: nobody pulls it, it names the digest the row already
// carries in its digest field, and there is a new one on every build, so as a
// tag value it grows without bound. Measured on this account: of the 201
// gh_package_version rows the walk produces today, 94 are these, 69 of them on
// gitlab-mcp-server alone. They are skipped, so tagged_versions counts the
// versions a person could name.
func isReferrerTag(tag string) bool {
	const prefix = "sha256-"
	if len(tag) != len(prefix)+64 || !strings.HasPrefix(tag, prefix) {
		return false
	}
	for _, r := range tag[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// packageMetadata is the ecosystem specific half of a package version. Only
// the container tags are read: a version with no tag is one nothing points at.
type packageMetadata struct {
	Container struct {
		Tags []string `json:"tags"`
	} `json:"container"`
}

// versions walks a package's version list.
//
// Each tagged version becomes a point stamped when it was published, which is
// the only record of when a container image was actually released. Untagged
// versions are counted but not kept: a build that produced fifty untagged
// layers would otherwise dominate the chart with rows nobody can name. The
// OCI referrers fallback tag is dropped for the same reason; see
// isReferrerTag.
func (p Profile) versions(ctx context.Context, c *ghapi.Client, kind, name string,
	base map[string]string,
) (total, tagged int, points []sink.Point, err error) {
	most := p.Walk.limit(5)
	for page := 1; page <= most; page++ {
		var vs []struct {
			Name      string          `json:"name"`
			CreatedAt time.Time       `json:"created_at"`
			HTMLURL   string          `json:"html_url"`
			Metadata  packageMetadata `json:"metadata"`
		}
		path := fmt.Sprintf("/user/packages/%s/%s/versions?per_page=100&page=%d",
			kind, url.PathEscape(name), page)
		if _, _, e := c.GetJSON(ctx, path, &vs, ""); e != nil {
			if isSkippable(e) {
				return total, tagged, points, nil
			}
			return total, tagged, points, e
		}
		if len(vs) == 0 {
			break
		}
		total += len(vs)
		for _, v := range vs {
			for _, tag := range v.Metadata.Container.Tags {
				if isReferrerTag(tag) {
					continue
				}
				tagged++
				points = append(points, sink.Point{
					Measurement: "gh_package_version",
					Tags:        merge(base, map[string]string{"tag": tag}),
					Fields:      map[string]any{"digest": v.Name, "published": 1, "url": v.HTMLURL},
					Time:        v.CreatedAt,
				})
			}
		}
		if len(vs) < 100 {
			break
		}
	}
	return total, tagged, points, nil
}

// Keys collects the account's own SSH and GPG keys.
//
// The per-repository twin already exists as gh_deploy_key, with the same
// "never used" reading. The account's are the ones that sign everything: two
// SSH keys here have never been used at all, and the GPG key that signs every
// commit has an expiry date that nothing else would mention until the day the
// signatures stopped verifying.
type Keys struct {
	Login string
}

func (k Keys) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	var points []sink.Point
	day := now.UTC().Truncate(24 * time.Hour)

	var ssh []struct {
		ID        int64      `json:"id"`
		Title     string     `json:"title"`
		CreatedAt time.Time  `json:"created_at"`
		LastUsed  *time.Time `json:"last_used"`
		ReadOnly  bool       `json:"read_only"`
		Verified  bool       `json:"verified"`
	}
	if _, _, err := c.GetJSON(ctx, "/user/keys?per_page=100", &ssh, ""); err == nil {
		for _, key := range ssh {
			fields := map[string]any{"keys": 1, "verified": key.Verified}
			if key.LastUsed != nil {
				fields["days_since_use"] = int(now.Sub(*key.LastUsed).Hours() / 24)
			} else {
				// Never used is not the same as used long ago, and it is the
				// reading that says a key can be removed.
				fields["never_used"] = 1
			}
			fields["age_days"] = int(now.Sub(key.CreatedAt).Hours() / 24)
			fields["url"] = "https://github.com/settings/keys"
			points = append(points, sink.Point{
				Measurement: "gh_key",
				Tags:        map[string]string{"user": k.Login, "kind": "ssh", "key": key.Title},
				Fields:      fields,
				// Stamped at the start of the day, not at creation. A key is a
				// standing fact like a deploy key, and dating it when it was
				// created would hide every key older than the dashboard range,
				// which is most of them.
				Time: day,
			})
		}
	} else if !isSkippable(err) {
		return points, err
	}

	var gpg []struct {
		KeyID     string     `json:"key_id"`
		CreatedAt time.Time  `json:"created_at"`
		ExpiresAt *time.Time `json:"expires_at"`
		Revoked   bool       `json:"revoked"`
		CanSign   bool       `json:"can_sign"`
		Emails    []struct {
			Email    string `json:"email"`
			Verified bool   `json:"verified"`
		} `json:"emails"`
	}
	if _, _, err := c.GetJSON(ctx, "/user/gpg_keys?per_page=100", &gpg, ""); err == nil {
		for _, key := range gpg {
			fields := map[string]any{
				"keys": 1, "revoked": key.Revoked, "can_sign": key.CanSign,
				"emails": len(key.Emails),
			}
			if key.ExpiresAt != nil {
				fields["days_to_expiry"] = int(key.ExpiresAt.Sub(now).Hours() / 24)
			}
			fields["age_days"] = int(now.Sub(key.CreatedAt).Hours() / 24)
			fields["url"] = "https://github.com/settings/keys"
			points = append(points, sink.Point{
				Measurement: "gh_key",
				Tags:        map[string]string{"user": k.Login, "kind": "gpg", "key": key.KeyID},
				Fields:      fields,
				Time:        day,
			})
		}
	} else if !isSkippable(err) {
		return points, err
	}
	return points, nil
}
