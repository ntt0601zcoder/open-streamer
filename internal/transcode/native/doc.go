//go:build libav

// Package native is the transcoder's media pipeline: it decodes each source
// track once with libavcodec, scales and encodes a software H.264 ladder,
// and re-encodes or drops audio, keeping every rung keyframe-aligned with
// the source. It uses cgo through go-astiav and builds only with the build
// tag libav, against FFmpeg 8.1 (scripts/ffmpeg-dev.sh).
package native

// go-astiav is the package's libav binding; the blank import keeps it in
// the module graph until the pipeline imports it by name.
import _ "github.com/asticode/go-astiav"
