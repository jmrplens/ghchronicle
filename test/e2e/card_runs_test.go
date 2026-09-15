package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// These pin how a run that draws a card shares a state file with the runs
// around it. Every config here sets every family to an hour, so a second run
// straight after a first finds nothing due by cadence: what these tests prove
// has to hold because of what the run is, not because a minute went by.
const cardRunsCadence = "1h"

// cardDesc is the <desc> of the card at path, which spells out every number
// the card shows. Two cards that say the same thing have the same desc.
func cardDesc(t *testing.T, path string, logs []byte) string {
	t.Helper()
	svg, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("card not written at %s: %v\n%s", path, err, logs)
	}
	m := regexp.MustCompile(`<desc id="ghcDesc">([^<]*)</desc>`).FindSubmatch(svg)
	if m == nil {
		t.Fatalf("the card at %s has no description:\n%s", path, truncate(svg))
	}
	return string(m[1])
}

// drawCard runs a card-only sweep with cfg and returns the card's description.
// The bound is generous rather than meaningful: a sweep of the fake takes
// tenths of a second, and the minutes are there so a run that hangs fails with
// the log rather than with the whole suite's deadline.
func drawCard(t *testing.T, cfg, card string) string {
	t.Helper()
	logs, err := run(t, 2*time.Minute, "-config", cfg, "-card", card, "-card-only")
	if err != nil {
		t.Fatalf("-card -card-only failed: %v\n%s", err, logs)
	}
	return cardDesc(t, card, logs)
}

// freshCard is the card a run with nothing in its state file draws, which is
// what every other card of the same account should say.
func freshCard(t *testing.T, base string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := writeConfigWithCadence(t, dir, base, "e2e-token", login, cardRunsCadence, "")
	desc := drawCard(t, cfg, filepath.Join(dir, "card.svg"))
	requireWholeAccount(t, "the card from a fresh state", desc)
	return desc
}

// requireWholeAccount is the floor under every comparison here: a card of
// zeros is what the bug drew, and two of them agree with each other perfectly,
// so a test that only compares cards has to pin one of them to a number the
// fixtures carry. The account's 1200 followers is that number.
func requireWholeAccount(t *testing.T, what, desc string) {
	t.Helper()
	if !strings.Contains(desc, "1200") {
		t.Fatalf("%s does not carry the fixture's 1200 followers: %s", what, desc)
	}
}

func TestASecondCardSharingTheStateFileSaysWhatTheFirstSaid(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, cardRunsCadence, "")

	first := drawCard(t, cfg, filepath.Join(dir, "first.svg"))
	requireWholeAccount(t, "the first card", first)
	second := drawCard(t, cfg, filepath.Join(dir, "second.svg"))
	if first != second {
		t.Errorf("the second card of one state file differs from the first\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestACardAfterACollectionSharingTheStateFileIsComplete and the test below it
// drive three sweeps against one fake GitHub, whose spent budget accumulates
// for its whole life rather than per run: a family that grew to spend much
// more, in the search bucket above all, would show up here as a later run
// collecting less than the one before it, not as a number in a table.
func TestACardAfterACollectionSharingTheStateFileIsComplete(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	want := freshCard(t, gh.URL())

	dir := t.TempDir()
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, cardRunsCadence, "")
	sweepOnce(t, cfg)
	if got := drawCard(t, cfg, filepath.Join(dir, "card.svg")); got != want {
		t.Errorf("a card drawn after -once on the same state file is not the card a fresh state draws\nwant: %s\ngot:  %s", want, got)
	}
}

// writtenByFamily is, from a run's log, the points each family handed the
// file sink, as "family=points" in order, so two runs can be compared whole.
// The what parameter names the run in the failure message, because the two
// runs compared here read the same and only one of them can be the empty one.
func writtenByFamily(t *testing.T, what, logs string) []string {
	t.Helper()
	var out []string
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, "msg=written") || !strings.Contains(line, "sink=file") {
			continue
		}
		_, pairs, _ := splitLogfmt(line)
		out = append(out, fmt.Sprintf("%s=%s", pairs["family"], pairs["points"]))
	}
	slices.Sort(out)
	if len(out) == 0 {
		t.Fatalf("%s wrote nothing to the file sink:\n%s", what, logs)
	}
	return out
}

func TestACardOnlyRunLeavesTheNextCollectionEverythingToCollect(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)

	fresh := t.TempDir()
	want := writtenByFamily(t, "-once on a fresh state", sweepOnce(t, writeConfigWithCadence(t, fresh, gh.URL(), "e2e-token", login, cardRunsCadence, "")))

	dir := t.TempDir()
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, cardRunsCadence, "")
	drawCard(t, cfg, filepath.Join(dir, "card.svg"))
	got := writtenByFamily(t, "-once after the card", sweepOnce(t, cfg))
	if !slices.Equal(got, want) {
		t.Errorf("-once after a card-only run on the same state file collected less than on a fresh state\nfresh state:    %v\nafter the card: %v", want, got)
	}
}

func TestABackfillWithACardWritesTheCard(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	// Two groups and a date, because an unbounded backfill of every family
	// against the fake walks until it hits the reserve and then parks an hour
	// for the window to turn over. These two groups carry the numbers the
	// cards are read for below.
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, cardRunsCadence,
		"groups: [account, audience]\n")
	card := filepath.Join(dir, "card.svg")

	logs, err := run(t, 2*time.Minute, "-config", cfg, "-backfill", "-backfill-since", "30d",
		"-card", card, "-card-theme", "both")
	if err != nil {
		t.Fatalf("-backfill -card failed: %v\n%s", err, logs)
	}
	for _, path := range []string{card, filepath.Join(dir, "card_dark.svg")} {
		requireWholeAccount(t, "the backfill's card at "+path, cardDesc(t, path, logs))
	}
}
