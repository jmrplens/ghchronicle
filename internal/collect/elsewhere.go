package collect

import (
	"slices"
	"strings"
)

// Collected is what the configuration collects beside the account's own
// repositories: the organizations it lists and the repositories it names.
//
// The searches for work elsewhere (Outbound's items, the lifetime counts of
// Totals) leave out where it is collected. Without that a pull request in an
// organization the account administers was written twice, once by the
// repository families as gh_pull_request and once as gh_external_contribution,
// and the work of building one's own organization read as work sent to
// strangers: on 2026-11-21, 33 of the 134 pull requests jmrplens authored were
// in kleidos-dev, which targets.orgs collects whole.
//
// The tag `own` of the comment and contribution rows is not this. It says the
// account owns the repository, it is part of every such row's identity, and an
// organization joining targets.orgs would give each row already written there
// a second one beside it.
type Collected struct {
	Orgs  []string
	Repos []string
}

// notIn is the part of a search that leaves out every repository the
// configuration collects: -user: for the account and each organization, and
// -repo: for each named repository none of them owns. GitHub took seven
// negations in one query without complaint on 2026-11-21, and each kept out
// what it named: 134 pull requests by jmrplens outside jmrplens, 101 outside
// kleidos-dev as well, 99 with conda-forge/staged-recipes left out too.
func (h Collected) notIn(login string) string {
	owners := []string{login}
	for _, org := range h.Orgs {
		if org != "" && !containsFold(owners, org) {
			owners = append(owners, org)
		}
	}
	parts := make([]string, 0, len(owners)+len(h.Repos))
	for _, owner := range owners {
		parts = append(parts, "-user:"+owner)
	}
	var named []string
	for _, full := range h.Repos {
		owner, _, ok := strings.Cut(full, "/")
		if ok && !containsFold(owners, owner) && !containsFold(named, full) {
			named = append(named, full)
		}
	}
	slices.SortFunc(named, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	for _, full := range named {
		parts = append(parts, "-repo:"+full)
	}
	return strings.Join(parts, " ")
}

// containsFold reports whether list holds s, whatever the case: GitHub's
// logins and repository names are not case sensitive.
func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(v string) bool { return strings.EqualFold(v, s) })
}
