// Package stream runs one stream from its spec: a Supervisor builds a
// generation (the source hub, the pipeline, the transcoder or thumbnailer
// and the outputs), serves it, applies output edits in place, and rebuilds
// it for any other edit or after a failure, with a backoff.
//
// # Goroutines
//
// Supervisor.Run is the only goroutine that builds, edits and tears down a
// generation. Every goroutine of a generation is started by it and waited
// for by it: the long-lived parts through generation.run, the packagers and
// the pushers in reattach and teardown. Status, View, Shot and Collect are
// safe from any goroutine and never block on Run.
package stream
