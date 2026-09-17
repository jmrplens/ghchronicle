// Package collect turns GitHub API responses into dated points.
package collect

import (
	"context"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// repoPathPrefix opens the REST path of anything asked about one repository.
const repoPathPrefix = "/repos/"

// Traffic collects the only data GitHub throws away.
//
// Views and clones live for exactly 14 days and then cease to exist anywhere.
// The whole window is re-read and rewritten on every sweep, each day stamped
// with its own date, so a collector that was off for a week loses nothing as
// long as it comes back inside the window. That is also why this must not be a
// Prometheus exporter: a scrape can only stamp "now".
//
// Referrers and popular paths are different: the API returns a top-10 snapshot
// with no dates at all, so they are stamped with the sweep time and read as
// "who was sending traffic when we asked".
type Traffic struct{}

type trafficCount struct {
	Timestamp time.Time `json:"timestamp"`
	Count     int       `json:"count"`
	Uniques   int       `json:"uniques"`
}

func (Traffic) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	// GitHub's own traffic graph, which is the page these numbers vanish from
	// after fourteen days.
	graph := githubPage(repo.FullName, "graphs", "traffic")
	var points []sink.Point

	var views struct {
		Count   int            `json:"count"`
		Uniques int            `json:"uniques"`
		Views   []trafficCount `json:"views"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/traffic/views", &views, ""); err != nil {
		if !isSkippable(err) {
			return nil, err
		}
	}
	for _, v := range views.Views {
		points = append(points, sink.Point{
			Measurement: "gh_traffic",
			Tags:        merge(base, map[string]string{"kind": "views"}),
			Fields:      withURL(map[string]any{"count": v.Count, "uniques": v.Uniques}, graph),
			Time:        v.Timestamp, // the day GitHub says, not the day we asked
		})
	}

	var clones struct {
		Count   int            `json:"count"`
		Uniques int            `json:"uniques"`
		Clones  []trafficCount `json:"clones"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/traffic/clones", &clones, ""); err != nil {
		if !isSkippable(err) {
			return nil, err
		}
	}
	for _, v := range clones.Clones {
		points = append(points, sink.Point{
			Measurement: "gh_traffic",
			Tags:        merge(base, map[string]string{"kind": "clones"}),
			Fields:      withURL(map[string]any{"count": v.Count, "uniques": v.Uniques}, graph),
			Time:        v.Timestamp,
		})
	}

	// Referrers and paths carry no dates. GitHub returns the top ten of the
	// same trailing fourteen days as one list, with no day attached, so they
	// are a snapshot rather than a series.
	//
	// Stamped at the start of the UTC day, not at the instant of the sweep:
	// four sweeps a day would otherwise write four copies of the same snapshot,
	// and any query that added them up would report four times the traffic.
	// One row per day per referrer is both idempotent and enough resolution
	// for a list that only moves as its fourteen-day window slides.
	day := now.UTC().Truncate(24 * time.Hour)
	var referrers []struct {
		Referrer string `json:"referrer"`
		Count    int    `json:"count"`
		Uniques  int    `json:"uniques"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/traffic/popular/referrers", &referrers, ""); err != nil {
		if !isSkippable(err) {
			return nil, err
		}
	}
	for _, r := range referrers {
		// `url` stays the traffic graph, the same page as every other row of
		// this sweep, because it is the only page these numbers have. The
		// referrer itself is a second, weaker link, and only when it is a
		// place rather than a name.
		fields := withURL(map[string]any{"count": r.Count, "uniques": r.Uniques}, graph)
		if hostLike(r.Referrer) {
			fields["referrer_url"] = "https://" + r.Referrer
		}
		points = append(points, sink.Point{
			Measurement: "gh_traffic_referrer",
			Tags:        merge(base, map[string]string{"referrer": r.Referrer}),
			Fields:      fields,
			Time:        day,
		})
	}

	var paths []struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Count   int    `json:"count"`
		Uniques int    `json:"uniques"`
	}
	if _, _, err := c.GetJSON(ctx, repoPathPrefix+repo.FullName+"/traffic/popular/paths", &paths, ""); err != nil {
		if !isSkippable(err) {
			return nil, err
		}
	}
	for _, p := range paths {
		points = append(points, sink.Point{
			Measurement: "gh_traffic_path",
			Tags:        merge(base, map[string]string{"path": p.Path}),
			// The path itself, which is the page the visitors landed on.
			Fields: withURL(map[string]any{
				"count": p.Count, "uniques": p.Uniques, "title": p.Title,
			}, githubRootedPage(p.Path)),
			Time: day,
		})
	}
	return points, nil
}

// hostLike reports whether a referrer names a place that can be linked to.
//
// Measured across twenty repositories on 2026-09-10: of forty referrer rows,
// twelve carry a brand GitHub coins for a search engine (Google, Bing,
// DuckDuckGo) and the rest carry a hostname (github.com, jmrplens.github.io,
// chatgpt.com, 1confluence.fnb.co.za, teams.public.onecdn.static.microsoft).
// https:// plus a brand is a dead link, so a value only earns one when it is
// dotted and lowercase, which every measured hostname is and no measured brand
// is. The test errs towards writing no link rather than a broken one.
func hostLike(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
