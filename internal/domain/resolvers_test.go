package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ResolveVideoEncoder is what the native transcoder encodes with and what
// the API's defaults table shows; cases pin every routing branch.
func TestResolveVideoEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		codec VideoCodec
		hw    HWAccel
		want  string
	}{
		{"empty + nvenc → h264_nvenc", "", HWAccelNVENC, "h264_nvenc"},
		{"empty + cpu → libx264", "", HWAccelNone, "libx264"},
		{"empty + vaapi → h264_vaapi", "", HWAccelVAAPI, "h264_vaapi"},
		{"h264 + qsv → h264_qsv", "h264", HWAccelQSV, "h264_qsv"},
		{"avc + videotoolbox → h264_videotoolbox", "avc", HWAccelVideoToolbox, "h264_videotoolbox"},
		{"hevc + vaapi → hevc_vaapi", "hevc", HWAccelVAAPI, "hevc_vaapi"},
		{"h265 + qsv → hevc_qsv", "h265", HWAccelQSV, "hevc_qsv"},
		{"hevc + videotoolbox → hevc_videotoolbox", "hevc", HWAccelVideoToolbox, "hevc_videotoolbox"},
		{"av1 + nvenc stays libsvtav1", "av1", HWAccelNVENC, "libsvtav1"},
		{"explicit av1_qsv preserved", "av1_qsv", HWAccelNone, "av1_qsv"},
		{"explicit libsvtav1 preserved", "libsvtav1", HWAccelNVENC, "libsvtav1"},
		{"h264 + nvenc → h264_nvenc", "h264", HWAccelNVENC, "h264_nvenc"},
		{"avc + cpu → libx264", "avc", HWAccelNone, "libx264"},
		{"h265 + nvenc → hevc_nvenc", "h265", HWAccelNVENC, "hevc_nvenc"},
		{"hevc + cpu → libx265", "hevc", HWAccelNone, "libx265"},
		{"av1 → libsvtav1", "av1", HWAccelNone, "libsvtav1"},
		{"mp2v → mpeg2video", "mp2v", HWAccelNone, "mpeg2video"},
		{"mpeg2video alias", "mpeg2video", HWAccelNone, "mpeg2video"},
		{"explicit h264_nvenc preserved", "h264_nvenc", HWAccelNone, "h264_nvenc"},
		{"explicit h264_qsv preserved", "h264_qsv", HWAccelNone, "h264_qsv"},
		{"unknown garbage → libx264 fallback", "garbage", HWAccelNone, "libx264"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveVideoEncoder(tt.codec, tt.hw))
		})
	}
}

func TestResolveAudioEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		codec AudioCodec
		want  string
	}{
		{"", "aac"},
		{AudioCodecCopy, "aac"},
		{AudioCodecAAC, "aac"},
		{AudioCodecMP2, "mp2"},
		{AudioCodecMP3, "libmp3lame"},
		{AudioCodecAC3, "ac3"},
		{AudioCodecEAC3, "eac3"},
		{"unknown", "aac"},
	}
	for _, tt := range cases {
		t.Run(string(tt.codec), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveAudioEncoder(tt.codec))
		})
	}
}

func TestResolveResizeMode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ResizeModePad, ResolveResizeMode(""))
	assert.Equal(t, ResizeModePad, ResolveResizeMode("garbage"))
	assert.Equal(t, ResizeModeCrop, ResolveResizeMode("crop"))
	assert.Equal(t, ResizeModeCrop, ResolveResizeMode("CROP"))
	assert.Equal(t, ResizeModeStretch, ResolveResizeMode("stretch"))
	assert.Equal(t, ResizeModeFit, ResolveResizeMode("fit"))
	assert.Equal(t, ResizeModePad, ResolveResizeMode("pad"))
}
