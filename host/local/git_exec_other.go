//go:build !unix

package local

import (
	"errors"
	"os"
	"os/exec"
)

// setProcessGroup is a no-op where process groups are not available; the
// direct child is still killed on timeout or cancellation.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup kills only the direct child on platforms without process
// groups. The real client is only supported on Unix; this keeps the package
// building elsewhere.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
