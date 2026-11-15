package ingest

import "testing"

// setRTMPListenPolicy sets the listener policy that Open passes to the
// registry until the test ends. Tests that use it must not run in parallel.
func setRTMPListenPolicy(t *testing.T, p listenPolicy) {
	t.Helper()
	old := rtmpListenPolicy
	rtmpListenPolicy = p
	t.Cleanup(func() { rtmpListenPolicy = old })
}
