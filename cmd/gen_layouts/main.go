// Command gen_layouts writes the card layouts the documentation site states,
// out of the registry that defines them.
//
// The layouts page gives every layout a section, and under each heading it
// states the same five things: the family, whether it animates, whether it has
// something that can keep going, the fields it draws when nothing is asked for,
// and the width it is drawn at with the minimum it refuses to go below. All of
// that is in internal/render, and every copy of it the site ever kept had to
// have a test written to stop it drifting: one read a markdown table off both
// pages and compared two booleans a row, four hand-written copies of the same
// pair. A copy with a test on it is still a copy.
//
// So the site does not keep one. This exports the registry into
// site/src/data/layouts.json, the page's component reads that file, and -check
// is what makes the file output rather than a fourth copy. The identifiers are
// exported as they are, because a field name and a family name are identifiers
// in both languages; the labels around them belong to the site, which is where
// the rest of its translations live.
//
//	go run ./cmd/gen_layouts            # write site/src/data/layouts.json
//	go run ./cmd/gen_layouts -check     # write nothing, fail if it is stale
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jmrplens/ghchronicle/internal/render"
)

// defaultOut is where the site reads the file from, relative to the repository
// root, which is where every other generator here is run from.
const defaultOut = "site/src/data/layouts.json"

// layout is one registry entry as the site reads it. A list and not an object
// keyed by name, so the file carries the registry's own order, which is the
// order -card-layouts prints and the order the page presents them in.
//
// Only what the page states is exported. Supports is left out on purpose: the
// three block fields have a table of their own on that page, checked by
// internal/render's documented_test.go, and a key nothing reads is the same
// invitation to drift as a key typed by hand.
type layout struct {
	Name     string   `json:"name"`
	Family   string   `json:"family"`
	Animated bool     `json:"animated"`
	Loops    bool     `json:"loops"`
	Width    int      `json:"width"`
	MinWidth int      `json:"minWidth"`
	MaxWidth int      `json:"maxWidth"`
	Fields   []string `json:"fields"`
}

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run is the whole command, with the arguments, the two streams and the exit
// status passed in and handed back rather than taken from the process, so a
// test can drive every way it ends. args[0] is the program name, as in os.Args.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", defaultOut, "the file the site reads the layouts from")
	check := flags.Bool("check", false, "compare with what is on disk instead of writing")
	// The statuses a flag.ExitOnError set would exit with: a request for the
	// usage is not a failure, a flag it does not know is.
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	body, err := layoutsJSON(render.Layouts())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	path := filepath.Clean(*out)
	if *check {
		old, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, readErr)
			return 1
		}
		if !bytes.Equal(old, body) {
			fmt.Fprintf(stderr, "%s no longer matches the layout registry. "+
				"Regenerate it with: go run ./cmd/gen_layouts\n", path)
			return 1
		}
		fmt.Fprintf(stdout, "%s is up to date\n", path)
		return 0
	}
	if err = os.WriteFile(path, body, 0o600); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s, %d layouts\n", path, len(render.Layouts()))
	return 0
}

// layoutsJSON is the file's exact bytes: two-space JSON with a trailing newline,
// which is what prettier asks of a JSON file in the site. A generator whose
// output the site's own formatter then rejects is a gate that cannot be
// satisfied.
//
// An empty registry is refused rather than exported, so that -check cannot pass
// over nothing: a committed `[]` compared with a generated `[]` is a gate that
// agrees with itself and says nothing about any layout. The same refusal the
// site's check-table-fit.mjs makes of an empty corpus. The registry is passed
// in rather than read here so that the refusal is reachable from a test.
func layoutsJSON(registered []render.Layout) ([]byte, error) {
	if len(registered) == 0 {
		return nil, errors.New("the layout registry is empty, so there is nothing to export " +
			"and -check would pass over nothing. Look at internal/render/layouts.go")
	}
	out := make([]layout, 0, len(registered))
	for _, l := range registered {
		out = append(out, layout{
			Name:     l.Name,
			Family:   l.Family,
			Animated: l.Animated,
			Loops:    l.Loops,
			Width:    l.Width,
			MinWidth: l.MinWidth,
			MaxWidth: l.MaxWidth,
			Fields:   l.Fields,
		})
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
