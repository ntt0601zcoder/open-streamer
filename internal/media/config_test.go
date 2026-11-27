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

// bitWriter builds RBSP bit strings for hand-made parameter sets.
type bitWriter struct{ bits []bool }

func (w *bitWriter) u(n int, v uint64) *bitWriter {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, v>>uint(i)&1 == 1)
	}
	return w
}

func (w *bitWriter) ue(v uint64) *bitWriter {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	return w.u(n, 0).u(n+1, v)
}

// nal appends rbsp_trailing_bits and returns the NAL unit with emulation
// prevention bytes inserted after the header.
func (w *bitWriter) nal(header ...byte) []byte {
	w.u(1, 1)
	for len(w.bits)%8 != 0 {
		w.u(1, 0)
	}
	out := append([]byte{}, header...)
	zeros := 0
	for i := 0; i < len(w.bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if w.bits[i+j] {
				b |= 0x80 >> uint(j)
			}
		}
		if zeros >= 2 && b <= 3 {
			out = append(out, 3) // emulation_prevention_three_byte
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// hostilePOC1SPS returns a Main-profile H.264 SPS with pic_order_cnt_type 1
// and num_ref_frames_in_pic_order_cnt_cycle set to cycle, followed by ones
// zero-valued offsets (one bit each) and a valid tail, so that a parser
// without a range check accepts it.
func hostilePOC1SPS(cycle uint64, ones int) []byte {
	w := &bitWriter{}
	w.u(8, 77).u(8, 0).u(8, 30)
	w.ue(0)      // seq_parameter_set_id
	w.ue(0)      // log2_max_frame_num_minus4
	w.ue(1)      // pic_order_cnt_type
	w.u(1, 0)    // delta_pic_order_always_zero_flag
	w.u(2, 0b11) // offset_for_non_ref_pic = 0, offset_for_top_to_bottom_field = 0
	w.ue(cycle)  // num_ref_frames_in_pic_order_cnt_cycle
	for i := 0; i < ones; i++ {
		w.u(1, 1) // offset_for_ref_frame[i] = 0
	}
	w.ue(1)       // max_num_ref_frames
	w.u(1, 0)     // gaps
	w.ue(19)      // 320
	w.ue(14)      // 240
	w.u(3, 0b110) // frame_mbs_only_flag, direct_8x8_inference_flag, frame_cropping_flag
	w.u(1, 0)     // vui_parameters_present_flag
	return w.nal(0x67)
}

// hostileSCCSPS returns a 320x240 H.265 SPS without VUI whose SCC extension
// declares 2^40 palette predictor initializers; mp4ff runs out of memory on
// it.
func hostileSCCSPS() []byte {
	w := &bitWriter{}
	w.u(4, 0).u(3, 0).u(1, 1)
	w.u(2, 0).u(1, 0).u(5, 1).u(32, 0x60000000).u(48, 0x900000000000).u(8, 60) // profile_tier_level
	w.ue(0).ue(1).ue(320).ue(240).u(1, 0)                                      // id, 4:2:0, size, no window
	w.ue(0).ue(0).ue(4).u(1, 1).ue(4).ue(2).ue(3)                              // depths, poc lsb, ordering
	w.ue(0).ue(3).ue(0).ue(3).ue(0).ue(0)                                      // block sizes
	w.u(4, 0b0100)                                                             // scaling, amp, sao, pcm
	w.ue(0)                                                                    // num_short_term_ref_pic_sets
	w.u(4, 0b0110)                                                             // long term, tmvp, smoothing, vui
	w.u(1, 1).u(4, 0b0001).u(4, 0)                                             // extension: scc only
	w.u(1, 0).u(1, 1).ue(63).ue(64).u(1, 1).ue(1 << 40)                        // palette, 2^40 initializers
	for range 64 {
		w.u(8, 0x80)
	}
	return w.nal(0x42, 0x01)
}

// hostileVPS returns an H.265 VPS with vps_num_hrd_parameters near 2^32;
// mp4ff allocates a table of that many pointers.
func hostileVPS() []byte {
	w := &bitWriter{}
	w.u(4, 0).u(1, 1).u(1, 1).u(6, 0).u(3, 0).u(1, 1).u(16, 0xffff)
	w.u(2, 0).u(1, 0).u(5, 1).u(32, 0x60000000).u(48, 0x900000000000).u(8, 60) // profile_tier_level
	w.u(1, 1).ue(4).ue(2).ue(0)                                                // sub-layer ordering
	w.u(6, 0).ue(0)                                                            // max layer id, layer sets
	w.u(1, 1).u(64, 25).u(1, 0)                                                // timing
	w.ue(0xfffffffe)                                                           // vps_num_hrd_parameters
	return w.nal(0x40, 0x01)
}

func TestMediaRejectsHostileSPS(t *testing.T) {
	pps := unhex(t, h264PPSHex)
	t.Run("cycle 100000", func(t *testing.T) {
		sps := hostilePOC1SPS(100000, 100000)
		_, err := NewH264Config([][]byte{sps}, [][]byte{pps})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "media: h264 config")
		_, err = ParamSetID(CodecH264, sps)
		require.Error(t, err)
		_, err = CodecString(&TrackConfig{Codec: CodecH264, SPS: [][]byte{sps}, PPS: [][]byte{pps}})
		require.Error(t, err)
	})
	t.Run("cycle huge", func(t *testing.T) {
		// A count near 2^32 used to make mp4ff allocate tens of gigabytes.
		sps := hostilePOC1SPS(0xfffffffe, 64)
		_, err := NewH264Config([][]byte{sps}, [][]byte{pps})
		require.Error(t, err)
		_, err = CodecString(&TrackConfig{Codec: CodecH264, SPS: [][]byte{sps}, PPS: [][]byte{pps}})
		require.Error(t, err)
	})
	t.Run("h265 scc", func(t *testing.T) {
		vps, pps := unhex(t, h265VPSHex), unhex(t, h265PPSHex)
		sps := hostileSCCSPS()
		_, err := NewH265Config([][]byte{vps}, [][]byte{sps}, [][]byte{pps})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sps_scc_extension")
		_, err = CodecString(&TrackConfig{Codec: CodecH265, SPS: [][]byte{sps}})
		require.Error(t, err)
	})
	t.Run("h265 vps", func(t *testing.T) {
		sps, pps := unhex(t, h265SPSHex), unhex(t, h265PPSHex)
		vps := hostileVPS()
		_, err := NewH265Config([][]byte{vps}, [][]byte{sps}, [][]byte{pps})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vps_num_hrd_parameters")
		_, err = ParamSetID(CodecH265, vps)
		require.Error(t, err)
	})
	t.Run("h265", func(t *testing.T) {
		sps := unhex(t, h265SPSHex)
		_, err := NewH265Config([][]byte{unhex(t, h265VPSHex)}, [][]byte{sps[:12]}, [][]byte{unhex(t, h265PPSHex)})
		require.Error(t, err)
		_, err = CodecString(&TrackConfig{Codec: CodecH265, SPS: [][]byte{sps[:12]}})
		require.Error(t, err)
	})
}

// TestParamSetIDRange checks that PPS ids beyond the spec range are
// rejected: a hostile source sending a fresh id per AU would otherwise grow
// the parameter-set maps without bound.
func TestParamSetIDRange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codec Codec
		hdr   []byte
		id    uint64
		ok    bool
	}{
		{"h264 pps 255", CodecH264, []byte{0x68}, 255, true},
		{"h264 pps 256", CodecH264, []byte{0x68}, 256, false},
		{"h264 pps 1<<20", CodecH264, []byte{0x68}, 1 << 20, false},
		{"h264 pps 4000000", CodecH264, []byte{0x68}, 4000000, false},
		{"h265 pps 63", CodecH265, []byte{0x44, 0x01}, 63, true},
		{"h265 pps 64", CodecH265, []byte{0x44, 0x01}, 64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := (&bitWriter{}).ue(tc.id).ue(0).nal(tc.hdr...)
			id, err := ParamSetID(tc.codec, p)
			if !tc.ok {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "out of range")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, uint32(tc.id), id)
		})
	}

	sps := unhex(t, h264SPSHex)
	hostile := (&bitWriter{}).ue(1 << 20).ue(0).nal(0x68)
	_, err := NewH264Config([][]byte{sps}, [][]byte{hostile})
	require.Error(t, err)
}
