package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/internal/render"
	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// TestCardGallery renders one card per registered layout, from the fake
// GitHub, into the directory GHC_CARD_GALLERY names. It is skipped without
// that variable, like every other opt-in in this tree.
//
// It exists because the documentation shows a picture of each layout and
// those pictures have to come from somewhere. Rendering them from a real
// account puts one person's name, repositories and star counts into an image
// the project publishes, and keeps them at whatever they were on the day
// somebody remembered to retake them. The fake account is the same one every
// other test here collects, so a layout that changes shape is one command
// away from a new set of pictures that agree with it.
//
//	mkdir -p /tmp/cards
//	GHC_CARD_GALLERY=/tmp/cards go test ./test/e2e/ -run TestCardGallery
//
// Every card comes from one sweep under -card-theme both, which writes
// card-<layout>.svg in the light palette and card-<layout>_dark.svg in the
// dark one beside it: the names the site's ThemeImage and the README's
// <picture> read, and the convention jmrplens/phonometry already uses for its
// own figures. Two files and not one "auto" file, because auto decides its
// palette with a prefers-color-scheme query inside the picture: that follows
// the reader's operating system rather than the theme they picked on the page,
// and a browser does not reliably evaluate it again inside an image. On GitHub
// the README's auto cards were seen switching palettes on a dark page when the
// tab was left and came back to.
//
// The layouts that have something continuous to keep going are drawn a second
// time with -card-motion loop, as card-<layout>-loop.svg and its _dark twin,
// for the page that shows what a loop looks like. Only those: on a layout that
// only reveals, loop draws the same card as once, so a looping picture of it
// would be a second copy of the first under a name that promises something
// else.
func TestCardGallery(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("GHC_CARD_GALLERY")
	if dir == "" {
		t.Skip("set GHC_CARD_GALLERY to an existing directory to render one card per layout")
	}
	// The binary renders into a directory this test owns and the cards are
	// copied out through a root, so the only thing the name from the
	// environment can reach is a file directly under the directory it names.
	out, err := os.OpenRoot(dir)
	if err != nil {
		// Not created here: a directory this test makes from a name it was
		// handed is a directory it can make anywhere, and asking the caller
		// to make their own output directory costs them one mkdir.
		t.Fatalf("GHC_CARD_GALLERY=%s must name a directory that exists: %v", dir, err)
	}
	defer func() { _ = out.Close() }()

	type variant struct{ layout, motion, name string }
	var variants []variant
	for _, layout := range render.Layouts() {
		variants = append(variants, variant{layout.Name, render.MotionOnce, "card-" + layout.Name})
		if layout.Loops {
			variants = append(variants, variant{layout.Name, render.MotionLoop, "card-" + layout.Name + "-loop"})
		}
	}
	// One directory for all the cards, which is the arrangement this used to
	// avoid. Each card had its own, because a card after the first came out
	// with a zero in each number and that was read as the ETag cache
	// answering 304 for everything; the cache is per process and each card is
	// a process of its own, so it never was. What zeroed them was the cadence:
	// the first card marked every family as run and the next run of the same
	// state file found none of them due. A card run now collects every family
	// whatever the cadence says, and -card-only writes nothing to the state
	// file at all, so one directory draws the same card as twenty.
	work, cfg := galleryAccount(t)
	for _, v := range variants {
		// A path per variant, so a run that writes nothing is a missing file
		// and not the previous variant's card copied out under a new name.
		card := filepath.Join(work, v.name+".svg")
		logs, runErr := run(t, 2*time.Minute, "-config", cfg,
			"-card", card, "-card-only", "-card-layout", v.layout,
			"-card-theme", "both", "-card-motion", v.motion)
		if runErr != nil {
			t.Fatalf("%s: %v\n%s", v.name, runErr, logs)
		}
		// A slice and not a map, so the log lists light before dark every time.
		copies := []struct{ src, dst string }{
			{card, v.name + ".svg"},
			{filepath.Join(work, v.name+"_dark.svg"), v.name + "_dark.svg"},
		}
		for _, c := range copies {
			svg, readErr := os.ReadFile(c.src)
			if readErr != nil {
				t.Fatalf("%s: card not written: %v\n%s", c.dst, readErr, logs)
			}
			if writeErr := out.WriteFile(c.dst, svg, 0o600); writeErr != nil {
				t.Fatalf("%s: %v", c.dst, writeErr)
			}
			t.Logf("%-36s %6d bytes", c.dst, len(svg))
		}
	}
}

// galleryAccount is the account the gallery is drawn from, and the config
// that collects it, written as config.yaml in a directory of the test's own.
// It is the fake GitHub on the base fixtures plus the gallery's own, which
// give the account a whole year of contributions, GitHub's full fourteen days
// of traffic, five repositories to rank and one of them in six languages. On
// the base account alone the heatmap was five cells out of eighty-four, the
// lists of repositories had one row and the language ring was one color.
//
// Shared with TestEveryCardCommandInTheDocsDrawsThePictureTheDocsShow, which
// has to draw from exactly this account for its pictures to be comparable
// with the gallery's.
func galleryAccount(t *testing.T) (work, cfg string) {
	t.Helper()
	gh := fakegh.New(t, "testdata", "testdata/gallery")
	// The one fake in this repository whose clock is stopped. The pictures the
	// docs show are committed under site/src/assets and compared byte for
	// byte, so the data they are drawn from has to be the same data every day,
	// while the fixtures themselves spell their dates as offsets precisely so
	// that they move. Freezing here is what lets both be true. The day is the
	// one those fixtures were written against.
	gh.FreezeAt(fixtureDay)
	work = t.TempDir()
	return work, writeConfig(t, work, gh.URL(), "e2e-token", login, "")
}

// fixtureDay is the day the fixtures under testdata call "now". Their dates
// are written as offsets from it, so resolving them against it gives back
// exactly the dates they were authored with, which is what keeps the committed
// cards identical from one day to the next.
var fixtureDay = time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
