//go:build !unix

package config

import "os"

// executable is true: Windows has no execute bit, so a regular file is the
// whole check.
func executable(os.FileMode) bool { return true }
