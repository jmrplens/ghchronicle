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
func drawCard(t *testing.T, cfg, card string) string {
	t.Helper()
	// Bounded, because an unbounded backfill against the fake walks until it
	// hits the reserve and then parks for the window to turn over.
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
	// A card of zeros would make every comparison below agree for the wrong
	// reason, so the baseline has to carry the account's followers.
	if !strings.Contains(desc, "1200") {
		t.Fatalf("the card from a fresh state does not carry the fixture's 1200 followers: %s", desc)
	}
	return desc
}

func TestASecondCardSharingTheStateFileSaysWhatTheFirstSaid(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, cardRunsCadence, "")

	first := drawCard(t, cfg, filepath.Join(dir, "first.svg"))
	second := drawCard(t, cfg, filepath.Join(dir, "second.svg"))
	if first != second {
		t.Errorf("the second card of one state file differs from the first\nfirst:  %s\nsecond: %s", first, second)
	}
}

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
// what names the run in the failure message, because the two compared here
// read the same and only one of them can be the empty one.
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
	// Two groups, because an unbounded walk of every family against the fake
	// spends its whole budget and then parks for the window to turn over.
	// These two carry the numbers the card is read for below.
	cfg := writeConfigWithCadence(t, dir, gh.URL(), "e2e-token", login, cardRunsCadence,
		"groups: [account, audience]\n")
	card := filepath.Join(dir, "card.svg")

	// Bounded, because an unbounded backfill against the fake walks until it
	// hits the reserve and then parks for the window to turn over.
	logs, err := run(t, 2*time.Minute, "-config", cfg, "-backfill", "-backfill-since", "30d",
		"-card", card, "-card-theme", "both")
	if err != nil {
		t.Fatalf("-backfill -card failed: %v\n%s", err, logs)
	}
	for _, path := range []string{card, filepath.Join(dir, "card_dark.svg")} {
		if desc := cardDesc(t, path, logs); !strings.Contains(desc, "1200") {
			t.Errorf("the backfill's card at %s does not carry the fixture's 1200 followers: %s", path, desc)
		}
	}
}
