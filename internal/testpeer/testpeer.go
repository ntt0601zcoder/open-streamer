// Package testpeer finds the external programs that tests talk to.
package testpeer

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// EnvRequire names the environment variable that turns a missing peer from a
// skip into a failure. CI sets it to "1".
const EnvRequire = "OS_REQUIRE_PEERS"

// EnvMediaMTX names the environment variable that holds the host:port of a
// MediaMTX RTMP listener for tests, and EnvRequireMediaMTX the one that turns
// its absence from a skip into a failure. CI sets them to "127.0.0.1:19350"
// and "1", keeping port 1935 free for tests of the RTMP default port.
const (
	EnvMediaMTX        = "OS_MEDIAMTX_RTMP"
	EnvRequireMediaMTX = "OS_REQUIRE_MEDIAMTX"
)

// Require returns the path of binary, which must support every listed
// protocol (checked with "-hide_banner -protocols", so it applies to ffmpeg
// and ffprobe). When the binary or a protocol is missing the test is skipped,
// or failed when OS_REQUIRE_PEERS=1.
func Require(t testing.TB, binary string, protocols ...string) string {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		missing(t, "peer %s not found: %v", binary, err)
		return ""
	}
	if len(protocols) == 0 {
		return path
	}
	out, err := exec.CommandContext(t.Context(), path, "-hide_banner", "-protocols").CombinedOutput()
	if err != nil {
		missing(t, "peer %s: listing protocols: %v", binary, err)
		return ""
	}
	have := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	for _, p := range protocols {
		if !have[p] {
			missing(t, "peer %s lacks protocol %s", binary, p)
			return ""
		}
	}
	return path
}

// MediaMTX returns the host:port of the MediaMTX RTMP listener named by
// OS_MEDIAMTX_RTMP. When it is unset the test is skipped, or failed when
// OS_REQUIRE_MEDIAMTX=1. MediaMTX is a running server, not a binary, so
// OS_REQUIRE_PEERS does not apply to it.
func MediaMTX(t testing.TB) (rtmpAddr string) {
	t.Helper()
	if addr := os.Getenv(EnvMediaMTX); addr != "" {
		return addr
	}
	if os.Getenv(EnvRequireMediaMTX) == "1" {
		t.Fatalf("peer MediaMTX: %s is not set", EnvMediaMTX)
		return ""
	}
	t.Skipf("peer MediaMTX: %s is not set", EnvMediaMTX)
	return ""
}

func missing(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf(format, args...)
		return
	}
	t.Skipf(format, args...)
}
