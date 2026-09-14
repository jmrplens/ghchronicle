package collect

import (
	"testing"
	"time"
)

func TestTrafficStampsEachRowAtItsOwnDate(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/traffic/views", "traffic_views.json")
	f.file("/repos/octocat/hello-world/traffic/clones", "traffic_clones.json")
	f.file("/repos/octocat/hello-world/traffic/popular/referrers", "traffic_referrers.json")
	f.file("/repos/octocat/hello-world/traffic/popular/paths", "traffic_paths.json")

	points, err := Traffic{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_traffic", "gh_traffic_referrer", "gh_traffic_path")
	if len(points) != 11 {
		t.Errorf("got %d points, want 3 views + 2 clones + 4 referrers + 2 paths", len(points))
	}

	// Each day is stamped at GitHub's own date, never at the sweep time.
	views := find(t, points, "gh_traffic", map[string]string{"kind": "views"})
	if want := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC); !views.Time.Equal(want) {
		t.Errorf("first view day stamped %s, want %s", views.Time, want)
	}
	if fieldInt(t, views, "count") != 120 || fieldInt(t, views, "uniques") != 18 {
		t.Errorf("view counts = %v", views.Fields)
	}
	var dayStamps int
	for _, p := range only(t, points, "gh_traffic") {
		if p.Time.Equal(testNow) {
			t.Errorf("a traffic day was stamped at the sweep time: %+v", p)
		}
		if p.Time.Hour() == 0 && p.Time.Minute() == 0 {
			dayStamps++
		}
		if p.Tags["full_name"] != "octocat/hello-world" || p.Tags["owner"] != "octocat" || p.Tags["repo"] != "hello-world" {
			t.Errorf("repository tags missing: %v", p.Tags)
		}
	}
	if dayStamps != 5 {
		t.Errorf("%d of 5 traffic rows are stamped at midnight", dayStamps)
	}
	clones := find(t, points, "gh_traffic", map[string]string{"kind": "clones"})
	if want := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC); !clones.Time.Equal(want) {
		t.Errorf("first clone day stamped %s, want %s", clones.Time, want)
	}

	// Referrers and paths carry no date of their own, so they land on the
	// start of the sweep's UTC day: four sweeps a day must write one row.
	day := startOfDay(testNow)
	for _, m := range []string{"gh_traffic_referrer", "gh_traffic_path"} {
		for _, p := range only(t, points, m) {
			if !p.Time.Equal(day) {
				t.Errorf("%s stamped %s, want start of UTC day %s", m, p.Time, day)
			}
		}
	}
	ref := find(t, points, "gh_traffic_referrer", map[string]string{"referrer": "Google"})
	if fieldInt(t, ref, "count") != 149 {
		t.Errorf("referrer count = %v", ref.Fields["count"])
	}
	path := find(t, points, "gh_traffic_path", map[string]string{"path": "/octocat/hello-world"})
	if path.Fields["title"] != "octocat/hello-world: My first repository on GitHub!" {
		t.Errorf("path title = %v", path.Fields["title"])
	}
}

func TestTrafficSkipsWhatTheTokenCannotSee(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	// Traffic needs push access. Without it every endpoint answers 403, which
	// is not a failure of the sweep.
	f.status("/repos/octocat/hello-world/traffic/views", 403, "Must have push access to repository")
	f.status("/repos/octocat/hello-world/traffic/clones", 403, "Must have push access to repository")
	f.file("/repos/octocat/hello-world/traffic/popular/referrers", "traffic_referrers.json")
	f.file("/repos/octocat/hello-world/traffic/popular/paths", "traffic_paths.json")

	points, err := Traffic{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatalf("a 403 must be skipped, not returned: %v", err)
	}
	checkPoints(t, points)
	if got := byMeasurement(points); len(got["gh_traffic"]) != 0 || len(got["gh_traffic_referrer"]) != 4 {
		t.Errorf("got %v", measurements(points))
	}
}

func TestTrafficReportsARealFailure(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.status("/repos/octocat/hello-world/traffic/views", 500, "boom")
	if _, err := (Traffic{}).Collect(ctx(t), f.Client, testRepo, testNow); err == nil {
		t.Fatal("a 500 is a failure and must be returned")
	}
}

// A referrer is not always a place. Measured across twenty repositories, a
// third of the rows carry a brand GitHub coins for a search engine, and
// https:// plus a brand is a dead link, so those rows carry no referrer_url at
// all while `url` still takes the reader to the traffic graph.
func TestTrafficReferrerLinks(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/repos/octocat/hello-world/traffic/popular/referrers", "traffic_referrers.json")
	points, err := Traffic{}.Collect(ctx(t), f.Client, testRepo, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	graph := "https://github.com/octocat/hello-world/graphs/traffic"
	for _, p := range only(t, points, "gh_traffic_referrer") {
		if p.Fields["url"] != graph {
			t.Errorf("%s: url = %v, want the traffic graph", p.Tags["referrer"], p.Fields["url"])
		}
	}
	brand := find(t, points, "gh_traffic_referrer", map[string]string{"referrer": "Google"})
	if hasField(brand, "referrer_url") {
		t.Errorf("a search engine's name is not a host: referrer_url = %v", brand.Fields["referrer_url"])
	}
	for _, c := range []struct{ referrer, want string }{
		{"github.com", "https://github.com"},
		{"teams.public.onecdn.static.microsoft", "https://teams.public.onecdn.static.microsoft"},
		{"1confluence.fnb.co.za", "https://1confluence.fnb.co.za"},
	} {
		got := find(t, points, "gh_traffic_referrer", map[string]string{"referrer": c.referrer})
		if got.Fields["referrer_url"] != c.want {
			t.Errorf("%s: referrer_url = %v, want %s", c.referrer, got.Fields["referrer_url"], c.want)
		}
	}
}

func TestHostLike(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"github.com", true},
		{"jmrplens.github.io", true},
		{"1confluence.fnb.co.za", true},
		{"xn--80ak6aa92e.com", true},
		{"Google", false},
		{"DuckDuckGo", false},
		// Capitalised means a brand, since GitHub reports hostnames lowercase.
		{"Yahoo.Japan", false},
		{"", false},
		{"no-dot", false},
		{"double..dot.com", false},
		{"-lead.com", false},
		{"trail-.com", false},
		{"has space.com", false},
		{"path.com/x", false},
	} {
		if got := hostLike(c.in); got != c.want {
			t.Errorf("hostLike(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
