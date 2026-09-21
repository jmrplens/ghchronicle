// Command gen_compose writes the compose files the documentation offers, one
// per combination a reader can pick.
//
// They are generated rather than written by hand for the reason every other
// generated thing here is: a compose file in a page is a promise that it
// works, and one that is typed twice is one that is wrong once. These are
// written once, shown by the page, and brought up by the end-to-end suite, so
// what a reader copies is what was tested.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run is the whole command, with its arguments and both streams passed in, so
// a test can drive every way it ends.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "fail if what is there is not what this writes")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := write(*check, stdout); err != nil {
		fmt.Fprintln(stderr, "gen_compose:", err)
		return 1
	}
	return 0
}

// deployDir is where the compose files go. Fixed rather than a flag: the
// destination is the project's, not the caller's, and a path that comes from
// outside is one the analysis has to be argued with about.
const deployDir = "deploy"

// write puts every combination where it goes, or compares them.
func write(check bool, out io.Writer) error {
	if err := os.MkdirAll(deployDir, 0o750); err != nil {
		return err
	}
	stale := 0
	for _, combination := range Combinations() {
		path := filepath.Join(deployDir, combination.File)
		want := combination.Compose()
		if check {
			have, err := os.ReadFile(path)
			if err != nil || string(have) != want {
				fmt.Fprintf(out, "  %s\n", path)
				stale++
			}
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
			return err
		}
	}
	if stale > 0 {
		return fmt.Errorf("%d compose file(s) are not what this writes; run: make compose", stale)
	}
	if !check {
		fmt.Fprintf(out, "wrote %d compose file(s) to %s\n", len(Combinations()), deployDir)
	}
	return nil
}

// trimmed is the file with no trailing blank lines, which is what a comparison
// wants and an editor tends to add.
func trimmed(s string) string { return strings.TrimRight(s, "\n") + "\n" }
