// Command sudo is the stand-in the tests of check_postgres put on PATH in
// place of `sudo -u postgres psql`. It behaves the way psql does where the
// command depends on it: a -c command succeeds silently, and a script on
// standard input has its \echo lines written to standard output and a refused
// statement reported on standard error, each at the moment psql reaches it.
//
// The test drives it through the environment:
//
//	FAKEPSQL_LOG     every call's arguments are appended to this file
//	FAKEPSQL_REFUSE  the marker numbers whose EXPLAIN is refused, and
//	                 "schema" to refuse the CREATE TABLE statements
//	FAKEPSQL_CREATE  "refuse" or "silent" to fail CREATE DATABASE with or
//	                 without a message, "vanish" to succeed and then move
//	                 the name PATH finds this binary under out of PATH's
//	                 reach, so the next call cannot start
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	args := strings.Join(os.Args[1:], " ")
	if err := logCall(args); err != nil {
		fmt.Fprintln(os.Stderr, "stand-in:", err)
		os.Exit(2)
	}
	if strings.Contains(args, " -c ") {
		os.Exit(command(args))
	}
	os.Exit(script())
}

// logCall appends one call to the log the test reads back.
func logCall(args string) error {
	f, err := os.OpenFile(filepath.Clean(os.Getenv("FAKEPSQL_LOG")), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(f, args); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// command answers a -c call, which only CREATE DATABASE can fail.
func command(args string) int {
	if !strings.Contains(args, "CREATE DATABASE") {
		return 0
	}
	switch os.Getenv("FAKEPSQL_CREATE") {
	case "refuse":
		fmt.Fprintln(os.Stderr, "ERROR:  permission denied to create database")
		return 1
	case "silent":
		return 3
	case "vanish":
		// The name on PATH, which the test made a link: taking the binary
		// itself away would take it from every test that follows. Renamed
		// rather than removed, because this is the program that is running,
		// and Windows refuses to delete a running program's file while it
		// lets it be renamed. The new name ends in an extension PATHEXT does
		// not list, so no lookup of "sudo" finds it on any platform.
		self, err := exec.LookPath("sudo")
		if err == nil {
			err = os.Rename(self, self+".vanished")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "stand-in:", err)
			return 2
		}
	}
	return 0
}

// script plays a script read from standard input, the part that matters being
// the order in which the markers and the refusals reach the two streams.
func script() int {
	refuse := strings.Fields(os.Getenv("FAKEPSQL_REFUSE"))
	marker := ""
	lines := bufio.NewScanner(os.Stdin)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for lines.Scan() {
		line := lines.Text()
		switch {
		case strings.HasPrefix(line, `\echo @@ `):
			marker = strings.TrimPrefix(line, `\echo @@ `)
			fmt.Fprintln(os.Stdout, "@@ "+marker)
		case strings.HasPrefix(line, "CREATE TABLE ") && slices.Contains(refuse, "schema"):
			fmt.Fprintln(os.Stderr, `ERROR:  type "nope" does not exist`)
		case strings.HasPrefix(line, "EXPLAIN ") && slices.Contains(refuse, marker):
			fmt.Fprintf(os.Stderr, "ERROR:  column \"q%s\" does not exist\n", marker)
		}
	}
	if lines.Err() != nil {
		fmt.Fprintln(os.Stderr, "stand-in:", lines.Err())
		return 2
	}
	// psql exits non-zero once a statement has failed; the command must not
	// read that as a failure to run.
	return 1
}
