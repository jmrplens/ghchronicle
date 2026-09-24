package collect

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Stargazers reconstructs the star curve from its beginning.
//
// The stargazers endpoint returns a `starred_at` per user when asked with the
// star media type, so the entire history is available on first run: a chart
// that goes back years, not one that starts the day the collector was
// installed. Every star is written at the instant it was given.
//
// This walk is the history, not the day to day. A repository is read whole the
// first time it is seen, and again in a backfill; after that an ordinary sweep
// never comes here, because the newest hundred stars of every repository
// already walked ride in one GraphQL query per ten repositories. The batch is
// in internal/run/audience.go, and internal/run/runner.go decides which of the
// two a repository gets. Full off, which only a backfill of a repository
// already walked produces, reads the last page alone, where new stars land.
type Stargazers struct {
	// Full forces a complete walk. The runner sets it on first sight of a
	// repository and then leaves it off.
	Full bool
}

const starAccept = "application/vnd.github.star+json"

var lastPageRe = regexp.MustCompile(`[?&]page=(\d+)>; rel="last"`)

// starRow is one entry of the stargazer list, read with the star+json media
// type so that it carries the date the star was given.
type starRow struct {
	StarredAt time.Time `json:"starred_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (s Stargazers) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	path := "/repos/" + repo.FullName + "/stargazers?per_page=100"

	var first []starRow
	link, _, err := c.GetJSON(ctx, path, &first, starAccept)
	if err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	later, err := s.pagesAfterFirst(ctx, c, path, link)
	if err != nil {
		return nil, err
	}
	return starPoints(append([][]starRow{first}, later...), base,
		githubPage(repo.FullName, "stargazers")), nil
}

// pagesAfterFirst reads whatever of the stargazer list comes after page one.
//
// A full walk takes every page; an ordinary sweep takes only the last, which
// is where new stars arrive. Either way the page numbers come from the Link
// header rather than from looping until a page comes back empty, which would
// cost one wasted call per repository.
func (s Stargazers) pagesAfterFirst(ctx context.Context, c *ghapi.Client, path, link string) ([][]starRow, error) {
	m := lastPageRe.FindStringSubmatch(link)
	if !s.Full {
		if m == nil {
			return nil, nil
		}
		var page []starRow
		if _, _, err := c.GetJSON(ctx, path+"&page="+m[1], &page, starAccept); err != nil {
			if isSkippable(err) {
				// The newest stars are a bonus on top of the first page, so a
				// page that is not there costs nothing.
				return nil, nil
			}
			return nil, err
		}
		return [][]starRow{page}, nil
	}
	last := 1
	if m != nil {
		last, _ = strconv.Atoi(m[1])
	}
	var pages [][]starRow
	for p := 2; p <= last; p++ {
		var page []starRow
		if _, _, err := c.GetJSON(ctx, path+"&page="+strconv.Itoa(p), &page, starAccept); err != nil {
			if isSkippable(err) {
				break
			}
			return nil, err
		}
		pages = append(pages, page)
	}
	return pages, nil
}

// starFields is one star's row: the repository's stargazer list, and the page
// of the person who gave it. Either is left out when there is none, so a row
// carries only the pages that exist.
func starFields(stargazers, user string) map[string]any {
	fields := withURL(map[string]any{"starred": 1}, stargazers)
	setNonEmpty(fields, "user_url", user)
	return fields
}

// starPoints stamps each star at the moment it was given, so re-reading the
// list rewrites the same rows rather than adding to them.
func starPoints(pages [][]starRow, base map[string]string, stargazers string) []sink.Point {
	var points []sink.Point
	for _, page := range pages {
		for _, st := range page {
			if st.StarredAt.IsZero() {
				continue
			}
			points = append(points, sink.Point{
				Measurement: "gh_star",
				Tags:        merge(base, map[string]string{"user": st.User.Login}),
				// Everywhere else in this tree `url` is the page of the thing
				// the row is about, and this row is about a star on a
				// repository, so it is the stargazer list. GitHub's own
				// repository page links there as stargazersPath, and it
				// answers 404 to a signed-out reader, as /watchers does. The
				// person is the other half of the row and has a page of their
				// own, which used to be what `url` held.
				Fields: starFields(stargazers, githubPage(st.User.Login)),
				Time:   st.StarredAt, // when it was given, not when we looked
			})
		}
	}
	return points
}
