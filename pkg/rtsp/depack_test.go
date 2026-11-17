package rtsp

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

var testT0 = time.Unix(1000, 0)

// splitAnnexB returns the NAL units of an Annex B byte stream.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func annexB(nalus [][]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// fixtureAUs returns the NAL units of each video PES of a fixture.
func fixtureAUs(t *testing.T, name string, h265 bool) [][][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	defer f.Close()
	d := mpegts.NewDemux(f)
	var video uint16
	var out [][][]byte
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				if s.Type == 0x1b || s.Type == 0x24 {
					video = s.PID
				}
			}
			continue
		}
		if ev.PES == nil || ev.PES.PID != video {
			continue
		}
		out = append(out, splitAnnexB(ev.PES.Data))
	}
	require.NotEmpty(t, out)
	_ = h265
	return out
}

func isAUDNal(n []byte, h265 bool) bool {
	if h265 {
		return (n[0]>>1)&0x3f == 35
	}
	return n[0]&0x1f == 9
}

type rtpSeq struct{ seq uint16 }

func (s *rtpSeq) packets(payloads [][]byte, ts uint32) []*rtp.Packet {
	out := make([]*rtp.Packet, 0, len(payloads))
	for i, pl := range payloads {
		out = append(out, &rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: s.seq, Timestamp: ts, SSRC: 7,
			Marker: i == len(payloads)-1,
		}, Payload: pl})
		s.seq++
	}
	return out
}

func videoMedia(c Codec) Media {
	return Media{Index: 3, Codec: c, Payload: 96, ClockRate: 90000}
}

func pushAll(t *testing.T, d Depacketizer, pkts []*rtp.Packet) []AU {
	t.Helper()
	out := make([]AU, 0, 8)
	for _, p := range pkts {
		out = append(out, d.Push(p, testT0)...)
	}
	return out
}

func roundTrip(t *testing.T, fixture string, c Codec) {
	t.Helper()
	h265 := c == CodecH265
	aus := fixtureAUs(t, fixture, h265)
	if len(aus) > 60 {
		aus = aus[:60]
	}
	d, err := NewDepacketizer(videoMedia(c), 8<<20)
	require.NoError(t, err)
	var h264p codecs.H264Payloader
	var h265p codecs.H265Payloader
	var seq rtpSeq
	got := make([]AU, 0, 64)
	for i, au := range aus {
		ab := annexB(au)
		var pls [][]byte
		if h265 {
			pls = h265p.Payload(1200, ab)
		} else {
			pls = h264p.Payload(1200, ab)
		}
		got = append(got, pushAll(t, d, seq.packets(pls, uint32(i)*3000))...)
	}
	got = append(got, d.Flush()...)
	want := make([][][]byte, 0, len(aus))
	for _, au := range aus {
		var k [][]byte
		for _, n := range au {
			if !isAUDNal(n, h265) {
				k = append(k, n)
			}
		}
		want = append(want, k)
	}
	require.Len(t, got, len(want))
	for i := range got {
		assert.Equal(t, 3, got[i].Track)
		assert.False(t, got[i].Lost)
		assert.Equal(t, uint32(i)*3000, got[i].Timestamp)
		require.Equal(t, len(want[i]), len(got[i].NALUs), "au %d", i)
		for j := range want[i] {
			require.True(t, bytes.Equal(want[i][j], got[i].NALUs[j]), "au %d nal %d", i, j)
		}
	}
}

func TestDepackH264RoundTrip(t *testing.T) { roundTrip(t, "h264_aac.ts", CodecH264) }
func TestDepackH265RoundTrip(t *testing.T) { roundTrip(t, "h265_aac51.ts", CodecH265) }

func nal264(typ byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 1)
	}
	b[0] = 0x60 | typ
	return b
}

func pkt(seq uint16, ts uint32, marker bool, pl []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, Marker: marker}, Payload: pl}
}

func fuA(nri, typ byte, //nolint:unparam // nri is part of the FU indicator.
	s, e bool, data []byte,
) []byte {
	h := typ
	if s {
		h |= 0x80
	}
	if e {
		h |= 0x40
	}
	return append([]byte{0x1c | nri, h}, data...)
}

func TestDepackLostFragment(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	idr := nal264(5, 3000)
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, false, fuA(0x60, 5, true, false, idr[1:1000])), testT0)...)
	// the middle and end fragments are lost.
	d.Gap()
	out = append(out, d.Push(pkt(4, 200, true, nal264(1, 50)), testT0)...)
	out = append(out, d.Push(pkt(5, 300, true, nal264(1, 60)), testT0)...)
	out = append(out, d.Flush()...)
	require.Len(t, out, 2)
	assert.True(t, out[0].Lost)
	assert.Equal(t, uint32(200), out[0].Timestamp)
	assert.False(t, out[1].Lost)
	assert.Equal(t, nal264(1, 60), out[1].NALUs[0])
}

func TestDepackLostFragmentNoGapCall(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	idr := nal264(5, 3000)
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, false, fuA(0x60, 5, true, false, idr[1:1000])), testT0)...)
	out = append(out, d.Push(pkt(2, 200, true, nal264(1, 50)), testT0)...)
	out = append(out, d.Push(pkt(3, 300, true, nal264(1, 51)), testT0)...)
	out = append(out, d.Flush()...)
	// the IDR never completed, so its Lost flag moves to the next AU.
	require.Len(t, out, 2)
	assert.True(t, out[0].Lost)
	assert.Equal(t, uint32(200), out[0].Timestamp)
	assert.False(t, out[1].Lost)
}

func TestDepackInvalidNALs(t *testing.T) {
	good := nal264(1, 20)
	stap := func(inner ...[]byte) []byte {
		pl := []byte{0x78}
		for _, n := range inner {
			pl = append(pl, byte(len(n)>>8), byte(len(n)))
			pl = append(pl, n...)
		}
		return pl
	}
	nestedSTAP := stap(good, stap(nal264(1, 8)))
	nestedFU := stap(good, fuA(0x60, 5, true, false, []byte{1, 2}))
	for name, pl := range map[string][]byte{
		"nested STAP-A": nestedSTAP,
		"FU in STAP-A":  nestedFU,
		"type 0":        stap(good, []byte{0x60, 1, 2}),
	} {
		d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
		require.NoError(t, err)
		d.Push(pkt(1, 100, true, pl), testT0)
		out := d.Flush()
		require.Len(t, out, 1, name)
		assert.True(t, out[0].Lost, name)
		assert.Equal(t, [][]byte{good}, out[0].NALUs, name)
	}
	// an FU whose inner type is 28 or 0 is rejected.
	for _, typ := range []byte{28, 0, 24} {
		d, _ := NewDepacketizer(videoMedia(CodecH264), 1<<20)
		d.Push(pkt(1, 100, false, good), testT0)
		d.Push(pkt(2, 100, false, fuA(0x60, typ, true, false, []byte{1, 2})), testT0)
		d.Push(pkt(3, 100, true, fuA(0x60, typ, false, true, []byte{3})), testT0)
		out := d.Flush()
		require.Len(t, out, 1)
		assert.True(t, out[0].Lost)
		assert.Equal(t, [][]byte{good}, out[0].NALUs)
	}
	// H.265: type 48+ and a 1-byte unit inside an AP.
	g := h265nal(1, 10)
	ap := func(inner ...[]byte) []byte {
		pl := []byte{48 << 1, 1}
		for _, n := range inner {
			pl = append(pl, byte(len(n)>>8), byte(len(n)))
			pl = append(pl, n...)
		}
		return pl
	}
	for name, pl := range map[string][]byte{
		"nested AP":  ap(g, ap(h265nal(1, 6))),
		"type 63":    ap(g, []byte{63 << 1, 1, 9}),
		"one byte":   ap(g, []byte{2}),
		"FU in AP":   ap(g, []byte{49 << 1, 1, 0x93, 5}),
		"type 50 FU": nil,
	} {
		d, _ := NewDepacketizer(videoMedia(CodecH265), 1<<20)
		if pl == nil {
			d.Push(pkt(1, 100, false, g), testT0)
			d.Push(pkt(2, 100, false, []byte{49 << 1, 1, 0x80 | 50, 1, 2}), testT0)
		} else {
			d.Push(pkt(1, 100, true, pl), testT0)
		}
		out := d.Flush()
		require.Len(t, out, 1, name)
		assert.True(t, out[0].Lost, name)
		assert.Equal(t, [][]byte{g}, out[0].NALUs, name)
	}
}

func TestDepackCarryCapped(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 8<<20)
	require.NoError(t, err)
	sps := nal264(7, 30000)
	for i := uint32(0); i < 10; i++ {
		d.Push(pkt(uint16(i), 100+i, true, sps), testT0)
	}
	d.Push(pkt(20, 200, true, nal264(5, 50)), testT0)
	out := d.Flush()
	require.Len(t, out, 1)
	// at most two 30000-byte sets (60000 < 64 KiB) plus the slice survive.
	assert.LessOrEqual(t, len(out[0].NALUs), 3)
	assert.Equal(t, nal264(5, 50), out[0].NALUs[len(out[0].NALUs)-1])
}

func TestDepackGapWithinAU(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, false, nal264(1, 40)), testT0)...)
	d.Gap()
	out = append(out, d.Push(pkt(3, 100, true, nal264(1, 41)), testT0)...)
	out = append(out, d.Push(pkt(4, 200, true, nal264(1, 42)), testT0)...)
	out = append(out, d.Flush()...)
	require.Len(t, out, 2)
	assert.True(t, out[0].Lost)
	assert.False(t, out[1].Lost, "same timestamp: the gap belongs to the first AU only")
}

func TestDepackParamSetsAlone(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	sps, pps, idr := nal264(7, 20), nal264(8, 5), nal264(5, 100)
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, true, sps), testT0)...)
	out = append(out, d.Push(pkt(2, 100, true, pps), testT0)...)
	out = append(out, d.Push(pkt(3, 200, true, idr), testT0)...)
	out = append(out, d.Flush()...)
	require.Len(t, out, 1)
	assert.Equal(t, uint32(200), out[0].Timestamp)
	assert.Equal(t, [][]byte{sps, pps, idr}, out[0].NALUs)
}

func TestDepackMarkerPerSlice(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	s1, s2, s3 := nal264(1, 30), nal264(1, 31), nal264(1, 32)
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, true, s1), testT0)...)
	out = append(out, d.Push(pkt(2, 100, true, s2), testT0.Add(time.Millisecond))...)
	out = append(out, d.Push(pkt(3, 100, true, s3), testT0.Add(2*time.Millisecond))...)
	assert.Empty(t, out)
	out = append(out, d.Push(pkt(4, 200, true, nal264(1, 33)), testT0.Add(40*time.Millisecond))...)
	require.Len(t, out, 1)
	assert.Equal(t, [][]byte{s1, s2, s3}, out[0].NALUs)
	assert.Equal(t, testT0.Add(2*time.Millisecond), out[0].Arrival)
	assert.Len(t, d.Flush(), 1)
}

func TestDepackAUDDropped(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	out := d.Push(pkt(1, 100, true, []byte{0x09, 0xf0}), testT0)
	out = append(out, d.Push(pkt(2, 100, true, nal264(1, 10)), testT0)...)
	out = append(out, d.Flush()...)
	require.Len(t, out, 1)
	assert.Len(t, out[0].NALUs, 1)
}

func TestDepackSTAPA(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	a, b := nal264(7, 10), nal264(8, 6)
	pl := make([]byte, 0, 64)
	pl = append(pl, 0x78, 0, byte(len(a)))
	pl = append(pl, a...)
	pl = append(pl, 0, byte(len(b)))
	pl = append(pl, b...)
	out := d.Push(pkt(1, 100, false, pl), testT0)
	out = append(out, d.Push(pkt(2, 100, true, nal264(5, 20)), testT0)...)
	out = append(out, d.Flush()...)
	require.Len(t, out, 1)
	assert.Equal(t, a, out[0].NALUs[0])
	assert.Equal(t, b, out[0].NALUs[1])
	// a truncated STAP-A marks the AU lost.
	d2, _ := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	d2.Push(pkt(1, 100, false, pl[:len(pl)-3]), testT0)
	d2.Push(pkt(2, 100, true, nal264(5, 20)), testT0)
	out = d2.Flush()
	require.Len(t, out, 1)
	assert.True(t, out[0].Lost)
}

func TestDepackMaxAU(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 100)
	require.NoError(t, err)
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, false, nal264(1, 60)), testT0)...)
	out = append(out, d.Push(pkt(2, 100, true, nal264(1, 60)), testT0)...)
	out = append(out, d.Push(pkt(3, 200, true, nal264(1, 50)), testT0)...)
	out = append(out, d.Flush()...)
	require.Len(t, out, 1)
	assert.Equal(t, uint32(200), out[0].Timestamp)
	oc, ok := d.(OversizeCounter)
	require.True(t, ok)
	assert.Equal(t, uint64(1), oc.Oversize())

	// a fragmented AU is checked before each append.
	d, _ = NewDepacketizer(videoMedia(CodecH264), 100)
	out = d.Push(pkt(1, 100, false, fuA(0x60, 5, true, false, make([]byte, 60))), testT0)
	out = append(out, d.Push(pkt(2, 100, false, fuA(0x60, 5, false, false, make([]byte, 60))), testT0)...)
	out = append(out, d.Push(pkt(3, 100, true, fuA(0x60, 5, false, true, make([]byte, 5))), testT0)...)
	out = append(out, d.Flush()...)
	assert.Empty(t, out)
}

func TestDepackFUANewStart(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	require.NoError(t, err)
	d.Push(pkt(1, 100, false, fuA(0x60, 5, true, false, []byte{1, 2, 3})), testT0)
	d.Push(pkt(2, 100, false, fuA(0x60, 5, true, false, []byte{4, 5})), testT0)
	d.Push(pkt(3, 100, true, fuA(0x60, 5, false, true, []byte{6})), testT0)
	out := d.Flush()
	require.Len(t, out, 1)
	assert.True(t, out[0].Lost)
	assert.Equal(t, []byte{0x65, 4, 5, 6}, out[0].NALUs[0])
}

func TestNewDepacketizerErrors(t *testing.T) {
	_, err := NewDepacketizer(Media{Codec: CodecOther}, 1)
	require.Error(t, err)
	_, err = NewDepacketizer(Media{Codec: CodecH264, Reason: "x"}, 1)
	require.Error(t, err)
	_, err = NewDepacketizer(Media{Codec: CodecAAC, ClockRate: 48000}, 1)
	require.Error(t, err)
}

func h265nal(typ byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 3)
	}
	b[0], b[1] = typ<<1, 1
	return b
}

func TestDepackH265APFUAndPACI(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH265), 1<<20)
	require.NoError(t, err)
	vps, sps := h265nal(32, 12), h265nal(33, 14)
	ap := make([]byte, 0, 64)
	ap = append(ap, 48<<1, 1, 0, byte(len(vps)))
	ap = append(ap, vps...)
	ap = append(ap, 0, byte(len(sps)))
	ap = append(ap, sps...)
	idr := h265nal(19, 500)
	fu := func(s, e bool, data []byte) []byte {
		h := byte(19)
		if s {
			h |= 0x80
		}
		if e {
			h |= 0x40
		}
		return append([]byte{49 << 1, 1, h}, data...)
	}
	out := make([]AU, 0, 8)
	out = append(out, d.Push(pkt(1, 100, false, ap), testT0)...)
	out = append(out, d.Push(pkt(2, 100, false, []byte{50 << 1, 1, 0, 0, 0}), testT0)...) // PACI
	out = append(out, d.Push(pkt(3, 100, false, fu(true, false, idr[2:200])), testT0)...)
	out = append(out, d.Push(pkt(4, 100, true, fu(false, true, idr[200:])), testT0)...)
	out = append(out, d.Push(pkt(5, 200, true, []byte{35 << 1, 1, 0x50}), testT0)...) // AUD
	out = append(out, d.Flush()...)
	require.Len(t, out, 1)
	assert.Equal(t, [][]byte{vps, sps, idr}, out[0].NALUs)
	assert.False(t, out[0].Lost)
}

func TestDepackH265Edges(t *testing.T) {
	d, err := NewDepacketizer(videoMedia(CodecH265), 1<<20)
	require.NoError(t, err)
	oc, ok := d.(OversizeCounter)
	require.True(t, ok)
	assert.Zero(t, oc.Oversize())
	g := h265nal(1, 10)

	// A payload shorter than the NAL header is ignored.
	d.Push(pkt(1, 100, false, g), testT0)
	d.Push(pkt(2, 100, false, []byte{2}), testT0)
	out := d.Flush()
	require.Len(t, out, 1)
	assert.False(t, out[0].Lost)
	assert.Equal(t, [][]byte{g}, out[0].NALUs)

	for name, bad := range map[string][]byte{
		"truncated FU":     {49 << 1, 1},
		"FU start and end": {49 << 1, 1, 0xc0 | 1, 5, 6},
	} {
		d.Push(pkt(3, 200, false, g), testT0)
		d.Push(pkt(4, 200, false, bad), testT0)
		out = d.Flush()
		require.Len(t, out, 1, name)
		assert.True(t, out[0].Lost, name)
		assert.Equal(t, [][]byte{g}, out[0].NALUs, name)
	}

	// Gap marks the pending AU lost.
	d.Push(pkt(5, 300, false, g), testT0)
	d.Gap()
	out = d.Flush()
	require.Len(t, out, 1)
	assert.True(t, out[0].Lost)
}

func TestDepackOwnership(t *testing.T) {
	d, _ := NewDepacketizer(videoMedia(CodecH264), 1<<20)
	buf := nal264(1, 20)
	want := append([]byte(nil), buf...)
	d.Push(pkt(1, 100, true, buf), testT0)
	for i := range buf {
		buf[i] = 0xee
	}
	out := d.Flush()
	require.Len(t, out, 1)
	assert.Equal(t, want, out[0].NALUs[0])
}

func TestSeqTracker(t *testing.T) {
	var s SeqTracker
	gap, reset, dup := s.Next(65534, 1)
	assert.Equal(t, 0, gap)
	assert.False(t, reset || dup)
	gap, reset, dup = s.Next(65535, 1)
	assert.Equal(t, 0, gap)
	assert.False(t, reset || dup)
	gap, reset, dup = s.Next(0, 1) // wrap
	assert.Equal(t, 0, gap)
	assert.False(t, reset || dup)
	_, _, dup = s.Next(0, 1)
	assert.True(t, dup)
	_, _, dup = s.Next(65535, 1) // older
	assert.True(t, dup)
	gap, reset, dup = s.Next(4, 1) // lost 1..3
	assert.Equal(t, 3, gap)
	assert.False(t, reset || dup)
	gap, reset, dup = s.Next(4+3000, 1)
	assert.Equal(t, 2999, gap)
	assert.False(t, reset || dup)
	_, reset, dup = s.Next(4+3000+3001, 1)
	assert.True(t, reset)
	assert.False(t, dup)
	// the tracker followed the jump
	gap, reset, _ = s.Next(4+3000+3002, 1)
	assert.Equal(t, 0, gap)
	assert.False(t, reset)
	// backward jump beyond the dup window
	_, reset, dup = s.Next(20000, 1)
	assert.True(t, reset)
	assert.False(t, dup)
	// a new SSRC resets even at the next sequence number
	gap, reset, dup = s.Next(20001, 2)
	assert.True(t, reset)
	assert.False(t, dup)
	assert.Equal(t, 0, gap)
}

// TestDepackNALFlood sends aggregation packets full of tiny NAL units: the
// AU bound must count a per-NAL overhead and the NAL count, so the flood
// is dropped as oversize with the pending AU kept small.
func TestDepackNALFlood(t *testing.T) {
	for _, tc := range []struct {
		codec Codec
		head  []byte // aggregation packet header
		nal   []byte // one tiny NAL unit
		slice []byte
	}{
		{CodecH264, []byte{24}, []byte{6}, nal264(5, 20)},
		{CodecH265, []byte{48 << 1, 1}, []byte{39 << 1, 1}, h265nal(19, 20)},
	} {
		d, err := NewDepacketizer(videoMedia(tc.codec), 0)
		require.NoError(t, err)
		pl := append([]byte(nil), tc.head...)
		for len(pl)+2+len(tc.nal) <= 65000 {
			pl = append(pl, 0, byte(len(tc.nal)))
			pl = append(pl, tc.nal...)
		}
		var asm *assembler
		switch dd := d.(type) {
		case *h264Depack:
			asm = dd.asm
		case *h265Depack:
			asm = dd.asm
		}
		require.NotNil(t, asm)
		d.Push(pkt(0, 1, false, tc.slice), testT0)
		for i := 1; i <= 380; i++ {
			d.Push(pkt(uint16(i), 1, false, pl), testT0)
			require.LessOrEqual(t, len(asm.nalus), maxNALsPerAU, "codec %d", tc.codec)
		}
		out := d.Push(pkt(381, 2, true, tc.slice), testT0)
		assert.Empty(t, out, "codec %d: the flooded AU is dropped", tc.codec)
		assert.Equal(t, uint64(1), d.(OversizeCounter).Oversize(), "codec %d", tc.codec)
		out = d.Flush()
		require.Len(t, out, 1, "codec %d: the next AU passes", tc.codec)
	}

	// The per-NAL overhead counts against the byte bound too.
	d, err := NewDepacketizer(videoMedia(CodecH264), 1000)
	require.NoError(t, err)
	pl := make([]byte, 1, 121)
	pl[0] = 24
	for range 40 { // 40 × (2+1) wire bytes, 40 × (1+32) accounted
		pl = append(pl, 0, 1, 6)
	}
	d.Push(pkt(1, 1, false, pl), testT0)
	d.Push(pkt(2, 1, false, nal264(5, 20)), testT0)
	assert.Empty(t, d.Push(pkt(3, 2, true, nal264(5, 20)), testT0))
	assert.Equal(t, uint64(1), d.(OversizeCounter).Oversize())
}
