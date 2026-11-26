// Package api is the server's REST API: the streams, the assets, the config
// editor, the reload, the status documents, the metrics and pprof, behind
// HTTP Basic authentication with two roles (a viewer reads, an editor reads
// and writes).
//
// # The order of one request
//
// The outer wrapper, API.ServeHTTP, fixes the order, and nothing of ours
// runs before its turn:
//
//  1. The route, from the pattern http.ServeMux.Handler returns and nothing
//     else. A path the mux redirects (a doubled slash, a "..", a missing
//     trailing slash) resolves to its target's route.
//  2. An unmatched request gets the mux's own 404 or 405 without
//     authentication; an open route (/healthz) is dispatched.
//  3. A PUT, DELETE or POST without X-Requested-By is refused, before any
//     credential is looked at: a cross-site form cannot set the header.
//  4. The Basic credentials; none is a 401 that counts against nobody.
//  5. The limiter and the users, in the order internal/auth documents:
//     Blocked, Remembered, Reserve, Verify.
//  6. The route's role, and an editor for ?secrets=1.
//  7. The dispatch through the mux, which fills the path values. A handler
//     of ours refuses a request the wrapper did not pass for its own route.
//
// # Secrets
//
// Stream documents are redacted in every response unless the query is
// exactly secrets=1 from an editor. The config text holds stream keys and
// password hashes and is an editor's. Nothing here logs a header, a query,
// a body, a spec or a password; every error text goes through
// logging.Scrub.
//
// # Assets
//
// The asset routes take a name asset.KindOf accepts, or answer 404 without
// asking the Backend. An upload is application/octet-stream; a
// Content-Length over its kind's asset.MaxBytes is a 413 before a byte is
// read, and the body is read through http.MaxBytesReader. At most two
// uploads run at once (R-E6): an upload waits up to 30 s for a slot, and only
// once it holds one are its deadlines set, through http.ResponseController:
// the body must arrive within 30 s plus 1 s per 256 KiB of its kind's limit,
// and the answer within 60 s more. A GET streams the Backend's open file with
// http.ServeContent under a write deadline of the same formula for the file's
// size, and closes it. The asset errors that carry a path (a write, an open,
// the font probe) are logged and answered with a fixed message.
//
// The package starts no goroutine.
package api
