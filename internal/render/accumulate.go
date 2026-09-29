package render

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// Accumulator turns a sweep's points into a Card.
//
// It is a sink, which is what lets the card be built from exactly the same
// data that goes to the database, with no second pass over the API and no
// separate idea of what the numbers mean. Attach it, run one sweep, render.
type Accumulator struct {
	Login string

	// Every number the card draws is a count, so each is kept whole, read
	// by sink.IntField or sink.Int64Field, which refuse what no int holds.
	account map[string]int
	// repos and languages are keyed by repoKey, never by the short name.
	repos   map[string]*repoRow
	traffic map[string]int
	days    map[time.Time]int
	// languages holds bytes per language per repository, newest snapshot
	// per pair, summed at render time. It is an int64 from the field to the
	// card's Language, so no narrowing stands between them: torvalds/linux
	// alone reports 1,452,105,738 bytes of C (read on 2026-09-29), and two
	// repositories like it add up past what a 32-bit int holds.
	languages map[string]map[string]int64
	// newest remembers the most recent timestamp seen per repository field, so
	// a sweep that writes the same repository twice keeps the later value.
	seen map[string]time.Time
}

type repoRow struct {
	key      string
	name     string
	language string
	stars    int
	forks    int
}

// NewAccumulator returns a sink that builds a card for one account.
func NewAccumulator(login string) *Accumulator {
	return &Accumulator{
		Login:     login,
		account:   map[string]int{},
		repos:     map[string]*repoRow{},
		traffic:   map[string]int{},
		days:      map[time.Time]int{},
		languages: map[string]map[string]int64{},
		seen:      map[string]time.Time{},
	}
}

func (a *Accumulator) Name() string { return "card" }
func (a *Accumulator) Close() error { return nil }

// Write folds the points a card can draw and ignores the rest, and says which
// is which: this sink reads six measurements of the ninety the collectors
// produce, so most of a sweep is not accepted here.
func (a *Accumulator) Write(_ context.Context, points []sink.Point) (int, error) {
	taken := 0
	for _, p := range points {
		switch p.Measurement {
		case "gh_account":
			taken++
			a.takeLatest("account", p)
		case "gh_contributions_total":
			taken++
			a.takeLatest("contributions", p)
		case "gh_repo":
			taken++
			a.takeRepo(p)
		case "gh_traffic":
			taken++
			// The whole window summed, which is what the card shows: GitHub
			// gives fourteen days and a card has room for one number.
			kind := p.Tags["kind"]
			if v, ok := sink.IntField(p.Fields["count"]); ok {
				a.traffic[kind+"_count"] += v
			}
			if v, ok := sink.IntField(p.Fields["uniques"]); ok {
				a.traffic[kind+"_uniques"] += v
			}
		case "gh_repo_language":
			taken++
			repo, lang := repoKey(p.Tags), p.Tags["language"]
			if repo == "" || lang == "" {
				break
			}
			if v, ok := sink.Int64Field(p.Fields["bytes"]); ok {
				if a.languages[repo] == nil {
					a.languages[repo] = map[string]int64{}
				}
				a.languages[repo][lang] = v
			}
		case "gh_contribution_day":
			taken++
			if v, ok := sink.IntField(p.Fields["contributions"]); ok {
				day := p.Time.UTC().Truncate(24 * time.Hour)
				// Later wins rather than adding: the same day arriving twice
				// is a re-read of the calendar, not two days of work.
				a.days[day] = v
			}
		}
	}
	return taken, nil
}

func (a *Accumulator) takeLatest(prefix string, p sink.Point) {
	if last, ok := a.seen[prefix]; ok && p.Time.Before(last) {
		return
	}
	a.seen[prefix] = p.Time
	for k, v := range p.Fields {
		if n, ok := sink.IntField(v); ok {
			a.account[prefix+"."+k] = n
		}
	}
}

// repoKey is what tells one repository from another. The short name does
// not: two owners can give a repository the same one, and a sweep of an
// account and its organizations meets both, a user's .github and an
// organization's, so keyed by it one repository's stars and forks replaced
// the other's. full_name is owner/name, which the Overview keys by too. The
// collectors write (none) there when either part is missing, so a value
// without the slash, like a point without the tag, leaves the short name as
// the only key there is.
func repoKey(tags map[string]string) string {
	if full := tags["full_name"]; strings.Contains(full, "/") {
		return full
	}
	return tags["repo"]
}

func (a *Accumulator) takeRepo(p sink.Point) {
	name := p.Tags["repo"]
	if name == "" {
		return
	}
	id := repoKey(p.Tags)
	key := "repo." + id
	if last, ok := a.seen[key]; ok && p.Time.Before(last) {
		return
	}
	a.seen[key] = p.Time
	r, known := a.repos[id]
	if !known {
		r = &repoRow{key: id, name: name}
		a.repos[id] = r
	}
	r.language = p.Tags["language"]
	if v, ok := sink.IntField(p.Fields["stars"]); ok {
		r.stars = v
	}
	if v, ok := sink.IntField(p.Fields["forks"]); ok {
		r.forks = v
	}
}

// Card renders what has been accumulated. Totals that GitHub reports directly
// are preferred over sums computed here, because they include repositories the
// sweep filtered out.
func (a *Accumulator) Card() Card {
	c := Card{Login: a.Login}
	c.Followers = a.account["account.followers"]
	c.Repos = a.account["account.public_repos"]
	c.Contributions = a.account["contributions.calendar_total"]
	c.Commits = a.account["contributions.commits"]
	c.PullRequests = a.account["contributions.pull_requests"]
	c.Reviews = a.account["contributions.reviews"]
	c.Issues = a.account["contributions.issues"]
	c.Views = a.traffic["views_count"]
	c.UniqueVisitors = a.traffic["views_uniques"]
	c.Clones = a.traffic["clones_count"]

	// Languages summed across repositories. No color is set: the renderer
	// carries Linguist's table, and GitHub's API does not return one.
	total := map[string]int64{}
	for _, langs := range a.languages {
		for name, bytes := range langs {
			total[name] += bytes
		}
	}
	for name, bytes := range total {
		c.Languages = append(c.Languages, Language{Name: name, Bytes: bytes})
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
	rows := make([]*repoRow, 0, len(a.repos))
	for _, r := range a.repos {
		c.Stars += r.stars
		c.Forks += r.forks
		rows = append(rows, r)
	}

	// Sorted here as well as in the renderer: the map iteration above is
	// random, and a caller reading TopRepos directly deserves a stable order.
	// Two owners' repositories of one name and one star count are told apart
	// by the key, which the list does not show.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].stars != rows[j].stars {
			return rows[i].stars > rows[j].stars
		}
		if rows[i].name != rows[j].name {
			return rows[i].name < rows[j].name
		}
		return rows[i].key < rows[j].key
	})
	for _, r := range rows {
		c.TopRepos = append(c.TopRepos, TopRepo{
			Name: r.name, Language: r.language, Stars: r.stars,
		})
	}

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

var _ sink.Sink = (*Accumulator)(nil)
