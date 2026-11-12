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

func missing(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf(format, args...)
		return
	}
	t.Skipf(format, args...)
}
