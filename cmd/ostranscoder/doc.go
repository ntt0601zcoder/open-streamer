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
//	ostranscoder -version   prints "ostranscoder wire <N> libavcodec <x.y.z>"
package main
