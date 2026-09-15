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
// GHC_CARD_THEME picks the theme, dark unless it says otherwise. `auto` puts
// both themes in one file behind a prefers-color-scheme query, which is what
// a card in a README wants.
//
// The files are SVG, which is what the tool writes. The documentation shows
// PNG, so `rsvg-convert -w <width> card.svg -o card.png` is the second step;
// the site's images were made at twice the layout's own width.
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

	// Matched against the three the renderer knows rather than passed
	// through: a value out of the environment that reaches a command line is
	// how a flag ends up carrying something nobody meant, and a typo here
	// would otherwise surface as a card that is silently the wrong theme.
	theme := "dark"
	switch os.Getenv("GHC_CARD_THEME") {
	case "", "dark":
	case "light":
		theme = "light"
	case "auto":
		theme = "auto"
	default:
		t.Fatalf("GHC_CARD_THEME=%q: dark, light or auto", os.Getenv("GHC_CARD_THEME"))
	}
	// The base fixtures plus the gallery's own, which give the account a whole
	// year of contributions, GitHub's full fourteen days of traffic, five
	// repositories to rank and one of them in six languages. On the base
	// account alone the heatmap was five cells out of eighty-four, the lists of
	// repositories had one row and the language ring was one color.
	gh := fakegh.New(t, "testdata", "testdata/gallery")

	for _, layout := range render.Layouts() {
		// A directory per layout, because the second sweep of the same state
		// file is answered 304 for everything and accumulates nothing: every
		// card after the first came out with a zero in each number, which is
		// a picture of a bug rather than of a layout.
		work := t.TempDir()
		cfg := writeConfig(t, work, gh.URL(), "e2e-token", login, "")
		card := filepath.Join(work, "card.svg")
		logs, runErr := run(t, 2*time.Minute, "-config", cfg,
			"-card", card, "-card-only",
			"-card-layout", layout.Name, "-card-theme", theme)
		if runErr != nil {
			t.Fatalf("%s: %v\n%s", layout.Name, runErr, logs)
		}
		svg, readErr := os.ReadFile(card)
		if readErr != nil {
			t.Fatalf("%s: card not written: %v\n%s", layout.Name, readErr, logs)
		}
		name := "card-" + layout.Name + ".svg"
		if writeErr := out.WriteFile(name, svg, 0o600); writeErr != nil {
			t.Fatalf("%s: %v", name, writeErr)
		}
		t.Logf("%-20s %6d bytes", layout.Name, len(svg))
	}
}
