//go:build unix

package ingest

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveReadBuffer(t *testing.T) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	assert.Positive(t, effectiveReadBuffer(pc))
	require.NoError(t, pc.Close())

	// A conn without a socket: SyscallConn fails and the buffer reads as 0.
	assert.Zero(t, effectiveReadBuffer(&net.UDPConn{}))
}
