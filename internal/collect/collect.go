package collect

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
)

// Repo identifies one repository across every collector.
type Repo struct {
	Owner    string
	Name     string
	FullName string
	Private  bool
	Fork     bool
	Archived bool
	// HasDiscussions is whether the forum is switched on, read from the same
	// listing that found the repository. The discussions query costs eleven
	// points whether or not there is anything to page, and measured on
	// 2026-09-11 sixteen of eighteen repositories answered
	// hasDiscussionsEnabled false, so asking them was a third of the daily
	// GraphQL spend for two hundred and eighteen bytes each.
	HasDiscussions bool
}

// isSkippable reports whether an error means "there is nothing here", as
// opposed to something being broken.
//
// Traffic needs push access, Dependabot answers 403 when disabled, and the
// /stats/* endpoints answer 202 while GitHub computes them. None of the three
// is a failure, and treating them as one would make a sweep across 50 repos
// fail on the first repository with a feature switched off.
func isSkippable(err error) bool {
	if _, ok := errors.AsType[*ghapi.RateLimitedError](err); ok {
		// GitHub answers a spent budget with the same 403 a switched-off
		// feature does. Skipping it would make every remaining collector
		// return nothing and report success.
		return false
	}
	var unavailable *ghapi.UnavailableError
	var notReady *ghapi.NotReadyError
	return errors.As(err, &unavailable) || errors.As(err, &notReady)
}

// merge copies its arguments left to right into a new map, so a later one wins
// where two set the same key. Variadic because a point's tag set is now built
// from three parts as often as from two: what the collector shares, the three
// tags that name the repository, and what this one point adds.
func merge(base map[string]string, extra ...map[string]string) map[string]string {
	size := len(base)
	for _, e := range extra {
		size += len(e)
	}
	out := make(map[string]string, size)
	maps.Copy(out, base)
	for _, e := range extra {
		maps.Copy(out, e)
	}
	return out
}

// isPaginationLimit reports whether GitHub refused to page any deeper. The
// activity feeds answer 422 once past their ceiling, which is a boundary of
// the data, not an error to retry.
func isPaginationLimit(err error) bool {
	return err != nil && strings.Contains(err.Error(), "pagination is limited")
}

// isSkippableGraphQL is the GraphQL twin of isSkippable: it reports whether a
// failed query means "there is nothing here" rather than "something is broken".
//
// It has to read the message because the gateway answers a repository that was
// renamed away and a feature that is switched off the same way it answers a
// spent budget: HTTP 200, an errors array, and no status to branch on. The
// type in that array is the only evidence there is, and matching it has to
// stay this narrow. A rate limit, a canceled sweep and a bad token also
// arrive here, and reading those as "nothing here" would make every remaining
// collector return no points and report success.
func isSkippableGraphQL(err error) bool {
	if err == nil {
		return false
	}
	if isSkippable(err) {
		return true
	}
	// A query the gateway gave up on is a request too large, not a repository
	// that went away, so the caller keeps the pages it already walked. Asking
	// again for the same query would only time out again.
	if _, ok := errors.AsType[*ghapi.TooLargeError](err); ok {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "graphql: NOT_FOUND:") ||
		strings.HasPrefix(msg, "graphql: FORBIDDEN:")
}

// Walk says how far a collector may page.
//
// A sweep is an increment and wants a page or two. A backfill wants
// everything, however long that takes, and bounds itself by date rather than
// by page count. Both are expressed here so a collector reads one value.
type Walk struct {
	// Pages caps the walk. Zero means the collector's own default; a
	// negative value means until the API runs out.
	Pages int
	// Since stops the walk once the API, which serves newest first, has gone
	// past this moment. Zero means no bound.
	Since time.Time
}

// Unbounded is a walk that stops only when the API does.
var Unbounded = Walk{Pages: -1}

// limit resolves Pages against a collector's default.
func (w Walk) limit(def int) int {
	switch {
	case w.Pages < 0:
		// A ceiling all the same, because an API that pages forever is a
		// bug on its side and an infinite loop on ours.
		return 100000
	case w.Pages == 0:
		return def
	}
	return w.Pages
}

// past reports whether an item dated t is older than the bound.
func (w Walk) past(t time.Time) bool {
	return !w.Since.IsZero() && !t.IsZero() && t.Before(w.Since)
}

// pages walks numbered REST pages of 100 until visit says stop, the page is
// short, the cap is reached, or GitHub refuses to page further.
//
// visit returns true to keep going. The generic parameter is the row type,
// so each collector keeps its own struct and this stays a loop.
func pages[T any](ctx context.Context, c *ghapi.Client, w Walk, def int, path func(page int) string, visit func(rows []T) bool) error {
	most := w.limit(def)
	for page := 1; page <= most; page++ {
		var rows []T
		if _, _, err := c.GetJSON(ctx, path(page), &rows, ""); err != nil {
			if isSkippable(err) || isPaginationLimit(err) {
				return nil
			}
			return err
		}
		if len(rows) == 0 || !visit(rows) || len(rows) < 100 {
			return nil
		}
	}
	return nil
}
