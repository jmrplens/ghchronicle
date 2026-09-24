package collect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// TestTheReasonAFailureIsFiledUnderIsBounded: the reason is a tag, and a tag
// built from a message is one series per failed call for ever, since the
// message carries the request and the request carries a run id.
func TestTheReasonAFailureIsFiledUnderIsBounded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nothing failed", nil, noneTag},
		{"the gateway gave up", &ghapi.StatusError{
			Path: "/repos/o/r/actions/runs/35082535901/jobs", Code: 502, Status: "502 Bad Gateway",
		}, "502"},
		{"a feature that is off", &ghapi.UnavailableError{Path: "/p", Status: 403}, "403"},
		{"a spent budget", &ghapi.RateLimitedError{Path: "/p", Resource: "core"}, "rate limited"},
		{"a query too large", &ghapi.TooLargeError{Status: 502}, "query too large"},
		{"a number GitHub is still computing", &ghapi.NotReadyError{Path: "/p"}, "not ready"},
		{"a sweep that was stopped", context.Canceled, "canceled"},
		{"a request that ran out of time", context.DeadlineExceeded, "timed out"},
		{"an errors array from GraphQL", errors.New("graphql: INTERNAL: something went wrong"), "other"},
		{"one of several", errors.Join(
			errors.New("graphql: INTERNAL: something went wrong"),
			&ghapi.StatusError{Path: "/p", Code: 502, Status: "502 Bad Gateway"},
		), "502"},
	} {
		if got := FailureReason(tc.err); got != tc.want {
			t.Errorf("%s: FailureReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestASweepReportsEveryFamilyItRanAndEveryRepositoryItLost pins the shape the
// dashboards read: one row per family, always, and one more per repository a
// family could not collect, under the same three tags that name a repository
// everywhere else.
func TestASweepReportsEveryFamilyItRanAndEveryRepositoryItLost(t *testing.T) {
	t.Parallel()
	broken := Repo{Owner: "jmrplens", Name: "phonometry", FullName: "jmrplens/phonometry"}
	points := CollectorPoints([]FamilyRun{
		{Family: "traffic", Repos: 59, Points: 812},
		{
			Family: "actions", Repos: 59, Failed: 1, Points: 4021,
			Failures: []RepoFailure{{Repo: broken, Err: &ghapi.StatusError{
				Path: "/repos/jmrplens/phonometry/actions/runs/35082535901/jobs",
				Code: 502, Status: "502 Bad Gateway",
			}}},
		},
	}, testNow)

	if len(points) != 3 {
		t.Fatalf("got %d rows, want one per family and one for the repository lost: %v", len(points), points)
	}
	for _, p := range points {
		if p.Measurement != "gh_collector_family" {
			t.Errorf("row of %s, want them all in one measurement", p.Measurement)
		}
		if !p.Time.Equal(testNow) {
			t.Errorf("%v is dated %s, want the sweep's own instant", p.Tags, p.Time)
		}
		for _, key := range []string{"family", "scope", "reason", "owner", "repo", "full_name"} {
			if p.Tags[key] == "" {
				t.Errorf("%v carries no %s, and a tag written on some rows only misplaces every node after it in Graphite", p.Tags, key)
			}
		}
	}

	quiet := points[0]
	if quiet.Tags["family"] != "traffic" || quiet.Tags["scope"] != scopeFamily {
		t.Errorf("the first row is %v, want the traffic family", quiet.Tags)
	}
	if quiet.Tags["reason"] != noneTag || quiet.Tags["repo"] != noneTag {
		t.Errorf("a family that failed on nothing is filed under %v", quiet.Tags)
	}
	// The one field that would otherwise be written on a failure alone. A
	// column no point has ever carried does not exist, and InfluxDB refuses a
	// query that names one rather than answering it with no rows, so the panel
	// listing the failures would be broken on every account that has none.
	if quiet.Fields["error"] != noneTag {
		t.Errorf("a family that failed on nothing carries error=%v, want the sentinel that "+
			"creates the column", quiet.Fields["error"])
	}
	if quiet.Fields["failed"] != 0 || quiet.Fields["repos"] != 59 || quiet.Fields["points"] != 812 {
		t.Errorf("the traffic row says %v, want 59 repositories, none failed, 812 rows", quiet.Fields)
	}

	lost := points[2]
	if lost.Tags["scope"] != scopeRepo || lost.Tags["full_name"] != "jmrplens/phonometry" {
		t.Errorf("the failure row is %v, want the repository it names", lost.Tags)
	}
	if lost.Tags["reason"] != "502" || lost.Fields["failed"] != 1 {
		t.Errorf("the failure row is %v / %v, want one 502", lost.Tags, lost.Fields)
	}
	if text, _ := lost.Fields["error"].(string); text == "" {
		t.Error("the failure row carries no message, which is the only place the request is named")
	}
}

// TestASweepThatRanNothingSaysNothing: absence is what says a family did not
// run, so an empty sweep writes no row rather than a row of zeroes.
func TestASweepThatRanNothingSaysNothing(t *testing.T) {
	t.Parallel()
	if points := CollectorPoints(nil, time.Now()); len(points) != 0 {
		t.Errorf("got %d rows from a sweep that ran no family", len(points))
	}
}
