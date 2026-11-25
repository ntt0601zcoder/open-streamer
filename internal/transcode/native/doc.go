//go:build libav

// Package native is the transcoder's media pipeline: it decodes each source
// track once with libavcodec, optionally draws a watermark on the decoded
// picture with libavfilter (text through drawtext and an embedded DejaVu
// Sans, read from a file so that operator text never enters a graph
// string), scales and encodes an H.264 ladder, and re-encodes or drops
// audio, keeping every rung keyframe-aligned with the source. It uses cgo through go-astiav and builds only with the build
// tag libav, against FFmpeg 8.1 (scripts/ffmpeg-dev.sh).
package native
