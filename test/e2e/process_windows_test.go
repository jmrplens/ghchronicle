//go:build windows

// process_windows_test.go is what the harness needs to know about the
// collector's process on Windows: its program name needs the .exe extension,
// and it cannot be sent os.Interrupt. os.Process's Signal refuses every signal
// there but Kill, and Kill is TerminateProcess, which would stop the collector
// without closing its sinks and so test nothing about shutdown. What a Windows
// console sends instead is a CTRL_BREAK event to a process group, and the Go
// runtime delivers that to the collector as the os.Interrupt it stops on.
package e2e

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// exeSuffix is what a built program's name ends in here. go build -o writes
// exactly the name it is given, and os/exec only finds a program by a name
// that ends in an extension PATHEXT lists.
const exeSuffix = ".exe"

// prepareForTermination puts the collector in a console process group of its
// own, so a CTRL_BREAK aimed at that group reaches the collector and nothing
// else, the test binary included.
func prepareForTermination(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// signalTermination sends CTRL_BREAK to the process group the collector was
// started in, whose id is the collector's pid.
func signalTermination(p *os.Process) error {
	pid := p.Pid
	if pid < 0 || uint64(pid) > math.MaxUint32 {
		return errors.New("the collector's pid is not a Windows process id")
	}
	return windows.GenerateConsoleCtrlEvent(syscall.CTRL_BREAK_EVENT, uint32(pid))
}
