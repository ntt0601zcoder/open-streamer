// Package wire is the length-prefixed protocol between the transcoder host
// (internal/transcode) and its child (cmd/ostranscoder). Each message is a
// protobuf envelope hand-coded with protowire: no code generation, no protoc.
package wire

// protowire is the package's encoding; the blank import keeps it in the
// module graph until the codec imports it by name.
import _ "google.golang.org/protobuf/encoding/protowire"
