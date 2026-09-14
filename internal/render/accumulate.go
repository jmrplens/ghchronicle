package render

import (
	"context"
	"sort"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Accumulator turns a sweep's points into a Card.
//
// It is a sink, which is what lets the card be built from exactly the same
// data that goes to the database, with no second pass over the API and no
// separate idea of what the numbers mean. Attach it, run one sweep, render.
type Accumulator struct {
	Login string

	account map[string]float64
	repos   map[string]*repoRow
	traffic map[string]float64
	days    map[time.Time]int
	// languages holds bytes per language per repository, newest snapshot
	// per pair, summed at render time.
	languages map[string]map[string]int
	// newest remembers the most recent timestamp seen per repository field, so
	// a sweep that writes the same repository twice keeps the later value.
	seen map[string]time.Time
}

type repoRow struct {
	name     string
	language string
	stars    int
	forks    int
}

// NewAccumulator returns a sink that builds a card for one account.
func NewAccumulator(login string) *Accumulator {
	return &Accumulator{
		Login:     login,
		account:   map[string]float64{},
		repos:     map[string]*repoRow{},
		traffic:   map[string]float64{},
		days:      map[time.Time]int{},
		languages: map[string]map[string]int{},
		seen:      map[string]time.Time{},
	}
}

func (a *Accumulator) Name() string { return "card" }
func (a *Accumulator) Close() error { return nil }

func (a *Accumulator) Write(_ context.Context, points []sink.Point) error {
	for _, p := range points {
		switch p.Measurement {
		case "gh_account":
			a.takeLatest("account", p)
		case "gh_contributions_total":
			a.takeLatest("contributions", p)
		case "gh_repo":
			a.takeRepo(p)
		case "gh_traffic":
			// The whole window summed, which is what the card shows: GitHub
			// gives fourteen days and a card has room for one number.
			kind := p.Tags["kind"]
			if v, ok := numberOf(p.Fields["count"]); ok {
				a.traffic[kind+"_count"] += v
			}
			if v, ok := numberOf(p.Fields["uniques"]); ok {
				a.traffic[kind+"_uniques"] += v
			}
		case "gh_repo_language":
			repo, lang := p.Tags["repo"], p.Tags["language"]
			if repo == "" || lang == "" {
				break
			}
			if v, ok := numberOf(p.Fields["bytes"]); ok {
				if a.languages[repo] == nil {
					a.languages[repo] = map[string]int{}
				}
				a.languages[repo][lang] = int(v)
			}
		case "gh_contribution_day":
			if v, ok := numberOf(p.Fields["contributions"]); ok {
				day := p.Time.UTC().Truncate(24 * time.Hour)
				// Later wins rather than adding: the same day arriving twice
				// is a re-read of the calendar, not two days of work.
				a.days[day] = int(v)
			}
		}
	}
	return nil
}

func (a *Accumulator) takeLatest(prefix string, p sink.Point) {
	if last, ok := a.seen[prefix]; ok && p.Time.Before(last) {
		return
	}
	a.seen[prefix] = p.Time
	for k, v := range p.Fields {
		if n, ok := numberOf(v); ok {
			a.account[prefix+"."+k] = n
		}
	}
}

func (a *Accumulator) takeRepo(p sink.Point) {
	name := p.Tags["repo"]
	if name == "" {
		return
	}
	key := "repo." + name
	if last, ok := a.seen[key]; ok && p.Time.Before(last) {
		return
	}
	a.seen[key] = p.Time
	r, known := a.repos[name]
	if !known {
		r = &repoRow{name: name}
		a.repos[name] = r
	}
	r.language = p.Tags["language"]
	if v, ok := numberOf(p.Fields["stars"]); ok {
		r.stars = int(v)
	}
	if v, ok := numberOf(p.Fields["forks"]); ok {
		r.forks = int(v)
	}
}

// Card renders what has been accumulated. Totals that GitHub reports directly
// are preferred over sums computed here, because they include repositories the
// sweep filtered out.
func (a *Accumulator) Card() Card {
	c := Card{Login: a.Login}
	c.Followers = int(a.account["account.followers"])
	c.Repos = int(a.account["account.public_repos"])
	c.Contributions = int(a.account["contributions.calendar_total"])
	c.Commits = int(a.account["contributions.commits"])
	c.PullRequests = int(a.account["contributions.pull_requests"])
	c.Reviews = int(a.account["contributions.reviews"])
	c.Issues = int(a.account["contributions.issues"])
	c.Views = int(a.traffic["views_count"])
	c.UniqueVisitors = int(a.traffic["views_uniques"])
	c.Clones = int(a.traffic["clones_count"])

	// Languages summed across repositories. No color is set: the renderer
	// carries Linguist's table, and GitHub's API does not return one.
	total := map[string]int{}
	for _, langs := range a.languages {
		for name, bytes := range langs {
			total[name] += bytes
		}
	}
	for name, bytes := range total {
		c.Languages = append(c.Languages, Language{Name: name, Bytes: int64(bytes)})
	}
	sort.Slice(c.Languages, func(i, j int) bool {
		if c.Languages[i].Bytes != c.Languages[j].Bytes {
			return c.Languages[i].Bytes > c.Languages[j].Bytes
		}
		return c.Languages[i].Name < c.Languages[j].Name
	})
	c.TrafficWindowDays = 14

	// Summed from the repositories, because the account endpoint reports
	// neither a star nor a fork total. That means the card counts what the
	// sweep collected, so excluding forks or archived repositories from the
	// targets is visible here too, which is the honest reading.
	for _, r := range a.repos {
		c.Stars += r.stars
		c.Forks += r.forks
		c.TopRepos = append(c.TopRepos, TopRepo{
			Name: r.name, Language: r.language, Stars: r.stars,
		})
	}

	// Sorted here as well as in the renderer: the map iteration above is
	// random, and a caller reading TopRepos directly deserves a stable order.
	sort.Slice(c.TopRepos, func(i, j int) bool {
		if c.TopRepos[i].Stars != c.TopRepos[j].Stars {
			return c.TopRepos[i].Stars > c.TopRepos[j].Stars
		}
		return c.TopRepos[i].Name < c.TopRepos[j].Name
	})

	if len(a.days) > 0 {
		keys := make([]time.Time, 0, len(a.days))
		for d := range a.days {
			keys = append(keys, d)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
		// The last year only. The backfill reaches back to the account's first
		// year, and a sparkline of nine years is a smear.
		cutoff := keys[len(keys)-1].AddDate(-1, 0, 0)
		for _, d := range keys {
			if d.Before(cutoff) {
				continue
			}
			c.Sparkline = append(c.Sparkline, a.days[d])
		}
	}
	return c
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

var _ sink.Sink = (*Accumulator)(nil)
