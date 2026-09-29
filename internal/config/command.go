package config

import (
	"runtime"
	"strings"
	"unicode"
)

// CommandLine is a command the reader can copy into a shell and run: the
// binary, -config naming the configuration at path, and flags. Every command
// the binary prints for somebody to run next is made here, so that each one
// carries the path the way the shell of the system it runs on reads it back.
func CommandLine(path string, flags ...string) string {
	return strings.Join(append([]string{"ghchronicle", "-config", QuotePath(path)}, flags...), " ")
}

// QuotePath is path as the shell of the system this runs on reads it back:
// see QuotePathFor.
func QuotePath(path string) string { return QuotePathFor(runtime.GOOS, path) }

// QuotePathFor is path as a shell of goos reads it back, by one rule on every
// system: a path made of nothing but letters, digits, the system's separator
// and / . _ - : is written as it is, and any other is quoted. The system is
// passed rather than read so that the Windows form is tested on every
// machine, and not only compiled on all but one.
//
// On Windows the quotes are double, the one form PowerShell and cmd both
// read as a single argument: cmd keeps a single quote as part of the word,
// so a path in single quotes reached the binary with the quotes in it and
// named no file. A Windows path cannot hold a double quote, so nothing inside
// needs escaping. What either shell still reads inside double quotes, a $ or
// a backquote for PowerShell and a %NAME% for cmd, cannot be written so that
// both read it back, and is left as it is.
//
// Elsewhere the quotes are single, inside which a POSIX shell expands
// nothing at all, and a single quote in the path is closed, escaped and
// opened again.
func QuotePathFor(goos, path string) string {
	separator := '/'
	if goos == "windows" {
		separator = '\\'
	}
	read := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != separator && !strings.ContainsRune("/._-:", r)
	}
	switch {
	case path != "" && !strings.ContainsFunc(path, read):
		return path
	case goos == "windows":
		return `"` + path + `"`
	default:
		return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	}
}
