package run

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestARefillOwedTwiceReadsBoth: two migrations clearing one store are one
// refill of every family and measurement of both, as far back as the
// further of the two, and no bound at all when either has none.
func TestARefillOwedTwiceReadsBoth(t *testing.T) {
	t.Parallel()
	early, late := time.Date(2019, 5, 2, 0, 0, 0, 0, time.UTC), time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)
	rec := &StoreRecord{}
	rec.OweRefill("2.6.1/gh_discussion_comment/is_answer", "gh_discussion_comment", []string{"outbound", "discussions"}, late)
	rec.OweRefill("1.0.0/gh_dependabot_alert_item/state", "gh_dependabot_alert_item", []string{"security"}, early)
	rec.OweRefill("2.6.1/gh_discussion_comment/is_answer", "gh_discussion_comment", []string{"outbound"}, late)
	want := Refill{
		Migrations:   []string{"1.0.0/gh_dependabot_alert_item/state", "2.6.1/gh_discussion_comment/is_answer"},
		Families:     []string{"discussions", "outbound", "security"},
		Measurements: []string{"gh_dependabot_alert_item", "gh_discussion_comment"},
		Since:        early,
	}
	got := *rec.Refill
	if !slices.Equal(got.Migrations, want.Migrations) || !slices.Equal(got.Families, want.Families) ||
		!slices.Equal(got.Measurements, want.Measurements) || !got.Since.Equal(want.Since) {
		t.Errorf("owed %+v, want %+v", got, want)
	}
	rec.OweRefill("1.0.0/gh_code_scanning_alert_item/state", "gh_code_scanning_alert_item", []string{"security"}, time.Time{})
	rec.OweRefill("2.6.1/gh_discussion_comment/is_answer", "gh_discussion_comment", []string{"outbound"}, late)
	if !rec.Refill.Since.IsZero() {
		t.Errorf("a refill owed with no bound reads back to %v", rec.Refill.Since)
	}
}

// TestADetachedStateIsNeverSaved: what a refill marks in its copy of the
// state goes nowhere, and the copy starts from what the file holds.
func TestADetachedStateIsNeverSaved(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	s := LoadState(path)
	s.Mark("outbound", walkClock)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copied := LoadState(path).Detached()
	if !copied.LastRun["outbound"].Equal(walkClock) {
		t.Errorf("the copy does not start from the file: %v", copied.LastRun)
	}
	copied.Mark("discussions", walkClock)
	copied.MarkHistory("octocat/hello-world", walkClock)
	copied.LastHead["octocat/hello-world"] = "abc"
	if err = copied.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a detached state wrote the file:\nbefore %s\nafter  %s", before, after)
	}
}
