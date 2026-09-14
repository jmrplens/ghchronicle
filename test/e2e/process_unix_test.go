//go:build !windows

// process_unix_test.go is what the harness needs to know about the collector's
// process on every platform but Windows: its program name takes no extension,
// and it is asked to stop with os.Interrupt, which the collector stops on and
// which reaches a process however it was started.
package e2e

import (
	"os"
	"os/exec"
)

// exeSuffix is what a built program's name ends in here: nothing.
const exeSuffix = ""

// prepareForTermination leaves the command as it is: a signal needs no
// preparation of the process that will receive it.
func prepareForTermination(*exec.Cmd) {}

// signalTermination asks the collector to shut down cleanly.
func signalTermination(p *os.Process) error {
	return p.Signal(os.Interrupt)
}
