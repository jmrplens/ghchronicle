package collect

import (
	"context"
	"strconv"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// StarHistory reads how many of a repository's stars were given on each day
// of its life, from GitHub's daily star history, and writes them as
// gh_star_day: one row per repository and day.
//
// It is read for every repository the sweep collects, whatever the token may
// see of it. Since July 2026 GitHub serves the stargazer list only to a
// repository's admins and collaborators, so for anyone else Stargazers finds
// a 404 and the GraphQL batch an empty connection, and gh_star has nothing to
// say. The history answers anyone who can read the repository, which is why
// it, and not gh_star, is what the dated star counts of every store that
// keeps rows are drawn from; Prometheus skips it and counts gh_star. gh_star
// stays as the names, where GitHub still gives them.
//
// What the endpoint answers, measured on 2026-09-24 and 2026-09-25 against
// every starred repository of the author's account, 440 stars:
//
//   - thirty weeks a page, newest first, which is both per_page's default
//     and its cap (a smaller one is honored, a larger one cut to thirty),
//     and an empty array past the last page;
//   - each week labeled Sunday 00:00 UTC with its seven days Sunday first,
//     and the days themselves America/Los_Angeles calendar days: bucketed that
//     way every star agreed with its starred_at, bucketed by UTC 38 of 127
//     days of one repository did not;
//   - the stars of the people who star the repository today, on the day each
//     of them starred, so an unstar takes a star off the day it was given;
//   - weeks back to the one the repository was created in, not to its first
//     star, zero weeks included;
//   - no Link header on a 304, which is why the walk never reads one.
type StarHistory struct {
	// Walk bounds the pages. Zero is page one, the newest thirty weeks, which
	// is what a sweep re-reads; Unbounded reads back to the repository's
	// first week; Since stops once a page reaches back past it.
	Walk Walk
}

// starHistoryWeeks is how many weeks a page of the history holds. Thirty is
// the default and the cap: a smaller per_page is honored and a larger one is
// cut to thirty. The walk sends none, so a page shorter than this is the last
// one.
const starHistoryWeeks = 30

// starWeek is one week of the history. Every field is written back as it was
// read, which the ETag cache relies on to answer a 304 with what the 200 did.
type starWeek struct {
	Week  int64 `json:"week"`
	Total int   `json:"total"`
	Days  []int `json:"days"`
}

func (s StarHistory) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	points, _, err := s.Read(ctx, c, repo, now)
	return points, err
}

// Read is Collect that also reports whether the walk was complete: whether it
// stopped where the history or the walk's own bound ends, on a short or empty
// page, at GitHub's paging ceiling, past Since, or at the page cap.
//
// A walk stopped by an answer that says there is nothing here keeps its rows
// and returns no error, the same as Collect, but is not complete, and the
// runner records a history as read whole only on a complete walk. On page one
// that answer is a repository with no history to read, which is every one of
// them on a GitHub Enterprise Server that does not serve the endpoint; left
// unrecorded, a server that starts serving it is read whole the first time it
// does. Further in it is not where a history ends, since the history ends on
// a short page, an empty one or a 422: a 403 or a 429 there is a secondary
// limit GitHub sent without the headers that would name it, a 404 a
// repository that went away mid-walk. Recorded, either would leave the older
// weeks unread until somebody ran a backfill.
func (s StarHistory) Read(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) (points []sink.Point, complete bool, err error) {
	base := repoTags(repo.Owner, repo.Name)
	path := repoPathPrefix + repo.FullName + "/stargazers/history"
	most := s.Walk.limit(1)
	for page := 1; page <= most; page++ {
		url := path
		if page > 1 {
			url += "?page=" + strconv.Itoa(page)
		}
		// The default media type every time: the ETag varies with Accept,
		// so asking another way would never be answered 304.
		var weeks []starWeek
		if _, _, err = c.GetJSON(ctx, url, &weeks, ""); err != nil {
			switch {
			case isPaginationLimit(err):
				// As far as GitHub pages, which is the end of the walk.
				return points, true, nil
			case isSkippable(err):
				// The pages already read are as true as they were.
				return points, false, nil
			}
			return points, false, err
		}
		points = append(points, starDays(weeks, base, page == 1, now)...)
		if len(weeks) < starHistoryWeeks || s.Walk.past(oldestWeek(weeks)) {
			return points, true, nil
		}
	}
	return points, true, nil
}

// starDays turns weeks of the history into one row per day.
//
// Each day is stamped at 00:00 UTC of the calendar date GitHub counted it
// under, anchored to the week the API labeled rather than to the clock, so a
// sweep on Tuesday and one on Friday write the same rows. The date is
// GitHub's Pacific one, stamped the way gh_contribution_day stamps its
// days, which needs no time zone database and keeps the two lined up.
//
// Zeros only on the first page. That page is what every sweep reads again,
// and an unstar there has to take a day back down to nothing, which only a
// row that is written can do. Older pages are read once and on a backfill,
// and writing their zeros too would be a row per day back to the day the
// repository was created: in InfluxDB 3 Core, which writes a file per day
// partition per request and never compacts, a checkpointed backfill would
// open thousands of them for every repository to say nothing.
//
// No day after now: the current week's later days are placeholders GitHub
// fills with zero, and a row dated in the future would stand in every
// dashboard until the day came.
func starDays(weeks []starWeek, base map[string]string, zeros bool, now time.Time) []sink.Point {
	var points []sink.Point
	for _, w := range weeks {
		// Truncated so that a label moved off midnight still lands on its
		// own date rather than on an hour of it.
		sunday := time.Unix(w.Week, 0).UTC().Truncate(24 * time.Hour)
		for i, stars := range w.Days {
			day := sunday.AddDate(0, 0, i)
			if day.After(now) || (stars == 0 && !zeros) {
				continue
			}
			points = append(points, sink.Point{
				Measurement: "gh_star_day",
				Tags:        base,
				Fields:      map[string]any{"stars": stars},
				Time:        day,
			})
		}
	}
	return points
}

// oldestWeek is where a page reaches back to. The page is served newest
// first, and the minimum is taken anyway, so nothing depends on that order.
func oldestWeek(weeks []starWeek) time.Time {
	var oldest time.Time
	for _, w := range weeks {
		if at := time.Unix(w.Week, 0).UTC(); oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest
}
