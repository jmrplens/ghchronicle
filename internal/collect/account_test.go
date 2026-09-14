package collect

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

func TestAccount(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var gotVars map[string]any
	f.graphQL(func(w http.ResponseWriter, r *http.Request, query string, vars map[string]any) {
		gotVars = vars
		if !strings.Contains(query, "contributionCalendar") {
			t.Errorf("query does not ask for the calendar:\n%s", query)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		f.write(w, "graphql_account.json")
	})
	f.file("/users/octocat", "user_profile.json")
	// The container registry, which GraphQL's packages connection does not
	// see: the fixture's connection says 0 and REST lists one.
	f.handle("/user/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("package_type") == "container" {
			f.write(w, "packages_container.json")
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_account", "gh_contributions_total", "gh_contribution_day",
		"gh_contribution_repo", "gh_contribution_day_repo", "gh_pinned_item", "gh_profile_flag",
		"gh_sponsorship", "gh_sponsors_listing", "gh_sponsors_tier", "gh_star_list")
	if gotVars["login"] != "octocat" {
		t.Errorf("variables = %v", gotVars)
	}
	if n := len(f.calls("/graphql")); n != 1 {
		t.Errorf("the whole account costs one query, made %d", n)
	}
	if n := len(f.calls("/users/octocat")); n != 1 {
		t.Errorf("the follow count costs one REST request, made %d", n)
	}
	if n := len(f.calls("/user/packages")); n != len(packageKinds) {
		t.Errorf("the package count costs one listing per registry, made %d", n)
	}
	checkAccountHeadline(t, points)

	checkCalendarDays(t, points)

	// The kind has to be named: the same repository now appears under more
	// than one of the four, and find takes the first match in a map whose
	// iteration order is random.
	other := find(t, points, "gh_contribution_repo",
		map[string]string{"repo": "someone/else", "kind": "commits"})
	if fieldInt(t, other, "commits") != 12 || fieldInt(t, other, "days") != 1 ||
		fieldInt(t, other, "commits_dated") != 12 {
		t.Errorf("contribution repo = %v", other.Fields)
	}
	// The three fields the daily breakdown adds belong to the commit
	// connection alone: the other three kinds have no sub-connection to count.
	issues := find(t, points, "gh_contribution_repo",
		map[string]string{"repo": "someone/else", "kind": "issues"})
	if hasField(issues, "days") || hasField(issues, "commits_dated") {
		t.Errorf("a non-commit kind has no breakdown: %v", issues.Fields)
	}
}

// checkAccountHeadline reads the two rows a profile tile shows: the account
// itself and its contribution totals for the year.
func checkAccountHeadline(t *testing.T, points []sink.Point) {
	t.Helper()
	acc := only(t, points, "gh_account")[0]
	if acc.Tags["user"] != "octocat" || !acc.Time.Equal(testNow) {
		t.Errorf("account = %v at %s", acc.Tags, acc.Time)
	}
	if fieldInt(t, acc, "followers") != 1200 || fieldInt(t, acc, "public_repos") != 42 || fieldInt(t, acc, "sponsors") != 3 {
		t.Errorf("account fields = %v", acc.Fields)
	}
	if fieldInt(t, acc, "account_age_days") < 5000 {
		t.Errorf("account_age_days = %v", acc.Fields["account_age_days"])
	}
	// GraphQL's connection counts 4 people; the profile follows 9 accounts,
	// five of them organizations. The headline field is the profile's number.
	if fieldInt(t, acc, "following") != 9 || fieldInt(t, acc, "following_users") != 4 {
		t.Errorf("following = %v, following_users = %v", acc.Fields["following"], acc.Fields["following_users"])
	}
	// GraphQL says 0 packages and REST lists a container: the headline
	// number is the one gh_package will agree with.
	if fieldInt(t, acc, "packages") != 1 {
		t.Errorf("packages = %v, want the REST count of 1, not GraphQL's zero", acc.Fields["packages"])
	}
	// The pronouns line as the profile shows it, a field beside the counts.
	if acc.Fields["pronouns"] != "he/him" {
		t.Errorf("pronouns = %v, want he/him", acc.Fields["pronouns"])
	}
	total := only(t, points, "gh_contributions_total")[0]
	if fieldInt(t, total, "commits") != 812 || fieldInt(t, total, "calendar_total") != 1105 || fieldInt(t, total, "restricted") != 120 {
		t.Errorf("contributions = %v", total.Fields)
	}
}

// TestCommitTotalsCountTheDatedCommits pins the one number that says whether
// the hundred-day page dropped anything.
//
// contributions.totalCount counts commits and the nodes count days: measured
// live on all 39 repositories of this account, the two agree exactly, and the
// busiest repository used 80 of the 100 available days. A repository that goes
// past a hundred commit days in one window has not been observed here, so it
// is asserted against the shape rather than served from a fixture pretending
// GitHub sent it.
func TestCommitTotalsCountTheDatedCommits(t *testing.T) {
	t.Parallel()
	var rs commitContributionRepos
	if err := json.Unmarshal([]byte(`[
      {"repository": {"nameWithOwner": "octocat/whole", "isPrivate": false},
       "contributions": {"totalCount": 9, "nodes": [
         {"occurredAt": "2026-09-07T07:00:00Z", "commitCount": 4},
         {"occurredAt": "2026-09-06T07:00:00Z", "commitCount": 5}]}},
      {"repository": {"nameWithOwner": "octocat/capped", "isPrivate": false},
       "contributions": {"totalCount": 900, "nodes": [
         {"occurredAt": "2026-09-07T07:00:00Z", "commitCount": 4}]}}
    ]`), &rs); err != nil {
		t.Fatal(err)
	}
	got := rs.totals()
	if got[0].total != 9 || got[0].dated != 9 || got[0].days != 2 {
		t.Errorf("a complete breakdown = %+v", got[0])
	}
	// 896 commits happened on days GitHub did not serve. Nothing else in the
	// output would say so.
	if got[1].total != 900 || got[1].dated != 4 || got[1].days != 1 {
		t.Errorf("a truncated breakdown = %+v", got[1])
	}
}

// TestAccountSplitsTheCalendarByRepository reads the per-repository daily
// breakdown, which is the only surface that sees the private repositories and
// the ones belonging to other people.
func TestAccountSplitsTheCalendarByRepository(t *testing.T) {
	t.Parallel()
	f := accountServer(t)
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	days := only(t, points, "gh_contribution_day_repo")
	if len(days) != 5 {
		t.Fatalf("got %d daily rows, want one per repository per day", len(days))
	}
	// Every row carries all four tags, whatever its values: a tag written
	// under an `if` would give the measurement two path depths.
	checkEveryRowTagged(t, days, "user", "repo", "private", "own")

	own := find(t, points, "gh_contribution_day_repo", map[string]string{"repo": "octocat/hello-world"})
	if own.Tags["own"] != "true" || own.Tags["private"] != "false" || own.Tags["user"] != "octocat" {
		t.Errorf("own repository row = %v", own.Tags)
	}
	// The date is the day the commits belong to, taken from occurredAt, and
	// occurredAt is not the day boundary it looks like: see calendarDay.
	if want := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC); !own.Time.Equal(want) {
		t.Errorf("stamped %s, want the day it belongs to %s", own.Time, want)
	}
	if fieldInt(t, own, "commits") != 8 {
		t.Errorf("own repository fields = %v", own.Fields)
	}
	// The winter row, which GitHub renders an hour further from midnight than
	// the summer one, has to land on its own day just the same.
	var winter []time.Time
	for _, p := range days {
		if p.Tags["repo"] == "octocat/hello-world" && fieldInt(t, p, "commits") == 489 {
			winter = append(winter, p.Time)
		}
	}
	want := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if len(winter) != 1 || !winter[0].Equal(want) {
		t.Errorf("the T08:00:00Z row stamped %v, want %s", winter, want)
	}

	private := find(t, points, "gh_contribution_day_repo", map[string]string{"repo": "octocat/secret-thing"})
	if private.Tags["private"] != "true" || private.Tags["own"] != "true" {
		t.Errorf("private repository row = %v", private.Tags)
	}
	third := find(t, points, "gh_contribution_day_repo", map[string]string{"repo": "someone/else"})
	if third.Tags["own"] != "false" || third.Tags["private"] != "false" {
		t.Errorf("third party row = %v", third.Tags)
	}

	checkDailyCommitsAddUp(t, points)
}

// checkEveryRowTagged reports every row that leaves one of the tags out.
func checkEveryRowTagged(t *testing.T, rows []sink.Point, tags ...string) {
	t.Helper()
	for _, p := range rows {
		for _, tag := range tags {
			if p.Tags[tag] == "" {
				t.Errorf("%v has no %s tag", p.Tags, tag)
			}
		}
	}
}

// checkDailyCommitsAddUp holds the per-repository daily breakdown to the
// per-repository totals. The breakdown is a strict subset of what the
// account row already reports, which is the check that says the two agree.
func checkDailyCommitsAddUp(t *testing.T, points []sink.Point) {
	t.Helper()
	var sum int64
	for _, p := range only(t, points, "gh_contribution_day_repo") {
		sum += fieldInt(t, p, "commits")
	}
	var totals, dated int64
	for _, p := range only(t, points, "gh_contribution_repo") {
		if p.Tags["kind"] == "commits" {
			totals += fieldInt(t, p, "commits")
			dated += fieldInt(t, p, "commits_dated")
		}
	}
	if sum != totals || sum != dated {
		t.Errorf("daily rows sum to %d, the per-repository totals to %d, commits_dated to %d",
			sum, totals, dated)
	}
}

// TestCalendarDayTakesTheDateFromTheString pins the dating rule of
// gh_contribution_day_repo.
//
// The first two forms are what this account measures today, and truncating a
// parsed time would agree with them. The third is the one that separates the
// two implementations: a positive offset renders local midnight on the
// previous UTC day, and truncating would move the row back a day. It has not
// been observed on this account, so it is asserted here against the helper
// rather than served from a fixture pretending GitHub sent it.
func TestCalendarDayTakesTheDateFromTheString(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"2026-09-08T07:00:00Z":      "2026-09-08",
		"2026-01-15T08:00:00Z":      "2026-01-15",
		"2026-09-08T00:00:00+09:00": "2026-09-08",
	} {
		got, err := calendarDay(in)
		if err != nil {
			t.Fatalf("calendarDay(%q): %v", in, err)
		}
		if got.Format("2006-01-02") != want || !got.Equal(got.UTC().Truncate(24*time.Hour)) {
			t.Errorf("calendarDay(%q) = %s, want %s at midnight UTC", in, got, want)
		}
	}
	if _, err := calendarDay("not a date"); err == nil {
		t.Error("a malformed timestamp must not become a day")
	}
}

// accountServer answers the one GraphQL query and the one profile request the
// collector makes.
func accountServer(t *testing.T) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		f.write(w, "graphql_account.json")
	})
	f.file("/users/octocat", "user_profile.json")
	return f
}

func TestAccountFollowingKeepsTheConnectionCountWhenTheProfileIsUnreachable(t *testing.T) {
	t.Parallel()
	// An undercount is better than no account at all. The runner discards
	// every point of a family whose collector returns an error, so a profile
	// request that fails must not take the calendar with it, whichever way it
	// fails: 404 for a login that is gone, 500 for a bad ten minutes at the
	// API.
	for _, tc := range []struct {
		name  string
		route func(f *fixtureServer)
	}{
		{"missing", func(*fixtureServer) {}},
		{"broken", func(f *fixtureServer) {
			f.status("/users/octocat", http.StatusInternalServerError, "Server Error")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureServer(t)
			f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
				f.write(w, "graphql_account.json")
			})
			tc.route(f)
			points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
			if err != nil {
				t.Fatalf("an unreachable profile is not a failure: %v", err)
			}
			acc := only(t, points, "gh_account")[0]
			if fieldInt(t, acc, "following") != 4 {
				t.Errorf("following = %v, want the GraphQL count as the fallback", acc.Fields["following"])
			}
			// The calendar is what would have been lost with it.
			if n := len(only(t, points, "gh_contribution_day")); n != 5 {
				t.Errorf("got %d calendar days, want the 5 the query already paid for", n)
			}
		})
	}
}

func TestAccountPinnedItemsAndProfileFlags(t *testing.T) {
	t.Parallel()
	f := accountServer(t)
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	pins := only(t, points, "gh_pinned_item")
	if len(pins) != 2 {
		t.Fatalf("got %d pinned items, want the repository and the gist", len(pins))
	}
	repo := find(t, points, "gh_pinned_item", map[string]string{"repo": "octocat/hello-world"})
	if !repo.Time.Equal(testNow) {
		t.Errorf("a pin has no date of its own and is stamped now, got %s", repo.Time)
	}
	if fieldInt(t, repo, "position") != 1 || fieldInt(t, repo, "stars") != 113 ||
		fieldInt(t, repo, "days_since_push") != 2 || repo.Fields["kind"] != "repository" {
		t.Errorf("pinned repository = %v", repo.Fields)
	}
	gist := find(t, points, "gh_pinned_item", map[string]string{"repo": "aa5a315d61ae9438b18d"})
	if gist.Fields["kind"] != "gist" || hasField(gist, "stars") {
		t.Errorf("pinned gist = %v", gist.Fields)
	}
	if gist.Fields["url"] != "https://gist.github.com/octocat/aa5a315d61ae9438b18d" {
		t.Errorf("pinned gist url = %v", gist.Fields["url"])
	}

	flags := only(t, points, "gh_profile_flag")
	if len(flags) != 8 {
		t.Fatalf("got %d flags, want the seven badges and the status", len(flags))
	}
	if hire := find(t, points, "gh_profile_flag", map[string]string{"flag": "hireable"}); hire.Fields["enabled"] != true {
		t.Errorf("hireable = %v", hire.Fields)
	}
	// hasSponsorsListing from the same query, the seventh boolean, beside
	// the has_listing the sponsors listing row already carries.
	if listing := find(t, points, "gh_profile_flag", map[string]string{"flag": "sponsors_listing"}); listing.Fields["enabled"] != true || len(listing.Fields) != 2 {
		t.Errorf("sponsors_listing = %v, want enabled and the url alone", listing.Fields)
	}
	if star := find(t, points, "gh_profile_flag", map[string]string{"flag": "github_star"}); star.Fields["enabled"] != false {
		t.Errorf("github_star = %v", star.Fields)
	}
	status := find(t, points, "gh_profile_flag", map[string]string{"flag": "limited_availability"})
	if status.Fields["enabled"] != true || status.Fields["message"] != "I may be slow to respond." {
		t.Errorf("status = %v", status.Fields)
	}
	if fieldInt(t, status, "age_days") < 1500 {
		t.Errorf("status age_days = %v", status.Fields["age_days"])
	}
}

func TestAccountSponsorshipsAreDatedWhenTheMoneyMoved(t *testing.T) {
	t.Parallel()
	f := accountServer(t)
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	checkSponsorships(t, points)
	checkSponsorsListing(t, points)
	checkSponsorsTiers(t, points)
}

// checkSponsorships reads the sponsorships in both directions, each dated at
// the moment the money moved.
func checkSponsorships(t *testing.T, points []sink.Point) {
	t.Helper()
	if got := len(only(t, points, "gh_sponsorship")); got != 3 {
		t.Fatalf("got %d sponsorships, want both directions including the lapsed ones", got)
	}
	out := find(t, points, "gh_sponsorship", map[string]string{"direction": "sponsor", "sponsorable": "unifiedjs"})
	if want := time.Date(2026, 9, 3, 20, 41, 47, 0, time.UTC); !out.Time.Equal(want) {
		t.Errorf("stamped %s, want the moment the money moved %s", out.Time, want)
	}
	if fieldInt(t, out, "amount_cents") != 500 || out.Fields["one_time"] != true ||
		out.Fields["tier"] != "$5 one time" || out.Fields["active"] != true {
		t.Errorf("outgoing sponsorship = %v", out.Fields)
	}
	in := find(t, points, "gh_sponsorship", map[string]string{"direction": "maintainer", "sponsorable": "mwp4444"})
	if want := time.Date(2021, 8, 31, 6, 42, 50, 0, time.UTC); !in.Time.Equal(want) {
		t.Errorf("stamped %s, want %s", in.Time, want)
	}
	if in.Fields["active"] != false || fieldInt(t, in, "amount_cents") != 1000 {
		t.Errorf("incoming sponsorship = %v", in.Fields)
	}
	// A private sponsorship names nobody. The row still needs an identity, and
	// it must not claim a profile URL for a login it does not have.
	private := find(t, points, "gh_sponsorship", map[string]string{"sponsorable": "private"})
	if hasField(private, "url") || hasField(private, "tier") {
		t.Errorf("private sponsorship = %v", private.Fields)
	}
}

// checkSponsorsListing reads the listing, which is current state.
func checkSponsorsListing(t *testing.T, points []sink.Point) {
	t.Helper()
	listing := only(t, points, "gh_sponsors_listing")[0]
	if !listing.Time.Equal(testNow) {
		t.Errorf("the listing is current state, got %s", listing.Time)
	}
	if fieldInt(t, listing, "lifetime_received_cents") != 1000 ||
		fieldInt(t, listing, "sponsor_spend_cents") != 500 ||
		fieldInt(t, listing, "goal_target") != 10 || fieldInt(t, listing, "tiers") != 2 {
		t.Errorf("listing = %v", listing.Fields)
	}
	if listing.Fields["next_payout_date"] != "2026-10-22" || listing.Fields["has_listing"] != true {
		t.Errorf("listing = %v", listing.Fields)
	}
}

// checkSponsorsTiers reads the tiers, which are standing inventory.
func checkSponsorsTiers(t *testing.T, points []sink.Point) {
	t.Helper()
	if got := len(only(t, points, "gh_sponsors_tier")); got != 2 {
		t.Fatalf("got %d tiers, want one row each", got)
	}
	tier := find(t, points, "gh_sponsors_tier", map[string]string{"tier": "$30 a month"})
	if !tier.Time.Equal(startOfDay(testNow)) {
		t.Errorf("a tier is standing inventory anchored to the day, got %s", tier.Time)
	}
	if fieldInt(t, tier, "price_cents") != 3000 || tier.Fields["retired"] != true ||
		tier.Fields["one_time"] != false {
		t.Errorf("tier = %v", tier.Fields)
	}
	if fieldInt(t, tier, "age_days") < 1900 {
		t.Errorf("tier age_days = %v", tier.Fields["age_days"])
	}
}

// A profile with no pronouns line answers null (octocat, checked on
// 2026-09-12), and the row carries no field rather than an empty string,
// the way a url is absolute or absent.
func TestAccountPronounsAreAbsentWhenTheProfileShowsNone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		page := string(fixture(t, "graphql_account.json"))
		page = strings.Replace(page, `"pronouns": "he/him"`, `"pronouns": null`, 1)
		if !strings.Contains(page, `"pronouns": null`) {
			t.Fatal("the fixture does not carry the pronouns line the test rewrites")
		}
		_, _ = w.Write([]byte(page))
	})
	f.file("/users/octocat", "user_profile.json")
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if acc := only(t, points, "gh_account")[0]; hasField(acc, "pronouns") {
		t.Errorf("pronouns = %v, want no field at all", acc.Fields["pronouns"])
	}
}

func TestAccountGraphQLErrorIsReturned(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a User with the login of 'nobody'."}]}`))
	})
	if _, err := (Account{Login: "nobody"}).Collect(ctx(t), f.Client, testNow); err == nil {
		t.Fatal("expected the GraphQL error")
	}
}

// historyServer answers the creation probe and the per-year query from
// their fixtures, telling them apart by the query text.
func historyServer(t *testing.T, f *fixtureServer, record *[]map[string]any) {
	t.Helper()
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, vars map[string]any) {
		*record = append(*record, vars)
		if strings.Contains(query, "contributionsCollection(from:") {
			f.write(w, "graphql_history.json")
			return
		}
		f.write(w, "graphql_created_at.json")
	})
}

func TestHistoryWalksEveryPastYearFromCreation(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	historyServer(t, f, &calls)

	// Created 2024, sweep in 2026: the probe, then 2024, 2025 and the year
	// in progress.
	points, err := History{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(calls) != 4 {
		t.Fatalf("made %d GraphQL calls, want the probe plus one per year including this one", len(calls))
	}
	if calls[1]["from"] != "2024-01-01T00:00:00Z" || calls[1]["to"] != "2024-12-31T23:59:59Z" {
		t.Errorf("2024 range = %v", calls[1])
	}
	if calls[2]["from"] != "2025-01-01T00:00:00Z" || calls[2]["login"] != "octocat" {
		t.Errorf("2025 range = %v", calls[2])
	}
	// The current year runs from the first of January to now, not to a
	// December nobody has lived yet.
	if calls[3]["from"] != "2026-01-01T00:00:00Z" || calls[3]["to"] != testNow.Format(time.RFC3339) {
		t.Errorf("2026 range = %v, want January to now", calls[3])
	}

	years := only(t, points, "gh_contribution_year")
	if len(years) != 3 {
		t.Fatalf("got %d yearly totals, want 3", len(years))
	}
	y2024 := find(t, points, "gh_contribution_year", map[string]string{"year": "2024"})
	// Stamped at the end of its own year, with the days it summarizes.
	if want := time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC); !y2024.Time.Equal(want) {
		t.Errorf("2024 stamped %s, want %s", y2024.Time, want)
	}
	if fieldInt(t, y2024, "contributions") != 393 || fieldInt(t, y2024, "commits") != 300 {
		t.Errorf("2024 fields = %v", y2024.Fields)
	}
	if y2024.Fields["partial"] != false {
		t.Errorf("a past year is complete: partial = %v", y2024.Fields["partial"])
	}
	checkYearInProgress(t, points)
	checkHistoryDays(t, points)

	// The backfill fills the per-repository split of the same years, out of
	// the same query, at no extra cost.
	byRepo := only(t, points, "gh_contribution_day_repo")
	if len(byRepo) != 6 {
		t.Fatalf("got %d per-repository daily rows, want 2 per year", len(byRepo))
	}
	row := find(t, points, "gh_contribution_day_repo", map[string]string{"repo": "octocat/hello-world"})
	if row.Tags["user"] != "octocat" || row.Tags["own"] != "true" || row.Tags["private"] != "false" {
		t.Errorf("backfilled row = %v", row.Tags)
	}
	if row.Time.Year() > 2025 {
		t.Errorf("a backfilled row is dated in its own year, got %s", row.Time)
	}
}

// checkYearInProgress reads the current year's row, which is a snapshot:
// stamped at the start of the UTC day so a daily run rewrites one row, and
// marked partial so a panel comparing years knows this bar is still growing.
// A "year" row existed only for past years before, so every by-year panel
// ended last December.
func checkYearInProgress(t *testing.T, points []sink.Point) {
	t.Helper()
	y2026 := find(t, points, "gh_contribution_year", map[string]string{"year": "2026"})
	if !y2026.Time.Equal(startOfDay(testNow)) {
		t.Errorf("the current year is stamped %s, want the start of today %s", y2026.Time, startOfDay(testNow))
	}
	if y2026.Fields["partial"] != true || fieldInt(t, y2026, "contributions") != 393 {
		t.Errorf("2026 fields = %v", y2026.Fields)
	}
}

func TestHistoryFromSkipsTheProbe(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var calls []map[string]any
	historyServer(t, f, &calls)
	points, err := History{Login: "octocat", From: 2025}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0]["from"] != "2025-01-01T00:00:00Z" {
		t.Errorf("calls = %v, want 2025 and the year in progress, no probe", calls)
	}
	if len(only(t, points, "gh_contribution_year")) != 2 {
		t.Error("want two yearly totals: 2025 and the year in progress")
	}
}

// TestAccountStarListsAreADailySnapshot pins the one row the profile has for
// how the account organizes the stars it gives: one per list, stamped at the
// start of the UTC day rather than at either date the list carries, with both
// of those dates surviving as ages.
func TestAccountStarListsAreADailySnapshot(t *testing.T) {
	t.Parallel()
	f := accountServer(t)
	points, err := Account{Login: "octocat"}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	lists := only(t, points, "gh_star_list")
	if len(lists) != 2 {
		t.Fatalf("got %d star lists, want the two the fixture carries", len(lists))
	}
	day := startOfDay(testNow)
	hosted := find(t, points, "gh_star_list", map[string]string{"list": "self-hosted"})
	if !hosted.Time.Equal(day) {
		t.Errorf("a list is a daily snapshot and is stamped %s, got %s", day, hosted.Time)
	}
	if hosted.Tags["user"] != "octocat" {
		t.Errorf("tags = %v", hosted.Tags)
	}
	if fieldInt(t, hosted, "items") != 6 || hosted.Fields["private"] != false ||
		hosted.Fields["name"] != "Self-Hosted" || fieldInt(t, hosted, "lists") != 1 {
		t.Errorf("self-hosted = %v", hosted.Fields)
	}
	// Made on 2024-02-03 and last added to on 2026-09-01, against a sweep on
	// 2026-09-08: the ages are what the two dates become.
	if got := fieldInt(t, hosted, "age_days"); got != 948 {
		t.Errorf("age_days = %d, want 948", got)
	}
	if got := fieldInt(t, hosted, "days_since_add"); got != 7 {
		t.Errorf("days_since_add = %d, want 7", got)
	}
	if hosted.Fields["url"] != "https://github.com/stars/octocat/lists/self-hosted" {
		t.Errorf("url = %v, want the list's own page", hosted.Fields["url"])
	}

	private := find(t, points, "gh_star_list", map[string]string{"list": "to-check"})
	if private.Fields["private"] != true || fieldInt(t, private, "items") != 1 {
		t.Errorf("to-check = %v", private.Fields)
	}
}

// checkCalendarDays reads the sweep's calendar: one point per day at that
// day's date, with the count and the shade.
func checkCalendarDays(t *testing.T, points []sink.Point) {
	t.Helper()
	days := only(t, points, "gh_contribution_day")
	if len(days) != 5 {
		t.Fatalf("got %d calendar days, want 5", len(days))
	}
	if want := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC); !days[0].Time.Equal(want) || fieldInt(t, days[0], "contributions") != 0 {
		t.Errorf("first day = %s %v", days[0].Time, days[0].Fields)
	}
	if want := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC); !days[4].Time.Equal(want) || fieldInt(t, days[4], "contributions") != 9 {
		t.Errorf("last day = %s %v", days[4].Time, days[4].Fields)
	}
	// The shade of each square is GitHub's quartile, not a function of the
	// count, so it travels with the row as the 0 to 4 the profile draws.
	for i, want := range []int64{0, 2, 3, 1, 4} {
		if got := fieldInt(t, days[i], "level"); got != want {
			t.Errorf("day %d level = %d, want %d", i, got, want)
		}
	}
}

// checkHistoryDays reads the backfilled calendar: five days per year, and a
// past year's squares carry their shade too, from the same query. The one
// fixture answers every year, so the day is there once per year.
func checkHistoryDays(t *testing.T, points []sink.Point) {
	t.Helper()
	days := only(t, points, "gh_contribution_day")
	if len(days) != 15 {
		t.Errorf("got %d days, want 5 per year", len(days))
	}
	for _, d := range days {
		if d.Time.Equal(time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)) && fieldInt(t, d, "level") != 4 {
			t.Errorf("2025-01-03 is FOURTH_QUARTILE in the fixture, got level %v", d.Fields["level"])
		}
	}
}
