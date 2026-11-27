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
//
// A document's secrets are the credentials inside its input and push URLs
// and outputs.srt_passphrase. They are write-only: Redacted is the view
// every response shows, with each secret replaced by the mask xxxxx, and
// Unmask resolves a document that was written back with masks in it against
// the stored one. A mask stands only for a stored URL of the same stream
// and the same list whose redacted form equals the sent text, so changing
// anything around a mask (host, port, user, path) needs the secret typed
// again. Masks of one form that stand for several stored values are
// resolved by order of occurrence when their counts are equal, and refused
// otherwise (KeepSecrets). Validate refuses a mask wherever it is left, so
// one is never stored.
package spec
