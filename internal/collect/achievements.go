package collect

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Achievements records the badges the profile page shows under its
// achievements tab: Pull Shark, Pair Extraordinaire, YOLO and the rest, with
// the tier each has reached.
//
// This is the one family that does not come from the API. GitHub exposes
// achievements nowhere in REST or GraphQL (checked on 2026-09-12: no field on
// User, no endpoint under /users), so the public profile page is read
// instead, without the token, and it costs nothing from any budget. The
// reading is strict on purpose: it accepts the markup recorded in
// testdata/achievements_page.html and nothing looser, and a page that no
// longer matches is a *MarkupError, which the runner logs once and writes no
// point for. A wrong number would look exactly like a right one on a
// dashboard, and a page GitHub redesigned would produce nothing but wrong
// numbers, so the parser refuses rather than guesses.
//
// One request a day is the intended cadence: a badge is earned over weeks.
//
// Beside the badges the family writes gh_achievement_progress, how far the
// account is from the next tier of each badge that has tiers; that half does
// come from the API, one counts query and a walk over the co-authored pull
// requests, and achievement_progress.go is where it lives.
//
// The same page carries the profile's vcard, and the vcard lists one link the
// API does not: the ORCID iD, which is connected in the profile settings and
// is in neither REST endpoint nor GraphQL's socialAccounts (checked on
// 2026-09-12). The read that is already paid for writes it as a
// gh_social_account row; see vcardSocialPoints.
type Achievements struct {
	Login string
	// WebBase is the root of the site the profile page is on. Empty means
	// github.com; a test hands in its own server.
	WebBase string
	// HTTP is the client the page is fetched with. Nil means a plain one
	// with a thirty second timeout. It is not the API client on purpose:
	// the API token has no business on a web page, and the page is not
	// charged to any API bucket.
	HTTP *http.Client
	// Warn is where the progress half reports what did not stop the
	// family: the counts it could not read, which leave the badges written
	// and the progress rows absent for the day, and a walk whose count is
	// a floor. Nil means silence.
	Warn func(msg string, args ...any)
}

// MarkupError says the achievements page no longer looks like the page this
// parser was written against. The runner logs it once, at warning, and the
// family writes nothing until the parser is updated. Reason says which of
// the expectations failed, so that update starts from the right place.
type MarkupError struct {
	Reason string
}

func (e *MarkupError) Error() string {
	return "achievements page: markup changed: " + e.Reason
}

// achievement is one badge as the page shows it.
type achievement struct {
	// Slug is GitHub's own name for the badge, pull-shark, the same in the
	// card's data attribute, the detail url and the badge image name.
	Slug string
	// Name is the badge's display name, Pull Shark.
	Name string
	// Tier is the badge level the page labels: 1 with no label, and the
	// number on the label otherwise (x2, x3, x4). Written as tier_number,
	// not tier: Elasticsearch maps a field name once across every
	// measurement's index, and tier is already a string on sponsorships,
	// which would make a top metric over this number fail on those shards.
	Tier int
	// TierName is the color GitHub gives the tier: default, bronze, silver
	// or gold, from the label's class and confirmed by the image's name.
	TierName string
	// Image is the badge image the page shows, at this tier, on GitHub's
	// asset host; a dashboard draws the badge from it.
	Image string
}

func (a Achievements) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	if a.Login == "" {
		return nil, nil
	}
	page, err := a.fetch(ctx)
	if isSkippable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	earned, err := parseAchievements(page, a.Login)
	if err != nil {
		return nil, err
	}
	socials, err := parseVcardSocials(page, a.Login)
	if err != nil {
		return nil, err
	}
	// A snapshot: the page says what is earned today, and a badge earned a
	// year ago reads the same as one earned this morning, so the row is
	// stamped at the start of the day it was read.
	day := now.UTC().Truncate(24 * time.Hour)
	points := a.vcardSocialPoints(socials, day)
	for _, e := range earned {
		points = append(points, sink.Point{
			Measurement: "gh_achievement",
			Tags:        map[string]string{"user": a.Login, "achievement": e.Slug},
			Fields: withURL(map[string]any{
				"name": e.Name, "tier_number": e.Tier, "tier_name": e.TierName, "present": 1,
				"image": e.Image,
			}, a.pageURL(e.Slug)),
			Time: day,
		})
	}
	// The progress rows ride beside the badges. A count the API would not
	// give is a day without progress rows, not a day without badges: the
	// page was read and what it says is written whatever the API did.
	counts, err := a.counts(ctx, c)
	if err != nil {
		a.warn("achievement counts unavailable, no progress rows today", "err", err)
		return points, nil
	}
	w := coauthoredWalk{login: a.Login}
	if walkErr := w.walk(ctx, c, counts.CreatedAt.UTC().Truncate(oneDay), day); walkErr != nil {
		a.warn("co-authored pull requests unavailable, no progress rows today", "err", walkErr, "queries", w.queries)
		return points, nil
	}
	if w.capped || w.truncated > 0 {
		a.warn("co-authored pull request count is a floor", "capped", w.capped, "truncated", w.truncated)
	}
	counts.Coauthored = w.pulls
	return append(points, a.progressPoints(earned, counts, day)...), nil
}

// warn reports through Warn when there is one.
func (a Achievements) warn(msg string, args ...any) {
	if a.Warn != nil {
		a.Warn(msg, args...)
	}
}

// fetch reads the achievements tab of the public profile, as an anonymous
// visitor would. No token: the page is public, and the API token must not
// travel to a host that is not the API.
func (a Achievements) fetch(ctx context.Context) (string, error) {
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.pageURL(""), http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ghchronicle")
	req.Header.Set("Accept", "text/html")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// The API answers its 404 as JSON and the site answers its own as
		// plain text (github.com, 2026-09-12). A JSON 404 means this url is
		// on the API host, which happens when github.base_url names a proxy
		// in front of api.github.com: reading it as "no badges" would keep
		// the family silent for good, so it is an error that names the fix.
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
			return "", fmt.Errorf("achievements page: %s answered 404 as %s, which is the API and not the site; set github.web_url to the site the profile is on", req.URL.Host, ct)
		}
		// The account has no badge, or is gone. An account with none has no
		// achievements tab, and the tab's url answers 404 while the profile
		// answers 200 (checked on 2026-09-12 against six accounts created
		// that week). Either way there is nothing to record and nothing to
		// fail the sweep over, the same reading the API families give a
		// 404.
		return "", &ghapi.UnavailableError{Path: req.URL.Path, Status: resp.StatusCode, Reason: "no profile page"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("achievements page: %s", resp.Status)
	}
	// The page is a quarter of a megabyte; a limit of four is a page that
	// has changed beyond recognition, and the parser will say so.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WebBaseFor is the site a profile page is on. A configured site wins,
// because the API root does not always say where the site is: a proxy in
// front of api.github.com is a root of its own, and deriving the site from
// it would send the family to a host that answers every page with a 404.
// Otherwise the site is derived from the API root the collectors use:
// github.com for api.github.com, the host itself for a GitHub Enterprise
// root of the form https://host/api/v3, and the root as given otherwise,
// which is how a test server answers both.
func WebBaseFor(apiBase, webURL string) string {
	if web := strings.TrimRight(webURL, "/"); web != "" {
		return web
	}
	base := strings.TrimRight(apiBase, "/")
	switch {
	case base == "" || base == "https://api.github.com":
		return "https://github.com"
	case strings.HasSuffix(base, "/api/v3"):
		return strings.TrimSuffix(base, "/api/v3")
	}
	return base
}

// pageURL is the achievements tab, or one badge's card on it when a slug is
// given, which is what the tab itself links a badge to.
func (a Achievements) pageURL(slug string) string {
	base := a.WebBase
	if base == "" {
		base = "https://github.com"
	}
	q := url.Values{}
	if slug != "" {
		q.Set("achievement", slug)
	}
	q.Set("tab", "achievements")
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(a.Login) + "?" + q.Encode()
}

// The markup the parser accepts, as recorded on 2026-09-12. Each earned badge
// is a <details> card carrying the slug as a data attribute; inside it, the
// badge image whose alt text names the badge and whose file name carries the
// tier color, the name as an <h3>, an optional tier label whose class carries
// the color and whose text is the multiplier, and the detail dialog whose src
// names the login and the slug again. Every one of these is checked against
// the others, so a page that moved one of them is a MarkupError rather than a
// badge with the wrong tier.
var (
	achievementCard   = regexp.MustCompile(`(?s)<details\s[^>]*?js-achievement-card-details[^>]*?data-achievement-slug="([^"]*)"[^>]*>(.*?)</details>`)
	achievementBadge  = regexp.MustCompile(`<img src="(https://github\.githubassets\.com/assets/([a-z0-9-]+)-(default|bronze|silver|gold)-[0-9a-f]+\.png)"[^>]*alt="Achievement: ([^"]+)"[^>]*class="achievement-badge-card"`)
	achievementName   = regexp.MustCompile(`<h3 class="f4 ws-normal">([^<]+)</h3>`)
	achievementLabel  = regexp.MustCompile(`<span[^>]*class="Label achievement-tier-label achievement-tier-label--(bronze|silver|gold)[^"]*"[^>]*>x(\d+)</span>`)
	achievementDialog = regexp.MustCompile(`<details-dialog\s[^>]*src="/users/([^/"]+)/achievements/([^/"]+)/detail"`)
)

// badgeReason opens every MarkupError raised while reading one card, so the
// failure names the badge whose markup moved rather than the page as a whole.
const badgeReason = "badge "

// parseAchievements reads the earned badges out of the page, or says how the
// page differs from the one it expects.
func parseAchievements(page, login string) ([]achievement, error) {
	cards := achievementCard.FindAllStringSubmatch(page, -1)
	if len(cards) == 0 {
		// A page with no card is not an account with no badge: such an
		// account has no achievements tab at all, and asking for it
		// answers 404, which fetch reads as unavailable (checked on
		// 2026-09-12 against six accounts created that week, every one
		// 404 on the tab and 200 on the profile). A 200 without a card is
		// a page this parser no longer recognizes.
		return nil, &MarkupError{Reason: "no achievement card on a page that answered 200"}
	}
	var out []achievement
	seen := map[string]bool{}
	for _, card := range cards {
		slug, body := card[1], card[2]
		if slug == "" {
			return nil, &MarkupError{Reason: "a card with an empty slug"}
		}
		if seen[slug] {
			return nil, &MarkupError{Reason: badgeReason + slug + " listed twice"}
		}
		seen[slug] = true
		one, err := parseAchievementCard(slug, body, login)
		if err != nil {
			return nil, err
		}
		out = append(out, one)
	}
	return out, nil
}

// parseAchievementCard reads one card, holding each part to the others.
func parseAchievementCard(slug, body, login string) (achievement, error) {
	badge := achievementBadge.FindStringSubmatch(body)
	if badge == nil {
		return achievement{}, &MarkupError{Reason: badgeReason + slug + ": no badge image with a tier in its name"}
	}
	image, tierName := badge[1], badge[3]
	if badge[2] != slug {
		return achievement{}, &MarkupError{Reason: fmt.Sprintf("badge %s: image is for %s", slug, badge[2])}
	}
	name := html.UnescapeString(badge[4])
	heading := achievementName.FindStringSubmatch(body)
	if len(heading) < 2 || html.UnescapeString(strings.TrimSpace(heading[1])) != name {
		return achievement{}, &MarkupError{Reason: badgeReason + slug + ": no heading, or one that disagrees with the image"}
	}
	dialog := achievementDialog.FindStringSubmatch(body)
	// The login is compared case-insensitively: GitHub renders its own
	// spelling of it, and the configured one need not match it letter for
	// letter for the same account.
	if len(dialog) < 3 || !strings.EqualFold(dialog[1], login) || dialog[2] != slug {
		return achievement{}, &MarkupError{Reason: badgeReason + slug + ": no detail dialog for this login and slug"}
	}
	out := achievement{Slug: slug, Name: name, Tier: 1, TierName: tierName, Image: image}
	label := achievementLabel.FindStringSubmatch(body)
	switch {
	case label == nil && tierName == "default":
		// No label is the first tier, and the image agrees.
	case label == nil:
		return achievement{}, &MarkupError{Reason: badgeReason + slug + ": a " + tierName + " image with no tier label"}
	case label[1] != tierName:
		return achievement{}, &MarkupError{Reason: fmt.Sprintf("badge %s: label says %s, image says %s", slug, label[1], tierName)}
	default:
		n, err := strconv.Atoi(label[2])
		if err != nil || n < 2 {
			return achievement{}, &MarkupError{Reason: badgeReason + slug + ": tier label x" + label[2]}
		}
		out.Tier = n
	}
	return out, nil
}

// IsMarkupError says whether an error is the page having changed, which the
// runner reports once rather than on every sweep.
func IsMarkupError(err error) bool {
	_, changed := errors.AsType[*MarkupError](err)
	return changed
}

// The vcard, as recorded on the same page. The username is the one element of
// it every profile renders, and the details list is rendered even when the
// profile has nothing to put in it: checked on 2026-09-12 against four
// accounts with badges and neither location, website, company nor social
// account, every one of them a `<ul class="vcard-details">` with nothing
// inside. So a page without either is a page this parser no longer
// recognizes, and an empty list is a profile that lists nothing.
//
// Each social account is an <li itemprop="social"> holding the provider's SVG
// and one link; the website above them is itemprop="url" and is not matched.
var (
	vcardUsername   = regexp.MustCompile(`<span class="p-nickname vcard-username d-block" itemprop="additionalName">\s*([^<\s]+)\s*</span>`)
	vcardDetails    = regexp.MustCompile(`(?s)<ul class="vcard-details">(.*?)</ul>`)
	vcardSocialItem = regexp.MustCompile(`(?s)<li itemprop="social" class="vcard-detail[^"]*">(.*?)</li>`)
	vcardLink       = regexp.MustCompile(`<a rel="nofollow me" class="Link--primary wb-break-all" href="([^"]+)">`)
)

// pageOnlyProviders names the hosts of the social links the profile page
// shows and the API does not list, and the provider each is written under.
// Every other social link on the page is one the API answers with its own
// provider name, mastodon, linkedin, bluesky or generic, and the profile
// family writes it from there; the page's copy of it is skipped rather than
// written a second time, because the host does not say which provider it is
// (a Mastodon account is on any host at all) and the API does.
var pageOnlyProviders = map[string]string{
	"orcid.org": "orcid",
}

// vcardSocial is one link the profile's vcard shows that the API does not.
type vcardSocial struct {
	Provider string
	URL      string
}

// parseVcardSocials reads the social links the API does not list out of the
// vcard, or says how the page differs from the one it expects. A page whose
// vcard lists no such link answers an empty slice and no error: that is a
// profile that has none, which most have.
func parseVcardSocials(page, login string) ([]vcardSocial, error) {
	name := vcardUsername.FindStringSubmatch(page)
	if len(name) < 2 || !strings.EqualFold(name[1], login) {
		return nil, &MarkupError{Reason: "no vcard username for this login"}
	}
	details := vcardDetails.FindStringSubmatch(page)
	if len(details) < 2 {
		return nil, &MarkupError{Reason: "no vcard details list on a page that answered 200"}
	}
	items := vcardSocialItem.FindAllStringSubmatch(details[1], -1)
	// The list is read up to its first closing tag, so a list nested before
	// the social items would end the read early and turn every account
	// below it into "none", silently. Every social item on the page is in
	// the vcard, so one outside the region read is the region being short.
	if all := len(vcardSocialItem.FindAllStringIndex(page, -1)); all != len(items) {
		return nil, &MarkupError{Reason: fmt.Sprintf("%d social accounts on the page, %d in the vcard details list", all, len(items))}
	}
	var out []vcardSocial
	seen := map[string]bool{}
	for _, item := range items {
		links := vcardLink.FindAllStringSubmatch(item[1], -1)
		if len(links) != 1 {
			return nil, &MarkupError{Reason: fmt.Sprintf("a social account with %d links, want one", len(links))}
		}
		link := html.UnescapeString(links[0][1])
		u, err := url.Parse(link)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, &MarkupError{Reason: "a social account whose link is not an absolute https url: " + link}
		}
		// The host as GitHub spells it today is lower case; the map is
		// keyed on the lower case form so a change of case does not turn
		// the ORCID into a link the API is assumed to list.
		provider, pageOnly := pageOnlyProviders[strings.ToLower(u.Hostname())]
		if !pageOnly {
			continue
		}
		if seen[provider] {
			return nil, &MarkupError{Reason: "social account " + provider + " listed twice"}
		}
		seen[provider] = true
		out = append(out, vcardSocial{Provider: provider, URL: link})
	}
	return out, nil
}

// vcardSocialPoints is one gh_social_account row per link the vcard shows
// and the API does not, the same shape the profile family writes for the
// ones it does, stamped at the start of the day like the badges beside it.
func (a Achievements) vcardSocialPoints(socials []vcardSocial, day time.Time) []sink.Point {
	var points []sink.Point
	for _, s := range socials {
		points = append(points, sink.Point{
			Measurement: "gh_social_account",
			Tags:        map[string]string{"user": a.Login, "provider": s.Provider},
			Fields:      map[string]any{"url": s.URL, "present": 1},
			Time:        day,
		})
	}
	return points
}
