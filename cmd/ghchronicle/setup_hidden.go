package main

import (
	"os"

	"golang.org/x/term"
)

// readHidden takes a line from a terminal without echoing it.
//
// golang.org/x/term is a fourth direct dependency on a module that has two, so
// it is worth saying why: a token typed in the open stays in the scrollback of
// whatever window it was typed into, and in the history of whatever recorded
// that session. The alternative was shelling out to `stty -echo`, which is not
// a thing Windows has. This package depends only on golang.org/x/sys, which is
// already here.
func readHidden(in *os.File) (string, error) {
	typed, err := term.ReadPassword(int(in.Fd()))
	return string(typed), err
}

// interactive says whether there is somebody on the other end, which is the
// terminal question and lives here because this is where the package that
// answers it already is.
func interactive(in *os.File) bool {
	return term.IsTerminal(int(in.Fd()))
}
