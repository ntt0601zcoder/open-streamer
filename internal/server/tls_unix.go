//go:build unix

package server

import "os"

// keyExposed reports whether the mode lets the group or others at the file.
func keyExposed(m os.FileMode) bool { return m.Perm()&0o077 != 0 }
