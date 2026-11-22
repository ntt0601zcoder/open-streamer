package rtsp

import (
	"bytes"
	"testing"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isDropped(n []byte, h265 bool) bool {
	if h265 {
		t := (n[0] >> 1) & 0x3f
		return t == 35 || t == 38
	}
	t := n[0] & 0x1f
	return t == 9 || t == 12
}

func keepNALs(au [][]byte, h265 bool) [][]byte {
	var k [][]byte
	for _, n := range au {
		if len(n) > 0 && !isDropped(n, h265) {
			k = append(k, n)
		}
	}
	return k
}

func packRoundTrip(t *testing.T, fixture string, c Codec) {
	t.Helper()
	h265 := c == CodecH265
	aus := fixtureAUs(t, fixture, h265)
	var pk Packetizer
	if h265 {
		pk = NewH265Packetizer(1200)
	} else {
		pk = NewH264Packetizer(1200)
	}
	d, err := NewDepacketizer(videoMedia(c), 8<<20)
	require.NoError(t, err)
	var seq rtpSeq
	var pionSeq rtpSeq
	got := make([]AU, 0, len(aus))
	for i, au := range aus {
		pls := pk.Packetize(au)
		require.NotEmpty(t, pls)
		for _, p := range pls {
			require.LessOrEqual(t, len(p), 1200)
		}
		want := keepNALs(au, h265)
		got = append(got, pushAll(t, d, seq.packets(pls, uint32(i)*3000))...)

		// Independent check through pion's depacketisers.
		var nals [][]byte
		var h264d codecs.H264Packet
		h265d := &codecs.H265Depacketizer{}
		var buf []byte
		for _, p := range pionSeq.packets(pls, uint32(i)*3000) {
			var out []byte
			var err error
			if h265 {
				out, err = h265d.Unmarshal(p.Payload)
			} else {
				out, err = h264d.Unmarshal(p.Payload)
			}
			require.NoError(t, err)
			buf = append(buf, out...)
		}
		nals = splitAnnexB(buf)
		require.Len(t, nals, len(want), "pion au %d", i)
		for j := range want {
			require.True(t, bytes.Equal(want[j], nals[j]), "pion au %d nal %d", i, j)
		}
	}
	got = append(got, d.Flush()...)
	require.Len(t, got, len(aus))
	for i := range got {
		want := keepNALs(aus[i], h265)
		require.Len(t, got[i].NALUs, len(want), "au %d", i)
		for j := range want {
			require.True(t, bytes.Equal(want[j], got[i].NALUs[j]), "au %d nal %d", i, j)
		}
	}
}

func TestPackH264RoundTrip(t *testing.T) {
	packRoundTrip(t, "h264_aac.ts", CodecH264)
	packRoundTrip(t, "h264_pyramid.ts", CodecH264)
}

func TestPackH265RoundTrip(t *testing.T) { packRoundTrip(t, "h265_aac51.ts", CodecH265) }

func TestPackFragmentBoundaries(t *testing.T) {
	const mtu = 100
	pk := NewH264Packetizer(mtu)
	for _, n := range []int{mtu - 1, mtu, mtu + 1, 3 * mtu} {
		nal := nal264(5, n)
		pls := pk.Packetize([][]byte{nal})
		var re []byte
		for i, p := range pls {
			require.LessOrEqual(t, len(p), mtu)
			if n <= mtu {
				require.Len(t, pls, 1)
				assert.True(t, bytes.Equal(nal, p))
				continue
			}
			assert.Equal(t, byte(28), p[0]&0x1f)
			assert.Equal(t, nal[0]&0xe0, p[0]&0xe0)
			assert.Equal(t, i == 0, p[1]&0x80 != 0)
			assert.Equal(t, i == len(pls)-1, p[1]&0x40 != 0)
			assert.Equal(t, byte(5), p[1]&0x1f)
			if i == 0 {
				re = append(re, nal[0])
			}
			re = append(re, p[2:]...)
		}
		if n > mtu {
			assert.True(t, bytes.Equal(nal, re), "n=%d", n)
		}
	}
	pk5 := NewH265Packetizer(mtu)
	for _, n := range []int{mtu - 1, mtu, mtu + 1, 3 * mtu} {
		nal := nal264(0, n)
		nal[0], nal[1] = 19<<1, 1
		pls := pk5.Packetize([][]byte{nal})
		if n <= mtu {
			require.Len(t, pls, 1)
			continue
		}
		var re []byte
		for i, p := range pls {
			require.LessOrEqual(t, len(p), mtu)
			assert.Equal(t, i == 0, p[2]&0x80 != 0)
			assert.Equal(t, i == len(pls)-1, p[2]&0x40 != 0)
			if i == 0 {
				re = append(re, nal[0], nal[1])
			}
			re = append(re, p[3:]...)
		}
		assert.True(t, bytes.Equal(nal, re))
	}
}

func TestPackH265FUHeader(t *testing.T) {
	nal := make([]byte, 500)
	// F=1, type 19, layer id 5 (bit 0 of byte0 = 1, bits in byte1), TID 3.
	nal[0] = 0x80 | 19<<1
	nal[1] = 5<<3 | 3
	// layerId = 5 => high bit in byte0 bit0 = 0; use layerId 33 to exercise it.
	nal[0] |= 1
	nal[1] = (33&0x1f)<<3 | 3
	pls := NewH265Packetizer(200).Packetize([][]byte{nal})
	require.Greater(t, len(pls), 1)
	for _, p := range pls {
		assert.Equal(t, byte(49), (p[0]>>1)&0x3f)
		assert.Equal(t, nal[0]&0x81, p[0]&0x81)
		assert.Equal(t, nal[1], p[1])
		assert.Equal(t, byte(19), p[2]&0x3f)
	}
}

func TestPackSkipsAUDAndFiller(t *testing.T) {
	idr := nal264(5, 20)
	pls := NewH264Packetizer(1200).Packetize([][]byte{{0x09, 0xf0}, idr, {0x0c, 0xff, 0xff}, {}})
	require.Len(t, pls, 1)
	assert.Equal(t, idr, pls[0])
	vcl := []byte{1 << 1, 1, 9, 9}
	pls = NewH265Packetizer(1200).Packetize([][]byte{{35 << 1, 1, 0x50}, vcl, {38 << 1, 1, 0xff}})
	require.Len(t, pls, 1)
	assert.Equal(t, vcl, pls[0])
	assert.Empty(t, NewH264Packetizer(1200).Packetize([][]byte{{0x09, 0xf0}}))
}
