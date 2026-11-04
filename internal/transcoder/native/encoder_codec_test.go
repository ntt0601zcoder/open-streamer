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
		{"nvenc h264", pb.HWBackend_HW_BACKEND_NVENC, "h264", "h264_nvenc"},
		{"nvenc avc", pb.HWBackend_HW_BACKEND_NVENC, "avc", "h264_nvenc"},
		{"nvenc h265", pb.HWBackend_HW_BACKEND_NVENC, "h265", "hevc_nvenc"},
		{"nvenc hevc", pb.HWBackend_HW_BACKEND_NVENC, "hevc", "hevc_nvenc"},
		{"nvenc av1 stays software", pb.HWBackend_HW_BACKEND_NVENC, "av1", "libsvtav1"},
		{"explicit av1_nvenc kept", pb.HWBackend_HW_BACKEND_NVENC, "av1_nvenc", "av1_nvenc"},
		{"explicit libsvtav1 kept", pb.HWBackend_HW_BACKEND_VAAPI, "libsvtav1", "libsvtav1"},
		{"uppercase alias", pb.HWBackend_HW_BACKEND_QSV, " H264 ", "h264_qsv"},
		{"nvenc explicit kept", pb.HWBackend_HW_BACKEND_NVENC, "h264_nvenc", "h264_nvenc"},
		{"nvenc empty", pb.HWBackend_HW_BACKEND_NVENC, "", "h264_nvenc"},
		{"vaapi h264", pb.HWBackend_HW_BACKEND_VAAPI, "h264", "h264_vaapi"},
		{"vaapi hevc", pb.HWBackend_HW_BACKEND_VAAPI, "hevc", "hevc_vaapi"},
		{"vaapi empty", pb.HWBackend_HW_BACKEND_VAAPI, "", "h264_vaapi"},
		{"qsv h264", pb.HWBackend_HW_BACKEND_QSV, "h264", "h264_qsv"},
		{"qsv h265", pb.HWBackend_HW_BACKEND_QSV, "h265", "hevc_qsv"},
		{"videotoolbox h264", pb.HWBackend_HW_BACKEND_VIDEOTOOLBOX, "avc", "h264_videotoolbox"},
		{"videotoolbox hevc", pb.HWBackend_HW_BACKEND_VIDEOTOOLBOX, "hevc", "hevc_videotoolbox"},
		{"cpu h264", pb.HWBackend_HW_BACKEND_CPU, "h264", "libx264"},
		{"cpu h265", pb.HWBackend_HW_BACKEND_CPU, "h265", "libx265"},
		{"unspecified avc", pb.HWBackend_HW_BACKEND_UNSPECIFIED, "avc", "libx264"},
		{"cpu explicit kept", pb.HWBackend_HW_BACKEND_CPU, "libx264", "libx264"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, encoderCodecForBackend(tc.hw, tc.explicit))
		})
	}
}
