//go:build !unix

package server

import "os"

// keyExposed is false: without Unix permission bits there is nothing to
// read from the mode.
func keyExposed(os.FileMode) bool { return false }
