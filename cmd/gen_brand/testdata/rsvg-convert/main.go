// Command rsvg-convert is the stand-in the tests of gen_brand put on PATH in
// place of librsvg's rasterizer. It takes the arguments gen_brand passes, `-w
// <width> <in.svg> -o <out.png>`, checks that the SVG is there to be read, and
// writes the arguments it was given into the PNG, so the test can see from
// the file what it was asked for and in which directory. Both names are
// resolved inside the working directory, as bare names from gen_brand are.
//
// FAKERSVG_FAIL makes it refuse, the way a real one does with an SVG it cannot
// parse: a message on standard error and a failing status.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	if os.Getenv("FAKERSVG_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "rsvg-convert: Error reading SVG: XML parse error")
		os.Exit(1)
	}
	if len(args) != 5 || args[0] != "-w" || args[3] != "-o" {
		fmt.Fprintf(os.Stderr, "rsvg-convert: unexpected arguments %q\n", args)
		os.Exit(2)
	}
	// gen_brand passes bare names and runs this in the output directory, so
	// both files are opened inside it and nowhere else.
	dir, err := os.OpenRoot(".")
	if err == nil {
		_, err = dir.Stat(args[2])
	}
	if err == nil {
		err = dir.WriteFile(args[4], []byte(strings.Join(args, " ")), 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rsvg-convert:", err)
		os.Exit(1)
	}
}
