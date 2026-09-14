package collect

import "strings"

// Small string helpers, kept here so the collectors read as data extraction
// rather than as parsing.

func splitAny(s, sep string) []string { return strings.Split(s, sep) }
func trimSpace(s string) string       { return strings.TrimSpace(s) }
func trimQuotes(s string) string      { return strings.Trim(s, `"`) }
func indexOf(s, sub string) int       { return strings.Index(s, sub) }

// noneTag is what a tag carries when GitHub gives nothing for it, and it is
// the only spelling of that in this package.
//
// A tag with an empty value is not written at all: sink.LineProtocol leaves it
// out, so the point lands in a second InfluxDB series that carries no such tag
// rather than beside its neighbors in one that says there is none, and the
// same emptiness lets a field of the same name through the clash rule that
// would otherwise have dropped it. Graphite is not the injured party, whatever
// the comments that used to sit on the five copies of this said: its path
// takes one node per tag key and renders an empty value as `none` itself, so
// its depth follows the keys, and the keys are written on every point.
//
// Parenthesised, and that is the whole reason this spelling won over the
// `none` and `unknown` it replaces: GitHub itself answers `unknown` in a
// Dependabot alert's `relationship`, measured on 4 of 119 alerts, so a
// fallback spelled that way cannot be told from an answer. `(none)` is not a
// value GitHub returns. It follows the `(ghost)` this package already writes
// for a login GitHub will not name.
const noneTag = "(none)"

// orNone is the one way a collector writes a tag it has no value for.
func orNone(s string) string {
	if s == "" {
		return noneTag
	}
	return s
}

// appLogin is the one spelling of a GitHub App's login in this package: the
// REST one, with the "[bot]" suffix, which is also how GitHub prints it.
//
// GraphQL leaves the suffix off and says __typename Bot instead, so the same
// app was `dependabot[bot]` on a workflow run (REST), `dependabot` on the pull
// request it opened (GraphQL) and both on the issue events, depending on which
// road the event came by. A table by author counted one bot as two. A login
// that already carries the suffix is left alone.
func appLogin(login string, bot bool) string {
	if bot && login != "" && !strings.HasSuffix(login, "[bot]") {
		return login + "[bot]"
	}
	return login
}
