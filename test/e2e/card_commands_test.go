package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/render"
)

// cardCommandBlock is a shell block in docs/card.md holding the command the
// documentation offers under a card. The commands are generated, by
// site/src/lib/card-command.mjs, and put there by the same reduction that
// writes the site's markdown twins, and `make check-docs` fails when the file
// no longer matches it, so a block read here is the line a reader copies from
// the site. They are the blocks that start with the layout, which is where
// that module puts it; the page's other shell examples start elsewhere.
var cardCommandBlock = regexp.MustCompile("(?m)^[ \t]*```sh\n[ \t]*(ghchronicle -card-layout [^\n]*)\n[ \t]*```$")

// TestEveryCardCommandInTheDocsDrawsThePictureTheDocsShow runs every card
// command docs/card.md offers, as written, against the account the gallery is
// drawn from, and holds both files it writes to the committed pictures of that
// layout: copy the command under a card, get that card.
//
// Each command names its picture itself, by its -card-layout and whether it
// asks for -card-motion loop, so a command that drifts from what the pictures
// were drawn with (a flag renamed, the gallery drawing with an argument the
// command lacks) fails here on a difference, not on a missing file. And every
// layout must have its command, with the looping one for each layout that
// loops, so a card losing its command fails too.
func TestEveryCardCommandInTheDocsDrawsThePictureTheDocsShow(t *testing.T) {
	t.Parallel()
	var pictures []string
	for _, layout := range render.Layouts() {
		pictures = append(pictures, "card-"+layout.Name)
		if layout.Loops {
			pictures = append(pictures, "card-"+layout.Name+"-loop")
		}
	}

	work, _ := galleryAccount(t)
	var drawn []string
	for _, command := range docsCardCommands(t) {
		// Split on spaces and nothing else, so the test stays honest about what
		// it runs: a command that needed a shell to mean what it says (quotes, a
		// variable, a line continued with a backslash) is refused, not
		// interpreted.
		if strings.ContainsAny(command, `"'$\`+"`") {
			t.Errorf("%q needs a shell to read; a card command is plain words", command)
			continue
		}
		args := strings.Fields(command)[1:]
		// The picture is looked up among the ones the registry says exist, so a
		// command naming a layout that has none fails as offering nothing.
		i := slices.Index(pictures, pictureOf(args))
		if i < 0 {
			t.Errorf("%s: draws %s, which the gallery has no picture of", command, pictureOf(args))
			continue
		}
		drawn = append(drawn, pictures[i])
		drawsPicture(t, work, command, args, pictures[i])
	}

	for _, picture := range pictures {
		if !slices.Contains(drawn, picture) {
			t.Errorf("docs/card.md offers no command for %s", picture)
		}
	}
}

// docsCardCommands is every distinct card command docs/card.md offers, in the
// order it offers them.
func docsCardCommands(t *testing.T) []string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "card.md"))
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, match := range cardCommandBlock.FindAllStringSubmatch(string(doc), -1) {
		if !slices.Contains(commands, match[1]) {
			commands = append(commands, match[1])
		}
	}
	return commands
}

// drawsPicture runs one command in work and holds the light and the dark file
// it writes to the committed picture of that name and its _dark twin.
func drawsPicture(t *testing.T, work, command string, args []string, picture string) {
	t.Helper()
	light, dark := filepath.Join(work, "card.svg"), filepath.Join(work, "card_dark.svg")
	// Removed first, so a run that writes nothing is a missing file and not the
	// previous command's card compared again.
	_ = os.Remove(light)
	_ = os.Remove(dark)
	logs, err := runIn(t, work, 2*time.Minute, args...)
	if err != nil {
		t.Errorf("%s: %v\n%s", command, err, logs)
		return
	}
	assets := filepath.Join("..", "..", "site", "src", "assets")
	sameFile(t, command, light, filepath.Join(assets, picture+".svg"))
	sameFile(t, command, dark, filepath.Join(assets, picture+"_dark.svg"))
}

// sameFile fails the test unless the file written at got holds exactly the
// bytes of the committed picture at want.
func sameFile(t *testing.T, command, got, want string) {
	t.Helper()
	written, err := os.ReadFile(got)
	if err != nil {
		t.Errorf("%s: wrote no %s: %v", command, filepath.Base(got), err)
		return
	}
	picture, err := os.ReadFile(want)
	if err != nil {
		t.Errorf("%s: the docs show no picture %s: %v", command, want, err)
		return
	}
	if !bytes.Equal(written, picture) {
		t.Errorf("%s: %s is not %s (%d bytes against %d)",
			command, filepath.Base(got), want, len(written), len(picture))
	}
}

// pictureOf is the gallery picture a card command's arguments draw: the
// layout named by -card-layout, and its looping picture under -card-motion
// loop.
func pictureOf(args []string) string {
	var layout, motion string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-card-layout":
			layout = args[i+1]
		case "-card-motion":
			motion = args[i+1]
		}
	}
	if motion == render.MotionLoop {
		return "card-" + layout + "-loop"
	}
	return "card-" + layout
}
