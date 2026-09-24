package collect

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

func TestIsSkippable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unavailable", &ghapi.UnavailableError{Path: "/x", Status: 403, Reason: "disabled"}, true},
		{"not ready", &ghapi.NotReadyError{Path: "/x"}, true},
		{"wrapped unavailable", fmt.Errorf("repo: %w", &ghapi.UnavailableError{Status: 404}), true},
		{"plain error", errors.New("500 Internal Server Error"), false},
		{"pagination limit is not skippable", errors.New("/x: 422: pagination is limited for this resource"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSkippable(tc.err); got != tc.want {
				t.Errorf("isSkippable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsPaginationLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("/users/x/events: 422 Unprocessable Entity: {\"message\":\"In order to keep the API fast for everyone, pagination is limited for this resource.\"}"), true},
		{errors.New("/users/x/events: 422 Unprocessable Entity: {\"message\":\"Validation Failed\"}"), false},
	}
	for _, tc := range cases {
		if got := isPaginationLimit(tc.err); got != tc.want {
			t.Errorf("isPaginationLimit(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestMergeDoesNotTouchTheBase(t *testing.T) {
	t.Parallel()
	base := map[string]string{"owner": "octocat", "repo": "hello-world"}
	got := merge(base, map[string]string{"kind": "views", "repo": "other"})
	if got["kind"] != "views" || got["owner"] != "octocat" {
		t.Errorf("merge lost a key: %v", got)
	}
	if got["repo"] != "other" {
		t.Errorf("extra must win over base, got %q", got["repo"])
	}
	if base["repo"] != "hello-world" || len(base) != 2 {
		t.Errorf("merge mutated the base map: %v", base)
	}
}

func TestTagHelpers(t *testing.T) {
	t.Parallel()
	if boolTag(true) != "true" || boolTag(false) != "false" {
		t.Error("boolTag must render true/false")
	}
	if visibility(true) != "private" || visibility(false) != "public" {
		t.Error("visibility must render private/public")
	}
	if got := login(nil); got != "(ghost)" {
		t.Errorf("a null author must become (ghost), got %q", got)
	}
	if got := login(&struct {
		Login string `json:"login"`
	}{Login: "alice"}); got != "alice" {
		t.Errorf("login = %q", got)
	}
}

func TestWalkLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		walk Walk
		def  int
		want int
	}{
		{"zero takes the collector's default", Walk{}, 5, 5},
		{"explicit pages win", Walk{Pages: 3}, 5, 3},
		{"unbounded is capped so a broken API cannot loop forever", Unbounded, 5, 100000},
		{"negative is unbounded", Walk{Pages: -7}, 2, 100000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.walk.limit(tc.def); got != tc.want {
				t.Errorf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWalkPast(t *testing.T) {
	t.Parallel()
	bound := testNow.AddDate(0, 0, -30)
	w := Walk{Since: bound}
	if !w.past(bound.Add(-time.Second)) {
		t.Error("an item older than the bound is past it")
	}
	if w.past(bound) || w.past(bound.Add(time.Hour)) {
		t.Error("an item at or after the bound is not past it")
	}
	if w.past(time.Time{}) {
		t.Error("an undated item must not end the walk")
	}
	if (Walk{}).past(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("with no bound nothing is past")
	}
}

// pagedRow is one item of the REST list pages walks in TestPagesHelper.
type pagedRow struct {
	ID int `json:"id"`
}

// pagedRows is one page of that list: n rows numbered from first, so a short
// page is told apart from a full one by its length alone.
func pagedRows(t *testing.T, n, first int) []byte {
	t.Helper()
	rows := make([]pagedRow, n)
	for i := range rows {
		rows[i].ID = first + i
	}
	return mustMarshal(t, rows)
}

func TestPagesHelper(t *testing.T) {
	t.Parallel()
	t.Run("walks full pages and stops at the short one", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle("/rows", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "1":
				_, _ = w.Write(pagedRows(t, 100, 0))
			case "2":
				_, _ = w.Write(pagedRows(t, 100, 100))
			default:
				_, _ = w.Write(pagedRows(t, 3, 200))
			}
		})
		var seen int
		err := pages(ctx(t), f.Client, Walk{Pages: 10}, 1, func(p int) string {
			return fmt.Sprintf("/rows?page=%d", p)
		}, func(rows []pagedRow) bool {
			seen += len(rows)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen != 203 || len(f.calls("/rows")) != 3 {
			t.Errorf("seen %d rows over %d calls", seen, len(f.calls("/rows")))
		}
	})

	t.Run("visit can stop the walk", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle("/rows", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pagedRows(t, 100, 0)) })
		err := pages(ctx(t), f.Client, Unbounded, 1, func(p int) string {
			return fmt.Sprintf("/rows?page=%d", p)
		}, func(rows []pagedRow) bool { return false })
		if err != nil || len(f.calls("/rows")) != 1 {
			t.Errorf("err=%v calls=%d", err, len(f.calls("/rows")))
		}
	})

	t.Run("the default cap applies when Walk is zero", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle("/rows", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pagedRows(t, 100, 0)) })
		err := pages(ctx(t), f.Client, Walk{}, 2, func(p int) string {
			return fmt.Sprintf("/rows?page=%d", p)
		}, func(rows []pagedRow) bool { return true })
		if err != nil || len(f.calls("/rows")) != 2 {
			t.Errorf("err=%v calls=%d, want the default of 2", err, len(f.calls("/rows")))
		}
	})

	t.Run("unavailable and the pagination limit end the walk quietly", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.handle("/rows", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write(pagedRows(t, 100, 0))
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"pagination is limited for this resource"}`))
		})
		var seen int
		err := pages(ctx(t), f.Client, Unbounded, 1, func(p int) string {
			return fmt.Sprintf("/rows?page=%d", p)
		}, func(rows []pagedRow) bool { seen += len(rows); return true })
		if err != nil || seen != 100 {
			t.Errorf("err=%v seen=%d", err, seen)
		}
		if missingErr := pages(ctx(t), f.Client, Unbounded, 1, func(int) string { return "/missing" }, func(rows []pagedRow) bool { return true }); missingErr != nil {
			t.Errorf("404 must be folded into nothing, got %v", missingErr)
		}
	})

	t.Run("a real failure is returned", func(t *testing.T) {
		t.Parallel()
		f := newFixtureServer(t)
		f.status("/rows", 500, "boom")
		if err := pages(ctx(t), f.Client, Unbounded, 1, func(int) string { return "/rows" }, func(rows []pagedRow) bool { return true }); err == nil {
			t.Error("a 500 must be returned")
		}
	})
}
