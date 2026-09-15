// Package ghchronicle exists for one reason: it is the only package that can
// read the repository's VERSION file at compile time.
//
// A go:embed directive cannot reach outside its own package directory, and
// VERSION sits at the module root because everything that has to agree on a
// release number reads it from there: the Makefile, the Dockerfile build args
// and the release workflow's preflight, which refuses to release when the
// pushed tag and the file disagree. A package at the root can embed it;
// nothing under cmd/ or internal/ can.
//
// Without this, a build nobody stamped reports "dev". That was true of every
// image the plain Dockerfile produced and of every `go install
// github.com/jmrplens/ghchronicle/cmd/ghchronicle@latest`, because the real
// number only ever arrived through the release ldflags, and a plausible wrong
// version is not something anyone notices.
package ghchronicle

import (
	// The //go:embed directive below reads VERSION at compile time, which
	// only works in a file that imports embed. Nothing here calls it.
	_ "embed"
	"strings"
)

// versionFile is the raw contents of VERSION, trailing newline included.
//
//go:embed VERSION
var versionFile string

// Version is the release this build was compiled from. It is the floor, never
// a guess: release ldflags may still override what a command reports, which is
// what happens when a tag names a number the file has not caught up with yet.
var Version = strings.TrimSpace(versionFile)
