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

// EnvMediaMTXRTSP names the environment variable that holds the host:port of
// a MediaMTX RTSP listener for tests. CI sets it to "127.0.0.1:18554"; that
// MediaMTX runs on the host network, so that UDP transport works, with the
// RTP and RTCP ports 18000 and 18001.
const EnvMediaMTXRTSP = "OS_MEDIAMTX_RTSP"

// EnvMediaMTXPull names the environment variable that, set to "1", says that
// the MediaMTX RTSP listener of OS_MEDIAMTX_RTSP was started with the paths
// m3b1pull and m3b1pullhevc pulling, on demand, from
// rtmp://127.0.0.1:19361/live/m3b1pull and …/live/m3b1pullhevc. CI sets it.
const EnvMediaMTXPull = "OS_MEDIAMTX_PULL"

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

// MediaMTXRTSP returns the host:port of the MediaMTX RTSP listener named by
// OS_MEDIAMTX_RTSP. When it is unset the test is skipped, or failed when
// OS_REQUIRE_MEDIAMTX=1.
func MediaMTXRTSP(t testing.TB) (rtspAddr string) {
	t.Helper()
	if addr := os.Getenv(EnvMediaMTXRTSP); addr != "" {
		return addr
	}
	if os.Getenv(EnvRequireMediaMTX) == "1" {
		t.Fatalf("peer MediaMTX: %s is not set", EnvMediaMTXRTSP)
		return ""
	}
	t.Skipf("peer MediaMTX: %s is not set", EnvMediaMTXRTSP)
	return ""
}

// MediaMTXPull returns the host:port of the MediaMTX RTSP listener whose
// paths pull our RTMP play output (EnvMediaMTXPull). When OS_MEDIAMTX_PULL is
// not "1", or OS_MEDIAMTX_RTSP is unset, the test is skipped, or failed when
// OS_REQUIRE_MEDIAMTX=1.
func MediaMTXPull(t testing.TB) (rtspHostPort string) {
	t.Helper()
	if os.Getenv(EnvMediaMTXPull) == "1" {
		return MediaMTXRTSP(t)
	}
	if os.Getenv(EnvRequireMediaMTX) == "1" {
		t.Fatalf("peer MediaMTX: %s is not 1", EnvMediaMTXPull)
		return ""
	}
	t.Skipf("peer MediaMTX: %s is not 1", EnvMediaMTXPull)
	return ""
}

// RequireMuxer checks that binary (ffmpeg) lists the output format muxer
// ("-hide_banner -muxers"). When it does not the test is skipped, or failed
// when OS_REQUIRE_PEERS=1.
func RequireMuxer(t testing.TB, binary, muxer string) {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		missing(t, "peer %s not found: %v", binary, err)
		return
	}
	out, err := exec.CommandContext(t.Context(), path, "-hide_banner", "-muxers").CombinedOutput()
	if err != nil {
		missing(t, "peer %s: listing muxers: %v", binary, err)
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		// " E  rtsp            RTSP output": flags, then the name.
		if f := strings.Fields(line); len(f) > 2 && strings.Contains(f[0], "E") && f[1] == muxer {
			return
		}
	}
	missing(t, "peer %s lacks muxer %s", binary, muxer)
}

func missing(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf(format, args...)
		return
	}
	t.Skipf(format, args...)
}
