package collect

import (
	"context"
	"fmt"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Billing collects what the account actually spent, day by day.
//
// The three old per-product endpoints are gone (410) and the `/user/` form of
// the usage report answers 404; only `/users/{login}/settings/billing/usage`
// works for a personal account. It reports one row per day, product, SKU and
// repository, which is the only place that says which repository burned the
// minutes.
//
// The net amount is not always zero: on this account it carries the monthly
// Copilot credit, so gross, discount and net are all kept rather than assuming
// one can be derived from the others.
type Billing struct {
	Login string
	// Months is how far back to walk, counting the current one.
	Months int
	// Walk widens it: a bounded Since walks back to that month, an unbounded
	// one keeps going until three consecutive months come back empty, which
	// is how GitHub says it has nothing older.
	Walk Walk
}

func (b Billing) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	months := b.Months
	if months <= 0 {
		months = 1
	}
	if b.Walk.Pages != 0 {
		months = b.Walk.limit(months)
	}
	var points []sink.Point
	empty := 0
	for i := range months {
		t := now.AddDate(0, -i, 0)
		if b.Walk.past(time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC)) {
			break
		}
		if empty >= 3 {
			break
		}
		path := fmt.Sprintf("/users/%s/settings/billing/usage?year=%d&month=%d", b.Login, t.Year(), int(t.Month()))
		var res struct {
			UsageItems []struct {
				Date           string  `json:"date"`
				Product        string  `json:"product"`
				SKU            string  `json:"sku"`
				Quantity       float64 `json:"quantity"`
				UnitType       string  `json:"unitType"`
				PricePerUnit   float64 `json:"pricePerUnit"`
				GrossAmount    float64 `json:"grossAmount"`
				DiscountAmount float64 `json:"discountAmount"`
				NetAmount      float64 `json:"netAmount"`
				RepositoryName string  `json:"repositoryName"`
				OrgName        string  `json:"organizationName"`
			} `json:"usageItems"`
		}
		if _, _, err := c.GetJSON(ctx, path, &res, ""); err != nil {
			if isSkippable(err) {
				empty++
				continue
			}
			return points, err
		}
		if len(res.UsageItems) == 0 {
			empty++
		} else {
			empty = 0
		}
		// k, not i: i is the month this page belongs to, and it is still
		// live here.
		for k := range res.UsageItems {
			u := &res.UsageItems[k]
			// GitHub dates these rows itself. Stamping them at their own day
			// is what makes a re-read of last month rewrite the same rows
			// instead of piling a second copy on today.
			// The field is documented as a date and delivered as a full
			// RFC 3339 timestamp, so both are accepted rather than trusting
			// either.
			day, err := time.Parse(time.RFC3339, u.Date)
			if err != nil {
				day, err = time.Parse("2006-01-02", u.Date)
				if err != nil {
					continue
				}
			}
			// The timestamp GitHub sends is the first billed minute of the
			// day, not midnight: measured on this account, 502 of 990 rows
			// arrived at times like 00:58:36. A row is one repository, one
			// SKU, one day, so it is stamped at the day, and a re-read
			// rewrites it whatever minute GitHub reports next time.
			day = day.UTC().Truncate(24 * time.Hour)
			points = append(points, sink.Point{
				Measurement: "gh_billing_usage",
				Tags: map[string]string{
					"user": b.Login, "product": u.Product, "sku": u.SKU,
					// Both are absent on a charge that belongs to no
					// repository and on a personal account with no
					// organization: measured on 2026-09-10, `org` was empty on
					// all 937 rows of this account and `repo` on the three
					// Copilot credit rows. An empty tag value is dropped on
					// the way into InfluxDB, which would split this
					// measurement into three series with three tag sets.
					"unit": u.UnitType,
					"repo": orNone(u.RepositoryName), "org": orNone(u.OrgName),
				},
				Fields: map[string]any{
					"quantity": u.Quantity, "price_per_unit": u.PricePerUnit,
					"gross": u.GrossAmount, "discount": u.DiscountAmount, "net": u.NetAmount,
					// The bill itself, which is the only page any of this can
					// be checked against.
					"url": "https://github.com/settings/billing/summary",
				},
				Time: day,
			})
		}
	}
	return points, nil
}
