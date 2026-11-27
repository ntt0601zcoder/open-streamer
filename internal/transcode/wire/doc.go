// Package wire is the length-prefixed protocol between the transcoder host
// (internal/transcode) and its child (cmd/ostranscoder). Each message is a
// protobuf envelope hand-coded with protowire: no code generation, no protoc.
//
// The stream is a sequence of records, each a 4-byte big-endian length (at
// most MaxMessage) followed by one envelope. The envelope holds exactly one
// field, and its number selects the message type: 1 Hello, 2 Configure,
// 3 Frame, 4 Discontinuity, 5 Stats, 6 Error, 7 Stop, 8 SetWatermark,
// 9 Shot. The field numbers inside each message are listed beside its
// encoder in wire.go.
//
// Version 2 added a rung's encoder, the Configure watermark and thumbnail,
// SetWatermark, Shot and three Stats counters. Each new field is absent
// when empty, so a Configure without them has version 1's bytes.
//
// Version 3 added a watermark's font (at most picture.MaxFontBytes), a
// rung's no_fallback and an Error's code, each absent when empty, so a
// message without them has version 2's bytes. The Writer computes the
// lengths of a Configure or SetWatermark first and copies a watermark's
// image and font once, into the envelope; the Reader's watermark aliases
// its read buffer.
package wire
