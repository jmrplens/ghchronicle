package collect

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

const cachesPath = "/repos/octocat/hello-world/actions/caches"

// codeQLCache is the prefix every CodeQL overlay cache of the fixture shares:
// the sixteen hex digits after it are the one segment cachePrefix cuts at,
// and the language, version and commit that tell two builds apart all come
// after them.
const codeQLCache = "codeql-overlay-base-database-1"

// cacheFixtureServer answers the run list and both cache calls, with the
// listing served by listing.
func cacheFixtureServer(t *testing.T, listing http.HandlerFunc) *fixtureServer {
	t.Helper()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/actions/runs", "actions_runs.json")
	f.file("/repos/octocat/hello-world/actions/cache/usage", "actions_cache.json")
	f.handle(cachesPath, listing)
	return f
}

// identity is what a store keys a row by: the measurement, the tag set and
// the timestamp. Two points with the same one are one row in InfluxDB, one
// document in Elasticsearch and one entry in the write ledger.
func identity(p sink.Point) string {
	keys := make([]string, 0, len(p.Tags))
	for k := range p.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(p.Measurement)
	for _, k := range keys {
		fmt.Fprintf(&b, ",%s=%s", k, p.Tags[k])
	}
	fmt.Fprintf(&b, " %d", p.Time.UnixNano())
	return b.String()
}

// sharedIdentities lists every identity more than one point of a pass carries.
func sharedIdentities(points []sink.Point) []string {
	seen := map[string]int{}
	for _, p := range points {
		seen[identity(p)]++
	}
	var out []string
	for id, n := range seen {
		if n > 1 {
			out = append(out, fmt.Sprintf("%d x %s", n, id))
		}
	}
	sort.Strings(out)
	return out
}

// TestActionsCacheEntriesOfOneCacheOnOneRefAreOneRow reads a listing shaped
// like the one this was found on: jmrplens/jmrplens on 2026-09-26 held fifteen
// CodeQL overlay caches on main, 57.9 MB between them, and every one of them
// cut to the same `cache` tag. Written a row per entry at the start of the
// day, they were fifteen writes of one row, and the store kept whichever came
// last, about 3.8 MB. So the entries of one cache on one ref are summed into
// one row before they leave the collector.
func TestActionsCacheEntriesOfOneCacheOnOneRefAreOneRow(t *testing.T) {
	t.Parallel()
	f := cacheFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "actions_caches_codeql.json"))
	})
	points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if shared := sharedIdentities(points); len(shared) > 0 {
		t.Errorf("points of one pass share a row: %v", shared)
	}
	if n := len(only(t, points, "gh_actions_cache_entry")); n != 3 {
		t.Fatalf("got %d cache rows, want one per cache and ref: 3", n)
	}

	main := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": codeQLCache, "ref": "refs/heads/main"})
	for field, want := range map[string]int64{
		"caches":     3,
		"size_bytes": 3871480 + 3801875 + 3835985,
		// The newest of the three was used three hours before the sweep;
		// the oldest was created six and a half days before it.
		"days_since_use": 0,
		"age_days":       6,
	} {
		if got := fieldInt(t, main, field); got != want {
			t.Errorf("%s = %d, want %d", field, got, want)
		}
	}
	if want := "codeql-overlay-base-database-1-d953d79b74456ce0-python-2.27.1-03c98ef912a921acf88122e355668982b233ba79-36241304048-1"; main.Fields["key"] != want {
		t.Errorf("key = %v, want the most recently used one, %s", main.Fields["key"], want)
	}
	if want := startOfDay(testNow); !main.Time.Equal(want) {
		t.Errorf("stamped %s, want the start of the day %s", main.Time, want)
	}

	// The same cache on another ref is another row, and so is another cache
	// on the same ref.
	pull := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": codeQLCache, "ref": "refs/pull/7/merge"})
	if fieldInt(t, pull, "caches") != 1 || fieldInt(t, pull, "size_bytes") != 3780981 || fieldInt(t, pull, "days_since_use") != 3 {
		t.Errorf("the pull request's CodeQL cache = %v", pull.Fields)
	}
	python := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": "setup-python-Linux-x64-24.04-Ubuntu-python-3.14.7-pip"})
	if fieldInt(t, python, "caches") != 1 || fieldInt(t, python, "age_days") != 27 || python.Tags["ref"] != "refs/heads/main" {
		t.Errorf("the pip cache = %v %v", python.Tags, python.Fields)
	}
}

// TestActionsCacheRowsDoNotDependOnTheListingOrder serves the same entries in
// the opposite order, with two entries of one cache on one ref last used at
// the same instant. The row has to be the same whichever page each entry
// arrived on, `key` included: a row that changed with the order would be a
// new value to write on a pass that saw nothing new. The tie is what the id
// is for, and the newer id is the one the row names in both orders.
func TestActionsCacheRowsDoNotDependOnTheListingOrder(t *testing.T) {
	t.Parallel()
	const newer = "codeql-overlay-base-database-1-d953d79b74456ce0-python-2.27.1-03c98ef912a921acf88122e355668982b233ba79-36241304048-1"
	tied := tiedCaches(t, fixture(t, "actions_caches_codeql.json"), 8150218760, 8138470109)
	listing := func(reverse bool) []sink.Point {
		t.Helper()
		f := cacheFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			body := tied
			if reverse {
				body = reversedCaches(t, body)
			}
			_, _ = w.Write(body)
		})
		points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
		}
		row := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": codeQLCache, "ref": "refs/heads/main"})
		if row.Fields["key"] != newer {
			t.Errorf("reversed %v: key = %v, want the newer id's of the two used at once, %s", reverse, row.Fields["key"], newer)
		}
		return byMeasurement(points)["gh_actions_cache_entry"]
	}
	forward, backward := listing(false), listing(true)
	if len(forward) != len(backward) {
		t.Fatalf("%d rows one way and %d the other", len(forward), len(backward))
	}
	lines := func(points []sink.Point) []string {
		out := make([]string, 0, len(points))
		for _, p := range points {
			out = append(out, sink.LineProtocol(p))
		}
		sort.Strings(out)
		return out
	}
	if a, b := lines(forward), lines(backward); !slices.Equal(a, b) {
		t.Errorf("the rows changed with the order of the listing:\n%s\nagainst\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
}

// tiedCaches is a cache listing in which the entry with id other was last
// used at the same instant as the one with id to.
func tiedCaches(t *testing.T, body []byte, to, other int64) []byte {
	t.Helper()
	var listing struct {
		Total  int              `json:"total_count"`
		Caches []map[string]any `json:"actions_caches"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	var used any
	for _, e := range listing.Caches {
		if e["id"] == float64(to) {
			used = e["last_accessed_at"]
		}
	}
	tied := 0
	for _, e := range listing.Caches {
		if e["id"] == float64(other) {
			e["last_accessed_at"] = used
			tied++
		}
	}
	if used == nil || tied != 1 {
		t.Fatalf("the listing has no entries %d and %d to tie", to, other)
	}
	out, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// reversedCaches is a cache listing with its entries in the opposite order.
func reversedCaches(t *testing.T, body []byte) []byte {
	t.Helper()
	var listing map[string]any
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	entries, _ := listing["actions_caches"].([]any)
	slices.Reverse(entries)
	out, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAnIdlePassOffersTheLedgerNothingNew runs the family twice over the same
// answers through the write ledger. The second pass has nothing new to say,
// so nothing of it may go out. The ledger keys a point by its identity and
// remembers one value for it, so of k entries sharing an identity it could
// hold only one and sent the other k-1 again on every pass: on the account
// this was measured on, 269 rows every fifteen minutes that carried nothing.
func TestAnIdlePassOffersTheLedgerNothingNew(t *testing.T) {
	t.Parallel()
	f := cacheFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "actions_caches_codeql.json"))
	})
	ledger := sink.LoadLedger("", 0, 0)
	pass := func() []sink.Point {
		t.Helper()
		points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
		}
		keep, commit := ledger.Reserve("influxdb", points)
		commit()
		return keep
	}
	if first := pass(); len(byMeasurement(first)["gh_actions_cache_entry"]) == 0 {
		t.Fatal("the first pass wrote no cache rows")
	}
	if again := pass(); len(again) > 0 {
		var what []string
		for _, p := range again {
			what = append(what, identity(p))
		}
		t.Errorf("an idle pass wrote %d points: %v", len(again), what)
	}
}

// cachePage is n cache entries of one cache on main, ids counting down from
// first and each created and used a minute before the one above it, the way
// GitHub lists them newest created first, from a listing of listed entries.
func cachePage(t *testing.T, n int, first int64, listed int) []byte {
	t.Helper()
	page := repeat(t, "actions_caches_codeql.json", "actions_caches", n, func(i int, row map[string]any) {
		id := first - int64(i)
		used := testNow.Add(-time.Duration(first-id) * time.Minute)
		row["id"] = id
		row["key"] = codeQLCache + "-d953d79b74456ce0-python-2.27.1-" + strconv.FormatInt(id, 16)
		row["last_accessed_at"] = used.Format(time.RFC3339)
		row["created_at"] = used.Format(time.RFC3339)
		row["size_in_bytes"] = 1000
	})
	var body map[string]any
	if err := json.Unmarshal(page, &body); err != nil {
		t.Fatal(err)
	}
	body["total_count"] = listed
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// storedCache is one entry of the cache a cacheListing holds.
type storedCache struct {
	id            int64
	created, used time.Time
}

// storedCaches is n entries of one cache on main, ids 1 to n, the lower the
// id the more recently it was created and used, a minute apart.
func storedCaches(n int) []storedCache {
	out := make([]storedCache, 0, n)
	for id := int64(1); id <= int64(n); id++ {
		at := testNow.Add(-time.Duration(id) * time.Minute)
		out = append(out, storedCache{id: id, created: at, used: at})
	}
	return out
}

// cacheListing serves a repository's cache the way GitHub pages it: in the
// order the request asks for, by last use when it asks for none, from what
// is stored at the moment of the request. After each page it serves it
// hands what is stored to between, which returns what is stored for the next
// request, so a test can change the cache between two pages.
func cacheListing(t *testing.T, stored []storedCache, between func(page int, stored []storedCache) []storedCache) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		order := slices.Clone(stored)
		by := func(e storedCache) time.Time { return e.used }
		if q.Get("sort") == "created_at" {
			by = func(e storedCache) time.Time { return e.created }
		}
		slices.SortStableFunc(order, func(a, b storedCache) int { return by(b).Compare(by(a)) })
		if q.Get("direction") == "asc" {
			slices.Reverse(order)
		}
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		page, perPage = max(page, 1), cmp.Or(perPage, 30)
		lo := min((page-1)*perPage, len(order))
		hi := min(lo+perPage, len(order))
		rows := make([]map[string]any, 0, hi-lo)
		for _, e := range order[lo:hi] {
			rows = append(rows, map[string]any{
				"id": e.id, "ref": "refs/heads/main", "size_in_bytes": 1000,
				"key":              codeQLCache + "-d953d79b74456ce0-python-2.27.1-" + strconv.FormatInt(e.id, 16),
				"created_at":       e.created.Format(time.RFC3339Nano),
				"last_accessed_at": e.used.Format(time.RFC3339Nano),
			})
		}
		body, err := json.Marshal(map[string]any{"total_count": len(order), "actions_caches": rows})
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(body)
		if between != nil {
			stored = between(page, stored)
		}
	}
}

// cachePagesAsked is the page of every listing request, in order.
func cachePagesAsked(f *fixtureServer) []string {
	var out []string
	for _, r := range f.calls(cachesPath) {
		out = append(out, r.Query["page"])
	}
	return out
}

// TestActionsCacheListingIsReadPastItsFirstPage serves a repository with more
// entries than a page holds. Measured on 2026-09-27, jmrplens/ghchronicle
// listed 118 and jmrplens/mikroscope 232 against a page of a hundred, and the
// rows of both were the most recently used hundred.
func TestActionsCacheListingIsReadPastItsFirstPage(t *testing.T) {
	t.Parallel()
	f := cacheFixtureServer(t, cacheListing(t, storedCaches(118), nil))
	points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	row := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": codeQLCache})
	if got := fieldInt(t, row, "caches"); got != 118 {
		t.Errorf("caches = %d, want the 118 entries of both pages", got)
	}
	if got := fieldInt(t, row, "size_bytes"); got != 118*1000 {
		t.Errorf("size_bytes = %d, want %d", got, 118*1000)
	}
	if got := cachePagesAsked(f); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("asked for pages %v, want 1 and 2", got)
	}
}

// TestActionsCacheListingThatChangesWhileReadIsNotWrittenShort changes the
// cache between the first page and the second, the way a busy repository's
// does, and holds each row to the whole of its cache or to no row at all.
//
// The listing's pages are numbered, so an entry that changes place between
// two of them moves every entry behind it. In the order GitHub lists by
// default, last use, a cache hit on an entry of the second page took it to
// the front: the entry that ended the first page was read again, the dedupe
// dropped it, and the one that was hit was never read, so the row said 149
// entries of the 150 there were. A new entry, newest created, lands on the
// page already read and loses nothing. A deletion loses an entry in any
// order, and the pass that sees one writes the totals row and no cache row,
// and reports no failure, since the day's next pass writes the row whole.
func TestActionsCacheListingThatChangesWhileReadIsNotWrittenShort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func([]storedCache) []storedCache
		caches int64 // zero is no row
	}{
		{"an entry of the second page used", func(stored []storedCache) []storedCache {
			stored[129].used = testNow
			return stored
		}, 150},
		{"an entry saved", func(stored []storedCache) []storedCache {
			return append(stored, storedCache{id: 1000, created: testNow, used: testNow})
		}, 150},
		{"an entry of the first page deleted", func(stored []storedCache) []storedCache {
			return slices.Delete(stored, 49, 50)
		}, 0},
	} {
		f := cacheFixtureServer(t, cacheListing(t, storedCaches(150), func(page int, stored []storedCache) []storedCache {
			if page == 1 {
				return tc.change(stored)
			}
			return stored
		}))
		points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(byMeasurement(points)["gh_actions_cache"]) != 1 {
			t.Errorf("%s: the totals row did not survive the listing", tc.name)
		}
		rows := byMeasurement(points)["gh_actions_cache_entry"]
		if tc.caches == 0 {
			if len(rows) > 0 {
				t.Errorf("%s: wrote %v from a listing an entry left while it was read", tc.name, rows[0].Fields)
			}
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows, want the one cache", tc.name, len(rows))
		}
		if got := fieldInt(t, rows[0], "caches"); got != tc.caches {
			t.Errorf("%s: caches = %d, want %d", tc.name, got, tc.caches)
		}
		if got := fieldInt(t, rows[0], "size_bytes"); got != tc.caches*1000 {
			t.Errorf("%s: size_bytes = %d, want %d", tc.name, got, tc.caches*1000)
		}
	}
}

// TestActionsCacheWalkIsBounded holds the walk to its cap, the default and
// one given, against a listing that never runs out.
func TestActionsCacheWalkIsBounded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		walk  Walk
		pages int
	}{
		{"the default", Walk{}, 10},
		{"a cap of two", Walk{Pages: 2}, 2},
	} {
		f := cacheFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			_, _ = w.Write(cachePage(t, 100, int64(100000-100*page), 100000))
		})
		points, err := Actions{CacheWalk: tc.walk}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := len(cachePagesAsked(f)); got != tc.pages {
			t.Errorf("%s: asked for %d pages, want %d", tc.name, got, tc.pages)
		}
		row := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": codeQLCache})
		if got := fieldInt(t, row, "caches"); got != int64(100*tc.pages) {
			t.Errorf("%s: caches = %d, want the %d entries read", tc.name, got, 100*tc.pages)
		}
	}
}

// TestActionsCacheWalkEndsOrFailsAsTheOtherWalksDo: a page past the first that
// answers a refusal or the pagination ceiling is the end of the data, and the
// rows are the pages before it. One that answers a failure is not: the rows
// would be a part of each cache, written over the whole one the day's earlier
// passes stored, so none is written and the failure is reported, beside the
// totals row, which is as true as it was.
func TestActionsCacheWalkEndsOrFailsAsTheOtherWalksDo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		message string
		fails   bool
	}{
		{"a refusal", http.StatusNotFound, "Not Found", false},
		{"the ceiling", http.StatusUnprocessableEntity, "In order to keep the API fast for everyone, pagination is limited for this resource.", false},
		{"a failure", http.StatusBadGateway, "Server Error", true},
	} {
		f := cacheFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"message": %q}`, tc.message)
				return
			}
			_, _ = w.Write(cachePage(t, 100, 1000, 150))
		})
		points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if (err != nil) != tc.fails {
			t.Errorf("%s: err = %v, want a failure %v", tc.name, err, tc.fails)
		}
		if len(byMeasurement(points)["gh_actions_cache"]) != 1 {
			t.Errorf("%s: the totals row did not survive the listing", tc.name)
		}
		rows := byMeasurement(points)["gh_actions_cache_entry"]
		switch {
		case tc.fails && len(rows) > 0:
			t.Errorf("%s: wrote %d rows from a listing that failed half way", tc.name, len(rows))
		case !tc.fails && (len(rows) != 1 || fieldInt(t, rows[0], "caches") != 100):
			t.Errorf("%s: want the one row of the first page's hundred entries, got %v", tc.name, rows)
		}
	}
}
