package collect

import "strings"

// repoTags is the one shape in which a point names a repository: the owner,
// the short name, and the two joined as full_name. Every collector builds that
// tag set here, and nowhere else spells the keys.
//
// Three shapes used to stand side by side, measured on a live store on
// 2026-09-17. Sixty-one measurements carried this one. Twelve carried no owner
// and put the full name in `repo`, so `repo='acme/telemetry'`. gh_billing_usage
// carried a short `repo`, no owner and an `org`. The overlap between the first
// two was exactly zero in both directions, which is worse than it sounds: a
// filter written against one group matched nothing at all in the other rather
// than matching badly, and a union counted every repository twice.
//
// This one won on more than the count. It is the only one of the three that can
// answer "whose repository is this", and that question belongs precisely to the
// twelve, since several of them are about repositories the account does not own
// (gh_external_contribution, gh_star_given, gh_contribution_repo). It also lets
// a dashboard group by owner without taking a string apart.
//
// The short name is not an identity, and that is the reason full_name travels
// with it rather than being left for the reader to rebuild. Two owners can name
// a repository the same thing, and now that other people's repositories are
// tagged the same way as the account's own, they meet in one column: a panel
// that needs identity groups by full_name, and one that needs to narrow to a
// person groups by owner.
func repoTags(owner, name string) map[string]string {
	full := ""
	if owner != "" && name != "" {
		full = owner + "/" + name
	}
	return map[string]string{
		"owner": orNone(owner), "repo": orNone(name), "full_name": orNone(full),
	}
}

// fullNameTags is repoTags for the surfaces that hand over the owner and the
// name in one string: the event feed's repo.name, a notification's full_name,
// every GraphQL nameWithOwner. Cutting it here rather than at each call site is
// what keeps `repo` short in all of them.
//
// A string with no slash is read as the name with no owner known, which is what
// GitHub returns for a pinned gist. A caller that knows the owner separately
// calls repoTags and keeps all three tags filled.
func fullNameTags(fullName string) map[string]string {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return repoTags("", fullName)
	}
	return repoTags(owner, name)
}
