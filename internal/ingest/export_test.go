package ingest

import (
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
)

// setRTMPListenPolicy sets the listener policy that Open passes to the
// registry until the test ends. Tests that use it must not run in parallel.
func setRTMPListenPolicy(t *testing.T, p rtmpsrv.Policy) {
	t.Helper()
	old := rtmpListenPolicy
	rtmpListenPolicy = p
	t.Cleanup(func() { rtmpListenPolicy = old })
}

// setRTMPListeners makes r the registry of the shared RTMP listeners until
// the test ends. Tests that use it must not run in parallel.
func setRTMPListeners(t *testing.T, r *rtmpsrv.Registry) {
	t.Helper()
	old := rtmpListeners
	rtmpListeners = r
	t.Cleanup(func() { rtmpListeners = old })
}
