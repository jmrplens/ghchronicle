package collect

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/sink"
)

func TestBillingParsesRFC3339Dates(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/settings/billing/usage", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("month") == "8" {
			// The documented shape is a bare date; both must parse.
			b := string(fixture(t, "billing_usage.json"))
			b = strings.ReplaceAll(b, "2026-09-01T00:58:36Z", "2026-08-03")
			b = strings.ReplaceAll(b, "2026-09-02T00:00:00Z", "2026-08-04")
			_, _ = w.Write([]byte(b))
			return
		}
		f.write(w, "billing_usage.json")
	})

	points, err := Billing{Login: "octocat", Months: 2}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/users/octocat/settings/billing/usage")
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want one per month", len(calls))
	}
	if calls[0].Query["year"] != "2026" || calls[0].Query["month"] != "9" || calls[1].Query["month"] != "8" {
		t.Errorf("months asked for: %v %v", calls[0].Query, calls[1].Query)
	}
	if len(points) != 4 {
		t.Fatalf("got %d points, want 2 rows for each of 2 months", len(points))
	}
	checkActionsCharge(t, points)
	copilot := find(t, points, "gh_billing_usage", map[string]string{"product": "Copilot"})
	if copilot.Fields["net"] != 10.0 {
		t.Errorf("net is not always zero, got %v", copilot.Fields["net"])
	}
	// A seat is charged to no repository, so all three of the tags that name
	// one say so rather than being left out: an empty tag value is dropped on
	// the way into InfluxDB and those rows would land in a series of their own.
	// Naming the account as the owner would be an invention, since the charge
	// is not about a repository at all.
	for _, tag := range []string{"owner", "repo", "full_name"} {
		if copilot.Tags[tag] != noneTag {
			t.Errorf("a charge that belongs to no repository names none of the three, got %v", copilot.Tags)
		}
	}
	// `org` was the same dimension as `owner` with the personal half left
	// blank, so it is gone and `owner` carries it in both cases.
	if _, isTag := copilot.Tags["org"]; isTag {
		t.Errorf("org is folded into owner, got %v", copilot.Tags)
	}
	checkBareDates(t, points)
}

// checkActionsCharge reads the September Actions row, whose date is
// documented as a date and delivered as a full RFC 3339 timestamp carrying
// the first billed minute of the day; the row lands on the day itself, so a
// re-read converges whatever minute GitHub reports next time.
func checkActionsCharge(t *testing.T, points []sink.Point) {
	t.Helper()
	actions := find(t, points, "gh_billing_usage", map[string]string{"product": "Actions", "repo": "hello-world"})
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); !actions.Time.Equal(want) {
		t.Errorf("RFC 3339 date stamped %s, want the start of the day %s", actions.Time, want)
	}
	// The casing is the assertion, not an accident of the fixture. GitHub
	// answers unitType with a capital ("Minutes", "GigabyteHours",
	// "AICredits"), the collector passes it through, and the cost panels
	// filter on `unit = 'Minutes'`. The fixture used to say "minutes", which
	// made those panels answer nothing in the containerised suite and read as
	// a defect in the panels for as long as it stood.
	// The organization GitHub names on the row is the owner of the repository
	// that burned the minutes, which is what `owner` carries now.
	if actions.Tags["sku"] != "Actions Linux" || actions.Tags["unit"] != "Minutes" ||
		actions.Tags["owner"] != "octocat" || actions.Tags["full_name"] != "octocat/hello-world" ||
		actions.Tags["user"] != "octocat" {
		t.Errorf("billing tags = %v", actions.Tags)
	}
	if actions.Fields["quantity"] != 214.0 || actions.Fields["gross"] != 1.712 || actions.Fields["discount"] != 1.712 || actions.Fields["net"] != 0.0 {
		t.Errorf("billing fields = %v", actions.Fields)
	}
}

// checkBareDates reads the August rows, which the fixture serves in the
// documented bare date form, and wants each at midnight of its own day.
func checkBareDates(t *testing.T, points []sink.Point) {
	t.Helper()
	var bareDates int
	for _, p := range only(t, points, "gh_billing_usage") {
		if p.Time.Month() == time.August {
			bareDates++
			if p.Time.Hour() != 0 {
				t.Errorf("bare date stamped %s", p.Time)
			}
		}
	}
	if bareDates != 2 {
		t.Errorf("got %d rows parsed from bare dates, want 2", bareDates)
	}
}

func TestBillingUnavailableMonthIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/settings/billing/usage", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("month") == "9" {
			f.write(w, "billing_usage.json")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	points, err := Billing{Login: "octocat", Months: 3}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a month GitHub no longer has is not an error: %v", err)
	}
	if len(points) != 2 {
		t.Errorf("got %d points, want the 2 of the month that answered", len(points))
	}
	if n := len(f.calls("/users/octocat/settings/billing/usage")); n != 3 {
		t.Errorf("made %d calls, want 3", n)
	}
}
