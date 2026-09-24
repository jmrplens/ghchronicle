package collect

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// Filter says which repositories a sweep should touch.
type Filter struct {
	User            string
	Orgs            []string
	Repos           []string
	Exclude         []string
	IncludeForks    bool
	IncludeArchived bool
	IncludePrivate  bool
}

// Discovery is what one read of the listings found.
//
// Repos is what the sweep collects. Archived is the archived repositories the
// filter set aside and nothing else: each of them passed every other rule
// (exclusion, fork, visibility) and was dropped for being archived alone, so
// it is the set that `include_archived: true` would have added. The listing
// already says which repositories are archived, so keeping them here costs
// nothing, and it is what lets a sweep put the archive on record without
// collecting the repositories themselves: measured on this account, eighteen
// of fifty-four repositories are archived, seventeen of them with the fork
// rule applied, and the panel that lists them was an error for as long as
// the filter hid them from every collector.
type Discovery struct {
	Repos    []Repo
	Archived []Repo
}

// Discover lists the repositories to collect.
//
// Forks and archived repositories are excluded by default: a fork's traffic is
// almost always zero and an archived repository cannot change, so including
// them by default would spend most of the rate limit on rows that never move.
func Discover(ctx context.Context, c *ghapi.Client, f *Filter) (Discovery, error) {
	seen := map[string]bool{}
	aside := map[string]bool{}
	var out Discovery
	keep := func(r Repo, filtered bool) {
		if seen[r.FullName] {
			return
		}
		if filtered && !f.wants(r) {
			if f.archivedAside(r) && !aside[r.FullName] {
				aside[r.FullName] = true
				out.Archived = append(out.Archived, r)
			}
			return
		}
		seen[r.FullName] = true
		out.Repos = append(out.Repos, r)
	}

	owned, err := f.ownedRepos(ctx, c)
	for _, r := range owned {
		keep(r, true)
	}
	if err != nil {
		return out, err
	}

	// Explicitly named repositories are collected whatever the filters say:
	// naming one is the clearest possible statement of intent.
	named, err := namedRepos(ctx, c, f.Repos)
	for _, r := range named {
		keep(r, false)
	}
	// A repository set aside by the listing and then named is collected in
	// full, and a collected repository has its archive on record from its
	// own totals row, so it has no business in the second list as well.
	out.Archived = slices.DeleteFunc(out.Archived, func(r Repo) bool { return seen[r.FullName] })
	return out, err
}

// wants reports whether a discovered repository survives the filter.
func (f *Filter) wants(r Repo) bool {
	switch {
	case excluded(r.FullName, f.Exclude):
		return false
	case r.Fork && !f.IncludeForks:
		return false
	case r.Archived && !f.IncludeArchived:
		return false
	case r.Private && !f.IncludePrivate:
		return false
	}
	return true
}

// archivedAside reports whether a repository the filter refused was refused
// for being archived and for nothing else: the same repository, live, would
// have been collected. An archived fork under the default fork rule is not
// set aside, because the fork rule is the operator's and stays as configured.
func (f *Filter) archivedAside(r Repo) bool {
	if !r.Archived || f.IncludeArchived {
		return false
	}
	r.Archived = false
	return f.wants(r)
}

// ownedRepos lists everything the token can see for the user and the
// organizations, unfiltered. Whatever it managed to read is returned even when
// a later page fails, so a sweep is not lost to one bad response.
func (f *Filter) ownedRepos(ctx context.Context, c *ghapi.Client) ([]Repo, error) {
	var sources []string
	if f.User != "" {
		// The authenticated form is used when the user is the token's owner,
		// because it is the only one that returns private repositories.
		sources = append(sources, "/user/repos?affiliation=owner&per_page=100")
	}
	for _, org := range f.Orgs {
		sources = append(sources, "/orgs/"+org+"/repos?per_page=100")
	}

	var out []Repo
	for _, src := range sources {
		for page := 1; page <= 20; page++ {
			var batch []struct {
				FullName string `json:"full_name"`
				Name     string `json:"name"`
				Private  bool   `json:"private"`
				Fork     bool   `json:"fork"`
				Archived bool   `json:"archived"`
				// The listing carries this flag; security_and_analysis it
				// does not, checked on 2026-09-11, which is why that one is
				// still read per repository.
				HasDiscussions bool `json:"has_discussions"`
				Owner          struct {
					Login string `json:"login"`
				} `json:"owner"`
			}
			if _, _, err := c.GetJSON(ctx, fmt.Sprintf("%s&page=%d", src, page), &batch, ""); err != nil {
				if isSkippable(err) {
					break
				}
				return out, err
			}
			for _, r := range batch {
				// A user filter means that user's repositories, even though the
				// authenticated listing can include ones owned elsewhere.
				if f.User != "" && !strings.EqualFold(r.Owner.Login, f.User) && len(f.Orgs) == 0 {
					continue
				}
				out = append(out, Repo{
					Owner: r.Owner.Login, Name: r.Name, FullName: r.FullName,
					Private: r.Private, Fork: r.Fork, Archived: r.Archived,
					HasDiscussions: r.HasDiscussions,
				})
			}
			if len(batch) < 100 {
				break
			}
		}
	}
	return out, nil
}

// namedRepos reads the repositories the configuration named one by one.
//
// A repository the token cannot see still counts as named: it is listed with
// what little is known about it rather than dropped, because the name in the
// configuration is the statement of intent.
func namedRepos(ctx context.Context, c *ghapi.Client, names []string) ([]Repo, error) {
	var out []Repo
	for _, full := range names {
		owner, name, ok := strings.Cut(full, "/")
		if !ok {
			return out, fmt.Errorf("targets.repos: %q is not owner/name", full)
		}
		var r struct {
			Private        bool `json:"private"`
			Fork           bool `json:"fork"`
			Archived       bool `json:"archived"`
			HasDiscussions bool `json:"has_discussions"`
		}
		if _, _, err := c.GetJSON(ctx, "/repos/"+full, &r, ""); err != nil && !isSkippable(err) {
			return out, err
		}
		out = append(out, Repo{
			Owner: owner, Name: name, FullName: full,
			Private: r.Private, Fork: r.Fork, Archived: r.Archived,
			HasDiscussions: r.HasDiscussions,
		})
	}
	return out, nil
}

// excluded matches a repository against the exclusion patterns, which accept
// shell globs so a whole prefix can be dropped with one line.
func excluded(full string, patterns []string) bool {
	for _, p := range patterns {
		if p == full {
			return true
		}
		if ok, _ := path.Match(p, full); ok {
			return true
		}
	}
	return false
}

// FirstSeen records when a repository entered the sweep, so a collector that
// needs a one-off full walk (the star history) knows to do it once.
type FirstSeen map[string]time.Time
