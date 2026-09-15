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
// Every layout is drawn twice, card-<layout>.svg in the light palette and
// card-<layout>_dark.svg in the dark one: the names the site's ThemeImage and
// the README's <picture> read, and the convention jmrplens/phonometry already
// uses for its own figures. Two files and not one "auto" file, because auto decides its
// palette with a prefers-color-scheme query inside the picture: that follows
// the reader's operating system rather than the theme they picked on the page,
// and a browser does not reliably evaluate it again inside an image. On GitHub
// the README's auto cards were seen switching palettes on a dark page when the
// tab was left and came back to.
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

	// The base fixtures plus the gallery's own, which give the account a whole
	// year of contributions, GitHub's full fourteen days of traffic, five
	// repositories to rank and one of them in six languages. On the base
	// account alone the heatmap was five cells out of eighty-four, the lists of
	// repositories had one row and the language ring was one color.
	gh := fakegh.New(t, "testdata", "testdata/gallery")

	themes := []struct{ flag, suffix string }{{"light", ".svg"}, {"dark", "_dark.svg"}}
	for _, layout := range render.Layouts() {
		for _, theme := range themes {
			// A directory per card, because the second sweep of the same state
			// file is answered 304 for everything and accumulates nothing:
			// every card after the first came out with a zero in each number,
			// which is a picture of a bug rather than of a layout.
			work := t.TempDir()
			cfg := writeConfig(t, work, gh.URL(), "e2e-token", login, "")
			card := filepath.Join(work, "card.svg")
			logs, runErr := run(t, 2*time.Minute, "-config", cfg,
				"-card", card, "-card-only",
				"-card-layout", layout.Name, "-card-theme", theme.flag)
			if runErr != nil {
				t.Fatalf("%s %s: %v\n%s", layout.Name, theme.flag, runErr, logs)
			}
			svg, readErr := os.ReadFile(card)
			if readErr != nil {
				t.Fatalf("%s %s: card not written: %v\n%s", layout.Name, theme.flag, readErr, logs)
			}
			name := "card-" + layout.Name + theme.suffix
			if writeErr := out.WriteFile(name, svg, 0o600); writeErr != nil {
				t.Fatalf("%s: %v", name, writeErr)
			}
			t.Logf("%-32s %6d bytes", name, len(svg))
		}
	}
}
