package domain

import "strings"

// Resolve* helpers map user-facing config aliases to the values FFmpeg
// (or downstream services) will actually use. They live here so the
// transcoder package can call them without importing FFmpeg-specific
// internals — the same lookup tables stay testable in pure Go.

// ResolveVideoEncoder maps a user-facing codec string + global HW backend
// to the libavcodec encoder name. It is the single source of truth for both
// the native transcoder and the API's codec×HW defaults table.
//
// Routing:
//   - "" / "h264" / "avc"  → h264_<hw> (h264_nvenc, h264_vaapi, h264_qsv,
//     h264_videotoolbox); libx264 on CPU
//   - "h265" / "hevc"      → hevc_<hw>; libx265 on CPU
//   - "av1"                → libsvtav1 on every backend (hardware AV1 needs
//     recent GPUs; spell av1_nvenc / av1_vaapi / av1_qsv to opt in)
//   - "mp2v" / "mpeg2video" → mpeg2video on every backend
//   - an explicit encoder name (h264_nvenc, libx264, av1_qsv, …) is kept
//   - anything else falls back to libx264
func ResolveVideoEncoder(codec VideoCodec, hw HWAccel) string {
	c := strings.TrimSpace(strings.ToLower(string(codec)))
	switch c {
	case "", "h264", "avc":
		return hwEncoder("h264", hw, "libx264")
	case "h265", "hevc":
		return hwEncoder("hevc", hw, "libx265")
	case "av1":
		return "libsvtav1"
	case "mp2v", "mpeg2video":
		// FFmpeg's built-in MPEG-2 Part 2 encoder — always available, no
		// external library. Used when feeding a DVB transmitter chain or
		// legacy headend system that consumes MPEG-2 input.
		return "mpeg2video"
	}
	if isEncoderName(c) {
		return string(codec)
	}
	return "libx264"
}

// hwEncoder is base_<backend> (h264_nvenc, hevc_vaapi, …), or cpu when the
// backend is none or unknown.
func hwEncoder(base string, hw HWAccel, cpu string) string {
	switch hw {
	case HWAccelNVENC, HWAccelVAAPI, HWAccelQSV, HWAccelVideoToolbox:
		return base + "_" + string(hw)
	default:
		return cpu
	}
}

// isEncoderName reports whether c already names a libavcodec encoder
// (lower-cased): a hardware encoder (<codec>_<backend>), a lib* software
// encoder, or an H.264/H.265 name.
func isEncoderName(c string) bool {
	for _, hw := range []string{"_nvenc", "_vaapi", "_qsv", "_videotoolbox"} {
		if strings.HasSuffix(c, hw) {
			return true
		}
	}
	return strings.HasPrefix(c, "lib") || strings.Contains(c, "264") || strings.Contains(c, "265") || strings.Contains(c, "hevc")
}

// ResolveAudioEncoder maps a user-facing codec to FFmpeg's encoder name.
// Empty / "copy" → AAC default (since copy is decided separately via
// AudioTranscodeConfig.Copy).
//
// MP2 (mp2a) maps to FFmpeg's built-in `mp2` encoder — MPEG-1 Layer II,
// always available in standard FFmpeg builds (no external library needed,
// unlike libtwolame). Valid bitrates 32-384 kbps; FFmpeg silently rounds
// to the nearest valid Layer II rate. Used for DVB broadcast contribution
// feeds and legacy IPTV headends that consume Layer II input.
func ResolveAudioEncoder(codec AudioCodec) string {
	c := strings.TrimSpace(strings.ToLower(string(codec)))
	switch c {
	case "", string(AudioCodecCopy), string(AudioCodecAAC):
		return "aac"
	case string(AudioCodecMP2):
		return "mp2"
	case string(AudioCodecMP3):
		return "libmp3lame"
	case string(AudioCodecAC3):
		return "ac3"
	case string(AudioCodecEAC3):
		return "eac3"
	}
	return "aac"
}

// ResolveResizeMode normalizes a free-form resize mode to the canonical
// constant. Empty / unknown → ResizeModePad.
func ResolveResizeMode(m ResizeMode) ResizeMode {
	switch ResizeMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case ResizeModeCrop:
		return ResizeModeCrop
	case ResizeModeStretch:
		return ResizeModeStretch
	case ResizeModeFit:
		return ResizeModeFit
	case ResizeModePad:
		return ResizeModePad
	}
	return ResizeModePad
}
