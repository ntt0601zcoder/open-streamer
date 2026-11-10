package media

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// Parameter sets extracted from testdata/fixtures/h264_aac.ts and h265_aac51.ts.
const (
	h264SPSHex = "674d400deca0a0fd80880000030008000003019078a14cb0"
	h264PPSHex = "68ebe3cb20"
	h265VPSHex = "40010c01ffff01600000030090000003000003003c959009"
	h265SPSHex = "42010101600000030090000003000003003ca00a080f16595924932bc05a020000030002000003003210"
	h265PPSHex = "4401c172b46240"
)

func TestNewAACConfig(t *testing.T) {
	cases := []struct {
		asc      []byte
		rate, ch int
	}{
		{[]byte{0x11, 0x90}, 48000, 2},
		{[]byte{0x12, 0x10}, 44100, 2},
		{[]byte{0x12, 0x08}, 44100, 1},
	}
	for _, c := range cases {
		cfg, err := NewAACConfig(c.asc)
		require.NoError(t, err)
		assert.Equal(t, CodecAAC, cfg.Codec)
		assert.Equal(t, c.rate, cfg.SampleRate)
		assert.Equal(t, c.ch, cfg.Channels)
		assert.Equal(t, c.asc, cfg.ASC)
	}

	in := []byte{0x11, 0x90}
	cfg, err := NewAACConfig(in)
	require.NoError(t, err)
	in[0] = 0
	assert.Equal(t, []byte{0x11, 0x90}, cfg.ASC)

	_, err = NewAACConfig([]byte{0xff})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media: aac config")
}

func TestNewH264Config(t *testing.T) {
	sps, pps := unhex(t, h264SPSHex), unhex(t, h264PPSHex)
	cfg, err := NewH264Config([][]byte{sps}, [][]byte{pps})
	require.NoError(t, err)
	assert.Equal(t, CodecH264, cfg.Codec)
	assert.Equal(t, 320, cfg.Width)
	assert.Equal(t, 240, cfg.Height)
	assert.Equal(t, [][]byte{sps}, cfg.SPS)
	assert.Equal(t, [][]byte{pps}, cfg.PPS)

	_, err = NewH264Config(nil, [][]byte{pps})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media: h264 config")

	pps1, pps0 := []byte{0x68, 0x50}, []byte{0x68, 0x80}
	cfg, err = NewH264Config([][]byte{sps}, [][]byte{pps1, pps0})
	require.NoError(t, err)
	assert.Equal(t, [][]byte{pps0, pps1}, cfg.PPS)

	_, err = NewH264Config([][]byte{sps}, [][]byte{{0x68, 0x80}, {0x68, 0xc0}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media: h264 config")

	// Copies.
	sps[0] = 0
	assert.Equal(t, byte(0x67), cfg.SPS[0][0])
}

func TestNewH265Config(t *testing.T) {
	vps, sps, pps := unhex(t, h265VPSHex), unhex(t, h265SPSHex), unhex(t, h265PPSHex)
	cfg, err := NewH265Config([][]byte{vps}, [][]byte{sps}, [][]byte{pps})
	require.NoError(t, err)
	assert.Equal(t, CodecH265, cfg.Codec)
	assert.Equal(t, 320, cfg.Width)
	assert.Equal(t, 240, cfg.Height)
	assert.Equal(t, [][]byte{vps}, cfg.VPS)
	assert.Equal(t, [][]byte{sps}, cfg.SPS)
	assert.Equal(t, [][]byte{pps}, cfg.PPS)

	_, err = NewH265Config([][]byte{vps}, nil, [][]byte{pps})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media: h265 config")

	_, err = NewH265Config(nil, [][]byte{sps}, [][]byte{pps})
	require.Error(t, err)

	// PPS ids: 4401 c1.. -> 0; 4401 50 -> 1.
	pps1 := []byte{0x44, 0x01, 0x50}
	cfg, err = NewH265Config([][]byte{vps}, [][]byte{sps}, [][]byte{pps1, pps})
	require.NoError(t, err)
	assert.Equal(t, [][]byte{pps, pps1}, cfg.PPS)

	_, err = NewH265Config([][]byte{vps}, [][]byte{sps}, [][]byte{pps, pps})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media: h265 config")
}

func TestParamSetID(t *testing.T) {
	id, err := ParamSetID(CodecH264, unhex(t, h264SPSHex))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), id)

	id, err = ParamSetID(CodecH264, unhex(t, h264PPSHex))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), id)

	id, err = ParamSetID(CodecH264, []byte{0x68, 0x21, 0x80})
	require.NoError(t, err)
	assert.Equal(t, uint32(3), id)

	id, err = ParamSetID(CodecH265, unhex(t, h265VPSHex))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), id)

	id, err = ParamSetID(CodecH265, unhex(t, h265SPSHex))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), id)

	id, err = ParamSetID(CodecH265, unhex(t, h265PPSHex))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), id)

	_, err = ParamSetID(CodecAAC, []byte{1, 2})
	assert.Error(t, err)
	_, err = ParamSetID(CodecH264, nil)
	assert.Error(t, err)
}

func TestTrackConfigEqual(t *testing.T) {
	mk := func(pps []byte) *TrackConfig {
		c, err := NewH264Config([][]byte{unhex(t, h264SPSHex)}, [][]byte{pps})
		require.NoError(t, err)
		return c
	}
	a, b := mk(unhex(t, h264PPSHex)), mk(unhex(t, h264PPSHex))
	assert.True(t, a.Equal(b))
	assert.False(t, a.Equal(mk([]byte{0x68, 0xeb, 0xe3, 0xcb, 0x21})))
	assert.False(t, a.Equal(nil))
	assert.False(t, (*TrackConfig)(nil).Equal(a))
	assert.True(t, (*TrackConfig)(nil).Equal(nil))

	aac, err := NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	assert.False(t, a.Equal(aac))
}
