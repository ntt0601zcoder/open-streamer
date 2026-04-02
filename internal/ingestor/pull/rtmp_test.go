package pull

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestH264AccessUnitForTS(t *testing.T) {
	t.Parallel()
	prefix := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 0, 0, 0, 1, 0x68, 0xce}
	au := []byte{0, 0, 0, 1, 0x65, 0x88}
	t.Run("no_prefix_non_key", func(t *testing.T) {
		t.Parallel()
		got := h264AccessUnitForTS(false, prefix, au)
		require.Equal(t, au, got)
	})
	t.Run("no_prefix_when_empty_codec", func(t *testing.T) {
		t.Parallel()
		got := h264AccessUnitForTS(true, nil, au)
		require.Equal(t, au, got)
	})
	t.Run("prefix_on_keyframe", func(t *testing.T) {
		t.Parallel()
		got := h264AccessUnitForTS(true, prefix, au)
		require.True(t, bytes.HasPrefix(got, prefix))
		require.True(t, bytes.HasSuffix(got, au))
	})
}

func TestIsH264AVCCAccessUnit(t *testing.T) {
	t.Parallel()
	t.Run("two_nalus", func(t *testing.T) {
		t.Parallel()
		// NAL len 2 + 0x09 0xF0, NAL len 3 + 0x67 0x42 0x00
		var b []byte
		n1 := []byte{0x09, 0xF0}
		n2 := []byte{0x67, 0x42, 0x00}
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, uint32(len(n1)))
		b = append(b, tmp...)
		b = append(b, n1...)
		binary.BigEndian.PutUint32(tmp, uint32(len(n2)))
		b = append(b, tmp...)
		b = append(b, n2...)
		require.True(t, isH264AVCCAccessUnit(b))
		out := h264ForTSMuxer(b)
		require.True(t, bytes.Contains(out, []byte{0, 0, 0, 1, 0x67}))
	})
	t.Run("length_field_looks_like_start_code", func(t *testing.T) {
		t.Parallel()
		// 0x00000100 = 256 — contains 00 00 01; must still be AVCC, not mistaken for Annex-B.
		n := make([]byte, 256)
		n[0] = 0x67 // SPS-like header
		var b []byte
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, 256)
		b = append(b, tmp...)
		b = append(b, n...)
		require.True(t, isH264AVCCAccessUnit(b))
		out := h264ForTSMuxer(b)
		require.True(t, bytes.HasPrefix(out, []byte{0, 0, 0, 1}))
		require.Equal(t, byte(0x67), out[4])
	})
	t.Run("annex_b_long_nal_not_avcc", func(t *testing.T) {
		t.Parallel()
		// 00 00 00 01 + 200-byte NAL: first uint32=1 would fake AVCC; walk must fail.
		b := []byte{0, 0, 0, 1, 0x67}
		b = append(b, make([]byte, 195)...)
		require.False(t, isH264AVCCAccessUnit(b))
		require.Equal(t, b, h264ForTSMuxer(b))
	})
}
