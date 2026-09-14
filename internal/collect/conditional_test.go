package collect

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// The conditional-request cache stores what a collector decoded, encoded
// again, and answers a 304 by decoding that. It is a fraction of the bytes
// GitHub sent, which is the point, and it is the same value only while every
// type a collector decodes into encodes what it decodes. A `json:"-"` field,
// a custom UnmarshalJSON without its MarshalJSON twin, or a field read but
// never written back would make the 304 answer with less than the 200 did,
// and nothing between the client and the sink would say so: the sweep would
// write fewer points, or points with zeroes in them, from the second sweep
// onwards, and only from the second.
//
// So every REST collector is run twice here against the fake GitHub the
// end-to-end suites use, which answers the repeat 304, and the two runs have
// to produce the same points. The other half of the promise, that a 304 is
// answered from an entry written by the same type, is the test at the end
// of this file: the one URL two collectors decode differently. The fake's route table is the one the suites
// share, imported rather than copied, so a family whose fixtures the suites
// gain is covered here the same day.

// conditionalFixtures is the end-to-end fixture directory, which describes
// testRepo under the login the fake serves.
const conditionalFixtures = "../../test/e2e/testdata"

// restCollectors is every collector that asks REST for something the fake
// answers, each with the options a sweep gives it. A collector missing here
// is one whose types nobody checks. Left out on purpose: the GraphQL-only
// ones, because a POST has no validator and the cache never sees it, and the
// ones whose REST calls the fixture set answers 404 (Account's profile, Keys,
// RateLimit, and the one search Totals still makes), because a 404 carries no
// validator either. Every one is taken by
// address, which satisfies the interface whichever receiver its Collect has.
func restCollectors(now time.Time) []struct {
	name string
	run  func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error)
} {
	repo := func(col interface {
		Collect(context.Context, *ghapi.Client, Repo, time.Time) ([]sink.Point, error)
	},
	) func(context.Context, *ghapi.Client) ([]sink.Point, error) {
		return func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
			return col.Collect(ctx, c, testRepo, now)
		}
	}
	account := func(col interface {
		Collect(context.Context, *ghapi.Client, time.Time) ([]sink.Point, error)
	},
	) func(context.Context, *ghapi.Client) ([]sink.Point, error) {
		return func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error) {
			return col.Collect(ctx, c, now)
		}
	}
	since := now.Add(-30 * 24 * time.Hour)
	return []struct {
		name string
		run  func(ctx context.Context, c *ghapi.Client) ([]sink.Point, error)
	}{
		{"Actions", repo(&Actions{Since: since, Jobs: true})},
		{"Analyses", repo(&Analyses{})},
		{"Artifacts", repo(&Artifacts{})},
		{"Billing", account(&Billing{Login: fakegh.Login, Months: 2})},
		{"Events", account(&Events{Login: fakegh.Login})},
		{"Forks", repo(&Forks{})},
		{"JobLogs", repo(&JobLogs{Since: since})},
		{"Notifications", account(&Notifications{})},
		{"Profile", account(&Profile{Login: fakegh.Login})},
		{"RepoActivity", repo(&RepoActivity{})},
		{"RepoActivityLog", repo(&RepoActivityLog{})},
		{"RepoCore", repo(&RepoCore{})},
		{"RepoInventory", repo(&RepoInventory{})},
		{"RulesetHistory", repo(&RulesetHistory{})},
		{"Security", repo(&Security{})},
		{"Settings", repo(&Settings{})},
		{"Stargazers", repo(&Stargazers{Full: true})},
		{"Traffic", repo(&Traffic{})},
	}
}

func TestEveryRESTCollectorAnswersA304AsItAnsweredThe200(t *testing.T) {
	t.Parallel()
	// One clock for both runs, so the only thing that can differ between
	// them is what the cache replayed. A window relative to it keeps the
	// fixtures spelled "@NOW@" inside every collector's horizon.
	now := time.Now().UTC()
	for _, tc := range restCollectors(now) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := fakegh.New(t, conditionalFixtures)
			c := ghapi.New("test-token", 0)
			c.SetBaseURL(gh.URL())

			first, err := tc.run(ctx(t), c)
			if err != nil {
				t.Fatalf("first run: %v", err)
			}
			asked := len(gh.Requests())
			second, err := tc.run(ctx(t), c)
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			assertTheRepeatWasAnswered304(t, gh.Requests(), asked)

			if len(first) == 0 {
				t.Fatalf("%s collected nothing, so there is nothing to compare", tc.name)
			}
			sortPoints(first)
			sortPoints(second)
			if len(first) != len(second) {
				t.Fatalf("the 200 gave %d points and the 304 %d:\n200: %v\n304: %v",
					len(first), len(second), measurements(first), measurements(second))
			}
			for i := range first {
				if !reflect.DeepEqual(first[i], second[i]) {
					t.Errorf("point %d differs between the 200 and the 304:\n200: %s\n304: %s",
						i, sink.LineProtocol(first[i]), sink.LineProtocol(second[i]))
				}
			}
		})
	}
}

// assertTheRepeatWasAnswered304 checks that the second run was actually the
// replay it is meant to be: every GET the first run was answered 200 for
// came back 304 when asked again, and at least one was. A collector whose
// second run never earned a 304 has not exercised the cache, and a parity it
// then reports is no parity at all.
func assertTheRepeatWasAnswered304(t *testing.T, reqs []fakegh.Request, split int) {
	t.Helper()
	answered := map[string]int{}
	for _, r := range reqs[:split] {
		answered[r.Path+"?"+r.Query] = r.Status
	}
	replayed := 0
	for _, r := range reqs[split:] {
		if r.Method != http.MethodGet || answered[r.Path+"?"+r.Query] != http.StatusOK {
			continue
		}
		// The job log is fetched through a redirect to object storage and
		// read as text, which the cache does not hold: the only answer the
		// fake gives that is not an API answer.
		if strings.HasPrefix(r.Path, "/storage/") {
			continue
		}
		if r.Status != http.StatusNotModified {
			t.Errorf("GET %s?%s was answered %d on the repeat, want 304", r.Path, r.Query, r.Status)
		}
		replayed++
	}
	if replayed == 0 {
		t.Errorf("the second run repeated none of the first run's %d requests, so nothing was replayed", split)
	}
}

// sortPoints orders points by their line protocol, which is a total order
// over measurement, tags, fields and time, so two runs that emit the same
// points in a different order compare equal.
func sortPoints(points []sink.Point) {
	slices.SortFunc(points, func(a, b sink.Point) int {
		return strings.Compare(sink.LineProtocol(a), sink.LineProtocol(b))
	})
}

func TestARepositoryNamedInTargetsIsNotAnsweredFromDiscoverysEntry(t *testing.T) {
	t.Parallel()
	// The one URL two collectors decode into two types: discovery reads four
	// flags out of GET /repos/{owner}/{repo} for every repository named in
	// targets.repos, and RepoCore reads forty fields out of the same URL a
	// moment later in the same sweep. The cache stores what a caller decoded,
	// so RepoCore's request must not be answered from discovery's entry, or
	// every field discovery did not keep is a zero on the first sweep and on
	// every sweep until the repository changes.
	now := time.Now().UTC()
	alone := func() []sink.Point {
		gh := fakegh.New(t, conditionalFixtures)
		c := ghapi.New("test-token", 0)
		c.SetBaseURL(gh.URL())
		points, err := RepoCore{}.Collect(ctx(t), c, testRepo, now)
		if err != nil {
			t.Fatalf("RepoCore alone: %v", err)
		}
		return points
	}()

	gh := fakegh.New(t, conditionalFixtures)
	c := ghapi.New("test-token", 0)
	c.SetBaseURL(gh.URL())
	found, err := Discover(ctx(t), c, &Filter{Repos: []string{testRepo.FullName}})
	if err != nil || len(found.Repos) != 1 {
		t.Fatalf("Discover: %v, %d repositories", err, len(found.Repos))
	}
	after, err := RepoCore{}.Collect(ctx(t), c, found.Repos[0], now)
	if err != nil {
		t.Fatalf("RepoCore after discovery: %v", err)
	}

	sortPoints(alone)
	sortPoints(after)
	if len(alone) != len(after) {
		t.Fatalf("RepoCore gave %d points alone and %d after discovery", len(alone), len(after))
	}
	for i := range alone {
		if !reflect.DeepEqual(alone[i], after[i]) {
			t.Errorf("point %d differs:\nalone: %s\nafter: %s",
				i, sink.LineProtocol(alone[i]), sink.LineProtocol(after[i]))
		}
	}
}
