package run

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// audienceRequests counts, over one slice of the fake's log, the REST reads of
// the star and fork lists and the GraphQL batches that replace them.
func audienceRequests(requests []fakegh.Request) (stargazers, forks, batches int) {
	for _, req := range requests {
		switch {
		case strings.HasSuffix(req.Path, "/stargazers"):
			stargazers++
		case strings.HasSuffix(req.Path, "/forks"):
			forks++
		case strings.Contains(req.GraphQL, "fragment audience on Repository"):
			batches++
		}
	}
	return stargazers, forks, batches
}

// TestStarsAndForksAreBatchedOnceWalked runs the two families three times in
// one process: the first sweep walks both lists through REST, page by page,
// because the repository has never been seen and the install is fresh; the
// next ordinary sweep asks GraphQL for the newest hundred of each in one
// batch and reads no REST list at all; a backfill walks REST again.
func TestStarsAndForksAreBatchedOnceWalked(t *testing.T) {
	t.Parallel()
	r, fake, log := fakeRunner(t)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	stargazers, forks, batches := audienceRequests(fake.Requests())
	if stargazers == 0 || forks == 0 {
		t.Fatalf("the first sweep read the stargazers %d times and the forks %d, want both walked", stargazers, forks)
	}
	if batches != 0 {
		t.Errorf("the first sweep sent %d batches for lists it had not walked yet", batches)
	}

	// Only stars and forks are due again: everything else was marked a
	// moment ago.
	before := len(fake.Requests())
	for _, family := range []string{"stars", "forks"} {
		every, _ := r.Cfg.Interval(family)
		r.State.LastRun[family] = time.Now().Add(-2 * every)
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v\n%s", err, log)
	}
	stargazers, forks, batches = audienceRequests(fake.Requests()[before:])
	if stargazers != 0 || forks != 0 {
		t.Errorf("an ordinary sweep read the stargazers %d times and the forks %d through REST, want the batch", stargazers, forks)
	}
	if batches != 2 {
		t.Errorf("an ordinary sweep sent %d batches, want one per family", batches)
	}
	if strings.Contains(log.String(), "batched collector failed") {
		t.Errorf("the batch failed:\n%s", log)
	}

	before = len(fake.Requests())
	for _, family := range []string{"stars", "forks"} {
		every, _ := r.Cfg.Interval(family)
		r.State.LastRun[family] = time.Now().Add(-2 * every)
	}
	r.Backfill, r.BackfillSince = true, time.Now().AddDate(-1, 0, 0)
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("backfill Once: %v\n%s", err, log)
	}
	stargazers, forks, batches = audienceRequests(fake.Requests()[before:])
	if stargazers == 0 || forks == 0 || batches != 0 {
		t.Errorf("a backfill read the stargazers %d times, the forks %d and sent %d batches, want REST only", stargazers, forks, batches)
	}
}

// audienceRunner is a runner over handler with only the forks family enabled,
// past its first sweep and with its one repository already seen, which is the
// state in which the batch serves the family.
func audienceRunner(t *testing.T, handler http.HandlerFunc) *Runner {
	t.Helper()
	r := sweepRunner(t, handler)
	r.Cfg.Every = everyOnly("forks")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.State.LastRun["forks"] = time.Now().Add(-time.Hour)
	r.State.FirstSaw["o/n"] = time.Now().Add(-time.Hour)
	return r
}

// TestAForksBatchThatFailsLeavesTheFamilyUnmarked: once a repository has been
// walked, the batch is the whole of the forks family for it, and a sweep
// whose batch failed collected nothing. Marking the family would hide that
// until its next cadence, half a day, the way a family that failed on every
// repository would be hidden.
func TestAForksBatchThatFailsLeavesTheFamilyUnmarked(t *testing.T) {
	t.Parallel()
	r := audienceRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	before := r.State.LastRun["forks"]
	if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if when := r.State.LastRun["forks"]; !when.Equal(before) {
		t.Errorf("a sweep whose batch failed marked forks as run at %s", when)
	}
}

// answerForksBatch answers the audience batch with one fork and the total
// the caller names, and every REST request with an empty page, counting the
// reads of the fork list.
func answerForksBatch(total int, walked *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/graphql" {
			_, _ = fmt.Fprintf(w, `{"data":{"r0":{"forks":{"totalCount":%d,"nodes":[{"nameWithOwner":"a/n",`+
				`"createdAt":"2025-02-10T10:00:00Z","pushedAt":"2025-02-20T10:00:00Z","stargazerCount":2,`+
				`"url":"https://github.com/a/n","owner":{"login":"a"}}]}}}}`, total)
			return
		}
		if strings.HasSuffix(req.URL.Path, "/forks") {
			walked.Add(1)
		}
		_, _ = w.Write([]byte("[]"))
	}
}

// TestARepositoryWithMoreForksThanTheBatchReadsIsStillWalked: the batch reads
// the newest hundred, and a fork row past them carries a star count and a
// days_since_push the REST walk used to refresh on up to five hundred forks a
// sweep. A repository the batch reports as holding more than its page is
// walked the old way in the same sweep; one that fits is not.
func TestARepositoryWithMoreForksThanTheBatchReadsIsStillWalked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		total  int
		walked int32
	}{{total: 150, walked: 1}, {total: 2, walked: 0}} {
		var walked atomic.Int32
		r := audienceRunner(t, answerForksBatch(tc.total, &walked))
		if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := walked.Load(); got != tc.walked {
			t.Errorf("with %d forks the REST list was read %d times, want %d", tc.total, got, tc.walked)
		}
		if _, marked := r.State.LastRun["forks"]; !marked {
			t.Errorf("with %d forks the family was not marked as run", tc.total)
		}
	}
}

// TestABatchThatFailsLeavesUnmarkedOnlyTheFamiliesItWas: a failed batch is
// the whole of branches, which collects nowhere else, and the whole of stars
// for a repository already walked, so both are left unmarked the way a family
// that failed on every repository is. It is none of repo, whose per-repository
// read asks the same thing again, and that family is still marked.
func TestABatchThatFailsLeavesUnmarkedOnlyTheFamiliesItWas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		family string
		marked bool
	}{{"stars", false}, {"branches", false}, {"repo", true}} {
		r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/graphql" {
				// An error GraphQL answers with a 200, which no batch skips:
				// a 5xx reads as a query too large, and some batches shrink
				// and carry on from that.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
				return
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		})
		r.Cfg.Every = everyOnly(tc.family)
		if err := r.Cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		log, buf := debugLog()
		r.Log = log
		r.State.LastRun[tc.family] = time.Now().Add(-time.Hour)
		r.State.FirstSaw["o/n"] = time.Now().Add(-time.Hour)
		before := r.State.LastRun[tc.family]
		if err := r.repoFamilies(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), `msg="batched collector failed" family=`+tc.family) {
			t.Fatalf("%s: the batch did not fail, so this case proves nothing:\n%s", tc.family, buf)
		}
		if marked := !r.State.LastRun[tc.family].Equal(before); marked != tc.marked {
			t.Errorf("%s: a sweep whose batch failed was marked %v, want %v", tc.family, marked, tc.marked)
		}
	}
}
