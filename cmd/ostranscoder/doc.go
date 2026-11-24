// Command ostranscoder is the per-stream transcoder child that
// internal/transcode spawns: it decodes the source once and encodes a
// software H.264 ladder through libavcodec (internal/transcode/native).
//
// It needs cgo and FFmpeg 8.1 and builds only with the build tag libav
// (make ostranscoder bundles it with its libraries); without the tag it is a
// stub that prints "ostranscoder: built without -tags libav" and exits 2.
//
// Usage:
//
//	ostranscoder            serves the wire protocol on fd 3, a Unix socket
//	ostranscoder -version   prints "ostranscoder wire <N> libavcodec <x.y.z>"
//
// On fd 3 it writes Hello, reads Configure, then turns every Frame into
// encoded frames, drains on Discontinuity, and writes Stats every second. It
// exits 0 after flushing on Stop or EOF from the host, 1 after writing a
// fatal Error, and 2 on bad usage. libav logs go to stderr at warning level.
package main
