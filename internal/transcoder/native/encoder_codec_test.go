package native

import (
	"testing"

	"github.com/stretchr/testify/assert"

	pb "github.com/ntt0601zcoder/open-streamer/internal/transcoder/native/proto"
)

func TestEncoderCodecForBackend_ResolvesAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		hw       pb.HWBackend
		explicit string
		want     string
	}{
		{"h264 nvenc", pb.HWBackend_HW_BACKEND_NVENC, "h264", "h264_nvenc"},
		{"avc nvenc", pb.HWBackend_HW_BACKEND_NVENC, "avc", "h264_nvenc"},
		{"h265 nvenc", pb.HWBackend_HW_BACKEND_NVENC, "h265", "hevc_nvenc"},
		{"hevc nvenc", pb.HWBackend_HW_BACKEND_NVENC, "hevc", "hevc_nvenc"},
		{"h264 cpu", pb.HWBackend_HW_BACKEND_CPU, "h264", "libx264"},
		{"explicit kept", pb.HWBackend_HW_BACKEND_NVENC, "h264_nvenc", "h264_nvenc"},
		{"empty nvenc", pb.HWBackend_HW_BACKEND_NVENC, "", "h264_nvenc"},
		{"empty cpu", pb.HWBackend_HW_BACKEND_CPU, "", "libx264"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, encoderCodecForBackend(tc.hw, tc.explicit))
		})
	}
}
