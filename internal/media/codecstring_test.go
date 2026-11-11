package media_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// fixtureConfigs demuxes a fixture and returns the first config of each codec.
func fixtureConfigs(t *testing.T, name string) map[media.Codec]*media.TrackConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	out := map[media.Codec]*media.TrackConfig{}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		if f.Config != nil {
			if _, ok := out[f.Config.Codec]; !ok {
				out[f.Config.Codec] = f.Config
			}
		}
	}
}

func TestCodecString(t *testing.T) {
	tests := []struct {
		fixture string
		codec   media.Codec
		want    string
	}{
		{"h264_aac.ts", media.CodecH264, "avc1.4D400D"},
		{"h264_640.ts", media.CodecH264, "avc1.4D401E"},
		{"h265_aac51.ts", media.CodecH265, "hvc1.1.6.L60.90"},
		{"h264_aac.ts", media.CodecAAC, "mp4a.40.2"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+tt.codec.String(), func(t *testing.T) {
			cfg := fixtureConfigs(t, tt.fixture)[tt.codec]
			require.NotNil(t, cfg)
			got, err := media.CodecString(cfg)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("asc", func(t *testing.T) {
		cfg, err := media.NewAACConfig([]byte{0x11, 0x90})
		require.NoError(t, err)
		got, err := media.CodecString(cfg)
		require.NoError(t, err)
		assert.Equal(t, "mp4a.40.2", got)
	})
	t.Run("errors", func(t *testing.T) {
		_, err := media.CodecString(nil)
		assert.ErrorContains(t, err, "media: codec string")
		_, err = media.CodecString(&media.TrackConfig{Codec: media.CodecH264})
		assert.ErrorContains(t, err, "media: codec string")
		_, err = media.CodecString(&media.TrackConfig{Codec: media.CodecAAC})
		assert.ErrorContains(t, err, "media: codec string")
		_, err = media.CodecString(&media.TrackConfig{Codec: 99})
		assert.ErrorContains(t, err, "media: codec string")
	})
}
