//go:build unix

package rtsp

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func rcvBuf(t *testing.T, c *net.UDPConn) int {
	t.Helper()
	rc, err := c.SyscallConn()
	require.NoError(t, err)
	var n int
	var serr error
	require.NoError(t, rc.Control(func(fd uintptr) {
		n, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	}))
	require.NoError(t, serr)
	return n
}

// TestClientUDPReadBuffer checks that the client asks for a 4 MiB receive
// buffer on its UDP sockets: the kernel grants more than a fresh socket's
// default (it may cap the request at net.core.rmem_max).
func TestClientUDPReadBuffer(t *testing.T) {
	fresh, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	def := rcvBuf(t, fresh)
	_ = fresh.Close()

	srv := NewServer(udpTestHandler{}, ServerConfig{UDPHost: "127.0.0.1"})
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	c, err := Dial(context.Background(), "rtsp://"+l.Addr().String()+"/live/cam", Config{Transport: "udp", Timeout: 2 * time.Second})
	require.NoError(t, err)
	defer c.Close()
	require.Len(t, c.tracks, 1)
	for _, s := range []*net.UDPConn{c.tracks[0].link.rtp, c.tracks[0].link.rtcp} {
		require.Greater(t, rcvBuf(t, s), def)
	}
}

type udpTestHandler struct{}

func (udpTestHandler) Describe(string) ([]TrackDesc, bool) {
	return []TrackDesc{{
		Codec: CodecH264, Payload: 96, ClockRate: 90000,
		ParamSets: [][]byte{{0x67, 0x42, 0xc0, 0x1e, 0x95}, {0x68, 0xce, 0x3c, 0x80}},
	}}, true
}

func (udpTestHandler) Play(ctx context.Context, _ *ServerSession) { <-ctx.Done() }
