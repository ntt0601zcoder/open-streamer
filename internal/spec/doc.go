// Package spec is the stream document: what an operator writes to describe
// one stream (its inputs, its ladder, its outputs), how that document is
// decoded and validated, and how two versions of it differ.
//
// The package is pure data. It imports only the standard library and
// internal/redact, starts no goroutines and holds no state. Every field
// carries a yaml tag equal to its json tag, so a stream reads the same in
// the config file and in the API.
//
// Validate checks the shape of a document: names, counts, lengths and the
// choices the product offers. Ranges the transcoder owns (presets,
// B-frames, sample rates, opacity, the thumbnail's fields) are left to the
// stream manager's check.
//
// Diff sorts an edit into one of four classes, by how much of the stream it
// touches: none, outputs only, live (the inputs or the watermark, applied
// to the running pipeline) and rebuild. Assets names the asset files a
// document refers to: the watermark's image or font.
package spec
