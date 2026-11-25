// Package server is the openstreamer process: it reads the one config file,
// binds the listeners (media HTTP, the API, RTMP, RTSP, SRT), runs the
// streams through a stream.Manager, and is the api.Backend, so every change
// of the configuration, from the API or from the file, goes through one
// function.
//
// # The apply path
//
// apply makes a text the config in force: a strict parse of the whole
// document, the manager's judgement of every stream, the write of the file
// (for a text that came over the API), the change of the streams, then the
// bookkeeping. A per-stream PUT or DELETE edits the text in force and applies
// the result; a whole-config PUT applies the client's exact bytes; SIGHUP
// and POST /api/v1/reload apply the file as it is on disk. All four hold one
// mutex for their whole length, so two writers are never interleaved.
//
// The server section is never applied while running: the listeners stay as
// they were bound, and the keys that differ from the ones the process
// started with are reported as restart_required until it restarts.
//
// # Locks
//
// cfg serialises the writers. The readers never take it: the text in force
// and the status are one immutable snapshot behind an atomic pointer, and
// the users another, so a status request, a login and a scrape do not wait
// for an apply that waits for a rebuild. Stale is not in the snapshot: it is
// asked of the file at every ConfigStatus, under the File's own mutex.
//
// # Goroutines
//
// New starts the RTSP and the SRT server's accept loops, the RTMP
// registry's listener and, through Manager.Load, one supervisor per stream.
// Run starts the two HTTP servers and the manager's Run, and stops and
// waits for all of them, New's included, before it returns: a Server that
// New returned must be Run, once.
package server
