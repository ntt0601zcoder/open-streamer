// Command ostranscoder is the per-stream transcoder child that
// internal/transcode spawns: it decodes the source once and encodes an
// H.264 ladder through libavcodec (internal/transcode/native), each rung
// with libx264 or NVENC; an NVENC rung that fails to open falls back to
// libx264, unless the rung sets NoFallback: the build then fails, and the
// fatal Error carries Code ErrorEncoderRefused and the rung.
//
// It needs cgo and FFmpeg 8.1 and builds only with the build tag libav
// (make ostranscoder bundles it with its libraries); without the tag it is a
// stub that prints "ostranscoder: built without -tags libav" and exits 2,
// whatever its arguments, -probe font included: the server reads 2 as no
// verdict.
//
// Usage:
//
//	ostranscoder               serves the wire protocol on fd 3, a Unix socket
//	ostranscoder -version      prints "ostranscoder wire <N> libavcodec <x.y.z>"
//	ostranscoder -probe nvenc  opens and closes an NVENC encoder: prints
//	                           "nvenc: ok" and exits 0, or "nvenc: <error>"
//	                           and exits 3
//	ostranscoder -probe font FILE
//	                           reads at most 24 MiB + 1 byte of FILE, checks
//	                           it as the server does, then draws text with
//	                           it as a watermark would: prints "font: ok"
//	                           and exits 0, or one line "font: <reason>",
//	                           without FILE's path, and exits 3
//
// On fd 3 it writes Hello, reads Configure, then turns every Frame into
// encoded frames and, when Configure asks for a thumbnail, Shot messages
// (from keys alone when there are no rungs), drains on Discontinuity,
// swaps the watermark at the next picture on SetWatermark (one it cannot
// take is fatal), and writes Stats every second. A watermark's font is
// written to the child's private temp directory, where drawtext reads it;
// fontconfig is given no font, so a font FreeType refuses fails the
// watermark instead of being drawn in a system font. A shot that cannot be made
// is counted in Stats.ThumbnailsFailed, never fatal; from keys alone that is
// a key that gives no picture, while a damaged key the decoder conceals
// still gives a shot. It exits 0 after flushing on Stop or EOF from the
// host, 1 after writing a fatal Error, and 2 on bad usage. libav logs go to
// stderr at warning level.
package main
