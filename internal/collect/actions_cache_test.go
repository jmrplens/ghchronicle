package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
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
// the opposite order. GitHub lists them by last use, and an entry used between
// two pages moves, so the row has to be the same whichever page each entry
// arrived on: a row that changed with the order would be a new value to write
// on every pass that saw nothing new.
func TestActionsCacheRowsDoNotDependOnTheListingOrder(t *testing.T) {
	t.Parallel()
	listing := func(reverse bool) []sink.Point {
		t.Helper()
		f := cacheFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			body := fixture(t, "actions_caches_codeql.json")
			if reverse {
				body = reversedCaches(t, body)
			}
			_, _ = w.Write(body)
		})
		points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
		if err != nil {
			t.Fatal(err)
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
// first and each used a minute before the one above it, the way GitHub lists
// them.
func cachePage(t *testing.T, n int, first int64) []byte {
	t.Helper()
	return repeat(t, "actions_caches_codeql.json", "actions_caches", n, func(i int, row map[string]any) {
		id := first - int64(i)
		used := testNow.Add(-time.Duration(first-id) * time.Minute)
		row["id"] = id
		row["key"] = codeQLCache + "-d953d79b74456ce0-python-2.27.1-" + strconv.FormatInt(id, 16)
		row["last_accessed_at"] = used.Format(time.RFC3339)
		row["created_at"] = used.Format(time.RFC3339)
		row["size_in_bytes"] = 1000
	})
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
//
// The second page opens with the entry the first ended on, which is what an
// entry used between the two requests does to a list ordered by last use: it
// moves to the front and pushes everything behind it down one place. Counted
// on both pages it would be one entry too many and its bytes twice.
func TestActionsCacheListingIsReadPastItsFirstPage(t *testing.T) {
	t.Parallel()
	f := cacheFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = w.Write(cachePage(t, 100, 1000))
		case "2":
			_, _ = w.Write(cachePage(t, 19, 901))
		default:
			t.Errorf("asked for page %s after a short page", r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(`{"total_count": 0, "actions_caches": []}`))
		}
	})
	points, err := Actions{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	row := find(t, points, "gh_actions_cache_entry", map[string]string{"cache": codeQLCache})
	if got := fieldInt(t, row, "caches"); got != 118 {
		t.Errorf("caches = %d, want the 118 distinct entries of both pages", got)
	}
	if got := fieldInt(t, row, "size_bytes"); got != 118*1000 {
		t.Errorf("size_bytes = %d, want %d", got, 118*1000)
	}
	if got := cachePagesAsked(f); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("asked for pages %v, want 1 and 2", got)
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
			_, _ = w.Write(cachePage(t, 100, int64(100000-100*page)))
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
			_, _ = w.Write(cachePage(t, 100, 1000))
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
