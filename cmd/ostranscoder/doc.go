// Command ostranscoder is the per-stream transcoder child that
// internal/transcode spawns: it decodes the source once and encodes an
// H.264 ladder through libavcodec (internal/transcode/native), each rung
// with libx264 or NVENC; an NVENC rung that fails to open falls back to
// libx264.
//
// It needs cgo and FFmpeg 8.1 and builds only with the build tag libav
// (make ostranscoder bundles it with its libraries); without the tag it is a
// stub that prints "ostranscoder: built without -tags libav" and exits 2.
//
// Usage:
//
//	ostranscoder               serves the wire protocol on fd 3, a Unix socket
//	ostranscoder -version      prints "ostranscoder wire <N> libavcodec <x.y.z>"
//	ostranscoder -probe nvenc  opens and closes an NVENC encoder: prints
//	                           "nvenc: ok" and exits 0, or "nvenc: <error>"
//	                           and exits 3
//
// On fd 3 it writes Hello, reads Configure, then turns every Frame into
// encoded frames, drains on Discontinuity, swaps the watermark at the next
// picture on SetWatermark (one it cannot take is fatal), and writes Stats
// every second. It
// exits 0 after flushing on Stop or EOF from the host, 1 after writing a
// fatal Error, and 2 on bad usage. libav logs go to stderr at warning level.
package main
