//go:build unix

package transcode

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// spawnSupported reports whether this platform can run the child.
const spawnSupported = true

// startProc starts binary in its own process group with the child's end of
// a socketpair as fd 3, and returns the host's end.
func startProc(binary string, args, env []string, stderr io.Writer) (*proc, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("transcode: spawn: %w", os.NewSyscallError("socketpair", err))
	}
	host := os.NewFile(uintptr(fds[0]), "transcode-host")
	peer := os.NewFile(uintptr(fds[1]), "transcode-child")
	defer func() { _ = peer.Close() }()
	conn, err := net.FileConn(host)
	_ = host.Close()
	if err != nil {
		return nil, fmt.Errorf("transcode: spawn: %w", err)
	}

	cmd := exec.Command(binary, args...) //nolint:noctx // stopped through Stop and the process group
	cmd.Env = env
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{peer}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("transcode: spawn: %w", err)
	}
	return &proc{cmd: cmd, conn: conn, pid: cmd.Process.Pid}, nil
}

// killGroup sends SIGKILL to the child's process group.
func (p *proc) killGroup() {
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
}
