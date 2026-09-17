package collect

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestEventsPaginationLimitEndsTheWalk(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/events", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write(repeat(t, "events.json", "", 100, func(i int, row map[string]any) {
				row["id"] = strconv.Itoa(40000000100 - i)
			}))
		default:
			// Measured: past its ceiling the feed answers 422.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"In order to keep the API fast for everyone, pagination is limited for this resource.","documentation_url":"https://docs.github.com/v3/#pagination"}`))
		}
	})
	points, err := (&Events{Login: "octocat", Pages: 3}).Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("a 422 pagination limit is the end of the data, not an error: %v", err)
	}
	checkPoints(t, points)
	if len(points) != 100 {
		t.Errorf("got %d events, want the 100 of the page before the limit", len(points))
	}
	if n := len(f.calls("/users/octocat/events")); n != 2 {
		t.Errorf("made %d calls, want 2: the page and the refusal", n)
	}
}

func TestEventsPointsAndPayloads(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/users/octocat/events", "events.json")
	points, err := (&Events{Login: "octocat"}).Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	if len(points) != 3 {
		t.Fatalf("got %d events, want 3", len(points))
	}
	if n := len(f.calls("/users/octocat/events")); n != 1 {
		t.Errorf("a short page ends the walk, made %d calls", n)
	}
	push := find(t, points, "gh_event", map[string]string{"type": "PushEvent"})
	if push.Tags["full_name"] != "octocat/hello-world" || push.Tags["owner"] != "octocat" ||
		push.Tags["repo"] != "hello-world" {
		t.Errorf("push tags = %v", push.Tags)
	}
	// The feed is the account's own, so the actor was the login on every row
	// and identified nothing; public is a value nobody groups by.
	for _, tag := range []string{"actor", "public"} {
		if _, isTag := push.Tags[tag]; isTag {
			t.Errorf("%s must not be a tag", tag)
		}
	}
	if push.Fields["public"] != true {
		t.Errorf("public = %v, want the field true", push.Fields["public"])
	}
	if fieldInt(t, push, "commits") != 3 || fieldInt(t, push, "events") != 1 {
		t.Errorf("push fields = %v", push.Fields)
	}
	// A push carries neither an action nor a ref type, and both are written
	// anyway: a tag written only sometimes is NULL in InfluxDB rather than
	// (none), and a panel grouping by action left every push out.
	if push.Tags["action"] != "(none)" || push.Tags["ref_type"] != "(none)" {
		t.Errorf("push tags = %v, want the fallback on action and ref_type", push.Tags)
	}
	if want := time.Date(2026, 9, 7, 9, 14, 53, 0, time.UTC); !push.Time.Equal(want) {
		t.Errorf("event stamped %s, want created_at %s", push.Time, want)
	}
	watch := find(t, points, "gh_event", map[string]string{"type": "WatchEvent"})
	if watch.Tags["action"] != "started" || watch.Tags["ref_type"] != "(none)" {
		t.Errorf("watch tags = %v", watch.Tags)
	}
	create := find(t, points, "gh_event", map[string]string{"type": "CreateEvent"})
	if create.Tags["ref_type"] != "tag" || create.Tags["action"] != "(none)" {
		t.Errorf("create tags = %v", create.Tags)
	}
}

func TestEventsNeverAsksPastThreePages(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/events", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(repeat(t, "events.json", "", 100, nil))
	})
	points, err := (&Events{Login: "octocat", Pages: 10}).Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/users/octocat/events")); n != 3 {
		t.Errorf("made %d calls, the feed's ceiling is 3 pages", n)
	}
	if len(points) != 300 {
		t.Errorf("got %d events", len(points))
	}
}

func TestNotifications(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/notifications", "notifications.json")
	points, err := (&Notifications{All: true}).Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	calls := f.calls("/notifications")
	if len(calls) != 1 {
		t.Fatalf("made %d calls, a page under 50 ends the walk", len(calls))
	}
	if calls[0].Query["all"] != "true" || calls[0].Query["per_page"] != "50" {
		t.Errorf("query = %v", calls[0].Query)
	}
	if len(points) != 5 {
		t.Fatalf("got %d notifications, want 5", len(points))
	}
	mention := find(t, points, "gh_notification", map[string]string{"reason": "mention"})
	if mention.Tags["subject_type"] != "Issue" || mention.Tags["full_name"] != "octocat/hello-world" ||
		mention.Tags["owner"] != "octocat" || mention.Tags["repo"] != "hello-world" ||
		mention.Tags["private"] != "false" {
		t.Errorf("mention tags = %v", mention.Tags)
	}
	// Reading a thread does not move its updated_at, so as a tag the read
	// row sat beside the unread one at the same instant for ever.
	if _, isTag := mention.Tags["unread"]; isTag {
		t.Error("unread must be a field, not a tag: the row keeps its date when the thread is read")
	}
	if hasField(mention, "unread") {
		t.Error("the demoted flag must not reuse the old tag's column name")
	}
	if mention.Fields["is_unread"] != true {
		t.Errorf("is_unread = %v, want true", mention.Fields["is_unread"])
	}
	if mention.Fields["title"] != "Flaky test on arm64" || fieldInt(t, mention, "notifications") != 1 {
		t.Errorf("mention fields = %v", mention.Fields)
	}
	if want := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC); !mention.Time.Equal(want) {
		t.Errorf("notification stamped %s, want updated_at %s", mention.Time, want)
	}
	review := find(t, points, "gh_notification", map[string]string{"reason": "review_requested"})
	if review.Fields["is_unread"] != false || review.Tags["private"] != "true" {
		t.Errorf("review = %v %v", review.Tags, review.Fields)
	}
}

func TestNotificationsPaginationLimit(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/notifications", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(repeat(t, "notifications.json", "", 50, nil))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"pagination is limited for this resource"}`))
	})
	points, err := (&Notifications{Pages: 5}).Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 50 {
		t.Errorf("got %d points", len(points))
	}
}

// Every notification lands somewhere a reader can open, which is what the
// inbox itself does not provide: measured over 500 notifications, 266 are
// CheckSuite rows whose subject.url is null, and the count of linked rows is
// the point of this test.
func TestNotificationLinks(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/notifications", "notifications.json")
	points, err := (&Notifications{All: true}).Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	for _, c := range []struct {
		reason string
		want   string
		why    string
	}{
		{
			"mention", "https://github.com/octocat/hello-world/issues/9",
			"an item with no comment is its own page",
		},
		{
			"review_requested", "https://github.com/octocat/hello-world/pull/43",
			"a latest_comment_url that repeats the subject is the same page",
		},
		{
			"comment", "https://github.com/octocat/hello-world/pull/691#issuecomment-5619299616",
			"an issue comment is an anchor on its item, which is what GitHub's own html_url says",
		},
		{
			"ci_activity", "https://github.com/octocat/hello-world/actions",
			"a CheckSuite has no url of its own, and its subject is a workflow run",
		},
		{
			"subscribed", "https://github.com/octocat/hello-world/discussions/14131",
			"a discussion maps like any other item",
		},
	} {
		got := find(t, points, "gh_notification", map[string]string{"reason": c.reason})
		if got.Fields["url"] != c.want {
			t.Errorf("%s: url = %v, want %s (%s)", c.reason, got.Fields["url"], c.want, c.why)
		}
	}
	for _, p := range points {
		if !hasField(p, "url") {
			t.Errorf("a notification without a link: %v", p.Tags)
		}
	}
}

// The fallback needs the repository, and a shape nobody has seen must not turn
// into a link that goes nowhere, nor into a confident link to the wrong page.
func TestNotificationURLEdges(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                        string
		subject, comment, kind, own string
		want                        string
	}{
		{
			"no repository leaves the row without a link",
			"", "", "CheckSuite", "", "",
		},
		{
			"a CheckSuite has no url of its own and is about a workflow run",
			"", "", "CheckSuite", "octocat/hello-world",
			"https://github.com/octocat/hello-world/actions",
		},
		{
			"an unmeasured type with no url gets the repository, not Actions",
			"", "", "RepositoryVulnerabilityAlert", "octocat/hello-world",
			"https://github.com/octocat/hello-world",
		},
		{
			"an unknown subject shape falls back to the repository",
			"https://api.github.com/repos/octocat/hello-world/check-suites/42", "", "CheckSuite",
			"octocat/hello-world", "https://github.com/octocat/hello-world/actions",
		},
		{
			"a review comment has no anchor we have measured, so the item wins",
			"https://api.github.com/repos/octocat/hello-world/pulls/7",
			"https://api.github.com/repos/octocat/hello-world/pulls/comments/12345", "PullRequest",
			"octocat/hello-world", "https://github.com/octocat/hello-world/pull/7",
		},
		{
			"a comment with no item at all falls back rather than linking the comment",
			"", "https://api.github.com/repos/octocat/hello-world/issues/comments/99", "PullRequest",
			"octocat/hello-world", "https://github.com/octocat/hello-world",
		},
	} {
		if got := notificationURL(c.subject, c.comment, c.kind, c.own); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The feed moves with every event, so its pages are never a 304; what saves
// the second and third page is knowing that the newest event of the previous
// sweep is on the first.
func TestEventsStopAtThePageThatCarriesTheLastEventSeen(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.handle("/users/octocat/events", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_, _ = w.Write(repeat(t, "events.json", "", 100, func(i int, row map[string]any) {
			row["id"] = strconv.Itoa(40000000000 - 100*page - i)
		}))
	})
	first := &Events{Login: "octocat"}
	if _, err := first.Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if first.Newest != "39999999900" {
		t.Errorf("Newest = %q, want the first id of the first page", first.Newest)
	}
	if n := len(f.calls("/users/octocat/events")); n != 3 {
		t.Errorf("with nothing seen before the whole feed is read, made %d calls", n)
	}

	// The event last seen is on the second page: that page is read, and
	// emitted whole, and the third is not asked for.
	second := &Events{Login: "octocat", After: "39999999750"}
	points, err := second.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls("/users/octocat/events")); n != 5 {
		t.Errorf("made %d calls in total, want 3 and then 2: the walk stops at the page with the id", n)
	}
	if len(points) != 200 {
		t.Errorf("got %d events, want both pages whole: re-emitting is idempotent and a late event lands behind ones already seen", len(points))
	}
	if second.Newest != "39999999900" {
		t.Errorf("Newest = %q", second.Newest)
	}
}

// A `since` cuts the inbox down to the threads that moved, and what the
// collector reports as newest is where the next window starts.
func TestNotificationsAskOnlySinceTheWindow(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	f.file("/notifications", "notifications.json")
	since := time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC)
	inbox := &Notifications{Since: since}
	points, err := inbox.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls("/notifications")
	if len(calls) != 1 || calls[0].Query["since"] != "2026-09-07T10:30:00Z" {
		t.Errorf("query = %v, want since=2026-09-07T10:30:00Z", calls[0].Query)
	}
	if len(points) == 0 {
		t.Fatal("no notification came back")
	}
	var latest time.Time
	for _, p := range points {
		if p.Time.After(latest) {
			latest = p.Time
		}
	}
	if !inbox.Newest.Equal(latest) {
		t.Errorf("Newest = %s, want the latest updated_at on the page, %s", inbox.Newest, latest)
	}
	// Without a window nothing is added to the query, which is what the
	// daily full pass and a first sweep ask for.
	if _, full := (&Notifications{}).Collect(ctx(t), f.Client, testNow); full != nil {
		t.Fatal(full)
	}
	if calls = f.calls("/notifications"); calls[1].Query["since"] != "" {
		t.Errorf("a full pass sent since=%q", calls[1].Query["since"])
	}
}
