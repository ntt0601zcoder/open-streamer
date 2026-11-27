// Package picture holds the watermark and thumbnail specs shared by the host
// and the transcoding child. It is pure Go, imports only the standard
// library, starts no goroutines and must never import cgo.
package picture

import _ "time/tzdata" // Zone names must resolve the same in the host and the child.
