package collect

import (
	"context"
	"maps"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// RateLimit records what the collector has left to spend.
//
// GitHub runs fifteen independent buckets. `GET /rate_limit` reports them all
// and charges for none, which is what makes almost all of this measurement
// free. One of the fifteen is not true, and it is the one that matters most
// here: measured on 2026-09-08 with the token this runs under, the endpoint
// answered graphql used=0, remaining=5000 in the same minute GraphQL itself
// answered used=162, remaining=4838 and moved by one on every query. The two
// do not even share a clock, since the endpoint's `reset` slides forward a
// second per call because nothing ever touches that bucket. A row built from
// it reads zero forever, exactly where the collector does spend GraphQL.
//
// So the graphql row is read from GraphQL's own counter and the endpoint's
// version of it is dropped. One resource, one row, one source, and no two
// rows for a reader to have to reconcile.
//
// It is not the only bucket the endpoint invents. Measured on 2026-09-12: an
// SBOM request answered x-ratelimit-resource dependency_sbom, used 1,
// remaining 99, reset in 59 s, and GET /rate_limit two seconds later said
// used 0, remaining 100, reset in 60 s, sliding forward a second per call
// exactly as graphql does. Every REST answer names the bucket it charged in
// its headers, and the client keeps the newest reading per bucket, so where
// that reading is still inside its window and says more was spent than the
// endpoint admits, the headers win (see fromHeaders). The one-minute windows
// of dependency_sbom and search are why the reading has to be current: a
// minute after the last SBOM request the endpoint's zero is the truth.
//
// It matters more the more surfaces are collected. A family silently skipped
// because the reserve was reached looks exactly like a family with nothing to
// report, and the difference is visible only here.
type RateLimit struct{}

type rateBucket struct {
	Limit     int   `json:"limit"`
	Used      int   `json:"used"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

func (RateLimit) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	var res struct {
		Resources map[string]rateBucket `json:"resources"`
	}
	if _, _, err := c.GetJSON(ctx, "/rate_limit", &res, ""); err != nil && !isSkippable(err) {
		return nil, err
	}
	points := make([]sink.Point, 0, len(res.Resources))
	for name, b := range res.Resources {
		// "rate" is the deprecated alias for "core" and reports the same
		// numbers under a second name, which would double every total.
		// "graphql" is reported here too and is not the counter GraphQL
		// charges, so it is read from GraphQL below instead.
		if name == "rate" || name == "graphql" || b.Limit == 0 {
			continue
		}
		used := b.Used
		if used == 0 && b.Remaining <= b.Limit {
			used = b.Limit - b.Remaining
		}
		limit, remaining, reset := b.Limit, b.Remaining, time.Unix(b.Reset, 0)
		if seen, ok := fromHeaders(c, name, used, now); ok {
			limit, used, remaining, reset = seen.Limit, seen.Used, seen.Remaining, seen.Reset
		}
		points = append(points, ratePoint(name, limit, used, remaining, reset, now, nil))
	}
	if p, ok := graphqlPoint(ctx, c, now); ok {
		points = append(points, p)
	}
	return points, nil
}

// fromHeaders is the last x-ratelimit-* reading the client saw for a bucket,
// when it is worth more than what the endpoint says: still inside its own
// window, and reporting more spent than the endpoint admits. A reading whose
// window has closed describes a budget that has since been refilled, and one
// that says less than the endpoint is older than the endpoint's answer.
func fromHeaders(c *ghapi.Client, resource string, endpointUsed int, now time.Time) (ghapi.RateState, bool) {
	seen, ok := c.RateFor(resource)
	if !ok || seen.Limit == 0 || seen.Reset.IsZero() || !seen.Reset.After(now) {
		return ghapi.RateState{}, false
	}
	used := seen.Used
	if used == 0 && seen.Remaining <= seen.Limit {
		used = seen.Limit - seen.Remaining
	}
	if used <= endpointUsed {
		return ghapi.RateState{}, false
	}
	seen.Used = used
	return seen, true
}

// graphqlPoint reads the GraphQL budget from GraphQL, which is the only place
// that reports it.
//
// A probe that fails falls back to the last answer the client saw while
// collecting anything else, and when there is no such answer it writes no row
// at all. That is deliberate: the defect this replaces was a row that read
// zero forever, and a missing row says "not measured" where a zero says
// "nothing spent". A failure here must also not throw away the buckets that
// did answer, so it is not returned as an error.
//
// The fallback is only taken while that answer still describes the window the
// point is being stamped in. This family runs every fifteen minutes and the
// batched queries every twelve hours, so the last reading left behind can be
// half a day old, and a spent budget from a window that closed long ago
// stamped `now` is a reading of a moment it was never taken at. The window is
// an hour, so what survives this test is at worst an hour of under-counting
// inside the window it belongs to.
func graphqlPoint(ctx context.Context, c *ghapi.Client, now time.Time) (sink.Point, bool) {
	rate, err := c.GraphQLRate(ctx)
	if err != nil {
		seen, ok := c.RateFor("graphql")
		if !ok || seen.Reset.IsZero() || !seen.Reset.After(now) {
			return sink.Point{}, false
		}
		rate = seen
	}
	if rate.Limit == 0 {
		return sink.Point{}, false
	}
	used := rate.Used
	if used == 0 && rate.Remaining <= rate.Limit {
		used = rate.Limit - rate.Remaining
	}
	// The four figures above are the whole token's window, shared with
	// whatever else holds it. These two are this process's own spend, which
	// is the only GraphQL consumption ghchronicle answers for.
	spend := c.GraphQLSpend()
	return ratePoint("graphql", rate.Limit, used, rate.Remaining, rate.Reset, now, map[string]any{
		"own_cost":    spend.Cost,
		"own_queries": spend.Queries,
	}), true
}

func ratePoint(resource string, limit, used, remaining int, reset, now time.Time, extra map[string]any) sink.Point {
	fields := map[string]any{
		"limit": limit, "used": used, "remaining": remaining,
		"used_ratio": float64(used) / float64(limit),
	}
	// A window with no reset instant is not a window a countdown can describe,
	// and a zero time would report a reset forty years in the past.
	if !reset.IsZero() {
		fields["seconds_to_reset"] = int(reset.Sub(now).Seconds())
	}
	maps.Copy(fields, extra)
	return sink.Point{
		Measurement: "gh_rate_limit",
		Tags:        map[string]string{"resource": resource},
		Fields:      fields,
		Time:        now,
	}
}
