//go:build unix

package server

import (
	"errors"
	"os"
	"syscall"
)

// canSegv is whether the probe stand-in can die by a signal.
const canSegv = true

// dieBySegv replaces the process with a shell that kills itself with
// SIGSEGV: the Go runtime ignores a SIGSEGV sent by kill, so the process
// must not be a Go one when it arrives.
func dieBySegv() {
	_ = syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -SEGV $$"}, os.Environ())
	os.Exit(1)
}

// processGone reports whether no process has the PID.
func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
