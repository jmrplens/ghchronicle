package collect

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// gqlString is the GraphQL type of a required string variable, which most
// parted queries declare more than once.
const gqlString = "String!"

// queryPart is one selection of a partedQuery: something the query asks that
// answers on its own, with the variables it uses beyond the wrapper's.
type queryPart struct {
	selection string
	vars      []string
}

// partedQuery is a GraphQL query that pages nothing and is made of parts that
// answer independently: the account's counts beside its sponsors and its
// contributions, a year's calendar beside its commits by repository.
//
// A page can be asked smaller when GitHub gives up on it (smallerPage); a
// query like this has no page, and it timed out all the same: the account
// query took more than six seconds 114 times and met the gateway's 502 three
// times in the production proxy's log to 2026-11-16. So ask asks every part
// in one query, as it always has, and when GitHub gives up on that, asks each
// half again on its own, down to a part a query, decoding every answer into
// the same value: an answer carries only the fields of the parts it was asked
// for, and json.Unmarshal leaves the others as the earlier answers set them.
type partedQuery struct {
	// name is the operation's name, empty for an anonymous query.
	name string
	// decls is the GraphQL type of every variable any part or the wrapper
	// uses, by its name without the $.
	decls map[string]string
	// open and close enclose the parts, as `user(login: $login) {` and `}`;
	// shared is the variables they use.
	open, close string
	shared      []string
	parts       []queryPart
}

// text is the query asking for parts, declaring only the variables they and
// the wrapper use: GraphQL refuses a query that declares one it does not use.
func (q partedQuery) text(parts []queryPart) string {
	used := slices.Clone(q.shared)
	for _, p := range parts {
		used = append(used, p.vars...)
	}
	slices.Sort(used)
	used = slices.Compact(used)
	decls := make([]string, 0, len(used))
	for _, v := range used {
		decls = append(decls, "$"+v+": "+q.decls[v])
	}
	var b strings.Builder
	b.WriteString("query")
	if q.name != "" {
		b.WriteString(" " + q.name)
	}
	if len(decls) > 0 {
		b.WriteString("(" + strings.Join(decls, ", ") + ")")
	}
	b.WriteString(" {\n" + q.open + "\n")
	for _, p := range parts {
		b.WriteString(p.selection + "\n")
	}
	b.WriteString(q.close + "\n}")
	return b.String()
}

// varsFor is vars narrowed to what parts and the wrapper use.
func (q partedQuery) varsFor(parts []queryPart, vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, v := range q.shared {
		out[v] = vars[v]
	}
	for _, p := range parts {
		for _, v := range p.vars {
			out[v] = vars[v]
		}
	}
	return out
}

// ask asks every part and decodes the answers into out.
func (q partedQuery) ask(ctx context.Context, c *ghapi.Client, vars map[string]any, out any) error {
	return q.askParts(ctx, c, q.parts, vars, out)
}

func (q partedQuery) askParts(ctx context.Context, c *ghapi.Client, parts []queryPart, vars map[string]any, out any) error {
	err := c.GraphQL(ctx, q.text(parts), q.varsFor(parts, vars), out)
	if err == nil || len(parts) < 2 {
		return err
	}
	if _, tooLarge := errors.AsType[*ghapi.TooLargeError](err); !tooLarge {
		return err
	}
	half := len(parts) / 2
	return errors.Join(
		q.askParts(ctx, c, parts[:half], vars, out),
		q.askParts(ctx, c, parts[half:], vars, out),
	)
}
