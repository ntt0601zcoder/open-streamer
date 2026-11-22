//go:build linux

package rtsp

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUDPPairSendBuffer checks that the RTP socket of the pair asks for a
// 4 MiB send buffer: Linux grants up to net.core.wmem_max and reports
// twice what it granted.
func TestUDPPairSendBuffer(t *testing.T) {
	raw, err := os.ReadFile("/proc/sys/net/core/wmem_max")
	if err != nil {
		t.Skipf("wmem_max: %v", err)
	}
	wmemMax, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	srv := NewServer(nil, ServerConfig{UDPHost: "127.0.0.1"})
	srv.mu.Lock()
	require.NoError(t, srv.bindUDP())
	srv.mu.Unlock()
	defer srv.closeUDP()
	sc, err := srv.udp.rtp.SyscallConn()
	require.NoError(t, err)
	var got int
	var serr error
	require.NoError(t, sc.Control(func(fd uintptr) {
		got, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	}))
	require.NoError(t, serr)
	assert.GreaterOrEqual(t, got, 2*min(udpSendBuffer, wmemMax))
}
