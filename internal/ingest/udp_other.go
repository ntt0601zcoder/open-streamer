//go:build !unix

package ingest

import "net"

// effectiveReadBuffer cannot read the socket option off unix platforms: it
// returns 0, so udp_read_buffer is 0 and udp_read_buffer_low reports 1.
func effectiveReadBuffer(*net.UDPConn) uint64 { return 0 }
