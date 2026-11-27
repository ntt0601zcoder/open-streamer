//go:build !unix

package asset

import "testing"

// mkfifo skips the test: there are no FIFOs in the file system here.
func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("no FIFOs on this system")
}

// assertNoReader is never reached, since mkfifo skips.
func assertNoReader(t *testing.T, _ string) { t.Helper() }
