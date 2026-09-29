package run

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// movedFake stands in for GraphQL for the three families the movement query
// can skip a repository for: it answers that query from moved, keyed by
// repository name, and every other query with an empty page, and remembers
// which repository each family asked about.
type movedFake struct {
	mu    sync.Mutex
	moved map[string]string
	fail  bool
	// asked is, per family, the repositories it sent a query about, in order;
	// gates counts the movement queries.
	asked map[string][]string
	gates int
	// open is the repositories the day's read of every open item was sent
	// for, which asks with states: OPEN and is never gated.
	open []string
}

var movedAlias = regexp.MustCompile(`(r\d+): repository\(owner: "[^"]*", name: "([^"]*)"\)`)

func (f *movedFake) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/graphql" {
		http.NotFound(w, req)
		return
	}
	var env struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(req.Body).Decode(&env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[string][]string{}
	}
	name, _ := env.Variables["name"].(string)
	switch {
	case strings.Contains(env.Query, "fragment moved on Repository"):
		f.gates++
		if f.fail {
			_, _ = w.Write([]byte(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`))
			return
		}
		var aliases []string
		for _, m := range movedAlias.FindAllStringSubmatch(env.Query, -1) {
			if answer, ok := f.moved[m[2]]; ok {
				aliases = append(aliases, fmt.Sprintf("%q:%s", m[1], answer))
			}
		}
		_, _ = w.Write([]byte(`{"data":{` + strings.Join(aliases, ",") + `}}`))
	case strings.Contains(env.Query, "history("):
		f.asked["commits"] = append(f.asked["commits"], name)
		_, _ = w.Write([]byte(`{"data":{"repository":{"defaultBranchRef":null}}}`))
	case strings.Contains(env.Query, "timelineItems(since"):
		f.asked["issueevents"] = append(f.asked["issueevents"], name)
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[]},"pullRequests":{"nodes":[]}}}}`))
	case strings.Contains(env.Query, "reviewThreads(") && strings.Contains(env.Query, "states: OPEN"):
		f.open = append(f.open, name)
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[]},"pullRequests":{"nodes":[]}}}}`))
	case strings.Contains(env.Query, "reviewThreads("):
		f.asked["issues"] = append(f.asked["issues"], name)
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[]},"pullRequests":{"nodes":[]}}}}`))
	default:
		_, _ = w.Write([]byte(`{"errors":[{"type":"UNKNOWN","message":"no answer for this query"}]}`))
	}
}

// movement is one repository's answer to the movement query.
func movement(head, items time.Time) string {
	return fmt.Sprintf(`{"defaultBranchRef":{"target":{"committedDate":%q}},`+
		`"issues":{"nodes":[{"updatedAt":%q}]},"pullRequests":{"nodes":[]}}`,
		head.UTC().Format(time.RFC3339), items.UTC().Format(time.RFC3339))
}

// movedRunner is a runner of the three families over the repositories named,
// each family last run one cadence before now and so due, and the day's read
// of every open item of issues already taken unless daily says it is due.
func movedRunner(t *testing.T, fake *movedFake, now time.Time, daily bool, names ...string) *Runner {
	t.Helper()
	r := sweepRunner(t, fake.serve)
	r.Cfg.Every = everyOnly("commits", "issues", "issueevents")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.repos = nil
	for _, name := range names {
		r.repos = append(r.repos, collect.Repo{Owner: "o", Name: name, FullName: "o/" + name})
	}
	r.reposAt = now
	r.Now = func() time.Time { return now }
	for _, family := range []string{"commits", "issues", "issueevents"} {
		r.State.Mark(family, now.Add(-time.Minute))
	}
	if !daily {
		r.State.MarkFull("issues", now)
	}
	return r
}

// TestARepositoryNothingMovedInIsNotAsked is the saving: of two
// repositories, the one the movement query says moved gets its commits, its
// pull requests and its timeline asked, and the one it says has not gets none
// of the three, for one movement query in the whole sweep.
func TestARepositoryNothingMovedInIsNotAsked(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	fake := &movedFake{moved: map[string]string{
		"busy": movement(now.Add(-time.Second), now.Add(-time.Second)),
		"idle": movement(now.Add(-time.Hour), now.Add(-time.Hour)),
	}}
	r := movedRunner(t, fake, now, false, "busy", "idle")
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"commits", "issues", "issueevents"} {
		if got := fake.asked[family]; len(got) != 1 || got[0] != "busy" {
			t.Errorf("%s asked about %v, want the repository that moved and not the idle one", family, got)
		}
		// A repository left unread is not one that failed.
		if last := r.State.LastRun[family]; !last.Equal(now) {
			t.Errorf("%s last ran at %s, want it marked at this sweep", family, last)
		}
	}
	if fake.gates != 1 {
		t.Errorf("%d movement queries for three families, want one per sweep", fake.gates)
	}

	// The next sweep asks again: an answer is only as good as the sweep it
	// was asked in.
	later := now.Add(time.Minute)
	r.Now = func() time.Time { return later }
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fake.gates != 2 {
		t.Errorf("%d movement queries over two sweeps, want one each", fake.gates)
	}

	// And a sweep none of the three runs in asks nothing: the query is only
	// ever the price of a read it can spare.
	r.Now = func() time.Time { return later.Add(time.Second) }
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fake.gates != 2 {
		t.Errorf("a sweep with none of the three families due sent a movement query, %d in all", fake.gates)
	}
}

// TestEachFamilyAsksOfTheMoveItReads: commits reads the default branch and
// the other two read issues and pull requests, so each is skipped on its own
// half of the answer. A repository pushed to with no item touched has its
// commits read and nothing else, and one whose items moved with no push the
// reverse. Asked of the other half, a push with no issue activity, or a
// comment with no push, would be left unread until it fell out of the two
// cadences every window reaches back, and then for good.
func TestEachFamilyAsksOfTheMoveItReads(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	fake := &movedFake{moved: map[string]string{
		"pushed":    movement(now.Add(-time.Second), now.Add(-time.Hour)),
		"discussed": movement(now.Add(-time.Hour), now.Add(-time.Second)),
	}}
	r := movedRunner(t, fake, now, false, "pushed", "discussed")
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	for family, want := range map[string]string{"commits": "pushed", "issues": "discussed", "issueevents": "discussed"} {
		if got := fake.asked[family]; len(got) != 1 || got[0] != want {
			t.Errorf("%s asked about %v, want only %s", family, got, want)
		}
	}
}

// TestEveryOpenItemIsReadWhateverMoved: the day's read of every open item
// exists for the items nobody touches, so a repository nothing moved in is
// exactly the one it is for. The read of what moved is gated as on any other
// sweep, beside it, against the day's own window, which reaches back a month
// on a state file with no day's read on record.
func TestEveryOpenItemIsReadWhateverMoved(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	idle := now.AddDate(0, -2, 0)
	fake := &movedFake{moved: map[string]string{
		"busy": movement(now, now),
		"idle": movement(idle, idle),
	}}
	r := movedRunner(t, fake, now, true, "busy", "idle")
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := fake.open; len(got) != 2 {
		t.Errorf("every open item was read for %v, want both repositories", got)
	}
	if got := fake.asked["issues"]; len(got) != 1 || got[0] != "busy" {
		t.Errorf("what moved was read for %v on the day's first sweep, want only the repository that moved", got)
	}
	if got := fake.asked["commits"]; len(got) != 1 || got[0] != "busy" {
		t.Errorf("commits asked about %v on the day's first sweep, want only the repository that moved", got)
	}
	// Once on record, the next sweep of the same UTC day reads what moved
	// only.
	fake.open, fake.asked = nil, nil
	r.State.Mark("issues", now.Add(-time.Hour))
	r.Now = func() time.Time { return now.Add(time.Second) }
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.asked["issues"]) == 0 {
		t.Fatal("the next sweep did not run issues, so it says nothing about the open read")
	}
	if len(fake.open) != 0 && sameUTCDay(now, now.Add(time.Second)) {
		t.Errorf("every open item was read again the same UTC day, for %v", fake.open)
	}
}

// TestNoAnswerIsReadAsBefore: a repository the query says nothing about, and
// every repository when the query fails, are asked exactly what they were
// asked before the query existed.
func TestNoAnswerIsReadAsBefore(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	cases := map[string]*movedFake{
		"a repository left out of the answer": {moved: map[string]string{
			"idle": movement(now.Add(-time.Hour), now.Add(-time.Hour)),
		}},
		"a failed query": {fail: true, moved: map[string]string{
			"idle":    movement(now.Add(-time.Hour), now.Add(-time.Hour)),
			"unnamed": movement(now.Add(-time.Hour), now.Add(-time.Hour)),
		}},
	}
	for name, fake := range cases {
		r := movedRunner(t, fake, now, false, "unnamed", "idle")
		if err := r.Once(t.Context()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, family := range []string{"commits", "issues", "issueevents"} {
			asked := strings.Join(fake.asked[family], ",")
			if !strings.Contains(asked, "unnamed") {
				t.Errorf("%s: %s asked about %q, want the repository with no answer read", name, family, asked)
			}
			if fake.fail && asked != "unnamed,idle" {
				t.Errorf("%s: %s asked about %q, want every repository read", name, family, asked)
			}
		}
	}
}

// TestAMoveAtTheStartOfTheWindowIsRead: history(since:) holds a commit
// committed at the very second it names, measured, so a head committed there
// is a commit to read, and an item updated there is an item to read.
func TestAMoveAtTheStartOfTheWindowIsRead(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	every := time.Minute
	since := now.Add(-2 * every)
	fake := &movedFake{moved: map[string]string{
		"edge":   movement(since, since),
		"before": movement(since.Add(-time.Second), since.Add(-time.Second)),
	}}
	r := movedRunner(t, fake, now, false, "edge", "before")
	if got := r.commits(now).Since; !got.Equal(since) {
		t.Fatalf("the commits window starts at %s, the test assumes %s", got, since)
	}
	if got := r.pulls(collect.Repo{FullName: "o/edge"}, now).Walk.Since; !got.Equal(since) {
		t.Fatalf("the pull request window starts at %s, the test assumes %s", got, since)
	}
	if got := r.issueEvents(now).From(now); !got.Equal(since) {
		t.Fatalf("the timeline window starts at %s, the test assumes %s", got, since)
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"commits", "issues", "issueevents"} {
		if got := fake.asked[family]; len(got) != 1 || got[0] != "edge" {
			t.Errorf("%s asked about %v, want the move on the window's first second read and the one before it not", family, got)
		}
	}
}

// TestABackfillAsksEverythingAndNotWhatMoved: a backfill is asked for once to
// read the whole history, so it sends no movement query and skips nothing.
func TestABackfillAsksEverythingAndNotWhatMoved(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	fake := &movedFake{moved: map[string]string{
		"idle": movement(now.AddDate(-1, 0, 0), now.AddDate(-1, 0, 0)),
	}}
	r := movedRunner(t, fake, now, false, "idle")
	r.Backfill, r.BackfillSince = true, now.AddDate(0, -1, 0)
	if err := r.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fake.gates != 0 {
		t.Errorf("a backfill sent %d movement queries", fake.gates)
	}
	for _, family := range []string{"commits", "issues", "issueevents"} {
		if got := fake.asked[family]; len(got) == 0 {
			t.Errorf("a backfill did not ask %s about a repository nothing moved in", family)
		}
	}
}

// TestTheFakeAccountIsAskedWhatMoved runs the query against the fake GitHub
// both end-to-end suites collect from, whose one repository's newest commit is
// four days old: the first sweep reads the month and finds it, and the next
// ordinary pass, whose window is two cadences of a minute, asks what moved and
// not the history.
func TestTheFakeAccountIsAskedWhatMoved(t *testing.T) {
	t.Parallel()
	r, fake, log := fakeRunner(t)
	r.Cfg.Every = everyOnly("commits")
	if err := r.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	commits := func(requests []fakegh.Request) (history, moved int) {
		for _, req := range requests {
			switch {
			case strings.Contains(req.GraphQL, "fragment moved on Repository"):
				moved++
			case strings.Contains(req.GraphQL, "history("):
				history++
			}
		}
		return history, moved
	}
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v\n%s", err, log)
	}
	if history, _ := commits(fake.Requests()); history == 0 {
		t.Fatal("the first sweep did not read the month of commits the fixture holds")
	}
	before := len(fake.Requests())
	every, _ := r.Cfg.Interval("commits")
	r.State.Mark("commits", time.Now().Add(-every))
	if err := r.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v\n%s", err, log)
	}
	history, moved := commits(fake.Requests()[before:])
	if moved != 1 || history != 0 {
		t.Errorf("the second sweep sent %d movement queries and %d history queries, want the one and none", moved, history)
	}
	if !strings.Contains(log.String(), "repositories left unread") {
		t.Errorf("the skip is not in the log:\n%s", log)
	}
}
