//go:build libav

// Package native is the transcoder's media pipeline: it decodes each source
// track once with libavcodec, scales and encodes a software H.264 ladder,
// and re-encodes or drops audio, keeping every rung keyframe-aligned with
// the source. It uses cgo through go-astiav and builds only with the build
// tag libav, against FFmpeg 8.1 (scripts/ffmpeg-dev.sh).
package native
