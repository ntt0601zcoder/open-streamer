//go:build unix

package ingest

import (
	"net"
	"syscall"
)

// effectiveReadBuffer returns the socket's receive buffer as the kernel
// reports it, or 0 when it cannot be read.
func effectiveReadBuffer(pc *net.UDPConn) uint64 {
	rc, err := pc.SyscallConn()
	if err != nil {
		return 0
	}
	var v int
	_ = rc.Control(func(fd uintptr) {
		v, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	})
	return uint64(max(v, 0))
}
