//go:build unix

package local

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd in a process group of its own, so killing the
// group takes an ssh (or any other) grandchild with it. exec.CommandContext
// kills only the direct child, and a grandchild holding the stdout pipe would
// otherwise outlive the deadline (RESEARCH Pitfall 14, T-10-12).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs cmd's whole process group. A group that is
// already gone is not an error; if the group kill fails for any other reason
// it falls back to killing the direct child.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		return kerr
	}
	return nil
}
