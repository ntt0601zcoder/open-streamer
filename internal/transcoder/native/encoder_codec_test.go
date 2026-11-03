package native

import (
	"testing"

	pb "github.com/ntt0601zcoder/open-streamer/internal/transcoder/native/proto"
)

func TestEncoderCodecForBackendResolvesAliases(t *testing.T) {
	t.Parallel()
	tests := []struct {
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
		{"explicit nvenc kept", pb.HWBackend_HW_BACKEND_NVENC, "h264_nvenc", "h264_nvenc"},
		{"empty nvenc", pb.HWBackend_HW_BACKEND_NVENC, "", "h264_nvenc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := encoderCodecForBackend(tt.hw, tt.explicit); got != tt.want {
				t.Fatalf("encoderCodecForBackend(%v, %q) = %q, want %q", tt.hw, tt.explicit, got, tt.want)
			}
		})
	}
}
