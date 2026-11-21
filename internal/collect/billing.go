package collect

import (
	"context"
	"fmt"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
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
	// Orgs are the organizations whose own report is read beside the
	// account's, into gh_org_billing_usage. A repository an organization owns
	// is billed to that organization and never appears in the account's
	// report: on 2026-11-21 kleidos-dev had burned 1,866 Actions minutes that
	// month, and the account's report named none of its repositories. It is a
	// measurement of its own because it is another bill, paid by somebody
	// else, and the account's cost panels add up gh_billing_usage. The
	// organization form answers its owners alone; to anyone else it is a 404,
	// read as a month with nothing in it.
	Orgs []string
	// Months is how far back to walk, counting the current one.
	Months int
	// Walk widens it: a bounded Since walks back to that month, an unbounded
	// one keeps going until three consecutive months come back empty, which
	// is how GitHub says it has nothing older.
	Walk Walk
}

// billingReport is one usage report: whose bill it is, whether that is an
// organization's, the path that answers it, and the page it is read on.
type billingReport struct {
	who, path, page string
	org             bool
}

// point is one row of the report: gh_billing_usage tagged with the account
// as `user`, or gh_org_billing_usage tagged with the organization as `org`.
func (r billingReport) point(tags map[string]string, fields map[string]any, day time.Time) sink.Point {
	if r.org {
		return sink.Point{Measurement: "gh_org_billing_usage", Tags: merge(tags, map[string]string{"org": r.who}), Fields: fields, Time: day}
	}
	return sink.Point{Measurement: "gh_billing_usage", Tags: merge(tags, map[string]string{"user": r.who}), Fields: fields, Time: day}
}

func (b Billing) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	reports := []billingReport{{
		who:  b.Login,
		path: "/users/" + b.Login + "/settings/billing/usage",
		page: "https://github.com/settings/billing/summary",
	}}
	for _, org := range b.Orgs {
		reports = append(reports, billingReport{
			who: org, org: true,
			path: "/organizations/" + org + "/settings/billing/usage",
			page: "https://github.com/organizations/" + org + "/settings/billing/summary",
		})
	}
	var points []sink.Point
	for _, r := range reports {
		more, err := b.report(ctx, c, r, now)
		points = append(points, more...)
		if err != nil {
			return points, err
		}
	}
	return points, nil
}

// report walks one report back month by month.
func (b Billing) report(ctx context.Context, c *ghapi.Client, r billingReport, now time.Time) ([]sink.Point, error) {
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
		path := fmt.Sprintf("%s?year=%d&month=%d", r.path, t.Year(), int(t.Month()))
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
				// Sent by the organization form of the report alone, where
				// it names the organization asked about: see billingRepoTags.
				OrgName string `json:"organizationName"`
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
			points = append(points, r.point(
				merge(billingRepoTags(r.who, u.OrgName, u.RepositoryName), map[string]string{
					"product": u.Product, "sku": u.SKU, "unit": u.UnitType,
				}),
				map[string]any{
					"quantity": u.Quantity, "price_per_unit": u.PricePerUnit,
					"gross": u.GrossAmount, "discount": u.DiscountAmount, "net": u.NetAmount,
					// The bill itself, which is the only page any of this can
					// be checked against.
					"url": r.page,
				},
				day,
			))
		}
	}
	return points, nil
}

// billingRepoTags names the repository a charge belongs to, in the one shape
// every other measurement names one in.
//
// `organizationName` is sent by the organization form of the report alone,
// where it is required and names the organization asked about, the owner of
// every repository in it; the account's form never sends it, absent rather
// than empty, and bills the login. So the organization, where there is one,
// is the owner, and otherwise the account is.
//
// A charge that belongs to no repository names none of the three. Those rows
// are the monthly Copilot credit, three of the 937 measured, and they are not
// about a repository, so saying the account owns one would be an invention
// rather than a fallback.
func billingRepoTags(login, org, repo string) map[string]string {
	if repo == "" {
		return repoTags("", "")
	}
	if org != "" {
		return repoTags(org, repo)
	}
	return repoTags(login, repo)
}
