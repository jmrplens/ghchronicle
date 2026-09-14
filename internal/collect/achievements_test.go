package collect

import (
	"net/http"
	"strings"
	"testing"
)

// achievementsPage serves the recorded profile page on the tab the collector
// asks for, and whatever the test hands in on the same path otherwise.
func achievementsPage(f *fixtureServer, login string, page func() string) {
	f.handle("/"+login, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tab") != "achievements" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			f.t.Errorf("the API token traveled to the web page: %q", auth)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page()))
	})
}

func recordedAchievements(t *testing.T) string {
	t.Helper()
	return string(fixture(t, "achievements_page.html"))
}

// TestAchievementsReadsTheProfilePage pins the reading of the page recorded
// on 2026-09-12 from https://github.com/jmrplens?tab=achievements: eight
// badges, Pull Shark at gold x4, Pair Extraordinaire at silver x3, and six
// at their first tier with no label at all.
func TestAchievementsReadsTheProfilePage(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return recordedAchievements(t) })

	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_achievement", "gh_social_account")
	if badges := only(t, points, "gh_achievement"); len(badges) != 8 {
		t.Fatalf("got %d badges, want the 8 on the page", len(badges))
	}
	for slug, want := range map[string]struct {
		name string
		tier int64
		kind string
	}{
		"pull-shark":                    {"Pull Shark", 4, "gold"},
		"pair-extraordinaire":           {"Pair Extraordinaire", 3, "silver"},
		"yolo":                          {"YOLO", 1, "default"},
		"galaxy-brain":                  {"Galaxy Brain", 1, "default"},
		"arctic-code-vault-contributor": {"Arctic Code Vault Contributor", 1, "default"},
	} {
		p := find(t, points, "gh_achievement", map[string]string{"user": "jmrplens", "achievement": slug})
		if p.Fields["name"] != want.name || fieldInt(t, p, "tier_number") != want.tier || p.Fields["tier_name"] != want.kind || p.Fields["present"] != 1 {
			t.Errorf("%s = %v, want %+v", slug, p.Fields, want)
		}
		if p.Fields["url"] != f.srv.URL+"/jmrplens?achievement="+slug+"&tab=achievements" {
			t.Errorf("%s url = %v", slug, p.Fields["url"])
		}
		// A snapshot of what is earned, stamped at the start of the day it
		// was read: the page does not say when a badge was earned.
		if !p.Time.Equal(startOfDay(testNow)) {
			t.Errorf("%s stamped %s, want the start of the day", slug, p.Time)
		}
	}
	// One request: the whole family is one page.
	if n := len(f.calls("/jmrplens")); n != 1 {
		t.Errorf("%d page requests, want 1", n)
	}
}

// The label is the tier: x2 is bronze, x3 silver, x4 gold. The recorded page
// has a silver x3 and a gold x4; the bronze x2 is the same card one tier
// down, rewritten in both the places the tier appears, because the parser
// holds the two to each other.
func TestAchievementsReadsTheTierBadge(t *testing.T) {
	t.Parallel()
	page := recordedAchievements(t)
	bronze := strings.ReplaceAll(page, "pair-extraordinaire-silver-", "pair-extraordinaire-bronze-")
	bronze = strings.ReplaceAll(bronze, "achievement-tier-label--silver", "achievement-tier-label--bronze")
	bronze = strings.ReplaceAll(bronze, `ml-2 tmp-ml-2">x3</span>`, `ml-2 tmp-ml-2">x2</span>`)
	if bronze == page {
		t.Fatal("the rewrite did not find the silver card it was written against")
	}
	earned, err := parseAchievements(bronze, "jmrplens")
	if err != nil {
		t.Fatal(err)
	}
	tiers := map[string]achievement{}
	for _, e := range earned {
		tiers[e.Slug] = e
	}
	if got := tiers["pair-extraordinaire"]; got.Tier != 2 || got.TierName != "bronze" {
		t.Errorf("bronze card read as %+v", got)
	}
	if got := tiers["pull-shark"]; got.Tier != 4 || got.TierName != "gold" {
		t.Errorf("gold card read as %+v", got)
	}
	if got := tiers["quickdraw"]; got.Tier != 1 || got.TierName != "default" {
		t.Errorf("unlabelled card read as %+v", got)
	}
}

// TestAchievementsRefuseAPageThatChanged pins the promise the family makes:
// a page that no longer matches is a MarkupError and no points, never a
// number read off the wrong element. Each case breaks one of the things the
// parser holds together.
func TestAchievementsRefuseAPageThatChanged(t *testing.T) {
	t.Parallel()
	page := recordedAchievements(t)
	cases := map[string]string{
		"a redesign with no cards and no heading": "<html><body><h2>Achievements</h2><div class=\"badges\"></div></body></html>",
		"the card attribute renamed":              strings.ReplaceAll(page, "data-achievement-slug=", "data-badge-slug="),
		"the tier label without its multiplier":   strings.ReplaceAll(page, `ml-2 tmp-ml-2">x4</span>`, `ml-2 tmp-ml-2">Gold</span>`),
		"the label and the image disagreeing":     strings.ReplaceAll(page, "achievement-tier-label--gold", "achievement-tier-label--silver"),
		"a tiered image with no label":            strings.ReplaceAll(page, "quickdraw-default-", "quickdraw-gold-"),
		"the heading disagreeing with the image":  strings.ReplaceAll(page, "<h3 class=\"f4 ws-normal\">YOLO</h3>", "<h3 class=\"f4 ws-normal\">Yolo</h3>"),
		"the detail dialog for another login":     strings.ReplaceAll(page, "/users/jmrplens/achievements/", "/users/octocat/achievements/"),
	}
	for name, changed := range cases {
		if name != "a redesign with no cards and no heading" && changed == page {
			t.Errorf("%s: the rewrite found nothing to change", name)
			continue
		}
		earned, err := parseAchievements(changed, "jmrplens")
		if !IsMarkupError(err) {
			t.Errorf("%s: got %d badges and %v, want a MarkupError", name, len(earned), err)
		}
	}

	// And the collector passes it up unchanged, with no points beside it.
	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return cases["the card attribute renamed"] })
	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if !IsMarkupError(err) || len(points) != 0 {
		t.Errorf("collect: got %d points and %v, want a MarkupError and nothing", len(points), err)
	}
}

// TestAchievementsReadTheORCIDFromTheVcard pins the one social link the
// page shows and no API lists: the ORCID iD, recorded on 2026-09-12 beside
// Mastodon, LinkedIn and Bluesky in the same vcard. Those three the API
// answers under their own provider names and the profile family writes from
// there, so the page's copy of them is not written a second time; the ORCID
// is written, under the provider its host names, in the shape the profile
// family uses. One request still: the vcard is on the achievements tab.
func TestAchievementsReadTheORCIDFromTheVcard(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return recordedAchievements(t) })

	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	socials := only(t, points, "gh_social_account")
	if len(socials) != 1 {
		t.Fatalf("got %d social accounts, want the ORCID alone: %v", len(socials), socials)
	}
	orcid := find(t, points, "gh_social_account", map[string]string{"user": "jmrplens", "provider": "orcid"})
	if orcid.Fields["url"] != "https://orcid.org/0000-0003-1250-6212" || orcid.Fields["present"] != 1 || len(orcid.Fields) != 2 {
		t.Errorf("orcid = %v", orcid.Fields)
	}
	if !orcid.Time.Equal(startOfDay(testNow)) {
		t.Errorf("orcid stamped %s, want the start of the day like the badges", orcid.Time)
	}
	for _, provider := range []string{"mastodon", "linkedin", "bluesky"} {
		for _, p := range socials {
			if p.Tags["provider"] == provider {
				t.Errorf("%s was written from the page as well as from the API", provider)
			}
		}
	}
	if n := len(f.calls("/jmrplens")); n != 1 {
		t.Errorf("%d page requests, want 1", n)
	}
	// The provider comes from the host however the page spells it: a
	// capitalised host is the same ORCID, not a link the API lists.
	spelled := strings.Replace(recordedAchievements(t), `href="https://orcid.org/`, `href="https://ORCID.org/`, 1)
	if spelt, spellErr := parseVcardSocials(spelled, "jmrplens"); spellErr != nil || len(spelt) != 1 || spelt[0].Provider != "orcid" {
		t.Errorf("a capitalised host: got %v and %v, want the one orcid", spelt, spellErr)
	}
}

// A vcard with no page-only link is a profile that has none, which most
// have: the badges are written and no social row is, without a warning. The
// recorded page with its ORCID item cut out is that profile.
func TestAchievementsWriteNoSocialRowForAVcardWithoutOne(t *testing.T) {
	t.Parallel()
	page := recordedAchievements(t)
	start := strings.Index(page, `<li itemprop="social" class="vcard-detail pt-1 "><!-- Generator: Adobe Illustrator`)
	end := strings.Index(page[start:], "</li>") + start + len("</li>")
	if start < 0 || end <= start || !strings.Contains(page[start:end], "orcid.org") {
		t.Fatal("the ORCID item is not where the test expects it")
	}
	without := page[:start] + page[end:]

	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return without })
	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantMeasurements(t, points, "gh_achievement")
	if n := len(only(t, points, "gh_achievement")); n != 8 {
		t.Errorf("got %d badges, want the 8 on the page", n)
	}
}

// TestAchievementsRefuseAVcardThatChanged is the vcard's half of the promise:
// a page whose vcard is gone or reshaped is a MarkupError and no points, and
// never "no accounts". Each case breaks one of the things the parser holds
// the page to.
func TestAchievementsRefuseAVcardThatChanged(t *testing.T) {
	t.Parallel()
	page := recordedAchievements(t)
	cases := map[string]string{
		"the details list renamed":              strings.ReplaceAll(page, `<ul class="vcard-details">`, `<ul class="profile-details">`),
		"the username element renamed":          strings.ReplaceAll(page, `class="p-nickname vcard-username d-block"`, `class="p-nickname d-block"`),
		"the vcard for another login":           strings.ReplaceAll(page, "itemprop=\"additionalName\">\n          jmrplens", "itemprop=\"additionalName\">\n          octocat"),
		"a social account without a link":       strings.ReplaceAll(page, `<a rel="nofollow me" class="Link--primary wb-break-all" href="https://orcid.org/0000-0003-1250-6212">`, `<span>`),
		"a social account linked twice":         strings.ReplaceAll(page, `href="https://linkedin.com/in/jmrplens">in/jmrplens</a>`, `href="https://linkedin.com/in/jmrplens">in/jmrplens</a><a rel="nofollow me" class="Link--primary wb-break-all" href="https://linkedin.com/in/jmrplens">`),
		"a social account with a relative link": strings.ReplaceAll(page, `href="https://orcid.org/0000-0003-1250-6212">`, `href="/orcid/0000-0003-1250-6212">`),
		// The details list is read up to its first closing tag; a list
		// nested before the accounts would otherwise read as a vcard with
		// none, which is the silent reading the parser exists to refuse.
		"a list nested before the social accounts": strings.ReplaceAll(page, `<ul class="vcard-details">`, `<ul class="vcard-details"><ul class="vcard-extra"></ul>`),
	}
	for name, changed := range cases {
		if changed == page {
			t.Errorf("%s: the rewrite found nothing to change", name)
			continue
		}
		socials, err := parseVcardSocials(changed, "jmrplens")
		if !IsMarkupError(err) {
			t.Errorf("%s: got %d accounts and %v, want a MarkupError", name, len(socials), err)
		}
	}
	// The same ORCID twice is two rows on one series, which no reader could
	// tell apart from one, so it is refused as well.
	twice := strings.Replace(page, `href="https://linkedin.com/in/jmrplens">`, `href="https://orcid.org/0000-0003-1250-6212">`, 1)
	if socials, err := parseVcardSocials(twice, "jmrplens"); !IsMarkupError(err) {
		t.Errorf("the ORCID listed twice: got %d accounts and %v, want a MarkupError", len(socials), err)
	}

	// And the collector passes it up unchanged, with no points beside it:
	// a page whose badges still read is refused whole, since the vcard is
	// the same page and one part of it having moved says the rest may have.
	f := newFixtureServer(t)
	achievementsPage(f, "jmrplens", func() string { return cases["the details list renamed"] })
	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if !IsMarkupError(err) || len(points) != 0 {
		t.Errorf("collect: got %d points and %v, want a MarkupError and nothing", len(points), err)
	}
}

// TestAchievementsTreatNoTabAsNoBadges pins what an account with no badge
// looks like, checked on 2026-09-12 against six accounts created that week:
// the profile answers 200 and has no achievements tab, and asking for the tab
// answers 404. That is nothing to write and nothing to warn about, and the
// same answer a profile that is gone gives.
func TestAchievementsTreatNoTabAsNoBadges(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// As github.com answers it: a plain-text 404, not the API's JSON one.
	f.handle("/nobody", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.NotFound(w, r)
	})
	points, err := Achievements{Login: "nobody", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err != nil || len(points) != 0 {
		t.Errorf("got %d points and %v, want nothing and no error", len(points), err)
	}
}

// A 200 with no card is not an account with none, since that account has no
// tab to answer 200 with: it is a page this parser does not recognize, and
// it is refused rather than read as "no badges".
func TestAchievementsRefuseACardlessPage(t *testing.T) {
	t.Parallel()
	page := "<html><body><turbo-frame><div><h2 class=\"tmp-mb-4\">Earned achievements</h2><div></div></div></turbo-frame></body></html>"
	earned, err := parseAchievements(page, "octocat")
	if !IsMarkupError(err) || len(earned) != 0 {
		t.Errorf("got %d badges and %v, want a MarkupError", len(earned), err)
	}
}

// The configured login need not match GitHub's spelling of it letter for
// letter: the page renders its own, and the account is the same.
func TestAchievementsAcceptTheLoginInAnyCase(t *testing.T) {
	t.Parallel()
	earned, err := parseAchievements(recordedAchievements(t), "JMRPlens")
	if err != nil || len(earned) != 8 {
		t.Errorf("got %d badges and %v, want the 8 on the page", len(earned), err)
	}
}

// A 404 the API host answers is not an account with no badges. Recorded on
// 2026-09-12: api.github.com answers /login?tab=achievements with a JSON
// 404, and github.com answers a missing tab as plain text; a config whose
// base_url is a proxy in front of the API sends the page request to the
// former, and reading that as "none" would keep the family silent for good.
func TestAchievementsRefuseTheAPIHostsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/jmrplens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","status":"404"}`))
	})
	points, err := Achievements{Login: "jmrplens", WebBase: f.srv.URL}.Collect(ctx(t), f.Client, testNow)
	if err == nil || isSkippable(err) || len(points) != 0 {
		t.Fatalf("got %d points and %v, want an error that is not skippable", len(points), err)
	}
	if !strings.Contains(err.Error(), "github.web_url") {
		t.Errorf("error %q does not name the setting that fixes it", err)
	}
}

// The page lives beside the API, not on it, and a configured site wins over
// the derivation, since a proxy in front of the API says nothing about
// where the site is.
func TestWebBaseFor(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                               "https://github.com",
		"https://api.github.com":         "https://github.com",
		"https://api.github.com/":        "https://github.com",
		"https://ghe.example.com/api/v3": "https://ghe.example.com",
		"http://127.0.0.1:4321":          "http://127.0.0.1:4321",
	} {
		if got := WebBaseFor(in, ""); got != want {
			t.Errorf("WebBaseFor(%q, \"\") = %q, want %q", in, got, want)
		}
	}
	if got := WebBaseFor("http://127.0.0.1:8766", "https://github.com/"); got != "https://github.com" {
		t.Errorf("WebBaseFor(proxy, site) = %q, want the site", got)
	}
}
