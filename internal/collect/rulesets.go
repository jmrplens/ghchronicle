package collect

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// RulesetHistory records every version of every ruleset a repository has,
// dated when that version was saved.
//
// gh_ruleset says what a ruleset enforces today and how many days ago it last
// changed. That number only summarizes: a ruleset switched off on a Tuesday
// and back on the Friday after reads as "changed three days ago" and nothing
// collected says a protection was ever absent. The history endpoint is the
// changelog itself, one entry per saved version with the actor that saved it,
// and it is the only record GitHub keeps of the moment a protection was turned
// off. Measured on 2026-09-11 against the ruleset guarding this account's
// busiest repository: 20 versions between 2026-05-06 and 2026-09-07, 3 KB, one
// core request, with an ETag so an unchanged history is a 304 on every sweep
// after the first.
//
// Two requests per repository that has any ruleset, and one per repository
// that has none. The list is the REST one rather than the GraphQL page
// RepoDetail already reads: that page carries no database id, the history
// endpoint is addressed by nothing else, and a REST list is conditional where
// a GraphQL point never is. Both requests carry an ETag, so a day on which
// nobody edited a protection costs nothing from the budget at all.
//
// The endpoint answers 404 for a ruleset that has been deleted between the
// list and the read, and for a repository whose rulesets this token cannot
// see. Neither is a failure of the sweep.
type RulesetHistory struct {
	// Walk bounds how far back the history is read. A sweep reads the newest
	// page of a hundred versions, which is every version any ruleset on the
	// measured account has; a backfill keeps paging.
	Walk Walk
}

// rulesetSummary is the part of GET /repos/{r}/rulesets a version needs:
// the id the history is addressed by, and the name and target that identify
// the ruleset in gh_ruleset, so one dashboard can join the two.
type rulesetSummary struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target"`
}

// rulesetVersion is one entry of the changelog. GitHub names the actor by id
// and type only; the login is not in the response.
type rulesetVersion struct {
	VersionID int64     `json:"version_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Actor     struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"actor"`
}

// Collect takes the sweep time like every per-repository collector and does
// not use it: every row carries the date GitHub saved that version, which is
// the whole point of the measurement.
func (rh RulesetHistory) Collect(ctx context.Context, c *ghapi.Client, repo Repo, _ time.Time) ([]sink.Point, error) {
	var rulesets []rulesetSummary
	if _, _, err := c.GetJSON(ctx, "/repos/"+repo.FullName+"/rulesets?per_page=100", &rulesets, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	var points []sink.Point
	for _, rs := range rulesets {
		pts, err := rh.versions(ctx, c, repo, base, rs)
		if err != nil {
			return points, err
		}
		points = append(points, pts...)
	}
	return points, nil
}

// versions reads one ruleset's changelog, newest first, and stops at the
// walk's bound.
func (rh RulesetHistory) versions(ctx context.Context, c *ghapi.Client, repo Repo, base map[string]string, rs rulesetSummary) ([]sink.Point, error) {
	id := strconv.FormatInt(rs.ID, 10)
	prefix := "/repos/" + repo.FullName + "/rulesets/" + id + "/history?per_page=100&page="
	tags := merge(base, map[string]string{
		// Lowercased like the target tag on the gh_ruleset row, so one
		// dashboard reads both. The REST list already answers in lowercase;
		// this keeps the tag the same if that ever changes.
		"ruleset": orNone(rs.Name), "target": strings.ToLower(orNone(rs.Target)),
	})
	url := githubPage(repo.FullName, "rules", id)
	var points []sink.Point
	err := pages(ctx, c, rh.Walk, 1, func(page int) string {
		return prefix + strconv.Itoa(page)
	}, func(rows []rulesetVersion) bool {
		for _, v := range rows {
			if rh.Walk.past(v.UpdatedAt) {
				return false
			}
			// The actor type is closed (User, Integration, Bot) and the id is
			// not: a series per actor id would be a series per person who
			// ever edited a rule, so the id rides as a field.
			points = append(points, sink.Point{
				Measurement: "gh_ruleset_version",
				Tags: merge(tags, map[string]string{
					"actor_type": strings.ToLower(orNone(v.Actor.Type)),
				}),
				Fields: withURL(map[string]any{
					"versions": 1, "version_id": v.VersionID,
					"ruleset_id": rs.ID, "actor_id": v.Actor.ID,
				}, url),
				Time: v.UpdatedAt,
			})
		}
		return true
	})
	return points, err
}
