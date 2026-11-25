//go:build unix

package config

import "os"

// executable reports whether the mode has an execute bit.
func executable(m os.FileMode) bool { return m.Perm()&0o111 != 0 }
