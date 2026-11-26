package ingest

import (
	"context"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "fixtures", name)
}

// The stable port range sits below every common ephemeral range: Linux
// defaults to 32768-60999, macOS and Windows to 49152-65535.
const (
	stablePortLow  = 20000
	stablePortHigh = 32767
)

// stableUDPPort returns a UDP port that is free now and lies outside the
// kernel's ephemeral range. See stablePort.
func stableUDPPort(t *testing.T) int {
	t.Helper()
	return stablePort(t, "udp")
}

// stableTCPPort is stableUDPPort for TCP.
func stableTCPPort(t *testing.T) string {
	t.Helper()
	return strconv.Itoa(stablePort(t, "tcp"))
}

// stablePort returns a port on 127.0.0.1 that is free for network now and
// lies below the kernel's ephemeral range. A test that releases a port and
// binds it again later (an aborted open, a Close, a peer that starts slowly)
// needs this: a :0 bind or an outgoing connection in any other process, such
// as another test package run in parallel by go test ./..., is handed ports
// from the ephemeral range only, so it cannot take this one in between. Only
// processes that bind fixed ports can, and the free check here covers those
// already running.
func stablePort(t *testing.T, network string) int {
	t.Helper()
	lo, hi := stablePortLow, stablePortHigh
	if eph := ephemeralLow(); eph > 0 && eph <= hi {
		hi = eph - 1
	}
	if hi-lo < 1000 {
		// The ephemeral range starts unusually low; a port below it is too
		// scarce to pick from, so fall back to a plain free port.
		t.Logf("ephemeral range starts at %d: using an ephemeral port", hi+1)
		return ephemeralFreePort(t, network)
	}
	for range 100 {
		port := lo + rand.IntN(hi-lo+1) //nolint:gosec // a test port, not a secret.
		if port == 554 || port == 1935 || port == 8554 {
			continue // never bind the RTSP and RTMP defaults (outside the range anyway)
		}
		if portFree(network, port) {
			return port
		}
	}
	t.Fatalf("no free %s port in %d-%d after 100 tries", network, lo, hi)
	return 0
}

// ephemeralLow returns the low end of the kernel's ephemeral port range, or 0
// where it cannot be read (every system but Linux).
func ephemeralLow() int {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		return 0
	}
	return n
}

// portFree reports whether network can bind 127.0.0.1:port now.
func portFree(network string, port int) bool {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	lc := new(net.ListenConfig)
	if network == "tcp" {
		ln, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			return false
		}
		_ = ln.Close()
		return true
	}
	pc, err := lc.ListenPacket(context.Background(), "udp", addr)
	if err != nil {
		return false
	}
	_ = pc.Close()
	return true
}

// ephemeralFreePort returns a port the kernel handed out for a :0 bind.
func ephemeralFreePort(t *testing.T, network string) int {
	t.Helper()
	if network == "tcp" {
		p, err := strconv.Atoi(freeTCPPort(t))
		require.NoError(t, err)
		return p
	}
	return freeUDPPort(t)
}
