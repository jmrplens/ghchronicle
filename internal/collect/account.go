package collect

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Account collects everything about the person rather than a repository.
//
// It is one GraphQL query plus one REST request. Measured against the live
// API, asking for the full 366-day contribution calendar, every contribution
// total, the per-repository breakdown, the pinned items, the profile flags and
// the whole sponsors block cost exactly one point out of 5000. The same data
// over REST would be dozens of calls and would not include the calendar at
// all, which exists nowhere else. The one REST request is for the follow
// count, which GraphQL gets wrong; see Collect.
type Account struct {
	Login string
}

const accountQuery = `
query($login: String!) {
  user(login: $login) {
    login createdAt
    followers { totalCount }
    following { totalCount }
    starredRepositories { totalCount }
    watching { totalCount }
    gists { totalCount }
    repositories(privacy: PUBLIC) { totalCount }
    projectsV2 { totalCount }
    packages { totalCount }
    sponsoring { totalCount }
    sponsors { totalCount }
    pronouns
    isHireable isDeveloperProgramMember isCampusExpert
    isGitHubStar isBountyHunter isEmployee
    status { message createdAt indicatesLimitedAvailability }
    pinnedItems(first: 6) {
      nodes {
        __typename
        ... on Repository { nameWithOwner stargazerCount pushedAt }
        ... on Gist { name }
      }
    }
    hasSponsorsListing
    monthlyEstimatedSponsorsIncomeInCents
    estimatedNextSponsorsPayoutInCents
    totalSponsorshipAmountAsSponsorInCents
    lifetimeReceivedSponsorshipValues(first: 100) {
      totalCount
      nodes { amountInCents }
    }
    sponsorsListing {
      name isPublic createdAt nextPayoutDate
      activeGoal { kind title targetValue percentComplete }
      tiers(first: 50) {
        totalCount
        nodes { name monthlyPriceInCents isOneTime createdAt adminInfo { isRetired } }
      }
    }
    sponsorshipsAsSponsor(first: 100, activeOnly: false) {
      nodes {
        createdAt isActive isOneTimePayment privacyLevel
        sponsorable { ... on User { login } ... on Organization { login } }
        tier { name monthlyPriceInCents }
      }
    }
    sponsorshipsAsMaintainer(first: 100, includePrivate: true, activeOnly: false) {
      nodes {
        createdAt isActive isOneTimePayment privacyLevel
        sponsorEntity { ... on User { login } ... on Organization { login } }
        tier { name monthlyPriceInCents }
      }
    }
    lists(first: 100) {
      totalCount
      nodes { name slug isPrivate createdAt lastAddedAt items { totalCount } }
    }
    contributionsCollection {
      totalCommitContributions
      totalIssueContributions
      totalPullRequestContributions
      totalPullRequestReviewContributions
      totalRepositoryContributions
      restrictedContributionsCount
      contributionCalendar {
        totalContributions
        weeks { contributionDays { date contributionCount contributionLevel } }
      }
      totalRepositoriesWithContributedCommits
      totalRepositoriesWithContributedIssues
      totalRepositoriesWithContributedPullRequests
      totalRepositoriesWithContributedPullRequestReviews
      commitContributionsByRepository(maxRepositories: 100) {
        repository { nameWithOwner isPrivate }
        contributions(first: 100) {
          totalCount
          nodes { occurredAt commitCount }
        }
      }
      issueContributionsByRepository(maxRepositories: 100) {
        repository { nameWithOwner }
        contributions { totalCount }
      }
      pullRequestContributionsByRepository(maxRepositories: 100) {
        repository { nameWithOwner }
        contributions { totalCount }
      }
      pullRequestReviewContributionsByRepository(maxRepositories: 100) {
        repository { nameWithOwner }
        contributions { totalCount }
      }
      repositoryContributions(first: 100) {
        totalCount
        nodes { occurredAt repository { nameWithOwner isFork isPrivate } }
      }
    }
  }
}`

// calendarSquare is one square of the contribution calendar, as both the
// account query and the history query read it.
type calendarSquare struct {
	Date  string `json:"date"`
	Count int    `json:"contributionCount"`
	// Level is the square's shade, which is GitHub's own quartile of the
	// year and not a function of the count alone: on 2026-09-12 this
	// account's 83 contributions of the 10th were SECOND_QUARTILE and 52
	// were the same. It is the one number a grid drawn from these rows
	// cannot compute for itself.
	Level string `json:"contributionLevel"`
}

// fields is the row: the count, and the shade as GitHub's 0 to 4.
func (d calendarSquare) fields() map[string]any {
	return map[string]any{"contributions": d.Count, "level": contributionLevel(d.Level)}
}

// contributionLevel is the calendar's shade as the number the profile draws
// it with: 0 for a blank square, 1 to 4 for the four quartiles. A value this
// table does not know reads as 0 rather than as a wrong shade.
func contributionLevel(level string) int {
	switch level {
	case "FIRST_QUARTILE":
		return 1
	case "SECOND_QUARTILE":
		return 2
	case "THIRD_QUARTILE":
		return 3
	case "FOURTH_QUARTILE":
		return 4
	}
	return 0
}

// contributionRepos is the shape of three of the four
// ...ContributionsByRepository fields: a repository and a count. They come
// back from the one query that was already being paid for.
type contributionRepos []struct {
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Contributions count `json:"contributions"`
}

// commitContributionRepos is the fourth, asked for the daily rows inside it.
//
// The sub-connection is one node per repository per day, and it is what turns
// the account-wide green calendar into "which repository was that green
// square". Asking for it changed nothing about the cost: measured against the
// live API on 2026-09-10, cost 1, 39 repositories, 534 daily rows, hasNextPage
// false on every one.
//
// The repository's own owner comes back as the prefix of nameWithOwner in
// every one of those 39, so ownership is cut out of the name rather than
// asked for a second time.
type commitContributionRepos []struct {
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
		IsPrivate     bool   `json:"isPrivate"`
	} `json:"repository"`
	Contributions struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			// A string, not a time.Time, on purpose. See commitDayPoints.
			OccurredAt  string `json:"occurredAt"`
			CommitCount int    `json:"commitCount"`
		} `json:"nodes"`
	} `json:"contributions"`
}

// repoTotal is one repository and one number, which is all
// gh_contribution_repo needs from two differently shaped connections.
type repoTotal struct {
	name  string
	total int
	// days is how many distinct days the breakdown returned, and only the
	// commit connection is asked for it. The daily rows stop at a hundred,
	// the largest page GraphQL serves, so a repository with commits on more
	// than a hundred days in the window would lose the tail with nothing to
	// show for it. This is what makes that ceiling visible.
	days int
	// dated is how many of those commits reached a dated row.
	//
	// contributions.totalCount counts commits and the nodes count days:
	// measured on all 39 repositories of this account, the node counts sum
	// exactly to totalCount. So total minus dated is precisely what the
	// hundred-day page dropped, which is zero today and would otherwise be a
	// silent loss on the backfill, where the window is a whole year.
	dated int
}

func (rs contributionRepos) totals() []repoTotal {
	out := make([]repoTotal, 0, len(rs))
	for _, r := range rs {
		out = append(out, repoTotal{name: r.Repository.NameWithOwner, total: r.Contributions.TotalCount})
	}
	return out
}

func (rs commitContributionRepos) totals() []repoTotal {
	out := make([]repoTotal, 0, len(rs))
	for _, r := range rs {
		dated := 0
		for _, n := range r.Contributions.Nodes {
			dated += n.CommitCount
		}
		out = append(out, repoTotal{
			name:  r.Repository.NameWithOwner,
			total: r.Contributions.TotalCount,
			days:  len(r.Contributions.Nodes),
			dated: dated,
		})
	}
	return out
}

// sponsorParty is the account on the other side of a sponsorship. It is an
// interface in the schema, so the login arrives through an inline fragment and
// is null when the sponsor chose to stay private.
type sponsorParty struct {
	Login string `json:"login"`
}

// sponsorship is one movement of money, in either direction. The two
// connections differ only in the name of the field naming the other party.
type sponsorship struct {
	CreatedAt        time.Time `json:"createdAt"`
	IsActive         bool      `json:"isActive"`
	IsOneTimePayment bool      `json:"isOneTimePayment"`
	PrivacyLevel     string    `json:"privacyLevel"`
	Tier             *struct {
		Name                string `json:"name"`
		MonthlyPriceInCents int    `json:"monthlyPriceInCents"`
	} `json:"tier"`
	Sponsorable   *sponsorParty `json:"sponsorable"`
	SponsorEntity *sponsorParty `json:"sponsorEntity"`
}

// other names the account that is not ours, whichever direction this is.
func (s sponsorship) other() string {
	for _, party := range []*sponsorParty{s.Sponsorable, s.SponsorEntity} {
		if party != nil && party.Login != "" {
			return party.Login
		}
	}
	// A PRIVATE sponsorship hides the party entirely, and an empty tag value
	// would be dropped by the sink, taking the row's identity with it.
	return "private"
}

type sponsorships struct {
	Nodes []sponsorship `json:"nodes"`
}

// starList is one of the lists the account files its stars into.
//
// The slug is the identity: it is what the list's page is addressed by, and
// the display name is free text. Whether GitHub keeps the slug when a list is
// renamed is not verified (a rename is a mutation, and every list measured
// still carries the slug of its name), so a renamed list may become a new
// series. A list has two dates and neither is the date of the row: createdAt
// is when it was made and lastAddedAt when a star last went into it, and both
// travel as ages so the row can be a daily snapshot beside the tiers and the
// pins.
type starList struct {
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	IsPrivate   bool      `json:"isPrivate"`
	CreatedAt   time.Time `json:"createdAt"`
	LastAddedAt time.Time `json:"lastAddedAt"`
	Items       count     `json:"items"`
}

// accountUser is the profile as the one account query returns it.
//
// It is a named type rather than an anonymous one so that each builder below
// can take the whole answer and render the part of it that is its own.
type accountUser struct {
	Login               string    `json:"login"`
	CreatedAt           time.Time `json:"createdAt"`
	Followers           count     `json:"followers"`
	Following           count     `json:"following"`
	StarredRepositories count     `json:"starredRepositories"`
	Watching            count     `json:"watching"`
	Gists               count     `json:"gists"`
	Repositories        count     `json:"repositories"`
	ProjectsV2          count     `json:"projectsV2"`
	Packages            count     `json:"packages"`
	Sponsoring          count     `json:"sponsoring"`
	Sponsors            count     `json:"sponsors"`

	// Pronouns is the profile's pronouns line, "he/him", or null when the
	// profile shows none (octocat, checked on 2026-09-12).
	Pronouns                 string `json:"pronouns"`
	IsHireable               bool   `json:"isHireable"`
	IsDeveloperProgramMember bool   `json:"isDeveloperProgramMember"`
	IsCampusExpert           bool   `json:"isCampusExpert"`
	IsGitHubStar             bool   `json:"isGitHubStar"`
	IsBountyHunter           bool   `json:"isBountyHunter"`
	IsEmployee               bool   `json:"isEmployee"`
	Status                   *struct {
		Message   string    `json:"message"`
		CreatedAt time.Time `json:"createdAt"`
		Limited   bool      `json:"indicatesLimitedAvailability"`
	} `json:"status"`
	PinnedItems struct {
		Nodes []struct {
			TypeName      string    `json:"__typename"`
			NameWithOwner string    `json:"nameWithOwner"`
			Name          string    `json:"name"`
			Stars         int       `json:"stargazerCount"`
			PushedAt      time.Time `json:"pushedAt"`
		} `json:"nodes"`
	} `json:"pinnedItems"`

	HasSponsorsListing bool `json:"hasSponsorsListing"`
	MonthlyIncome      int  `json:"monthlyEstimatedSponsorsIncomeInCents"`
	NextPayout         int  `json:"estimatedNextSponsorsPayoutInCents"`
	SponsorSpend       int  `json:"totalSponsorshipAmountAsSponsorInCents"`
	LifetimeReceived   struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			AmountInCents int `json:"amountInCents"`
		} `json:"nodes"`
	} `json:"lifetimeReceivedSponsorshipValues"`
	SponsorsListing          *sponsorsListing `json:"sponsorsListing"`
	SponsorshipsAsSponsor    sponsorships     `json:"sponsorshipsAsSponsor"`
	SponsorshipsAsMaintainer sponsorships     `json:"sponsorshipsAsMaintainer"`
	Lists                    struct {
		TotalCount int        `json:"totalCount"`
		Nodes      []starList `json:"nodes"`
	} `json:"lists"`
	Contributions contributionsCollection `json:"contributionsCollection"`
}

// sponsorsListing is the sponsors page an account publishes. It is null until
// somebody opens one, which is not the same fact as a listing nobody sponsors.
type sponsorsListing struct {
	Name           string    `json:"name"`
	IsPublic       bool      `json:"isPublic"`
	CreatedAt      time.Time `json:"createdAt"`
	NextPayoutDate string    `json:"nextPayoutDate"`
	ActiveGoal     *struct {
		Kind            string `json:"kind"`
		Title           string `json:"title"`
		TargetValue     int    `json:"targetValue"`
		PercentComplete int    `json:"percentComplete"`
	} `json:"activeGoal"`
	Tiers struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Name      string    `json:"name"`
			Price     int       `json:"monthlyPriceInCents"`
			IsOneTime bool      `json:"isOneTime"`
			CreatedAt time.Time `json:"createdAt"`
			AdminInfo *struct {
				IsRetired bool `json:"isRetired"`
			} `json:"adminInfo"`
		} `json:"nodes"`
	} `json:"tiers"`
}

// contributionsCollection is the last twelve months as GraphQL totals them:
// what was done, how many repositories it was done in, and the calendar behind
// both.
type contributionsCollection struct {
	Commits      int `json:"totalCommitContributions"`
	Issues       int `json:"totalIssueContributions"`
	PullRequests int `json:"totalPullRequestContributions"`
	Reviews      int `json:"totalPullRequestReviewContributions"`
	Repositories int `json:"totalRepositoryContributions"`
	Restricted   int `json:"restrictedContributionsCount"`
	Calendar     struct {
		Total int `json:"totalContributions"`
		Weeks []struct {
			Days []calendarSquare `json:"contributionDays"`
		} `json:"weeks"`
	} `json:"contributionCalendar"`
	ReposWithCommits int `json:"totalRepositoriesWithContributedCommits"`
	ReposWithIssues  int `json:"totalRepositoriesWithContributedIssues"`
	ReposWithPulls   int `json:"totalRepositoriesWithContributedPullRequests"`
	ReposWithReviews int `json:"totalRepositoriesWithContributedPullRequestReviews"`

	ByRepository        commitContributionRepos `json:"commitContributionsByRepository"`
	IssuesByRepository  contributionRepos       `json:"issueContributionsByRepository"`
	PullsByRepository   contributionRepos       `json:"pullRequestContributionsByRepository"`
	ReviewsByRepository contributionRepos       `json:"pullRequestReviewContributionsByRepository"`

	RepositoryContributions struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			OccurredAt time.Time `json:"occurredAt"`
			Repository struct {
				NameWithOwner string `json:"nameWithOwner"`
				IsFork        bool   `json:"isFork"`
				IsPrivate     bool   `json:"isPrivate"`
			} `json:"repository"`
		} `json:"nodes"`
	} `json:"repositoryContributions"`
}

func (a Account) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	var res struct {
		User accountUser `json:"user"`
	}
	if err := c.GraphQL(ctx, accountQuery, map[string]any{"login": a.Login}, &res); err != nil {
		return nil, err
	}
	u := &res.User
	base := map[string]string{"user": u.Login}

	points := []sink.Point{
		accountPoint(u, a.followingCount(ctx, c, u), packageCount(ctx, c, u), base, now),
		contributionsTotalPoint(u, base, now),
	}
	points = append(points, contributionDayPoints(u, base)...)
	points = append(points, contributionRepoPoints(u, base, now)...)
	points = append(points, commitDayPoints(u.Contributions.ByRepository, u.Login)...)
	points = append(points, repoCreatedPoints(u, base)...)
	points = append(points, pinnedItemPoints(u, base, now)...)
	points = append(points, profileFlagPoints(u, base, now)...)
	points = append(points, sponsorshipPoints(u, base)...)
	points = append(points, sponsorsListingPoints(u, base, now)...)
	points = append(points, starListPoints(u, base, now)...)
	return points, nil
}

// starListPoints is the lists the account files its stars into, one row per
// list with how many stars it holds.
//
// gh_star_given records every star the account gave and gh_account.starred
// counts them; neither says how the account organizes them, which is what a
// list is. It rides in the query that was already being paid for: measured on
// 2026-09-11, eleven lists with their item counts added nothing to a cost of
// one.
//
// A daily snapshot, like the tiers beside it, and for the same reason: a list
// carries a creation date and the date a star last went into it, and dating
// the row at either would put a list made in 2024 outside every dashboard
// range, where it would read as no lists at all. Both dates survive as ages.
func starListPoints(u *accountUser, base map[string]string, now time.Time) []sink.Point {
	day := now.UTC().Truncate(24 * time.Hour)
	var points []sink.Point
	for _, l := range u.Lists.Nodes {
		if l.Slug == "" {
			continue
		}
		fields := withURL(map[string]any{
			"lists": 1, "items": l.Items.TotalCount, "private": l.IsPrivate,
			"name": l.Name,
		}, githubPage("stars", u.Login, "lists", l.Slug))
		if !l.CreatedAt.IsZero() {
			fields["age_days"] = int(now.Sub(l.CreatedAt).Hours() / 24)
		}
		if !l.LastAddedAt.IsZero() {
			fields["days_since_add"] = int(now.Sub(l.LastAddedAt).Hours() / 24)
		}
		points = append(points, sink.Point{
			Measurement: "gh_star_list",
			Tags:        merge(base, map[string]string{"list": l.Slug}),
			Fields:      fields,
			Time:        day,
		})
	}
	return points
}

// followingCount is how many accounts the profile follows, people and
// organizations both.
//
// GraphQL's User.following is a connection of User, so it counts people and
// silently drops the organizations the account follows. Measured in the same
// minute with the same token: GraphQL says 4, GET /users/{login} says 9, and
// GET /user/following lists those 9 as 4 users and 5 organizations. The
// profile's own number is the REST one, and it costs a single request out of
// five thousand an hour.
//
// Any failure of that request keeps the connection's count rather than
// returning an error. The GraphQL answer is already in hand, and the runner
// drops every point of a family whose collector fails: a flaky profile request
// would otherwise cost the sweep the contribution calendar, which is the one
// thing here that exists nowhere else. When the fallback happens, following
// equals following_users, which is the tell that the profile was not reached.
func (a Account) followingCount(ctx context.Context, c *ghapi.Client, u *accountUser) int {
	var profile struct {
		Following int `json:"following"`
	}
	if _, _, err := c.GetJSON(ctx, "/users/"+url.PathEscape(a.Login), &profile, ""); err != nil {
		return u.Following.TotalCount
	}
	return profile.Following
}

// packageCount is how many packages the account has, counted the way the
// profile family lists them, because GraphQL's User.packages does not see the
// container registry: measured on this account, the connection answers 0
// while REST lists four containers, and the 0 was published as
// gh_account.packages beside four gh_package rows. Six requests twice a day,
// and GitHub answers an unchanged list with a free 304.
//
// A failure keeps GraphQL's count, for the reason followingCount gives: the
// calendar is already in hand and a flaky listing must not cost the sweep
// the one thing that exists nowhere else. The larger of the two wins, so a
// registry that GraphQL can count is never under-counted by a listing that
// stopped halfway.
func packageCount(ctx context.Context, c *ghapi.Client, u *accountUser) int {
	total := 0
	for _, kind := range packageKinds {
		for page := 1; page <= 10; page++ {
			var pkgs []struct {
				Name string `json:"name"`
			}
			path := "/user/packages?package_type=" + kind + "&per_page=100&page=" + strconv.Itoa(page)
			if _, _, err := c.GetJSON(ctx, path, &pkgs, ""); err != nil {
				if isSkippable(err) || isPaginationLimit(err) {
					break
				}
				return max(total, u.Packages.TotalCount)
			}
			total += len(pkgs)
			if len(pkgs) < 100 {
				break
			}
		}
	}
	return max(total, u.Packages.TotalCount)
}

// accountPoint is the headline row: what the profile page shows about itself.
//
// The pronouns ride on it as a field, absent when the profile shows none. A
// field and never a tag: it is free text the owner can edit, and as a tag
// every edit would fork the account's one series.
func accountPoint(u *accountUser, following, packages int, base map[string]string, now time.Time) sink.Point {
	fields := withURL(map[string]any{
		"followers": u.Followers.TotalCount, "following": following,
		"starred": u.StarredRepositories.TotalCount, "watching": u.Watching.TotalCount,
		"following_users": u.Following.TotalCount,
		"gists":           u.Gists.TotalCount, "public_repos": u.Repositories.TotalCount,
		"projects": u.ProjectsV2.TotalCount, "packages": packages,
		"sponsoring": u.Sponsoring.TotalCount, "sponsors": u.Sponsors.TotalCount,
		"account_age_days": int(now.Sub(u.CreatedAt).Hours() / 24),
	}, githubPage(u.Login))
	setNonEmpty(fields, "pronouns", strings.TrimSpace(u.Pronouns))
	return sink.Point{
		Measurement: "gh_account",
		Tags:        base,
		Fields:      fields,
		Time:        now,
	}
}

// contributionsTotalPoint is the year's work in one row.
func contributionsTotalPoint(u *accountUser, base map[string]string, now time.Time) sink.Point {
	return sink.Point{
		Measurement: "gh_contributions_total",
		Tags:        base,
		Fields: withURL(map[string]any{
			"commits": u.Contributions.Commits, "issues": u.Contributions.Issues,
			"pull_requests": u.Contributions.PullRequests, "reviews": u.Contributions.Reviews,
			"repositories": u.Contributions.Repositories, "restricted": u.Contributions.Restricted,
			"calendar_total": u.Contributions.Calendar.Total,
			// The breadth of the work, not its volume: fifty repositories with
			// a pull request against thirty eight with a commit is a different
			// story from either number alone.
			"repos_with_commits": u.Contributions.ReposWithCommits,
			"repos_with_issues":  u.Contributions.ReposWithIssues,
			"repos_with_pulls":   u.Contributions.ReposWithPulls,
			"repos_with_reviews": u.Contributions.ReposWithReviews,
		}, githubPage(u.Login)),
		Time: now,
	}
}

// dayLayout is how GitHub writes a contribution day: a date with no time of
// day at all, which is also its whole length in calendarDay.
const dayLayout = "2006-01-02"

// contributionDayPoints is the calendar, one point per day at that day's date.
//
// This is the only place the green-squares history exists, and it is why the
// collector is worth running: rewriting the last year on every sweep is
// idempotent and repairs any gap.
func contributionDayPoints(u *accountUser, base map[string]string) []sink.Point {
	var points []sink.Point
	for _, w := range u.Contributions.Calendar.Weeks {
		for _, d := range w.Days {
			day, err := time.Parse(dayLayout, d.Date)
			if err != nil {
				continue
			}
			points = append(points, sink.Point{
				Measurement: "gh_contribution_day",
				Tags:        base,
				Fields:      withURL(d.fields(), githubPage(u.Login)),
				Time:        day,
			})
		}
	}
	return points
}

// contributionRepoPoints is work in other people's repositories: the part a
// sweep over one's own repos can never see.
//
// Four kinds, one row per repository per kind, so a repository where the only
// contribution was an issue is no longer invisible. They arrive in the query
// that was already being paid for.
func contributionRepoPoints(u *accountUser, base map[string]string, now time.Time) []sink.Point {
	var points []sink.Point
	for kind, byRepo := range map[string][]repoTotal{
		"commits": u.Contributions.ByRepository.totals(),
		"issues":  u.Contributions.IssuesByRepository.totals(),
		"pulls":   u.Contributions.PullsByRepository.totals(),
		"reviews": u.Contributions.ReviewsByRepository.totals(),
	} {
		for _, r := range byRepo {
			fields := withURL(map[string]any{"contributions": r.total},
				githubPage(r.name))
			if kind == "commits" {
				// The field this measurement has always carried. Kept so a
				// dashboard and a year of history do not have to be rewritten.
				fields["commits"] = r.total
				fields["days"] = r.days
				fields["commits_dated"] = r.dated
			}
			points = append(points, sink.Point{
				Measurement: "gh_contribution_repo",
				Tags: merge(base, map[string]string{
					"repo": r.name, "kind": kind,
				}),
				Fields: fields,
				Time:   now,
			})
		}
	}
	return points
}

// commitDayPoints is the green calendar split per repository per day.
//
// gh_contribution_day says how much green there was on a day; this says which
// repository it came from, and it is the only surface that sees the private
// and third-party repositories a per-repository sweep never touches. Measured
// on this account: 8 of the 39 are private and 6 belong to somebody else, and
// the daily counts sum exactly to totalCommitContributions.
//
// The date is taken from the string rather than from a parsed time on purpose.
// occurredAt is a local midnight rendered in UTC, not UTC midnight: measured
// today, all 534 rows land on T07:00:00Z (479) or T08:00:00Z (55), which is
// midnight in a zone seven and eight hours behind UTC. While GitHub renders
// that as Z the two readings agree, because the day part of a Z timestamp is
// its UTC day. They part company the moment GitHub renders the offset instead:
// 2026-09-08T00:00:00+09:00 is the 8th, and truncating the instant it parses
// to gives the 7th. Reading the day part is right in both renderings, so it is
// what this does.
func commitDayPoints(repos commitContributionRepos, login string) []sink.Point {
	var points []sink.Point
	for _, r := range repos {
		name := r.Repository.NameWithOwner
		if name == "" {
			continue
		}
		// One map for every day of a repository: the tag set is the identity
		// of the series and does not vary within it.
		tags := map[string]string{
			"user": login, "repo": name,
			"private": boolTag(r.Repository.IsPrivate),
			"own":     boolTag(isOwn(name, login)),
		}
		for _, d := range r.Contributions.Nodes {
			day, err := calendarDay(d.OccurredAt)
			if err != nil {
				continue
			}
			points = append(points, sink.Point{
				Measurement: "gh_contribution_day_repo",
				Tags:        tags,
				Fields: withURL(map[string]any{"commits": d.CommitCount},
					githubPage(name)),
				Time: day,
			})
		}
	}
	return points
}

// calendarDay reads the date out of a GraphQL timestamp without parsing the
// time of day, which for occurredAt is not the day boundary it looks like.
func calendarDay(ts string) (time.Time, error) {
	if len(ts) > len(dayLayout) {
		ts = ts[:len(dayLayout)]
	}
	return time.Parse(dayLayout, ts)
}

// repoCreatedPoints is repositories created, dated when they were created.
//
// Forks and private repositories included, which is the point: those are
// exactly the ones a sweep with include_forks off never discovers.
func repoCreatedPoints(u *accountUser, base map[string]string) []sink.Point {
	var points []sink.Point
	for _, r := range u.Contributions.RepositoryContributions.Nodes {
		points = append(points, sink.Point{
			Measurement: "gh_repo_created",
			Tags: merge(base, map[string]string{
				"repo": r.Repository.NameWithOwner,
				"fork": strconv.FormatBool(r.Repository.IsFork),
			}),
			Fields: withURL(map[string]any{
				"created": 1, "private": r.Repository.IsPrivate,
			}, githubPage(r.Repository.NameWithOwner)),
			Time: r.OccurredAt,
		})
	}
	return points
}

// pinnedItemPoints is the six things the profile puts first.
//
// A pin has no date of its own, so this is current state stamped now, like the
// account row it sits beside. The order is the identity of the arrangement,
// but a repository that moves from slot two to slot three is the same pin, so
// the position travels as a field and the repository is the series.
func pinnedItemPoints(u *accountUser, base map[string]string, now time.Time) []sink.Point {
	var points []sink.Point
	for i, item := range u.PinnedItems.Nodes {
		name, kind := item.NameWithOwner, "repository"
		link := githubPage(name)
		if item.TypeName == "Gist" {
			name, kind = item.Name, "gist"
			link = pageURL("https://gist.github.com", u.Login, name)
		}
		if name == "" {
			continue
		}
		fields := withURL(map[string]any{
			"pinned": 1, "position": i + 1, "kind": kind,
		}, link)
		if kind == "repository" {
			fields["stars"] = item.Stars
			if !item.PushedAt.IsZero() {
				fields["days_since_push"] = int(now.Sub(item.PushedAt).Hours() / 24)
			}
		}
		points = append(points, sink.Point{
			Measurement: "gh_pinned_item",
			Tags:        merge(base, map[string]string{"repo": name}),
			Fields:      fields,
			Time:        now,
		})
	}
	return points
}

// profileFlagPoints is the badges the profile advertises.
//
// Seven booleans that change once in years, but the day one of them changes
// is worth being able to point at. The seventh, sponsors_listing, is whether
// the account has a Sponsors profile at all: gh_sponsors_listing.has_listing
// carries the same answer beside the money, and it is a flag here as well so
// that the closed list of what the profile advertises is in one place. The
// availability status is the eighth flag rather than a measurement of its
// own: one row, the same shape, with the message it displays.
func profileFlagPoints(u *accountUser, base map[string]string, now time.Time) []sink.Point {
	var points []sink.Point
	for flag, on := range map[string]bool{
		"hireable":          u.IsHireable,
		"developer_program": u.IsDeveloperProgramMember,
		"campus_expert":     u.IsCampusExpert,
		"github_star":       u.IsGitHubStar,
		"bounty_hunter":     u.IsBountyHunter,
		"employee":          u.IsEmployee,
		"sponsors_listing":  u.HasSponsorsListing,
	} {
		points = append(points, sink.Point{
			Measurement: "gh_profile_flag",
			Tags:        merge(base, map[string]string{"flag": flag}),
			Fields:      withURL(map[string]any{"enabled": on}, githubPage(u.Login)),
			Time:        now,
		})
	}
	st := u.Status
	if st == nil {
		return points
	}
	fields := withURL(map[string]any{
		"enabled": st.Limited, "message": st.Message,
	}, githubPage(u.Login))
	if !st.CreatedAt.IsZero() {
		fields["age_days"] = int(now.Sub(st.CreatedAt).Hours() / 24)
	}
	return append(points, sink.Point{
		Measurement: "gh_profile_flag",
		Tags:        merge(base, map[string]string{"flag": "limited_availability"}),
		Fields:      fields,
		Time:        now,
	})
}

// sponsorshipPoints is every sponsorship, stamped when it was made.
//
// This is the only dated record of the money: gh_account.sponsoring and
// gh_account.sponsors hold counts as of now and say neither when nor to whom.
// activeOnly is off on purpose, and that is what recovers a lapsed one:
// measured on this account, the maintainer side is empty by default and
// returns the 2021-08-31 sponsorship once the filter is off.
func sponsorshipPoints(u *accountUser, base map[string]string) []sink.Point {
	var points []sink.Point
	for direction, conn := range map[string]sponsorships{
		"sponsor":    u.SponsorshipsAsSponsor,
		"maintainer": u.SponsorshipsAsMaintainer,
	} {
		for _, sp := range conn.Nodes {
			if sp.CreatedAt.IsZero() {
				continue
			}
			other := sp.other()
			fields := map[string]any{
				"sponsorship": 1, "active": sp.IsActive,
				"one_time": sp.IsOneTimePayment, "privacy": sp.PrivacyLevel,
			}
			if other != "private" {
				setNonEmpty(fields, "url", githubPage(other))
			}
			if sp.Tier != nil {
				fields["tier"] = sp.Tier.Name
				fields["amount_cents"] = sp.Tier.MonthlyPriceInCents
			}
			points = append(points, sink.Point{
				Measurement: "gh_sponsorship",
				Tags: merge(base, map[string]string{
					"direction": direction, "sponsorable": other,
				}),
				Fields: fields,
				Time:   sp.CreatedAt,
			})
		}
	}
	return points
}

// sponsorsListingPoints is the listing and the tiers it offers.
//
// lifetime_received_cents exists nowhere else: sponsors is 0 and the monthly
// estimate is 0 the moment the last sponsorship lapses, and the money that did
// arrive stops being visible in any other field.
func sponsorsListingPoints(u *accountUser, base map[string]string, now time.Time) []sink.Point {
	var lifetime int
	for _, v := range u.LifetimeReceived.Nodes {
		lifetime += v.AmountInCents
	}
	listing := map[string]any{
		"has_listing":             u.HasSponsorsListing,
		"monthly_income_cents":    u.MonthlyIncome,
		"next_payout_cents":       u.NextPayout,
		"sponsor_spend_cents":     u.SponsorSpend,
		"lifetime_received_cents": lifetime,
		"sponsorships_received":   u.LifetimeReceived.TotalCount,
	}
	setNonEmpty(listing, "url", githubPage("sponsors", u.Login))
	l := u.SponsorsListing
	if l != nil {
		listing["listing_name"] = l.Name
		listing["listing_public"] = l.IsPublic
		listing["tiers"] = l.Tiers.TotalCount
		if l.NextPayoutDate != "" {
			listing["next_payout_date"] = l.NextPayoutDate
		}
		if !l.CreatedAt.IsZero() {
			listing["listing_age_days"] = int(now.Sub(l.CreatedAt).Hours() / 24)
		}
		if g := l.ActiveGoal; g != nil {
			listing["goal_kind"] = g.Kind
			listing["goal_title"] = g.Title
			listing["goal_target"] = g.TargetValue
			listing["goal_percent"] = g.PercentComplete
		}
	}
	points := []sink.Point{{
		Measurement: "gh_sponsors_listing", Tags: base, Fields: listing, Time: now,
	}}
	if l == nil {
		return points
	}
	// A tier does have a creation date, but it is standing inventory the way
	// an SSH key is, and these eight have not changed since 2021: stamped when
	// they were made they would fall outside every dashboard range and read as
	// "no tiers". Anchored to the start of the day instead, so two sweeps
	// converge on one row per tier, with the age as a field for anyone who
	// wants the creation date back.
	day := now.UTC().Truncate(24 * time.Hour)
	for _, tier := range l.Tiers.Nodes {
		if tier.Name == "" {
			continue
		}
		fields := withURL(map[string]any{
			"tiers": 1, "price_cents": tier.Price, "one_time": tier.IsOneTime,
		}, githubPage("sponsors", u.Login))
		if !tier.CreatedAt.IsZero() {
			fields["age_days"] = int(now.Sub(tier.CreatedAt).Hours() / 24)
		}
		if tier.AdminInfo != nil {
			fields["retired"] = tier.AdminInfo.IsRetired
		}
		points = append(points, sink.Point{
			Measurement: "gh_sponsors_tier",
			Tags:        merge(base, map[string]string{"tier": tier.Name}),
			Fields:      fields,
			Time:        day,
		})
	}
	return points
}

type count struct {
	TotalCount int `json:"totalCount"`
}

// History backfills the contribution calendar of past years.
//
// `contributionsCollection` defaults to the last twelve months, but it accepts
// an explicit range, and one year costs one point of a five thousand point
// budget. An account created in 2017 is nine queries away from having its
// entire green-squares history stored, which no other endpoint offers at any
// price. The same query carries the per-repository daily breakdown of that
// year at no extra cost, so the backfill fills gh_contribution_day_repo as
// well as gh_contribution_day.
//
// It is meant to run once for the past years. Each day is stamped at its own
// date, so a repeat rewrites the same rows rather than adding to them, but
// there is no reason to pay for those twice. The current year is different:
// it is asked for on every run, from the first of January to now, so a "by
// year" panel has a bar for this year rather than ending at last December.
// Its row is a snapshot of a year in progress, stamped at the start of the
// UTC day and marked partial; the newest row of the series is the value, and
// the first run of the next year replaces it with the final one dated the
// thirty-first of December.
type History struct {
	Login string
	// From is the first year to fetch. Zero means the account's creation year.
	From int
}

const historyQuery = `
query($login: String!, $from: DateTime!, $to: DateTime!) {
  user(login: $login) {
    contributionsCollection(from: $from, to: $to) {
      totalCommitContributions
      totalIssueContributions
      totalPullRequestContributions
      totalPullRequestReviewContributions
      totalRepositoryContributions
      restrictedContributionsCount
      totalRepositoriesWithContributedCommits
      totalRepositoriesWithContributedIssues
      totalRepositoriesWithContributedPullRequests
      totalRepositoriesWithContributedPullRequestReviews
      contributionCalendar {
        totalContributions
        weeks { contributionDays { date contributionCount contributionLevel } }
      }
      commitContributionsByRepository(maxRepositories: 100) {
        repository { nameWithOwner isPrivate }
        contributions(first: 100) {
          totalCount
          nodes { occurredAt commitCount }
        }
      }
    }
  }
}`

// yearContributions is one past year of a contributionsCollection, the shape
// historyQuery asks for: the totals, the calendar, and the per-repository
// commit breakdown that rides along with them.
type yearContributions struct {
	TotalCommit     int `json:"totalCommitContributions"`
	TotalIssue      int `json:"totalIssueContributions"`
	TotalPR         int `json:"totalPullRequestContributions"`
	TotalReview     int `json:"totalPullRequestReviewContributions"`
	TotalRepository int `json:"totalRepositoryContributions"`
	Restricted      int `json:"restrictedContributionsCount"`
	ReposCommits    int `json:"totalRepositoriesWithContributedCommits"`
	ReposIssues     int `json:"totalRepositoriesWithContributedIssues"`
	ReposPulls      int `json:"totalRepositoriesWithContributedPullRequests"`
	ReposReviews    int `json:"totalRepositoriesWithContributedPullRequestReviews"`
	Calendar        struct {
		TotalContributions int `json:"totalContributions"`
		Weeks              []struct {
			Days []calendarSquare `json:"contributionDays"`
		} `json:"weeks"`
	} `json:"contributionCalendar"`
	// The per-repository daily breakdown of a past year, in the same query,
	// for the same one point: measured against the live API, 2023 came back
	// cost 1 with its 32 daily rows summing exactly to
	// totalCommitContributions.
	ByRepository commitContributionRepos `json:"commitContributionsByRepository"`
}

func (h History) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	first := h.From
	if first == 0 {
		// One probe to find out when the account was created, so this does not
		// spend a query per year on years that predate it.
		var who struct {
			User struct {
				CreatedAt time.Time `json:"createdAt"`
			} `json:"user"`
		}
		q := `query($login: String!) { user(login: $login) { createdAt } }`
		if err := c.GraphQL(ctx, q, map[string]any{"login": h.Login}, &who); err != nil {
			return nil, err
		}
		first = who.User.CreatedAt.Year()
	}

	var points []sink.Point
	for year := first; year <= now.Year(); year++ {
		from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(year, 12, 31, 23, 59, 59, 0, time.UTC)
		// The year in progress ends now, not in December: GitHub answers a
		// range that runs into the future, but the row would then claim a
		// window nobody has lived yet.
		partial := year == now.Year()
		stamp := to
		if partial {
			to = now.UTC()
			stamp = now.UTC().Truncate(24 * time.Hour)
		}
		var res struct {
			User struct {
				ContributionsCollection yearContributions `json:"contributionsCollection"`
			} `json:"user"`
		}
		vars := map[string]any{
			"login": h.Login,
			"from":  from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		}
		if err := c.GraphQL(ctx, historyQuery, vars, &res); err != nil {
			return points, err
		}
		cc := res.User.ContributionsCollection
		points = append(points, commitDayPoints(cc.ByRepository, h.Login)...)
		for _, w := range cc.Calendar.Weeks {
			for _, d := range w.Days {
				day, err := time.Parse(dayLayout, d.Date)
				if err != nil {
					continue
				}
				points = append(points, sink.Point{
					Measurement: "gh_contribution_day",
					Tags:        map[string]string{"user": h.Login},
					Fields:      withURL(d.fields(), githubPage(h.Login)),
					Time:        day,
				})
			}
		}
		// One yearly total, stamped at the end of its own year so it sits with
		// the days it summarizes rather than with today; the year in progress
		// is stamped at the start of today, a snapshot that the next day's
		// run replaces.
		points = append(points, sink.Point{
			Measurement: "gh_contribution_year",
			Tags:        map[string]string{"user": h.Login, "year": strconv.Itoa(year)},
			Fields: map[string]any{
				"contributions": cc.Calendar.TotalContributions,
				"commits":       cc.TotalCommit, "issues": cc.TotalIssue,
				"pull_requests": cc.TotalPR, "reviews": cc.TotalReview,
				"repositories": cc.TotalRepository, "restricted": cc.Restricted,
				"repos_with_commits": cc.ReposCommits, "repos_with_issues": cc.ReposIssues,
				"repos_with_pulls": cc.ReposPulls, "repos_with_reviews": cc.ReposReviews,
				// Whether the year is still being written. A panel comparing
				// years can grey this bar out; a panel reading "this year so
				// far" can filter on it.
				"partial": partial,
			},
			Time: stamp,
		})
	}
	return points, nil
}
