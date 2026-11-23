//go:build unix

package testpeer

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs cmd in a process group of its own and makes its
// cancellation kill the whole group, so that a wrapper (timeout) does not
// leave its child behind.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:wrapcheck // exec.Cmd contract
	}
}
