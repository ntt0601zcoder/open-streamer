//go:build !unix

package server

import "os"

// canSegv is whether the probe stand-in can die by a signal.
const canSegv = false

// dieBySegv is not available here; the row that needs it is skipped.
func dieBySegv() { os.Exit(1) }

// processGone cannot be asked here; it reports true.
func processGone(int) bool { return true }
